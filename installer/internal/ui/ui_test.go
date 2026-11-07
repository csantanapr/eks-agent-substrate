package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/config"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/preflight"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/run"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/snap"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		nm, _ := m.Update(key(k))
		m = nm.(Model)
	}
	return m
}

// dryRun builds the model the way main does for --dry-run, from the record
// of the live install in testdata/.
func dryRun(t *testing.T, snapDir string) (Model, run.Header) {
	t.Helper()
	repo, err := config.LoadRepo(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Skipf("no eks-talk checkout next to installer/: %v", err)
	}
	h, evs, err := run.LoadRecord(filepath.Join("..", "..", "testdata", "install.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Repo: repo, DryRun: true, Replay: &run.Replay{Events: evs}, SnapshotDir: snapDir,
		Seed: map[string]string{"CLUSTER_NAME": h.Cluster, "AWS_REGION": h.Region}, Demos: map[string]bool{}}
	for k, v := range h.Values {
		opt.Seed[k] = v
	}
	for _, d := range h.Demos {
		opt.Demos[d] = true
	}
	if err := json.Unmarshal(h.Checks, &opt.Checks); err != nil {
		t.Fatalf("the record has no preflight results: %v", err)
	}
	return New(opt), h
}

// pump feeds the run's events into the model until the run ends.
func pump(t *testing.T, m Model) Model {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for m.running {
		select {
		case ev := <-m.events:
			nm, _ := m.Update(eventMsg(ev))
			m = nm.(Model)
		case <-deadline:
			t.Fatal("the replay did not end")
		}
	}
	return m
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func checkSize(t *testing.T, name, view string, w, h int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Errorf("%s: %d lines, want %d", name, len(lines), h)
	}
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("%s: line %d is %d cells wide (max %d): %q", name, i+1, lw, w, l)
		}
	}
}

// The recorded live run, replayed through every screen: the same screens,
// nothing written or run, and the checklist ends with every row done.
func TestDryRunReplay(t *testing.T) {
	snaps := t.TempDir()
	m, h := dryRun(t, snaps)
	localBefore := exists(m.opt.Repo.LocalPath())
	const W, H = 126, 33
	checkSize(t, "welcome", m.render(W, H), W, H)
	m = press(t, m, "enter")
	if m.screen != sCheck || m.pending != 0 || len(m.checks) == 0 {
		t.Fatalf("check screen: screen %v, %d pending, %d checks", m.screen, m.pending, len(m.checks))
	}
	for _, c := range m.checks {
		if c.Status != preflight.Pass {
			t.Errorf("recorded check %s: %v %s", c.Name, c.Status, c.Detail)
		}
	}
	checkSize(t, "check", m.render(W, H), W, H)
	m = press(t, m, "enter")
	if m.screen != sSettings {
		t.Fatalf("screen %v, want settings", m.screen)
	}
	checkSize(t, "settings", m.render(W, H), W, H)
	m = press(t, m, "n")
	if m.screen != sReview {
		t.Fatalf("screen %v, want review: %s", m.screen, m.msg)
	}
	if !strings.Contains(m.localText, "CLUSTER_NAME="+h.Cluster+"\n") {
		t.Errorf("review config.local.env:\n%s", m.localText)
	}
	checkSize(t, "review", m.render(W, H), W, H)
	m = press(t, m, "y")
	if m.screen != sProvision || !m.running {
		t.Fatalf("screen %v running %v", m.screen, m.running)
	}
	m = pump(t, m)
	if m.screen != sDone || m.exitCode != 0 {
		t.Fatalf("after the replay: screen %v exit %d", m.screen, m.exitCode)
	}
	if len(m.groups) != len(plan.Install(m.demos)) {
		t.Errorf("%d groups for %d planned commands", len(m.groups), len(plan.Install(m.demos)))
	}
	for _, g := range m.groups {
		if g.Status != plan.Done {
			t.Errorf("%s: %v", g.Cmd.ID, g.Status)
		}
		for _, r := range g.Rows {
			if r.Status != plan.Done && !(r.Status == plan.Skipped && r.Optional) {
				t.Errorf("%s: row %q is %v", g.Cmd.ID, r.Title, r.Status)
			}
		}
	}
	if g := m.groups[0]; len(g.Timings) == 0 || g.Total == 0 {
		t.Errorf("create-cluster.sh's Timings table was not read: %+v", g.Timings)
	}
	done := m.render(W, H)
	checkSize(t, "done", done, W, H)
	if !strings.Contains(done, "Agent Substrate is running on "+h.Cluster) {
		t.Errorf("done screen:\n%s", done)
	}
	// A dry run writes nothing into the checkout.
	if after := exists(m.opt.Repo.LocalPath()); after != localBefore {
		t.Errorf("config.local.env existed %v before the dry run and %v after", localBefore, after)
	}
	// Teardown is refused in a dry run.
	m = press(t, m, "t")
	if m.screen != sDone || !strings.Contains(m.msg, "dry run") {
		t.Errorf("t in a dry run: screen %v msg %q", m.screen, m.msg)
	}
}

