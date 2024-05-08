// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/instances"
	"github.com/hashicorp/terraform/internal/lang"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/namedvals"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

func TestEvaluatorGetTerraformAttr(t *testing.T) {
	evaluator := &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		NamedValues: namedvals.NewState(),
	}
	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})

	t.Run("workspace", func(t *testing.T) {
		want := cty.StringVal("foo")
		got, diags := scope.Data.GetTerraformAttr(addrs.TerraformAttr{
			Name: "workspace",
		}, tfdiags.SourceRange{})
		if len(diags) != 0 {
			t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
		}
		if !got.RawEquals(want) {
			t.Errorf("wrong result %q; want %q", got, want)
		}
	})
}

func TestEvaluatorGetPathAttr(t *testing.T) {
	evaluator := &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		Config: &configs.Config{
			Module: &configs.Module{
				SourceDir: "bar/baz",
			},
		},
		NamedValues: namedvals.NewState(),
	}
	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})

	t.Run("module", func(t *testing.T) {
		want := cty.StringVal("bar/baz")
		got, diags := scope.Data.GetPathAttr(addrs.PathAttr{
			Name: "module",
		}, tfdiags.SourceRange{})
		if len(diags) != 0 {
			t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
		}
		if !got.RawEquals(want) {
			t.Errorf("wrong result %#v; want %#v", got, want)
		}
	})

	t.Run("root", func(t *testing.T) {
		want := cty.StringVal("bar/baz")
		got, diags := scope.Data.GetPathAttr(addrs.PathAttr{
			Name: "root",
		}, tfdiags.SourceRange{})
		if len(diags) != 0 {
			t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
		}
		if !got.RawEquals(want) {
			t.Errorf("wrong result %#v; want %#v", got, want)
		}
	})
}

func TestEvaluatorGetOutputValue(t *testing.T) {
	evaluator := &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		Config: &configs.Config{
			Module: &configs.Module{
				Outputs: map[string]*configs.Output{
					"some_output": {
						Name:      "some_output",
						Sensitive: true,
					},
					"some_other_output": {
						Name: "some_other_output",
					},
				},
			},
		},
		State: states.BuildState(func(state *states.SyncState) {
			state.SetOutputValue(addrs.AbsOutputValue{
				Module: addrs.RootModuleInstance,
				OutputValue: addrs.OutputValue{
					Name: "some_output",
				},
			}, cty.StringVal("first"), true)
			state.SetOutputValue(addrs.AbsOutputValue{
				Module: addrs.RootModuleInstance,
				OutputValue: addrs.OutputValue{
					Name: "some_other_output",
				},
			}, cty.StringVal("second"), false)
		}).SyncWrapper(),
	}

	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})

	want := cty.StringVal("first").Mark(marks.Sensitive)
	got, diags := scope.Data.GetOutput(addrs.OutputValue{
		Name: "some_output",
	}, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}
	if !got.RawEquals(want) {
		t.Errorf("wrong result %#v; want %#v", got, want)
	}

	want = cty.StringVal("second")
	got, diags = scope.Data.GetOutput(addrs.OutputValue{
		Name: "some_other_output",
	}, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}
	if !got.RawEquals(want) {
		t.Errorf("wrong result %#v; want %#v", got, want)
	}
}

// This particularly tests that a sensitive attribute in config
// results in a value that has a "sensitive" cty Mark
func TestEvaluatorGetInputVariable(t *testing.T) {
	namedValues := namedvals.NewState()
	namedValues.SetInputVariableValue(
		addrs.RootModuleInstance.InputVariable("some_var"), cty.StringVal("bar"),
	)
	namedValues.SetInputVariableValue(
		addrs.RootModuleInstance.InputVariable("some_other_var"), cty.StringVal("boop").Mark(marks.Sensitive),
	)

	evaluator := &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		Config: &configs.Config{
			Module: &configs.Module{
				Variables: map[string]*configs.Variable{
					"some_var": {
						Name:           "some_var",
						Sensitive:      true,
						Default:        cty.StringVal("foo"),
						Type:           cty.String,
						ConstraintType: cty.String,
					},
					// Avoid double marking a value
					"some_other_var": {
						Name:           "some_other_var",
						Sensitive:      true,
						Default:        cty.StringVal("bar"),
						Type:           cty.String,
						ConstraintType: cty.String,
					},
				},
			},
		},
		NamedValues: namedValues,
	}

	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})

	want := cty.StringVal("bar").Mark(marks.Sensitive)
	got, diags := scope.Data.GetInputVariable(addrs.InputVariable{
		Name: "some_var",
	}, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}
	if !got.RawEquals(want) {
		t.Errorf("wrong result %#v; want %#v", got, want)
	}

	want = cty.StringVal("boop").Mark(marks.Sensitive)
	got, diags = scope.Data.GetInputVariable(addrs.InputVariable{
		Name: "some_other_var",
	}, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}
	if !got.RawEquals(want) {
		t.Errorf("wrong result %#v; want %#v", got, want)
	}
}

