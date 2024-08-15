// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty-debug/ctydebug"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	testing_provider "github.com/hashicorp/terraform/internal/providers/testing"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

type deferredActionsTest struct {
	// If true, this test will be skipped.
	skip bool

	// The configuration to use for this test. The keys are the filenames.
	configs map[string]string

	// The starting state for the first stage. This can be nil, and the test
	// will create a new empty state if so.
	state *states.State

	// This test will execute a plan-apply cycle for every entry in this
	// slice. At each stage the plan and apply outputs will be validated
	// against the expected values.
	stages []deferredActionsTestStage
}

type deferredActionsTestStage struct {
	// The inputs at each plan-apply cycle.
	inputs map[string]cty.Value

	// The values we want to be planned within each cycle.
	wantPlanned map[string]cty.Value

	// The values we want to be deferred within each cycle.
	wantDeferred map[string]ExpectedDeferred

	// The expected actions from the plan step.
	wantActions map[string]plans.Action

	// The values we want to be applied during each cycle. If this is
	// nil, then the apply step will be skipped.
	wantApplied map[string]cty.Value

	// The values we want to be returned by the outputs. If applied is
	// nil, then this should also be nil as the apply step will be
	// skipped.
	wantOutputs map[string]cty.Value

	// Whether the plan should be completed during this stage.
	complete bool

	// Some of our tests produce expected warnings, set this to true to allow
	// warnings to be present in the returned diagnostics.
	allowWarnings bool

	// buildOpts is an optional field, that lets the test specify additional
	// options to be used when building the plan.
	buildOpts func(opts *PlanOpts)

	// wantDiagnostic is an optional field, that lets the test specify the
	// expected diagnostics to be returned by the plan.
	wantDiagnostic func(diags tfdiags.Diagnostics) bool
}

type ExpectedDeferred struct {
	Reason providers.DeferredReason
	Action plans.Action
}

var (
	// We build some fairly complex configurations here, so we'll use separate
	// variables for each one outside of the test function itself for clarity.

	// dataForEachTest is a test for deferral of data sources due to unknown
	// for_each values. Since data sources don't result in planned changes,
	// deferral has to be observed indirectly by checking for deferral of
	// downstream objects that would otherwise have no reason to be deferred.
	dataForEachTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "each" {
	type = set(string)
}

data "test" "a" {
	for_each = var.each

	name = "a:${each.key}"
}

resource "test" "b" {
	name = "b"
	upstream_names = [for v in data.test.a : v.name]
}

output "from_data" {
	value = [for v in data.test.a : v.output]
}

output "from_resource" {
	value = test.b.output
}
`,
		},
		stages: []deferredActionsTestStage{
			// Stage 0. Unknown for_each in data source. The resource and
			// outputs get transitively deferred.
			{
				inputs: map[string]cty.Value{
					"each": cty.DynamicVal,
				},
				wantPlanned: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"output":         cty.UnknownVal(cty.String),
						"upstream_names": cty.UnknownVal(cty.Set(cty.String)),
					}),
				},
				wantActions: map[string]plans.Action{},
				wantDeferred: map[string]ExpectedDeferred{
					"test.b": {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{},
				// TODO: These deferred output values are wrong, but outputs are a separate ticket.
				wantOutputs: map[string]cty.Value{
					"from_data":     cty.EmptyTupleVal,
					"from_resource": cty.NullVal(cty.DynamicPseudoType),
				},
				complete:      false,
				allowWarnings: false,
			},
			// Stage 1. Everything's known now, so it converges.
			{
				inputs: map[string]cty.Value{
					"each": cty.SetVal([]cty.Value{cty.StringVal("hey"), cty.StringVal("ho"), cty.StringVal("let's go")}),
				},
				wantPlanned: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"output":         cty.UnknownVal(cty.String),
						"upstream_names": cty.SetVal([]cty.Value{cty.StringVal("a:hey"), cty.StringVal("a:ho"), cty.StringVal("a:let's go")}),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				wantApplied: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"output":         cty.StringVal("b"),
						"upstream_names": cty.SetVal([]cty.Value{cty.StringVal("a:hey"), cty.StringVal("a:ho"), cty.StringVal("a:let's go")}),
					}),
				},
				wantOutputs: map[string]cty.Value{
					"from_data":     cty.TupleVal([]cty.Value{cty.StringVal("a:hey"), cty.StringVal("a:ho"), cty.StringVal("a:let's go")}),
					"from_resource": cty.StringVal("b"),
				},
				complete:      true,
				allowWarnings: false,
			},
		},
	}

	// dataCountTest is a test for deferral of data sources due to unknown
	// count values. Since data sources don't result in planned changes,
	// deferral has to be observed indirectly by checking for deferral of
	// downstream objects that would otherwise have no reason to be deferred.
	dataCountTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "data_count" {
	type = number
}

data "test" "a" {
	count = var.data_count

	name = "a:${count.index}"
}

resource "test" "b" {
	name = "b"
	upstream_names = [for v in data.test.a : v.name]
}

output "from_data" {
	value = [for v in data.test.a : v.output]
}

output "from_resource" {
	value = test.b.output
}
`,
		},
		stages: []deferredActionsTestStage{
			// Stage 0. Unknown count in data source. The resource and
			// outputs get transitively deferred.
			{
				inputs: map[string]cty.Value{
					"data_count": cty.DynamicVal,
				},
				wantPlanned: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"output":         cty.UnknownVal(cty.String),
						"upstream_names": cty.UnknownVal(cty.Set(cty.String)),
					}),
				},
				wantActions: map[string]plans.Action{},
				wantDeferred: map[string]ExpectedDeferred{
					"test.b": {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{},
				// TODO: These deferred output values are wrong, but outputs are a separate ticket.
				wantOutputs: map[string]cty.Value{
					"from_data":     cty.EmptyTupleVal,
					"from_resource": cty.NullVal(cty.DynamicPseudoType),
				},
				complete:      false,
				allowWarnings: false,
			},
			// Stage 1. Everything's known now, so it converges.
			{
				inputs: map[string]cty.Value{
					"data_count": cty.NumberIntVal(3),
				},
				wantPlanned: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"output":         cty.UnknownVal(cty.String),
						"upstream_names": cty.SetVal([]cty.Value{cty.StringVal("a:0"), cty.StringVal("a:1"), cty.StringVal("a:2")}),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				wantApplied: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"output":         cty.StringVal("b"),
						"upstream_names": cty.SetVal([]cty.Value{cty.StringVal("a:0"), cty.StringVal("a:1"), cty.StringVal("a:2")}),
					}),
				},
				wantOutputs: map[string]cty.Value{
					"from_data":     cty.TupleVal([]cty.Value{cty.StringVal("a:0"), cty.StringVal("a:1"), cty.StringVal("a:2")}),
					"from_resource": cty.StringVal("b"),
				},
				complete:      true,
				allowWarnings: false,
			},
		},
	}

	// resourceForEachTest is a test that exercises the deferred actions
	// mechanism with a configuration that has a resource with an unknown
	// for_each attribute.
	//
	// We execute three plan-apply cycles. The first one with an unknown input
	// into the for_each. The second with a known for_each value. The final
	// with the same known for_each value to ensure that the plan is empty.
	resourceForEachTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "each" {
	type = set(string)
}

