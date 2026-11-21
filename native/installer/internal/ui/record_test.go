package ui

import (
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
