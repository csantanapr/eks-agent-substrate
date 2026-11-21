package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
)

// Plugins for these tests: a default one on the system node group, one that
// another requires, mode3-iso, which needs EKS Auto Mode and is the AWS
// Nitro sandbox class's, and t-kata, which adds the microvm node group as
// mode 3 does.
func init() {
	for _, p := range []plugin.Plugin{
		{ID: "t-gateway", Title: "LLM gateway", Summary: "A gateway to Amazon Bedrock.", Order: 10, Minutes: 3, Default: true, Nodes: []string{"system"}},
		{ID: "t-kata", Title: "Kata pool", Summary: "Workers in Kata on the microvm nodes.", Order: 60, Minutes: 5, Nodes: []string{"microvm"}, AddsNodes: true},
		{ID: "t-claw", Title: "Claw agent", Summary: "Workers on the gvisor nodes.", Order: 70, Minutes: 5, Nodes: []string{"gvisor"}},
		{ID: "t-efs", Title: "EFS volumes", Summary: "Amazon EFS volumes per actor.", Order: 20, Minutes: 6, HourlyUSD: 0.04, SetupAWS: true,
			Check: func(lookup func(string) (string, bool)) error {
				if v, _ := lookup("T_EFS_IMAGE"); v != "" && !strings.Contains(v, "@sha256:") {
					return errors.New("set T_EFS_IMAGE to <image>@sha256:<digest>")
				}
				return nil
			}},
		{ID: "t-s3files", Title: "S3 Files volumes", Summary: "S3 Files volumes per actor.", Order: 30, Minutes: 7, HourlyUSD: 0.05, Requires: []string{"t-efs"}, SetupAWS: true},
		{ID: "mode3-iso", Title: "AWS Nitro sandbox", Summary: "Worker Pods in Nitro sandboxes.", Order: 40, Minutes: 4, HourlyUSD: 0.12, Class: "nitro",
			NeedsAutoMode: "needs EKS Auto Mode: its isolated-sandbox RuntimeClass and NodePool come from Auto Mode", FollowsNitroCompute: true},
		{ID: "t-ondemo", Title: "Counter volumes", Summary: "Counter templates with a volume.", Order: 50, Minutes: 2, NeedsDemo: true},
		{ID: "t-rot", Title: "Login rotation", Summary: "Rotates the RDS logins.", Order: 80, Minutes: 8, Nodes: []string{"system"},
			NeedsRDS: "rotates the RDS logins, so it needs the RDS database"},
		// z-base runs on the system group and builds on the counter demo,
		// and z-top requires it: EFS and S3 Files.
		{ID: "z-base", Title: "Z base", Summary: "On the system nodes.", Order: 90, Minutes: 1, Nodes: []string{"system"}, NeedsDemo: true},
		{ID: "z-top", Title: "Z top", Summary: "Built on Z base.", Order: 91, Minutes: 1, Requires: []string{"z-base"}},
	} {
		plugin.Register(p)
	}
}

var at = time.Date(2026, 11, 13, 21, 41, 0, 0, time.FixedZone("EST", -5*3600))

func fixedNow() time.Time { return at }

// sized is m after a 126x33 terminal, the screenshot size.
func sized(m Model) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: 126, Height: 33})
	return next.(Model)
}

// toSettings creates cluster "fresh" with the RDS database.
func toSettings(t *testing.T, d Deps, s *state.Setup) Model {
	t.Helper()
	m := toStep(t, New(d, s), state.Cluster)
	m = press(m, "enter", "enter", "enter")
	if m.m.Current() != state.Settings {
		t.Fatalf("at %s, want Settings", m.m.Current().Title())
	}
	return m
}

// cursorTo moves the Settings cursor to key.
func cursorTo(t *testing.T, m Model, key string) Model {
	t.Helper()
	for range len(m.set.fields) {
		if m.set.fields[m.set.cursor].key == key {
			return m
		}
		m = press(m, "down")
	}
	t.Fatalf("no field %s", key)
	return m
}

func setText(m Model, value string) Model {
	m = press(m, "enter")
	for range m.set.input {
		m = press(m, "backspace")
	}
	m = typeIn(m, value)
	return press(m, "enter")
}

func TestSplashPlaysAndAnyKeySkipsIt(t *testing.T) {
	d := testDeps(&recorder{})
	d.Splash = true
	m := sized(New(d, newSetup("new")))
	if v := m.View(); !strings.Contains(v, "Agent Substrate 1.0.0") || !strings.Contains(v, "press any key") || strings.Contains(v, "Check your setup") {
		t.Fatalf("splash:\n%s", v)
	}
	m = press(m, "x")
	if m.splash || m.m.Current() != state.Welcome || !strings.Contains(m.View(), "Check your setup") {
		t.Fatalf("a key did not skip to Welcome:\n%s", m.View())
	}
	// Unskipped, it ends on its own after its frames, and a late tick is
	// ignored.
	m = sized(New(d, newSetup("new")))
	for range splashFrames {
		next, _ := m.Update(splashMsg{})
		m = next.(Model)
	}
	if m.splash {
		t.Error("the splash did not end")
	}
	if next, cmd := m.Update(splashMsg{}); next.(Model).splash || cmd != nil {
		t.Error("a tick after the splash restarted it")
	}
	m = sized(New(d, newSetup("new")))
	if m = press(m, "ctrl+c"); !m.quitting {
		t.Error("ctrl+c on the splash did not quit")
	}
}