resource "test" "a" {
	name = "a"
}

resource "test" "b" {
	for_each = var.each

	name           = "b:${each.key}"
	upstream_names = [test.a.name]
}

resource "test" "c" {
	name = "c"
	upstream_names = setunion(
		[for v in test.b : v.name],
		[test.a.name],
	)
}

output "a" {
	value = test.a
}
output "b" {
	value = test.b
}
output "c" {
	value = test.c
}
		`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"each": cty.DynamicVal,
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("b:").
							NotNull().
							NewValue(),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c"),
						"upstream_names": cty.UnknownVal(cty.Set(cty.String)).RefineNotNull(),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a": plans.Create,
					// The other resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.b[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
					"test.c":        {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),

					// FIXME: The system is currently producing incorrect
					//   results for output values that are derived from
					//   resources that had deferred actions, because we're
					//   not quite reconstructing all of the deferral state
					//   correctly during the apply phase. The commented-out
					//   lines below show how this _ought_ to look, but
					//   we're accepting the incorrect answer for now so we
					//   can start to gather feedback on the experiment
					//   sooner, since the output value state at the interim
					//   steps isn't really that important for demonstrating
					//   the overall effect. We should fix this before
					//   stabilizing the experiment, though.

					// Currently we produce an incorrect result for output
					// value "b" because the expression evaluator doesn't
					// realize it's supposed to be treating this as deferred
					// during the apply phase, and so it incorrectly decides
					// that there are no instances due to the lack of
					// instances in the state.
					"b": cty.EmptyObjectVal,
					// We can't say anything about test.b until we know what
					// its instance keys are.
					// "b": cty.DynamicVal,

					// Currently we produce an incorrect result for output
					// value "c" because the expression evaluator doesn't
					// realize it's supposed to be treating this as deferred
					// during the apply phase, and so it incorrectly decides
					// that there is instance due to the lack of instances
					// in the state.
					"c": cty.NullVal(cty.DynamicPseudoType),
					// test.c evaluates to the placeholder value that shows
					// what we're expecting this object to look like in the
					// next round.
					// "c": cty.ObjectVal(map[string]cty.Value{
					// 	"name":           cty.StringVal("c"),
					// 	"upstream_names": cty.UnknownVal(cty.Set(cty.String)).RefineNotNull(),
					// }),
				},
			},
			{
				inputs: map[string]cty.Value{
					"each": cty.SetVal([]cty.Value{
						cty.StringVal("1"),
						cty.StringVal("2"),
					}),
				},
				wantPlanned: map[string]cty.Value{
					// test.a gets re-planned (to confirm that nothing has changed)
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
					// test.b is now planned for real, once for each instance
					"b:1": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:1"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"b:2": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:2"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					// test.c gets re-planned, so we can finalize its values
					// based on the new results from test.b.
					"c": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("c"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
							cty.StringVal("b:1"),
							cty.StringVal("b:2"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					// Since this plan is "complete", we expect to have a planned
					// action for every resource instance, although test.a is
					// no-op because nothing has changed for it since last round.
					`test.a`:      plans.NoOp,
					`test.b["1"]`: plans.Create,
					`test.b["2"]`: plans.Create,
					`test.c`:      plans.Create,
				},
				wantDeferred: make(map[string]ExpectedDeferred),
				wantApplied: map[string]cty.Value{
					// Since test.a is no-op, it isn't visited during apply. The
					// other instances should all be applied, though.
					"b:1": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:1"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.StringVal("b:1"),
					}),
					"b:2": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:2"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.StringVal("b:2"),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("c"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
							cty.StringVal("b:1"),
							cty.StringVal("b:2"),
						}),
						"output": cty.StringVal("c"),
					}),
				},
				wantOutputs: map[string]cty.Value{
					// Now everything should be fully resolved and known.
					// A is fully resolved and known.
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"1": cty.ObjectVal(map[string]cty.Value{
							"name": cty.StringVal("b:1"),
							"upstream_names": cty.SetVal([]cty.Value{
								cty.StringVal("a"),
							}),
							"output": cty.StringVal("b:1"),
						}),
						"2": cty.ObjectVal(map[string]cty.Value{
							"name": cty.StringVal("b:2"),
							"upstream_names": cty.SetVal([]cty.Value{
								cty.StringVal("a"),
							}),
							"output": cty.StringVal("b:2"),
						}),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("c"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
							cty.StringVal("b:1"),
							cty.StringVal("b:2"),
						}),
						"output": cty.StringVal("c"),
					}),
				},
				complete: true,
			},
			{
				inputs: map[string]cty.Value{
					"each": cty.SetVal([]cty.Value{
						cty.StringVal("1"),
						cty.StringVal("2"),
					}),
				},
				wantPlanned: map[string]cty.Value{
					// Everything gets re-planned to confirm that nothing has changed.
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
					"b:1": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:1"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.StringVal("b:1"),
					}),
					"b:2": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:2"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.StringVal("b:2"),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("c"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
							cty.StringVal("b:1"),
							cty.StringVal("b:2"),
						}),
						"output": cty.StringVal("c"),
					}),
				},
				wantActions: map[string]plans.Action{
					// No changes needed
					`test.a`:      plans.NoOp,
					`test.b["1"]`: plans.NoOp,
					`test.b["2"]`: plans.NoOp,
					`test.c`:      plans.NoOp,
				},
				wantDeferred: make(map[string]ExpectedDeferred),
				complete:     true,
				// We won't execute an apply step in this stage, because the
				// plan should be empty.
			},
		},
	}

	resourceCountTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "a" {
	name = "a"
}

resource "test" "b" {
	count = var.resource_count
    name = "b:${count.index}"
    upstream_names = [test.a.name]
}

resource "test" "c" {
	name = "c"
	upstream_names = setunion(
		[for v in test.b : v.name],
		[test.a.name],
	)
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.DynamicVal,
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("b:").
							NotNull().
							NewValue(),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c"),
						"upstream_names": cty.UnknownVal(cty.Set(cty.String)).RefineNotNull(),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.b[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
					"test.c":        {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
				},
				wantOutputs: make(map[string]cty.Value),
			},
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.NumberIntVal(2),
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
					"b:0": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:0"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"b:1": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b:1"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("c"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a"),
							cty.StringVal("b:0"),
							cty.StringVal("b:1"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					// Since this plan is "complete", we expect to have a planned
					// action for every resource instance, although test.a is
					// no-op because nothing has changed for it since last round.
					`test.a`:    plans.NoOp,
					`test.b[0]`: plans.Create,
					`test.b[1]`: plans.Create,
					`test.c`:    plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     true,
				// Don't run an apply for this cycle.
			},
		},
	}

	resourceInModuleForEachTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "each" {
	type = set(string)
}

module "mod" {
  source = "./mod"

  each = var.each
}

resource "test" "a" {
	name = "a"
	upstream_names = module.mod.names
}
`,
			"mod/main.tf": `
variable "each" {
	type = set(string)
}

resource "test" "names" {
	for_each = var.each
	name = "b:${each.key}"
}

output "names" {
	value = [for v in test.names : v.name]
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"each": cty.DynamicVal,
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("b:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.UnknownVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{},
				wantDeferred: map[string]ExpectedDeferred{
					"module.mod.test.names[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
					"test.a":                       {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				wantApplied: make(map[string]cty.Value),
				wantOutputs: make(map[string]cty.Value),
			},
			{
				inputs: map[string]cty.Value{
					"each": cty.SetVal([]cty.Value{
						cty.StringVal("1"),
						cty.StringVal("2"),
					}),
				},
				wantPlanned: map[string]cty.Value{
					"b:1": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b:1"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b:2": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b:2"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.SetVal([]cty.Value{cty.StringVal("b:1"), cty.StringVal("b:2")}),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"module.mod.test.names[\"1\"]": plans.Create,
					"module.mod.test.names[\"2\"]": plans.Create,
					"test.a":                       plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     true,
			},
		},
	}

	createBeforeDestroyLifecycleTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
# This resource should be replaced in the plan, with create before destroy.
resource "test" "a" {
	name = "a"

	lifecycle {
		create_before_destroy = true
	}
}

# This resource should be replaced in the plan, with destroy before create.
resource "test" "b" {
	name = "b"
}

variable "resource_count" {
	type = number
}

# These resources are "maybe-orphans", we should see a generic plan action for
# these, but nothing in the actual plan.
resource "test" "c" {
	count = var.resource_count
	name = "c:${count.index}"

	lifecycle {
		create_before_destroy = true
	}
}
`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectTainted, // force a replace in our plan
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "a",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.b"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectTainted, // force a replace in our plan
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "b",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.c[0]"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectTainted, // force a replace in our plan
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "c:0",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("c:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a": plans.CreateThenDelete,
					"test.b": plans.DeleteThenCreate,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.c[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
			},
		},
	}

	// The next test isn't testing deferred actions specifically. Instead,
	// they're just testing the "removed" block works within the alternate
	// execution path for deferred actions.

	forgetResourcesTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
