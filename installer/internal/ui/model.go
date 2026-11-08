// Package ui is the installer's full-screen terminal interface: Welcome,
// Check your setup, Settings, Review, Provision and Done.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/config"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/preflight"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/run"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/snap"
)

type screen int

const (
	sWelcome screen = iota
	sCheck
	sSettings
	sReview
	sProvision
	sDone
	sTeardown // the "type the cluster name" confirm
	sSplash   // the opening animation, before Welcome
)

var rail = []string{"Welcome", "Check your setup", "Settings", "Review", "Provision", "Done"}

// Options configure a session.
type Options struct {
	Repo config.Repo
	// RepoRev is the checkout's short commit, for the Welcome screen.
	RepoRev string
	// Splash plays the opening animation before Welcome (not with Last).
	Splash bool
	// Seed overrides the defaults (flags --cluster, --region, --profile).
	Seed map[string]string
	// Demos, when set, replaces the default demo choices.
	Demos map[string]bool
	// Auto walks the screens on its own, a few seconds each, through to
	// Done (for --dry-run --auto). It never starts a teardown.
	Auto     bool
	AutoQuit bool
	// DryRun replays Record instead of running anything.
	DryRun bool
	Replay *run.Replay
	// Checks, when set, replace the live preflight checks (dry run, tests).
	Checks []preflight.Result
	// SnapshotDir: write each screen there (ANSI + HTML), at SnapW x SnapH.
	SnapshotDir  string
	SnapW, SnapH int
	// Redact masks the AWS account id on screen and in snapshots.
	Redact bool
	// Now is the clock (tests); default time.Now.
	Now func() time.Time
	// Last, when set, opens on the Done screen of that finished run (the
	// record of a real install in the checkout's .work/installer/), so the
	// summary and Teardown are there again after the installer was closed.
	Last *LastRun
}

// LastRun is a recorded install to reopen.
type LastRun struct {
	Header  run.Header
	Events  []run.Event
	LogPath string
}

// Model is the Bubble Tea model.
type Model struct {
	opt    Options
	screen screen
	w, h   int

	values  map[string]string
	demos   map[string]bool
	unavail map[string]string // choices this host cannot run, and why
	fields  []field
	cursor  int
	edit    bool
	input   textinput.Model
	errs    map[string]string
	msg     string
	// msgUntil, when set, clears msg after that time (short notices).
	msgUntil time.Time

	checks  []preflight.Result
	pending int
	account string

	localText string
	cmds      []plan.Command

	groups    []*plan.Group
	byID      map[string]int
	events    chan run.Event
	cancel    context.CancelFunc
	running   bool
	teardown  bool
	runStart  time.Time
	runEnd    time.Time
	lastEvent time.Time
	exitCode  int
	logLines  []string
	scroll    int // lines up from the bottom of the log; 0 follows
	logPath   string
	record    string
	quitArmed bool

	confirm textinput.Model
	spin    spinner.Model
	snaps   snap.Writer
	snapN   int
	snapped map[string]bool
	autoN   int
	frame   int // of the splash
}

// New builds the model.
func New(opt Options) Model {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.SnapW == 0 {
		opt.SnapW, opt.SnapH = 126, 33
	}
	m := Model{opt: opt, values: map[string]string{}, demos: map[string]bool{}, errs: map[string]string{}, snapped: map[string]bool{}}
	for _, k := range config.Managed {
		m.values[k] = opt.Repo.Effective(k)
	}
	for k, v := range opt.Seed {
		if v != "" {
			m.values[k] = v
		}
	}
	m.unavail = map[string]string{}
	if !opt.DryRun {
		if dir := credproviderDir(); !isCredproviderCheckout(dir) {
			m.unavail["credprovider-aws"] = "no credprovider-aws-secret-manager checkout at " + dir + "; clone it there or set CREDPROVIDER_DIR"
		}
	}
	for _, d := range plan.Demos {
		d = m.demo(d.ID)
		m.demos[d.ID] = d.Default && d.Disabled == ""
		if d.ID == "openclaw" {
			// deploy.sh deploys OpenClaw when the config names a checkout.
			m.demos[d.ID] = m.values["OPENCLAW_DIR"] != "" || m.values["OPENCLAW_REPO"] != ""
		}
		if d.ID == "mode3" {
			m.demos[d.ID] = m.values["MODE3"] == "on"
		}
		if d.ID == "mode3-iso" {
			m.demos[d.ID] = m.values["MODE3_ISO"] == "on"
		}
		if opt.Demos != nil && d.Disabled == "" && !d.Locked {
			m.demos[d.ID] = opt.Demos[d.ID]
		}
	}
	m.fields = settingsFields()
	m.input = textinput.New()
	m.input.Prompt = ""
	m.input.CharLimit = 200
	m.confirm = textinput.New()
	m.confirm.Prompt = "› "
	m.confirm.CharLimit = 40
	m.spin = spinner.New(spinner.WithSpinner(spinner.Spinner{
		Frames: []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}, FPS: time.Second / 10}), spinner.WithStyle(sOrange))
	m.snaps = snap.Writer{Dir: opt.SnapshotDir, Title: "eks-talk installer — " + filepath.Base(opt.Repo.Dir)}
	m.w, m.h = opt.SnapW, opt.SnapH
	if opt.Last != nil {
		m.loadLast(*opt.Last)
	} else if opt.Splash {
		m.screen = sSplash
	}
	return m
}

