package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// The Review screen is the gate before anything runs: every command the run
// will make, in order (the builder's Display lines), each step's time, the
// total, and what the AWS resources the run creates cost per hour and per
// day. Nothing has been created when it shows; y starts the install.

// stepMinutes are rough step times from live installs in us-west-2. The
// plugins step is the sum of the chosen plugins' Minutes.
var stepMinutes = map[state.Step]int{
	state.CreateCluster: 20,
	state.Provision:     1,
	state.RDS:           8,
	state.ControlPlane:  2,
	state.Demo:          11,
}

// Hourly on-demand list prices in us-west-2 (AWS Price List API,
// 2026-11-05).
var ec2Price = map[string]float64{
	"m8i.large": 0.10584, "m8i.xlarge": 0.21168, "m8i.2xlarge": 0.42336, "m8i.4xlarge": 0.84672, "m8i.8xlarge": 1.69344,
}

const (
	eksHourly     = 0.10          // the EKS control plane
	natHourly     = 0.045 + 0.005 // one NAT gateway and its public IPv4 address
	rdsHourly     = 0.065         // db.t4g.medium, setup-aws create rds's default
	rdsStorageGB  = 20
	gp3PerGBMonth = 0.08
	hoursPerMonth = 730
	priceRegion   = "us-west-2"
)

// nodeDiskGB is each node group's root volume (setup-aws nodegroups.go).
var nodeDiskGB = map[string]float64{"SYSTEM": 100, "GVISOR": 200, "MICROVM": 200}

// clusterHourly is what a new cluster costs an hour: the control plane, the
// NAT gateway, the nodes and their gp3 volumes. ok is false, with the
// instance type, when the type is not in the price list.
func clusterHourly(c state.ClusterConfig) (usd float64, unknown string) {
	usd = eksHourly + natHourly
	for _, g := range []struct {
		name, typ, nodes string
	}{{"SYSTEM", c.SystemInstanceType, c.SystemNodes}, {"GVISOR", c.GVisorInstanceType, c.GVisorNodes}, {"MICROVM", c.MicroVMInstanceType, c.MicroVMNodes}} {
		n, _ := strconv.Atoi(g.nodes)
		p, ok := ec2Price[g.typ]
		if !ok {
			return 0, g.typ
		}
		usd += float64(n) * (p + nodeDiskGB[g.name]*gp3PerGBMonth/hoursPerMonth)
	}
	return usd, ""
}

// planStep is one row of the plan.
type planStep struct {
	step    state.Step
	display string
	summary bool // display names the step rather than a command to paste
	minutes int
	hourly  float64
	// unknown names an instance type with no price, for the cost note.
	unknown string
}

// plan is the long steps after Review on the current flow, in order.
func (m Model) plan() []planStep {
	var out []planStep
	after := false
	for _, st := range m.m.Order() {
		if st == state.Review {
			after = true
			continue
		}
		build, _, long := m.longStep(st)
		if !after || !long || build == nil {
			continue
		}
		spec := build(m.s)
		p := planStep{step: st, display: spec.Display, summary: spec.DisplayIsSummary, minutes: stepMinutes[st]}
		switch st {
		case state.CreateCluster:
			p.hourly, p.unknown = clusterHourly(m.s.NewClusterConfig)
		case state.RDS:
			p.hourly = rdsHourly + rdsStorageGB*gp3PerGBMonth/hoursPerMonth
		case state.InstallPlugins:
			for _, pl := range resolved(m.s.Plugins) {
				p.minutes += pl.Minutes
				p.hourly += pl.HourlyUSD
			}
		}
		out = append(out, p)
	}
	return out
}

// costLine is the plan's total cost, with the price list's Region, under a
// headline that says whether the run creates anything billed by the hour.
func costLine(plan []planStep, region string) (headline, cost string) {
	headline = "This creates billable AWS resources."
	total := 0.0
	for _, p := range plan {
		if p.unknown != "" {
			return headline, "Cost: see the AWS Pricing Calculator for " + p.unknown + " (not in this installer's price list)."
		}
		total += p.hourly
	}
	if total == 0 {
		return "This creates AWS resources, none of them billed by the hour.", "Cost: S3 and ECR storage only, on top of the cluster you already run."
	}
	s := fmt.Sprintf("Cost: about $%.2f an hour ($%.0f a day) until teardown, on-demand", total, total*24)
	if region != priceRegion {
		return headline, s + ", at " + priceRegion + " prices; see the AWS Pricing Calculator for " + region + "."
	}
	return headline, s + ", " + priceRegion + "."
}

