package cmd

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// karpenterOnTeardownCalls are every call inventory and teardown make on a
// deployment made with KARPENTER=on and no KARPENTER_NODE_OVERLAY
// (karpenterDeployment), AWS's (recordTD) and the Kubernetes API's, in
// order: its inventory, its teardown, --plan and --keep-cluster, and its
// teardown when the Kubernetes API does not answer. Each run ends with a
// "result" line.
func karpenterOnTeardownCalls(t *testing.T) map[string][]string {
	t.Helper()
	runs := []struct {
		name                         string
		plan, keep, inventory, unrch bool
	}{
		{"inventory", false, false, true, false},
		{"teardown", false, false, false, false},
		{"teardown --plan", true, false, false, false},
		{"teardown --keep-cluster", false, true, false, false},
		{"teardown, unreachable cluster", false, false, false, true},
	}
	out := map[string][]string{}
	for _, r := range runs {
		kd := karpenterDeployment(t)
		if r.unrch {
			kd.f.kube = unreachableKube(t)
		}
		e, _, _ := tdTestEnv(t, kd.f, r.plan, r.keep)
		// No settle window: the number of late-instance scans would depend
		// on timing (TestTeardownKarpenterCutsOffWithNoInstance covers it).
		karpenterSettle = 0
		log := &callLog{}
		e.c = recordTD(e.c, log)
		var err error
		if r.inventory {
			_, err = e.inventory(context.Background())
		} else {
			err = e.teardown(context.Background())
		}
		result := "result ok"
		if err != nil {
			result = "result " + err.Error()
		}
		calls := append(slices.Clone(log.calls), result)
		for _, c := range kd.kube.calls {
			calls = append(calls, "Kubernetes "+c)
		}
		out[r.name] = calls
	}
	return out
}

// A teardown of a deployment made with KARPENTER=on and no
// KARPENTER_NODE_OVERLAY makes the calls it made before
// KARPENTER_NODE_OVERLAY, with the same inputs, in the same order:
// testdata/karpenter-on-teardown-calls.json was recorded on this branch
// before it, with the same fixtures and recorder.
func TestTeardownKarpenterOnIsUnchanged(t *testing.T) {
	got := karpenterOnTeardownCalls(t)
	raw, err := os.ReadFile("testdata/karpenter-on-teardown-calls.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d runs, want %d", len(got), len(want))
	}
	for name, calls := range want {
		g := got[name]
		if slices.Equal(g, calls) {
			continue
		}
		i := 0
		for i < len(g) && i < len(calls) && g[i] == calls[i] {
			i++
		}
		at := func(s []string) string {
			if i < len(s) {
				return s[i]
			}
			return "(end)"
		}
		t.Errorf("%s: %d calls, want %d; first difference at %d:\n got  %s\n want %s", name, len(g), len(calls), i, at(g), at(calls))
	}
}