// loadLast rebuilds the checklist of a recorded run and shows its end.
func (m *Model) loadLast(l LastRun) {
	_ = json.Unmarshal(l.Header.Checks, &m.checks)
	for _, c := range m.checks {
		if c.ID == "identity" {
			if f := strings.Fields(c.Detail); len(f) > 1 {
				m.account = strings.TrimSuffix(f[1], ",")
			}
		}
	}
	m.groups, m.byID = nil, map[string]int{}
	m.runStart, m.logPath = l.Header.Started, l.LogPath
	m.runEnd = m.runStart
	for _, ev := range l.Events {
		t := l.Header.Started.Add(time.Duration(ev.Ms) * time.Millisecond)
		m.runEnd, m.lastEvent = t, t
		g := m.group(ev.ID)
		switch ev.Kind {
		case run.Start:
			g.Begin(t)
			m.addLog("$ " + ev.Display)
		case run.Line:
			g.Line(t, ev.Text)
			m.addLog(plan.StripANSI(ev.Text))
		case run.Exit:
			g.Exit(t, ev.Code)
		case run.End:
			m.exitCode = ev.Code
		}
	}
	m.screen = sDone
	if m.exitCode != 0 {
		m.screen = sProvision
	}
}

func (m Model) now() time.Time {
	if m.opt.DryRun && !m.lastEvent.IsZero() {
		return m.lastEvent
	}
	return m.opt.Now()
}

type tickMsg time.Time
type checkMsg struct {
	i   int
	res preflight.Result
}
type eventMsg run.Event
type snapMsg string

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	if m.opt.Last != nil {
		return tea.Batch(m.spin.Tick, tick(), m.snapLater("06-done"))
	}
	if m.screen == sSplash {
		return tea.Batch(m.spin.Tick, tick(), splashTick())
	}
	return tea.Batch(m.spin.Tick, tick(), m.snapLater("01-welcome"))
}

// snapLater saves a snapshot of the current screen on the next update, after
// the screen has drawn its latest state.
func (m Model) snapLater(name string) tea.Cmd {
	if m.opt.SnapshotDir == "" {
		return nil
	}
	return func() tea.Msg { return snapMsg(name) }
}