// The header names what the run acts on, on every screen, with the account
// masked once it is known, and the clock.
func TestHeaderOnEveryScreen(t *testing.T) {
	d := testDeps(&recorder{})
	d.Now = fixedNow
	s := newSetup("new")
	s.Profile = "eks-sandbox"
	frames, withAccount := 0, 0
	views = func(v string) {
		frames++
		head := strings.SplitN(v, "\n", 2)[0]
		if !strings.Contains(head, "AGENT SUBSTRATE") || !strings.Contains(head, "us-west-2 · eks-sandbox") || !strings.Contains(head, "9:41 PM EST") {
			t.Errorf("header %q", head)
		}
		if strings.Contains(head, "••••••••3333") {
			withAccount++
		}
	}
	defer func() { views = nil }()
	m := New(d, s)
	if head := strings.SplitN(m.View(), "\n", 2)[0]; strings.Contains(head, "3333") {
		t.Errorf("account shown before it is read: %q", head)
	}
	toStep(t, m, state.Complete)
	if frames < 10 || withAccount == 0 {
		t.Errorf("%d frames, %d with the account", frames, withAccount)
	}
	s = newSetup("")
	if head := strings.SplitN(New(d, s).View(), "\n", 2)[0]; !strings.Contains(head, "no cluster yet") || !strings.Contains(head, "default credentials") {
		t.Errorf("header without a cluster or profile: %q", head)
	}
}

