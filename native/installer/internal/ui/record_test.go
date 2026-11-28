package ui

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// recordOnly is the probe answer of a cluster without ate-system whose
// plugin record lists t-efs and, pending, mode3-iso deployed with nitro.
func recordOnly(nitro string) string {
	return probe.Marker + "false pools=gvisor,microvm,nitro,system plugins=t-efs pending=mode3-iso nitro-compute=" + nitro
}

// Issue #22: a cluster whose record outlived its control plane is badged
// with what the record lists, and enter chooses it, with its record, as a
// clean one: it is not held on the guard.
func TestARecordOnlyClusterIsBadgedAndChosen(t *testing.T) {
	r := &recorder{probes: map[string]string{"new": recordOnly(plugin.NitroNodeGroup)}}
	s := newSetup("new")
	m := toStep(t, New(testDeps(r), s), state.Cluster)
	v := stripANSI(m.View())
	// Joined as one line, with the wrap after a hyphen undone.
	panel := strings.ReplaceAll(strings.Join(strings.Fields(stripANSI(m.clusterPanel(0))), " "), "- ", "-")
	if !strings.Contains(v, "partial install: plugins mode3-iso, t-efs") ||
		!strings.Contains(panel, "new has no ate-system namespace, and its plugin record (kube-system/substrate-eks-plugins) still lists mode3-iso (pending, node-group), t-efs. Enter installs here") {
		t.Errorf("no record-only badge and panel:\n%s\n%s", v, panel)
	}
	m = press(m, "enter")
	want := plugin.Record{Plugins: []string{"t-efs"}, Pending: []string{"mode3-iso"}, NitroCompute: plugin.NitroNodeGroup}
	if m.m.Current() != state.Database || m.cl.mode != "list" || s.ClusterIsNew || !s.ClusterRecord.Equal(want) {
		t.Errorf("at %s, mode %q, record %+v", m.m.Current().Title(), m.cl.mode, s.ClusterRecord)
	}
}

// Review refuses a chosen plugin recorded with another NITRO_COMPUTE than
// the run's, or with one this installer does not know, as the control
// plane step's record check would after Provision and RDS: the screen names
// it with its ways out and y starts nothing. Both ways: the recorded
// NITRO_COMPUTE starts; so does a run without that plugin (the class off),
// and a chosen plugin the record holds no NITRO_COMPUTE for (t-efs).
func TestReviewRefusesAPluginRecordedWithAnotherNitroCompute(t *testing.T) {
	const ways = "run the wizard with --nitro-compute node-group, or remove it first with substrate-eks --plugin-action remove --plugins mode3-iso --cluster new --region us-west-2 (which removes it with NITRO_COMPUTE=node-group), then run the wizard again"
	for _, tc := range []struct {
		name, recorded, run, classes string
		plugins                      []string
		refused                      string
	}{
		{"another mode", plugin.NitroNodeGroup, plugin.NitroAutoMode, "gvisor,nitro", nil,
			"plugin mode3-iso was deployed with NITRO_COMPUTE=node-group (recorded in kube-system/substrate-eks-plugins), not auto-mode: " + ways},
		{"an unknown mode", "future-mode", plugin.NitroAutoMode, "gvisor,nitro", nil,
			`plugin mode3-iso: kube-system/substrate-eks-plugins records nitro-compute="future-mode", which is not auto-mode or node-group: this installer cannot deploy, check or remove it`},
		{"the recorded mode", plugin.NitroNodeGroup, plugin.NitroNodeGroup, "gvisor,nitro", nil, ""},
		{"without the plugin", plugin.NitroNodeGroup, plugin.NitroAutoMode, "gvisor", []string{"t-efs"}, ""},
	} {
		r := &recorder{probes: map[string]string{"new": recordOnly(tc.recorded)}}
		s := newSetup("new")
		s.SandboxClasses, s.Plugins, s.NewClusterConfig.NitroCompute = tc.classes, tc.plugins, tc.run
		m := sized(toStep(t, New(testDeps(r), s), state.Review))
		if s.NewClusterConfig.NitroCompute != tc.run || state.HasClass(s.SandboxClasses, state.ClassNitro) != strings.Contains(tc.classes, "nitro") {
			t.Fatalf("%s: Settings changed the run: NITRO_COMPUTE=%s, classes %s", tc.name, s.NewClusterConfig.NitroCompute, s.SandboxClasses)
		}
		v := strings.Join(strings.Fields(stripANSI(m.View())), " ")
		m = press(m, "y")
		if tc.refused == "" {
			if strings.Contains(v, "Nothing can start") || m.m.Current() == state.Review || len(r.ran()) == 0 {
				t.Errorf("%s: at %s, ran %q:\n%s", tc.name, m.m.Current().Title(), r.ran(), v)
			}
			continue
		}
		if !strings.Contains(v, "Nothing can start: "+tc.refused[:40]) || m.m.Current() != state.Review || len(r.ran()) != 0 ||
			!strings.Contains(m.notice, "nothing started: "+tc.refused) {
			t.Errorf("%s: at %s, ran %q, notice %q:\n%s", tc.name, m.m.Current().Title(), r.ran(), m.notice, v)
		}
	}
}

