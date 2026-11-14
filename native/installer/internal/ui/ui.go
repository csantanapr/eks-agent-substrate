// Package ui is the wizard, a Bubble Tea program over state.Machine, the
// same shape as substrate-gke's installer/internal/ui: one screen per Step,
// a step rail, and long steps that stream an execx.Spec with a checklist.
// The cluster screen (cluster.go) carries substrate-gke's create-or-connect
// list and reinstall guard; choices.go has the database and teardown choices.
package ui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/snap"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// Deps are what the screens call out to; tests and --dry-run replace them.
type Deps struct {
	Runner execx.Runner
	// Checks are the doctor checks.
	Checks []doctor.Check
	// Identity, Clusters and Describe read AWS.
	Identity func(ctx context.Context) (account, arn string, err error)
	Clusters func(ctx context.Context) ([]awsx.Cluster, error)
	Describe func(ctx context.Context, name string) (awsx.Cluster, error)
	// Probe is the reinstall guard's read of one cluster (probe.ParseInstalled
	// reads its output).
	Probe func(cluster string, s *state.Setup) execx.Spec
	// The long steps' commands, one per step. Plugins is --plugin-action
	// deploy for the chosen plugins.
	CreateCluster, Bootstrap, RDS, ControlPlane, Plugins, Demo func(*state.Setup) execx.Spec
	RemoveControlPlane, Teardown                               func(*state.Setup) execx.Spec
	// Verify and Inventory are commands the Complete screen prints.
	Verify, Inventory func(*state.Setup) execx.Spec
	// LogPath is shown on failure and at the end.
	LogPath string
	DryRun  bool

	// Splash plays the opening animation before Welcome; any key skips it.
	Splash bool
	// Now is the header clock; nil is time.Now.
	Now func() time.Time
	// SnapshotDir is where s saves the screen (snap.Writer).
	SnapshotDir string
	// Redact masks account ids, ARNs and bucket names on screen, in
	// snapshots and in the exit notes (--redact).
	Redact bool
	// Config is config.env and config.local.env: the Settings screen's help.
	Config state.Config
	// SaveConfig writes the Settings answers to config.local.env when the
	// install starts and returns its path; nil writes nothing (a dry run, or
	// no eks-talk checkout).
	SaveConfig func(*state.Setup) (string, error)
	// StopTree ends every process the installer started and their children,
	// for ctrl+c on a running step (StopProcessTree); nil only cancels the
	// step, which is enough for the dry run's replay.
	StopTree func()
}

type (
	checksMsg   []doctor.Result
	identityMsg struct {
		account, arn string
		err          error
	}
	execMsg struct {
		t  *task
		ev execx.Event
		ch <-chan execx.Event
	}
	tickMsg time.Time
)

// errStopped is a step the user stopped with ctrl+c: done, not successful,
// and rerun with enter like a failure.
var errStopped = errors.New("stopped with ctrl+c")

// task is a long step in flight or finished.
type task struct {
	items      []steps.ChecklistItem
	active     int
	lines      []string
	done       bool
	err        error
	start, end time.Time
	spec       execx.Spec
	// cancel ends the Runner's context; stopped is the user's ctrl+c, which
	// makes the end of the step errStopped whatever the runner reports.
	cancel  context.CancelFunc
	stopped bool
}

func newTask(spec execx.Spec, items []steps.ChecklistItem) *task {
	return &task{items: items, active: -1, start: time.Now(), spec: spec}
}

// Model is the wizard.
type Model struct {
	d       Deps
	m       *state.Machine
	s       *state.Setup
	w, h    int
	frame   int
	results []doctor.Result
	idErr   error

	// tasks are the long steps run so far, by step. A failed one is rerun in
	// place with enter; every step is idempotent.
	tasks map[state.Step]*task

	cl    *clusterScreen
	db    *databaseScreen
	scope *scopeScreen
	set   *settingsScreen
	pl    *pluginsScreen

	// splash is the opening animation, at splashFrame.
	splash      bool
	splashFrame int
	// stopAsk is ctrl+c on a running step, waiting for y.
	stopAsk bool
	// notice is a one-line message under the screen, until noticeUntil.
	notice      string
	noticeUntil time.Time
	// session names this run's snapshots; snapN counts them.
	session string
	snapN   int

	quitting bool
}