# This should work as expected, with the resource being removed from state
# but not destroyed. This should work even with the unknown_instances experiment
# enabled.
removed {
	from = test.a

	lifecycle {
		destroy = false
	}
}
`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a[0]"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectTainted, // force a replace in our plan
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "a",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a[1]"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectTainted, // force a replace in our plan
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "a",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				wantPlanned: map[string]cty.Value{},
				wantActions: map[string]plans.Action{
					"test.a[0]": plans.Forget,
					"test.a[1]": plans.Forget,
				},
				wantDeferred:  map[string]ExpectedDeferred{},
				allowWarnings: true,
				complete:      true,
			},
		},
	}

	importIntoUnknownInstancesTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "a" {
	count = var.resource_count
    name  = "a"
}

import {
    id = "a"
	to = test.a[0]
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				wantPlanned: map[string]cty.Value{
					// This time round, we don't actually perform the import
					// because we don't know which instances we're importing.
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: make(map[string]plans.Action),
				wantDeferred: map[string]ExpectedDeferred{
					"test.a[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
				wantApplied: make(map[string]cty.Value),
				wantOutputs: make(map[string]cty.Value),
			},
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.NumberIntVal(1),
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a"),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a[0]": plans.NoOp, // noop not create because of the import.
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     true,
			},
		},
	}

	targetDeferredResourceTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "a" {
	count = var.resource_count
	name  = "a:${count.index}"
}

resource "test" "b" {
	name = "b"
}

resource "test" "c" {
	name = "c"
}
`,
		},
		stages: []deferredActionsTestStage{
			// In this stage, we're testing that targeting test.a[0] will still
			// prompt the plan to include the deferral of the unknown
			// test.a["*"] instances.
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustResourceInstanceAddr("test.a[0]"), mustResourceInstanceAddr("test.b")}
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("a:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
				allowWarnings: true,
			},
			// This stage is the same as above, except we're targeting the
			// non-instanced test.a. This should still make the unknown
			// test.a["*"] instances appear in the plan as deferrals.
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustResourceInstanceAddr("test.a"), mustResourceInstanceAddr("test.b")}
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("a:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
				allowWarnings: true,
			},
			// Finally, we don't target test.a at all. So we shouldn't see it
			// anywhere in planning or deferrals.
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustResourceInstanceAddr("test.b")}
				},
				wantPlanned: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred:  map[string]ExpectedDeferred{},
				allowWarnings: true,
			},
		},
	}

	targetResourceThatDependsOnDeferredResourceTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "c" {
	name = "c"
}

