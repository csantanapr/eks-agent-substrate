package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// hasField reports a Settings row for key.
func hasField(m Model, key string) bool {
	return slices.ContainsFunc(m.set.fields, func(f field) bool { return f.key == key })
}

// viewLine is the first line of the screen that holds text.
func viewLine(t *testing.T, m Model, text string) string {
	t.Helper()
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, text) {
			return l
		}
	}
	t.Fatalf("no line with %q:\n%s", text, m.View())
	return ""
}

// hasHint reports the nitro class's panel line (nitroSwitchHint) on screen
// v. The narrow panel wraps it, so this looks for its first words.
func hasHint(v string) bool { return strings.Contains(v, "Turning the class on") }

// repoConfig is the repository's config.env as the wizard reads it, with no
// config.local.env and no directory, so nothing is written.
func repoConfig(t *testing.T) state.Config {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "..", "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defaults, err := state.ParseConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return state.Config{Defaults: defaults}
}

// autoModeOff is Settings for the new cluster "fresh" with Auto Mode turned
// off and the sandbox classes of s.
func autoModeOff(t *testing.T, d Deps, s *state.Setup) Model {
	t.Helper()
	m := cursorTo(t, toSettings(t, d, s), "AUTO_MODE")
	m = press(m, "enter")
	if m.set.values["AUTO_MODE"] != "" || m.notice != "" {
		t.Fatalf("AUTO_MODE=%s, notice %q", m.set.values["AUTO_MODE"], m.notice)
	}
	return m
}

// planHourly is the cost an hour of step st on Review's plan.
func planHourly(t *testing.T, m Model, st state.Step) float64 {
	t.Helper()
	for _, p := range m.plan() {
		if p.step == st {
			return p.hourly
		}
	}
	t.Fatalf("no %s in the plan", st.Title())
	return 0
}