// New returns the wizard at Welcome, on the install flow, or on the teardown
// flow when s.Teardown.
func New(d Deps, s *state.Setup) Model {
	if d.Now == nil {
		d.Now = time.Now
	}
	m := Model{d: d, m: state.NewMachine(), s: s, tasks: map[state.Step]*task{},
		cl: newClusterScreen(), db: &databaseScreen{}, scope: &scopeScreen{},
		set: &settingsScreen{}, pl: &pluginsScreen{},
		splash: d.Splash, session: d.Now().Format("20060102-150405")}
	if s.Teardown {
		m.m.SetOrder(state.TeardownOrder)
	} else {
		m.m.SetOrder(state.InstallOrder(s))
	}
	return m
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init starts the spinner, and the splash when it plays.
func (m Model) Init() tea.Cmd {
	if m.splash {
		return tea.Batch(tick(), splashTick())
	}
	return tick()
}

func listen(t *task, ch <-chan execx.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return execMsg{t: t, ev: execx.Event{Done: true}}
		}
		return execMsg{t: t, ev: ev, ch: ch}
	}
}

func (m *Model) run(t *task) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	return listen(t, m.d.Runner.Start(ctx, t.spec))
}

// runningTask is the long step in flight on this screen, the cluster
// guard's removal included, or nil.
func (m Model) runningTask() *task {
	if t := m.tasks[m.m.Current()]; t != nil && !t.done {
		return t
	}
	if t := m.cl.removal; m.m.Current() == state.Cluster && m.cl.mode == "removing" && t != nil && !t.done {
		return t
	}
	return nil
}

// stopTask is the confirmed ctrl+c on a running step (review F10). Ending
// the Runner's context alone kills only the direct child: `go run` leaves
// the program it built running, and that program its kubectl and ate-setup,
// all still writing to the step's pipes. So StopTree first ends the whole
// tree, then the context is cancelled. The step then ends as errStopped,
// which enter reruns as it does a failure.
func (m *Model) stopTask(t *task) tea.Cmd {
	t.stopped = true
	tree, cancel := m.d.StopTree, t.cancel
	return func() tea.Msg {
		if tree != nil {
			tree()
		}
		if cancel != nil {
			cancel()
		}
		return nil
	}
}

// longStep builds the command and checklist of a step that runs one, and
// reports false for every other step.
func (m *Model) longStep(st state.Step) (func(*state.Setup) execx.Spec, []steps.ChecklistItem, bool) {
	switch st {
	case state.CreateCluster:
		return m.d.CreateCluster, steps.Cluster(), true
	case state.Provision:
		return m.d.Bootstrap, steps.Bootstrap(), true
	case state.RDS:
		return m.d.RDS, steps.RDS(), true
	case state.ControlPlane:
		return m.d.ControlPlane, steps.ControlPlane(), true
	case state.InstallPlugins:
		return m.d.Plugins, steps.Plugins(resolved(m.s.Plugins)), true
	case state.Demo:
		return m.d.Demo, steps.Demo(strings.Split(m.s.SandboxClasses, ",")), true
	case state.RemoveControlPlane:
		return m.d.RemoveControlPlane, steps.RemoveControlPlane(), true
	case state.TeardownAWS:
		return m.d.Teardown, steps.Teardown(), true
	}
	return nil, nil, false
}

// startStep (re)runs the current long step.
func (m *Model) startStep(st state.Step) tea.Cmd {
	build, items, _ := m.longStep(st)
	t := newTask(build(m.s), items)
	m.tasks[st] = t
	return m.run(t)
}

// enter starts whatever the new current step does on arrival.
func (m *Model) enter() tea.Cmd {
	cur := m.m.Current()
	switch cur {
	case state.CheckSetup:
		m.results = nil
		checks := m.d.Checks
		return func() tea.Msg {
			out := make([]doctor.Result, len(checks))
			for i, c := range checks {
				out[i] = c.Run(context.Background())
			}
			return checksMsg(out)
		}
	case state.Account:
		m.idErr = nil
		return func() tea.Msg {
			a, arn, err := m.d.Identity(context.Background())
			return identityMsg{a, arn, err}
		}
	case state.Cluster:
		return m.enterCluster()
	case state.Database:
		m.db.cursor = 0
		if m.s.Database == state.DatabaseBundled {
			m.db.cursor = 1
		}
	case state.Settings:
		m.enterSettings()
	case state.Plugins:
		m.enterPlugins()
	case state.TeardownScope:
		return m.enterScope()
	}
	if _, _, long := m.longStep(cur); long && m.tasks[cur] == nil {
		return m.startStep(cur)
	}
	return nil
}