resource "test" "a" {
	count = var.resource_count
	name  = "a:${count.index}"
	upstream_names = [test.c.name]
}

resource "test" "b" {
	name = "b"
	upstream_names = [for v in test.a : v.name]
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustResourceInstanceAddr("test.b")}
				},
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("a:").
							NotNull().
							NewValue(),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("c"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.UnknownVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.c": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
					"test.b":        {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{
					"c": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("c"),
					}),
				},
				wantOutputs:   map[string]cty.Value{},
				allowWarnings: true,
			},
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustResourceInstanceAddr("test.b")}
				},
				inputs: map[string]cty.Value{
					"resource_count": cty.NumberIntVal(2),
				},
				wantPlanned: map[string]cty.Value{
					"a:0": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("a:0"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("c"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"a:1": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("a:1"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("c"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a:0"),
							cty.StringVal("a:1"),
						}),
						"output": cty.UnknownVal(cty.String),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("c"),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a[0]": plans.Create,
					"test.a[1]": plans.Create,
					"test.b":    plans.Create,
					"test.c":    plans.NoOp,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				wantApplied: map[string]cty.Value{
					"a:0": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("a:0"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("c"),
						}),
						"output": cty.StringVal("a:0"),
					}),
					"a:1": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("a:1"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("c"),
						}),
						"output": cty.StringVal("a:1"),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name": cty.StringVal("b"),
						"upstream_names": cty.SetVal([]cty.Value{
							cty.StringVal("a:0"),
							cty.StringVal("a:1"),
						}),
						"output": cty.StringVal("b"),
					}),
				},
				wantOutputs:   map[string]cty.Value{},
				allowWarnings: true,
			},
		},
	}

	targetDeferredResourceTriggersDependenciesTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	count = 2
	name  = "a:${count.index}"
}

resource "test" "b" {
	for_each = toset([ for v in test.a : v.output ])
	name = "b:${each.value}"
}
`,
		},
		stages: []deferredActionsTestStage{
			// The first time round, we target test.b only. Because test.b
			// depends on test.a, we should see test.a instances in the plan.
			// Then, when we apply the plan test.a should still be applied even
			// through test.b was deferred and is technically not in the plan.
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustAbsResourceAddr("test.b")}
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("b:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"a:0": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:0"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"a:1": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:1"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a[0]": plans.Create,
					"test.a[1]": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.b[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{
					"a:0": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:0"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a:0"),
					}),
					"a:1": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:1"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a:1"),
					}),
				},
				wantOutputs:   make(map[string]cty.Value),
				allowWarnings: true,
			},
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Targets = []addrs.Targetable{mustAbsResourceAddr("test.b")}
				},
				wantPlanned: map[string]cty.Value{
					"a:0": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:0"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a:0"),
					}),
					"a:1": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:1"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("a:1"),
					}),
					"b:a:0": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b:a:0"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b:a:1": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b:a:1"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a[0]":       plans.NoOp,
					"test.a[1]":       plans.NoOp,
					"test.b[\"a:0\"]": plans.Create,
					"test.b[\"a:1\"]": plans.Create,
				},
				wantDeferred:  make(map[string]ExpectedDeferred),
				allowWarnings: true,
				complete:      false, // because we still did targeting
			},
		},
	}

	replaceDeferredResourceTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "a" {
	count = var.resource_count
	name  = "a:${count.index}"
}

resource "test" "b" {
	name = "b"
}

resource "test" "c" {
	name = "c"
}
`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a[0]"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "a:0",
						"output": "a:0",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.b"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "b",
						"output": "b",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.c"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "c",
						"output": "c",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				buildOpts: func(opts *PlanOpts) {
					opts.ForceReplace = []addrs.AbsResourceInstance{mustResourceInstanceAddr("test.a[0]"), mustResourceInstanceAddr("test.b")}
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("a:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"c": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("c"),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.DeleteThenCreate,
					"test.c": plans.NoOp,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
			},
		},
	}

	customConditionsTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "a" {
	count = var.resource_count
	name  = "a:${count.index}"

	lifecycle {
		postcondition {
			condition = self.name == "a:${count.index}"
			error_message = "self.name is not a:${count.index}"
		}
	}
}

