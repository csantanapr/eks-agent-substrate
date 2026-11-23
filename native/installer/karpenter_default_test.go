package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// defaultSpec is a Spec as the golden file holds it.
type defaultSpec struct {
	Label, Display string
	Argv, Env      []string
	SimLines       []string
	Summary        bool
}

// defaultSpecs are every step's Spec for runs without KARPENTER: a new
// cluster with config.env's defaults, one without Auto Mode, one of
// self-managed node groups on the bundled PostgreSQL, and a connected
// cluster. The environment is config.env's defaults only.
func defaultSpecs(t *testing.T) map[string]map[string]defaultSpec {
	t.Helper()
	b := builder{self: "/w/native/bin/substrate-eks", setupAWS: "/w/native/tools/setup-aws", cwd: "/w/native", treeRoot: controlplane.DefaultTreeRoot()}
	setup := func(change func(*state.Setup)) *state.Setup {
		s := state.NewSetupFrom(func(string) (string, bool) { return "", false })
		s.Region, s.Profile, s.Account, s.Plugins = "us-west-2", "eks-sandbox", "111122223333", []string{}
		s.NewCluster("demo")
		change(s)
		return s
	}
	runs := map[string]*state.Setup{
		"new": setup(func(*state.Setup) {}),
		"new, Auto Mode off": setup(func(s *state.Setup) {
			s.NewClusterConfig.AutoMode = ""
			s.NewCluster("demo")
		}),
		"new, Auto Mode off, self-managed, bundled, demo": setup(func(s *state.Setup) {
			s.NewClusterConfig.AutoMode, s.NewClusterConfig.NodeGroupType = "", "self-managed"
			s.Database, s.DemoMode = state.DatabaseBundled, true
			s.NewCluster("demo")
		}),
		"connected": setup(func(s *state.Setup) {
			s.SelectCluster(simCluster("demo", "us-west-2", true), []string{"gvisor", "microvm", "system"}, nil, false, plugin.Record{})
		}),
	}
	out := map[string]map[string]defaultSpec{}
	for name, s := range runs {
		specs := map[string]defaultSpec{}
		for step, sp := range allSpecs(b, s) {
			specs[step] = asDefaultSpec(sp)
		}
		out[name] = specs
	}
	return out
}

func asDefaultSpec(sp execx.Spec) defaultSpec {
	return defaultSpec{Label: sp.Label, Display: sp.Display, Argv: sp.Argv, Env: sp.Env, SimLines: sp.SimLines, Summary: sp.DisplayIsSummary}
}

// The steps of a run without KARPENTER run what they ran before it:
// testdata/default-specs.json was recorded on main before KARPENTER
// (e36ba8f, v2.1.0). The one change is KARPENTER= and
// KARPENTER_NODE_OVERLAY= in create cluster's environment, cluster knobs
// like the others: empty, so setup-aws reads them as off, and not shown.
func TestDefaultSpecsUnchanged(t *testing.T) {
	got := defaultSpecs(t)
	raw, err := os.ReadFile("testdata/default-specs.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]map[string]defaultSpec
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d runs, want %d", len(got), len(want))
	}
	for name, specs := range want {
		if len(got[name]) != len(specs) {
			t.Errorf("%s: %d steps, want %d", name, len(got[name]), len(specs))
		}
		for step, w := range specs {
			g := got[name][step]
			if step == "create cluster" {
				for _, knob := range []string{"KARPENTER=", "KARPENTER_NODE_OVERLAY="} {
					if n := slices.Index(g.Env, knob); n < 0 || slices.Contains(w.Env, knob) {
						t.Errorf("%s: create cluster's environment has no empty %s: %q", name, knob, g.Env)
					} else {
						g.Env = slices.Delete(slices.Clone(g.Env), n, n+1)
					}
				}
			}
			if !reflect.DeepEqual(g, w) {
				b, _ := json.MarshalIndent(g, "", " ")
				t.Errorf("%s: %s changed:\n%s", name, step, b)
			}
		}
	}
}
