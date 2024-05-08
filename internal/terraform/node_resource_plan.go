// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// nodeExpandPlannableResource represents an addrs.ConfigResource and implements
// DynamicExpand to a subgraph containing all of the addrs.AbsResourceInstance
// resulting from both the containing module and resource-specific expansion.
type nodeExpandPlannableResource struct {
	*NodeAbstractResource

	// ForceCreateBeforeDestroy might be set via our GraphNodeDestroyerCBD
	// during graph construction, if dependencies require us to force this
	// on regardless of what the configuration says.
	ForceCreateBeforeDestroy *bool

	// skipRefresh indicates that we should skip refreshing individual instances
	skipRefresh bool

	preDestroyRefresh bool

	// skipPlanChanges indicates we should skip trying to plan change actions
	// for any instances.
	skipPlanChanges bool

	// forceReplace are resource instance addresses where the user wants to
	// force generating a replace action. This set isn't pre-filtered, so
	// it might contain addresses that have nothing to do with the resource
	// that this node represents, which the node itself must therefore ignore.
	forceReplace []addrs.AbsResourceInstance

	// We attach dependencies to the Resource during refresh, since the
	// instances are instantiated during DynamicExpand.
	// FIXME: These would be better off converted to a generic Set data
	// structure in the future, as we need to compare for equality and take the
	// union of multiple groups of dependencies.
	dependencies []addrs.ConfigResource

	// these are a record of all the addresses used in expansion so they can be
	// validated as a complete set. While the type is guaranteed to be
	// addrs.AbsResourceInstance for all these, we use addrs.Checkable because
	// the expandedInstances need to be passed to the check state to register
	// the instances for checks.
	expandedImports   addrs.Set[addrs.Checkable]
	expandedInstances addrs.Set[addrs.Checkable]
}

var (
	_ GraphNodeDestroyerCBD         = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeDynamicExpandable    = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeReferenceable        = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeReferencer           = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeImportReferencer     = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeConfigResource       = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeAttachResourceConfig = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeAttachDependencies   = (*nodeExpandPlannableResource)(nil)
	_ GraphNodeTargetable           = (*nodeExpandPlannableResource)(nil)
	_ graphNodeExpandsInstances     = (*nodeExpandPlannableResource)(nil)
)

func (n *nodeExpandPlannableResource) Name() string {
	return n.NodeAbstractResource.Name() + " (expand)"
}

func (n *nodeExpandPlannableResource) expandsInstances() {
}

// GraphNodeAttachDependencies
func (n *nodeExpandPlannableResource) AttachDependencies(deps []addrs.ConfigResource) {
	n.dependencies = deps
}

// GraphNodeDestroyerCBD
func (n *nodeExpandPlannableResource) CreateBeforeDestroy() bool {
	if n.ForceCreateBeforeDestroy != nil {
		return *n.ForceCreateBeforeDestroy
	}

	// If we have no config, we just assume no
	if n.Config == nil || n.Config.Managed == nil {
		return false
	}

	return n.Config.Managed.CreateBeforeDestroy
}

// GraphNodeDestroyerCBD
func (n *nodeExpandPlannableResource) ModifyCreateBeforeDestroy(v bool) error {
	n.ForceCreateBeforeDestroy = &v
	return nil
}

