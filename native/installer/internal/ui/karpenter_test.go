package ui

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// rowOf is the Settings line of the row labeled label (a setting, or a
// node group).
func rowOf(t *testing.T, m Model, label string) string {
	t.Helper()
	for _, l := range strings.Split(stripANSI(m.settingsView()), "\n") {
		if strings.HasPrefix(l, "  "+label+" ") {
			return l
		}
	}
	t.Fatalf("no %s row:\n%s", label, m.View())
	return ""
}

// flat is v with each line's padding gone and its lines joined by spaces,
// as text that wraps reads.
func flat(v string) string { return strings.Join(strings.Fields(v), " ") }

// hasLine reports a line of v that is want, its padding aside.
func hasLine(v, want string) bool {
	for _, l := range strings.Split(v, "\n") {
		if strings.TrimRight(l, " ") == want {
			return true
		}
	}
	return false
}

// karpenterOn is the Settings of a new cluster with Auto Mode off and
// KARPENTER=on.
func karpenterOn(t *testing.T, s *state.Setup) Model {
	t.Helper()
	m := toSettings(t, testDeps(&recorder{}), s)
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	m = press(cursorTo(t, m, "KARPENTER"), "enter")
	if m.set.values["KARPENTER"] != "on" {
		t.Fatalf("KARPENTER=%q", m.set.values["KARPENTER"])
	}
	return m
}