resource "test" "b" {
	name = "b"

	lifecycle {
		postcondition {
			condition = self.name == "b"
			error_message = "self.name is not b"
		}
	}
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("a:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("b"),
					}),
				},
				wantOutputs: make(map[string]cty.Value),
			},
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.NumberIntVal(1),
				},
				wantPlanned: map[string]cty.Value{
					"a:0": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a:0"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("b"),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a[0]": plans.Create,
					"test.b":    plans.NoOp,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     true,
			},
		},
	}

	customConditionsWithOrphansTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "resource_count" {
	type = number
}

resource "test" "b" {
	name = "b"

	lifecycle {
		postcondition {
			condition = self.name == "b"
			error_message = "self.name is not b"
		}
	}
}

# test.c will already be in state, so we can test the actions of orphaned
# resources with custom conditions.
resource "test" "c" {
	count = var.resource_count
	name = "c:${count.index}"

	lifecycle {
		postcondition {
			condition = self.name == "c:${count.index}"
			error_message = "self.name is not c:${count.index}"
		}
	}
}
`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.c[0]"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "c:0",
						"output": "c:0",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				},
			)
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.c[1]"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "c:1",
						"output": "c:1",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				},
			)
		}),
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.UnknownVal(cty.Number),
				},
				wantPlanned: map[string]cty.Value{
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name": cty.UnknownVal(cty.String).Refine().
							StringPrefixFull("c:").
							NotNull().
							NewValue(),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.b": plans.Create,
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.c[\"*\"]": {Reason: providers.DeferredReasonInstanceCountUnknown, Action: plans.Create},
				},
				wantApplied: map[string]cty.Value{
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("b"),
					}),
				},
				wantOutputs: make(map[string]cty.Value),
			},
			{
				inputs: map[string]cty.Value{
					"resource_count": cty.NumberIntVal(1),
				},
				wantPlanned: map[string]cty.Value{
					"c:0": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("c:0"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("c:0"),
					}),
					"b": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("b"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("b"),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.c[0]": plans.NoOp,
					"test.c[1]": plans.Delete,
					"test.b":    plans.NoOp,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     true,
			},
		},
	}

	// resourceReadTest is a test that covers the behavior of reading resources
	// in a refresh when the refresh is responding with a deferral.
	resourceReadTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "a"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "deferred_read", // this signals the mock provider to defer the read
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Mode = plans.RefreshOnlyMode
				},
				inputs:      map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Read},
				},
				complete: false,
			},

			{
				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					// The read is deferred but the plan is not so we can still
					// plan the resource.
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_read"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.NullVal(cty.String),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Update},
				},
				complete: false,
			},
		},
	}

	resourceReadButForbiddenTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "a"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "deferred_read", // this signals the mock provider to defer the read
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Mode = plans.RefreshOnlyMode
					opts.DeferralAllowed = false
				},
				inputs:      map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{},

				wantActions: map[string]plans.Action{},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     false,

				wantDiagnostic: func(diags tfdiags.Diagnostics) bool {
					for _, diag := range diags {
						if diag.Description().Summary == "Provider deferred changes when Terraform did not allow deferrals" {
							return true
						}
					}
					return false
				},
			},
		},
	}

	readDataSourceTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
data "test" "a" {
	name       = "deferred_read"
}

resource "test" "b" {
	name = data.test.a.name
}

output "a" {
	value = data.test.a
}

output "b" {
	value = test.b
}
	`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					// test.b is deferred but still being planned. It being listed
					// here does not mean it's in the plan.
					"<unknown>": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.UnknownVal(cty.String),
						"output":         cty.UnknownVal(cty.String),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
					}),
				},
				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.NullVal(cty.Object(map[string]cty.Type{
						"name":   cty.String,
						"output": cty.String,
					})),
					"b": cty.NullVal(cty.DynamicPseudoType),
				},
				wantDeferred: map[string]ExpectedDeferred{
					// data.test.a is not part of the plan so we can only
					// observe the indirect consequence on the resource.
					"test.b": {Reason: providers.DeferredReasonDeferredPrereq, Action: plans.Create},
				},
				complete: false,
			},
		},
	}

	readDataSourceButForbiddenTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
data "test" "a" {
	name       = "deferred_read"
}

resource "test" "b" {
	name = data.test.a.name
}

output "a" {
	value = data.test.a
}

