package views

import (
	"fmt"

	"github.com/hashicorp/terraform/internal/command/arguments"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/terraform"
)

// FIXME: this is a temporary partial definition of the view for the show
// command, in place to allow access to the plan renderer which is now in the
// views package.
type Show interface {
	Plan(plan *plans.Plan, schemas *terraform.Schemas)
}

// FIXME: the show view should support both human and JSON types. This code is
// currently only used to render the plan in human-readable UI, so does not yet
// support JSON.
func NewShow(vt arguments.ViewType, view *View) Show {
	switch vt {
	case arguments.ViewHuman:
		return &ShowHuman{View: *view}
	default:
		panic(fmt.Sprintf("unknown view type %v", vt))
	}
}

type ShowHuman struct {
	View
}

var _ Show = (*ShowHuman)(nil)

func (v *ShowHuman) Plan(plan *plans.Plan, schemas *terraform.Schemas) {
	renderPlan(plan, schemas, &v.View)
}
