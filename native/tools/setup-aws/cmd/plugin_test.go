package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

// fakePlugin is a pluginDef whose one teardown target and inventory row are
// in memory: deleted records the delete, left is what inventory sees.
type fakePlugin struct {
	deleted, refuse bool
	order           *[]string
}

func (p *fakePlugin) def(id string, cluster bool) pluginDef {
	return pluginDef{ID: id,
		Teardown: func(*tdEnv) []pluginStep {
			return []pluginStep{{Cluster: cluster, Step: func(context.Context) ([]tdTarget, []string, error) {
				if p.refuse {
					return nil, []string{id + " thing owned by someone else"}, nil
				}
				if p.deleted {
					return nil, nil, nil
				}
				return []tdTarget{{kind: "thing", id: id, del: func(context.Context) error {
					*p.order = append(*p.order, "delete "+id)
					p.deleted = true
					return nil
				}}}, nil, nil
			}}}
		},
		Inventory: func(context.Context, *tdEnv) ([]tdRow, error) {
			if p.deleted {
				return nil, nil
			}
			return []tdRow{{Kind: "thing", ID: id}}, nil
		},
	}
}

// The plugin phase runs before the rest of the teardown and its leftovers
// count in the final inventory; --keep-cluster keeps a step marked Cluster.
func TestTeardownRunsPluginsFirst(t *testing.T) {
	f := fullDeployment()
	e, _, logs := tdTestEnv(t, f, false, false)
	var order []string
	a, b := &fakePlugin{order: &order}, &fakePlugin{order: &order}
	pluginDefs["a"], pluginDefs["b"] = a.def("a", false), b.def("b", false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "delete a" || order[1] != "delete b" {
		t.Errorf("plugin deletes = %v", order)
	}
	if len(f.calls) == 0 {
		t.Fatal("core teardown deleted nothing")
	}
	l := logs.String()
	if i, j := strings.Index(l, "delete thing a"), strings.Index(l, phases.Line(phases.Teardown, 2)); i < 0 || j < 0 || i > j {
		t.Errorf("plugin delete not before phase 2:\n%s", l)
	}
}

func TestTeardownKeepClusterKeepsPluginClusterSteps(t *testing.T) {
	f := fullDeployment()
	e, _, _ := tdTestEnv(t, f, false, true)
	var order []string
	data, infra := &fakePlugin{order: &order}, &fakePlugin{order: &order}
	pluginDefs["data"], pluginDefs["infra"] = data.def("data", false), infra.def("infra", true)
	ps := e.pluginPhase()
	if len(ps.steps) != 1 {
		t.Fatalf("keep-cluster: %d plugin steps, want 1 (the data one)", len(ps.steps))
	}
	e.keepCluster = false
	if ps := e.pluginPhase(); len(ps.steps) != 2 {
		t.Fatalf("full teardown: %d plugin steps, want 2", len(ps.steps))
	}
}

// Both directions for `setup-aws plugin <id> --delete`: it deletes the
// plugin's resources and checks nothing is left; a refusal stops it before
// any delete.
func TestDeletePlugin(t *testing.T) {
	f := fullDeployment()
	e, _, logs := tdTestEnv(t, f, false, false)
	var order []string
	p := &fakePlugin{order: &order}
	if err := e.deletePlugin(context.Background(), p.def("x", true)); err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 || !strings.Contains(logs.String(), "Plugin x deleted") {
		t.Errorf("order %v, logs:\n%s", order, logs)
	}
	if len(f.calls) != 0 {
		t.Errorf("deleting a plugin touched the core deployment: %v", f.calls)
	}

	order = nil
	r := &fakePlugin{order: &order, refuse: true}
	err := e.deletePlugin(context.Background(), r.def("y", false))
	if err == nil || !strings.Contains(err.Error(), "someone else") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if len(order) != 0 {
		t.Errorf("deleted after a refusal: %v", order)
	}
}