func TestSettingsEditAndValidate(t *testing.T) {
	repo, err := config.LoadRepo(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Skip(err)
	}
	// No provider checkout here, so that plugin cannot be chosen.
	t.Setenv("CREDPROVIDER_DIR", t.TempDir())
	m := New(Options{Repo: repo, Checks: []preflight.Result{}})
	// Start with OpenClaw off, whatever this checkout's config says.
	m.setOpenClaw(false)
	m.demos["openclaw"] = false
	m.demos["mode3"], m.values["MODE3"] = false, ""
	m = press(t, m, "enter", "enter") // welcome -> check -> settings
	if m.screen != sSettings {
		t.Fatalf("screen %v", m.screen)
	}
	// Cluster name: clear it and type a bad one.
	m = press(t, m, "enter")
	m.input.SetValue("Bad_Name")
	m = press(t, m, "enter")
	if _, bad := m.errs["CLUSTER_NAME"]; !bad {
		t.Error("an invalid cluster name is flagged")
	}
	m = press(t, m, "n")
	if m.screen != sSettings {
		t.Error("review is refused while a setting is invalid")
	}
	m = press(t, m, "enter")
	m.input.SetValue("substrate-tui")
	m = press(t, m, "enter")
	// Toggle S3 Files: it needs EFS, which turns on with it.
	for m.fields[m.cursor].key != "s3files" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if !m.demos["s3files"] || !m.demos["efs"] {
		t.Errorf("demos %v", m.demos)
	}
	// Mode 3: on writes MODE3=on, off clears it.
	for m.fields[m.cursor].key != "mode3" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if !m.demos["mode3"] || m.values["MODE3"] != "on" {
		t.Errorf("mode3 on: %v %q", m.demos["mode3"], m.values["MODE3"])
	}
	m = press(t, m, " ")
	if m.demos["mode3"] || m.values["MODE3"] != "" {
		t.Errorf("mode3 off: %v %q", m.demos["mode3"], m.values["MODE3"])
	}
	for m.fields[m.cursor].key != "s3files" {
		m = press(t, m, "up")
	}
	// OpenClaw: on writes the repository to clone, off clears it again.
	for m.fields[m.cursor].key != "openclaw" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if !m.demos["openclaw"] || m.values["OPENCLAW_REPO"] != plan.OpenClawRepo || m.values["OPENCLAW_REF"] != plan.OpenClawRef {
		t.Errorf("openclaw on: %v %q %q", m.demos["openclaw"], m.values["OPENCLAW_REPO"], m.values["OPENCLAW_REF"])
	}
	m = press(t, m, " ")
	if m.demos["openclaw"] || m.values["OPENCLAW_REPO"] != "" {
		t.Errorf("openclaw off: %v %q", m.demos["openclaw"], m.values["OPENCLAW_REPO"])
	}
	// OpenClaw needs EFS: turning EFS off turns OpenClaw off, and OpenClaw on
	// turns EFS back on.
	m = press(t, m, " ")
	for m.fields[m.cursor].key != "efs" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if m.demos["efs"] || m.demos["openclaw"] || m.demos["s3files"] || m.values["OPENCLAW_REPO"] != "" {
		t.Errorf("efs off: %v %q", m.demos, m.values["OPENCLAW_REPO"])
	}
	for m.fields[m.cursor].key != "openclaw" {
		m = press(t, m, "up")
	}
	m = press(t, m, " ")
	if !m.demos["openclaw"] || !m.demos["efs"] {
		t.Errorf("openclaw on: %v", m.demos)
	}
	for m.fields[m.cursor].key != "s3files" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	for m.fields[m.cursor].key != "credprovider-aws" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if m.demos["credprovider-aws"] || !strings.Contains(m.msg, "CREDPROVIDER_DIR") {
		t.Errorf("the provider plugin needs its checkout: %v %q", m.demos["credprovider-aws"], m.msg)
	}
	v := plan.StripANSI(m.render(126, 33))
	for _, want := range []string{"Kube API access CIDRs", "Demos", "Plugins", "OpenClaw always-on agent", "Secrets Manager credential provider"} {
		if !strings.Contains(v, want) {
			t.Errorf("settings screen lacks %q", want)
		}
	}
	if strings.Index(v, "OpenClaw") > strings.Index(v, "Plugins") {
		t.Error("OpenClaw is listed under Demos, above Plugins")
	}
	m = press(t, m, "n")
	if m.screen != sReview {
		t.Fatalf("screen %v: %s", m.screen, m.msg)
	}
	if !strings.Contains(m.localText, "OPENCLAW_REPO="+plan.OpenClawRepo) {
		t.Errorf("config.local.env lacks OPENCLAW_REPO:\n%s", m.localText)
	}
	var ids []string
	for _, c := range m.cmds {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); got != "create-cluster deploy counter-verify efs efs-verify s3files s3files-verify openclaw openclaw-verify" {
		t.Errorf("commands: %s", got)
	}
}