func (n *nodeExpandPlannableResource) DynamicExpand(ctx EvalContext) (*Graph, tfdiags.Diagnostics) {
	var g Graph

	// First, make sure the count and the foreach don't refer to the same
	// resource. The config maybe nil if we are generating configuration, or
	// deleting a resource.
	if n.Config != nil {
		var diags tfdiags.Diagnostics
		diags = diags.Append(validateSelfRefInExpr(n.Addr.Resource, n.Config.Count))
		diags = diags.Append(validateSelfRefInExpr(n.Addr.Resource, n.Config.ForEach))
		if diags.HasErrors() {
			return nil, diags
		}
	}

	expander := ctx.InstanceExpander()
	moduleInstances := expander.ExpandModule(n.Addr.Module, false)

	// The possibility of partial-expanded modules and resources is
	// currently guarded by a language experiment, and so to minimize the
	// risk of that experiment impacting mainline behavior we currently
	// branch off into an entirely-separate codepath in those situations,
	// at the expense of duplicating some of the logic for behavior this
	// method would normally handle.
	//
	// Normally language experiments are confined to only a single module,
	// but this one has potential cross-module impact once enabled for at
	// least one, and so this flag is true if _any_ module in the configuration
	// has opted in to the experiment. Our intent is for this different
	// codepath to produce the same results when there aren't any
	// partial-expanded modules, but bugs might make that not true and so
	// this is conservative to minimize the risk of breaking things for
	// those who aren't participating in the experiment.
	//
	// TODO: If this experiment is stablized then we should aim to combine
	// these two codepaths back together, so that the behavior is less likely
	// to diverge under future maintenence.
	if n.unknownInstancesExperimentEnabled {
		pem := expander.UnknownModuleInstances(n.Addr.Module, false)
		return n.dynamicExpandWithUnknownInstancesExperiment(ctx, moduleInstances, pem)
	}

	// Lock the state while we inspect it
	state := ctx.State().Lock()

	var orphans []*states.Resource
	for _, res := range state.Resources(n.Addr) {
		found := false
		for _, m := range moduleInstances {
			if m.Equal(res.Addr.Module) {
				found = true
				break
			}
		}
		// The module instance of the resource in the state doesn't exist
		// in the current config, so this whole resource is orphaned.
		if !found {
			orphans = append(orphans, res)
		}
	}

	// We'll no longer use the state directly here, and the other functions
	// we'll call below may use it so we'll release the lock.
	state = nil
	ctx.State().Unlock()

	// The concrete resource factory we'll use for orphans
	concreteResourceOrphan := func(a *NodeAbstractResourceInstance) *NodePlannableResourceInstanceOrphan {
		// Add the config and state since we don't do that via transforms
		a.Config = n.Config
		a.ResolvedProvider = n.ResolvedProvider
		a.Schema = n.Schema
		a.ProvisionerSchemas = n.ProvisionerSchemas
		a.ProviderMetas = n.ProviderMetas
		a.Dependencies = n.dependencies

		return &NodePlannableResourceInstanceOrphan{
			NodeAbstractResourceInstance: a,
			skipRefresh:                  n.skipRefresh,
			skipPlanChanges:              n.skipPlanChanges,
		}
	}

	for _, res := range orphans {
		for key := range res.Instances {
			addr := res.Addr.Instance(key)
			abs := NewNodeAbstractResourceInstance(addr)
			abs.AttachResourceState(res)
			n := concreteResourceOrphan(abs)
			g.Add(n)
		}
	}

	// The above dealt with the expansion of the containing module, so now
	// we need to deal with the expansion of the resource itself across all
	// instances of the module.
	//
	// We'll gather up all of the leaf instances we learn about along the way
	// so that we can inform the checks subsystem of which instances it should
	// be expecting check results for, below.
	var diags tfdiags.Diagnostics
	n.expandedImports = addrs.MakeSet[addrs.Checkable]()
	n.expandedInstances = addrs.MakeSet[addrs.Checkable]()
	for _, module := range moduleInstances {
		resAddr := n.Addr.Resource.Absolute(module)
		err := n.expandResourceInstances(ctx, resAddr, &g)
		diags = diags.Append(err)
	}
	if diags.HasErrors() {
		return nil, diags
	}

	diags = diags.Append(n.validateExpandedImportTargets())

	// If this is a resource that participates in custom condition checks
	// (i.e. it has preconditions or postconditions) then the check state
	// wants to know the addresses of the checkable objects so that it can
	// treat them as unknown status if we encounter an error before actually
	// visiting the checks.
	if checkState := ctx.Checks(); checkState.ConfigHasChecks(n.NodeAbstractResource.Addr) {
		checkState.ReportCheckableObjects(n.NodeAbstractResource.Addr, n.expandedInstances)
	}

	addRootNodeToGraph(&g)

	return &g, diags
}