// jump switches the machine to order and walks it forward to step to. The
// flow is linear, so replaying Next leaves the same history Prev walks back
// through; state.Machine stays substrate-gke's code.
func (m *Model) jump(order []state.Step, to state.Step) {
	m.m.SetOrder(order)
	for m.m.Current() != to {
		before := m.m.Current()
		if m.m.Next() == before {
			return
		}
	}
}

// commit records a choice that shapes the flow (the cluster, the database)
// and moves on. Any step already run belonged to the earlier answers, so
// none is kept.
func (m *Model) commit() tea.Cmd {
	m.tasks = map[state.Step]*task{}
	m.jump(state.InstallOrder(m.s), m.m.Current())
	m.m.Next()
	return m.enter()
}

func (m Model) canAdvance() bool {
	switch cur := m.m.Current(); cur {
	case state.CheckSetup:
		if m.results == nil {
			return false
		}
		for _, r := range m.results {
			if r.Status == doctor.Fail {
				return false
			}
		}
		return true
	case state.Account:
		return m.s.Account != "" && m.idErr == nil
	case state.Complete:
		return false
	default:
		if _, _, long := m.longStep(cur); long {
			t := m.tasks[cur]
			return t != nil && t.done && t.err == nil
		}
	}
	return true
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		m.frame++
		if m.notice != "" && !m.noticeUntil.IsZero() && m.d.Now().After(m.noticeUntil) {
			m.notice, m.noticeUntil = "", time.Time{}
		}
		return m, tick()
	case splashMsg:
		return m.onSplash()
	case checksMsg:
		m.results = msg
	case identityMsg:
		m.s.Account, m.s.Arn, m.idErr = msg.account, msg.arn, msg.err
	case clustersMsg:
		return m, m.clustersLoaded(msg)
	case probeMsg:
		return m, m.probed(msg)
	case describeMsg:
		m.scopeDescribed(msg)
	case execMsg:
		t := msg.t
		if msg.ev.Line != "" {
			t.lines = append(t.lines, maskIDs(msg.ev.Line))
			t.active = steps.Progress(t.items, t.active, msg.ev.Line)
		}
		if !msg.ev.Done {
			return m, listen(t, msg.ch)
		}
		t.done, t.err, t.end = true, msg.ev.Err, time.Now()
		// A cancelled Runner may close its channel without an error (execx
		// drops the final event once its context ends), so the stop decides.
		if t.stopped {
			t.err = errStopped
		}
		if t.cancel != nil {
			t.cancel()
		}
		if t.err == nil {
			t.active = len(t.items)
		}
		if m.runningTask() == nil {
			m.stopAsk = false
		}
		if t == m.cl.removal {
			return m, m.removalDone()
		}
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

// key routes a key: the splash and the stop confirmation take every key,
// then ctrl+c and the snapshot key, then the screen's own.
func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()
	if m.splash {
		if k == "ctrl+c" {
			m.quitting = true
			return tea.Quit
		}
		return m.endSplash() // any other key skips it
	}
	if m.stopAsk {
		t := m.runningTask()
		switch {
		case t == nil:
			m.stopAsk = false
		case k == "y" || k == "ctrl+c":
			m.stopAsk = false
			return m.stopTask(t)
		case k == "n" || k == "esc":
			m.stopAsk = false
		}
		return nil
	}
	if k == "ctrl+c" {
		// A running step is stopped, after a confirmation, rather than left
		// running behind a closed wizard (review F10).
		if t := m.runningTask(); t != nil && !t.stopped {
			m.stopAsk = true
			return nil
		}
		m.quitting = true
		return tea.Quit
	}
	if k == "ctrl+s" || (k == "s" && !m.capturesText()) {
		m.snapshot()
		return nil
	}
	m.notice, m.noticeUntil = "", time.Time{}
	switch m.m.Current() {
	case state.Cluster:
		return m.clusterKey(msg)
	case state.Database:
		return m.databaseKey(msg)
	case state.Settings:
		return m.settingsKey(msg)
	case state.Plugins:
		return m.pluginsKey(msg)
	case state.Review:
		return m.reviewKey(msg)
	case state.TeardownScope:
		return m.scopeKey(msg)
	}
	return m.stepKey(msg)
}

