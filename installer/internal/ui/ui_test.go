package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/config"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/preflight"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/run"
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
	m := New(Options{Repo: repo, Checks: []preflight.Result{}})
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
	for m.fields[m.cursor].key != "openclaw" {
		m = press(t, m, "down")
	}
	m = press(t, m, " ")
	if m.demos["openclaw"] {
		t.Error("a disabled demo cannot be chosen")
	}
	m = press(t, m, "n")
	if m.screen != sReview {
		t.Fatalf("screen %v: %s", m.screen, m.msg)
	}
	var ids []string
	for _, c := range m.cmds {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); got != "create-cluster deploy counter-verify efs efs-verify s3files s3files-verify" {
		t.Errorf("commands: %s", got)
	}
}

func TestRedact(t *testing.T) {
	m := Model{opt: Options{Redact: true}}
	if got := m.redact("substrate-tui-snap-135728714534-us-west-2"); got != "substrate-tui-snap-1357xxxx4534-us-west-2" {
		t.Error(got)
	}
}