// dynamicExpandWithUnknownInstancesExperiment is a temporary experimental
// variant of DynamicExpand that we use when at least one module is
// participating in the "unknown_instances" language experiment.
//
// This is not exactly in the typical spirit of language experiments in that
// the effect is not scoped only to the module where the opt-in is declared:
// if there are bugs in this method then they could potentially also affect
// resources in modules not directly participating. We're accepting that
// as a pragmatic compromise here since unknown expansion of a module call
// is inherently a cross-module concern.
//
// If we move forward with unknown instances as a stable feature then we
// should find a way to meld this logic with the main DynamicExpand logic,
// but it's separate for now to minimize the risk of the experiment impacting
// configurations that are not opted into it.
func (n *nodeExpandPlannableResource) dynamicExpandWithUnknownInstancesExperiment(globalCtx EvalContext, knownInsts []addrs.ModuleInstance, partialInsts addrs.Set[addrs.PartialExpandedModule]) (*Graph, tfdiags.Diagnostics) {
	var g Graph
	var diags tfdiags.Diagnostics

	// We need to resolve the expansions of the resource itself, separately
	// for each of the dynamic module prefixes it appears under.
	knownAddrs := addrs.MakeSet[addrs.AbsResourceInstance]()
	partialExpandedAddrs := addrs.MakeSet[addrs.PartialExpandedResource]()
	for _, moduleAddr := range knownInsts {
		resourceAddr := n.Addr.Resource.Absolute(moduleAddr)
		// The rest of our work here needs to know which module instance it's
		// working in, so that it can evaluate expressions in the appropriate scope.
		moduleCtx := evalContextForModuleInstance(globalCtx, resourceAddr.Module)

		// writeResourceState calculates the dynamic expansion of the given
		// resource as a side-effect, along with its other work.
		moreDiags := n.writeResourceState(moduleCtx, resourceAddr)
		diags = diags.Append(moreDiags)
		if moreDiags.HasErrors() {
			continue
		}

		// We can now ask for all of the individual resource instances that
		// we know, or for those with not-yet-known expansion.
		expander := moduleCtx.InstanceExpander()
		_, knownInstKeys, haveUnknownKeys := expander.ResourceInstanceKeys(resourceAddr)
		for _, instKey := range knownInstKeys {
			instAddr := resourceAddr.Instance(instKey)
			knownAddrs.Add(instAddr)
		}
		if haveUnknownKeys {
			partialAddr := moduleAddr.UnexpandedResource(resourceAddr.Resource)
			partialExpandedAddrs.Add(partialAddr)
		}
	}
	for _, moduleAddr := range partialInsts {
		// Resources that appear under partial-expanded module prefixes are
		// also partial-expanded resource addresses.
		partialAddr := moduleAddr.Resource(n.Addr.Resource)
		partialExpandedAddrs.Add(partialAddr)
	}
	// If we accumulated any error diagnostics in our work so far then
	// we'll just bail out at this point.
	if diags.HasErrors() {
		return nil, diags
	}

	// We need to search the prior state for any resource instances that
	// belong to module instances that are no longer declared in the
	// configuration, which is one way a resource instance can be classified
	// as an "orphan".
	//
	// However, if any instance is under a partial-expanded prefix then
	// we can't know whether it's still desired or not, and so we'll need
	// to defer dealing with it to a future plan/apply round.
	//
	// We need to compare with the resource instances we can find in the
	// state, so we'll need to briefly hold the state lock while we inspect
	// those. The following inline function limits the scope of the lock.
	orphanAddrs := addrs.MakeSet[addrs.AbsResourceInstance]()
	maybeOrphanAddrs := addrs.MakeSet[addrs.AbsResourceInstance]()
	func() {
		ss := globalCtx.PrevRunState()
		state := ss.Lock()
		defer ss.Unlock()

		for _, res := range state.Resources(n.Addr) {
		Instances:
			for instKey := range res.Instances {
				instAddr := res.Addr.Instance(instKey)

				for _, partialAddr := range partialExpandedAddrs {
					if partialAddr.MatchesInstance(instAddr) {
						// The instance is beneath a partial-expanded prefix, so
						// we can't decide yet whether it's an orphan or not,
						// but we'll still note it so we can make sure to
						// refresh its state.
						maybeOrphanAddrs.Add(instAddr)
						continue Instances
					}
				}
				if !knownAddrs.Has(instAddr) {
					// If we get here then the instance is not under an
					// partial-expanded prefix and is not in our set of
					// fully-known desired state instances, and so it's
					// an "orphan".
					orphanAddrs.Add(instAddr)
				}
			}
		}
	}()

	// TEMP: The code that deals with some other language/workflow features
	// is not yet updated to be able to handle partial-expanded resource
	// address prefixes, to constrain the scope of the initial experimental
	// implementation. We'll reject some of those cases with errors, just to
	// be explicit that they don't work rather than just quietly doing
	// something incomplete/broken/strange.
	if len(partialExpandedAddrs) != 0 {
		// Some other parts of the system aren't yet able to make sense of
		// partial-expanded resource addresses, so we'll forbid them for
		// now and improve on this in later iterations of the experiment.
		if len(n.Targets) != 0 {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Cannot use resource targeting with unknown count or for_each",
				"In the current phase of the unknown_instances language experiment, the -target=... planning option is not yet supported whenever unknown count or for_each are present.",
			))
		}
		if len(n.forceReplace) != 0 {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Cannot use forced replacement with unknown count or for_each",
				"In the current phase of the unknown_instances language experiment, the -replace=... planning option is not yet supported whenever unknown count or for_each are present.",
			))
		}
		if diags.HasErrors() {
			return nil, diags
		}
	}

	// At this point we have four different sets of resource instance
	// addresses:
	// - knownAddrs are definitely in the desired state. They may or may not
	//   also be in the previous run state.
	// - partialExpandedAddrs are unbounded sets of instances that _might_
	//   be in the desired state, but we can't know until a future round.
	// - orphanAddrs are in the previous run state but definitely not in
	//   the desired state.
	// - maybeOrphanAddrs are in the previous run state and we can't know
	//   whether they are in the desired state until a future round.
	//
	// Each resource instance in the union of all of the above sets needs to
	// be represented as part of _some_ graph node, but we'll build them
	// differently depending on which set they came from.
	for _, addr := range knownAddrs {
		log.Printf("[TRACE] nodeExpandPlannableResource: %s is definitely in the desired state", addr)
		v := &NodePlannableResourceInstance{
			NodeAbstractResourceInstance: NewNodeAbstractResourceInstance(addr),
			skipRefresh:                  n.skipRefresh,
			skipPlanChanges:              n.skipPlanChanges,
			forceReplace:                 n.forceReplace,
			// TODO: replaceTriggeredBy?
			// TODO: importTarget?
			// TODO: ForceCreateBeforeDestroy?
		}
		v.ResolvedProvider = n.ResolvedProvider
		v.Config = n.Config
		g.Add(v)
	}
	for _, addr := range partialExpandedAddrs {
		log.Printf("[TRACE] nodeExpandPlannableResource: desired instances matching %s are not yet known", addr)
		v := &nodePlannablePartialExpandedResource{
			addr:             addr,
			config:           n.Config,
			resolvedProvider: n.ResolvedProvider,
			skipPlanChanges:  n.skipPlanChanges,
		}
		g.Add(v)
	}
	for _, addr := range orphanAddrs {
		log.Printf("[TRACE] nodeExpandPlannableResource: %s is in previous state but no longer desired", addr)
		v := &NodePlannableResourceInstanceOrphan{
			NodeAbstractResourceInstance: NewNodeAbstractResourceInstance(addr),
			skipRefresh:                  n.skipRefresh,
			skipPlanChanges:              n.skipPlanChanges,
			// TODO: forgetResources?
			// TODO: forgetModules?
		}
		v.ResolvedProvider = n.ResolvedProvider
		v.Config = n.Config
		g.Add(v)
	}
	for _, addr := range maybeOrphanAddrs {
		// For any object in the previous run state where we cannot yet know
		// if it's an orphan, we can't yet properly plan it but we still
		// want to refresh it, in the same way we would if this were a
		// refresh-only plan.
		log.Printf("[TRACE] nodeExpandPlannableResource: %s is in previous state but unknown whether it's still desired", addr)
		v := &NodePlannableResourceInstance{
			NodeAbstractResourceInstance: NewNodeAbstractResourceInstance(addr),
			skipRefresh:                  n.skipRefresh,
			skipPlanChanges:              true, // We never plan for a "maybe-orphan"
			forceReplace:                 n.forceReplace,
			// TODO: replaceTriggeredBy?
			// TODO: importTarget?
			// TODO: ForceCreateBeforeDestroy?
		}
		v.ResolvedProvider = n.ResolvedProvider
		v.Config = n.Config
		g.Add(v)
	}

	addRootNodeToGraph(&g)
	return &g, diags
}

