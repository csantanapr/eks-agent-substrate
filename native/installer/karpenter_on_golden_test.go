package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// karpenterOnSpecs are every step's Spec for new clusters with KARPENTER=on
// and no KARPENTER_NODE_OVERLAY: config.env's defaults without Auto Mode,
// the same with self-managed node groups on the bundled PostgreSQL with the
// demo, and the gvisor class alone.
func karpenterOnSpecs(t *testing.T) map[string]map[string]defaultSpec {
	t.Helper()
	b := builder{self: "/w/native/bin/substrate-eks", setupAWS: "/w/native/tools/setup-aws", cwd: "/w/native", treeRoot: controlplane.DefaultTreeRoot()}
	setup := func(change func(*state.Setup)) *state.Setup {
		s := state.NewSetupFrom(func(string) (string, bool) { return "", false })
		s.Region, s.Profile, s.Account, s.Plugins = "us-west-2", "eks-sandbox", "111122223333", []string{}
		s.NewClusterConfig.AutoMode, s.NewClusterConfig.Karpenter = "", "on"
		change(s)
		s.NewCluster("demo")
		return s
	}
	runs := map[string]*state.Setup{
		"new, Karpenter": setup(func(*state.Setup) {}),
		"new, Karpenter, self-managed, bundled, demo": setup(func(s *state.Setup) {
			s.NewClusterConfig.NodeGroupType = "self-managed"
			s.Database, s.DemoMode = state.DatabaseBundled, true
		}),
		"new, Karpenter, gvisor only": setup(func(s *state.Setup) { s.SandboxClasses = "gvisor" }),
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

// The steps of a run with KARPENTER=on and no KARPENTER_NODE_OVERLAY run
// what they ran before KARPENTER_NODE_OVERLAY:
// testdata/karpenter-on-specs.json was recorded on this branch before it.
// The merge of main (e36ba8f) changed it only where main's own defaults
// changed: the teardown row "Deleting the node groups..." (#17).
// The one change is KARPENTER_NODE_OVERLAY= in create cluster's
// environment, a cluster knob like the others: empty, so setup-aws reads
// it as off, and not shown.
func TestKarpenterOnSpecsUnchanged(t *testing.T) {
	got := karpenterOnSpecs(t)
	raw, err := os.ReadFile("testdata/karpenter-on-specs.json")
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
				if n := slices.Index(g.Env, "KARPENTER_NODE_OVERLAY="); n < 0 || slices.Contains(w.Env, "KARPENTER_NODE_OVERLAY=") {
					t.Errorf("%s: create cluster's environment has no empty KARPENTER_NODE_OVERLAY: %q", name, g.Env)
				} else {
					g.Env = slices.Delete(slices.Clone(g.Env), n, n+1)
				}
			}
			if !reflect.DeepEqual(g, w) {
				b, _ := json.MarshalIndent(g, "", " ")
				t.Errorf("%s: %s changed:\n%s", name, step, b)
			}
		}
	}
}