// The Karpenter row: a new cluster with Auto Mode has none (the default
// screen is unchanged); without Auto Mode it shows off, and enter turns it
// on. The gvisor row says its count is a NodePool's limit; the system and
// microvm rows stay node groups; the panel says what Karpenter makes; n
// applies it, and the Create the cluster checklist gets Karpenter's two
// rows. A connected cluster has no row.
func TestSettingsKarpenterRow(t *testing.T) {
	s := newSetup("fresh")
	m := toSettings(t, testDeps(&recorder{}), s)
	if hasField(m, "KARPENTER") {
		t.Fatalf("Auto Mode on has a Karpenter row:\n%s", m.View())
	}
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	if l := rowOf(t, m, "Karpenter"); !strings.Contains(l, "Karpenter          off") {
		t.Fatalf("row %q", l)
	}
	for _, g := range []string{"gvisor", "microvm"} {
		if l := rowOf(t, m, g); strings.Contains(l, "NodePool") {
			t.Errorf("KARPENTER off: %s row %q", g, l)
		}
	}
	m = press(cursorTo(t, m, "KARPENTER"), "enter")
	if l := rowOf(t, m, "Karpenter"); !strings.Contains(l, "Karpenter          on") {
		t.Fatalf("row %q", l)
	}
	for g, note := range map[string]string{"gvisor": "2    NodePool", "microvm": "2    nested virt", "system": "2"} {
		if l := strings.TrimRight(rowOf(t, m, g), " "); !strings.HasSuffix(l, note) {
			t.Errorf("%s row %q", g, l)
		}
	}
	if !strings.Contains(stripANSI(m.settingsView()), "\n"+karpenterNodes+"   ") {
		t.Errorf("no %q line:\n%s", karpenterNodes, m.settingsView())
	}
	if v := flat(stripANSI(m.settingsView())); !strings.Contains(v, "Now: KARPENTER=on") || !strings.Contains(help["KARPENTER"], "Karpenter 1.15.2 makes the gvisor nodes in a NodePool") ||
		!strings.Contains(help["KARPENTER"], "The microvm and nitro classes stay on their node groups") || strings.Contains(help["KARPENTER"], "mode 3") {
		t.Errorf("panel:\n%s", v)
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.Karpenter != "on" || !s.Karpenter() {
		t.Fatalf("at %s, KARPENTER=%q: %s", m.m.Current().Title(), s.NewClusterConfig.Karpenter, m.notice)
	}
	_, items, _ := m.longStep(state.CreateCluster)
	if len(items) != 11 || items[9].Label != "Creating Karpenter's IAM role and interruption queue" || items[10].Label != "Installing Karpenter and its NodePools" {
		t.Errorf("checklist %+v", items)
	}

	m = toStep(t, New(testDeps(&recorder{}), newSetup("new")), state.Settings)
	if hasField(m, "KARPENTER") {
		t.Errorf("a connected cluster has a Karpenter row:\n%s", m.View())
	}
}

// Both ways for each finding of KARPENTER on a new cluster. It needs Auto
// Mode off: turning Auto Mode back on keeps the row, with the finding, and
// n stops. It needs the gvisor group: with only the nitro or the microvm
// class it is refused, and the microvm group a plugin adds (mode 3) does
// not count. TAGS may not set a key Karpenter
// sets, with it; the Karpenter marker is refused with or without it, on a
// new cluster only. A value that is not on, off or empty shows the row
// even on Auto Mode, with the finding. The minimal profile turns it off.
func TestSettingsKarpenterFindings(t *testing.T) {
	m := karpenterOn(t, newSetup("fresh"))
	if _, bad := m.set.errs["KARPENTER"]; bad {
		t.Fatalf("on, Auto Mode off: %v", m.set.errs)
	}
	m = press(cursorTo(t, m, "AUTO_MODE"), "enter")
	if !hasField(m, "KARPENTER") || m.set.errs["KARPENTER"] != "needs EKS Auto Mode off: Auto Mode runs its own Karpenter" {
		t.Fatalf("Auto Mode on: %v", m.set.errs)
	}
	if m = press(m, "n"); m.m.Current() != state.Settings || !strings.Contains(m.notice, "need a fix") {
		t.Fatalf("n went on to %s", m.m.Current().Title())
	}

	// The nitro class alone, on the nitro node group.
	s := newSetup("fresh")
	m = karpenterOn(t, s)
	m = press(cursorTo(t, m, state.ClassNitro), "enter")
	m = press(cursorTo(t, m, "NITRO_COMPUTE"), "enter")
	m = press(cursorTo(t, m, state.ClassNitro), "enter")
	m = press(cursorTo(t, m, state.ClassGVisor), "enter")
	m = press(cursorTo(t, m, state.ClassMicroVM), "enter")
	if m.set.values[classKey] != "nitro" || m.set.errs["KARPENTER"] != "makes a NodePool for gVisor: turn that class on" {
		t.Fatalf("classes %q: %v", m.set.values[classKey], m.set.errs)
	}
	m = press(cursorTo(t, m, state.ClassMicroVM), "enter")
	if m.set.errs["KARPENTER"] != "makes a NodePool for gVisor: turn that class on" {
		t.Errorf("microvm on: %v", m.set.errs)
	}
	m = press(cursorTo(t, m, state.ClassGVisor), "enter")
	if _, bad := m.set.errs["KARPENTER"]; bad {
		t.Errorf("gvisor on: %v", m.set.errs)
	}
	// The microvm group a plugin adds (mode 3) makes no NodePool.
	s = newSetup("fresh")
	s.Plugins, s.SandboxClasses = []string{"t-kata"}, "nitro"
	s.NewClusterConfig.NitroCompute = plugin.NitroNodeGroup
	m = karpenterOn(t, s)
	if m.set.errs["KARPENTER"] != "makes a NodePool for gVisor: turn that class on" {
		t.Errorf("mode 3's microvm group counted: %v", m.set.errs)
	}

	m = karpenterOn(t, newSetup("fresh"))
	m = setText(cursorTo(t, m, "TAGS"), "team=a karpenter.sh/discovery=x")
	if m.set.errs["TAGS"] != "karpenter.sh/discovery is a key Karpenter sets itself: remove it" {
		t.Errorf("Karpenter's key with Karpenter: %v", m.set.errs)
	}
	m = press(cursorTo(t, m, "KARPENTER"), "enter")
	if _, bad := m.set.errs["TAGS"]; bad {
		t.Errorf("Karpenter's key without Karpenter: %v", m.set.errs)
	}
	m = setText(cursorTo(t, m, "TAGS"), "agent-substrate-on-eks/karpenter=on")
	if m.set.errs["TAGS"] != "agent-substrate-on-eks/karpenter is a key setup-aws sets itself: remove it" {
		t.Errorf("the marker without Karpenter: %v", m.set.errs)
	}
	s = newSetup("new")
	s.Tags = "agent-substrate-on-eks/karpenter=on"
	m = toStep(t, New(testDeps(&recorder{}), s), state.Settings)
	if _, bad := m.set.errs["TAGS"]; bad {
		t.Errorf("the marker on a connected cluster, which create cluster does not run on: %v", m.set.errs)
	}

	s = newSetup("fresh")
	s.NewClusterConfig.Karpenter = "yes"
	m = toSettings(t, testDeps(&recorder{}), s)
	if !hasField(m, "KARPENTER") || m.set.errs["KARPENTER"] != "on, off, or empty for off" {
		t.Errorf("KARPENTER=yes on Auto Mode: %v", m.set.errs)
	}
	s = newSetup("fresh")
	s.NewClusterConfig.Karpenter = "off"
	m = toSettings(t, testDeps(&recorder{}), s)
	if hasField(m, "KARPENTER") {
		t.Errorf("KARPENTER=off on Auto Mode has a row")
	}

	m = press(karpenterOn(t, newSetup("fresh")), "m")
	if m.set.values["KARPENTER"] != "" || m.set.values["AUTO_MODE"] != "on" || hasField(m, "KARPENTER") || len(m.set.errs) != 0 {
		t.Errorf("minimal profile: KARPENTER=%q, errs %v", m.set.values["KARPENTER"], m.set.errs)
	}
}

// With KARPENTER=on the microvm group stays a node group, so a plugin on it
// that cannot run on Karpenter's nodes (mode 3) is offered as before: one
// chosen before stays chosen, and it can be turned off and on.
func TestPluginsScreenOffersMode3WithKarpenter(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = []string{"t-kata"}
	m := press(karpenterOn(t, s), "n")
	if m.m.Current() != state.Plugins || !m.pl.chosen["t-kata"] || strings.Contains(m.notice, "not selected") {
		t.Fatalf("at %s, chosen %v: %q", m.m.Current().Title(), m.pl.chosen, m.notice)
	}
	m = press(pluginAt(m, "t-kata"), " ")
	m = press(pluginAt(m, "t-kata"), " ")
	if !m.pl.chosen["t-kata"] {
		t.Errorf("chosen %v: %q", m.pl.chosen, m.notice)
	}
	if groups := s.NodeGroups(); !slices.Contains(groups, "microvm") || s.NewClusterConfig.IsPool("microvm") {
		t.Errorf("groups %v", groups)
	}
}

// Review with KARPENTER=on names Karpenter and its NodePool with its limit,
// and prices the cluster from its node groups alone ($0 for the NodePool
// with no node) up to the NodePool at its limit, which is what the same
// cluster costs with node groups. The Create the cluster step says what it
// makes.
func TestReviewKarpenter(t *testing.T) {
	s := newSetup("fresh")
	m := press(karpenterOn(t, s), "n", "enter")
	if m.m.Current() != state.Review {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	var cc planStep
	for _, p := range m.plan() {
		if p.step == state.CreateCluster {
			cc = p
		}
	}
	c := s.NewClusterConfig
	fixed, _ := clusterHourly(c, []string{"system", "microvm"})
	c.Karpenter = ""
	all, _ := clusterHourly(c, s.NodeGroups())
	if math.Abs(cc.hourly-fixed) > 1e-9 || math.Abs(cc.hourly+cc.upTo-all) > 1e-9 || cc.upTo < 0.85 || cc.pools != 1 {
		t.Errorf("hourly %.4f up to %.4f; want %.4f up to %.4f", cc.hourly, cc.upTo, fixed, all)
	}
	v := stripANSI(m.reviewView(0))
	for _, want := range []string{
		"Cluster fresh (new: this run creates it) · EKS 1.37 · Auto Mode off · us-west…",
		"Node groups: system 2×m8i.xlarge · microvm 2×m8i.2xlarge",
		"Karpenter 1.15.2 NodePool gvisor up to 2×m8i.2xlarge",
	} {
		if !hasLine(v, want) {
			t.Errorf("review lacks the line %q:\n%s", want, v)
		}
	}
	for _, want := range []string{
		" 1 Create the cluster ~20 min $1.49–2.38 ",
		"Cost: about $1.55 to $2.44 an hour ($37 to $59 a day) until teardown,",
		"us-west-2. The Karpenter NodePool costs $0 with no node, and up to $0.89 an hour at its node limit.",
	} {
		if !strings.Contains(flat(v), strings.TrimSpace(want)) {
			t.Errorf("review lacks %q:\n%s", want, v)
		}
	}
	m.jump(state.InstallOrder(s), state.CreateCluster)
	if v := flat(strings.ReplaceAll(stripANSI(m.body(0, 0)), "│", " ")); !strings.Contains(v, "VPC, IAM roles, EKS cluster fresh, its system and microvm node groups and Karpenter's gvisor NodePool") ||
		!strings.Contains(v, "Karpenter NodePool gvisor up to 2×m8i.2xlarge") {
		t.Errorf("Create the cluster:\n%s", v)
	}
	if head, line := costLine([]planStep{{hourly: 1}}, region); strings.Contains(line, "Karpenter") || !strings.Contains(head, "billable") {
		t.Errorf("no NodePools: %s", line)
	}
	if _, line := costLine([]planStep{{hourly: 1, upTo: 2, pools: 2}}, region); !strings.Contains(line, "The Karpenter NodePools cost $0 with no node, and up to $2.00 an hour at their node limits.") {
		t.Errorf("two NodePools: %s", line)
	}
}

// A connected cluster with Karpenter NodePools (the probe's nodepools= and
// karpenter): Settings and Review name them, and Review says Karpenter; a
// plugin that cannot run on Karpenter's nodes is not offered where its
// node group (microvm) is a NodePool, and is where only gvisor's is.
func TestConnectedClusterWithKarpenter(t *testing.T) {
	r := &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=gvisor,microvm,system nodepools=gvisor,microvm karpenter"}}
	s := newSetup("new")
	m := toStep(t, New(testDeps(r), s), state.Settings)
	if v := stripANSI(m.settingsView()); !s.ClusterKarpenter || !strings.Contains(v, "\n  node groups: system   ") || !strings.Contains(v, "\n  Karpenter NodePools: gvisor, microvm   ") {
		t.Fatalf("settings:\n%s", m.View())
	}
	if _, bad := m.set.errs[classKey]; bad {
		t.Errorf("the classes on NodePool-only pools: %v", m.set.errs)
	}
	m = press(m, "n")
	m = press(pluginAt(m, "t-kata"), " ")
	if m.pl.chosen["t-kata"] || m.notice != "Kata pool needs a label kata-deploy sets; the microvm node group of new is a Karpenter NodePool" {
		t.Errorf("t-kata: %q", m.notice)
	}
	m = press(m, "enter")
	v := stripANSI(m.reviewView(0))
	if !hasLine(v, "Node groups on the cluster: system") || !hasLine(v, "Karpenter NodePools on the cluster: gvisor, microvm") {
		t.Errorf("review:\n%s", v)
	}
	// Only the gvisor pool is a NodePool (KARPENTER=on): mode 3 is offered.
	r = &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=gvisor,microvm,system nodepools=gvisor karpenter"}}
	m = press(toStep(t, New(testDeps(r), newSetup("new")), state.Settings), "n")
	if m = press(pluginAt(m, "t-kata"), " "); !m.pl.chosen["t-kata"] {
		t.Errorf("t-kata with a gvisor NodePool only: %q", m.notice)
	}
	// Karpenter without a NodePool of this installer.
	r = &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=gvisor,microvm,system karpenter"}}
	m = press(toStep(t, New(testDeps(r), newSetup("new")), state.Settings), "n", "enter")
	if v := stripANSI(m.reviewView(0)); !strings.Contains(v, "Node groups on the cluster: system, gvisor, microvm") ||
		!strings.Contains(v, "Karpenter on the cluster, with no NodePool of this installer") {
		t.Errorf("review, no NodePool:\n%s", v)
	}
}