// validateExpandedImportTargets checks that all expanded imports correspond to
// a configured instance.
func (n *nodeExpandPlannableResource) validateExpandedImportTargets() tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	for _, addr := range n.expandedImports {
		if !n.expandedInstances.Has(addr) {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Configuration for import target does not exist",
				fmt.Sprintf("The configuration for the given import %s does not exist. All target instances must have an associated configuration to be imported.", addr),
			))
			return diags
		}
	}

	return diags
}

// expandResourceInstances calculates the dynamic expansion for the resource
// itself in the context of a particular module instance.
//
// It has several side-effects:
//   - Adds a node to Graph g for each leaf resource instance it discovers, whether present or orphaned.
//   - Registers the expansion of the resource in the "expander" object embedded inside EvalContext globalCtx.
//   - Adds each present (non-orphaned) resource instance address to checkableAddrs (guaranteed to always be addrs.AbsResourceInstance, despite being declared as addrs.Checkable).
//
// After calling this for each of the module instances the resource appears
// within, the caller must register the final superset instAddrs with the
// checks subsystem so that it knows the fully expanded set of checkable
// object instances for this resource instance.
func (n *nodeExpandPlannableResource) expandResourceInstances(globalCtx EvalContext, resAddr addrs.AbsResource, g *Graph) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	// The rest of our work here needs to know which module instance it's
	// working in, so that it can evaluate expressions in the appropriate scope.
	moduleCtx := evalContextForModuleInstance(globalCtx, resAddr.Module)

	// writeResourceState is responsible for informing the expander of what
	// repetition mode this resource has, which allows expander.ExpandResource
	// to work below.
	moreDiags := n.writeResourceState(moduleCtx, resAddr)
	diags = diags.Append(moreDiags)
	if moreDiags.HasErrors() {
		return diags
	}

	// Before we expand our resource into potentially many resource instances,
	// we'll verify that any mention of this resource in n.forceReplace is
	// consistent with the repetition mode of the resource. In other words,
	// we're aiming to catch a situation where naming a particular resource
	// instance would require an instance key but the given address has none.
	expander := moduleCtx.InstanceExpander()
	instanceAddrs := expander.ExpandResource(resAddr)

	// If there's a number of instances other than 1 then we definitely need
	// an index.
	mustHaveIndex := len(instanceAddrs) != 1
	// If there's only one instance then we might still need an index, if the
	// instance address has one.
	if len(instanceAddrs) == 1 && instanceAddrs[0].Resource.Key != addrs.NoKey {
		mustHaveIndex = true
	}
	if mustHaveIndex {
		for _, candidateAddr := range n.forceReplace {
			if candidateAddr.Resource.Key == addrs.NoKey {
				if n.Addr.Resource.Equal(candidateAddr.Resource.Resource) {
					switch {
					case len(instanceAddrs) == 0:
						// In this case there _are_ no instances to replace, so
						// there isn't any alternative address for us to suggest.
						diags = diags.Append(tfdiags.Sourceless(
							tfdiags.Warning,
							"Incompletely-matched force-replace resource instance",
							fmt.Sprintf(
								"Your force-replace request for %s doesn't match any resource instances because this resource doesn't have any instances.",
								candidateAddr,
							),
						))
					case len(instanceAddrs) == 1:
						diags = diags.Append(tfdiags.Sourceless(
							tfdiags.Warning,
							"Incompletely-matched force-replace resource instance",
							fmt.Sprintf(
								"Your force-replace request for %s doesn't match any resource instances because it lacks an instance key.\n\nTo force replacement of the single declared instance, use the following option instead:\n  -replace=%q",
								candidateAddr, instanceAddrs[0],
							),
						))
					default:
						var possibleValidOptions strings.Builder
						for _, addr := range instanceAddrs {
							fmt.Fprintf(&possibleValidOptions, "\n  -replace=%q", addr)
						}

						diags = diags.Append(tfdiags.Sourceless(
							tfdiags.Warning,
							"Incompletely-matched force-replace resource instance",
							fmt.Sprintf(
								"Your force-replace request for %s doesn't match any resource instances because it lacks an instance key.\n\nTo force replacement of particular instances, use one or more of the following options instead:%s",
								candidateAddr, possibleValidOptions.String(),
							),
						))
					}
				}
			}
		}
	}
	// NOTE: The actual interpretation of n.forceReplace to produce replace
	// actions is in the per-instance function we're about to call, because
	// we need to evaluate it on a per-instance basis.

	for _, addr := range instanceAddrs {
		// If this resource is participating in the "checks" mechanism then our
		// caller will need to know all of our expanded instance addresses as
		// checkable object instances.
		n.expandedInstances.Add(addr)
	}

	// Our graph builder mechanism expects to always be constructing new
	// graphs rather than adding to existing ones, so we'll first
	// construct a subgraph just for this individual modules's instances and
	// then we'll steal all of its nodes and edges to incorporate into our
	// main graph which contains all of the resource instances together.
	instG, instDiags := n.resourceInstanceSubgraph(moduleCtx, resAddr, instanceAddrs)
	if instDiags.HasErrors() {
		diags = diags.Append(instDiags)
		return diags
	}
	g.Subsume(&instG.AcyclicGraph.Graph)

	return diags
}

