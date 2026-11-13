// Package ui is the wizard, a Bubble Tea program over state.Machine, the
// same shape as substrate-gke's installer/internal/ui: one screen per Step,
// a step rail, and long steps that stream an execx.Spec with a checklist.
// The screens are a POC subset of substrate-gke's.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// Deps are what the screens call out to; tests and --dry-run replace them.
type Deps struct {
	Runner execx.Runner
	// Checks are the doctor checks.
	Checks []doctor.Check
	// Identity and Clusters read AWS.
	Identity func(ctx context.Context) (account, arn string, err error)
	Clusters func(ctx context.Context) ([]awsx.Cluster, error)
	// Bootstrap and ControlPlane build the long steps' commands.
	Bootstrap    func(*state.Setup) execx.Spec
	ControlPlane func(*state.Setup) execx.Spec
	// LogPath is shown on failure and at the end.
	LogPath string
	DryRun  bool
}

type (
	checksMsg   []doctor.Result
	identityMsg struct {
		account, arn string
		err          error
	}
	clustersMsg struct {
		list []awsx.Cluster
		err  error
	}
	execMsg struct {
		ev execx.Event
		ch <-chan execx.Event
	}
	tickMsg time.Time
)

// task is a long step in flight or finished.
type task struct {
	items  []steps.ChecklistItem
	active int
	lines  []string
	done   bool
	err    error
	start  time.Time
	spec   execx.Spec
}

// Model is the wizard.
type Model struct {
	d       Deps
	m       *state.Machine
	s       *state.Setup
	w, h    int
	frame   int
	results []doctor.Result

	idErr    error
	clusters []awsx.Cluster
	clErr    error
	cursor   int

	prov, cp *task
	quitting bool
}

// New returns the wizard at Welcome.
func New(d Deps, s *state.Setup) Model {
	return Model{d: d, m: state.NewMachine(), s: s}
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init starts the spinner.
func (m Model) Init() tea.Cmd { return tick() }

func listen(ch <-chan execx.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return execMsg{ev: execx.Event{Done: true}, ch: nil}
		}
		return execMsg{ev: ev, ch: ch}
	}
}

// enter starts whatever the new current step does on arrival.
func (m *Model) enter() tea.Cmd {
	switch m.m.Current() {
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
		return func() tea.Msg {
			a, arn, err := m.d.Identity(context.Background())
			return identityMsg{a, arn, err}
		}
	case state.Cluster:
		m.clusters, m.clErr = nil, nil
		return func() tea.Msg {
			l, err := m.d.Clusters(context.Background())
			return clustersMsg{l, err}
		}
	case state.Provision:
		if m.prov == nil {
			m.prov = m.startTask(m.d.Bootstrap(m.s), steps.Bootstrap())
			return listen(m.d.Runner.Start(context.Background(), m.prov.spec))
		}
	case state.ControlPlane:
		if m.cp == nil {
			m.cp = m.startTask(m.d.ControlPlane(m.s), steps.ControlPlane())
			return listen(m.d.Runner.Start(context.Background(), m.cp.spec))
		}
	}
	return nil
}

func (m *Model) startTask(spec execx.Spec, items []steps.ChecklistItem) *task {
	return &task{items: items, active: -1, start: time.Now(), spec: spec}
}

func (m *Model) current() *task {
	switch m.m.Current() {
	case state.Provision:
		return m.prov
	case state.ControlPlane:
		return m.cp
	}
	return nil
}

func (m Model) canAdvance() bool {
	switch m.m.Current() {
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
	case state.Cluster:
		return len(m.clusters) > 0 && m.clusters[m.cursor].Supported()
	case state.Provision, state.ControlPlane:
		t := m.current()
		return t != nil && t.done && t.err == nil
	case state.Complete:
		return false
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
		return m, tick()
	case checksMsg:
		m.results = msg
	case identityMsg:
		m.s.Account, m.s.Arn, m.idErr = msg.account, msg.arn, msg.err
	case clustersMsg:
		m.clusters, m.clErr = msg.list, msg.err
		for i, c := range m.clusters {
			if c.Name == m.s.ClusterName {
				m.cursor = i
			}
		}
	case execMsg:
		t := m.current()
		if t == nil {
			return m, nil
		}
		if msg.ev.Line != "" {
			t.lines = append(t.lines, msg.ev.Line)
			t.active = steps.Progress(t.items, t.active, msg.ev.Line)
		}
		if msg.ev.Done {
			t.done, t.err = true, msg.ev.Err
			if t.err == nil {
				t.active = len(t.items)
			}
			return m, nil
		}
		return m, listen(msg.ch)
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if t := m.current(); t != nil && !t.done && msg.String() == "q" {
				return m, nil // q does not abandon a running step; ctrl+c does
			}
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.m.Current() == state.Cluster && m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.m.Current() == state.Cluster && m.cursor < len(m.clusters)-1 {
				m.cursor++
			}
		case "esc":
			if t := m.current(); t == nil || t.done {
				m.m.Prev()
			}
		case "enter":
			if !m.canAdvance() {
				return m, nil
			}
			if m.m.Current() == state.Cluster {
				c := m.clusters[m.cursor]
				m.s.ClusterName, m.s.ClusterVersion, m.s.AutoMode = c.Name, c.Version, c.AutoMode
			}
			m.m.Next()
			return m, m.enter()
		}
	}
	return m, nil
}