func TestEvaluatorGetResource(t *testing.T) {
	stateSync := states.BuildState(func(ss *states.SyncState) {
		ss.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "test_resource",
				Name: "foo",
			}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectReady,
				AttrsJSON: []byte(`{"id":"foo", "nesting_list": [{"sensitive_value":"abc"}], "nesting_map": {"foo":{"foo":"x"}}, "nesting_set": [{"baz":"abc"}], "nesting_single": {"boop":"abc"}, "nesting_nesting": {"nesting_list":[{"sensitive_value":"abc"}]}, "value":"hello"}`),
				AttrSensitivePaths: []cty.PathValueMarks{
					{
						Path:  cty.GetAttrPath("nesting_list").IndexInt(0).GetAttr("sensitive_value"),
						Marks: cty.NewValueMarks(marks.Sensitive),
					},
					{
						Path:  cty.GetAttrPath("nesting_map").IndexString("foo").GetAttr("foo"),
						Marks: cty.NewValueMarks(marks.Sensitive),
					},
					{
						Path:  cty.GetAttrPath("nesting_nesting").GetAttr("nesting_list").IndexInt(0).GetAttr("sensitive_value"),
						Marks: cty.NewValueMarks(marks.Sensitive),
					},
					{
						Path:  cty.GetAttrPath("nesting_set"),
						Marks: cty.NewValueMarks(marks.Sensitive),
					},
					{
						Path:  cty.GetAttrPath("nesting_single").GetAttr("boop"),
						Marks: cty.NewValueMarks(marks.Sensitive),
					},
					{
						Path:  cty.GetAttrPath("value"),
						Marks: cty.NewValueMarks(marks.Sensitive),
					},
				},
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("test"),
				Module:   addrs.RootModule,
			},
		)
	}).SyncWrapper()

	rc := &configs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_resource",
		Name: "foo",
		Config: configs.SynthBody("", map[string]cty.Value{
			"id": cty.StringVal("foo"),
		}),
		Provider: addrs.Provider{
			Hostname:  addrs.DefaultProviderRegistryHost,
			Namespace: "hashicorp",
			Type:      "test",
		},
	}

	evaluator := &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		Changes: plans.NewChanges().SyncWrapper(),
		Config: &configs.Config{
			Module: &configs.Module{
				ManagedResources: map[string]*configs.Resource{
					"test_resource.foo": rc,
				},
			},
		},
		State:       stateSync,
		NamedValues: namedvals.NewState(),
		Plugins: schemaOnlyProvidersForTesting(map[addrs.Provider]providers.ProviderSchema{
			addrs.NewDefaultProvider("test"): {
				ResourceTypes: map[string]providers.Schema{
					"test_resource": {
						Block: &configschema.Block{
							Attributes: map[string]*configschema.Attribute{
								"id": {
									Type:     cty.String,
									Computed: true,
								},
								"value": {
									Type:      cty.String,
									Computed:  true,
									Sensitive: true,
								},
							},
							BlockTypes: map[string]*configschema.NestedBlock{
								"nesting_list": {
									Block: configschema.Block{
										Attributes: map[string]*configschema.Attribute{
											"value":           {Type: cty.String, Optional: true},
											"sensitive_value": {Type: cty.String, Optional: true, Sensitive: true},
										},
									},
									Nesting: configschema.NestingList,
								},
								"nesting_map": {
									Block: configschema.Block{
										Attributes: map[string]*configschema.Attribute{
											"foo": {Type: cty.String, Optional: true, Sensitive: true},
										},
									},
									Nesting: configschema.NestingMap,
								},
								"nesting_set": {
									Block: configschema.Block{
										Attributes: map[string]*configschema.Attribute{
											"baz": {Type: cty.String, Optional: true, Sensitive: true},
										},
									},
									Nesting: configschema.NestingSet,
								},
								"nesting_single": {
									Block: configschema.Block{
										Attributes: map[string]*configschema.Attribute{
											"boop": {Type: cty.String, Optional: true, Sensitive: true},
										},
									},
									Nesting: configschema.NestingSingle,
								},
								"nesting_nesting": {
									Block: configschema.Block{
										BlockTypes: map[string]*configschema.NestedBlock{
											"nesting_list": {
												Block: configschema.Block{
													Attributes: map[string]*configschema.Attribute{
														"value":           {Type: cty.String, Optional: true},
														"sensitive_value": {Type: cty.String, Optional: true, Sensitive: true},
													},
												},
												Nesting: configschema.NestingList,
											},
										},
									},
									Nesting: configschema.NestingSingle,
								},
							},
						},
					},
				},
			},
		}),
	}

	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})

	want := cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("foo"),
		"nesting_list": cty.ListVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"sensitive_value": cty.StringVal("abc").Mark(marks.Sensitive),
				"value":           cty.NullVal(cty.String),
			}),
		}),
		"nesting_map": cty.MapVal(map[string]cty.Value{
			"foo": cty.ObjectVal(map[string]cty.Value{"foo": cty.StringVal("x").Mark(marks.Sensitive)}),
		}),
		"nesting_nesting": cty.ObjectVal(map[string]cty.Value{
			"nesting_list": cty.ListVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{
					"sensitive_value": cty.StringVal("abc").Mark(marks.Sensitive),
					"value":           cty.NullVal(cty.String),
				}),
			}),
		}),
		"nesting_set": cty.SetVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"baz": cty.StringVal("abc").Mark(marks.Sensitive),
			}),
		}),
		"nesting_single": cty.ObjectVal(map[string]cty.Value{
			"boop": cty.StringVal("abc").Mark(marks.Sensitive),
		}),
		"value": cty.StringVal("hello").Mark(marks.Sensitive),
	})

	addr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_resource",
		Name: "foo",
	}
	got, diags := scope.Data.GetResource(addr, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}

	if !got.RawEquals(want) {
		t.Errorf("wrong result:\ngot: %#v\nwant: %#v", got, want)
	}
}