// Import blocks are expanded in conjunction with their associated resource block.
func (n nodeExpandPlannableResource) expandResourceImports(ctx EvalContext, addr addrs.AbsResource, instanceAddrs []addrs.AbsResourceInstance) (addrs.Map[addrs.AbsResourceInstance, string], tfdiags.Diagnostics) {
	// Imports maps the target address to an import ID.
	imports := addrs.MakeMap[addrs.AbsResourceInstance, string]()
	var diags tfdiags.Diagnostics

	if len(n.importTargets) == 0 {
		return imports, diags
	}

	// Import blocks are only valid within the root module, and must be
	// evaluated within that context
	ctx = evalContextForModuleInstance(ctx, addrs.RootModuleInstance)

	for _, imp := range n.importTargets {
		if imp.Config == nil {
			// if we have a legacy addr, it was supplied on the commandline so
			// there is nothing to expand
			if !imp.LegacyAddr.Equal(addrs.AbsResourceInstance{}) {
				imports.Put(imp.LegacyAddr, imp.IDString)
				n.expandedImports.Add(imp.LegacyAddr)
				return imports, diags
			}

			// legacy import tests may have no configuration
			log.Printf("[WARN] no configuration for import target %#v", imp)
			continue
		}

		if imp.Config.ForEach == nil {
			importID, evalDiags := evaluateImportIdExpression(imp.Config.ID, ctx, EvalDataForNoInstanceKey)
			diags = diags.Append(evalDiags)
			if diags.HasErrors() {
				return imports, diags
			}

			traversal, hds := hcl.AbsTraversalForExpr(imp.Config.To)
			diags = diags.Append(hds)
			to, tds := addrs.ParseAbsResourceInstance(traversal)
			diags = diags.Append(tds)
			if diags.HasErrors() {
				return imports, diags
			}

			imports.Put(to, importID)
			n.expandedImports.Add(to)

			log.Printf("[TRACE] expandResourceImports: found single import target %s", to)
			continue
		}

		forEachData, forEachDiags := newForEachEvaluator(imp.Config.ForEach, ctx, false).ImportValues()
		diags = diags.Append(forEachDiags)
		if forEachDiags.HasErrors() {
			return imports, diags
		}

		for _, keyData := range forEachData {
			res, evalDiags := evalImportToExpression(imp.Config.To, keyData)
			diags = diags.Append(evalDiags)
			if diags.HasErrors() {
				return imports, diags
			}

			importID, evalDiags := evaluateImportIdExpression(imp.Config.ID, ctx, keyData)
			diags = diags.Append(evalDiags)
			if diags.HasErrors() {
				return imports, diags
			}

			imports.Put(res, importID)
			n.expandedImports.Add(res)
			log.Printf("[TRACE] expandResourceImports: expanded import target %s", res)
		}
	}

	// filter out any import which already exist in state
	state := ctx.State()
	for _, el := range imports.Elements() {
		if state.ResourceInstance(el.Key) != nil {
			log.Printf("[DEBUG] expandResourceImports: skipping import address %s already in state", el.Key)
			imports.Remove(el.Key)
		}
	}

	return imports, diags
}