// View renders the rail and the current screen.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	rail := m.rail()
	body := m.body()
	return lipgloss.JoinHorizontal(lipgloss.Top, rail, "  ", body) + "\n" + m.hints()
}

func (m Model) rail() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Substrate on EKS") + "\n\n")
	cur := m.m.Current()
	for _, s := range m.m.Order() {
		n, numbered := m.m.Position(s)
		label := s.Title()
		if numbered {
			label = fmt.Sprintf("%d. %s", n, label)
		}
		switch {
		case s == cur:
			b.WriteString(theme.Selected.Render(theme.GlyphActive+" "+label) + "\n")
		case s < cur:
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

func (m Model) body() string {
	var b strings.Builder
	switch m.m.Current() {
	case state.Welcome:
		b.WriteString(theme.Logo() + "\n\n")
		b.WriteString(theme.Title.Render("Agent Substrate on Amazon EKS") + "\n\n")
		b.WriteString("This wizard provisions the AWS resources Substrate uses and installs\n")
		b.WriteString("its control plane on an existing EKS cluster.\n\n")
		b.WriteString(theme.Subtle.Render("Every step is idempotent. Nothing is created before Provision.") + "\n")
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
		b.WriteString(theme.Title.Render("Connect your cluster") + "\n\n")
		switch {
		case m.clErr != nil:
			b.WriteString(theme.Bad.Render(m.clErr.Error()) + "\n")
		case m.clusters == nil:
			b.WriteString(m.spinner() + " eks:ListClusters in " + m.s.Region + "…\n")
		case len(m.clusters) == 0:
			b.WriteString("No EKS clusters in " + m.s.Region + ". Create one first.\n")
		default:
			for i, c := range m.clusters {
				line := fmt.Sprintf("%-24s %-6s %s", c.Name, c.Version, c.Badge())
				if !c.Supported() {
					line = theme.Fainted.Render(line)
				}
				if i == m.cursor {
					line = theme.Selected.Render("› " + line)
				} else {
					line = "  " + line
				}
				b.WriteString(line + "\n")
			}
			b.WriteString("\n" + theme.Subtle.Render("Substrate needs EKS 1.37 or newer (the PodCertificate APIs).") + "\n")
		}
	case state.Provision:
		b.WriteString(theme.Title.Render("Provision AWS resources") + "\n")
		b.WriteString(theme.Subtle.Render("S3 buckets, EKS Pod Identity roles, ECR repositories for "+m.s.ClusterName) + "\n\n")
		b.WriteString(m.taskView(m.prov))
	case state.ControlPlane:
		b.WriteString(theme.Title.Render("Turn on Substrate") + "\n")
		b.WriteString(theme.Subtle.Render("ate-setup deploy ate-system "+m.s.SubstrateTag+" · sandbox classes "+m.s.SandboxClasses) + "\n\n")
		b.WriteString(m.taskView(m.cp))
	case state.Complete:
		b.WriteString(theme.Good.Render(theme.GlyphDone+" Agent Substrate is running on "+m.s.ClusterName) + "\n\n")
		b.WriteString("Try it:\n")
		kc := awsx.KubeconfigPath(m.s.ClusterName, m.s.Region)
		b.WriteString(theme.CommandLine.Render("  export KUBECONFIG="+kc) + "\n")
		b.WriteString(theme.CommandLine.Render("  kubectl -n ate-system get pods") + "\n\n")
		b.WriteString(theme.Subtle.Render("Everything is ClusterIP; reach it with kubectl port-forward or kubectl ate proxy.") + "\n")
		if m.d.LogPath != "" {
			b.WriteString(theme.Subtle.Render("Log: "+m.d.LogPath) + "\n")
		}
	}
	return theme.AccentPanel.Width(m.bodyWidth()).Render(strings.TrimRight(b.String(), "\n"))
}

func (m Model) taskView(t *task) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.CommandLine.Render("$ "+t.spec.Display) + "\n\n")
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
	b.WriteString("\n")
	tail := t.lines
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	for _, l := range tail {
		b.WriteString(theme.Fainted.Render(truncate(l, m.bodyWidth()-4)) + "\n")
	}
	if t.err != nil {
		b.WriteString("\n" + theme.Bad.Render("Failed: "+t.err.Error()) + "\n")
		if m.d.LogPath != "" {
			b.WriteString(theme.Subtle.Render("Full output: "+m.d.LogPath) + "\n")
		}
	} else if t.done {
		b.WriteString(theme.Good.Render(fmt.Sprintf("Done in %s", time.Since(t.start).Round(time.Second))) + "\n")
	}
	return b.String()
}

func (m Model) hints() string {
	h := []string{"enter continue", "esc back", "ctrl+c quit"}
	if m.m.Current() == state.Cluster {
		h = append([]string{"↑/↓ select"}, h...)
	}
	return theme.Subtle.Render("  " + strings.Join(h, " · "))
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
