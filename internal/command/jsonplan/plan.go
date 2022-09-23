package jsonplan

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/checks"
	"github.com/hashicorp/terraform/internal/command/jsonchecks"
	"github.com/hashicorp/terraform/internal/command/jsonconfig"
	"github.com/hashicorp/terraform/internal/command/jsonstate"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/version"
)

// FormatVersion represents the version of the json format and will be
// incremented for any change to this format that requires changes to a
// consuming parser.
const FormatVersion = "1.1"

// Plan is the top-level representation of the json format of a plan. It includes
// the complete config and current state.
type plan struct {
	FormatVersion    string      `json:"format_version,omitempty"`
	TerraformVersion string      `json:"terraform_version,omitempty"`
	Variables        variables   `json:"variables,omitempty"`
	PlannedValues    stateValues `json:"planned_values,omitempty"`
	// ResourceDrift and ResourceChanges are sorted in a user-friendly order
	// that is undefined at this time, but consistent.
	ResourceDrift      []resourceChange  `json:"resource_drift,omitempty"`
	ResourceChanges    []resourceChange  `json:"resource_changes,omitempty"`
	OutputChanges      map[string]change `json:"output_changes,omitempty"`
	PriorState         json.RawMessage   `json:"prior_state,omitempty"`
	Config             json.RawMessage   `json:"configuration,omitempty"`
	RelevantAttributes []resourceAttr    `json:"relevant_attributes,omitempty"`
	Conditions         []conditionResult `json:"condition_results,omitempty"`
	Checks             json.RawMessage   `json:"checks,omitempty"`
}

func newPlan() *plan {
	return &plan{
		FormatVersion: FormatVersion,
	}
}

// resourceAttr contains the address and attribute of an external for the
// RelevantAttributes in the plan.
type resourceAttr struct {
	Resource string          `json:"resource"`
	Attr     json.RawMessage `json:"attribute"`
}

