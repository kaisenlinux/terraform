// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package moduleref

import (
	"testing"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/modsdir"
)

func TestResolver_Resolve(t *testing.T) {
	cfg := configs.NewEmptyConfig()
	cfg.Module = &configs.Module{
		ModuleCalls: map[string]*configs.ModuleCall{
			"foo": {Name: "foo"},
		},
	}

	manifest := modsdir.Manifest{
		"foo": modsdir.Record{
			Key:        "foo",
			SourceAddr: "./foo",
		},
		"bar": modsdir.Record{
			Key:        "bar",
			SourceAddr: "./bar",
		},
	}

	resolver := NewResolver(manifest)
	result := resolver.Resolve(cfg)

	if len(result.Records) != 1 {
		t.Fatalf("expected the resolved number of entries to equal 1, got: %d", len(result.Records))
	}

	// For the foo record
	if result.Records[0].Key != "foo" {
		t.Fatal("expected to find reference for module \"foo\"")
	}
}

func TestResolver_ResolveNestedChildren(t *testing.T) {
	cfg := configs.NewEmptyConfig()
	cfg.Children = make(map[string]*configs.Config)
	cfg.Module = &configs.Module{
		ModuleCalls: map[string]*configs.ModuleCall{
			"foo": {Name: "foo"},
		},
	}

	childCfg := &configs.Config{
		Path:     addrs.Module{"fellowship"},
		Children: make(map[string]*configs.Config),
		Module: &configs.Module{
			ModuleCalls: map[string]*configs.ModuleCall{
				"frodo": {Name: "frodo"},
			},
		},
	}

	childCfg2 := &configs.Config{
		Path:     addrs.Module{"fellowship", "weapons"},
		Children: make(map[string]*configs.Config),
		Module: &configs.Module{
			ModuleCalls: map[string]*configs.ModuleCall{
				"sting": {Name: "sting"},
			},
		},
	}

	cfg.Children["fellowship"] = childCfg
	childCfg.Children["weapons"] = childCfg2

	manifest := modsdir.Manifest{
		"foo": modsdir.Record{
			Key:        "foo",
			SourceAddr: "./foo",
		},
		"bar": modsdir.Record{
			Key:        "bar",
			SourceAddr: "./bar",
		},
		"fellowship.frodo": modsdir.Record{
			Key:        "fellowship.frodo",
			SourceAddr: "fellowship/frodo",
		},
		"fellowship.weapons.sting": modsdir.Record{
			Key:        "fellowship.weapons.sting",
			SourceAddr: "fellowship/weapons/sting",
		},
		"fellowship.weapons.anduril": modsdir.Record{
			Key:        "fellowship.weapons.anduril",
			SourceAddr: "fellowship/weapons/anduril",
		},
	}

	resolver := NewResolver(manifest)
	result := resolver.Resolve(cfg)

	if len(result.Records) != 3 {
		t.Fatalf("expected the resolved number of entries to equal 3, got: %d", len(result.Records))
	}

	assertions := map[string]bool{
		"foo":                        true,
		"bar":                        false,
		"fellowship.frodo":           true,
		"fellowship.weapons.sting":   true,
		"fellowship.weapons.anduril": false,
	}

	for _, record := range result.Records {
		referenced, ok := assertions[record.Key]
		if !ok || !referenced {
			t.Fatalf("expected to find referenced entry with key: %s", record.Key)
		}
	}
}