// capturesText reports a screen where s is a typed character: a name being
// typed, a confirmation, or a setting being edited. ctrl+s still snapshots.
func (m Model) capturesText() bool {
	switch m.m.Current() {
	case state.Cluster:
		return m.cl.mode == "name" || m.cl.mode == "remove-confirm"
	case state.TeardownScope:
		return true
	case state.Settings:
		return m.set.editing
	}
	return false
}

// snapshot saves the screen as it is now (snap.Writer) and says where.
func (m *Model) snapshot() {
	m.snapN++
	name := fmt.Sprintf("snap-%s-%02d-%s", m.session, m.snapN, slug(m.m.Current().Title()))
	path, err := snap.Writer{Dir: m.d.SnapshotDir, Title: "substrate-eks installer"}.Save(name, m.View())
	m.notice = "saved snapshot " + tildify(path) + " (.ansi, .html beside it)"
	if err != nil {
		m.notice = "snapshot not saved: " + err.Error()
	}
	m.noticeUntil = m.d.Now().Add(4 * time.Second)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// stepKey handles the keys of every screen without its own list or input.
func (m *Model) stepKey(msg tea.KeyMsg) tea.Cmd {
	cur := m.m.Current()
	t := m.tasks[cur]
	running := t != nil && !t.done
	switch msg.String() {
	case "q":
		if running {
			return nil // q does not abandon a running step; ctrl+c does
		}
		m.quitting = true
		return tea.Quit
	case "esc":
		if !running {
			m.m.Prev()
		}
	case "t":
		if cur == state.Complete && !m.s.Teardown {
			return m.startTeardown()
		}
	case "enter":
		// A failed long step is rerun in place: every step is idempotent.
		if t != nil && t.done && t.err != nil {
			return m.startStep(cur)
		}
		// A failed read is rerun too, after the user fixes what it names.
		if (cur == state.Account && m.idErr != nil) || (cur == state.CheckSetup && m.results != nil && !m.canAdvance()) {
			return m.enter()
		}
		if !m.canAdvance() {
			return nil
		}
		m.m.Next()
		return m.enter()
	}
	return nil
}

// startTeardown leaves the install's Complete screen for the teardown flow of
// the cluster just installed; esc on the choice comes back.
func (m *Model) startTeardown() tea.Cmd {
	m.s.Teardown, m.scope.fromComplete = true, true
	m.jump(state.TeardownOrder, state.TeardownScope)
	return m.enter()
}

// View renders the header, the rail, the current screen and the key hints.
// Every 12-digit number on it is masked to its last four digits, so a
// screenshot never carries an account id, whatever a command printed (issue
// #5 AC10.4); --redact masks the rest of each id, ARNs and bucket names too.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if m.splash {
		return m.splashView()
	}
	head := m.header()
	foot := m.hints()
	out := head + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, m.rail(), "  ", m.body(m.bodyHeight(head, foot))) + "\n" + foot
	return m.redact(maskIDs(out))
}

// bodyHeight is the rows the body panel may take, borders included, or 0
// when the terminal size is not known yet.
func (m Model) bodyHeight(head, foot string) int {
	if m.h <= 0 {
		return 0
	}
	return max(m.h-lipgloss.Height(head)-lipgloss.Height(foot), 5)
}

func (m Model) rail() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Substrate on EKS") + "\n\n")
	cur := m.m.Current()
	at, _ := m.m.Position(cur)
	if cur == state.Complete {
		at = len(m.m.Order())
	}
	for i, s := range m.m.Order() {
		n, numbered := m.m.Position(s)
		label := s.Title()
		if numbered {
			label = fmt.Sprintf("%d. %s", n, label)
		}
		switch {
		case s == cur:
			b.WriteString(theme.Selected.Render(theme.GlyphActive+" "+label) + "\n")
		case i < at:
			b.WriteString(theme.Good.Render(theme.GlyphDone) + " " + theme.Subtle.Render(label) + "\n")
		default:
			b.WriteString(theme.Fainted.Render(theme.GlyphPending+" "+label) + "\n")
		}
	}
	return theme.Panel.Width(30).Render(strings.TrimRight(b.String(), "\n"))
}

