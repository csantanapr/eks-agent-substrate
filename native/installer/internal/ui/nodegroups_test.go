package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
)

// groupRows are the node groups Settings has rows for.
func groupRows(m Model) []string {
	var out []string
	for _, f := range m.set.fields {
		if f.group != "" && !slices.Contains(out, f.group) {
			out = append(out, f.group)
		}
	}
	return out
}

// Settings shows rows only for the node groups the new cluster gets, and
// they follow the classes, the database and Auto Mode as they are toggled:
// the default three; gVisor alone, system and gvisor with RDS and gvisor
// alone with the bundled PostgreSQL; the AWS Nitro sandbox alone on Auto
// Mode, none, with the line that says so.
func TestSettingsShowsOnlyTheNodeGroupsTheClusterGets(t *testing.T) {
	m := toSettings(t, testDeps(&recorder{}), newSetup("fresh"))
	if got := groupRows(m); !slices.Equal(got, []string{"system", "gvisor", "microvm"}) {
		t.Fatalf("default rows %v", got)
	}
	m = cursorTo(t, m, state.ClassMicroVM)
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"system", "gvisor"}) || strings.Contains(m.View(), "MICROVM") || strings.Contains(m.View(), "nested virt") {
		t.Errorf("gVisor alone: rows %v\n%s", got, m.View())
	}
	// The bundled PostgreSQL takes the system row away on Auto Mode; RDS
	// brings it back.
	m = cursorTo(t, m, dbKey)
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"gvisor"}) {
		t.Errorf("gVisor alone, bundled: rows %v\n%s", got, m.View())
	}
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"system", "gvisor"}) {
		t.Errorf("gVisor alone, RDS again: rows %v\n%s", got, m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	m = cursorTo(t, m, state.ClassGVisor)
	m = press(m, "enter")
	if got := groupRows(m); len(got) != 0 || !strings.Contains(m.View(), noGroupsLine) {
		t.Errorf("Nitro alone: rows %v\n%s", got, m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || m.s.SandboxClasses != "nitro" || len(m.s.NodeGroups()) != 0 {
		t.Errorf("at %s, classes %q, groups %v", m.m.Current().Title(), m.s.SandboxClasses, m.s.NodeGroups())
	}
}

// m sets the minimal profile: Auto Mode on, the AWS Nitro sandbox alone,
// the bundled PostgreSQL; the run then makes no node group and has no RDS
// step, and Review says everything runs on Auto Mode nodes. Its cost is the
// control plane, the NAT gateway and the class's Auto Mode node, with no
// node group; the default's is unchanged (TestReviewShowsThePlanTimesAndCost).
func TestMinimalProfileMakesNoNodeGroup(t *testing.T) {
	r := &recorder{}
	s := newSetup("fresh")
	m := toSettings(t, testDeps(r), s)
	m = cursorTo(t, m, "AUTO_MODE")
	m = press(m, "enter") // off: m turns it back on
	m = press(m, "m")
	if v := m.set.values; v["AUTO_MODE"] != "on" || v[classKey] != "nitro" || v[dbKey] != state.DatabaseBundled {
		t.Fatalf("minimal values %v", v)
	}
	m = press(m, "n", "enter")
	if m.m.Current() != state.Review {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	v := m.View()
	for _, want := range []string{"No node groups: everything runs on EKS Auto Mode nodes", "classes nitro", "$0.15", "$0.12"} {
		if !strings.Contains(v, want) {
			t.Errorf("review lacks %q:\n%s", want, v)
		}
	}
	if slices.Contains(m.m.Order(), state.RDS) || !slices.Contains(m.m.Order(), state.NitroSandbox) {
		t.Errorf("order %v", m.m.Order())
	}
	if usd, _ := clusterHourly(s.NewClusterConfig, nil); usd != eksHourly+natHourly {
		t.Errorf("no node group: $%.4f", usd)
	}
}

// pluginAt puts the Plugins screen's cursor on id.
func pluginAt(m Model, id string) Model {
	for i, p := range choosable() {
		if p.ID == id {
			m.pl.cursor = i
		}
	}
	return m
}

// The Plugins screen refuses, with the reason, a plugin pinned to a sandbox
// group whose class is off (t-claw on gvisor), and never one on the system
// group: choosing it makes that group. On the minimal profile the default
// gateway would add the system group, so it is not preselected, and the
// screen says so; chosen, it makes the system group, and stays when t-kata
// (the microvm group) comes and goes. Review then shows the system group.
func TestPluginsScreenRefusesAPluginWhoseNodesTheRunDoesNotMake(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = nil // the defaults
	m := toSettings(t, testDeps(&recorder{}), s)
	m = press(m, "m", "n")
	if v := m.View(); m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || m.notice != "not preselected: t-gateway (adds the system node group)" ||
		!strings.Contains(v, "Adds the system node group to the new cluster") || strings.Contains(v, "Not available") {
		t.Fatalf("the default gateway on the minimal profile: notice %q\n%s", m.notice, v)
	}
	m = press(pluginAt(m, "t-gateway"), " ")
	if !m.pl.chosen["t-gateway"] {
		t.Errorf("gateway refused on the minimal profile:\n%s", m.View())
	}
	m = press(pluginAt(m, "t-kata"), " ")
	m = press(pluginAt(m, "t-kata"), " ")
	if m.pl.chosen["t-kata"] || !m.pl.chosen["t-gateway"] {
		t.Errorf("t-kata on and off: %v", m.pl.chosen)
	}
	m = press(pluginAt(m, "t-claw"), " ")
	if m.pl.chosen["t-claw"] || m.notice != "Claw agent runs on the gvisor node group: turn on its sandbox class in Settings" {
		t.Errorf("t-claw chosen without the gvisor group:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !slices.Equal(s.Plugins, []string{"t-gateway"}) || !slices.Equal(s.NodeGroups(), []string{"system"}) ||
		!strings.Contains(m.View(), "Node groups: system 2×m8i.xlarge") {
		t.Errorf("at %s, plugins %v, groups %v\n%s", m.m.Current().Title(), s.Plugins, s.NodeGroups(), m.View())
	}
}

// The default gateway is preselected where the run makes the system group
// anyway (the default: Auto Mode, RDS, gvisor and microvm), and not with the
// bundled PostgreSQL and the gVisor class, which then make only the gvisor
// group; Review shows exactly that group and its cost.
func TestDefaultGatewayIsPreselectedOnlyWhereItAddsNoNodeGroup(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = nil
	m := press(toSettings(t, testDeps(&recorder{}), s), "n")
	if m.m.Current() != state.Plugins || !m.pl.chosen["t-gateway"] || strings.Contains(m.notice, "not preselected") ||
		strings.Contains(m.View(), "Adds the system node group") {
		t.Fatalf("default run, at %s: chosen %v, notice %q\n%s", m.m.Current().Title(), m.pl.chosen, m.notice, m.View())
	}

	s = newSetup("fresh")
	s.Plugins = nil
	m = toSettings(t, testDeps(&recorder{}), s)
	m = cursorTo(t, m, state.ClassMicroVM)
	m = press(m, "enter")
	m = cursorTo(t, m, dbKey)
	m = press(m, "enter")
	if got := groupRows(m); !slices.Equal(got, []string{"gvisor"}) || m.set.values[dbKey] != state.DatabaseBundled {
		t.Fatalf("bundled, gVisor: rows %v, database %q\n%s", got, m.set.values[dbKey], m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || m.notice != "not preselected: t-gateway (adds the system node group)" {
		t.Fatalf("bundled, gVisor, at %s: chosen %v, notice %q\n%s", m.m.Current().Title(), m.pl.chosen, m.notice, m.View())
	}
	m = press(m, "enter")
	v := m.View()
	if m.m.Current() != state.Review || !slices.Equal(s.NodeGroups(), []string{"gvisor"}) || !strings.Contains(v, "Node groups: gvisor 2×m8i.2xlarge") ||
		strings.Contains(v, "system 2×") {
		t.Errorf("at %s, groups %v\n%s", m.m.Current().Title(), s.NodeGroups(), v)
	}
	if usd, _ := clusterHourly(s.NewClusterConfig, s.NodeGroups()); usd != eksHourly+natHourly+2*(ec2Price["m8i.2xlarge"]+200*gp3PerGBMonth/hoursPerMonth) {
		t.Errorf("bundled, gVisor: $%.4f an hour", usd)
	}

	// A connected cluster gets no node group from the run: its system
	// nodes are there, so the gateway is preselected, whatever the database
	// and classes (a new cluster with these would make system and gvisor,
	// not the three the cluster has).
	r := &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=system,gvisor,microvm"}}
	s = newSetup("new")
	s.Plugins, s.Database, s.SandboxClasses = nil, state.DatabaseBundled, state.ClassGVisor
	m = press(toStep(t, New(testDeps(r), s), state.Settings), "n")
	if m.m.Current() != state.Plugins || !m.pl.chosen["t-gateway"] || strings.Contains(m.notice, "not preselected") {
		t.Errorf("connected, at %s: chosen %v, notice %q", m.m.Current().Title(), m.pl.chosen, m.notice)
	}
}

// A connected cluster: no node group is made, its own (the probe's) decide.
// A class whose node group it lacks is refused in Settings, naming it, and
// a plugin pinned to such a group is refused on the Plugins screen.
func TestConnectedClusterRefusesWhatItsNodesLack(t *testing.T) {
	r := &recorder{probes: map[string]string{"new": "SUBSTRATE_EKS_INSTALLED false pools=system,microvm"}}
	s := newSetup("new")
	m := toStep(t, New(testDeps(r), s), state.Settings)
	if !slices.Equal(s.ClusterPools, []string{"microvm", "system"}) || !strings.Contains(m.View(), "node groups: system, microvm") || len(groupRows(m)) != 0 {
		t.Fatalf("pools %v:\n%s", s.ClusterPools, m.View())
	}
	// gvisor is on by default: n names the missing group.
	m = press(m, "n")
	if m.m.Current() != state.Settings || m.set.errs[classKey] != "gVisor: no gvisor node group" {
		t.Fatalf("at %s, errs %v", m.m.Current().Title(), m.set.errs)
	}
	m = cursorTo(t, m, state.ClassGVisor)
	m = press(m, "enter", "enter") // off, and refused on again
	if m.set.classOn(state.ClassGVisor) || !strings.Contains(m.View(), "Not available: new has") || m.notice != "gVisor new has no gvisor node group (no node labeled ate.dev/pool=gvisor)" {
		t.Errorf("gvisor on without its nodes:\n%s", m.View())
	}
	m = press(m, "n")
	for i, p := range choosable() {
		if p.ID == "t-claw" {
			m.pl.cursor = i
		}
	}
	m = press(m, " ")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-claw"] || m.notice != "Claw agent runs on the gvisor node group, which new does not have" {
		t.Errorf("t-claw on a cluster without gvisor nodes:\n%s", m.View())
	}
}

// The Create the cluster step names the node groups it makes, one group in
// the singular: gvisor alone (Auto Mode, the bundled PostgreSQL), and
// system and gvisor with RDS.
func TestCreateClusterStepNamesItsNodeGroups(t *testing.T) {
	for db, want := range map[string]string{
		state.DatabaseBundled: "EKS cluster fresh and its gvisor node group ",
		state.DatabaseRDS:     "EKS cluster fresh and its system and gvisor node groups ",
	} {
		s := newSetup("fresh")
		s.SandboxClasses, s.Database = state.ClassGVisor, db
		s.NewCluster("fresh")
		m := sized(New(testDeps(&recorder{}), s))
		m.jump(state.InstallOrder(s), state.CreateCluster)
		m.tasks[state.CreateCluster] = newTask(execx.Spec{Label: "create cluster"}, steps.Cluster("", false))
		if v := stripANSI(m.View()); !strings.Contains(v, want) {
			t.Errorf("%s: want %q in\n%s", db, want, v)
		}
	}
}

// Review NIT-4: Settings checks only the node groups it shows, so a group a
// plugin chosen after it adds is checked when the Plugins screen moves on.
// t-kata adds the microvm group to a gVisor-only run, and the gateway the
// system group on Auto Mode with the bundled PostgreSQL. A bad setting of
// that group keeps the run on the Plugins screen, naming the key; back in
// Settings the group's row is shown with the error, and once it is fixed the
// run reaches Review.
func TestAGroupAPluginAddsIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name, plugin, key string
		bundled           bool
		set               func(*state.ClusterConfig)
	}{
		{"mode 3's microvm group", "t-kata", "MICROVM_NODES", false, func(c *state.ClusterConfig) { c.MicroVMNodes = "abc" }},
		{"the gateway's system group", "t-gateway", "SYSTEM_NODES", true, func(c *state.ClusterConfig) { c.SystemNodes = "0" }},
	} {
		s := newSetup("fresh")
		tc.set(&s.NewClusterConfig)
		m := toSettings(t, testDeps(&recorder{}), s)
		m = cursorTo(t, m, state.ClassMicroVM)
		m = press(m, "enter")
		if tc.bundled {
			m = cursorTo(t, m, dbKey)
			m = press(m, "enter")
		}
		m = press(m, "n")
		if m.m.Current() != state.Plugins {
			t.Fatalf("%s: Settings refused a group it does not show: %v", tc.name, m.set.errs)
		}
		m = press(pluginAt(m, tc.plugin), " ", "enter")
		if m.m.Current() != state.Plugins || !strings.HasPrefix(m.notice, tc.key+"=") || !strings.Contains(m.notice, "set it in Settings") {
			t.Fatalf("%s: at %s, notice %q", tc.name, m.m.Current().Title(), m.notice)
		}
		m = press(m, "esc")
		if _, bad := m.set.errs[tc.key]; m.m.Current() != state.Settings || !bad {
			t.Fatalf("%s: back at %s, errors %v", tc.name, m.m.Current().Title(), m.set.errs)
		}
		m = setText(cursorTo(t, m, tc.key), "2")
		m = press(m, "n", "enter")
		if m.m.Current() != state.Review {
			t.Errorf("%s: fixed, at %s, notice %q", tc.name, m.m.Current().Title(), m.notice)
		}
	}
}

// Review NIT-6: the help on every class row says when an Auto Mode cluster
// with neither gVisor nor microVM gets a node group: only from a plugin,
// mode 3's microvm group (with RDS the system group too), or the system
// group for a plugin that runs on it; none without one. The rule agrees.
// The help names only the system-group plugins the Plugins screen offers
// there (review of 1ca284d, NIT-1): the gateway, and login rotation with
// RDS; t-gateway and t-rot are this package's two.
func TestClassHelpSaysWhenAPluginAddsANodeGroup(t *testing.T) {
	for _, c := range []struct {
		db      string
		plugins []string
		want    []string
	}{
		{state.DatabaseBundled, nil, nil},
		{state.DatabaseRDS, nil, nil},
		{state.DatabaseBundled, []string{"t-kata"}, []string{"microvm"}},
		{state.DatabaseRDS, []string{"t-kata"}, []string{"system", "microvm"}},
		{state.DatabaseBundled, []string{"t-gateway"}, []string{"system"}},
	} {
		if got := state.NewNodeGroups(true, c.db, state.ClassNitro, plugin.NitroAutoMode, c.plugins); !slices.Equal(got, c.want) {
			t.Errorf("Auto Mode, %s, nitro, %v: %v, want %v", c.db, c.plugins, got, c.want)
		}
	}
	for db, want := range map[string][]string{state.DatabaseBundled: {"t-gateway"}, state.DatabaseRDS: {"t-gateway", "t-rot"}} {
		s := newSetup("fresh")
		s.NewCluster("fresh")
		s.AutoMode, s.SandboxClasses, s.Database = true, state.ClassNitro, db
		var offered []string
		for _, p := range choosable() {
			if slices.Contains(p.Nodes, "system") && s.PluginProblem(p, nil) == "" {
				offered = append(offered, p.ID)
			}
		}
		if !slices.Equal(offered, want) {
			t.Errorf("Auto Mode, %s, nitro: system-group plugins offered %v, want %v", db, offered, want)
		}
	}
	for _, c := range state.Classes {
		for _, want := range []string{
			"With neither class, an Auto Mode cluster gets a node group only from a plugin",
			"mode 3 adds the microvm group (and with RDS the system group)",
			"a plugin that runs on the system group (the LLM gateway, or login rotation with RDS) adds that one.",
			"Without such a plugin it gets none",
		} {
			if !strings.Contains(help[c], want) {
				t.Errorf("%s help lacks %q:\n%s", c, want, help[c])
			}
		}
	}
}

// Both ways: login rotation (t-rot, db-rotation's shape) is refused on the
// Plugins screen with the bundled PostgreSQL, with the reason and the row
// note "needs RDS", so it can neither add the system group nor fail in the
// plugins step; with RDS it is chosen.
func TestPluginsScreenRefusesLoginRotationOnTheBundledDatabase(t *testing.T) {
	for _, db := range []string{state.DatabaseBundled, state.DatabaseRDS} {
		s := newSetup("fresh")
		m := toSettings(t, testDeps(&recorder{}), s)
		m = cursorTo(t, m, state.ClassMicroVM)
		m = press(m, "enter")
		if db == state.DatabaseBundled {
			m = cursorTo(t, m, dbKey)
			m = press(m, "enter")
		}
		m = press(m, "n")
		m = press(pluginAt(m, "t-rot"), " ")
		v := m.View()
		if db == state.DatabaseBundled {
			if m.pl.chosen["t-rot"] || m.notice != "Login rotation rotates the RDS logins, so it needs the RDS database; the run uses the bundled PostgreSQL (Settings: Database)" ||
				!strings.Contains(v, "needs RDS") {
				t.Errorf("bundled: chosen %v, notice %q\n%s", m.pl.chosen, m.notice, v)
			}
			m = press(m, "enter")
			if m.m.Current() != state.Review || len(s.Plugins) != 0 || !slices.Equal(s.NodeGroups(), []string{"gvisor"}) {
				t.Errorf("bundled: at %s, plugins %v, groups %v", m.m.Current().Title(), s.Plugins, s.NodeGroups())
			}
			continue
		}
		if !m.pl.chosen["t-rot"] || strings.Contains(v, "needs RDS") {
			t.Errorf("RDS: chosen %v, notice %q\n%s", m.pl.chosen, m.notice, v)
		}
	}
}

// m after the Plugins screen: the chosen plugins that would add a node group
// under the minimal profile, the default gateway (system) and t-kata
// (microvm), are cleared and named; t-efs, which adds none, stays. The run
// then makes no node group, as m says. Choosing one again on the Plugins
// screen makes its group, as before. m before the Plugins screen, on what
// --plugins named, clears the same plugins from the Setup itself, and the
// run reaches Review with none of them and no node group.
func TestMinimalProfileClearsAPluginThatAddsANodeGroup(t *testing.T) {
	s := newSetup("fresh")
	s.Plugins = nil // the defaults: the gateway, which the default run's system group fits
	m := press(toSettings(t, testDeps(&recorder{}), s), "n")
	m = press(pluginAt(m, "t-kata"), " ")
	m = press(pluginAt(m, "t-efs"), " ")
	m = press(m, "enter")
	if m.m.Current() != state.Review || !slices.Equal(s.Plugins, []string{"t-gateway", "t-efs", "t-kata"}) {
		t.Fatalf("at %s, plugins %v", m.m.Current().Title(), s.Plugins)
	}
	m = press(m, "esc", "esc")
	if m.m.Current() != state.Settings {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	m = press(m, "m")
	if m.notice != "Cleared t-gateway and t-kata for the minimal profile (no node group); choose again on the Plugins screen to keep the group" ||
		len(groupRows(m)) != 0 || !strings.Contains(m.View(), noGroupsLine) {
		t.Fatalf("notice %q, rows %v\n%s", m.notice, groupRows(m), m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || m.pl.chosen["t-kata"] || !m.pl.chosen["t-efs"] {
		t.Fatalf("at %s, chosen %v", m.m.Current().Title(), m.pl.chosen)
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !slices.Equal(s.Plugins, []string{"t-efs"}) || len(s.NodeGroups()) != 0 ||
		!strings.Contains(m.View(), "No node groups: everything runs on EKS Auto Mode nodes") {
		t.Errorf("at %s, plugins %v, groups %v\n%s", m.m.Current().Title(), s.Plugins, s.NodeGroups(), m.View())
	}

	// Before the Plugins screen: the selection is what --plugins named.
	s = newSetup("fresh")
	s.Plugins = []string{"t-gateway", "t-efs", "t-kata"}
	m = toSettings(t, testDeps(&recorder{}), s)
	if m.pl.shown {
		t.Fatal("the Plugins screen was shown before Settings")
	}
	m = press(m, "m")
	if m.notice != "Cleared t-gateway and t-kata for the minimal profile (no node group); choose again on the Plugins screen to keep the group" ||
		!slices.Equal(s.Plugins, []string{"t-efs"}) || len(groupRows(m)) != 0 {
		t.Errorf("before the Plugins screen: notice %q, plugins %v, rows %v", m.notice, s.Plugins, groupRows(m))
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || m.pl.chosen["t-gateway"] || m.pl.chosen["t-kata"] || !m.pl.chosen["t-efs"] {
		t.Errorf("before the Plugins screen: at %s, chosen %v", m.m.Current().Title(), m.pl.chosen)
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !slices.Equal(s.Plugins, []string{"t-efs"}) || len(s.NodeGroups()) != 0 {
		t.Errorf("before the Plugins screen: at %s, plugins %v, groups %v", m.m.Current().Title(), s.Plugins, s.NodeGroups())
	}
}

// The panel names the node group a row's plugin adds, given the rest of the
// selection without it and the chosen plugins that require it, which
// clearing it clears too (review of 1ca284d, NIT-3). On Auto Mode with the
// bundled PostgreSQL and gVisor alone (the gvisor group only), z-top adds
// the system group through z-base before it is chosen. Once it is, z-base's
// row says so, as EFS's does with S3 Files chosen: clearing z-base clears
// z-top, and the group goes with them. Review makes the group.
func TestPluginsPanelNamesTheGroupARequiredPluginAdds(t *testing.T) {
	const adds = "Adds the system node group to the new cluster"
	s := newSetup("fresh")
	m := toSettings(t, testDeps(&recorder{}), s)
	m = press(cursorTo(t, m, state.ClassMicroVM), "enter")
	m = press(cursorTo(t, m, dbKey), "enter")
	m = press(m, "n")
	if m = pluginAt(m, "z-top"); m.m.Current() != state.Plugins || !strings.Contains(m.View(), adds) {
		t.Fatalf("z-top not chosen, at %s:\n%s", m.m.Current().Title(), m.View())
	}
	m = press(m, " ")
	if !m.pl.chosen["z-top"] || !m.pl.chosen["z-base"] {
		t.Fatalf("chosen %v, notice %q", m.pl.chosen, m.notice)
	}
	if m = pluginAt(m, "z-base"); !strings.Contains(m.View(), adds) {
		t.Errorf("z-base, chosen for z-top, does not name the system group:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !slices.Equal(s.NodeGroups(), []string{"system", "gvisor"}) {
		t.Errorf("at %s, plugins %v, groups %v", m.m.Current().Title(), s.Plugins, s.NodeGroups())
	}
}