func (m *Model) saveSnap(name string) {
	if m.opt.SnapshotDir == "" {
		return
	}
	view := m.render(m.opt.SnapW, m.opt.SnapH)
	if err := m.snaps.Save(name, view); err != nil {
		m.msg = "snapshot: " + err.Error()
	}
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case spinner.TickMsg:
		var c tea.Cmd
		m.spin, c = m.spin.Update(msg)
		return m, c
	case tickMsg:
		if m.msg != "" && !m.msgUntil.IsZero() && m.opt.Now().After(m.msgUntil) {
			m.msg, m.msgUntil = "", time.Time{}
		}
		if m.opt.Auto {
			return m.auto()
		}
		return m, tick()
	case snapMsg:
		m.saveSnap(string(msg))
		return m, nil
	case splashMsg:
		return m.onSplash()
	case checkMsg:
		m.checks[msg.i] = msg.res
		m.pending--
		if msg.res.ID == "identity" && msg.res.Status == preflight.Pass {
			if f := strings.Fields(msg.res.Detail); len(f) > 1 {
				m.account = strings.TrimSuffix(f[1], ",")
			}
		}
		if m.pending == 0 {
			return m, m.snapLater("02-check")
		}
		return m, nil
	case eventMsg:
		return m.onEvent(run.Event(msg))
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m Model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		if m.running && !m.quitArmed {
			m.quitArmed = true
			m.msg = "A run is in progress. Press ctrl+c again to stop it; the scripts are idempotent, so running the installer again resumes."
			return m, nil
		}
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	}
	if m.screen == sSplash {
		return m.endSplash() // any key skips it
	}
	if key == "ctrl+s" || (key == "s" && !m.edit && m.screen != sTeardown) {
		m.snapN++
		name := fmt.Sprintf("snap-%02d-%s", m.snapN, strings.ToLower(strings.ReplaceAll(rail[min(int(m.screen), len(rail)-1)], " ", "-")))
		m.saveSnap(name)
		m.msg = "saved snapshot " + name
		if m.opt.SnapshotDir == "" {
			m.msg = "set INSTALLER_SNAPSHOT_DIR to save snapshots"
		}
		m.msgUntil = m.opt.Now().Add(3 * time.Second)
		return m, nil
	}
	m.msg, m.msgUntil = "", time.Time{}
	switch m.screen {
	case sWelcome:
		switch key {
		case "enter":
			return m.toCheck()
		case "q", "esc":
			return m, tea.Quit
		}
	case sCheck:
		switch key {
		case "enter", "n":
			if m.pending > 0 {
				m.msg = "the checks are still running"
				return m, nil
			}
			if m.failedChecks() > 0 {
				m.msg = "fix the failed checks, then press r to check again"
				return m, nil
			}
			m.screen = sSettings
			return m, m.snapLater("03-settings")
		case "r":
			return m.toCheck()
		case "b", "esc":
			m.screen = sWelcome
		case "q":
			return m, tea.Quit
		}
	case sSettings:
		return m.settingsKey(k)
	case sReview:
		switch key {
		case "y":
			return m.start(false)
		case "b", "esc":
			m.screen = sSettings
		case "q":
			return m, tea.Quit
		}
	case sProvision:
		switch key {
		case "up", "k":
			m.scroll++
		case "down", "j":
			m.scroll = max(0, m.scroll-1)
		case "pgup":
			m.scroll += 10
		case "pgdown":
			m.scroll = max(0, m.scroll-10)
		case "end", "f":
			m.scroll = 0
		case "enter":
			if !m.running && m.exitCode == 0 && !m.teardown {
				m.screen = sDone
				return m, m.snapLater("06-done")
			}
		case "r":
			if !m.running && m.exitCode != 0 {
				return m.start(m.teardown)
			}
		case "q":
			if !m.running {
				return m, tea.Quit
			}
			m.msg = "a run is in progress; ctrl+c twice stops it"
		}
	case sDone:
		switch key {
		case "t":
			if m.opt.DryRun {
				m.msg = "dry run: teardown runs only against a real install"
				return m, nil
			}
			m.screen = sTeardown
			m.confirm.SetValue("")
			m.confirm.Focus()
			return m, textinput.Blink
		case "l":
			m.screen = sProvision
		case "q", "esc":
			return m, tea.Quit
		}
	case sTeardown:
		switch key {
		case "esc":
			m.screen = sDone
			m.confirm.Blur()
			return m, nil
		case "enter":
			if strings.TrimSpace(m.confirm.Value()) != m.values["CLUSTER_NAME"] {
				m.msg = "the name does not match; nothing was deleted"
				return m, nil
			}
			m.confirm.Blur()
			return m.start(true)
		}
		var c tea.Cmd
		m.confirm, c = m.confirm.Update(k)
		return m, c
	}
	return m, nil
}

// auto advances one screen every few ticks.
func (m Model) auto() (tea.Model, tea.Cmd) {
	m.autoN++
	if m.autoN < 3 {
		return m, tick()
	}
	var next tea.Model = m
	var c tea.Cmd
	switch m.screen {
	case sWelcome:
		next, c = m.toCheck()
	case sCheck:
		if m.pending > 0 || m.failedChecks() > 0 {
			return m, tick()
		}
		m.screen = sSettings
		next, c = m, m.snapLater("03-settings")
	case sSettings:
		next, c = m.toReview()
	case sReview:
		next, c = m.start(false)
	case sDone:
		if m.opt.AutoQuit {
			return m, tea.Quit
		}
		return m, tick()
	default:
		return m, tick()
	}
	nm := next.(Model)
	nm.autoN = 0
	return nm, tea.Batch(c, tick())
}

