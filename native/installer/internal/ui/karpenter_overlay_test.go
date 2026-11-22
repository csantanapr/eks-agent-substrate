package ui

import (
	"math"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// overlayOn is karpenterOn with KARPENTER_NODE_OVERLAY=on.
func overlayOn(t *testing.T, s *state.Setup) Model {
	t.Helper()
	m := press(cursorTo(t, karpenterOn(t, s), "KARPENTER_NODE_OVERLAY"), "enter")
	if m.set.values["KARPENTER_NODE_OVERLAY"] != "on" {
		t.Fatalf("KARPENTER_NODE_OVERLAY=%q", m.set.values["KARPENTER_NODE_OVERLAY"])
	}
	return m
}

// nitroOnGroup turns on the nitro class on the nitro node group
// (NITRO_COMPUTE=node-group), with a Nitro Sandbox AMI: Karpenter needs Auto
// Mode off, so turning the class on sets NITRO_COMPUTE=node-group (Settings'
// switch, switchNitro) and says so.
func nitroOnGroup(t *testing.T, m Model) Model {
	t.Helper()
	m = press(cursorTo(t, m, state.ClassNitro), "enter")
	if !m.set.nitro.on || !strings.Contains(m.View(), nitroSwitchLine) {
		t.Fatalf("no switch to node-group: %+v", m.set.nitro)
	}
	m = setText(cursorTo(t, m, "NITRO_AMI_ID"), "ami-0a17000000000000e")
	if !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup {
		t.Fatalf("classes %q, NITRO_COMPUTE=%q", m.set.values[classKey], m.set.values["NITRO_COMPUTE"])
	}
	return m
}

// The Karpenter overlay row: none without KARPENTER=on (Auto Mode on, or
// Auto Mode off with Karpenter off), off once Karpenter is on, and enter
// turns it on. Then the microvm and nitro rows say NodePool, the panel says
// it is alpha, n applies it, and the checklist is Karpenter's. Turning
// Karpenter off turns the overlay off and hides the row. A connected
// cluster has no row.
func TestSettingsKarpenterOverlayRow(t *testing.T) {
	s := newSetup("fresh")
	m := toSettings(t, testDeps(&recorder{}), s)
	if hasField(m, "KARPENTER_NODE_OVERLAY") {
		t.Fatal("Auto Mode on has an overlay row")
	}
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	if hasField(m, "KARPENTER_NODE_OVERLAY") {
		t.Fatal("Karpenter off has an overlay row")
	}
	m = press(cursorTo(t, m, "KARPENTER"), "enter")
	if l := rowOf(t, m, "Karpenter overlay"); !strings.Contains(l, "Karpenter overlay  off") {
		t.Fatalf("row %q", l)
	}
	if l := strings.TrimRight(rowOf(t, m, "microvm"), " "); !strings.HasSuffix(l, "2    nested virt") {
		t.Errorf("overlay off: microvm row %q", l)
	}
	// The nitro class on the nitro group, with its AMI.
	m = nitroOnGroup(t, m)
	if l := rowOf(t, m, "nitro"); !strings.Contains(l, " 1    Nitro Enclaves  ") {
		t.Errorf("overlay off: nitro row %q", l)
	}
	m = press(cursorTo(t, m, "KARPENTER_NODE_OVERLAY"), "enter")
	if l := rowOf(t, m, "Karpenter overlay"); !strings.Contains(l, "Karpenter overlay  on") {
		t.Fatalf("row %q", l)
	}
	// The panel beside the rows can end a row's line.
	for g, note := range map[string]string{"gvisor": " 2    NodePool  ", "microvm": " 2    nested, NodePool  ", "nitro": " 1    NodePool  ", "system": " 2  "} {
		if l := rowOf(t, m, g) + "  "; !strings.Contains(l, note) {
			t.Errorf("%s row %q", g, l)
		}
	}
	if v := flat(stripANSI(m.settingsView())); !strings.Contains(v, "About this setting on: Karpenter also makes") ||
		!strings.Contains(help["KARPENTER_NODE_OVERLAY"], "NodeOverlay is an alpha Karpenter API: it can change") {
		t.Errorf("panel:\n%s", v)
	}
	if len(m.set.errs) != 0 {
		t.Fatalf("findings %v", m.set.errs)
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || !s.NewClusterConfig.NodeOverlayOn() {
		t.Fatalf("at %s, KARPENTER_NODE_OVERLAY=%q: %s", m.m.Current().Title(), s.NewClusterConfig.KarpenterNodeOverlay, m.notice)
	}
	if _, pools := s.NewClusterConfig.SplitPools(s.NodeGroups()); strings.Join(pools, ",") != "gvisor,microvm,nitro" {
		t.Errorf("pools %v", pools)
	}
	if _, items, _ := m.longStep(state.CreateCluster); len(items) != 11 || items[10].Label != "Installing Karpenter and its NodePools" {
		t.Errorf("checklist %+v", items)
	}

	m = press(cursorTo(t, overlayOn(t, newSetup("fresh")), "KARPENTER"), "enter")
	if m.set.values["KARPENTER_NODE_OVERLAY"] != "" || hasField(m, "KARPENTER_NODE_OVERLAY") {
		t.Errorf("Karpenter off: KARPENTER_NODE_OVERLAY=%q, row %v", m.set.values["KARPENTER_NODE_OVERLAY"], hasField(m, "KARPENTER_NODE_OVERLAY"))
	}
	if m = toStep(t, New(testDeps(&recorder{}), newSetup("new")), state.Settings); hasField(m, "KARPENTER_NODE_OVERLAY") {
		t.Error("a connected cluster has an overlay row")
	}
}

// Both ways for the overlay's findings: on without Karpenter (from the
// config files) shows the row, even on Auto Mode, with the finding, and n
// stops; Karpenter on clears it. A value that is not on, off or empty is
// refused. The minimal profile turns it off.
func TestSettingsKarpenterOverlayFindings(t *testing.T) {
	s := newSetup("fresh")
	s.NewClusterConfig.KarpenterNodeOverlay = "on"
	m := toSettings(t, testDeps(&recorder{}), s)
	if !hasField(m, "KARPENTER_NODE_OVERLAY") || m.set.errs["KARPENTER_NODE_OVERLAY"] != "needs Karpenter on" {
		t.Fatalf("overlay without Karpenter: %v", m.set.errs)
	}
	if m = press(m, "n"); m.m.Current() != state.Settings {
		t.Fatalf("n went on to %s", m.m.Current().Title())
	}
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	m = press(cursorTo(t, m, "KARPENTER"), "enter")
	if _, bad := m.set.errs["KARPENTER_NODE_OVERLAY"]; bad || m.set.values["KARPENTER_NODE_OVERLAY"] != "on" {
		t.Errorf("Karpenter on: %v", m.set.errs)
	}
	s = newSetup("fresh")
	s.NewClusterConfig.KarpenterNodeOverlay = "yes"
	m = toSettings(t, testDeps(&recorder{}), s)
	if !hasField(m, "KARPENTER_NODE_OVERLAY") || m.set.errs["KARPENTER_NODE_OVERLAY"] != "on, off, or empty for off" {
		t.Errorf("KARPENTER_NODE_OVERLAY=yes: %v", m.set.errs)
	}
	s = newSetup("fresh")
	s.NewClusterConfig.KarpenterNodeOverlay = "off"
	if m = toSettings(t, testDeps(&recorder{}), s); hasField(m, "KARPENTER_NODE_OVERLAY") {
		t.Error("KARPENTER_NODE_OVERLAY=off without Karpenter has a row")
	}
	m = press(overlayOn(t, newSetup("fresh")), "m")
	if m.set.values["KARPENTER_NODE_OVERLAY"] != "" || hasField(m, "KARPENTER_NODE_OVERLAY") || len(m.set.errs) != 0 {
		t.Errorf("minimal profile: KARPENTER_NODE_OVERLAY=%q, errs %v", m.set.values["KARPENTER_NODE_OVERLAY"], m.set.errs)
	}
}

// With KARPENTER_NODE_OVERLAY=on the microvm group is a NodePool, so mode 3
// is not offered: one chosen before is dropped, and choosing it says why
// and names the setting. With KARPENTER=on alone it is offered
// (TestPluginsScreenOffersMode3WithKarpenter).
func TestPluginsScreenRefusesMode3WithOverlay(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = []string{"t-kata"}
	m := press(overlayOn(t, s), "n")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-kata"] || !strings.Contains(m.notice, "not selected: t-kata") {
		t.Fatalf("at %s, chosen %v: %q", m.m.Current().Title(), m.pl.chosen, m.notice)
	}
	m = press(pluginAt(m, "t-kata"), " ")
	if m.pl.chosen["t-kata"] || m.notice != "Kata pool needs a label kata-deploy sets; KARPENTER_NODE_OVERLAY makes the microvm node group a Karpenter NodePool (Settings: Karpenter overlay)" {
		t.Errorf("t-kata: chosen %v, %q", m.pl.chosen, m.notice)
	}
}

// Review with KARPENTER_NODE_OVERLAY=on names the NodeOverlay as alpha and
// the three NodePools with their limits; it prices the system group alone
// up to the three NodePools at their limits, which is what the cluster
// costs with node groups. The Create the cluster step says what it makes.
func TestReviewNodeOverlay(t *testing.T) {
	s := newSetup("fresh")
	m := overlayOn(t, s)
	m = nitroOnGroup(t, m)
	m = press(m, "n", "enter")
	if m.m.Current() != state.Review {
		t.Fatalf("at %s: %s", m.m.Current().Title(), m.notice)
	}
	var cc planStep
	for _, p := range m.plan() {
		if p.step == state.CreateCluster {
			cc = p
		}
	}
	c := s.NewClusterConfig
	fixed, _ := clusterHourly(c, []string{"system"})
	c.Karpenter, c.KarpenterNodeOverlay = "", ""
	all, _ := clusterHourly(c, s.NodeGroups())
	if math.Abs(cc.hourly-fixed) > 1e-9 || math.Abs(cc.hourly+cc.upTo-all) > 1e-9 || cc.pools != 3 {
		t.Errorf("hourly %.4f up to %.4f (%d pools); want %.4f up to %.4f", cc.hourly, cc.upTo, cc.pools, fixed, all)
	}
	v := stripANSI(m.reviewView(0))
	for _, want := range []string{"Node groups: system 2×m8i.xlarge", "Karpenter 1.15.2 with NodeOverlay (alpha) NodePools gvisor up to 2×m8i.2xlarg…"} {
		if !hasLine(v, want) {
			t.Errorf("review lacks the line %q:\n%s", want, v)
		}
	}
	if !strings.Contains(flat(v), "The Karpenter NodePools cost $0 with no node") {
		t.Errorf("review:\n%s", v)
	}
	m.jump(state.InstallOrder(s), state.CreateCluster)
	if v := flat(strings.ReplaceAll(stripANSI(m.body(0, 0)), "│", " ")); !strings.Contains(v, "its system node group and Karpenter's gvisor, microvm and nitro NodePools") {
		t.Errorf("Create the cluster:\n%s", v)
	}
	if s.NewClusterConfig.NitroCompute != plugin.NitroNodeGroup {
		t.Errorf("NITRO_COMPUTE=%q", s.NewClusterConfig.NitroCompute)
	}
}