// GetResource will return a planned object's After value
// if there is a change for that resource instance.
func TestEvaluatorGetResource_changes(t *testing.T) {
	// Set up existing state
	stateSync := states.BuildState(func(ss *states.SyncState) {
		ss.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "test_resource",
				Name: "foo",
			}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
			&states.ResourceInstanceObjectSrc{
				Status:    states.ObjectPlanned,
				AttrsJSON: []byte(`{"id":"foo", "to_mark_val":"tacos", "sensitive_value":"abc"}`),
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("test"),
				Module:   addrs.RootModule,
			},
		)
	}).SyncWrapper()

	// Create a change for the existing state resource,
	// to exercise retrieving the After value of the change
	changesSync := plans.NewChanges().SyncWrapper()
	change := &plans.ResourceInstanceChange{
		Addr: mustResourceInstanceAddr("test_resource.foo"),
		ProviderAddr: addrs.AbsProviderConfig{
			Module:   addrs.RootModule,
			Provider: addrs.NewDefaultProvider("test"),
		},
		Change: plans.Change{
			Action: plans.Update,
			// Provide an After value that contains a marked value
			After: cty.ObjectVal(map[string]cty.Value{
				"id":              cty.StringVal("foo"),
				"to_mark_val":     cty.StringVal("pizza").Mark(marks.Sensitive),
				"sensitive_value": cty.StringVal("abc").Mark(marks.Sensitive),
				"sensitive_collection": cty.MapVal(map[string]cty.Value{
					"boop": cty.StringVal("beep"),
				}).Mark(marks.Sensitive),
			}),
		},
	}

	// Set up our schemas
	schemas := &Schemas{
		Providers: map[addrs.Provider]providers.ProviderSchema{
			addrs.NewDefaultProvider("test"): {
				ResourceTypes: map[string]providers.Schema{
					"test_resource": {
						Block: &configschema.Block{
							Attributes: map[string]*configschema.Attribute{
								"id": {
									Type:     cty.String,
									Computed: true,
								},
								"to_mark_val": {
									Type:     cty.String,
									Computed: true,
								},
								"sensitive_value": {
									Type:      cty.String,
									Computed:  true,
									Sensitive: true,
								},
								"sensitive_collection": {
									Type:      cty.Map(cty.String),
									Computed:  true,
									Sensitive: true,
								},
							},
						},
					},
				},
			},
		},
	}

	// The resource we'll inspect
	addr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "test_resource",
		Name: "foo",
	}
	schema, _ := schemas.ResourceTypeConfig(addrs.NewDefaultProvider("test"), addr.Mode, addr.Type)
	// This encoding separates out the After's marks into its AfterValMarks
	csrc, _ := change.Encode(schema.ImpliedType())
	changesSync.AppendResourceInstanceChange(csrc)

	evaluator := &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		Changes: changesSync,
		Config: &configs.Config{
			Module: &configs.Module{
				ManagedResources: map[string]*configs.Resource{
					"test_resource.foo": {
						Mode: addrs.ManagedResourceMode,
						Type: "test_resource",
						Name: "foo",
						Provider: addrs.Provider{
							Hostname:  addrs.DefaultProviderRegistryHost,
							Namespace: "hashicorp",
							Type:      "test",
						},
					},
				},
			},
		},
		State:       stateSync,
		NamedValues: namedvals.NewState(),
		Plugins:     schemaOnlyProvidersForTesting(schemas.Providers),
	}

	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})

	want := cty.ObjectVal(map[string]cty.Value{
		"id":              cty.StringVal("foo"),
		"to_mark_val":     cty.StringVal("pizza").Mark(marks.Sensitive),
		"sensitive_value": cty.StringVal("abc").Mark(marks.Sensitive),
		"sensitive_collection": cty.MapVal(map[string]cty.Value{
			"boop": cty.StringVal("beep"),
		}).Mark(marks.Sensitive),
	})

	got, diags := scope.Data.GetResource(addr, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}

	if !got.RawEquals(want) {
		t.Errorf("wrong result:\ngot: %#v\nwant: %#v", got, want)
	}
}