output "b" {
	value = test.b
}
	`,
		},
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.DeferralAllowed = false
				},
				inputs:      map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{},
				wantActions: map[string]plans.Action{},

				wantOutputs: map[string]cty.Value{
					"a": cty.NullVal(cty.Object(map[string]cty.Type{
						"name": cty.String,
					})),
					"b": cty.NullVal(cty.DynamicPseudoType),
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     false,

				wantDiagnostic: func(diags tfdiags.Diagnostics) bool {
					for _, diag := range diags {
						if diag.Description().Summary == "Provider deferred changes when Terraform did not allow deferrals" {
							return true
						}
					}
					return false
				},
			},
		},
	}

	// planCreateResourceChange is a test that covers the behavior of planning a resource that is being created.
	planCreateResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					"deferred_resource_change": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.NullVal(cty.DynamicPseudoType),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Create},
				},
				complete: false,
			},
		},
	}

	// planUpdateResourceChange is a test that covers the behavior of planning a resource that is being updated
	planUpdateResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "old_value",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{

				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					"deferred_resource_change": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("old_value"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.NullVal(cty.String),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Update},
				},
				complete: false,
			},
		},
	}

	// planNoOpResourceChange is a test that covers the behavior of planning a resource that is the same as the current state.
	planNoOpResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "deferred_resource_change",
						"output": "computed_output",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{

				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					"deferred_resource_change": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("computed_output"),
					}),
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("computed_output"),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.NoOp},
				},
				complete: false,
			},
		},
	}

	// planReplaceResourceChange is a test that covers the behavior of planning a resource that the provider
	// marks as needing replacement.
	planReplaceResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "old_value",
						"output": "mark_for_replacement", // tells the mock provider to replace the resource
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					"deferred_resource_change": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("old_value"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("mark_for_replacement"),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.DeleteThenCreate},
				},
				complete: false,
			},
		},
	}

	// planForceReplaceResourceChange is a test that covers the behavior of planning a resource that is marked for replacement
	planForceReplaceResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "old_value",
						"output": "computed_output",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.ForceReplace = []addrs.AbsResourceInstance{
						{
							Module: addrs.RootModuleInstance,
							Resource: addrs.ResourceInstance{
								Resource: addrs.Resource{
									Mode: addrs.ManagedResourceMode,
									Type: "test",
									Name: "a",
								},
								Key: addrs.NoKey,
							},
						},
					}
				},
				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					"deferred_resource_change": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("old_value"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("computed_output"),
					}),
				},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.DeleteThenCreate},
				},
				complete: false,
			},
		},
	}

	// planDeleteResourceChange is a test that covers the behavior of planning a resource that is removed from the config.
	planDeleteResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
// Empty config, expect to delete everything
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name":   "deferred_resource_change",
						"output": "computed_output",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{

				inputs:      map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Delete},
				},
				complete: false,
			},
		},
	}

	// planDestroyResourceChange is a test that covers the behavior of planning a resource
	planDestroyResourceChange = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "deferred_resource_change",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Mode = plans.DestroyMode
				},
				inputs: map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{
					// This is here because of the additional full plan run if
					// the previous state is not empty (and refresh is not skipped).
					"deferred_resource_change": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("deferred_resource_change"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},

				wantActions: map[string]plans.Action{},
				wantApplied: map[string]cty.Value{
					// The all resources will be deferred, so shouldn't
					// have any action at this stage.
				},
				wantOutputs: map[string]cty.Value{},
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Delete},
				},
				complete: false,
			},
		},
	}

	planDestroyResourceChangeButForbidden = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
resource "test" "a" {
	name = "deferred_resource_change"
}
output "a" {
	value = test.a
}
		`,
		},
		state: states.BuildState(func(state *states.SyncState) {
			state.SetResourceInstanceCurrent(
				mustResourceInstanceAddr("test.a"),
				&states.ResourceInstanceObjectSrc{
					Status: states.ObjectReady,
					AttrsJSON: mustParseJson(map[string]interface{}{
						"name": "deferred_resource_change",
					}),
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("test"),
					Module:   addrs.RootModule,
				})
		}),
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					opts.Mode = plans.DestroyMode
					opts.DeferralAllowed = false
				},
				inputs:      map[string]cty.Value{},
				wantPlanned: map[string]cty.Value{},

				wantActions: map[string]plans.Action{},

				wantOutputs:  map[string]cty.Value{},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     false,
				wantDiagnostic: func(diags tfdiags.Diagnostics) bool {
					for _, diag := range diags {
						if diag.Description().Summary == "Provider deferred changes when Terraform did not allow deferrals" {
							return true
						}
					}
					return false
				},
			},
		},
	}

	importDeferredTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "import_id" {
    type = string
}

resource "test" "a" {
    name = "a"
}

import {
    id = var.import_id
    to = test.a
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				inputs: map[string]cty.Value{
					"import_id": cty.StringVal("deferred"), // Telling the test case to defer the import
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.UnknownVal(cty.String),
					}),
				},
				wantActions: make(map[string]plans.Action),
				wantDeferred: map[string]ExpectedDeferred{
					"test.a": {Reason: providers.DeferredReasonProviderConfigUnknown, Action: plans.Create},
				},
				wantApplied: make(map[string]cty.Value),
				wantOutputs: make(map[string]cty.Value),
				complete:    false,
			},
			{
				inputs: map[string]cty.Value{
					"import_id": cty.StringVal("can_be_imported"),
				},
				wantPlanned: map[string]cty.Value{
					"a": cty.ObjectVal(map[string]cty.Value{
						"name":           cty.StringVal("a"),
						"upstream_names": cty.NullVal(cty.Set(cty.String)),
						"output":         cty.StringVal("can_be_imported"),
					}),
				},
				wantActions: map[string]plans.Action{
					"test.a": plans.Update,
				},
				wantDeferred: map[string]ExpectedDeferred{},
				complete:     true,
			},
		},
	}

	importDeferredButForbiddenTest = deferredActionsTest{
		configs: map[string]string{
			"main.tf": `
variable "import_id" {
    type = string
}

resource "test" "a" {
    name = "a"
}

import {
    id = var.import_id
    to = test.a
}
`,
		},
		stages: []deferredActionsTestStage{
			{
				buildOpts: func(opts *PlanOpts) {
					// We want to test if the user gets presented with a diagnostic in case no deferrals are allowed
					opts.DeferralAllowed = false
				},
				inputs: map[string]cty.Value{
					"import_id": cty.StringVal("deferred"), // Telling the test case to defer the import
				},
				wantPlanned:  map[string]cty.Value{},
				wantActions:  make(map[string]plans.Action),
				wantDeferred: map[string]ExpectedDeferred{},
				wantOutputs:  make(map[string]cty.Value),
				complete:     false,

				wantDiagnostic: func(diags tfdiags.Diagnostics) bool {
					for _, diag := range diags {
						if diag.Description().Summary == "Provider deferred changes when Terraform did not allow deferrals" {
							return true
						}
					}
					return false
				},
			},
		},
	}
)

