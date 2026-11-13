package ui

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

const region = "us-west-2"

// recorder is the Runner of these tests: it records each command's Label,
// fails the first fails[label] starts of a label, and replays SimLines.
// probes holds each cluster's probe answer (probe.SimLine's names by
// default), which a "remove <cluster>" start clears, as the real removal
// would.
type recorder struct {
	mu      sync.Mutex
	started []string
	fails   map[string]int
	probes  map[string]string
}

func (r *recorder) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	r.mu.Lock()
	r.started = append(r.started, spec.Label)
	n := 0
	for _, l := range r.started {
		if l == spec.Label {
			n++
		}
	}
	if c, ok := strings.CutPrefix(spec.Label, "remove "); ok {
		r.probes[c] = probe.Marker + "false"
	}
	fail := n <= r.fails[spec.Label]
	r.mu.Unlock()
	if fail {
		ch := make(chan execx.Event, 2)
		ch <- execx.Event{Line: "error: rollout timed out"}
		ch <- execx.Event{Done: true, Err: context.DeadlineExceeded}
		close(ch)
		return ch
	}
	return execx.DryRun{Delay: 1}.Start(ctx, spec)
}

// ran lists the commands started, probes left out.
func (r *recorder) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.started {
		if !strings.HasPrefix(l, "probe ") {
			out = append(out, l)
		}
	}
	return out
}

func (r *recorder) count(label string) int {
	n := 0
	for _, l := range r.ran() {
		if l == label {
			n++
		}
	}
	return n
}

func owned(name string) awsx.Cluster {
	return awsx.Cluster{Name: name, Version: "1.37", Status: "ACTIVE", AutoMode: true,
		Tags: map[string]string{awsx.CreatedByTagKey: awsx.CreatedByValue, awsx.OwnerTagKey: name + "." + region}}
}

func notOwned(name string) awsx.Cluster {
	return awsx.Cluster{Name: name, Version: "1.37", Status: "ACTIVE"}
}

var clusters = []awsx.Cluster{
	{Name: "old", Version: "1.36", Status: "ACTIVE"},
	owned("new"),
	notOwned("app-installed"),
	owned("own-installed"),
	notOwned("team-partial"),
}

func spec(label string, sim ...string) func(*state.Setup) execx.Spec {
	return func(*state.Setup) execx.Spec { return execx.Spec{Label: label, Display: label, SimLines: sim} }
}

func testDeps(r *recorder) Deps {
	if r.probes == nil {
		r.probes = map[string]string{}
	}
	return Deps{
		Runner: r,
		Checks: []doctor.Check{{Name: "aws", Run: func(context.Context) doctor.Result { return doctor.Result{Status: doctor.OK} }}},
		Identity: func(context.Context) (string, string, error) {
			return "111122223333", "arn:aws:sts::111122223333:assumed-role/Admin/x", nil
		},
		Clusters: func(context.Context) ([]awsx.Cluster, error) { return clusters, nil },
		Describe: func(_ context.Context, name string) (awsx.Cluster, error) {
			for _, c := range clusters {
				if c.Name == name {
					return c, nil
				}
			}
			return owned(name), nil
		},
		Probe: func(c string, _ *state.Setup) execx.Spec {
			r.mu.Lock()
			line, ok := r.probes[c]
			r.mu.Unlock()
			if !ok {
				line = probe.SimLine(c)
			}
			return execx.Spec{Label: "probe " + c, SimLines: []string{line}}
		},
		CreateCluster: spec("create cluster", "Step 1/9: Checking the account, Region and quotas..."),
		Bootstrap:     spec("bootstrap", "Step 1/4: Checking the EKS cluster..."),
		RDS:           spec("create rds", "Step 1/4: Creating the RDS subnet group and security group..."),
		// An account id in a command's output, as ate-setup prints bucket
		// names, and in the command shown, as an argv with a bucket would.
		ControlPlane: func(*state.Setup) execx.Spec {
			return execx.Spec{Label: "control plane", Display: "deploy --snap-bucket new-snap-111122223333-us-west-2",
				SimLines: []string{"Uploading assets to s3://new-assets-111122223333-us-west-2/microvm-assets/ ...", "[step]: deploy_crds"}}
		},
		Demo:      spec("demo", "Deploying the counter demo on gvisor"),
		Verify:    spec("verify"),
		Inventory: spec("inventory"),
		RemoveControlPlane: func(s *state.Setup) execx.Spec {
			return execx.Spec{Label: "remove " + s.ClusterName, Display: "substrate-eks --remove-control-plane --cluster " + s.ClusterName}
		},
		Teardown: func(s *state.Setup) execx.Spec {
			l := "teardown keep=" + strconv.FormatBool(s.KeepCluster)
			return execx.Spec{Label: l, Display: l}
		},
	}
}