// Settings edits the new cluster's knobs; a bad value is shown and stops n,
// and the answers reach the Setup, its version and Auto Mode with them
// (review F13: ClusterVersion follows the knobs).
func TestSettingsEditsTheNewClusterAndValidates(t *testing.T) {
	s := newSetup("fresh")
	m := toSettings(t, testDeps(&recorder{}), s)
	if v := m.View(); !strings.Contains(v, "Node groups") || !strings.Contains(v, "m8i.2xlarge") || !strings.Contains(v, "nested virt") {
		t.Fatalf("no cluster knobs:\n%s", v)
	}
	m = setText(m, "1.36")
	if !strings.Contains(m.View(), "an EKS version 1.37 or newer") {
		t.Errorf("no finding shown:\n%s", m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Settings || !strings.Contains(m.View(), "need a fix") {
		t.Fatalf("n went past a bad value to %s", m.m.Current().Title())
	}
	m = setText(m, "1.38")
	m = cursorTo(t, m, "AUTO_MODE")
	m = press(m, "enter")
	m = cursorTo(t, m, "TAGS")
	m = setText(m, "team=a auto-delete=no")
	m = press(m, "n")
	if m.m.Current() != state.Plugins {
		t.Fatalf("at %s after n", m.m.Current().Title())
	}
	if c := s.NewClusterConfig; c.KubernetesVersion != "1.38" || c.AutoMode != "" || s.ClusterVersion != "1.38" || s.AutoMode || s.Tags != "team=a auto-delete=no" {
		t.Errorf("setup %+v", s)
	}
	// Esc goes back with the answers kept.
	m = press(m, "esc")
	if m.m.Current() != state.Settings || m.set.values["KUBERNETES_VERSION"] != "1.38" {
		t.Errorf("back at %s with %v", m.m.Current().Title(), m.set.values["KUBERNETES_VERSION"])
	}
}

func TestSettingsForAConnectedClusterHasNoClusterKnobs(t *testing.T) {
	m := toStep(t, New(testDeps(&recorder{}), newSetup("new")), state.Settings)
	v := m.View()
	if strings.Contains(v, "Node groups") || strings.Contains(v, "Node AMI") || !strings.Contains(v, "connected, not created") || !strings.Contains(v, "EKS 1.37") {
		t.Errorf("connected cluster settings:\n%s", v)
	}
}

// A new cluster's node AMI: EKS's by default; a bad ID is refused and stops
// n; a good one reaches the Setup and the cluster summary.
func TestSettingsNodeAMI(t *testing.T) {
	s := newSetup("fresh")
	m := toSettings(t, testDeps(&recorder{}), s)
	if v := m.View(); !strings.Contains(v, "Node AMI") || !strings.Contains(v, "EKS optimized AL2023") {
		t.Fatalf("no Node AMI row:\n%s", v)
	}
	m = cursorTo(t, m, "NODE_AMI_ID")
	m = setText(m, "ami-nope")
	if !strings.Contains(m.View(), "an AMI ID (ami-...), or empty") {
		t.Errorf("no finding shown:\n%s", m.View())
	}
	m = press(m, "n")
	if m.m.Current() != state.Settings {
		t.Fatalf("n went past a bad AMI to %s", m.m.Current().Title())
	}
	m = setText(m, "ami-0123456789abcdef0")
	m = press(m, "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.NodeAMIID != "ami-0123456789abcdef0" {
		t.Fatalf("at %s, NodeAMIID %q", m.m.Current().Title(), s.NewClusterConfig.NodeAMIID)
	}
	if got := s.NewClusterConfig.Summary(s.NodeGroups()); !strings.HasSuffix(got, " · node AMI ami-0123456789abcdef0") {
		t.Errorf("summary %q", got)
	}
}

// A new cluster's node group type: managed by default; enter or space
// toggles it, n applies it, and the cluster summary and the Create the
// cluster checklist follow it. A connected cluster has no such row.
func TestSettingsNodeGroupType(t *testing.T) {
	s := newSetup("fresh")
	m := toSettings(t, testDeps(&recorder{}), s)
	row := func(m Model) string {
		for _, l := range strings.Split(m.View(), "\n") {
			if strings.Contains(l, "Node group type") {
				return l
			}
		}
		t.Fatalf("no Node group type row:\n%s", m.View())
		return ""
	}
	m = cursorTo(t, m, "NODE_GROUP_TYPE")
	if l := row(m); !strings.Contains(l, " managed") || strings.Contains(l, "self-managed") {
		t.Fatalf("row %q, want managed", l)
	}
	m = press(m, "enter")
	if m.set.values["NODE_GROUP_TYPE"] != "self-managed" || m.set.editing || !strings.Contains(row(m), "self-managed") {
		t.Fatalf("enter: %q (editing %v), row %q", m.set.values["NODE_GROUP_TYPE"], m.set.editing, row(m))
	}
	m = press(m, " ")
	if m.set.values["NODE_GROUP_TYPE"] != "managed" {
		t.Fatalf("space: %q", m.set.values["NODE_GROUP_TYPE"])
	}
	m = press(m, "enter", "n")
	if m.m.Current() != state.Plugins || s.NewClusterConfig.NodeGroupType != "self-managed" {
		t.Fatalf("at %s, NodeGroupType %q", m.m.Current().Title(), s.NewClusterConfig.NodeGroupType)
	}
	if got := s.NewClusterConfig.Summary(s.NodeGroups()); !strings.Contains(got, " · self-managed node groups") {
		t.Errorf("summary %q", got)
	}
	_, items, _ := m.longStep(state.CreateCluster)
	if len(items) != 9 || items[7].Label != "Creating the self-managed node groups" || items[8].Label != "Waiting for the self-managed nodes to be Ready" {
		t.Errorf("checklist %+v", items)
	}

	m = toStep(t, New(testDeps(&recorder{}), newSetup("new")), state.Settings)
	if strings.Contains(m.View(), "Node group type") {
		t.Errorf("a connected cluster has a Node group type row:\n%s", m.View())
	}
}

// The side panel of the Node group type row shows the value and config.env's,
// as a toggle's does.
func TestSettingsNodeGroupTypePanel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.env"), []byte("NODE_GROUP_TYPE=managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := state.LoadConfig(dir, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	d := testDeps(&recorder{})
	d.Config = cfg
	m := cursorTo(t, toSettings(t, d, newSetup("fresh")), "NODE_GROUP_TYPE")
	m = press(m, "enter")
	// The narrow panel wraps each line after its colon.
	v := m.View()
	for _, want := range []string{"Now:", "NODE_GROUP_TYPE=self-", "config.env:", "NODE_GROUP_TYPE=managed"} {
		if !strings.Contains(v, want) {
			t.Errorf("no %q in the panel:\n%s", want, v)
		}
	}
}

// A key without a row is not used, so a bad value of it does not hold up
// the run: on a connected cluster the new cluster's knobs (an empty or
// unknown NODE_GROUP_TYPE, a bad KUBERNETES_VERSION). A new cluster's
// still must be fixed.
func TestSettingsIgnoresKeysWithoutARow(t *testing.T) {
	s := newSetup("new")
	s.NewClusterConfig.NodeGroupType, s.NewClusterConfig.KubernetesVersion = "", "1.2"
	m := toStep(t, New(testDeps(&recorder{}), s), state.Settings)
	if m.s.ClusterIsNew {
		t.Fatal("cluster new is new")
	}
	if m = press(m, "n"); m.m.Current() != state.Plugins {
		t.Fatalf("a connected cluster stayed on %s: %q\n%s", m.m.Current().Title(), m.notice, m.View())
	}
	// SANDBOX_CLASSES has no row of its own, but a row per class: a class
	// named twice must still be fixed.
	s = newSetup("fresh")
	s.SandboxClasses = "gvisor,gvisor"
	m = press(toSettings(t, testDeps(&recorder{}), s), "n")
	if m.m.Current() != state.Settings || !strings.Contains(m.notice, "need a fix") {
		t.Errorf("went past SANDBOX_CLASSES=gvisor,gvisor to %s", m.m.Current().Title())
	}

	s = newSetup("fresh")
	s.NewClusterConfig.NodeGroupType = "bogus"
	m = press(toSettings(t, testDeps(&recorder{}), s), "n")
	if m.m.Current() != state.Settings || !strings.Contains(m.View(), "managed or self-managed") {
		t.Errorf("a new cluster went past NODE_GROUP_TYPE=bogus to %s", m.m.Current().Title())
	}
}

// The sandbox classes keep at least one, and they and the database reach the
// run: the RDS step goes, the demo checks only gvisor.
func TestSettingsDatabaseAndClassesChangeTheRun(t *testing.T) {
	r := &recorder{}
	s := newSetup("new")
	s.DemoMode = true
	m := toStep(t, New(testDeps(r), s), state.Settings)
	m = cursorTo(t, m, "microvm")
	m = press(m, "enter")
	m = cursorTo(t, m, "gvisor")
	m = press(m, "enter")
	if m.set.values[classKey] != "gvisor" || !strings.Contains(m.View(), "At least one sandbox class") {
		t.Fatalf("classes %q:\n%s", m.set.values[classKey], m.View())
	}
	m = cursorTo(t, m, dbKey)
	m = press(m, "enter", "n")
	if s.SandboxClasses != "gvisor" || s.Database != state.DatabaseBundled || slices.Contains(m.m.Order(), state.RDS) {
		t.Fatalf("setup %+v, order %v", s, m.m.Order())
	}
	m = toStep(t, m, state.Complete)
	if r.count("create rds") != 0 || len(m.tasks[state.Demo].items) != 2 {
		t.Errorf("ran %q, demo rows %d", r.ran(), len(m.tasks[state.Demo].items))
	}
}

func TestPluginsDefaultsRequiresAndTheInstallStep(t *testing.T) {
	r := &recorder{}
	s := newSetup("new")
	s.Plugins = nil // not chosen yet: the defaults
	m := toStep(t, New(testDeps(r), s), state.Plugins)
	if !m.pl.chosen["t-gateway"] || m.pl.chosen["t-efs"] {
		t.Fatalf("defaults %v", m.pl.chosen)
	}
	m = press(m, "down", "down", " ") // S3 Files, which requires EFS
	if !m.pl.chosen["t-s3files"] || !m.pl.chosen["t-efs"] || !strings.Contains(m.View(), "for t-s3files") {
		t.Fatalf("requires not selected: %v\n%s", m.pl.chosen, m.View())
	}
	m = press(m, "up", " ") // clearing EFS clears what requires it
	if m.pl.chosen["t-efs"] || m.pl.chosen["t-s3files"] {
		t.Fatalf("dependents kept: %v", m.pl.chosen)
	}
	m = press(m, "down", " ", "enter")
	if !slices.Equal(s.Plugins, []string{"t-gateway", "t-efs", "t-s3files"}) {
		t.Fatalf("plugins %q", s.Plugins)
	}
	o := m.m.Order()
	if i := slices.Index(o, state.InstallPlugins); i < 0 || o[i-1] != state.ControlPlane {
		t.Fatalf("order %v: want Install the plugins after the control plane", o)
	}
	m = toStep(t, m, state.Complete)
	before(t, r.ran(), "control plane", "plugins")
	tk := m.tasks[state.InstallPlugins]
	if len(tk.items) != 6 || tk.active != 6 || tk.items[0].Label != "Deploy LLM gateway" {
		t.Errorf("plugin checklist %d rows, active %d", len(tk.items), tk.active)
	}
}

// Both ways: choosing a plugin that builds on the counter demo turns the
// demo on and runs it before the plugins; a choice without one leaves the
// demo off.
func TestPluginsThatNeedTheDemoTurnItOn(t *testing.T) {
	for _, tc := range []struct {
		ids  []string
		demo bool
	}{{[]string{"t-ondemo"}, true}, {[]string{"t-gateway"}, false}} {
		s := newSetup("new")
		s.Plugins, s.DemoMode = tc.ids, false
		m := toStep(t, New(testDeps(&recorder{}), s), state.Plugins)
		m = press(m, "enter")
		if s.DemoMode != tc.demo {
			t.Fatalf("%v: DemoMode %v, want %v", tc.ids, s.DemoMode, tc.demo)
		}
		o := m.m.Order()
		d, p := slices.Index(o, state.Demo), slices.Index(o, state.InstallPlugins)
		if tc.demo && (d < 0 || p < d) {
			t.Errorf("%v: order %v, want the demo before the plugins", tc.ids, o)
		}
		if !tc.demo && d >= 0 {
			t.Errorf("%v: order %v has the demo", tc.ids, o)
		}
	}
}

// Both ways: a notice too long for the footer is shortened and the keys stay
// whole; a short one is shown whole.
func TestFooterNoticeNeverCutsTheKeys(t *testing.T) {
	s := newSetup("new")
	m := sized(toStep(t, New(testDeps(&recorder{}), s), state.Review))
	m.notice = strings.Repeat("a long notice about the plugins ", 6)
	if h := m.hints(); !strings.Contains(h, "ctrl+c quit") || !strings.Contains(h, "…") {
		t.Errorf("long notice: %q", h)
	}
	m.notice = "wrote ~/x"
	if h := m.hints(); !strings.Contains(h, "wrote ~/x") || !strings.Contains(h, "ctrl+c quit") {
		t.Errorf("short notice: %q", h)
	}
}

func TestPluginsFlagPreselectsInsteadOfTheDefaults(t *testing.T) {
	s := newSetup("new")
	s.Plugins = []string{"t-s3files"}
	m := toStep(t, New(testDeps(&recorder{}), s), state.Plugins)
	if m.pl.chosen["t-gateway"] || !m.pl.chosen["t-efs"] || !m.pl.chosen["t-s3files"] {
		t.Errorf("chosen %v", m.pl.chosen)
	}
	m = press(m, "enter")
	if !slices.Equal(s.Plugins, []string{"t-efs", "t-s3files"}) {
		t.Errorf("plugins %q", s.Plugins)
	}
}

// The AWS Nitro sandbox is a sandbox class, chosen in Settings: it is not on
// the Plugins screen, and with Auto Mode its plugin gets its own step after
// the control plane, before the demo. Without Auto Mode it runs only on the
// nitro node group: turning it on sets NITRO_COMPUTE=node-group
// (TestSettingsSwitchesNitroComputeWithTheClass), and auto-mode chosen
// under it stops n with the reason (both directions).
func TestNitroIsASandboxClassChosenInSettings(t *testing.T) {
	for _, auto := range []bool{true, false} {
		r := &recorder{}
		s := newSetup("fresh")
		s.DemoMode = true
		m := toSettings(t, testDeps(r), s)
		if !auto {
			m = cursorTo(t, m, "AUTO_MODE")
			m = press(m, "enter")
		}
		m = cursorTo(t, m, state.ClassNitro)
		m = press(m, "enter")
		if !m.set.classOn(state.ClassNitro) {
			t.Fatalf("auto=%v: nitro off:\n%s", auto, m.View())
		}
		if got, want := m.set.values["NITRO_COMPUTE"], map[bool]string{true: plugin.NitroAutoMode, false: plugin.NitroNodeGroup}[auto]; got != want {
			t.Fatalf("auto=%v: NITRO_COMPUTE=%s, want %s", auto, got, want)
		}
		if !auto {
			// auto-mode chosen under the class without Auto Mode: n names
			// it and stays.
			m = cursorTo(t, m, "NITRO_COMPUTE")
			m = press(m, "enter", "n")
			if m.m.Current() != state.Settings || !strings.Contains(m.View(), "AWS Nitro sandbox: needs EKS Auto Mode") {
				t.Errorf("n went on with auto-mode and Auto Mode off: at %s:\n%s", m.m.Current().Title(), m.View())
			}
			if m = cursorTo(t, m, state.ClassNitro); !strings.Contains(m.View(), "Not available: needs EKS") {
				t.Errorf("no refusal on the class panel:\n%s", m.View())
			}
			continue
		}
		m = press(m, "n")
		if strings.Contains(m.View(), "AWS Nitro sandbox") {
			t.Errorf("the class's plugin is on the Plugins screen:\n%s", m.View())
		}
		m = press(m, "enter")
		o := m.m.Order()
		c, n, d := slices.Index(o, state.ControlPlane), slices.Index(o, state.NitroSandbox), slices.Index(o, state.Demo)
		if c < 0 || n != c+1 || d != n+1 {
			t.Fatalf("order %v: want Set up Nitro sandboxes between the control plane and the demo", o)
		}
		m = toStep(t, m, state.Complete)
		before(t, r.ran(), "control plane", "nitro")
		before(t, r.ran(), "nitro", "demo")
		if tk := m.tasks[state.NitroSandbox]; tk == nil || len(tk.items) != 2 || tk.items[0].Label != "Deploy AWS Nitro sandbox" {
			t.Errorf("nitro step checklist %+v", tk)
		}
		const nitroRow = "Check suspend and resume on nitro (deployed by Set up Nitro sandboxes)"
		if tk := m.tasks[state.Demo]; len(tk.items) != 5 || tk.items[4].Label != nitroRow {
			t.Errorf("demo checklist %+v", tk.items)
		}
		// The row is one line of the panel at the screenshot size.
		m = sized(m)
		m.jump(state.InstallOrder(s), state.Demo)
		if v := m.View(); !strings.Contains(v, nitroRow) {
			t.Errorf("the nitro row is wrapped or missing at 126 columns:\n%s", v)
		}
	}
}

// Review shows every command of the run in order with its time, the total
// and the cost; nothing starts until y.
func TestReviewShowsThePlanTimesAndCost(t *testing.T) {
	r := &recorder{}
	s := newSetup("fresh")
	s.DemoMode = true
	s.Plugins = []string{"t-efs"}
	m := toSettings(t, testDeps(r), s)
	m = press(m, "n", "enter")
	if m.m.Current() != state.Review {
		t.Fatalf("at %s", m.m.Current().Title())
	}
	v := m.View()
	last := -1
	for _, want := range []string{"$ create cluster", "$ bootstrap", "$ create rds", "$ deploy --snap-bucket", "$ demo", "$ substrate-eks --plugin-action deploy --plugins t-efs"} {
		i := strings.Index(v, want)
		if i < 0 || i < last {
			t.Errorf("plan lacks %q, or out of order:\n%s", want, v)
		}
		last = i
	}
	// 20+1+8+2+6+11 minutes; the cluster $2.38, RDS $0.07, EFS $0.04.
	for _, want := range []string{"~20 min", "$2.38", "$0.07", "$0.04", "About 48 minutes in all", "about $2.48 an hour ($60 a day)"} {
		if !strings.Contains(v, want) {
			t.Errorf("review lacks %q:\n%s", want, v)
		}
	}
	m = press(m, "enter")
	if m.m.Current() != state.Review || len(r.ran()) != 0 || !strings.Contains(m.View(), "press y to start") {
		t.Fatalf("enter started the install: at %s, ran %q", m.m.Current().Title(), r.ran())
	}
	m = press(m, "y")
	if m.m.Current() != state.CreateCluster || r.count("create cluster") != 1 {
		t.Errorf("y: at %s, ran %q", m.m.Current().Title(), r.ran())
	}
}

// The cost follows the choices: nothing hourly on a connected cluster with
// the bundled database, other Regions at us-west-2 prices, and an instance
// type with no price is named instead of guessed.
func TestReviewCostFollowsTheChoices(t *testing.T) {
	s := newSetup("new")
	s.Database = state.DatabaseBundled
	m := toStep(t, New(testDeps(&recorder{}), s), state.Review)
	if v := m.View(); !strings.Contains(v, "none of them billed by the hour") || strings.Contains(v, "billable") || !strings.Contains(v, "About 3 minutes") {
		t.Errorf("connected, bundled:\n%s", v)
	}
	if head, got := costLine([]planStep{{hourly: 1}}, "us-east-1"); !strings.Contains(got, "at us-west-2 prices; see the AWS Pricing Calculator for us-east-1") || !strings.Contains(head, "billable") {
		t.Errorf("other Region: %s / %s", head, got)
	}
	c := state.ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	all := []string{"system", "gvisor", "microvm"}
	if usd, unknown := clusterHourly(c, all); unknown != "" || usd < 2.376 || usd > 2.377 {
		t.Errorf("default cluster $%.4f (%q); the price table says $2.3764", usd, unknown)
	}
	c.SystemInstanceType = "c7i.large"
	if _, unknown := clusterHourly(c, all); unknown != "c7i.large" {
		t.Errorf("unknown type: %q", unknown)
	}
	if _, got := costLine([]planStep{{unknown: "c7i.large"}}, region); !strings.Contains(got, "Pricing Calculator for c7i.large") {
		t.Errorf("unknown type cost: %s", got)
	}
}

// y writes config.local.env through SaveConfig before anything runs; a
// failed write starts nothing (both directions).
func TestReviewSavesTheSettingsBeforeStarting(t *testing.T) {
	for _, fail := range []bool{true, false} {
		r := &recorder{}
		d := testDeps(r)
		saved := 0
		d.SaveConfig = func(*state.Setup) (string, error) {
			saved++
			if fail {
				return "", errors.New("read-only file system")
			}
			return "/x/config.local.env", nil
		}
		m := toStep(t, New(d, newSetup("new")), state.Review)
		m = press(m, "y")
		if saved != 1 || (len(r.ran()) == 0) != fail || (m.m.Current() == state.Review) != fail {
			t.Errorf("fail=%v: saved %d, ran %q, at %s", fail, saved, r.ran(), m.m.Current().Title())
		}
		if fail && !strings.Contains(m.View(), "not written, nothing started") {
			t.Errorf("no refusal:\n%s", m.View())
		}
	}
}

// A setting a chosen plugin's Check refuses stops the install on Review,
// before anything is written or created: here it is only in
// config.local.env, and on t-efs, which the chosen t-s3files requires. The
// screen names it and y starts nothing; a good value starts (review of
// 86ba13f, finding 3).
func TestReviewRefusesAPluginSettingBeforeAnything(t *testing.T) {
	for _, tc := range []struct {
		value   string
		refused bool
	}{{"repo:v1", true}, {"repo@sha256:" + strings.Repeat("0", 64), false}} {
		dir := t.TempDir()
		for name, body := range map[string]string{"config.env": "T_EFS_IMAGE=\n", "config.local.env": "T_EFS_IMAGE=" + tc.value + "\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := state.LoadConfig(dir, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		r := &recorder{}
		d := testDeps(r)
		d.Config = cfg
		saved := 0
		d.SaveConfig = func(*state.Setup) (string, error) { saved++; return "/x/config.local.env", nil }
		s := newSetup("new")
		s.Plugins = []string{"t-s3files"}
		m := toStep(t, New(d, s), state.Review)
		named := strings.Contains(m.View(), "Nothing can start: plugin t-efs: set T_EFS_IMAGE to")
		m = press(m, "y")
		if tc.refused {
			if !named || m.m.Current() != state.Review || len(r.ran()) != 0 || saved != 0 || !strings.Contains(m.View(), "nothing started: plugin t-efs") {
				t.Errorf("%s: named=%v, at %s, ran %q, saved %d:\n%s", tc.value, named, m.m.Current().Title(), r.ran(), saved, m.View())
			}
			continue
		}
		if named || m.m.Current() == state.Review || len(r.ran()) == 0 || saved != 1 {
			t.Errorf("%s: named=%v, at %s, ran %q, saved %d", tc.value, named, m.m.Current().Title(), r.ran(), saved)
		}
	}
}

// The plan fits the screenshot size, cut to one line a command when it must.
func TestReviewFitsTheTerminal(t *testing.T) {
	s := newSetup("fresh")
	s.DemoMode = true
	s.Plugins = []string{"t-gateway", "t-s3files"}
	m := sized(toSettings(t, testDeps(&recorder{}), s))
	m = press(m, "n", "enter")
	v := m.View()
	if h := lipgloss.Height(v); h > 33 || !strings.Contains(v, "Press y to start") {
		t.Errorf("%d rows:\n%s", h, v)
	}
}

// s saves the screen; where s is a typed character it types, and ctrl+s
// saves (both directions).
func TestSnapshotKey(t *testing.T) {
	d := testDeps(&recorder{})
	d.SnapshotDir = t.TempDir()
	m := press(New(d, newSetup("")), "s")
	files, _ := filepath.Glob(filepath.Join(d.SnapshotDir, "snap-*-01-welcome.txt"))
	if len(files) != 1 {
		t.Fatalf("snapshots %v", files)
	}
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), "Agent Substrate on Amazon EKS") || strings.Contains(string(b), "\x1b[") {
		t.Errorf("snapshot text:\n%s", b)
	}
	if !strings.Contains(m.View(), "saved snapshot") {
		t.Error("no notice")
	}
	m = toStep(t, m, state.Cluster)
	m = press(m, "enter", "s") // the name prompt
	if m.cl.name != "s" {
		t.Errorf("s in the name prompt: name %q", m.cl.name)
	}
	m = press(m, "ctrl+s")
	all, _ := filepath.Glob(filepath.Join(d.SnapshotDir, "*.txt"))
	if len(all) != 2 {
		t.Errorf("snapshots %v", all)
	}
	for _, ext := range []string{".ansi", ".html"} {
		if _, err := os.Stat(strings.TrimSuffix(files[0], ".txt") + ext); err != nil {
			t.Error(err)
		}
	}
}

// --redact masks all of each account id, ARNs' account and resource name,
// and bucket names, keeping each line's width; without it the last four
// digits and the names show (both directions).
func TestRedact(t *testing.T) {
	for _, on := range []bool{true, false} {
		var all strings.Builder
		views = func(v string) { all.WriteString(v) }
		d := testDeps(&recorder{})
		d.Redact = on
		s := newSetup("new")
		toStep(t, New(d, s), state.Complete)
		views = nil
		v := all.String()
		for _, shown := range []string{"••••••••3333", "assumed-role/Admin/x", "new-snap-", "s3://new-assets"} {
			if strings.Contains(v, shown) == on {
				t.Errorf("redact=%v: %q shown=%v", on, shown, !on)
			}
		}
	}
	m := Model{d: Deps{Redact: true}}
	in := "arn:aws:sts::••••••••3333:assumed-role/Admin/you s3://x-snap-••••••••3333-us-west-2/a"
	if out := m.redact(in); utf8.RuneCountInString(out) != utf8.RuneCountInString(in) || strings.Contains(out, "Admin") || !strings.HasPrefix(out, "arn:aws:sts::") {
		t.Errorf("redact %q -> %q", in, out)
	}
	// Phone numbers (E.164, review N2), both ways: masked with --redact,
	// keeping the + and the width; shown without it. A + before too few or
	// too many digits for E.164 is not a phone number.
	for in, want := range map[string]string{
		"Sending status ping to +15555550100.": "Sending status ping to +•••••••••••.",
		"WHATSAPP_PEER=+447700900123 set":      "WHATSAPP_PEER=+•••••••••••• set",
		"diff +12 lines, +0123456789":          "diff +12 lines, +0123456789",
		"id +1234567890123456789":              "id +1234567890123456789",
	} {
		if out := m.redact(in); out != want {
			t.Errorf("redact %q -> %q, want %q", in, out, want)
		}
		if out := (Model{}).redact(in); out != in {
			t.Errorf("without --redact %q -> %q", in, out)
		}
	}
}

var foldedRows = regexp.MustCompile(`(✓ \d+ done|○ \d+ more)`)

// Seven plugins make the Install the plugins step 14 rows, as on the owner's
// Mac run. Whatever the terminal's height, a running step shows its active
// row (under the stop confirmation, which stays first), and a failed or
// stopped one its failed row and outcome: rows that do not fit fold into one
// line each side of that row. A terminal tall enough for everything shows
// every row, unfolded.
func TestTheActiveRowStaysOnAShortScreen(t *testing.T) {
	ids := []string{"t-gateway", "t-efs", "t-s3files", "t-ondemo", "t-kata", "t-claw", "t-rot"}
	for _, tc := range []struct {
		h, active int
		err       error
		ask       bool
		want      []string
	}{
		{30, 13, nil, true, []string{"Stop plugins?", "⠋ Check Login rotation"}},
		{24, 13, nil, false, []string{"⠋ Check Login rotation", "output line 19"}},
		{24, 3, errStopped, false, []string{"✗ Check EFS volumes", "Stopped with ctrl+c"}},
		{24, 3, errors.New("helm upgrade failed"), false, []string{"✗ Check EFS volumes", "Failed: helm upgrade failed"}},
		{30, 3, errors.New("helm upgrade failed"), false, []string{"✗ Check EFS volumes", "Failed: helm upgrade failed"}},
	} {
		s := newSetup("new")
		s.Plugins = ids
		m := New(testDeps(&recorder{}), s)
		next, _ := m.Update(tea.WindowSizeMsg{Width: 126, Height: tc.h})
		m = next.(Model)
		m.jump(state.InstallOrder(s), state.InstallPlugins)
		tk := newTask(execx.Spec{Label: "plugins", Display: "substrate-eks --plugin-action deploy --cluster new --region us-west-2 --plugins " + strings.Join(ids, ",")},
			steps.Plugins(resolved(ids)))
		tk.active, tk.done, tk.err = tc.active, tc.err != nil, tc.err
		for i := range 20 {
			tk.lines = append(tk.lines, fmt.Sprintf("output line %d", i))
		}
		m.tasks[state.InstallPlugins] = tk
		m.stopAsk = tc.ask
		v := stripANSI(m.View())
		name := fmt.Sprintf("%d rows, row %d, %v", tc.h, tc.active, tc.err)
		if len(tk.items) != 14 {
			t.Fatalf("%s: %d rows, want 14", name, len(tk.items))
		}
		last := -1
		for _, want := range tc.want {
			i := strings.Index(v, want)
			if i < 0 || i < last {
				t.Errorf("%s: %q missing or out of order:\n%s", name, want, v)
			}
			last = i
		}
		if h := lipgloss.Height(v); h > tc.h {
			t.Errorf("%s: %d rows tall", name, h)
		}
	}
	// Tall enough for everything: every row, none folded.
	s := newSetup("new")
	s.Plugins = ids
	m := New(testDeps(&recorder{}), s)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 126, Height: 60})
	m = next.(Model)
	m.jump(state.InstallOrder(s), state.InstallPlugins)
	tk := newTask(execx.Spec{Label: "plugins", Display: "substrate-eks --plugin-action deploy"}, steps.Plugins(resolved(ids)))
	tk.active = 13
	m.tasks[state.InstallPlugins] = tk
	v := stripANSI(m.View())
	for _, it := range tk.items {
		if !strings.Contains(v, it.Label) {
			t.Errorf("60 rows: %q missing:\n%s", it.Label, v)
		}
	}
	if f := foldedRows.FindString(v); f != "" {
		t.Errorf("60 rows: folded %q:\n%s", f, v)
	}
}

// On a full terminal the body keeps its top, so a long step's outcome is
// drawn above its output tail and is never the line cut off.
func TestOutcomeStaysOnAShortScreen(t *testing.T) {
	for _, err := range []error{errStopped, context.DeadlineExceeded} {
		s := newSetup("fresh")
		s.ClusterIsNew = true
		m := sized(New(testDeps(&recorder{}), s))
		m.jump(state.InstallOrder(s), state.CreateCluster)
		tk := newTask(execx.Spec{Label: "create cluster", Display: strings.Repeat("CLUSTER_NAME=fresh ", 8)}, steps.Cluster(""))
		tk.done, tk.err, tk.active = true, err, 2
		for i := range 20 {
			tk.lines = append(tk.lines, fmt.Sprintf("output line %d", i))
		}
		m.tasks[state.CreateCluster] = tk
		v := m.View()
		if h := lipgloss.Height(v); h > 33 || !(strings.Contains(v, "Stopped with ctrl+c") || strings.Contains(v, "Failed: context deadline exceeded")) {
			t.Errorf("%v: %d rows, outcome cut:\n%s", err, h, v)
		}
	}
}