func (m *Model) reviewKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "y":
		if m.d.SaveConfig != nil {
			path, err := m.d.SaveConfig(m.s)
			if err != nil {
				m.notice = "config.local.env not written, nothing started: " + err.Error()
				return nil
			}
			m.notice = "wrote " + tildify(path)
		}
		m.m.Next()
		return m.enter()
	case "enter":
		m.notice = "press y to start the install; nothing has run yet"
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	}
	return nil
}

var envPrefix = regexp.MustCompile(`^\$ [A-Z_]+=`)

// cutCommand fits a command in n cells on one line. A setup-aws command
// starts with its environment and ends with what it does, so it keeps its
// end; any other keeps its start.
func cutCommand(cmd string, n int) string {
	r := []rune(cmd)
	if len(r) <= n {
		return cmd
	}
	if envPrefix.MatchString(cmd) {
		return "$ …" + string(r[len(r)-(n-3):])
	}
	return truncate(cmd, n)
}

// reviewView draws the plan in h rows (0: as many as it takes). A plan that
// does not fit shows each command on one line, cut, instead of wrapped.
func (m Model) reviewView(h int) string {
	s := m.s
	w := m.bodyWidth() - 2
	plan := m.plan()

	what := "connected, not created"
	if s.ClusterIsNew {
		what = "new: this run creates it"
	}
	auto := "Auto Mode off"
	if s.AutoMode {
		auto = "Auto Mode on"
	}
	plugins := "none"
	if ps := resolved(s.Plugins); len(ps) > 0 {
		var ids []string
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		plugins = strings.Join(ids, ", ")
	}
	demo := "no"
	if s.DemoMode {
		demo = "the counter on " + s.SandboxClasses
	}
	head := []string{
		theme.Title.Render("Review the plan") + "  " + theme.Subtle.Render("nothing has run yet"),
		theme.Subtle.Render(truncate(fmt.Sprintf("Cluster %s (%s) · EKS %s · %s · %s", s.ClusterName, what, s.ClusterVersion, auto, s.Region), w)),
		theme.Subtle.Render(truncate(fmt.Sprintf("Substrate %s · %s · database %s · plugins: %s · demo: %s", s.SubstrateTag, s.SandboxClasses, s.Database, plugins, demo), w)),
		theme.Title.Render(pad("   Step", w-24)) + theme.Title.Render(fmt.Sprintf("%8s  %12s", "Time", "Cost an hour")),
	}

	total := 0
	rows := func(wrap bool) []string {
		var out []string
		total = 0
		for i, p := range plan {
			total += p.minutes
			cost := "–"
			if p.hourly > 0 {
				cost = fmt.Sprintf("$%.2f", p.hourly)
			}
			out = append(out, pad(fmt.Sprintf("%2d %s", i+1, p.step.Title()), w-24)+fmt.Sprintf("%8s  %12s", fmt.Sprintf("~%d min", p.minutes), cost))
			cmd := p.display
			if !p.summary {
				cmd = "$ " + cmd
			}
			if !wrap {
				out = append(out, "   "+theme.CommandLine.Render(cutCommand(cmd, w-3)))
				continue
			}
			for _, l := range strings.Split(lipgloss.NewStyle().Width(w-5).Render(cmd), "\n") {
				out = append(out, "   "+theme.CommandLine.Render(strings.TrimRight(l, " ")))
			}
		}
		return out
	}

	where := "Settings are not saved: no eks-talk checkout (config.env) was found."
	switch {
	case m.d.DryRun:
		where = "Dry run: the commands are replayed; nothing is written or created."
	case m.d.SaveConfig != nil && m.d.Config.Dir != "":
		where = "y writes the settings to " + tildify(m.d.Config.LocalPath()) + " (mode 0600), then starts."
	}
	headline, cost := costLine(plan, s.Region)
	view := func(body []string) string {
		foot := []string{"",
			theme.Subtle.Render(fmt.Sprintf("About %d minutes in all. Each step is idempotent: a failed one is rerun with enter.", total)),
			lipgloss.NewStyle().Bold(true).Render(headline),
			cost,
			theme.Subtle.Render(where),
			"Press " + theme.Key.Render("y") + " to start.",
		}
		return lipgloss.NewStyle().Width(w).Render(strings.Join(append(append(head, body...), foot...), "\n"))
	}
	out := view(rows(true))
	if h > 0 && lipgloss.Height(out) > h {
		out = view(rows(false))
	}
	return out
}
