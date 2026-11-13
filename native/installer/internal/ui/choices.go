package ui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// databaseScreen chooses the control plane's database (issue #5 R3): RDS,
// which adds the RDS step after Provision, or the bundled PostgreSQL.
type databaseScreen struct{ cursor int }

var databases = []struct{ value, label, detail string }{
	{state.DatabaseRDS, "Amazon RDS for PostgreSQL (recommended)",
		"A private RDS instance in the cluster's VPC, reachable only from the cluster; the master password is managed by RDS and the logins live in Secrets Manager."},
	{state.DatabaseBundled, "Bundled PostgreSQL",
		"Substrate's own PostgreSQL in ate-system, on an EBS volume. Nothing to create in AWS; the data goes with the cluster."},
}

func (m *Model) databaseKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		m.db.cursor = 0
	case "down", "j":
		m.db.cursor = 1
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	case "enter":
		m.s.Database = databases[m.db.cursor].value
		return m.commit()
	}
	return nil
}

func (m Model) databaseView() string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose the database") + "\n")
	b.WriteString(theme.Subtle.Render("Where the Substrate control plane keeps its state") + "\n\n")
	para := lipgloss.NewStyle().Width(m.bodyWidth() - 8)
	for i, d := range databases {
		row := d.label
		if i == m.db.cursor {
			row = theme.Selected.Render("› " + row)
		} else {
			row = "  " + row
		}
		b.WriteString(row + "\n")
		b.WriteString(theme.Subtle.Render(indent(para.Render(d.detail), "    ")) + "\n\n")
	}
	return b.String()
}

// scopeScreen is the teardown's one choice: keep the cluster, or delete it
// too, which is offered only for a cluster this installer created. Either is
// confirmed by typing the cluster's name.
type scopeScreen struct {
	cluster      *awsx.Cluster
	err          error
	cursor       int // 0 keep the cluster, 1 delete everything
	confirm      string
	fromComplete bool
}

type describeMsg struct {
	c   awsx.Cluster
	err error
}

func (m *Model) enterScope() tea.Cmd {
	m.scope.cluster, m.scope.err, m.scope.cursor, m.scope.confirm = nil, nil, 0, ""
	name, describe := m.s.ClusterName, m.d.Describe
	return func() tea.Msg {
		c, err := describe(context.Background(), name)
		return describeMsg{c, err}
	}
}

func (m *Model) scopeDescribed(msg describeMsg) {
	m.scope.err = msg.err
	if msg.err == nil {
		c := msg.c
		m.scope.cluster = &c
	}
}

func (m Model) scopeOwned() bool {
	return m.scope.cluster != nil && m.scope.cluster.CreatedByInstaller(m.s.Region)
}

func (m *Model) scopeKey(msg tea.KeyMsg) tea.Cmd {
	sc := m.scope
	switch msg.String() {
	case "up":
		sc.cursor = 0
	case "down":
		if m.scopeOwned() {
			sc.cursor = 1
		}
	case "esc":
		if sc.fromComplete {
			// Back to the install's Complete screen, which [t] left.
			m.s.Teardown, sc.fromComplete = false, false
			m.jump(state.InstallOrder(m.s), state.Complete)
			return nil
		}
		m.m.Prev()
	case "enter":
		if sc.err != nil {
			return m.enterScope()
		}
		if sc.cluster == nil || strings.TrimSpace(sc.confirm) != m.s.ClusterName {
			return nil
		}
		m.s.ClusterOwned = m.scopeOwned()
		m.s.KeepCluster = sc.cursor == 0 || !m.s.ClusterOwned
		// A teardown step already run had the earlier answer; both steps
		// are idempotent, so they run again with this one.
		m.tasks = map[state.Step]*task{}
		m.m.Next()
		return m.enter()
	default:
		sc.confirm = edit(sc.confirm, msg)
	}
	return nil
}

func (m Model) scopeHints() []string {
	if m.scope.err != nil {
		return []string{"enter retry", "esc back", "ctrl+c quit"}
	}
	return []string{"↑/↓ select", "type the name, then enter", "esc back", "ctrl+c quit"}
}

func (m Model) scopeView() string {
	sc := m.scope
	name := m.s.ClusterName
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose what to remove") + "\n")
	b.WriteString(theme.Subtle.Render("From cluster "+name+" in "+m.s.Region) + "\n\n")
	switch {
	case sc.err != nil:
		b.WriteString(theme.Bad.Render(sc.err.Error()) + "\n")
		return b.String()
	case sc.cluster == nil:
		b.WriteString(m.spinner() + " eks:DescribeCluster " + name + "…\n")
		return b.String()
	}
	para := lipgloss.NewStyle().Width(m.bodyWidth() - 8)
	options := []struct{ label, detail string }{
		{"Remove Substrate and its AWS data, keep the cluster",
			"ate-setup delete ate-system, then setup-aws teardown --keep-cluster: the buckets, Pod Identity roles, ECR repositories and the RDS database go; the cluster, its node groups and its network stay."},
		{"Delete everything, including cluster " + name,
			"ate-setup delete ate-system, then setup-aws teardown: also the node groups, the cluster, the launch template and the VPC."},
	}
	if !m.scopeOwned() {
		options[1].detail = "Not offered: this installer did not create " + name + ", and it never deletes a cluster it did not create.\n(no " + awsx.CreatedByTagKey + "=" + awsx.CreatedByValue + " tag on " + name + ")"
	}
	for i, o := range options {
		row := o.label
		switch {
		case i == sc.cursor:
			row = theme.Selected.Render("› " + row)
		case i == 1 && !m.scopeOwned():
			row = theme.Fainted.Render("  " + row)
		default:
			row = "  " + row
		}
		b.WriteString(row + "\n" + theme.Subtle.Render(indent(para.Render(o.detail), "    ")) + "\n\n")
	}
	b.WriteString("Type " + theme.Key.Render(name) + " to confirm:\n")
	b.WriteString(theme.Selected.Render("› "+sc.confirm+"▏") + "\n")
	return b.String()
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