// Settings, on a new cluster: the default has no nitro row; the nitro class
// adds "Nitro sandbox on" (auto-mode); node-group adds the Nitro AMI row,
// which n refuses empty or malformed, the nitro node group row, and the
// notes; a Nitro Sandbox AMI lets the run go on with the nitro group, and
// Review prices it with the cluster, not as Auto Mode nodes.
func TestSettingsNitroOnTheNodeGroup(t *testing.T) {
	s := newSetup("fresh")
	s.DemoMode = true
	m := toSettings(t, testDeps(&recorder{}), s)
	if hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") || strings.Contains(m.View(), "Nitro sandbox on  ") || slices.Contains(groupRows(m), "nitro") {
		t.Fatalf("the default has a nitro row:\n%s", m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	if !hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") || slices.Contains(groupRows(m), "nitro") ||
		!strings.Contains(m.View(), "on Auto Mode nodes") {
		t.Fatalf("nitro on Auto Mode's nodes:\n%s", m.View())
	}
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	v := m.View()
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !hasField(m, "NITRO_AMI_ID") ||
		!slices.Equal(groupRows(m), []string{"system", "gvisor", "microvm", "nitro"}) || !strings.Contains(v, "Nitro Enclaves") ||
		!strings.Contains(v, "nitro node group") || !strings.Contains(v, "a Nitro Sandbox AMI is needed") {
		t.Fatalf("node-group: values %v, rows %v\n%s", m.set.values["NITRO_COMPUTE"], groupRows(m), v)
	}
	if !strings.Contains(v, "Now: NITRO_COMPUTE=node-") {
		t.Errorf("no panel value:\n%s", v)
	}
	m = press(m, "n")
	if m.m.Current() != state.Settings || !strings.HasPrefix(m.set.errs["NITRO_AMI_ID"], "a Nitro Sandbox AMI is needed") {
		t.Fatalf("n went on without a Nitro AMI: at %s, errs %v", m.m.Current().Title(), m.set.errs)
	}
	m = cursorTo(t, m, "NITRO_AMI_ID")
	m = setText(m, "ami-x")
	if !strings.Contains(m.set.errs["NITRO_AMI_ID"], "the ID of a Nitro Sandbox AMI") {
		t.Errorf("a malformed AMI: errs %v", m.set.errs)
	}
	m = setText(m, "ami-0a17000000000000e")
	if _, bad := m.set.errs["NITRO_AMI_ID"]; bad {
		t.Fatalf("errs %v", m.set.errs)
	}
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroNodeGroup || s.NewClusterConfig.NitroAMIID != "ami-0a17000000000000e" ||
		!slices.Equal(s.NodeGroups(), []string{"system", "gvisor", "microvm", "nitro"}) {
		t.Fatalf("at %s, %+v, groups %v", m.m.Current().Title(), s.NewClusterConfig, s.NodeGroups())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || !strings.Contains(s.NewClusterConfig.GroupsLine(s.NodeGroups()), " · nitro 1×m8i.xlarge") ||
		!strings.Contains(m.View(), "Node groups: system 2×m8i.xlarge") {
		t.Fatalf("at %s:\n%s", m.m.Current().Title(), m.View())
	}
	// The cluster: the default's $2.3764, plus a m8i.xlarge and its 100 GB.
	if usd := planHourly(t, m, state.CreateCluster); usd < 2.598 || usd > 2.600 {
		t.Errorf("cluster $%.4f an hour", usd)
	}
	if usd := planHourly(t, m, state.NitroSandbox); usd != 0 {
		t.Errorf("Nitro sandbox step $%.2f an hour, want none (the nodes are the nitro group's)", usd)
	}
	// m, the minimal profile, is Auto Mode's: auto-mode, no nitro group.
	m = press(m, "esc", "esc")
	if m.m.Current() != state.Settings {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	m = press(m, "m")
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || hasField(m, "NITRO_AMI_ID") || len(groupRows(m)) != 0 {
		t.Errorf("minimal: %v, rows %v", m.set.values["NITRO_COMPUTE"], groupRows(m))
	}
}

// Without Auto Mode, a new cluster shows where the nitro class would run,
// and the class is not refused: it runs on the nitro node group. Here the
// operator sets node-group first, so Settings has nothing to switch and
// shows no line about it. The class goes on with the system and nitro
// groups the rule requires, and the install runs the class's step, whose
// subtitle names the nitro node group. The Nitro AMI panel's build command
// is for the Kubernetes version Settings has.
func TestSettingsNitroWithoutAutoMode(t *testing.T) {
	r := &recorder{}
	s := newSetup("fresh")
	s.SandboxClasses = state.ClassGVisor
	m := toSettings(t, testDeps(r), s)
	m = cursorTo(t, m, "AUTO_MODE")
	m = press(m, "enter")
	if !hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") {
		t.Fatalf("Auto Mode off: no Nitro sandbox on row:\n%s", m.View())
	}
	if l := viewLine(t, m, "] AWS Nitro sandbox"); !strings.Contains(l, "[ ] AWS Nitro sandbox") || !strings.Contains(l, nitroGroupNote) {
		t.Errorf("Auto Mode off: the class row %q, want it open, on the nitro node group", l)
	}
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	// No nitro group without the class: no Nitro AMI row yet.
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || hasField(m, "NITRO_AMI_ID") || slices.Contains(groupRows(m), "nitro") {
		t.Fatalf("node-group, class off: rows %v, AMI row %v", groupRows(m), hasField(m, "NITRO_AMI_ID"))
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	if !m.set.classOn(state.ClassNitro) || !hasField(m, "NITRO_AMI_ID") || !slices.Equal(groupRows(m), []string{"system", "gvisor", "nitro"}) {
		t.Fatalf("node-group without Auto Mode: on %v, rows %v\n%s", m.set.classOn(state.ClassNitro), groupRows(m), m.View())
	}
	if strings.Contains(m.View(), nitroSwitchLine) || m.set.nitro.on {
		t.Errorf("the operator's node-group has Settings' line:\n%s", m.View())
	}
	m = cursorTo(t, m, "NITRO_AMI_ID")
	if v := m.View(); !strings.Contains(v, "k8s=1.37") || !strings.Contains(v, "amazon-eks-ami: make") {
		t.Errorf("the Nitro AMI panel for 1.37:\n%s", v)
	}
	m = cursorTo(t, m, "KUBERNETES_VERSION")
	m = setText(m, "1.38")
	m = cursorTo(t, m, "NITRO_AMI_ID")
	if v := m.View(); !strings.Contains(v, "k8s=1.38") || strings.Contains(v, "k8s=1.37") {
		t.Errorf("the Nitro AMI panel for 1.38:\n%s", v)
	}
	m = setText(m, "ami-0a17000000000000e")
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.AutoMode || !slices.Equal(s.NodeGroups(), []string{"system", "gvisor", "nitro"}) {
		t.Fatalf("at %s, Auto Mode %v, groups %v", m.m.Current().Title(), s.AutoMode, s.NodeGroups())
	}
	m = toStep(t, m, state.NitroSandbox)
	if v := m.View(); !strings.Contains(v, nitroSubtitle(plugin.NitroNodeGroup)[:60]) || strings.Contains(v, "NodePool ate-isolated") {
		t.Errorf("the step's subtitle:\n%s", v)
	}
	m = toStep(t, m, state.Complete)
	before(t, r.ran(), "create cluster", "nitro")
}

// The Set up Nitro sandboxes subtitle says where the class runs: Auto
// Mode's NodePool by default (empty or auto-mode, the text it always had),
// the nitro node group with node-group.
func TestNitroSubtitle(t *testing.T) {
	const autoMode = "The AWS Nitro sandbox class: NodePool ate-isolated on EKS Auto Mode, atelet-iso, and a counter pool on RuntimeClass isolated-sandbox, checked across a suspend and resume"
	if nitroSubtitle("") != autoMode || nitroSubtitle(plugin.NitroAutoMode) != autoMode {
		t.Errorf("auto-mode: %q", nitroSubtitle(""))
	}
	if g := nitroSubtitle(plugin.NitroNodeGroup); !strings.Contains(g, "on the nitro node group (ate.dev/pool=nitro)") || strings.Contains(g, "NodePool") {
		t.Errorf("node-group: %q", g)
	}
}

// A connected cluster has no NITRO_COMPUTE row (config.env decides). With
// NITRO_COMPUTE=node-group the class needs a node labeled
// ate.dev/pool=nitro, with or without Auto Mode. With auto-mode on a
// cluster without Auto Mode (team-partial), turning the class on sets
// node-group, as on a new cluster, with the line that says so: the class
// goes on with the cluster's nitro group, and without one it is refused
// for the missing group, NITRO_COMPUTE left as it was. With Auto Mode
// (new) auto-mode stays.
func TestConnectedClusterNitroNodeGroup(t *testing.T) {
	const noGroup = "team-partial has no nitro node group (no node labeled ate.dev/pool=nitro)"
	for _, c := range []struct {
		cluster, pools, compute string
		on                      bool
		notice                  string
		want                    string // NITRO_COMPUTE after
		line                    bool   // Settings' line under the class
	}{
		{"team-partial", "system,gvisor,nitro", plugin.NitroNodeGroup, true, "", plugin.NitroNodeGroup, false},
		{"new", "system,gvisor,nitro", plugin.NitroNodeGroup, true, "", plugin.NitroNodeGroup, false},
		{"team-partial", "system,gvisor", plugin.NitroNodeGroup, false, noGroup, plugin.NitroNodeGroup, false},
		{"team-partial", "system,gvisor,nitro", plugin.NitroAutoMode, true, "", plugin.NitroNodeGroup, true},
		// No Auto Mode and no nitro group: the press is refused for the
		// missing group, and a refused press changes nothing, so
		// NITRO_COMPUTE keeps the value the operator had.
		{"team-partial", "system,gvisor", plugin.NitroAutoMode, false, noGroup, plugin.NitroAutoMode, false},
		{"new", "system,gvisor", plugin.NitroAutoMode, true, "", plugin.NitroAutoMode, false},
	} {
		r := &recorder{probes: map[string]string{c.cluster: "SUBSTRATE_EKS_INSTALLED false pools=" + c.pools}}
		s := newSetup(c.cluster)
		s.SandboxClasses = state.ClassGVisor
		s.NewClusterConfig.NitroCompute = c.compute
		m := toStep(t, New(testDeps(r), s), state.Settings)
		if hasField(m, "NITRO_COMPUTE") || hasField(m, "NITRO_AMI_ID") {
			t.Errorf("%+v: a connected cluster has the nitro rows", c)
		}
		if !c.on && c.compute == plugin.NitroAutoMode {
			// Refused before the press too, for the group, not Auto Mode.
			if l := viewLine(t, m, "] AWS Nitro sandbox"); !strings.Contains(l, "[–] AWS Nitro sandbox") || !strings.Contains(l, "no nitro node group") {
				t.Errorf("%+v: the class row %q", c, l)
			}
		}
		// The panel says what turning the class on does only when it
		// switches and is not refused.
		if m = cursorTo(t, m, state.ClassNitro); hasHint(m.View()) != c.line {
			t.Errorf("%+v: hint %v:\n%s", c, !c.line, m.View())
		}
		m = press(m, "enter")
		if m.set.classOn(state.ClassNitro) != c.on || !strings.Contains(m.notice, c.notice) || (c.notice == "" && m.notice != "") {
			t.Errorf("%+v: on %v, notice %q", c, m.set.classOn(state.ClassNitro), m.notice)
		}
		if got := m.set.values["NITRO_COMPUTE"]; got != c.want || strings.Contains(m.View(), nitroSwitchLine) != c.line {
			t.Errorf("%+v: NITRO_COMPUTE=%s, line %v:\n%s", c, got, !c.line, m.View())
		}
		if c.on {
			if m = press(m, "n"); m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != c.want {
				t.Errorf("%+v: at %s, errs %v, NITRO_COMPUTE=%s", c, m.m.Current().Title(), m.set.errs, s.NewClusterConfig.NitroCompute)
			}
		}
	}
}

// Without Auto Mode, turning the AWS Nitro sandbox on sets
// NITRO_COMPUTE=node-group, so the class goes on: the Nitro AMI row and the
// nitro group come with it, and a line under the class says what changed
// and why. Turning the class off again puts auto-mode back, and the rows
// and the line go, with a notice. Both ways, twice. While the class is
// off, its panel says what turning it on does; the gVisor panel does not.
func TestSettingsSwitchesNitroComputeWithTheClass(t *testing.T) {
	s := newSetup("fresh")
	s.SandboxClasses = state.ClassGVisor
	m := cursorTo(t, autoModeOff(t, testDeps(&recorder{}), s), state.ClassGVisor)
	if hasHint(m.View()) {
		t.Errorf("the gVisor panel has the nitro class's hint:\n%s", m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	if v := m.View(); strings.Contains(v, "Not available") || m.set.nitro.on {
		t.Fatalf("the class is refused before the press:\n%s", v)
	}
	for range 2 {
		if v := m.View(); !hasHint(v) {
			t.Errorf("off: the panel does not say what turning it on does:\n%s", v)
		}
		m = press(m, "enter")
		v := m.View()
		if !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || m.notice != "" {
			t.Fatalf("on: classes %q, NITRO_COMPUTE=%s, notice %q", m.set.values[classKey], m.set.values["NITRO_COMPUTE"], m.notice)
		}
		if !hasField(m, "NITRO_AMI_ID") || !slices.Equal(groupRows(m), []string{"system", "gvisor", "nitro"}) {
			t.Errorf("on: AMI row %v, rows %v", hasField(m, "NITRO_AMI_ID"), groupRows(m))
		}
		if l := viewLine(t, m, "] AWS Nitro sandbox"); !strings.Contains(l, nitroGroupNote) || !strings.Contains(v, nitroSwitchLine) ||
			strings.Contains(v, "Not available") || hasHint(v) {
			t.Errorf("on: no line, or a refusal or the hint:\n%s", v)
		}
		m = press(m, "enter")
		v = m.View()
		if m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.notice != "Back to NITRO_COMPUTE=auto-mode" {
			t.Fatalf("off: classes %q, NITRO_COMPUTE=%s, notice %q", m.set.values[classKey], m.set.values["NITRO_COMPUTE"], m.notice)
		}
		if hasField(m, "NITRO_AMI_ID") || slices.Contains(groupRows(m), "nitro") || strings.Contains(v, nitroSwitchLine) {
			t.Errorf("off: AMI row %v, rows %v:\n%s", hasField(m, "NITRO_AMI_ID"), groupRows(m), v)
		}
	}
	// The operator's node-group: turning the class on changes nothing, so
	// the panel does not say it would.
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	if m = cursorTo(t, m, state.ClassNitro); m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || hasHint(m.View()) {
		t.Errorf("node-group picked: NITRO_COMPUTE=%s, the hint shown:\n%s", m.set.values["NITRO_COMPUTE"], m.View())
	}
	// Empty means auto-mode, comes back empty, and the notice says auto-mode.
	s = newSetup("fresh")
	s.NewClusterConfig.NitroCompute = ""
	m = cursorTo(t, autoModeOff(t, testDeps(&recorder{}), s), state.ClassNitro)
	m = press(m, "enter")
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup {
		t.Fatalf("empty: NITRO_COMPUTE=%s", m.set.values["NITRO_COMPUTE"])
	}
	if m = press(m, "enter"); m.set.values["NITRO_COMPUTE"] != "" || m.notice != "Back to NITRO_COMPUTE=auto-mode" {
		t.Errorf("empty: back to %q, notice %q", m.set.values["NITRO_COMPUTE"], m.notice)
	}
}

// Turning Auto Mode off under the AWS Nitro sandbox on auto-mode sets
// node-group the same way, and turning it on again puts auto-mode back.
// With Auto Mode on nothing is switched: the class goes on and off on
// auto-mode, with no line and no nitro group.
func TestSettingsSwitchesNitroComputeWithAutoMode(t *testing.T) {
	s := newSetup("fresh")
	m := cursorTo(t, toSettings(t, testDeps(&recorder{}), s), state.ClassNitro)
	if l := viewLine(t, m, "] AWS Nitro sandbox"); !strings.Contains(l, "[ ] AWS Nitro sandbox") || !strings.Contains(l, "on Auto Mode nodes") ||
		hasHint(m.View()) {
		t.Errorf("Auto Mode on, the default: the class row %q, or the hint:\n%s", l, m.View())
	}
	m = press(m, "enter", "enter", "enter")
	if !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.set.nitro.on || m.notice != "" ||
		hasField(m, "NITRO_AMI_ID") || strings.Contains(m.View(), nitroSwitchLine) {
		t.Fatalf("Auto Mode on: NITRO_COMPUTE=%s, notice %q:\n%s", m.set.values["NITRO_COMPUTE"], m.notice, m.View())
	}
	m = cursorTo(t, m, "AUTO_MODE")
	for range 2 {
		m = press(m, "enter")
		if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !hasField(m, "NITRO_AMI_ID") ||
			!slices.Equal(groupRows(m), []string{"system", "gvisor", "microvm", "nitro"}) || !strings.Contains(m.View(), nitroSwitchLine) {
			t.Fatalf("Auto Mode off: NITRO_COMPUTE=%s, rows %v:\n%s", m.set.values["NITRO_COMPUTE"], groupRows(m), m.View())
		}
		// n stops for the Nitro AMI only: the class is not refused.
		if m = press(m, "n"); m.m.Current() != state.Settings || m.set.errs["NITRO_AMI_ID"] == "" || m.set.errs[classKey] != "" {
			t.Errorf("Auto Mode off: at %s, errs %v", m.m.Current().Title(), m.set.errs)
		}
		m = press(m, "enter")
		if m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || hasField(m, "NITRO_AMI_ID") || slices.Contains(groupRows(m), "nitro") ||
			strings.Contains(m.View(), nitroSwitchLine) || m.notice != "Back to NITRO_COMPUTE=auto-mode" {
			t.Fatalf("Auto Mode on again: NITRO_COMPUTE=%s, rows %v, notice %q", m.set.values["NITRO_COMPUTE"], groupRows(m), m.notice)
		}
	}
	if m = press(m, "n"); m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroAutoMode || !s.AutoMode {
		t.Errorf("at %s, NITRO_COMPUTE=%s, Auto Mode %v", m.m.Current().Title(), s.NewClusterConfig.NitroCompute, s.AutoMode)
	}
}

// A NITRO_COMPUTE the operator picks is theirs, and Settings never changes
// it back: node-group picked before the class stays when the class goes
// off; picked over Settings' switch, it stays when Auto Mode comes on. The
// m key sets its own, and leaves no switch behind.
func TestSettingsKeepsTheOperatorsNitroCompute(t *testing.T) {
	m := autoModeOff(t, testDeps(&recorder{}), newSetup("fresh"))
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	if !m.set.classOn(state.ClassNitro) || m.set.nitro.on || strings.Contains(m.View(), nitroSwitchLine) {
		t.Fatalf("picked first: on %v, switch %+v", m.set.classOn(state.ClassNitro), m.set.nitro)
	}
	if m = press(m, "enter"); m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || m.notice != "" {
		t.Errorf("picked first, class off: NITRO_COMPUTE=%s, notice %q", m.set.values["NITRO_COMPUTE"], m.notice)
	}

	m = autoModeOff(t, testDeps(&recorder{}), newSetup("fresh"))
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter")
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter", "enter") // auto-mode, then node-group: the operator's
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || m.set.nitro.on || strings.Contains(m.View(), nitroSwitchLine) {
		t.Fatalf("picked over the switch: NITRO_COMPUTE=%s, switch %+v", m.set.values["NITRO_COMPUTE"], m.set.nitro)
	}
	m = cursorTo(t, m, "AUTO_MODE")
	if m = press(m, "enter"); m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || m.notice != "" {
		t.Errorf("picked over the switch, Auto Mode on: NITRO_COMPUTE=%s, notice %q", m.set.values["NITRO_COMPUTE"], m.notice)
	}

	m = autoModeOff(t, testDeps(&recorder{}), newSetup("fresh"))
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter", "m")
	if v := m.set.values; v["NITRO_COMPUTE"] != plugin.NitroAutoMode || v["AUTO_MODE"] != "on" || m.set.nitro.on || strings.Contains(m.View(), nitroSwitchLine) {
		t.Errorf("m after the switch: NITRO_COMPUTE=%s, AUTO_MODE=%s, switch %+v", v["NITRO_COMPUTE"], v["AUTO_MODE"], m.set.nitro)
	}
}

// Settings' switch goes with its values: after n and back it still holds,
// and turning the class off still puts auto-mode back; esc without n drops
// it with the rest. Connecting a cluster with Auto Mode instead puts
// auto-mode back when Settings opens.
func TestSettingsNitroSwitchAcrossVisits(t *testing.T) {
	s := newSetup("fresh")
	s.SandboxClasses = state.ClassGVisor
	m := cursorTo(t, autoModeOff(t, testDeps(&recorder{}), s), state.ClassNitro)
	m = press(m, "enter")
	m = cursorTo(t, m, "NITRO_AMI_ID")
	m = setText(m, "ami-0a17000000000000e")
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroNodeGroup || s.AutoMode {
		t.Fatalf("at %s, NITRO_COMPUTE=%s, Auto Mode %v", m.m.Current().Title(), s.NewClusterConfig.NitroCompute, s.AutoMode)
	}
	if m = press(m, "esc"); m.m.Current() != state.Settings || !strings.Contains(m.View(), nitroSwitchLine) || m.notice != "" {
		t.Fatalf("back at %s, notice %q:\n%s", m.m.Current().Title(), m.notice, m.View())
	}
	m = cursorTo(t, m, state.ClassNitro)
	if m = press(m, "enter"); m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.notice != "Back to NITRO_COMPUTE=auto-mode" {
		t.Fatalf("class off on the second visit: NITRO_COMPUTE=%s, notice %q", m.set.values["NITRO_COMPUTE"], m.notice)
	}
	// esc drops the visit's answers: the Setup still has the class and
	// Settings' node-group, so the switch is back with them.
	m = press(m, "esc", "enter")
	if m.m.Current() != state.Settings || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !m.set.classOn(state.ClassNitro) || !m.set.nitro.on {
		t.Fatalf("after esc: at %s, NITRO_COMPUTE=%s, switch %+v", m.m.Current().Title(), m.set.values["NITRO_COMPUTE"], m.set.nitro)
	}

	// A switch made and left with esc is not the Setup's.
	s = newSetup("fresh")
	s.SandboxClasses = state.ClassGVisor
	m = cursorTo(t, autoModeOff(t, testDeps(&recorder{}), s), state.ClassNitro)
	m = press(m, "enter", "esc", "enter")
	if m.m.Current() != state.Settings || m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode ||
		m.set.nitro.on || m.notice != "" || strings.Contains(m.View(), nitroSwitchLine) {
		t.Fatalf("esc without n: at %s, NITRO_COMPUTE=%s, switch %+v, notice %q", m.m.Current().Title(), m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice)
	}

	// Back to the Cluster screen, and on to new, which has Auto Mode.
	s = newSetup("fresh")
	s.SandboxClasses = state.ClassGVisor
	m = cursorTo(t, autoModeOff(t, testDeps(&recorder{}), s), state.ClassNitro)
	m = press(m, "enter")
	m = cursorTo(t, m, "NITRO_AMI_ID")
	m = setText(m, "ami-0a17000000000000e")
	m = press(m, "n", "esc", "esc", "esc")
	if m.m.Current() != state.Cluster {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	m = press(m, "up", "up", "up", "up", "enter", "enter")
	if m.m.Current() != state.Settings || m.s.ClusterName != "new" || !m.s.AutoMode {
		t.Fatalf("at %s on %s (Auto Mode %v)", m.m.Current().Title(), m.s.ClusterName, m.s.AutoMode)
	}
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.set.nitro.on || m.notice != "Back to NITRO_COMPUTE=auto-mode" || len(m.set.errs) != 0 {
		t.Errorf("new: NITRO_COMPUTE=%s, switch %+v, notice %q, errs %v", m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice, m.set.errs)
	}
}

// A run that used the switch: Review has the nitro group with the cluster,
// priced there, and the Set up Nitro sandboxes step, and config.local.env
// records node-group with Auto Mode off. A run that undid it has neither
// and writes no NITRO_COMPUTE. The NITRO_COMPUTE panel shows config.env's
// help, which says Settings does it.
func TestSettingsNitroSwitchReachesConfigLocalEnv(t *testing.T) {
	const says = "With Auto Mode off and the class on, the wizard's Settings sets node-group for you. It puts the old value back when you change either. A value you pick yourself stays."
	if doc := joinParagraphs(repoConfig(t).Defaults.Doc("NITRO_COMPUTE")); !strings.Contains(doc, says) {
		t.Errorf("config.env's NITRO_COMPUTE help: %s", doc)
	}
	for _, undo := range []bool{false, true} {
		d := testDeps(&recorder{})
		d.Config = repoConfig(t)
		var written string
		d.SaveConfig = func(s *state.Setup) (string, error) {
			text, err := d.Config.RenderLocal(state.ConfigValues(s))
			written = text
			return "/x/config.local.env", err
		}
		s := newSetup("fresh")
		m := cursorTo(t, autoModeOff(t, d, s), state.ClassNitro)
		m = press(m, "enter")
		m = cursorTo(t, m, "NITRO_COMPUTE")
		if v := m.View(); !strings.Contains(v, "Where the nitro sandbox") || strings.Contains(v, "Where the AWS Nitro") {
			t.Errorf("the panel is not config.env's help:\n%s", v)
		}
		m = cursorTo(t, m, "NITRO_AMI_ID")
		m = setText(m, "ami-0a17000000000000e")
		if undo {
			m = cursorTo(t, m, state.ClassNitro)
			m = press(m, "enter")
		}
		m = press(m, "n", "enter")
		if m.m.Current() != state.Review {
			t.Fatalf("undo=%v: at %s, errs %v", undo, m.m.Current().Title(), m.set.errs)
		}
		v := m.View()
		nitro := strings.Contains(s.NewClusterConfig.GroupsLine(s.NodeGroups()), " · nitro 1×m8i.xlarge")
		if nitro == undo || slices.Contains(m.m.Order(), state.NitroSandbox) == undo || !strings.Contains(v, "Auto Mode off") ||
			strings.Contains(v, "classes gvisor,microvm,nitro") == undo {
			t.Errorf("undo=%v: groups %v:\n%s", undo, s.NodeGroups(), v)
		}
		// The default's three groups at $2.3764, plus the nitro group's
		// m8i.xlarge and its 100 GB when the run has it.
		if usd := planHourly(t, m, state.CreateCluster); (usd > 2.598 && usd < 2.600) == undo {
			t.Errorf("undo=%v: cluster $%.4f an hour", undo, usd)
		}
		m = press(m, "y")
		if written == "" {
			t.Fatalf("undo=%v: nothing written: %q", undo, m.notice)
		}
		for _, want := range []string{"AUTO_MODE=\"\"\n", "NITRO_AMI_ID=ami-0a17000000000000e\n"} {
			if !strings.Contains(written, want) {
				t.Errorf("undo=%v: config.local.env lacks %q:\n%s", undo, want, written)
			}
		}
		if got := strings.Contains(written, "NITRO_COMPUTE=node-group\n") && strings.Contains(written, "SANDBOX_CLASSES=gvisor,microvm,nitro\n"); got == undo {
			t.Errorf("undo=%v: config.local.env:\n%s", undo, written)
		}
		if undo && (strings.Contains(written, "NITRO_COMPUTE=") || strings.Contains(written, "SANDBOX_CLASSES=")) {
			t.Errorf("undo: config.local.env keeps a value the operator did not pick:\n%s", written)
		}
	}
}

// Settings opens with the AWS Nitro sandbox already on with auto-mode, on
// a cluster without Auto Mode: it sets node-group, as when the class is
// turned on. Here the operator connected new (Auto Mode), turned the class
// on, and went back to connect team-partial. With a nitro group the class
// goes on and n moves on. Without one the class shows the missing group,
// and turning it off puts auto-mode back.
func TestSettingsSwitchesNitroComputeOnEntry(t *testing.T) {
	for _, group := range []bool{true, false} {
		pools := "system,gvisor"
		if group {
			pools += ",nitro"
		}
		r := &recorder{probes: map[string]string{
			"new":          "SUBSTRATE_EKS_INSTALLED false pools=system,gvisor",
			"team-partial": "SUBSTRATE_EKS_INSTALLED false pools=" + pools,
		}}
		s := newSetup("new")
		s.SandboxClasses = state.ClassGVisor
		m := cursorTo(t, toStep(t, New(testDeps(r), s), state.Settings), state.ClassNitro)
		m = press(m, "enter", "n")
		if m.m.Current() != state.Plugins || s.SandboxClasses != "gvisor,nitro" || s.NewClusterConfig.NitroCompute != plugin.NitroAutoMode {
			t.Fatalf("group=%v: new: at %s, classes %q, NITRO_COMPUTE=%s", group, m.m.Current().Title(), s.SandboxClasses, s.NewClusterConfig.NitroCompute)
		}
		if m = press(m, "esc", "esc", "esc"); m.m.Current() != state.Cluster {
			t.Fatalf("group=%v: at %s", group, m.m.Current().Title())
		}
		m = press(m, "down", "down", "down", "enter", "enter")
		if m.m.Current() != state.Settings || m.s.ClusterName != "team-partial" || m.s.AutoMode {
			t.Fatalf("group=%v: at %s on %s (Auto Mode %v)", group, m.m.Current().Title(), m.s.ClusterName, m.s.AutoMode)
		}
		v := m.View()
		if !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !m.set.nitro.on ||
			!strings.Contains(v, nitroSwitchLine) || m.notice != "" {
			t.Fatalf("group=%v: classes %q, NITRO_COMPUTE=%s, switch %+v, notice %q:\n%s", group, m.set.values[classKey], m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice, v)
		}
		l := viewLine(t, m, "] AWS Nitro sandbox")
		if group {
			if !strings.Contains(l, nitroGroupNote) || strings.Contains(l, "needs EKS Auto Mode") || len(m.set.errs) != 0 {
				t.Errorf("group: the class row %q, errs %v", l, m.set.errs)
			}
			if m = press(m, "n"); m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroNodeGroup {
				t.Errorf("group: at %s, NITRO_COMPUTE=%s, errs %v", m.m.Current().Title(), s.NewClusterConfig.NitroCompute, m.set.errs)
			}
			continue
		}
		if !strings.Contains(l, "no nitro node group") || m.set.errs[classKey] != "AWS Nitro sandbox: no nitro node group" {
			t.Errorf("no group: the class row %q, errs %v", l, m.set.errs)
		}
		if m = press(m, "n"); m.m.Current() != state.Settings {
			t.Fatalf("no group: n went on to %s", m.m.Current().Title())
		}
		m = cursorTo(t, m, state.ClassNitro)
		m = press(m, "enter")
		if m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.set.nitro.on ||
			m.notice != "Back to NITRO_COMPUTE=auto-mode" || strings.Contains(m.View(), nitroSwitchLine) {
			t.Fatalf("no group, class off: NITRO_COMPUTE=%s, switch %+v, notice %q", m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice)
		}
		if m = press(m, "n"); m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroAutoMode || s.SandboxClasses != state.ClassGVisor {
			t.Errorf("no group: at %s, classes %q, NITRO_COMPUTE=%s", m.m.Current().Title(), s.SandboxClasses, s.NewClusterConfig.NitroCompute)
		}
	}
}

// A new cluster whose Setup has the AWS Nitro sandbox on and Auto Mode off
// (config.local.env from an earlier run, or --sandbox-classes) gets
// node-group when Settings opens, with the line and the Nitro AMI row.
// After that only the nitro class and Auto Mode switch it: with auto-mode
// picked again under the class, n is refused, turning gVisor off and on
// keeps auto-mode, and turning the nitro class off and on switches again.
func TestSettingsNitroSwitchFollowsOnlyTheClassAndAutoMode(t *testing.T) {
	s := newSetup("fresh")
	s.SandboxClasses = "gvisor,microvm,nitro"
	s.NewClusterConfig.AutoMode = ""
	m := toSettings(t, testDeps(&recorder{}), s)
	if v := m.View(); !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !m.set.nitro.on ||
		!strings.Contains(v, nitroSwitchLine) || !hasField(m, "NITRO_AMI_ID") || m.notice != "" || m.set.errs[classKey] != "" || m.set.errs["NITRO_AMI_ID"] == "" {
		t.Fatalf("entry: NITRO_COMPUTE=%s, switch %+v, notice %q, errs %v:\n%s", m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice, m.set.errs, v)
	}
	const refused = "AWS Nitro sandbox: needs EKS Auto Mode"
	m = cursorTo(t, m, "NITRO_COMPUTE")
	m = press(m, "enter")
	if m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.set.nitro.on || m.set.errs[classKey] != refused {
		t.Fatalf("auto-mode picked: NITRO_COMPUTE=%s, switch %+v, errs %v", m.set.values["NITRO_COMPUTE"], m.set.nitro, m.set.errs)
	}
	m = cursorTo(t, m, state.ClassGVisor)
	for _, on := range []bool{false, true} {
		m = press(m, "enter")
		if m.set.classOn(state.ClassGVisor) != on || m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.set.nitro.on || m.set.errs[classKey] != refused {
			t.Fatalf("gVisor on=%v: classes %q, NITRO_COMPUTE=%s, switch %+v, errs %v", on, m.set.values[classKey], m.set.values["NITRO_COMPUTE"], m.set.nitro, m.set.errs)
		}
	}
	if m = press(m, "n"); m.m.Current() != state.Settings {
		t.Fatalf("n went on with auto-mode and Auto Mode off: at %s", m.m.Current().Title())
	}
	m = cursorTo(t, m, state.ClassNitro)
	m = press(m, "enter", "enter")
	if !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !m.set.nitro.on || m.set.errs[classKey] != "" {
		t.Errorf("nitro off and on: NITRO_COMPUTE=%s, switch %+v, errs %v", m.set.values["NITRO_COMPUTE"], m.set.nitro, m.set.errs)
	}
}

// A switch made on a connected cluster without Auto Mode is kept with n,
// and the next visit still has it. Going back to create a new cluster,
// whose AUTO_MODE is on, puts auto-mode back when Settings opens.
func TestSettingsNitroSwitchFromAConnectedCluster(t *testing.T) {
	r := &recorder{probes: map[string]string{"team-partial": "SUBSTRATE_EKS_INSTALLED false pools=system,gvisor,nitro"}}
	s := newSetup("team-partial")
	s.SandboxClasses = state.ClassGVisor
	m := cursorTo(t, toStep(t, New(testDeps(r), s), state.Settings), state.ClassNitro)
	m = press(m, "enter", "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroNodeGroup || s.SandboxClasses != "gvisor,nitro" {
		t.Fatalf("at %s, classes %q, NITRO_COMPUTE=%s", m.m.Current().Title(), s.SandboxClasses, s.NewClusterConfig.NitroCompute)
	}
	if m = press(m, "esc"); m.m.Current() != state.Settings || m.set.values["NITRO_COMPUTE"] != plugin.NitroNodeGroup || !m.set.nitro.on ||
		!strings.Contains(m.View(), nitroSwitchLine) || m.notice != "" {
		t.Fatalf("back at %s: NITRO_COMPUTE=%s, switch %+v, notice %q", m.m.Current().Title(), m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice)
	}
	if m = press(m, "esc", "esc"); m.m.Current() != state.Cluster {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	m = press(m, "G", "enter")
	m = typeIn(m, "fresh")
	m = press(m, "enter", "enter")
	if m.m.Current() != state.Settings || !m.s.ClusterIsNew || m.s.ClusterName != "fresh" || m.set.values["AUTO_MODE"] != "on" {
		t.Fatalf("at %s on %s (new %v, AUTO_MODE=%s)", m.m.Current().Title(), m.s.ClusterName, m.s.ClusterIsNew, m.set.values["AUTO_MODE"])
	}
	if v := m.View(); !m.set.classOn(state.ClassNitro) || m.set.values["NITRO_COMPUTE"] != plugin.NitroAutoMode || m.set.nitro.on ||
		m.notice != "Back to NITRO_COMPUTE=auto-mode" || strings.Contains(v, nitroSwitchLine) || hasField(m, "NITRO_AMI_ID") || len(m.set.errs) != 0 {
		t.Fatalf("fresh: NITRO_COMPUTE=%s, switch %+v, notice %q, errs %v:\n%s", m.set.values["NITRO_COMPUTE"], m.set.nitro, m.notice, m.set.errs, v)
	}
	if m = press(m, "n"); m.m.Current() != state.Plugins || s.NewClusterConfig.NitroCompute != plugin.NitroAutoMode {
		t.Errorf("fresh: at %s, NITRO_COMPUTE=%s", m.m.Current().Title(), s.NewClusterConfig.NitroCompute)
	}
}
