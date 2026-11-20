package cmd

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// defaultTeardownCalls are every call inventory and teardown make on
// deployments made without KARPENTER (teardown_record_test.go records
// them): the full deployment, its --plan and --keep-cluster, a deployment
// of self-managed node groups, and an empty account. Each run ends with a
// "result" line.
func defaultTeardownCalls(t *testing.T) map[string][]string {
	t.Helper()
	runs := []struct {
		name       string
		f          func() *tdFake
		plan, keep bool
		inventory  bool
	}{
		{"inventory", fullDeployment, false, false, true},
		{"teardown", fullDeployment, false, false, false},
		{"teardown --plan", fullDeployment, true, false, false},
		{"teardown --keep-cluster", fullDeployment, false, true, false},
		{"teardown, self-managed", selfManagedDeployment, false, false, false},
		{"teardown, empty account", newTDFake, false, false, false},
	}
	out := map[string][]string{}
	for _, r := range runs {
		e, _, _ := tdTestEnv(t, r.f(), r.plan, r.keep)
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
		out[r.name] = append(log.calls, result)
	}
	return out
}

// A teardown of a deployment made without KARPENTER makes the calls it made
// before KARPENTER, with the same inputs, in the same order:
// testdata/teardown-default-calls.json was recorded on the branch before it
// (nitro-node-groups, fa5605a), with the same fixtures and recorder.
func TestTeardownWithoutKarpenterIsUnchanged(t *testing.T) {
	got := defaultTeardownCalls(t)
	raw, err := os.ReadFile("testdata/teardown-default-calls.json")
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
	for name, calls := range got {
		for _, c := range calls {
			for _, op := range []string{"SQS.", "Events.", "Objects", "EC2.TerminateInstances"} {
				if strings.HasPrefix(c, op) {
					t.Errorf("%s: called %s", name, c)
				}
			}
		}
	}
}