func (n *nodeExpandPlannableResource) resourceInstanceSubgraph(ctx EvalContext, addr addrs.AbsResource, instanceAddrs []addrs.AbsResourceInstance) (*Graph, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Now that the resources are all expanded, we can expand the imports for
	// this resource.
	imports, importDiags := n.expandResourceImports(ctx, addr, instanceAddrs)
	diags = diags.Append(importDiags)

	if n.Config == nil && n.generateConfigPath != "" && imports.Len() == 0 {
		// We're generating configuration, but there's nothing to import, which
		// means the import block must have expanded to zero instances.
		// the instance expander will always return a single instance because
		// we have assumed there will eventually be a configuration for this
		// resource, so return here before we add that to the graph.
		return &Graph{}, diags
	}

	// Our graph transformers require access to the full state, so we'll
	// temporarily lock it while we work on this.
	state := ctx.State().Lock()
	defer ctx.State().Unlock()

	// The concrete resource factory we'll use
	concreteResource := func(a *NodeAbstractResourceInstance) dag.Vertex {
		var m *NodePlannableResourceInstance

		// If we're in legacy import mode (the import CLI command), we only need
		// to return the import node, not a plannable resource node.
		for _, importTarget := range n.importTargets {
			if importTarget.LegacyAddr.Equal(a.Addr) {
				return &graphNodeImportState{
					Addr:             importTarget.LegacyAddr,
					ID:               imports.Get(importTarget.LegacyAddr),
					ResolvedProvider: n.ResolvedProvider,
				}
			}
		}

		// Add the config and state since we don't do that via transforms
		a.Config = n.Config
		a.ResolvedProvider = n.ResolvedProvider
		a.Schema = n.Schema
		a.ProvisionerSchemas = n.ProvisionerSchemas
		a.ProviderMetas = n.ProviderMetas
		a.dependsOn = n.dependsOn
		a.Dependencies = n.dependencies
		a.preDestroyRefresh = n.preDestroyRefresh
		a.generateConfigPath = n.generateConfigPath

		m = &NodePlannableResourceInstance{
			NodeAbstractResourceInstance: a,

			// By the time we're walking, we've figured out whether we need
			// to force on CreateBeforeDestroy due to dependencies on other
			// nodes that have it.
			ForceCreateBeforeDestroy: n.CreateBeforeDestroy(),
			skipRefresh:              n.skipRefresh,
			skipPlanChanges:          n.skipPlanChanges,
			forceReplace:             n.forceReplace,
		}

		importID, ok := imports.GetOk(a.Addr)
		if ok {
			m.importTarget = ImportTarget{
				IDString: importID,
			}
		}

		return m
	}

	// The concrete resource factory we'll use for orphans
	concreteResourceOrphan := func(a *NodeAbstractResourceInstance) dag.Vertex {
		// Add the config and state since we don't do that via transforms
		a.Config = n.Config
		a.ResolvedProvider = n.ResolvedProvider
		a.Schema = n.Schema
		a.ProvisionerSchemas = n.ProvisionerSchemas
		a.ProviderMetas = n.ProviderMetas

		return &NodePlannableResourceInstanceOrphan{
			NodeAbstractResourceInstance: a,
			skipRefresh:                  n.skipRefresh,
			skipPlanChanges:              n.skipPlanChanges,
		}
	}

	// Start creating the steps
	steps := []GraphTransformer{
		// Expand the count or for_each (if present)
		&ResourceCountTransformer{
			Concrete:      concreteResource,
			Schema:        n.Schema,
			Addr:          n.ResourceAddr(),
			InstanceAddrs: instanceAddrs,
		},

		// Add the count/for_each orphans
		&OrphanResourceInstanceCountTransformer{
			Concrete:      concreteResourceOrphan,
			Addr:          addr,
			InstanceAddrs: instanceAddrs,
			State:         state,
		},

		// Attach the state
		&AttachStateTransformer{State: state},

		// Targeting
		&TargetsTransformer{Targets: n.Targets},

		// Connect references so ordering is correct
		&ReferenceTransformer{},

		// Make sure there is a single root
		&RootTransformer{},
	}

	// Build the graph
	b := &BasicGraphBuilder{
		Steps: steps,
		Name:  "nodeExpandPlannableResource",
	}
	graph, graphDiags := b.Build(addr.Module)
	diags = diags.Append(graphDiags)

	return graph, diags
}
