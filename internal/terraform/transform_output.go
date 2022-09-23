package terraform

import (
	"log"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/plans"
)

// OutputTransformer is a GraphTransformer that adds all the outputs
// in the configuration to the graph.
//
// This is done for the apply graph builder even if dependent nodes
// aren't changing since there is no downside: the state will be available
// even if the dependent items aren't changing.
type OutputTransformer struct {
	Config  *configs.Config
	Changes *plans.Changes

	// If this is a planned destroy, root outputs are still in the configuration
	// so we need to record that we wish to remove them
	removeRootOutputs bool

	// Refresh-only mode means that any failing output preconditions are
	// reported as warnings rather than errors
	RefreshOnly bool
}

func (t *OutputTransformer) Transform(g *Graph) error {
	return t.transform(g, t.Config)
}

func (t *OutputTransformer) transform(g *Graph, c *configs.Config) error {
	// If we have no config then there can be no outputs.
	if c == nil {
		return nil
	}

	// Transform all the children. We must do this first because
	// we can reference module outputs and they must show up in the
	// reference map.
	for _, cc := range c.Children {
		if err := t.transform(g, cc); err != nil {
			return err
		}
	}

	// Add outputs to the graph, which will be dynamically expanded
	// into NodeApplyableOutputs to reflect possible expansion
	// through the presence of "count" or "for_each" on the modules.

	var changes []*plans.OutputChangeSrc
	if t.Changes != nil {
		changes = t.Changes.Outputs
	}

	for _, o := range c.Module.Outputs {
		addr := addrs.OutputValue{Name: o.Name}

		var rootChange *plans.OutputChangeSrc
		for _, c := range changes {
			if c.Addr.Module.IsRoot() && c.Addr.OutputValue.Name == o.Name {
				rootChange = c
			}
		}

		destroy := t.removeRootOutputs
		if rootChange != nil {
			destroy = rootChange.Action == plans.Delete
		}

		// If this is a root output and we're destroying, we add the destroy
		// node directly, as there is no need to expand.

		var node dag.Vertex
		switch {
		case c.Path.IsRoot() && destroy:
			node = &NodeDestroyableOutput{
				Addr:   addr.Absolute(addrs.RootModuleInstance),
				Config: o,
			}

		default:
			node = &nodeExpandOutput{
				Addr:        addr,
				Module:      c.Path,
				Config:      o,
				Destroy:     t.removeRootOutputs,
				RefreshOnly: t.RefreshOnly,
			}
		}

		log.Printf("[TRACE] OutputTransformer: adding %s as %T", o.Name, node)
		g.Add(node)
	}

	return nil
}
