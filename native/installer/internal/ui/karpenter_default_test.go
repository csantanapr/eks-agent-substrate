package ui

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// defaultScreen is what one run without KARPENTER shows: the Settings
// screen as it opens, its rows, then Review's plan, view and Create the
// cluster checklist.
type defaultScreen struct {
	Settings  string
	Fields    []string
	Review    string
	Plan      []defaultPlanStep
	Checklist []string
}

type defaultPlanStep struct {
	Step, Display   string
	Summary         bool
	Minutes         int
	Hourly, Unknown string
}

// defaultScreens are the runs the golden file holds: a new cluster with
// config.env's defaults, the same without Auto Mode (once on the bundled
// PostgreSQL with self-managed node groups and the demo), and a connected
// cluster.
func defaultScreens(t *testing.T) map[string]defaultScreen {
	t.Helper()
	out := map[string]defaultScreen{}
	record := func(name string, m Model) {
		var ds defaultScreen
		// The screen without the Karpenter row, which a new cluster
		// without Auto Mode shows; every other row and the panel stay.
		set := *m.set
		set.fields = nil
		for i, f := range m.set.fields {
			ds.Fields = append(ds.Fields, f.key)
			if f.key == "KARPENTER" {
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

	record("new", toSettings(t, testDeps(&recorder{}), newSetup("fresh")))

	m := toSettings(t, testDeps(&recorder{}), newSetup("fresh"))
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	record("new, Auto Mode off", m)

	s := newSetup("fresh")
	s.DemoMode = true
	m = toSettings(t, testDeps(&recorder{}), s)
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	m = press(cursorTo(t, m, "NODE_GROUP_TYPE"), "enter")
	m = press(cursorTo(t, m, dbKey), "enter")
	record("new, Auto Mode off, self-managed, bundled, demo", m)

	record("connected", toStep(t, New(testDeps(&recorder{}), newSetup("new")), state.Settings))
	return out
}

// jsonFloat keeps a cost exact across the JSON file.
func jsonFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// The screens of a run without KARPENTER are what they were before it:
// testdata/default-screens.json was recorded on the branch before KARPENTER
// (nitro-node-groups, fa5605a). Without Auto Mode, Settings adds only the
// Karpenter row, after Node group type.
func TestDefaultScreensUnchanged(t *testing.T) {
	got := defaultScreens(t)
	raw, err := os.ReadFile("testdata/default-screens.json")
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
		// Without Auto Mode a new cluster has the Karpenter row, off.
		if strings.Contains(name, "Auto Mode off") {
			i := slices.Index(w.Fields, "NODE_GROUP_TYPE")
			w.Fields = slices.Insert(slices.Clone(w.Fields), i+1, "KARPENTER")
		}
		if !reflect.DeepEqual(g, w) {
			b, _ := json.MarshalIndent(g, "", " ")
			t.Errorf("%s changed:\n%s", name, b)
		}
	}
}