func TestContextApply_deferredActions(t *testing.T) {
	tests := map[string]deferredActionsTest{
		"resource_for_each":                                 resourceForEachTest,
		"resource_in_module_for_each":                       resourceInModuleForEachTest,
		"resource_count":                                    resourceCountTest,
		"create_before_destroy":                             createBeforeDestroyLifecycleTest,
		"forget_resources":                                  forgetResourcesTest,
		"import_into_unknown":                               importIntoUnknownInstancesTest,
		"target_deferred_resource":                          targetDeferredResourceTest,
		"target_resource_that_depends_on_deferred_resource": targetResourceThatDependsOnDeferredResourceTest,
		"target_deferred_resource_triggers_dependencies":    targetDeferredResourceTriggersDependenciesTest,
		"replace_deferred_resource":                         replaceDeferredResourceTest,
		"custom_conditions":                                 customConditionsTest,
		"custom_conditions_with_orphans":                    customConditionsWithOrphansTest,
		"resource_read":                                     resourceReadTest,
		"data_read":                                         readDataSourceTest,
		"data_for_each":                                     dataForEachTest,
		"data_count":                                        dataCountTest,
		"plan_create_resource_change":                       planCreateResourceChange,
		"plan_update_resource_change":                       planUpdateResourceChange,
		"plan_noop_resource_change":                         planNoOpResourceChange,
		"plan_replace_resource_change":                      planReplaceResourceChange,
		"plan_force_replace_resource_change":                planForceReplaceResourceChange,
		"plan_delete_resource_change":                       planDeleteResourceChange,
		"plan_destroy_resource_change":                      planDestroyResourceChange,
		"import_deferred":                                   importDeferredTest,
		"import_deferred_but_forbidden":                     importDeferredButForbiddenTest,
		"resource_read_but_forbidden":                       resourceReadButForbiddenTest,
		"data_read_but_forbidden":                           readDataSourceButForbiddenTest,
		"plan_destroy_resource_change_but_forbidden":        planDestroyResourceChangeButForbidden,
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.skip {
				t.SkipNow()
			}

			// Initialise the config.
			cfg := testModuleInline(t, test.configs)

			// Initialise the state.
			state := test.state
			if state == nil {
				state = states.NewState()
			}

			// Run through our cycle of planning and applying changes, checking
			// the results at each step.
			for ix, stage := range test.stages {
				t.Run(fmt.Sprintf("round-%d", ix), func(t *testing.T) {

					provider := &deferredActionsProvider{
						plannedChanges: &deferredActionsChanges{
							changes: make(map[string]cty.Value),
						},
						appliedChanges: &deferredActionsChanges{
							changes: make(map[string]cty.Value),
						},
					}

					ctx := testContext2(t, &ContextOpts{
						Providers: map[addrs.Provider]providers.Factory{
							addrs.NewDefaultProvider("test"): testProviderFuncFixed(provider.Provider()),
						},
					})

					opts := &PlanOpts{
						Mode:            plans.NormalMode,
						DeferralAllowed: true,
						SetVariables: func() InputValues {
							values := InputValues{}
							for name, value := range stage.inputs {
								values[name] = &InputValue{
									Value:      value,
									SourceType: ValueFromCaller,
								}
							}
							return values
						}(),
					}

					if stage.buildOpts != nil {
						stage.buildOpts(opts)
					}

					var plan *plans.Plan
					t.Run("plan", func(t *testing.T) {

						var diags tfdiags.Diagnostics
						plan, diags = ctx.Plan(cfg, state, opts)

						if stage.wantDiagnostic == nil {
							// We expect the correct planned changes and no diagnostics.
							if stage.allowWarnings {
								assertNoErrors(t, diags)
							} else {
								assertNoDiagnostics(t, diags)
							}
						} else {
							if !stage.wantDiagnostic(diags) {
								t.Fatalf("missing expected diagnostics: %s", diags)
							} else {
								// We don't want to make any further assertions in this case.
								// If diagnostics are expected it's valid that no plan may be returned.
								return
							}
						}

						if plan == nil {
							t.Fatalf("plan is nil")
						}

						if plan.Complete != stage.complete {
							t.Errorf("wrong completion status in plan: got %v, want %v", plan.Complete, stage.complete)
						}

						provider.plannedChanges.Test(t, stage.wantPlanned)

						// We expect the correct actions.
						gotActions := make(map[string]plans.Action)
						for _, cs := range plan.Changes.Resources {
							gotActions[cs.Addr.String()] = cs.Action
						}
						if diff := cmp.Diff(stage.wantActions, gotActions); diff != "" {
							t.Errorf("wrong actions in plan\n%s", diff)
						}

						gotDeferred := make(map[string]ExpectedDeferred)
						for _, dc := range plan.DeferredResources {
							gotDeferred[dc.ChangeSrc.Addr.String()] = ExpectedDeferred{Reason: dc.DeferredReason, Action: dc.ChangeSrc.Action}
						}
						if diff := cmp.Diff(stage.wantDeferred, gotDeferred); diff != "" {
							t.Errorf("wrong deferred reasons or actions in plan\n%s", diff)
						}

					})

					if stage.wantApplied == nil {
						// Don't execute the apply stage if wantApplied is nil.
						return
					}

					if opts.Mode == plans.RefreshOnlyMode {
						// Don't execute the apply stage if mode is refresh-only.
						return
					}

					t.Run("apply", func(t *testing.T) {

						updatedState, diags := ctx.Apply(plan, cfg, nil)

						// We expect the correct applied changes and no diagnostics.
						if stage.allowWarnings {
							assertNoErrors(t, diags)
						} else {
							assertNoDiagnostics(t, diags)
						}
						provider.appliedChanges.Test(t, stage.wantApplied)

						// We also want the correct output values.
						gotOutputs := make(map[string]cty.Value)
						for name, output := range updatedState.RootOutputValues {
							gotOutputs[name] = output.Value
						}
						if diff := cmp.Diff(stage.wantOutputs, gotOutputs, ctydebug.CmpOptions); diff != "" {
							t.Errorf("wrong output values\n%s", diff)
						}

						// Update the state for the next stage.
						state = updatedState
					})
				})
			}
		})
	}
}

// deferredActionsChanges is a concurrent-safe map of changes from a
// deferredActionsProvider.
type deferredActionsChanges struct {
	sync.RWMutex
	changes map[string]cty.Value
}