// Change is the representation of a proposed change for an object.
type change struct {
	// Actions are the actions that will be taken on the object selected by the
	// properties below. Valid actions values are:
	//    ["no-op"]
	//    ["create"]
	//    ["read"]
	//    ["update"]
	//    ["delete", "create"]
	//    ["create", "delete"]
	//    ["delete"]
	// The two "replace" actions are represented in this way to allow callers to
	// e.g. just scan the list for "delete" to recognize all three situations
	// where the object will be deleted, allowing for any new deletion
	// combinations that might be added in future.
	Actions []string `json:"actions,omitempty"`

	// Before and After are representations of the object value both before and
	// after the action. For ["create"] and ["delete"] actions, either "before"
	// or "after" is unset (respectively). For ["no-op"], the before and after
	// values are identical. The "after" value will be incomplete if there are
	// values within it that won't be known until after apply.
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`

	// AfterUnknown is an object value with similar structure to After, but
	// with all unknown leaf values replaced with true, and all known leaf
	// values omitted.  This can be combined with After to reconstruct a full
	// value after the action, including values which will only be known after
	// apply.
	AfterUnknown json.RawMessage `json:"after_unknown,omitempty"`

	// BeforeSensitive and AfterSensitive are object values with similar
	// structure to Before and After, but with all sensitive leaf values
	// replaced with true, and all non-sensitive leaf values omitted. These
	// objects should be combined with Before and After to prevent accidental
	// display of sensitive values in user interfaces.
	BeforeSensitive json.RawMessage `json:"before_sensitive,omitempty"`
	AfterSensitive  json.RawMessage `json:"after_sensitive,omitempty"`

	// ReplacePaths is an array of arrays representing a set of paths into the
	// object value which resulted in the action being "replace". This will be
	// omitted if the action is not replace, or if no paths caused the
	// replacement (for example, if the resource was tainted). Each path
	// consists of one or more steps, each of which will be a number or a
	// string.
	ReplacePaths json.RawMessage `json:"replace_paths,omitempty"`
}

type output struct {
	Sensitive bool            `json:"sensitive"`
	Type      json.RawMessage `json:"type,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// variables is the JSON representation of the variables provided to the current
// plan.
type variables map[string]*variable

type variable struct {
	Value json.RawMessage `json:"value,omitempty"`
}

// Marshal returns the json encoding of a terraform plan.
func Marshal(
	config *configs.Config,
	p *plans.Plan,
	sf *statefile.File,
	schemas *terraform.Schemas,
) ([]byte, error) {
	output := newPlan()
	output.TerraformVersion = version.String()

	err := output.marshalPlanVariables(p.VariableValues, config.Module.Variables)
	if err != nil {
		return nil, fmt.Errorf("error in marshalPlanVariables: %s", err)
	}

	// output.PlannedValues
	err = output.marshalPlannedValues(p.Changes, schemas)
	if err != nil {
		return nil, fmt.Errorf("error in marshalPlannedValues: %s", err)
	}

	// output.ResourceDrift
	if len(p.DriftedResources) > 0 {
		// In refresh-only mode, we render all resources marked as drifted,
		// including those which have moved without other changes. In other plan
		// modes, move-only changes will be included in the planned changes, so
		// we skip them here.
		var driftedResources []*plans.ResourceInstanceChangeSrc
		if p.UIMode == plans.RefreshOnlyMode {
			driftedResources = p.DriftedResources
		} else {
			for _, dr := range p.DriftedResources {
				if dr.Action != plans.NoOp {
					driftedResources = append(driftedResources, dr)
				}
			}
		}
		output.ResourceDrift, err = output.marshalResourceChanges(driftedResources, schemas)
		if err != nil {
			return nil, fmt.Errorf("error in marshaling resource drift: %s", err)
		}
	}

	if err := output.marshalRelevantAttrs(p); err != nil {
		return nil, fmt.Errorf("error marshaling relevant attributes for external changes: %s", err)
	}

	// output.ResourceChanges
	if p.Changes != nil {
		output.ResourceChanges, err = output.marshalResourceChanges(p.Changes.Resources, schemas)
		if err != nil {
			return nil, fmt.Errorf("error in marshaling resource changes: %s", err)
		}
	}

	// output.OutputChanges
	err = output.marshalOutputChanges(p.Changes)
	if err != nil {
		return nil, fmt.Errorf("error in marshaling output changes: %s", err)
	}

	// output.Conditions (deprecated in favor of Checks, below)
	err = output.marshalCheckResults(p.Checks)
	if err != nil {
		return nil, fmt.Errorf("error in marshaling check results: %s", err)
	}

	// output.Checks
	if p.Checks != nil && p.Checks.ConfigResults.Len() > 0 {
		output.Checks = jsonchecks.MarshalCheckStates(p.Checks)
	}

	// output.PriorState
	if sf != nil && !sf.State.Empty() {
		output.PriorState, err = jsonstate.Marshal(sf, schemas)
		if err != nil {
			return nil, fmt.Errorf("error marshaling prior state: %s", err)
		}
	}

	// output.Config
	output.Config, err = jsonconfig.Marshal(config, schemas)
	if err != nil {
		return nil, fmt.Errorf("error marshaling config: %s", err)
	}

	ret, err := json.Marshal(output)
	return ret, err
}

func (p *plan) marshalPlanVariables(vars map[string]plans.DynamicValue, decls map[string]*configs.Variable) error {
	p.Variables = make(variables, len(vars))

	for k, v := range vars {
		val, err := v.Decode(cty.DynamicPseudoType)
		if err != nil {
			return err
		}
		valJSON, err := ctyjson.Marshal(val, val.Type())
		if err != nil {
			return err
		}
		p.Variables[k] = &variable{
			Value: valJSON,
		}
	}

	// In Terraform v1.1 and earlier we had some confusion about which subsystem
	// of Terraform was the one responsible for substituting in default values
	// for unset module variables, with root module variables being handled in
	// three different places while child module variables were only handled
	// during the Terraform Core graph walk.
	//
	// For Terraform v1.2 and later we rationalized that by having the Terraform
	// Core graph walk always be responsible for selecting defaults regardless
	// of root vs. child module, but unfortunately our earlier accidental
	// misbehavior bled out into the public interface by making the defaults
	// show up in the "vars" map to this function. Those are now correctly
	// omitted (so that the plan file only records the variables _actually_
	// set by the caller) but consumers of the JSON plan format may be depending
	// on our old behavior and so we'll fake it here just in time so that
	// outside consumers won't see a behavior change.
	for name, decl := range decls {
		if _, ok := p.Variables[name]; ok {
			continue
		}
		if val := decl.Default; val != cty.NilVal {
			valJSON, err := ctyjson.Marshal(val, val.Type())
			if err != nil {
				return err
			}
			p.Variables[name] = &variable{
				Value: valJSON,
			}
		}
	}

	if len(p.Variables) == 0 {
		p.Variables = nil // omit this property if there are no variables to describe
	}

	return nil
}

func (p *plan) marshalResourceChanges(resources []*plans.ResourceInstanceChangeSrc, schemas *terraform.Schemas) ([]resourceChange, error) {
	var ret []resourceChange

	for _, rc := range resources {
		var r resourceChange
		addr := rc.Addr
		r.Address = addr.String()
		if !addr.Equal(rc.PrevRunAddr) {
			r.PreviousAddress = rc.PrevRunAddr.String()
		}

		dataSource := addr.Resource.Resource.Mode == addrs.DataResourceMode
		// We create "delete" actions for data resources so we can clean up
		// their entries in state, but this is an implementation detail that
		// users shouldn't see.
		if dataSource && rc.Action == plans.Delete {
			continue
		}

		schema, _ := schemas.ResourceTypeConfig(
			rc.ProviderAddr.Provider,
			addr.Resource.Resource.Mode,
			addr.Resource.Resource.Type,
		)
		if schema == nil {
			return nil, fmt.Errorf("no schema found for %s (in provider %s)", r.Address, rc.ProviderAddr.Provider)
		}

		changeV, err := rc.Decode(schema.ImpliedType())
		if err != nil {
			return nil, err
		}
		// We drop the marks from the change, as decoding is only an
		// intermediate step to re-encode the values as json
		changeV.Before, _ = changeV.Before.UnmarkDeep()
		changeV.After, _ = changeV.After.UnmarkDeep()

		var before, after []byte
		var beforeSensitive, afterSensitive []byte
		var afterUnknown cty.Value

		if changeV.Before != cty.NilVal {
			before, err = ctyjson.Marshal(changeV.Before, changeV.Before.Type())
			if err != nil {
				return nil, err
			}
			marks := rc.BeforeValMarks
			if schema.ContainsSensitive() {
				marks = append(marks, schema.ValueMarks(changeV.Before, nil)...)
			}
			bs := jsonstate.SensitiveAsBool(changeV.Before.MarkWithPaths(marks))
			beforeSensitive, err = ctyjson.Marshal(bs, bs.Type())
			if err != nil {
				return nil, err
			}
		}
		if changeV.After != cty.NilVal {
			if changeV.After.IsWhollyKnown() {
				after, err = ctyjson.Marshal(changeV.After, changeV.After.Type())
				if err != nil {
					return nil, err
				}
				afterUnknown = cty.EmptyObjectVal
			} else {
				filteredAfter := omitUnknowns(changeV.After)
				if filteredAfter.IsNull() {
					after = nil
				} else {
					after, err = ctyjson.Marshal(filteredAfter, filteredAfter.Type())
					if err != nil {
						return nil, err
					}
				}
				afterUnknown = unknownAsBool(changeV.After)
			}
			marks := rc.AfterValMarks
			if schema.ContainsSensitive() {
				marks = append(marks, schema.ValueMarks(changeV.After, nil)...)
			}
			as := jsonstate.SensitiveAsBool(changeV.After.MarkWithPaths(marks))
			afterSensitive, err = ctyjson.Marshal(as, as.Type())
			if err != nil {
				return nil, err
			}
		}

		a, err := ctyjson.Marshal(afterUnknown, afterUnknown.Type())
		if err != nil {
			return nil, err
		}
		replacePaths, err := encodePaths(rc.RequiredReplace)
		if err != nil {
			return nil, err
		}

		r.Change = change{
			Actions:         actionString(rc.Action.String()),
			Before:          json.RawMessage(before),
			After:           json.RawMessage(after),
			AfterUnknown:    a,
			BeforeSensitive: json.RawMessage(beforeSensitive),
			AfterSensitive:  json.RawMessage(afterSensitive),
			ReplacePaths:    replacePaths,
		}

		if rc.DeposedKey != states.NotDeposed {
			r.Deposed = rc.DeposedKey.String()
		}

		key := addr.Resource.Key
		if key != nil {
			r.Index = key
		}

		switch addr.Resource.Resource.Mode {
		case addrs.ManagedResourceMode:
			r.Mode = "managed"
		case addrs.DataResourceMode:
			r.Mode = "data"
		default:
			return nil, fmt.Errorf("resource %s has an unsupported mode %s", r.Address, addr.Resource.Resource.Mode.String())
		}
		r.ModuleAddress = addr.Module.String()
		r.Name = addr.Resource.Resource.Name
		r.Type = addr.Resource.Resource.Type
		r.ProviderName = rc.ProviderAddr.Provider.String()

		switch rc.ActionReason {
		case plans.ResourceInstanceChangeNoReason:
			r.ActionReason = "" // will be omitted in output
		case plans.ResourceInstanceReplaceBecauseCannotUpdate:
			r.ActionReason = "replace_because_cannot_update"
		case plans.ResourceInstanceReplaceBecauseTainted:
			r.ActionReason = "replace_because_tainted"
		case plans.ResourceInstanceReplaceByRequest:
			r.ActionReason = "replace_by_request"
		case plans.ResourceInstanceReplaceByTriggers:
			r.ActionReason = "replace_by_triggers"
		case plans.ResourceInstanceDeleteBecauseNoResourceConfig:
			r.ActionReason = "delete_because_no_resource_config"
		case plans.ResourceInstanceDeleteBecauseWrongRepetition:
			r.ActionReason = "delete_because_wrong_repetition"
		case plans.ResourceInstanceDeleteBecauseCountIndex:
			r.ActionReason = "delete_because_count_index"
		case plans.ResourceInstanceDeleteBecauseEachKey:
			r.ActionReason = "delete_because_each_key"
		case plans.ResourceInstanceDeleteBecauseNoModule:
			r.ActionReason = "delete_because_no_module"
		case plans.ResourceInstanceDeleteBecauseNoMoveTarget:
			r.ActionReason = "delete_because_no_move_target"
		case plans.ResourceInstanceReadBecauseConfigUnknown:
			r.ActionReason = "read_because_config_unknown"
		case plans.ResourceInstanceReadBecauseDependencyPending:
			r.ActionReason = "read_because_dependency_pending"
		default:
			return nil, fmt.Errorf("resource %s has an unsupported action reason %s", r.Address, rc.ActionReason)
		}

		ret = append(ret, r)

	}

	sort.Slice(ret, func(i, j int) bool {
		return ret[i].Address < ret[j].Address
	})

	return ret, nil
}

func (p *plan) marshalOutputChanges(changes *plans.Changes) error {
	if changes == nil {
		// Nothing to do!
		return nil
	}

	p.OutputChanges = make(map[string]change, len(changes.Outputs))
	for _, oc := range changes.Outputs {
		changeV, err := oc.Decode()
		if err != nil {
			return err
		}
		// We drop the marks from the change, as decoding is only an
		// intermediate step to re-encode the values as json
		changeV.Before, _ = changeV.Before.UnmarkDeep()
		changeV.After, _ = changeV.After.UnmarkDeep()

		var before, after []byte
		var afterUnknown cty.Value

		if changeV.Before != cty.NilVal {
			before, err = ctyjson.Marshal(changeV.Before, changeV.Before.Type())
			if err != nil {
				return err
			}
		}
		if changeV.After != cty.NilVal {
			if changeV.After.IsWhollyKnown() {
				after, err = ctyjson.Marshal(changeV.After, changeV.After.Type())
				if err != nil {
					return err
				}
				afterUnknown = cty.False
			} else {
				filteredAfter := omitUnknowns(changeV.After)
				if filteredAfter.IsNull() {
					after = nil
				} else {
					after, err = ctyjson.Marshal(filteredAfter, filteredAfter.Type())
					if err != nil {
						return err
					}
				}
				afterUnknown = unknownAsBool(changeV.After)
			}
		}

		// The only information we have in the plan about output sensitivity is
		// a boolean which is true if the output was or is marked sensitive. As
		// a result, BeforeSensitive and AfterSensitive will be identical, and
		// either false or true.
		outputSensitive := cty.False
		if oc.Sensitive {
			outputSensitive = cty.True
		}
		sensitive, err := ctyjson.Marshal(outputSensitive, outputSensitive.Type())
		if err != nil {
			return err
		}

		a, _ := ctyjson.Marshal(afterUnknown, afterUnknown.Type())

		c := change{
			Actions:         actionString(oc.Action.String()),
			Before:          json.RawMessage(before),
			After:           json.RawMessage(after),
			AfterUnknown:    a,
			BeforeSensitive: json.RawMessage(sensitive),
			AfterSensitive:  json.RawMessage(sensitive),
		}

		p.OutputChanges[oc.Addr.OutputValue.Name] = c
	}

	return nil
}

func (p *plan) marshalCheckResults(results *states.CheckResults) error {
	if results == nil {
		return nil
	}

	// For the moment this is still producing the flat structure from
	// the initial release of preconditions/postconditions in Terraform v1.2.
	// This therefore discards the aggregate information about any configuration
	// objects that might end up with zero instances declared.
	// We'll need to think about what we want to do here in order to expose
	// the full check details while hopefully also remaining compatible with
	// what we previously documented.

	for _, configElem := range results.ConfigResults.Elems {
		for _, objectElem := range configElem.Value.ObjectResults.Elems {
			objectAddr := objectElem.Key
			result := objectElem.Value

			var boolResult, unknown bool
			switch result.Status {
			case checks.StatusPass:
				boolResult = true
			case checks.StatusFail:
				boolResult = false
			case checks.StatusError:
				boolResult = false
			case checks.StatusUnknown:
				unknown = true
			}

			// We need to export one of the previously-documented condition
			// types here even though we're no longer actually representing
			// individual checks, so we'll fib a bit and just report a
			// fixed string depending on the object type. Note that this
			// means we'll report that a resource postcondition failed even
			// if it was actually a precondition, which is non-ideal but
			// hopefully we replace this with an object-first data structure
			// in the near future.
			fakeCheckType := "Condition"
			switch objectAddr.(type) {
			case addrs.AbsResourceInstance:
				fakeCheckType = "ResourcePostcondition"
			case addrs.AbsOutputValue:
				fakeCheckType = "OutputPrecondition"
			}

			// NOTE: Our original design for this data structure exposed
			// each declared check individually, but checks don't really
			// have durable addresses between runs so we've now simplified
			// the model to say that it's entire objects that pass or fail,
			// via the combination of all of their checks.
			//
			// The public data structure for this was built around the
			// original design and so we approximate that here by
			// generating only a single "condition" per object in most cases,
			// but will generate one for each error message if we're
			// reporting a failure and we have at least one message.
			if result.Status == checks.StatusFail && len(result.FailureMessages) != 0 {
				for _, msg := range result.FailureMessages {
					p.Conditions = append(p.Conditions, conditionResult{
						Address:      objectAddr.String(),
						Type:         fakeCheckType,
						Result:       boolResult,
						Unknown:      unknown,
						ErrorMessage: msg,
					})
				}
			} else {
				p.Conditions = append(p.Conditions, conditionResult{
					Address: objectAddr.String(),
					Type:    fakeCheckType,
					Result:  boolResult,
					Unknown: unknown,
				})
			}
		}
	}

	sort.Slice(p.Conditions, func(i, j int) bool {
		return p.Conditions[i].Address < p.Conditions[j].Address
	})
	return nil
}

func (p *plan) marshalPlannedValues(changes *plans.Changes, schemas *terraform.Schemas) error {
	// marshal the planned changes into a module
	plan, err := marshalPlannedValues(changes, schemas)
	if err != nil {
		return err
	}
	p.PlannedValues.RootModule = plan

	// marshalPlannedOutputs
	outputs, err := marshalPlannedOutputs(changes)
	if err != nil {
		return err
	}
	p.PlannedValues.Outputs = outputs

	return nil
}

func (p *plan) marshalRelevantAttrs(plan *plans.Plan) error {
	for _, ra := range plan.RelevantAttributes {
		addr := ra.Resource.String()
		path, err := encodePath(ra.Attr)
		if err != nil {
			return err
		}

		p.RelevantAttributes = append(p.RelevantAttributes, resourceAttr{addr, path})
	}
	return nil
}

// omitUnknowns recursively walks the src cty.Value and returns a new cty.Value,
// omitting any unknowns.
//
// The result also normalizes some types: all sequence types are turned into
// tuple types and all mapping types are converted to object types, since we
// assume the result of this is just going to be serialized as JSON (and thus
// lose those distinctions) anyway.
func omitUnknowns(val cty.Value) cty.Value {
	ty := val.Type()
	switch {
	case val.IsNull():
		return val
	case !val.IsKnown():
		return cty.NilVal
	case ty.IsPrimitiveType():
		return val
	case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
		var vals []cty.Value
		it := val.ElementIterator()
		for it.Next() {
			_, v := it.Element()
			newVal := omitUnknowns(v)
			if newVal != cty.NilVal {
				vals = append(vals, newVal)
			} else if newVal == cty.NilVal {
				// element order is how we correlate unknownness, so we must
				// replace unknowns with nulls
				vals = append(vals, cty.NullVal(v.Type()))
			}
		}
		// We use tuple types always here, because the work we did above
		// may have caused the individual elements to have different types,
		// and we're doing this work to produce JSON anyway and JSON marshalling
		// represents all of these sequence types as an array.
		return cty.TupleVal(vals)
	case ty.IsMapType() || ty.IsObjectType():
		vals := make(map[string]cty.Value)
		it := val.ElementIterator()
		for it.Next() {
			k, v := it.Element()
			newVal := omitUnknowns(v)
			if newVal != cty.NilVal {
				vals[k.AsString()] = newVal
			}
		}
		// We use object types always here, because the work we did above
		// may have caused the individual elements to have different types,
		// and we're doing this work to produce JSON anyway and JSON marshalling
		// represents both of these mapping types as an object.
		return cty.ObjectVal(vals)
	default:
		// Should never happen, since the above should cover all types
		panic(fmt.Sprintf("omitUnknowns cannot handle %#v", val))
	}
}

// recursively iterate through a cty.Value, replacing unknown values (including
// null) with cty.True and known values with cty.False.
//
// The result also normalizes some types: all sequence types are turned into
// tuple types and all mapping types are converted to object types, since we
// assume the result of this is just going to be serialized as JSON (and thus
// lose those distinctions) anyway.
//
// For map/object values, all known attribute values will be omitted instead of
// returning false, as this results in a more compact serialization.
func unknownAsBool(val cty.Value) cty.Value {
	ty := val.Type()
	switch {
	case val.IsNull():
		return cty.False
	case !val.IsKnown():
		if ty.IsPrimitiveType() || ty.Equals(cty.DynamicPseudoType) {
			return cty.True
		}
		fallthrough
	case ty.IsPrimitiveType():
		return cty.BoolVal(!val.IsKnown())
	case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
		length := val.LengthInt()
		if length == 0 {
			// If there are no elements then we can't have unknowns
			return cty.EmptyTupleVal
		}
		vals := make([]cty.Value, 0, length)
		it := val.ElementIterator()
		for it.Next() {
			_, v := it.Element()
			vals = append(vals, unknownAsBool(v))
		}
		// The above transform may have changed the types of some of the
		// elements, so we'll always use a tuple here in case we've now made
		// different elements have different types. Our ultimate goal is to
		// marshal to JSON anyway, and all of these sequence types are
		// indistinguishable in JSON.
		return cty.TupleVal(vals)
	case ty.IsMapType() || ty.IsObjectType():
		var length int
		switch {
		case ty.IsMapType():
			length = val.LengthInt()
		default:
			length = len(val.Type().AttributeTypes())
		}
		if length == 0 {
			// If there are no elements then we can't have unknowns
			return cty.EmptyObjectVal
		}
		vals := make(map[string]cty.Value)
		it := val.ElementIterator()
		for it.Next() {
			k, v := it.Element()
			vAsBool := unknownAsBool(v)
			// Omit all of the "false"s for known values for more compact
			// serialization
			if !vAsBool.RawEquals(cty.False) {
				vals[k.AsString()] = vAsBool
			}
		}
		// The above transform may have changed the types of some of the
		// elements, so we'll always use an object here in case we've now made
		// different elements have different types. Our ultimate goal is to
		// marshal to JSON anyway, and all of these mapping types are
		// indistinguishable in JSON.
		return cty.ObjectVal(vals)
	default:
		// Should never happen, since the above should cover all types
		panic(fmt.Sprintf("unknownAsBool cannot handle %#v", val))
	}
}

func actionString(action string) []string {
	switch {
	case action == "NoOp":
		return []string{"no-op"}
	case action == "Create":
		return []string{"create"}
	case action == "Delete":
		return []string{"delete"}
	case action == "Update":
		return []string{"update"}
	case action == "CreateThenDelete":
		return []string{"create", "delete"}
	case action == "Read":
		return []string{"read"}
	case action == "DeleteThenCreate":
		return []string{"delete", "create"}
	default:
		return []string{action}
	}
}

// encodePaths lossily encodes a cty.PathSet into an array of arrays of step
// values, such as:
//
//	[["length"],["triggers",0,"value"]]
//
// The lossiness is that we cannot distinguish between an IndexStep with string
// key and a GetAttr step. This is fine with JSON output, because JSON's type
// system means that those two steps are equivalent anyway: both are object
// indexes.
//
// JavaScript (or similar dynamic language) consumers of these values can
// iterate over the the steps starting from the root object to reach the
// value that each path is describing.
func encodePaths(pathSet cty.PathSet) (json.RawMessage, error) {
	if pathSet.Empty() {
		return nil, nil
	}

	pathList := pathSet.List()
	jsonPaths := make([]json.RawMessage, 0, len(pathList))

	for _, path := range pathList {
		jsonPath, err := encodePath(path)
		if err != nil {
			return nil, err
		}
		jsonPaths = append(jsonPaths, jsonPath)
	}

	return json.Marshal(jsonPaths)
}

func encodePath(path cty.Path) (json.RawMessage, error) {
	steps := make([]json.RawMessage, 0, len(path))
	for _, step := range path {
		switch s := step.(type) {
		case cty.IndexStep:
			key, err := ctyjson.Marshal(s.Key, s.Key.Type())
			if err != nil {
				return nil, fmt.Errorf("Failed to marshal index step key %#v: %s", s.Key, err)
			}
			steps = append(steps, key)
		case cty.GetAttrStep:
			name, err := json.Marshal(s.Name)
			if err != nil {
				return nil, fmt.Errorf("Failed to marshal get attr step name %#v: %s", s.Name, err)
			}
			steps = append(steps, name)
		default:
			return nil, fmt.Errorf("Unsupported path step %#v (%t)", step, step)
		}
	}
	return json.Marshal(steps)
}