func newSetup(cluster string) *state.Setup {
	s := state.NewSetup()
	s.Region, s.ClusterName = region, cluster
	return s
}

func key(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// views, when set, gets every frame the tests render.
var views func(string)

// pump runs cmd and every command it leads to, the way the Bubble Tea
// runtime would, until nothing is left (the spinner's tick aside).
func pump(m Model, cmd tea.Cmd) Model {
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, tickMsg, tea.QuitMsg:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			next, nc := m.Update(msg)
			m = next.(Model)
			if views != nil {
				views(m.View())
			}
			queue = append(queue, nc)
		}
	}
	return m
}

// press sends each key and runs what it starts to completion.
func press(m Model, keys ...string) Model {
	for _, k := range keys {
		next, cmd := m.Update(key(k))
		m = next.(Model)
		if views != nil {
			views(m.View())
		}
		m = pump(m, cmd)
	}
	return m
}

// typeIn types text into the field the screen has focused.
func typeIn(m Model, text string) Model {
	for _, r := range text {
		m = press(m, string(r))
	}
	return m
}

// toStep presses enter until the wizard is at step, failing after 20.
func toStep(t *testing.T, m Model, step state.Step) Model {
	t.Helper()
	for range 20 {
		if m.m.Current() == step {
			return m
		}
		m = press(m, "enter")
	}
	t.Fatalf("never reached %s; at %s (cluster mode %q)", step.Title(), m.m.Current().Title(), m.cl.mode)
	return m
}

func before(t *testing.T, list []string, a, b string) {
	t.Helper()
	i, j := slices.Index(list, a), slices.Index(list, b)
	if i < 0 || j < 0 || i > j {
		t.Errorf("want %q before %q in %q", a, b, list)
	}
}

func TestHappyPathReachesComplete(t *testing.T) {
	r := &recorder{}
	s := newSetup("new")
	m := toStep(t, New(testDeps(r), s), state.Complete)
	if s.ClusterName != "new" || !s.AutoMode || s.ClusterIsNew || !s.ClusterOwned {
		t.Errorf("setup = %+v", s)
	}
	if got, want := r.ran(), []string{"bootstrap", "create rds", "control plane"}; !slices.Equal(got, want) {
		t.Errorf("ran %q, want %q", got, want)
	}
	if !strings.Contains(m.View(), "Agent Substrate is running on new") {
		t.Error("complete screen missing")
	}
	// The teardown commands are printed again on exit, unwrapped.
	notes := strings.Join(m.ExitNotes(), "\n")
	if !strings.Contains(notes, "  substrate-eks --remove-control-plane --cluster new\n  teardown keep=true") {
		t.Errorf("exit notes:\n%s", notes)
	}
}

func TestFailedCheckBlocksTheWizard(t *testing.T) {
	d := testDeps(&recorder{})
	d.Checks[0].Run = func(context.Context) doctor.Result { return doctor.Result{Status: doctor.Fail} }
	m := press(New(d, newSetup("new")), "enter", "enter")
	if got := m.m.Current(); got != state.CheckSetup {
		t.Fatalf("advanced past a failed check to %s", got.Title())
	}
}

func TestUnsupportedClusterCannotBeSelected(t *testing.T) {
	m := toStep(t, New(testDeps(&recorder{}), newSetup("old")), state.Cluster)
	m = press(m, "enter") // "old" is 1.36
	if got := m.m.Current(); got != state.Cluster || !strings.Contains(m.View(), "needs 1.37+") {
		t.Fatalf("selected an unsupported cluster and moved to %s", got.Title())
	}
}

// AC5.1: each cluster is probed in the background and badged.
func TestPickerBadgesEachCluster(t *testing.T) {
	m := toStep(t, New(testDeps(&recorder{}), newSetup("new")), state.Cluster)
	v := m.View()
	for _, want := range []string{"new                      1.37  * ✓ ready · Auto Mode", "installed v1.0.0", "partial install", "needs 1.37+", "+ Create a new cluster"} {
		if !strings.Contains(v, want) {
			t.Errorf("picker lacks %q:\n%s", want, v)
		}
	}
}