func TestEvaluatorGetModule(t *testing.T) {
	evaluator := evaluatorForModule(states.NewState().SyncWrapper(), plans.NewChanges().SyncWrapper())
	evaluator.Instances.SetModuleSingle(addrs.RootModuleInstance, addrs.ModuleCall{Name: "mod"})
	evaluator.NamedValues.SetOutputValue(
		addrs.OutputValue{Name: "out"}.Absolute(addrs.ModuleInstance{addrs.ModuleInstanceStep{Name: "mod"}}),
		cty.StringVal("bar").Mark(marks.Sensitive),
	)
	data := &evaluationStateData{
		Evaluator: evaluator,
	}
	scope := evaluator.Scope(data, nil, nil, lang.ExternalFuncs{})
	want := cty.ObjectVal(map[string]cty.Value{"out": cty.StringVal("bar").Mark(marks.Sensitive)})
	got, diags := scope.Data.GetModule(addrs.ModuleCall{
		Name: "mod",
	}, tfdiags.SourceRange{})

	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics %s", spew.Sdump(diags))
	}
	if !got.RawEquals(want) {
		t.Errorf("wrong result\ngot:  %#v\nwant: %#v", got, want)
	}
}

func evaluatorForModule(stateSync *states.SyncState, changesSync *plans.ChangesSync) *Evaluator {
	return &Evaluator{
		Meta: &ContextMeta{
			Env: "foo",
		},
		Config: &configs.Config{
			Module: &configs.Module{
				ModuleCalls: map[string]*configs.ModuleCall{
					"mod": {
						Name: "mod",
					},
				},
			},
			Children: map[string]*configs.Config{
				"mod": {
					Path: addrs.Module{"module.mod"},
					Module: &configs.Module{
						Outputs: map[string]*configs.Output{
							"out": {
								Name:      "out",
								Sensitive: true,
							},
						},
					},
				},
			},
		},
		State:       stateSync,
		Changes:     changesSync,
		Instances:   instances.NewExpander(nil),
		NamedValues: namedvals.NewState(),
	}
}