// The provider plugin is offered when its checkout is there, and runs its
// deploy and its check.
func TestCredproviderPlugin(t *testing.T) {
	repo, err := config.LoadRepo(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDPROVIDER_DIR", dir)
	m := New(Options{Repo: repo, Checks: []preflight.Result{}})
	m = press(t, m, "enter", "enter")
	for m.fields[m.cursor].key != "credprovider-aws" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if !m.demos["credprovider-aws"] {
		t.Fatalf("not chosen: %q", m.msg)
	}
	var ids []string
	for _, c := range plan.Install(m.demos) {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); !strings.Contains(got, "credprovider-aws credprovider-aws-verify") {
		t.Errorf("commands: %s", got)
	}
	// With every choice on, Review still shows every command and the prompt.
	for _, id := range []string{"openclaw", "efs", "s3files", "db-rotation", "mode3"} {
		m.demos[id] = true
	}
	m = press(t, m, "n")
	if m.screen != sReview {
		t.Fatalf("screen %v: %s", m.screen, m.msg)
	}
	v := plan.StripANSI(m.render(126, 33))
	checkSize(t, "review", v, 126, 33)
	for _, want := range []string{"demos/credprovider-aws/verify.sh", "demos/openclaw/deploy.sh verify", "to write config.local.env and start"} {
		if !strings.Contains(v, want) {
			t.Errorf("review lacks %q:\n%s", want, v)
		}
	}
}

// --done reopens a finished install on its Done screen, with the same
// checklist the run ended with, and Teardown offered.
func TestReopenLastRun(t *testing.T) {
	repo, err := config.LoadRepo(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Skip(err)
	}
	h, evs, err := run.LoadRecord(filepath.Join("..", "..", "testdata", "install.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{Repo: repo, Seed: h.Values, Last: &LastRun{Header: h, Events: evs, LogPath: "x.log"}})
	if m.screen != sDone || m.exitCode != 0 || m.running {
		t.Fatalf("screen %v exit %d running %v", m.screen, m.exitCode, m.running)
	}
	if len(m.groups) != 3 || m.groups[1].Total == 0 || m.account != "111122223333" {
		t.Errorf("%d groups, deploy total %d, account %q", len(m.groups), m.groups[1].Total, m.account)
	}
	if d := m.runEnd.Sub(m.runStart); d < 30*time.Minute {
		t.Errorf("run took %v by the record", d)
	}
	v := m.render(126, 33)
	checkSize(t, "done", v, 126, 33)
	for _, want := range []string{"Agent Substrate is running on substrate-tui", "./clean.sh --yes", "export PATH=$PWD/.work/bin:$PATH"} {
		if !strings.Contains(plan.StripANSI(v), want) {
			t.Errorf("done screen lacks %q", want)
		}
	}
	m = press(t, m, "t")
	if m.screen != sTeardown {
		t.Errorf("t opens the teardown confirm, screen %v", m.screen)
	}
	m = press(t, m, "esc")
	if m.screen != sDone {
		t.Errorf("esc goes back, screen %v", m.screen)
	}
}

// A teardown names its steps Teardown and Removed, and a finished one
// shows Removed as the current step.
func TestTeardownRail(t *testing.T) {
	m := Model{screen: sProvision, teardown: true, running: true, values: map[string]string{"CLUSTER_NAME": "c"}}
	rail := func() string { return plan.StripANSI(strings.Split(m.header(160), "\n")[1]) }
	if got := rail(); !strings.Contains(got, " 5 Teardown ") || !strings.Contains(got, "6 Removed") || strings.Contains(got, "Provision") {
		t.Errorf("running: %q", got)
	}
	m.running = false
	if got := rail(); !strings.Contains(got, iconDone+" Teardown") || !strings.Contains(got, " 6 Removed ") {
		t.Errorf("finished: %q", got)
	}
	m.exitCode = 1
	if got := rail(); !strings.Contains(got, " 5 Teardown ") {
		t.Errorf("failed: %q", got)
	}
	m = Model{screen: sProvision, values: map[string]string{}}
	if got := rail(); !strings.Contains(got, " 5 Provision ") || !strings.Contains(got, "6 Done") {
		t.Errorf("install: %q", got)
	}
}

func TestRedact(t *testing.T) {
	m := Model{opt: Options{Redact: true}}
	if got := m.redact("substrate-tui-snap-444455556666-us-west-2"); got != "substrate-tui-snap-111122223333-us-west-2" {
		t.Error(got)
	}
}

// colors sets the color profile for the rest of the test.
func colors(t *testing.T, p termenv.Profile) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

// Every splash frame fits the window, with the art when there is room and
// the plain title when there is not. SPLASH_FRAMES_DIR=DIR also saves every
// frame at 80x24, 126x33 and 140x45 there, as snapshots.
func TestSplashFits(t *testing.T) {
	colors(t, termenv.TrueColor)
	dir := os.Getenv("SPLASH_FRAMES_DIR")
	m := Model{screen: sSplash}
	for _, sz := range []struct {
		w, h int
		art  bool
	}{{80, 24, true}, {126, 33, true}, {140, 45, true}, {200, 60, true}, {60, 26, true}, {90, 26, true}, {59, 19, false}, {79, 20, false}, {20, 6, false}} {
		for f := 0; f <= splashFrames; f++ {
			m.frame = f
			name := fmt.Sprintf("splash %dx%d frame %d", sz.w, sz.h, f)
			v := m.splash(sz.w, sz.h)
			checkSize(t, name, v, sz.w, sz.h)
			// The art paints cell backgrounds; the plain title does not.
			if got := strings.Contains(v, "48;2;"); got != sz.art {
				t.Errorf("%s: art %v, want %v", name, got, sz.art)
			}
			if dir != "" && sz.w*sz.h >= 80*24 && sz.w <= 140 && sz.art {
				w := snap.Writer{Dir: filepath.Join(dir, fmt.Sprintf("%dx%d", sz.w, sz.h)), Title: "splash"}
				if err := w.Save(fmt.Sprintf("frame-%02d", f), v); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	m.frame = splashFrames
	for _, sz := range [][2]int{{80, 24}, {126, 33}, {140, 45}} {
		v := plan.StripANSI(m.splash(sz[0], sz[1]))
		for _, want := range []string{"Agent Substrate 1.0.0", "on Amazon EKS", " Amazon EKS ", " gVisor ", " microVM ", " microVM+gVisor ", "press any key"} {
			if !strings.Contains(v, want) {
				t.Errorf("%dx%d: the last frame lacks %q:\n%s", sz[0], sz[1], want, v)
			}
		}
	}
}

// Without color (NO_COLOR) the splash is the plain title, with no blocks.
func TestSplashNoColor(t *testing.T) {
	colors(t, termenv.Ascii)
	m := Model{screen: sSplash, frame: 20}
	v := m.splash(80, 24)
	checkSize(t, "no color", v, 80, 24)
	if strings.Contains(v, "\x1b[") || strings.Contains(v, "▀") || !strings.Contains(v, "Agent Substrate 1.0.0") {
		t.Errorf("no color:\n%s", v)
	}
}

// The splash opens the installer, a key skips it, and it ends on its own
// at Welcome; --done opens on Done without it.
func TestSplashSkip(t *testing.T) {
	repo, err := config.LoadRepo(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Skip(err)
	}
	opt := Options{Repo: repo, Checks: []preflight.Result{}, Splash: true}
	m := New(opt)
	if m.screen != sSplash {
		t.Fatalf("starts on screen %v", m.screen)
	}
	checkSize(t, "splash in the model", m.render(80, 24), 80, 24)
	m = press(t, m, "x")
	if m.screen != sWelcome {
		t.Fatalf("a key goes to Welcome, screen %v", m.screen)
	}
	// A tick from the skipped splash changes nothing.
	nm, c := m.Update(splashMsg{})
	if m2 := nm.(Model); m2.screen != sWelcome || m2.frame != m.frame || c != nil {
		t.Errorf("late tick: screen %v frame %d", m2.screen, m2.frame)
	}
	// enter on the splash skips it and does not also start the checks.
	m = press(t, New(opt), "enter")
	if m.screen != sWelcome {
		t.Errorf("enter: screen %v", m.screen)
	}
	m = New(opt)
	for i := 0; i < splashFrames; i++ {
		if m.screen != sSplash {
			t.Fatalf("the splash ended after %d frames", i)
		}
		nm, _ := m.Update(splashMsg{})
		m = nm.(Model)
	}
	if m.screen != sWelcome {
		t.Errorf("after %d frames: screen %v", splashFrames, m.screen)
	}
	if d := time.Duration(splashFrames) * splashEvery; d < 3*time.Second || d > 4*time.Second {
		t.Errorf("the splash lasts %v", d)
	}

	h, evs, err := run.LoadRecord(filepath.Join("..", "..", "testdata", "install.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opt.Last = &LastRun{Header: h, Events: evs, LogPath: "x.log"}
	if m := New(opt); m.screen != sDone {
		t.Errorf("--done: screen %v", m.screen)
	}
}