// toCheck starts the preflight checks, all at once.
func (m Model) toCheck() (tea.Model, tea.Cmd) {
	m.screen = sCheck
	in := m.checkInput()
	if m.opt.Checks != nil {
		m.checks = append([]preflight.Result{}, m.opt.Checks...)
		m.pending = 0
		for _, r := range m.checks {
			if r.ID == "identity" {
				if f := strings.Fields(r.Detail); len(f) > 1 {
					m.account = strings.TrimSuffix(f[1], ",")
				}
			}
		}
		return m, m.snapLater("02-check")
	}
	checks := preflight.Checks(preflight.Exec)
	m.checks = make([]preflight.Result, len(checks))
	m.pending = len(checks)
	var cmds []tea.Cmd
	for i, c := range checks {
		m.checks[i] = preflight.Result{ID: c.ID, Group: c.Group, Name: c.Name, Status: preflight.Running}
		i, c := i, c
		cmds = append(cmds, func() tea.Msg { return checkMsg{i, c.Run(context.Background(), in)} })
	}
	return m, tea.Batch(cmds...)
}

func (m Model) checkInput() preflight.Input {
	n := func(k string) int { v, _ := strconv.Atoi(m.values[k]); return v }
	return preflight.Input{
		Repo: m.opt.Repo.Dir, Profile: m.values["AWS_PROFILE"], Region: m.values["AWS_REGION"], Cluster: m.values["CLUSTER_NAME"],
		Groups: []preflight.NodeGroup{
			{Name: "system", Type: m.values["SYSTEM_INSTANCE_TYPE"], Nodes: n("SYSTEM_NODES"), Max: n("SYSTEM_NODES") + 1},
			{Name: "gvisor", Type: m.values["GVISOR_INSTANCE_TYPE"], Nodes: n("GVISOR_NODES"), Max: n("GVISOR_NODES") + 2},
			{Name: "microvm", Type: m.values["MICROVM_INSTANCE_TYPE"], Nodes: n("MICROVM_NODES"), Max: n("MICROVM_NODES") + 2},
		},
	}
}

func (m Model) failedChecks() int {
	n := 0
	for _, c := range m.checks {
		if c.Status == preflight.Fail {
			n++
		}
	}
	return n
}

// toReview renders the config.local.env text and the command list.
func (m Model) toReview() (tea.Model, tea.Cmd) {
	m.errs = config.Validate(m.values)
	if len(m.errs) > 0 {
		m.msg = fmt.Sprintf("%d setting(s) need a fix", len(m.errs))
		return m, nil
	}
	header := []string{
		"Written by the eks-talk installer.",
		"lib/common.sh reads it after config.env.",
		"Set here: what differs from config.env,",
		"plus the cluster, Region and profile.",
	}
	text, err := config.Render(m.opt.Repo.Defaults, m.opt.Repo.Local, m.values, header)
	if err != nil {
		m.msg = err.Error()
		return m, nil
	}
	m.localText = text
	m.cmds = plan.Install(m.demos)
	m.screen = sReview
	return m, m.snapLater("04-review")
}

// start writes config.local.env (not in a dry run) and runs the plan, or the
// teardown.
func (m Model) start(teardown bool) (tea.Model, tea.Cmd) {
	cmds := m.cmds
	if teardown {
		cmds = plan.Teardown()
	}
	stamp := m.opt.Now().In(et).Format("20060102-150405")
	work := filepath.Join(m.opt.Repo.Dir, ".work", "installer")
	if !m.opt.DryRun {
		if !teardown {
			backup := filepath.Join(work, "config.local.env.bak-"+stamp)
			if err := config.Write(m.opt.Repo.LocalPath(), m.localText, backup); err != nil {
				m.msg = "cannot write config.local.env: " + err.Error()
				return m, nil
			}
		}
		if err := os.MkdirAll(work, 0o700); err != nil {
			m.msg = err.Error()
			return m, nil
		}
		kind := "install"
		if teardown {
			kind = "teardown"
		}
		m.logPath = filepath.Join(work, kind+"-"+stamp+".log")
		m.record = filepath.Join(work, kind+"-"+stamp+".jsonl")
	} else {
		m.logPath = "(dry run: nothing is written)"
	}
	m.teardown = teardown
	m.groups, m.byID = nil, map[string]int{}
	for i, c := range cmds {
		m.groups = append(m.groups, plan.NewGroup(c))
		m.byID[c.ID] = i
	}
	m.logLines, m.scroll, m.exitCode = nil, 0, 0
	m.running, m.quitArmed = true, false
	m.runStart, m.runEnd, m.lastEvent = m.opt.Now(), time.Time{}, time.Time{}
	m.screen = sProvision
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.events = make(chan run.Event, 256)
	var r run.Runner
	if m.opt.DryRun {
		r = m.opt.Replay
	} else {
		checks, _ := json.Marshal(m.checks)
		var demos []string
		for _, d := range plan.Demos {
			if m.demos[d.ID] {
				demos = append(demos, d.ID)
			}
		}
		r = &run.Exec{Dir: m.opt.Repo.Dir, LogPath: m.logPath, Record: m.record,
			Header: run.Header{Cluster: m.values["CLUSTER_NAME"], Region: m.values["AWS_REGION"],
				Values: m.values, Demos: demos, Checks: checks}}
	}
	ch := m.events
	go r.Run(ctx, cmds, ch)
	return m, waitEvent(ch)
}