func (m Model) spinner() string { return theme.SpinnerFrames[m.frame%len(theme.SpinnerFrames)] }

func (m Model) bodyWidth() int {
	if m.w > 40 {
		return m.w - 36
	}
	return 80
}

// body is the current screen in its panel, h rows tall at most (0: as tall
// as it is). A screen taller than that keeps its top, as substrate-gke's
// clampHeight does; the stop confirmation is drawn first so it is never cut.
func (m Model) body(h int) string {
	var b strings.Builder
	cur := m.m.Current()
	if m.stopAsk {
		if t := m.runningTask(); t != nil {
			b.WriteString(theme.Warning.Render("Stop "+t.spec.Label+"?") + "\n")
			b.WriteString(theme.Subtle.Render("This ends the command and every process it started. The step is marked") + "\n")
			b.WriteString(theme.Subtle.Render("stopped; enter runs it again (every step is idempotent).") + "\n")
			b.WriteString(theme.Key.Render("y") + " stop it   " + theme.Key.Render("n") + " keep it running\n\n")
		}
	}
	switch cur {
	case state.Settings:
		b.WriteString(m.settingsView())
	case state.Plugins:
		b.WriteString(m.pluginsView())
	case state.Review:
		b.WriteString(m.reviewView(max(h-2, 0)))
	case state.InstallPlugins:
		b.WriteString(theme.Title.Render("Install the plugins") + "\n")
		b.WriteString(theme.Subtle.Render("Deploy and check "+strings.Join(pluginTitles(resolved(m.s.Plugins)), ", ")) + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.Welcome:
		b.WriteString(theme.Logo() + "\n\n")
		if m.s.Teardown {
			b.WriteString(theme.Title.Render("Tear down Agent Substrate on "+m.s.ClusterName) + "\n\n")
			b.WriteString("This wizard removes Substrate from the cluster with ate-setup, then deletes\n")
			b.WriteString("the AWS resources this deployment created, and the cluster if you choose.\n\n")
			b.WriteString(theme.Subtle.Render("Only resources tagged agent-substrate-on-eks/deployment="+m.s.ClusterName+"."+m.s.Region+" are deleted.") + "\n")
		} else {
			b.WriteString(theme.Title.Render("Agent Substrate on Amazon EKS") + "\n\n")
			b.WriteString("This wizard creates an EKS cluster or connects an existing one, provisions\n")
			b.WriteString("the AWS resources Substrate uses, and installs its control plane.\n\n")
			b.WriteString(theme.Subtle.Render("Every step is idempotent. Nothing is created until you start the install on the Review screen, which shows every command, its time and the cost.") + "\n")
		}
		if m.d.DryRun {
			b.WriteString("\n" + theme.Warning.Render("--dry-run: commands are replayed; nothing touches AWS.") + "\n")
		}
	case state.CheckSetup:
		b.WriteString(theme.Title.Render("Check your setup") + "\n\n")
		if m.results == nil {
			b.WriteString(m.spinner() + " probing…\n")
			break
		}
		for i, c := range m.d.Checks {
			r := m.results[i]
			g := theme.Good.Render(theme.GlyphDone)
			switch r.Status {
			case doctor.Warn:
				g = theme.Warning.Render(theme.GlyphWarn)
			case doctor.Fail:
				g = theme.Bad.Render(theme.GlyphFail)
			}
			fmt.Fprintf(&b, "%s %-16s %s\n", g, c.Name, theme.Subtle.Render(truncate(r.Detail, m.bodyWidth()-22)))
			if r.Fix != "" {
				b.WriteString("    " + theme.Warning.Render("fix: "+r.Fix) + "\n")
			}
		}
	case state.Account:
		b.WriteString(theme.Title.Render("Choose AWS account") + "\n\n")
		profile := m.s.Profile
		if profile == "" {
			profile = "(default credential chain)"
		}
		fmt.Fprintf(&b, "Profile  %s\nRegion   %s\n", profile, m.s.Region)
		switch {
		case m.idErr != nil:
			b.WriteString("\n" + theme.Bad.Render(m.idErr.Error()) + "\n")
		case m.s.Account == "":
			b.WriteString("\n" + m.spinner() + " sts:GetCallerIdentity…\n")
		default:
			fmt.Fprintf(&b, "Account  %s\nCaller   %s\n", maskAccount(m.s.Account), theme.Subtle.Render(maskArn(m.s.Arn)))
		}
	case state.Cluster:
		b.WriteString(m.clusterView())
	case state.Database:
		b.WriteString(m.databaseView())
	case state.TeardownScope:
		b.WriteString(m.scopeView())
	case state.CreateCluster:
		b.WriteString(theme.Title.Render("Create the cluster") + "\n")
		b.WriteString(theme.Subtle.Render("VPC, IAM roles, EKS cluster "+m.s.ClusterName+" and its system, gvisor and microvm node groups") + "\n")
		b.WriteString(theme.Subtle.Render(m.s.NewClusterConfig.Summary()) + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.Provision:
		b.WriteString(theme.Title.Render("Provision AWS resources") + "\n")
		b.WriteString(theme.Subtle.Render("S3 buckets, EKS Pod Identity roles, ECR repositories for "+m.s.ClusterName) + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.RDS:
		b.WriteString(theme.Title.Render("Create the database") + "\n")
		b.WriteString(theme.Subtle.Render("A private RDS for PostgreSQL instance in the cluster's VPC; its logins in Secrets Manager") + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.ControlPlane:
		b.WriteString(theme.Title.Render("Turn on Substrate") + "\n")
		b.WriteString(theme.Subtle.Render("ate-setup deploy ate-system "+m.s.SubstrateTag+" · sandbox classes "+m.s.SandboxClasses+" · database "+m.s.Database) + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.Demo:
		b.WriteString(theme.Title.Render("Deploy the demo") + "\n")
		b.WriteString(theme.Subtle.Render("The counter demo on "+m.s.SandboxClasses+", then a suspend and resume check on each") + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.RemoveControlPlane:
		b.WriteString(theme.Title.Render("Remove Substrate") + "\n")
		b.WriteString(theme.Subtle.Render("ate-setup delete ate-system on "+m.s.ClusterName+": every actor, template and the control plane") + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.TeardownAWS:
		b.WriteString(theme.Title.Render("Delete AWS resources") + "\n")
		what := "Everything tagged agent-substrate-on-eks/deployment=" + m.s.ClusterName + "." + m.s.Region
		if m.s.KeepCluster {
			what += ", except the cluster and its network"
		}
		b.WriteString(theme.Subtle.Render(what) + "\n\n")
		b.WriteString(m.taskView(m.tasks[cur]))
	case state.Complete:
		b.WriteString(m.completeView())
	}
	content := strings.TrimRight(b.String(), "\n")
	if h > 2 {
		// Wrapped first, so a long line counts as the rows it takes.
		inner := lipgloss.NewStyle().Width(m.bodyWidth() - 2).Render(content)
		if lines := strings.Split(inner, "\n"); len(lines) > h-2 {
			content = strings.Join(lines[:h-2], "\n")
		}
	}
	return theme.AccentPanel.Width(m.bodyWidth()).Render(content)
}

// completeView is the end of either flow. The install's prints the teardown
// as the exact commands [t] runs (issue #5 AC6.4).
func (m Model) completeView() string {
	var b strings.Builder
	cmd := func(s string) { b.WriteString(theme.CommandLine.Render("  "+s) + "\n") }
	if m.s.Teardown {
		if m.s.KeepCluster {
			b.WriteString(theme.Good.Render(theme.GlyphDone+" Removed Substrate and its AWS resources; cluster "+m.s.ClusterName+" is kept") + "\n\n")
		} else {
			b.WriteString(theme.Good.Render(theme.GlyphDone+" Deleted cluster "+m.s.ClusterName+" and every AWS resource this deployment created") + "\n\n")
		}
		b.WriteString("Check that nothing with this deployment's owner tag is left:\n")
		cmd(m.d.Inventory(m.s).Display)
	} else {
		b.WriteString(theme.Good.Render(theme.GlyphDone+" Agent Substrate is running on "+m.s.ClusterName) + "\n\n")
		b.WriteString("Try it:\n")
		// The installer's own kubeconfig (mode 0600, the chosen profile, no
		// token); ~/.kube/config is never read or written.
		cmd("export KUBECONFIG=" + homeTilde(awsx.KubeconfigPath(m.s.ClusterName, m.s.Region)))
		cmd("kubectl -n ate-system get pods")
		if m.s.DemoMode {
			b.WriteString("Check the demo again any time:\n")
			cmd(m.d.Verify(m.s).Display)
		}
		b.WriteString(theme.Subtle.Render("Everything is ClusterIP; reach it with kubectl port-forward or kubectl ate proxy.") + "\n\n")
		b.WriteString("Tear it down: press " + theme.Key.Render("t") + ", or run these in order (printed again on exit):\n")
		for _, c := range m.teardownCommands() {
			cmd(c)
		}
		if m.s.ClusterOwned {
			b.WriteString(theme.Subtle.Render("Without --keep-cluster it also deletes cluster "+m.s.ClusterName+", which this installer created.") + "\n")
		}
	}
	if m.d.LogPath != "" {
		b.WriteString("\n" + theme.Subtle.Render("Log: "+tildify(m.d.LogPath)) + "\n")
	}
	return b.String()
}

// teardownCommands are what [t] runs for the cluster just installed, keeping
// the cluster, as commands to paste.
func (m Model) teardownCommands() []string {
	keep := *m.s
	keep.KeepCluster = true
	return []string{m.d.RemoveControlPlane(m.s).Display, m.d.Teardown(&keep).Display}
}

// ExitNotes are lines to print once the wizard has exited from Complete: the
// commands the screen showed, without the panel's border and wrapping, so
// they can be copied.
func (m Model) ExitNotes() []string {
	if m.m.Current() != state.Complete {
		return nil
	}
	var out []string
	if m.s.Teardown {
		out = []string{"Check that nothing with this deployment's owner tag is left:", "  " + m.d.Inventory(m.s).Display}
	} else {
		out = []string{"Agent Substrate is running on " + m.s.ClusterName + ".",
			"  export KUBECONFIG=" + awsx.KubeconfigPath(m.s.ClusterName, m.s.Region),
			"To tear it down, keeping the cluster, run these in order:"}
		for _, c := range m.teardownCommands() {
			out = append(out, "  "+c)
		}
		if m.s.ClusterOwned {
			out = append(out, "Without --keep-cluster, setup-aws teardown also deletes cluster "+m.s.ClusterName+".")
		}
	}
	for i := range out {
		out[i] = m.redact(maskIDs(out[i]))
	}
	return out
}

func (m Model) taskView(t *task) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	shown := t.spec.Display
	if !t.spec.DisplayIsSummary {
		shown = "$ " + shown
	}
	b.WriteString(theme.CommandLine.Render(shown) + "\n\n")
	for i, it := range t.items {
		switch {
		case i < t.active || (t.done && t.err == nil):
			b.WriteString(theme.Good.Render(theme.GlyphDone) + " " + it.Label + "\n")
		case i == t.active && t.err != nil:
			b.WriteString(theme.Bad.Render(theme.GlyphFail) + " " + it.Label + "\n")
		case i == t.active:
			b.WriteString(theme.Accent.Render(m.spinner()) + " " + it.Label + "\n")
		default:
			b.WriteString(theme.Fainted.Render(theme.GlyphPending+" "+it.Label) + "\n")
		}
	}
	// The outcome comes before the output's tail, so a screen too short for
	// both still shows it (the body keeps its top).
	switch {
	case t.stopped && !t.done:
		b.WriteString("\n" + theme.Warning.Render(m.spinner()+" Stopping: ending the command and every process it started…") + "\n")
	case errors.Is(t.err, errStopped):
		b.WriteString("\n" + theme.Warning.Render("Stopped with ctrl+c: the command and every process it started were ended · enter to run it again") + "\n")
	case t.err != nil:
		b.WriteString("\n" + theme.Bad.Render("Failed: "+t.err.Error()+" · enter to retry") + "\n")
		if m.d.LogPath != "" {
			b.WriteString(theme.Subtle.Render("Full output: "+tildify(m.d.LogPath)) + "\n")
		}
	case t.done:
		b.WriteString("\n" + theme.Good.Render(fmt.Sprintf("Done in %s", t.end.Sub(t.start).Round(time.Second))) + "\n")
	}
	b.WriteString("\n")
	tail := t.lines
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	for _, l := range tail {
		b.WriteString(theme.Fainted.Render(truncate(l, m.bodyWidth()-4)) + "\n")
	}
	return b.String()
}

func (m Model) hints() string {
	var h []string
	cur := m.m.Current()
	switch {
	case m.stopAsk:
		h = []string{"y stop the step", "n keep it running"}
	case m.runningTask() != nil:
		h = []string{"ctrl+c stop this step"}
	case cur == state.Cluster:
		h = m.clusterHints()
	case cur == state.Database:
		h = []string{"↑/↓ select", "enter confirm", "esc back", "ctrl+c quit"}
	case cur == state.Settings:
		h = m.settingsHints()
	case cur == state.Plugins:
		h = []string{"↑/↓ move", "space select", "enter continue", "esc back", "ctrl+c quit"}
	case cur == state.Review:
		h = []string{"y start the install", "esc back", "ctrl+c quit"}
	case cur == state.TeardownScope:
		h = m.scopeHints()
	case cur == state.Complete:
		h = []string{"q quit"}
		if !m.s.Teardown {
			h = []string{"t tear down", "q quit"}
		}
	default:
		h = []string{"enter continue", "esc back", "ctrl+c quit"}
		if t := m.tasks[cur]; t != nil && t.done && t.err != nil {
			h = []string{"enter retry", "esc back", "ctrl+c quit"}
		}
	}
	if !m.stopAsk {
		if m.capturesText() {
			h = append(h, "ctrl+s snapshot")
		} else {
			h = append(h, "s snapshot")
		}
	}
	keys := strings.Join(h, " · ")
	line := theme.Subtle.Render("  " + keys)
	if m.notice != "" {
		// The keys always fit: a long notice is shortened, not the keys
		// after it (a notice cut the review's "ctrl+c quit" in half).
		notice := m.notice
		if room := max(m.width(), 40) - lipgloss.Width(keys) - 7; lipgloss.Width(notice) > room {
			notice = ""
			if room >= 12 {
				notice = truncate(m.notice, room)
			}
		}
		if notice != "" {
			line = "  " + theme.Warning.Render(notice) + theme.Fainted.Render("  ·  ") + theme.Subtle.Render(keys)
		}
	}
	return lipgloss.NewStyle().MaxWidth(max(m.width(), 40)).Render(line)
}

// width is the terminal's, or the width the screens assume before the first
// WindowSizeMsg (bodyWidth's 80 plus the rail).
func (m Model) width() int {
	if m.w > 40 {
		return m.w
	}
	return m.bodyWidth() + 36
}

func truncate(s string, n int) string {
	if n < 4 || len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// maskAccount shows only the last four digits, so a screenshot of the wizard
// never carries an account id.
func maskAccount(a string) string {
	if len(a) < 4 {
		return a
	}
	return strings.Repeat("•", len(a)-4) + a[len(a)-4:]
}

func maskArn(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) > 4 {
		parts[4] = maskAccount(parts[4])
	}
	return strings.Join(parts, ":")
}

// accountID is an AWS account id as it appears in output: a bucket name, an
// ARN, an ECR registry host.
var accountID = regexp.MustCompile(`\b[0-9]{12}\b`)

// maskIDs masks every account id in s.
func maskIDs(s string) string { return accountID.ReplaceAllStringFunc(s, maskAccount) }

// edit applies a key to a one-line text field: typed characters append and
// backspace deletes. It is all the name and confirmation prompts need.
func edit(s string, k tea.KeyMsg) string {
	switch k.Type {
	case tea.KeyBackspace:
		if r := []rune(s); len(r) > 0 {
			return string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		if len(s) < 40 {
			return s + string(k.Runes)
		}
	}
	return s
}

// editN is edit for a Settings value: up to n bytes, spaces included (TAGS
// and the CIDRs are space-separated lists).
func editN(s string, k tea.KeyMsg, n int) string {
	if k.Type == tea.KeySpace && len(s) < n {
		return s + " "
	}
	if k.Type == tea.KeyRunes && len(s) < n {
		return s + string(k.Runes)
	}
	return edit(s, tea.KeyMsg{Type: k.Type})
}
