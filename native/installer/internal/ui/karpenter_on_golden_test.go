package ui

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// karpenterOnScreens are the screens of runs with KARPENTER=on and no
// KARPENTER_NODE_OVERLAY, as defaultScreens records them, but for the
// Karpenter NodeOverlay row, which Settings shows with KARPENTER=on: its
// Fields keep it, and the Settings screen is drawn without it. The runs: a
// new cluster with config.env's defaults (Auto Mode off), the same with
// self-managed node groups on the bundled PostgreSQL with the demo, and one
// with the gvisor class alone.
func karpenterOnScreens(t *testing.T) map[string]defaultScreen {
	t.Helper()
	out := map[string]defaultScreen{}
	record := func(name string, m Model) {
		var ds defaultScreen
		set := *m.set
		set.fields = nil
		for i, f := range m.set.fields {
			ds.Fields = append(ds.Fields, f.key)
			if f.key == "KARPENTER_NODE_OVERLAY" {
				if i < m.set.cursor {
					set.cursor--
				}
				continue
			}
			set.fields = append(set.fields, f)
		}
		v := m
		v.set = &set
		ds.Settings = stripANSI(v.settingsView())
		m = press(m, "n")
		if m.m.Current() != state.Plugins {
			t.Fatalf("%s: at %s after n: %s", name, m.m.Current().Title(), m.notice)
		}
		m = press(m, "enter")
		if m.m.Current() != state.Review {
			t.Fatalf("%s: at %s after the plugins", name, m.m.Current().Title())
		}
		ds.Review = stripANSI(m.reviewView(0))
		for _, p := range m.plan() {
			ds.Plan = append(ds.Plan, defaultPlanStep{Step: p.step.Title(), Display: p.display, Summary: p.summary, Minutes: p.minutes,
				Hourly: jsonFloat(p.hourly), Unknown: p.unknown})
		}
		if _, items, ok := m.longStep(state.CreateCluster); ok {
			for _, it := range items {
				ds.Checklist = append(ds.Checklist, it.Label)
			}
		}
		out[name] = ds
	}
	record("new, Karpenter", karpenterOn(t, newSetup("fresh")))

	s := newSetup("fresh")
	s.DemoMode = true
	m := karpenterOn(t, s)
	m = press(cursorTo(t, m, "NODE_GROUP_TYPE"), "enter")
	m = press(cursorTo(t, m, dbKey), "enter")
	record("new, Karpenter, self-managed, bundled, demo", m)

	m = karpenterOn(t, newSetup("fresh"))
	m = press(cursorTo(t, m, state.ClassMicroVM), "enter")
	record("new, Karpenter, gvisor only", m)
	return out
}

// The screens of a run with KARPENTER=on and no KARPENTER_NODE_OVERLAY are
// what they were before KARPENTER_NODE_OVERLAY:
// testdata/karpenter-on-screens.json was recorded on this branch before it.
// Settings adds only the Karpenter NodeOverlay row, off, after Karpenter.
func TestKarpenterOnScreensUnchanged(t *testing.T) {
	got := karpenterOnScreens(t)
	raw, err := os.ReadFile("testdata/karpenter-on-screens.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]defaultScreen
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d runs, want %d", len(got), len(want))
	}
	for name, w := range want {
		g := got[name]
		if i := slices.Index(w.Fields, "KARPENTER"); i >= 0 && !slices.Contains(w.Fields, "KARPENTER_NODE_OVERLAY") {
			w.Fields = slices.Insert(slices.Clone(w.Fields), i+1, "KARPENTER_NODE_OVERLAY")
		}
		if !reflect.DeepEqual(g, w) {
			b, _ := json.MarshalIndent(g, "", " ")
			t.Errorf("%s changed:\n%s", name, b)
		}
	}
}