func (d *deferredActionsChanges) Set(key string, value cty.Value) {
	d.Lock()
	defer d.Unlock()
	if d.changes == nil {
		d.changes = make(map[string]cty.Value)
	}
	d.changes[key] = value
}

func (d *deferredActionsChanges) Get(key string) cty.Value {
	d.RLock()
	defer d.RUnlock()
	return d.changes[key]
}

func (d *deferredActionsChanges) Test(t *testing.T, expected map[string]cty.Value) {
	t.Helper()
	d.RLock()
	defer d.RUnlock()
	if diff := cmp.Diff(expected, d.changes, ctydebug.CmpOptions); diff != "" {
		t.Errorf("wrong changes\n%s", diff)
	}
}

// deferredActionsProvider is a wrapper around the mock provider that keeps
// track of its own planned changes.
type deferredActionsProvider struct {
	plannedChanges *deferredActionsChanges
	appliedChanges *deferredActionsChanges
}

func (provider *deferredActionsProvider) Provider() providers.Interface {
	return &testing_provider.MockProvider{
		GetProviderSchemaResponse: &providers.GetProviderSchemaResponse{
			ResourceTypes: map[string]providers.Schema{
				"test": {
					Block: &configschema.Block{
						Attributes: map[string]*configschema.Attribute{
							"name": {
								Type:     cty.String,
								Required: true,
							},
							"upstream_names": {
								Type:     cty.Set(cty.String),
								Optional: true,
							},
							"output": {
								Type:     cty.String,
								Computed: true,
							},
						},
					},
				},
			},
			DataSources: map[string]providers.Schema{
				"test": {
					Block: &configschema.Block{
						Attributes: map[string]*configschema.Attribute{
							"name": {
								Type:     cty.String,
								Required: true,
							},
							"output": {
								Computed: true,
								Type:     cty.String,
							},
						},
					},
				},
			},
		},
		ReadResourceFn: func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
			if key := req.PriorState.GetAttr("name"); key.IsKnown() && key.AsString() == "deferred_read" {
				return providers.ReadResourceResponse{
					NewState: req.PriorState,
					Deferred: &providers.Deferred{
						Reason: providers.DeferredReasonProviderConfigUnknown,
					},
				}
			}

			return providers.ReadResourceResponse{
				NewState: req.PriorState,
			}
		},
		ReadDataSourceFn: func(req providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
			if key := req.Config.GetAttr("name"); key.IsKnown() && key.AsString() == "deferred_read" {
				return providers.ReadDataSourceResponse{
					State: req.Config,
					Deferred: &providers.Deferred{
						Reason: providers.DeferredReasonProviderConfigUnknown,
					},
				}
			}
			return providers.ReadDataSourceResponse{
				State: cty.ObjectVal(map[string]cty.Value{
					"name":   req.Config.GetAttr("name"),
					"output": req.Config.GetAttr("name"),
				}),
			}
		},
		PlanResourceChangeFn: func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
			var deferred *providers.Deferred
			var requiresReplace []cty.Path
			if req.ProposedNewState.IsNull() {
				// Then we're deleting a concrete instance.
				if key := req.PriorState.GetAttr("name"); key.IsKnown() && key.AsString() == "deferred_resource_change" {
					deferred = &providers.Deferred{
						Reason: providers.DeferredReasonProviderConfigUnknown,
					}
				}

				return providers.PlanResourceChangeResponse{
					PlannedState: req.ProposedNewState,
					Deferred:     deferred,
				}
			}

			key := "<unknown>"
			if v := req.Config.GetAttr("name"); v.IsKnown() {
				key = v.AsString()
			}

			plannedState := req.ProposedNewState
			if key == "deferred_resource_change" {
				deferred = &providers.Deferred{
					Reason: providers.DeferredReasonProviderConfigUnknown,
				}
			}

			if plannedState.GetAttr("output").IsNull() {
				plannedStateValues := req.ProposedNewState.AsValueMap()
				plannedStateValues["output"] = cty.UnknownVal(cty.String)
				plannedState = cty.ObjectVal(plannedStateValues)
			} else if plannedState.GetAttr("output").AsString() == "mark_for_replacement" {
				requiresReplace = append(requiresReplace, cty.GetAttrPath("name"), cty.GetAttrPath("output"))
			}

			provider.plannedChanges.Set(key, plannedState)
			return providers.PlanResourceChangeResponse{
				PlannedState:    plannedState,
				Deferred:        deferred,
				RequiresReplace: requiresReplace,
			}
		},
		ApplyResourceChangeFn: func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
			key := req.Config.GetAttr("name").AsString()
			newState := req.PlannedState

			if !newState.GetAttr("output").IsKnown() {
				newStateValues := req.PlannedState.AsValueMap()
				newStateValues["output"] = cty.StringVal(key)
				newState = cty.ObjectVal(newStateValues)
			}

			provider.appliedChanges.Set(key, newState)
			return providers.ApplyResourceChangeResponse{
				NewState: newState,
			}
		},
		ImportResourceStateFn: func(request providers.ImportResourceStateRequest) providers.ImportResourceStateResponse {
			if request.ID == "deferred" {
				return providers.ImportResourceStateResponse{
					ImportedResources: []providers.ImportedResource{},
					Deferred: &providers.Deferred{
						Reason: providers.DeferredReasonProviderConfigUnknown,
					},
				}
			}

			return providers.ImportResourceStateResponse{
				ImportedResources: []providers.ImportedResource{
					{
						TypeName: request.TypeName,
						State: cty.ObjectVal(map[string]cty.Value{
							"name":           cty.StringVal(request.ID),
							"upstream_names": cty.NullVal(cty.Set(cty.String)),
							"output":         cty.StringVal(request.ID),
						}),
					},
				},
			}
		},
	}
}

func mustParseJson(values map[string]interface{}) []byte {
	data, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return data
}