func waitEvent(ch chan run.Event) tea.Cmd {
	return func() tea.Msg { return eventMsg(<-ch) }
}

const maxLog = 4000

func (m Model) onEvent(ev run.Event) (tea.Model, tea.Cmd) {
	t := ev.Time
	if t.IsZero() {
		t = m.opt.Now()
	}
	m.lastEvent = t
	if m.opt.DryRun && m.runEnd.IsZero() && len(m.logLines) == 0 && ev.Kind == run.Start {
		m.runStart = t.Add(-time.Duration(ev.Ms) * time.Millisecond)
	}
	var cmds []tea.Cmd
	g := m.group(ev.ID)
	switch ev.Kind {
	case run.Start:
		if g != nil {
			g.Begin(t)
		}
		m.addLog("$ " + ev.Display)
	case run.Line:
		if g != nil {
			before := g.Current()
			g.Line(t, ev.Text)
			if after := g.Current(); after != nil && after != before {
				m.snapN++
				cmds = append(cmds, m.snapLater(fmt.Sprintf("05-provision-%03d", m.snapN)))
			}
		}
		m.addLog(plan.StripANSI(ev.Text))
	case run.Exit:
		if g != nil {
			g.Exit(t, ev.Code)
		}
		if ev.Code != 0 {
			m.addLog(fmt.Sprintf("exit %d", ev.Code))
		}
	case run.End:
		m.running = false
		m.runEnd = t
		m.exitCode = ev.Code
		m.cancel = nil
		if ev.Code != 0 {
			cmds = append(cmds, m.snapLater("05-provision-failed"))
			return m, tea.Batch(cmds...)
		}
		if m.teardown {
			cmds = append(cmds, m.snapLater("07-teardown-done"))
			return m, tea.Batch(cmds...)
		}
		m.screen = sDone
		cmds = append(cmds, m.snapLater("05-provision-final"), m.snapLater("06-done"))
		return m, tea.Batch(cmds...)
	}
	cmds = append(cmds, waitEvent(m.events))
	return m, tea.Batch(cmds...)
}

func (m *Model) group(id string) *plan.Group {
	if i, ok := m.byID[id]; ok {
		return m.groups[i]
	}
	if id == "" {
		return nil
	}
	c, ok := plan.ByID(id)
	if !ok {
		c = plan.Command{ID: id, Title: id, Args: []string{id}}
	}
	m.groups = append(m.groups, plan.NewGroup(c))
	m.byID[id] = len(m.groups) - 1
	return m.groups[len(m.groups)-1]
}

func (m *Model) addLog(s string) {
	m.logLines = append(m.logLines, s)
	if len(m.logLines) > maxLog {
		m.logLines = m.logLines[len(m.logLines)-maxLog:]
	}
	if m.scroll > 0 {
		m.scroll++ // keep the view where the reader left it
	}
}

var accountRe = regexp.MustCompile(`\b[0-9]{12}\b`)

// redact replaces any AWS account id with the documentation placeholder
// 111122223333.
func (m Model) redact(s string) string {
	if !m.opt.Redact {
		return s
	}
	return accountRe.ReplaceAllString(s, "111122223333")
}

// View implements tea.Model.
func (m Model) View() string { return m.render(m.w, m.h) }