func TestCreateNewClusterRunsCreateClusterBeforeProvision(t *testing.T) {
	r := &recorder{}
	s := newSetup("fresh") // not listed: the cursor starts on the create row
	m := toStep(t, New(testDeps(r), s), state.Cluster)
	m = press(m, "enter")
	if m.cl.mode != "name" || !strings.Contains(m.View(), "› fresh") || !strings.Contains(m.View(), "EKS 1.37 · Auto Mode on") {
		t.Fatalf("name prompt with the review line missing:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Database || !s.ClusterIsNew || !s.ClusterOwned || s.ClusterName != "fresh" {
		t.Fatalf("at %s, setup %+v", m.m.Current().Title(), s)
	}
	order := m.m.Order()
	if i, j := slices.Index(order, state.CreateCluster), slices.Index(order, state.Provision); i < 0 || i != j-1 {
		t.Fatalf("order %v: want CreateCluster right before Provision", order)
	}
	m = press(m, "enter") // RDS, the default
	if m.m.Current() != state.CreateCluster {
		t.Fatalf("at %s, want Create the cluster", m.m.Current().Title())
	}
	m = toStep(t, m, state.Provision)
	before(t, r.ran(), "create cluster", "bootstrap")
}

// The name is refused while it breaks the rule or names a listed cluster,
// and q types into it rather than quitting.
func TestNewClusterNameIsValidatedAndMustNotExist(t *testing.T) {
	m := toStep(t, New(testDeps(&recorder{}), newSetup("")), state.Cluster)
	m = press(m, "enter")
	for _, bad := range []string{"new", "Bad_Name", "q"} {
		m = typeIn(m, bad)
		m = press(m, "enter")
		if m.m.Current() != state.Cluster || m.cl.mode != "name" {
			t.Fatalf("%q: left the prompt (at %s, mode %s)", bad, m.m.Current().Title(), m.cl.mode)
		}
		if bad == "new" && !strings.Contains(m.View(), "new already exists") {
			t.Errorf("no duplicate-name refusal:\n%s", m.View())
		}
		for range bad {
			m = press(m, "backspace")
		}
	}
	if m.quitting {
		t.Error("q quit from the name prompt")
	}
}

// AC5.2: an installed or partial cluster cannot be installed over, and the
// remove offer appears only on a cluster this installer created.
func TestInstalledClusterIsGuarded(t *testing.T) {
	for _, name := range []string{"app-installed", "team-partial"} {
		r := &recorder{}
		m := toStep(t, New(testDeps(r), newSetup(name)), state.Cluster)
		m = press(m, "enter", "enter", "t", "enter")
		if m.m.Current() != state.Cluster || (m.cl.mode != "installed" && m.cl.mode != "partial") {
			t.Fatalf("%s: at %s, mode %q", name, m.m.Current().Title(), m.cl.mode)
		}
		v := m.View()
		if !strings.Contains(v, "did not create "+name) || strings.Contains(v, "remove Substrate from it") {
			t.Errorf("%s: want the refusal and no remove offer:\n%s", name, v)
		}
		if len(r.ran()) != 0 {
			t.Errorf("%s: ran %q", name, r.ran())
		}
	}
}

func TestGuardRemovesSubstrateFromAClusterItCreated(t *testing.T) {
	r := &recorder{fails: map[string]int{"remove own-installed": 1}}
	s := newSetup("own-installed")
	m := toStep(t, New(testDeps(r), s), state.Cluster)
	m = press(m, "enter")
	if m.cl.mode != "installed" || !strings.Contains(m.View(), "t remove Substrate from it") {
		t.Fatalf("mode %q, want the remove offer:\n%s", m.cl.mode, m.View())
	}
	m = press(m, "t", "enter") // the name is not typed yet
	if m.cl.mode != "remove-confirm" || len(r.ran()) != 0 {
		t.Fatalf("removed without the typed confirmation: mode %q, ran %q", m.cl.mode, r.ran())
	}
	m = typeIn(m, "own-installed")
	m = press(m, "enter")
	if m.cl.mode != "removing" || m.cl.removal.err == nil {
		t.Fatalf("want a failed removal, mode %q", m.cl.mode)
	}
	m = press(m, "enter") // retry, then the cluster is probed again and chosen
	if r.count("remove own-installed") != 2 || m.m.Current() != state.Database || s.ClusterName != "own-installed" {
		t.Fatalf("ran %q, at %s, cluster %q", r.ran(), m.m.Current().Title(), s.ClusterName)
	}
}

func TestDatabaseChoiceAddsOrOmitsTheRDSStep(t *testing.T) {
	for _, tc := range []struct {
		keys    []string
		want    string
		hasStep bool
	}{
		{[]string{"enter"}, state.DatabaseRDS, true},
		{[]string{"down", "enter"}, state.DatabaseBundled, false},
	} {
		r := &recorder{}
		s := newSetup("new")
		m := toStep(t, New(testDeps(r), s), state.Database)
		m = press(m, tc.keys...)
		m = toStep(t, m, state.Complete)
		if s.Database != tc.want || slices.Contains(m.m.Order(), state.RDS) != tc.hasStep || (r.count("create rds") == 1) != tc.hasStep {
			t.Errorf("%s: order %v, ran %q", tc.want, m.m.Order(), r.ran())
		}
	}
}

func TestDemoAddsTheDemoStep(t *testing.T) {
	r := &recorder{}
	s := newSetup("new")
	s.DemoMode = true
	m := New(testDeps(r), s)
	if o := m.m.Order(); o[len(o)-2] != state.Demo {
		t.Fatalf("order %v: want Demo before Complete", o)
	}
	m = toStep(t, m, state.Complete)
	before(t, r.ran(), "control plane", "demo")
	if !strings.Contains(m.View(), "Check the demo again") {
		t.Errorf("no verify command on Complete:\n%s", m.View())
	}
}

// AC6.4: the Complete screen prints the teardown commands, [t] runs them in
// that order, and the scope choice reaches setup-aws as --keep-cluster.
func TestTeardownFromCompleteRunsRemoveThenTeardown(t *testing.T) {
	for _, tc := range []struct {
		keys []string
		keep bool
	}{{nil, true}, {[]string{"down"}, false}} {
		r := &recorder{}
		s := newSetup("new")
		m := toStep(t, New(testDeps(r), s), state.Complete)
		v := m.View()
		if !strings.Contains(v, "substrate-eks --remove-control-plane --cluster new") || !strings.Contains(v, "teardown keep=true") {
			t.Fatalf("Complete lacks the teardown commands:\n%s", v)
		}
		m = press(m, "t")
		if m.m.Current() != state.TeardownScope {
			t.Fatalf("t went to %s", m.m.Current().Title())
		}
		m = press(m, tc.keys...)
		m = press(m, "enter") // not confirmed yet
		if m.m.Current() != state.TeardownScope {
			t.Fatal("teardown started without the typed name")
		}
		m = typeIn(m, "new")
		m = toStep(t, m, state.Complete)
		want := "teardown keep=" + strconv.FormatBool(tc.keep)
		before(t, r.ran(), "remove new", want)
		if s.KeepCluster != tc.keep {
			t.Errorf("KeepCluster = %v, want %v", s.KeepCluster, tc.keep)
		}
	}
}

// --teardown on a cluster this installer did not create offers only
// --keep-cluster.
func TestTeardownNeverDeletesAClusterItDidNotCreate(t *testing.T) {
	r := &recorder{}
	s := newSetup("app-installed")
	s.Teardown = true
	m := toStep(t, New(testDeps(r), s), state.TeardownScope)
	m = press(m, "down")
	m = typeIn(m, "app-installed")
	m = toStep(t, m, state.Complete)
	if !slices.Equal(r.ran(), []string{"remove app-installed", "teardown keep=true"}) || !s.KeepCluster {
		t.Errorf("ran %q", r.ran())
	}
}

// --keep-cluster (make teardown KEEP_CLUSTER=true) keeps even a cluster this
// installer created; without it the same keys delete it (both directions).
func TestKeepClusterFlagKeepsAnOwnedCluster(t *testing.T) {
	for _, only := range []bool{true, false} {
		r := &recorder{}
		s := newSetup("own-installed")
		s.Teardown, s.KeepClusterOnly = true, only
		m := toStep(t, New(testDeps(r), s), state.TeardownScope)
		m = press(m, "down")
		m = typeIn(m, "own-installed")
		m = toStep(t, m, state.Complete)
		want := []string{"remove own-installed", "teardown keep=" + strconv.FormatBool(only)}
		if !slices.Equal(r.ran(), want) || s.KeepCluster != only {
			t.Errorf("KeepClusterOnly=%v: ran %q, KeepCluster=%v", only, r.ran(), s.KeepCluster)
		}
	}
}

// AC11.3: every long step that fails is retried in place with enter.
func TestEveryLongStepRetriesOnEnter(t *testing.T) {
	long := []string{"create cluster", "bootstrap", "create rds", "control plane", "demo", "remove fresh", "teardown keep=true"}
	r := &recorder{fails: map[string]int{}}
	for _, l := range long {
		r.fails[l] = 1
	}
	s := newSetup("fresh")
	s.DemoMode = true
	m := toStep(t, New(testDeps(r), s), state.Cluster)
	m = press(m, "enter", "enter", "enter") // create "fresh", RDS
	for m.m.Current() != state.Complete {
		cur := m.m.Current()
		if tk := m.tasks[cur]; tk == nil || tk.err == nil || !strings.Contains(m.View(), "enter to retry") {
			t.Fatalf("%s: want a failed step showing the retry", cur.Title())
		}
		m = press(m, "enter")
		if tk := m.tasks[cur]; m.m.Current() != cur || !tk.done || tk.err != nil {
			t.Fatalf("%s: retry did not complete in place", cur.Title())
		}
		m = press(m, "enter")
	}
	m = press(m, "t")
	m = typeIn(m, "fresh")
	m = press(m, "enter")
	for m.m.Current() != state.Complete {
		cur := m.m.Current()
		m = press(m, "enter", "enter")
		if m.m.Current() == cur {
			t.Fatalf("%s: stuck after the retry", cur.Title())
		}
	}
	for _, l := range long {
		if n := r.count(l); n != 2 {
			t.Errorf("%s started %d times, want 2 (fail, retry)", l, n)
		}
	}
}

// AC10.4: no account id on any frame, including one a command printed.
func TestAccountIdNeverShown(t *testing.T) {
	id := regexp.MustCompile(`\b[0-9]{12}\b`)
	frames := 0
	views = func(v string) {
		frames++
		if id.MatchString(v) {
			t.Errorf("frame shows an account id:\n%s", v)
		}
	}
	defer func() { views = nil }()
	s := newSetup("new")
	s.DemoMode = true
	m := toStep(t, New(testDeps(&recorder{}), s), state.Complete)
	// Each output line is masked as it is stored, not only when drawn.
	for st, tk := range m.tasks {
		for _, l := range tk.lines {
			if id.MatchString(l) {
				t.Errorf("%s stored %q", st.Title(), l)
			}
		}
	}
	m = press(m, "t")
	m = typeIn(m, "new")
	toStep(t, m, state.Complete)
	if frames < 20 {
		t.Errorf("only %d frames checked", frames)
	}
	if got := maskIDs("s3://x-snap-111122223333-us-west-2/"); got != "s3://x-snap-••••••••3333-us-west-2/" {
		t.Errorf("maskIDs = %q", got)
	}
}

// A cluster the probe cannot read is not offered for install; r retries.
func TestProbeFailureBlocksTheClusterUntilRetried(t *testing.T) {
	r := &recorder{fails: map[string]int{"probe new": 2}} // the background and the first foreground probe
	m := toStep(t, New(testDeps(r), newSetup("new")), state.Cluster)
	if !strings.Contains(m.View(), "not checked") {
		t.Fatalf("failed probe not badged:\n%s", m.View())
	}
	m = press(m, "enter")
	if m.m.Current() != state.Cluster || !strings.Contains(m.View(), "Could not check new") || !strings.Contains(m.View(), "rollout timed out") {
		t.Fatalf("at %s:\n%s", m.m.Current().Title(), m.View())
	}
	m = press(m, "enter") // enter does not skip the check
	if m.m.Current() != state.Cluster {
		t.Fatal("enter skipped a failed probe")
	}
	m = press(m, "r")
	if m.m.Current() != state.Database {
		t.Fatalf("at %s after the retry", m.m.Current().Title())
	}
}