// Issue #25: Review (nitroRecordErr) and the control plane step's record
// check (plugin.CheckNitroRecord, whose rule Run's deploy shares) judge the
// same record and run by one rule (plugin.Record.NitroConflict): both refuse
// or both pass, and both name the same recorded value. The cases are the
// ones the review of #22 traced: the run's default, a pending plugin, an
// unknown recorded value, class and required plugins, a plugin with no
// recorded value, and one that does not follow NITRO_COMPUTE. The plugin
// package reads the ConfigMap Review's record is parsed from, through a
// fake kubectl. An unknown recorded value gets the same refusal, byte for
// byte, from both.
func TestReviewAndThePluginPackageAgreeOnTheRecordedNitroCompute(t *testing.T) {
	exec := plugin.Exec
	t.Cleanup(func() { plugin.Exec = exec })
	named := regexp.MustCompile(`NITRO_COMPUTE=(\S+) \(recorded in |nitro-compute="([^"]*)"`)
	// name is the recorded value err names: "" for no refusal.
	name := func(err error) string {
		if err == nil {
			return ""
		}
		if g := named.FindStringSubmatch(err.Error()); g != nil {
			return g[1] + g[2]
		}
		return "no recorded value in " + err.Error()
	}
	for _, tc := range []struct {
		name, data, classes, run string
		plugins                  []string
		names                    string
	}{
		{"the run's default, another mode", `"plugins":"mode3-iso","nitro-compute":"node-group"`, "gvisor,nitro", "", nil, plugin.NitroNodeGroup},
		{"the run's default, the recorded mode", `"plugins":"mode3-iso","nitro-compute":"auto-mode"`, "gvisor,nitro", "", nil, ""},
		{"another mode", `"plugins":"mode3-iso","nitro-compute":"auto-mode"`, "gvisor,nitro", plugin.NitroNodeGroup, nil, plugin.NitroAutoMode},
		{"the recorded mode", `"plugins":"mode3-iso","nitro-compute":"node-group"`, "gvisor,nitro", plugin.NitroNodeGroup, nil, ""},
		{"a pending plugin", `"plugins":"t-efs","pending":"mode3-iso","nitro-compute":"node-group"`, "gvisor,nitro", plugin.NitroAutoMode, nil, plugin.NitroNodeGroup},
		{"a pending plugin, the recorded mode", `"plugins":"","pending":"mode3-iso","nitro-compute":"node-group"`, "gvisor,nitro", plugin.NitroNodeGroup, nil, ""},
		{"an unknown mode", `"plugins":"mode3-iso","nitro-compute":"future-mode"`, "gvisor,nitro", plugin.NitroNodeGroup, nil, "future-mode"},
		{"an unknown mode, pending", `"plugins":"","pending":"mode3-iso","nitro-compute":"future-mode"`, "gvisor,nitro", "", nil, "future-mode"},
		{"chosen as a plugin, not by its class", `"plugins":"mode3-iso","nitro-compute":"auto-mode"`, "gvisor", plugin.NitroNodeGroup, []string{"mode3-iso"}, plugin.NitroAutoMode},
		{"a class plugin beside a required one", `"plugins":"mode3-iso,t-efs","nitro-compute":"node-group"`, "gvisor,nitro", plugin.NitroAutoMode, []string{"t-s3files"}, plugin.NitroNodeGroup},
		{"a required plugin in a record with a mode", `"plugins":"t-efs,t-s3files","nitro-compute":"node-group"`, "gvisor", plugin.NitroAutoMode, []string{"t-s3files"}, ""},
		{"no recorded value", `"plugins":"mode3-iso"`, "gvisor,nitro", plugin.NitroNodeGroup, nil, ""},
		{"not in the record", `"plugins":"t-efs","nitro-compute":"node-group"`, "gvisor,nitro", plugin.NitroAutoMode, nil, ""},
		{"a plugin that does not follow NITRO_COMPUTE", `"plugins":"t-efs","nitro-compute":"node-group"`, "gvisor", plugin.NitroAutoMode, []string{"t-efs"}, ""},
		{"no record", "", "gvisor,nitro", plugin.NitroNodeGroup, nil, ""},
	} {
		js := ""
		if tc.data != "" {
			js = `{"metadata":{"name":"substrate-eks-plugins","namespace":"kube-system","resourceVersion":"7"},"data":{` + tc.data + `}}`
		}
		rec, err := plugin.ParseRecordJSON([]byte(js))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		read := append([]string{"kubectl", "--kubeconfig", "/kc"}, plugin.RecordArgs...)
		plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
			if !slices.Equal(c.Argv, read) {
				return fmt.Errorf("not the record read: %q", c.Argv)
			}
			_, err := io.WriteString(c.Stdout, js)
			return err
		}
		s := newSetup("new")
		s.SandboxClasses, s.Plugins, s.NewClusterConfig.NitroCompute, s.ClusterRecord = tc.classes, tc.plugins, tc.run, rec
		review := Model{s: s}.nitroRecordErr()
		cp := plugin.CheckNitroRecord(context.Background(), io.Discard, plugin.Options{Kubeconfig: "/kc", NitroCompute: tc.run}, s.InstallPlugins())
		if name(review) != tc.names || name(cp) != tc.names {
			t.Errorf("%s: want both to name %q (\"\" for no refusal):\nReview: %v\nplugin: %v", tc.name, tc.names, review, cp)
			continue
		}
		if plugin.CheckNitroCompute(tc.names) != nil && tc.names != "" && review.Error() != cp.Error() {
			t.Errorf("%s: an unknown mode, refused in other words:\nReview: %v\nplugin: %v", tc.name, review, cp)
		}
	}
}
