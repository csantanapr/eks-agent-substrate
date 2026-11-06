package ui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/preflight"
)

// render draws the whole screen at w x h cells.
func (m Model) render(w, h int) string {
	if w < 90 || h < 26 {
		return fmt.Sprintf("The installer needs a terminal of at least 90 x 26 (this one is %d x %d).", w, h)
	}
	head := m.header(w)
	foot := m.footer(w)
	bodyH := h - lipgloss.Height(head) - lipgloss.Height(foot)
	msgLine := ""
	if m.msg != "" {
		msgLine = " " + sYellow.Render(cut(m.msg, w-2))
		bodyH--
	}
	var body string
	switch m.screen {
	case sWelcome:
		body = m.welcome(w, bodyH)
	case sCheck:
		body = m.checkView(w, bodyH)
	case sSettings:
		body = m.settingsView(w, bodyH)
	case sReview:
		body = m.review(w, bodyH)
	case sProvision:
		body = m.provision(w, bodyH)
	case sDone:
		body = m.done(w, bodyH)
	case sTeardown:
		body = m.teardownView(w, bodyH)
	}
	parts := []string{head, fit(body, w, bodyH)}
	if msgLine != "" {
		parts = append(parts, pad(msgLine, w))
	}
	parts = append(parts, foot)
	return m.redact(strings.Join(parts, "\n"))
}

func (m Model) header(w int) string {
	left := " " + sTitle.Render("◆ Agent Substrate on Amazon EKS") + "  " + sMuted.Render("installer")
	if m.opt.DryRun {
		left += "  " + sKey.Render("DRY RUN")
	}
	right := sText.Render(m.values["CLUSTER_NAME"]) + sDim.Render(" · ") + sText.Render(m.values["AWS_REGION"])
	if p := m.values["AWS_PROFILE"]; p != "" {
		right += sDim.Render(" · ") + sMuted.Render(p)
	}
	right += " "
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	line1 := left + strings.Repeat(" ", max(1, gap)) + right

	cur := int(m.screen)
	if m.screen == sTeardown {
		cur = int(sDone)
	}
	names := rail
	if m.teardown && m.screen == sProvision {
		// A teardown reuses the Provision screen; its last step is Removed.
		names = append(append([]string{}, rail[:sProvision]...), "Teardown", "Removed")
		if !m.running && m.exitCode == 0 {
			cur = int(sDone)
		}
	}
	var steps []string
	for i, s := range names {
		label := fmt.Sprintf("%d %s", i+1, s)
		switch {
		case i == cur:
			steps = append(steps, sSel.Render(" "+label+" "))
		case i < cur:
			steps = append(steps, sGreen.Render(iconDone+" ")+sMuted.Render(s))
		default:
			steps = append(steps, sDim.Render(label))
		}
	}
	line2 := " " + strings.Join(steps, sDim.Render("  "+iconArrow+"  "))
	return pad(line1, w) + "\n" + pad(line2, w) + "\n" + rule(w)
}

func (m Model) footer(w int) string {
	var keys string
	switch m.screen {
	case sWelcome:
		keys = keyHelp("enter", "check your setup", "q", "quit")
	case sCheck:
		keys = keyHelp("enter", "continue", "r", "check again", "b", "back", "q", "quit")
	case sSettings:
		if m.edit {
			keys = keyHelp("enter", "save", "esc", "cancel")
		} else {
			keys = keyHelp("↑↓", "move", "enter", "edit / toggle", "n", "review", "b", "back", "q", "quit")
		}
	case sReview:
		if m.opt.DryRun {
			keys = keyHelp("y", "start the dry run (nothing is written or run)", "b", "back", "q", "quit")
		} else {
			keys = keyHelp("y", "write config.local.env and start", "b", "back to settings", "q", "quit")
		}
	case sProvision:
		switch {
		case m.running:
			keys = keyHelp("↑↓ PgUp PgDn", "scroll the log", "f", "follow", "ctrl+c", "stop")
		case m.exitCode != 0:
			keys = keyHelp("r", "run again (the scripts resume)", "↑↓", "scroll the log", "q", "quit")
		case m.teardown:
			keys = keyHelp("q", "quit")
		default:
			keys = keyHelp("enter", "done", "↑↓", "scroll the log", "q", "quit")
		}
	case sDone:
		keys = keyHelp("t", "teardown", "l", "log", "q", "quit")
	case sTeardown:
		keys = keyHelp("enter", "confirm", "esc", "cancel")
	}
	right := sDim.Render(clock(m.now())) + " "
	gap := w - 1 - lipgloss.Width(keys) - lipgloss.Width(right)
	return rule(w) + "\n" + pad(" "+keys+strings.Repeat(" ", max(1, gap))+right, w)
}

// ---------------------------------------------------------------- welcome

func (m Model) nodes(g string) int {
	n, _ := strconv.Atoi(m.values[strings.ToUpper(g)+"_NODES"])
	return n
}

func (m Model) welcome(w, h int) string {
	v := m.values
	total := m.nodes("system") + m.nodes("gvisor") + m.nodes("microvm")
	row := func(k, d string) string { return "    " + sBold.Render(pad(k, 20)) + sText.Render(d) }
	rev := ""
	if m.opt.RepoRev != "" {
		rev = " (" + m.opt.RepoRev + ")"
	}
	cost := estimate(v)
	lines := []string{
		"",
		"  " + sTitle.Render("Install Agent Substrate on a new Amazon EKS cluster"),
		"",
		"  " + sText.Render("A front end for the eks-talk scripts in ") + sCode.Render(m.opt.Repo.Dir) + sMuted.Render(rev) + sText.Render("."),
		"  " + sText.Render("It writes ") + sCode.Render("config.local.env") + sText.Render(" there, then runs ") + sCode.Render("./create-cluster.sh") + sText.Render(" and ") + sCode.Render("./deploy.sh") + sText.Render(";"),
		"  " + sText.Render("you can run the same scripts yourself. Every step is idempotent: run it again after a failure."),
		"",
		"  " + sSection.Render("What it creates in your AWS account"),
		row("Amazon EKS", fmt.Sprintf("cluster %s, Kubernetes %s, three managed node groups", v["CLUSTER_NAME"], v["KUBERNETES_VERSION"])),
		row("Amazon EC2", fmt.Sprintf("%d nodes: %d %s system, %d %s gVisor, %d %s microVM (nested virt)", total,
			m.nodes("system"), v["SYSTEM_INSTANCE_TYPE"], m.nodes("gvisor"), v["GVISOR_INSTANCE_TYPE"], m.nodes("microvm"), v["MICROVM_INSTANCE_TYPE"])),
		row("Amazon S3", "snapshot and asset buckets: private, encrypted, TLS only"),
		row("Amazon RDS", "PostgreSQL for Substrate, private, TLS verify-full"),
		row("Secrets Manager", "Substrate's two database logins"),
		row("Amazon ECR", "the counter demo image"),
		row("EKS Pod Identity", "one IAM role per service account; IMDSv2 with hop limit 1 on nodes"),
		row("Amazon Bedrock", "model calls through the in-cluster LLM gateway"),
		"",
		"    " + sBold.Render(pad("Time", 20)) + sText.Render("about 35 minutes: cluster ~20, Substrate ~15, demo check ~1; plugins add more"),
		"    " + sBold.Render(pad("Cost", 20)) + sText.Render(cost),
		"    " + pad("", 20) + sMuted.Render("Everything stays until you choose Teardown on the last screen."),
	}
	return strings.Join(lines, "\n")
}

// Hourly on-demand list prices in us-west-2 (AWS Price List API, 2026-11-05).
var ec2Price = map[string]float64{
	"m8i.large": 0.10584, "m8i.xlarge": 0.21168, "m8i.2xlarge": 0.42336, "m8i.4xlarge": 0.84672, "m8i.8xlarge": 1.69344,
}

// estimate is a rough hourly cost: nodes, EKS, one NAT gateway, RDS and
// the gp3 volumes (100 GB system, 200 GB gvisor and microvm, 20 GB RDS).
func estimate(v map[string]string) string {
	if v["AWS_REGION"] != "us-west-2" {
		return "see the AWS Pricing Calculator for " + v["AWS_REGION"] + " (about $2.45 an hour in us-west-2)"
	}
	total := 0.10 + 0.045 + 0.005 + 0.065 // EKS, NAT gateway + its IPv4 address, db.t4g.medium
	gb := 20.0
	for _, g := range []struct {
		name string
		disk float64
	}{{"SYSTEM", 100}, {"GVISOR", 200}, {"MICROVM", 200}} {
		n, _ := strconv.Atoi(v[g.name+"_NODES"])
		p, ok := ec2Price[v[g.name+"_INSTANCE_TYPE"]]
		if !ok {
			return "see the AWS Pricing Calculator for " + v[g.name+"_INSTANCE_TYPE"]
		}
		total += float64(n) * p
		gb += float64(n) * g.disk
	}
	total += gb * 0.08 / 730
	return fmt.Sprintf("about $%.2f an hour while it runs ($%.0f a day), on-demand, us-west-2", total, total*24)
}

// costShort is estimate without the Region note.
func costShort(v map[string]string) string {
	s := estimate(v)
	if i := strings.Index(s, " while it runs"); i > 0 {
		j := strings.Index(s, "),")
		if j > i {
			return s[:i] + s[i+len(" while it runs"):j+1] + " until teardown"
		}
	}
	return s
}

// ---------------------------------------------------------------- check

func (m Model) checkView(w, h int) string {
	var pass, warn, fail int
	for _, c := range m.checks {
		switch c.Status {
		case preflight.Pass:
			pass++
		case preflight.Warn:
			warn++
		case preflight.Fail:
			fail++
		}
	}
	var lines []string
	summary := sGreen.Render(fmt.Sprintf("%d passed", pass))
	if warn > 0 {
		summary += sDim.Render(" · ") + sYellow.Render(fmt.Sprintf("%d warning", warn))
	}
	if fail > 0 {
		summary += sDim.Render(" · ") + sRed.Render(fmt.Sprintf("%d failed", fail))
	}
	if m.pending > 0 {
		summary += sDim.Render(" · ") + m.spin.View() + sMuted.Render(fmt.Sprintf(" %d running", m.pending))
	}
	lines = append(lines, "", "  "+sTitle.Render("Check your setup")+"   "+summary, "")
	nameW := 24
	group := ""
	for _, c := range m.checks {
		if c.Group != group {
			if group != "" {
				lines = append(lines, "")
			}
			group = c.Group
			lines = append(lines, "  "+sSection.Render(group))
		}
		var icon string
		detail := sText.Render(c.Detail)
		switch c.Status {
		case preflight.Running:
			icon = m.spin.View()
			detail = sMuted.Render("checking…")
		case preflight.Pass:
			icon = sGreen.Render(iconDone)
		case preflight.Warn:
			icon = sYellow.Render(iconWarn)
			detail = sYellow.Render(c.Detail)
		case preflight.Fail:
			icon = sRed.Render(iconFail)
			detail = sRed.Render(c.Detail)
		}
		lines = append(lines, "    "+icon+" "+sBold.Render(pad(c.Name, nameW))+cut(detail, w-nameW-8))
		if c.Status == preflight.Fail && c.Fix != "" {
			lines = append(lines, "      "+pad("", nameW)+sMuted.Render("fix: "+c.Fix))
		}
	}
	lines = append(lines, "", "  "+sMuted.Render("Read-only: tool versions, aws sts get-caller-identity, the EC2 vCPU quota (L-1216C47A)."))
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- settings

func (m Model) settingsView(w, h int) string {
	leftW := 66
	rightW := w - leftW - 4
	var left []string
	left = append(left, "", "  "+sTitle.Render("Settings")+"   "+sMuted.Render("defaults from "+filepath.Join(filepath.Base(m.opt.Repo.Dir), "config.env")))
	section := ""
	cell := func(i int, wd int) string {
		f := m.fields[i]
		val := m.values[f.key]
		if m.edit && i == m.cursor {
			m.input.Width = wd - 2
			return pad(sText.Render(m.input.View()), wd)
		}
		shown := val
		if shown == "" {
			shown = "(SDK default chain)"
		}
		st := sText
		if val == "" {
			st = sMuted
		}
		if _, bad := m.errs[f.key]; bad {
			st = sRed
		}
		if i == m.cursor {
			return sSel.Render(pad(" "+cut(shown, wd-2)+" ", wd))
		}
		return " " + pad(st.Render(cut(shown, wd-2)), wd-1)
	}
	for i := 0; i < len(m.fields); i++ {
		f := m.fields[i]
		if f.kind == fDemo {
			break
		}
		if f.section != section {
			section = f.section
			left = append(left, "", "  "+sSection.Render(section))
			if section == "Node groups" {
				left = append(left, "    "+sMuted.Render(pad("group", 13)+pad("instance type", 24)+" nodes"))
			}
		}
		if f.group != "" {
			// One row per node group: instance type and count side by side.
			note := ""
			if f.group == "microvm" {
				note = sMuted.Render(" nested virt")
			}
			left = append(left, "    "+sBold.Render(pad(f.group, 12))+cell(i, 22)+"  "+cell(i+1, 6)+note)
			i++
			continue
		}
		left = append(left, "    "+sBold.Render(pad(f.label, 22))+cell(i, 40))
		if msg, bad := m.errs[f.key]; bad {
			left = append(left, "    "+pad("", 22)+sRed.Render(" "+cut(msg, 40)))
		}
	}
	left = append(left, "    "+sBold.Render(pad("Sandbox classes", 22))+" "+sGreen.Render(iconDone)+sText.Render(" gVisor  ")+sGreen.Render(iconDone)+sText.Render(" microVM  ")+sMuted.Render("(both, always)"))

	var right []string
	right = append(right, "", "")
	section = ""
	for i, f := range m.fields {
		if f.kind != fDemo {
			continue
		}
		if f.section != section {
			section = f.section
			right = append(right, "", "  "+sSection.Render(section))
		}
		d := m.demo(f.key)
		box := "[ ]"
		st := sText
		switch {
		case d.Locked:
			box = "[" + iconDone + "]"
		case d.Disabled != "":
			box = "[" + iconSkip + "]"
			st = sDim
		case m.demos[f.key]:
			box = "[" + iconDone + "]"
		}
		text := box + " " + d.Title
		if i == m.cursor {
			right = append(right, "  "+sSel.Render(pad(" "+text+" ", rightW-2)))
		} else {
			boxSt := sOrange
			if d.Disabled != "" {
				boxSt = sDim
			}
			right = append(right, "   "+boxSt.Render(box)+" "+st.Render(cut(d.Title, rightW-8)))
		}
	}
	// The help for the focused field: the comment above it in config.env,
	// or the demo's note.
	f := m.fields[m.cursor]
	right = append(right, "", "  "+sSection.Render("About this setting"))
	var help string
	if f.kind == fDemo {
		d := m.demo(f.key)
		help = d.Note
		if d.Disabled != "" {
			help = "Not available here: " + d.Disabled + "."
		}
	} else {
		help = m.opt.Repo.Defaults.Doc(f.key)
		if def, ok := m.opt.Repo.Defaults.Get(f.key); ok {
			help = strings.TrimSpace(help + "\nconfig.env: " + f.key + "=" + def)
		}
	}
	for _, l := range wrap(help, rightW-4) {
		right = append(right, "  "+sMuted.Render(l))
	}
	return hjoin(2, fit(strings.Join(left, "\n"), leftW, h), fit(strings.Join(right, "\n"), rightW, h))
}

// wrap breaks text into lines of at most w cells. Lines of a comment block
// join into paragraphs; indented lines (commands, examples) stay as they are.
func wrap(s string, w int) []string {
	var out []string
	para := ""
	flush := func() {
		line := ""
		for _, word := range strings.Fields(para) {
			if line != "" && lipgloss.Width(line)+1+lipgloss.Width(word) > w {
				out = append(out, line)
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		if line != "" {
			out = append(out, line)
		}
		para = ""
	}
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.TrimSpace(l) == "":
			flush()
		case strings.HasPrefix(l, " "):
			flush()
			out = append(out, cut(l, w))
		default:
			para += " " + l
		}
	}
	flush()
	return out
}

// ---------------------------------------------------------------- review

func (m Model) review(w, h int) string {
	leftW := 58
	rightW := w - leftW - 4
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cLine).Padding(0, 1).Width(leftW - 4)
	var cfg []string
	for _, l := range strings.Split(strings.TrimRight(m.localText, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "#"):
			cfg = append(cfg, sDim.Render(cut(l, leftW-8)))
		case strings.Contains(l, "="):
			k, v, _ := strings.Cut(l, "=")
			cfg = append(cfg, sCyan.Render(k)+sDim.Render("=")+sText.Render(cut(v, leftW-9-len(k))))
		default:
			cfg = append(cfg, l)
		}
	}
	where := "written to " + m.opt.Repo.Dir + "/"
	if m.opt.DryRun {
		where = "dry run: shown, not written"
	}
	left := []string{"", "  " + sTitle.Render("Review") + "   " + sMuted.Render("nothing has changed yet"), "",
		"  " + sSection.Render("config.local.env"), "  " + sMuted.Render(cut(where, leftW-4)),
		indent(box.Render(strings.Join(cfg, "\n")), 1)}

	right := []string{"", "", "", "  " + sSection.Render("Commands, in order") + "  " + sMuted.Render("in "+filepath.Base(m.opt.Repo.Dir)+"/")}
	total := 0
	// Three lines a command (gap, command, summary) when they fit with the 7
	// closing lines; one line a command when many are chosen.
	compact := len(right)+3*len(m.cmds)+7 > h
	if compact {
		right = append(right, "")
	}
	for i, c := range m.cmds {
		total += c.Minutes
		line := "  " + sOrange.Render(pad(fmt.Sprintf("%d", i+1), 2)) + " " + sCode.Render(pad(c.Display(), rightW-16)) + sMuted.Render(fmt.Sprintf("~%d min", c.Minutes))
		if compact {
			right = append(right, line)
			continue
		}
		right = append(right, "", line, "     "+sText.Render(cut(c.Summary, rightW-6)))
	}
	right = append(right, "",
		"  "+sMuted.Render(fmt.Sprintf("About %d minutes in all. The output streams to the", total)),
		"  "+sMuted.Render("Provision screen and to a log in .work/installer/."),
		"",
		"  "+sBold.Render("This creates billable AWS resources."),
		"  "+sText.Render(cut("Cost: "+costShort(m.values), rightW-4)),
	)
	if !m.opt.DryRun {
		right = append(right, "  "+sText.Render("Press ")+sKey.Render("y")+sText.Render(" to write config.local.env and start."))
	} else {
		right = append(right, "  "+sText.Render("Press ")+sKey.Render("y")+sText.Render(" to replay the recorded run."))
	}
	return hjoin(2, fit(strings.Join(left, "\n"), leftW, h), fit(strings.Join(right, "\n"), rightW, h))
}

func indent(s string, n int) string {
	p := strings.Repeat(" ", n)
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}

// ---------------------------------------------------------------- provision

func (m Model) statusIcon(s plan.Status) string {
	switch s {
	case plan.Running:
		return m.spin.View()
	case plan.Done:
		return sGreen.Render(iconDone)
	case plan.Failed:
		return sRed.Render(iconFail)
	case plan.Skipped:
		return sDim.Render(iconSkip)
	}
	return sDim.Render(iconPending)
}

func (m Model) checklist(w, h int) []string {
	now := m.now()
	var out []string
	for _, g := range m.groups {
		d := ""
		switch {
		case g.Status == plan.Running:
			d = dur(now.Sub(g.Start))
		case !g.End.IsZero():
			d = dur(g.End.Sub(g.Start))
		}
		title := sBold.Render(g.Cmd.Title)
		if g.Status == plan.Pending {
			title = sMuted.Render(g.Cmd.Title)
		}
		line := " " + m.statusIcon(g.Status) + " " + pad(title, w-11) + " " + sMuted.Render(fmt.Sprintf("%7s", d))
		out = append(out, line)
		out = append(out, "   "+sDim.Render(cut(g.Cmd.Display(), w-4)))
		if g.Status != plan.Running && g.Status != plan.Failed {
			continue
		}
		for _, r := range g.Rows {
			rd := ""
			if r.Status == plan.Running || r.Status == plan.Done || r.Status == plan.Failed {
				rd = dur(r.Duration(now))
			}
			st := sText
			switch r.Status {
			case plan.Pending:
				st = sMuted
			case plan.Skipped:
				st = sDim
			case plan.Running:
				st = sBold
			case plan.Failed:
				st = sRed
			}
			out = append(out, "   "+m.statusIcon(r.Status)+" "+pad(st.Render(r.Title), w-13)+" "+sMuted.Render(fmt.Sprintf("%7s", rd)))
			if r.Status == plan.Running && r.Detail != "" {
				out = append(out, "     "+sDim.Render(cut(r.Detail, w-6)))
			}
		}
	}
	if len(out) > h {
		// Keep the running group in view: drop finished lines from the top.
		out = out[len(out)-h:]
	}
	return out
}

func (m Model) provision(w, h int) string {
	now := m.now()
	leftW := 54
	rightW := w - leftW - 3

	// Status line and progress across all rows.
	var done, total int
	cur := ""
	for _, g := range m.groups {
		d, t := g.Progress()
		if g.Status == plan.Done {
			d = t
		}
		done += d
		total += t
		if r := g.Current(); r != nil {
			cur = r.Title
		}
	}
	verb := "Installing"
	if m.teardown {
		verb = "Tearing down"
	}
	var status string
	elapsed := now.Sub(m.runStart)
	if !m.runEnd.IsZero() {
		elapsed = m.runEnd.Sub(m.runStart)
	}
	switch {
	case m.running:
		status = m.spin.View() + " " + sBold.Render(verb+" "+m.values["CLUSTER_NAME"]) + sDim.Render("  ·  ") + sText.Render(cur)
	case m.exitCode != 0:
		status = sRed.Render(iconFail+" Failed") + sDim.Render("  ·  ") + sRed.Render(m.failedStep())
	case m.teardown:
		status = sGreen.Render(iconDone + " Teardown finished: nothing is left in AWS for " + m.values["CLUSTER_NAME"])
	default:
		status = sGreen.Render(iconDone + " Install finished")
	}
	right := sMuted.Render("started "+clock(m.runStart)+"  elapsed ") + sBold.Render(dur(elapsed)) + " "
	statusLine := " " + pad(status, w-2-lipgloss.Width(right)) + right

	barW := w - 22
	filled := 0
	if total > 0 {
		filled = barW * done / total
	}
	bar := " " + sOrange.Render(strings.Repeat("━", filled)) + sDim.Render(strings.Repeat("━", barW-filled)) +
		sMuted.Render(fmt.Sprintf("  %d of %d steps", done, total))

	bodyH := h - 4
	left := strings.Join(m.checklist(leftW, bodyH), "\n")

	// Log pane: the last lines, or where the reader scrolled to.
	logH := bodyH - 1
	lines := m.logLines
	end := len(lines) - m.scroll
	if end < 0 {
		end = 0
	}
	start := max(0, end-logH)
	var lp []string
	for _, l := range lines[start:end] {
		lp = append(lp, styleLog(cut(strings.ReplaceAll(l, "\t", "    "), rightW-2)))
	}
	title := sSection.Render("Log") + "  " + sDim.Render(cut(m.logPath, rightW-8))
	if m.scroll > 0 {
		title = sSection.Render("Log") + "  " + sYellow.Render(fmt.Sprintf("scrolled up %d lines (f follows)", m.scroll))
	}
	logPane := title + "\n" + strings.Join(lp, "\n")

	sep := lipgloss.NewStyle().Foreground(cLine).Render(strings.TrimRight(strings.Repeat("│\n", bodyH), "\n"))
	body := hjoin(1, fit(left, leftW, bodyH), sep, fit(logPane, rightW, bodyH))
	out := []string{statusLine, bar, "", body}
	if m.exitCode != 0 && !m.running {
		out[2] = " " + sRed.Render("The step failed: read the log above, fix the cause, and press r. Full log: ") + sCode.Render(cut(m.logPath, w-70))
	}
	return strings.Join(out, "\n")
}

func (m Model) failedStep() string {
	for _, g := range m.groups {
		if g.Status == plan.Failed {
			for _, r := range g.Rows {
				if r.Status == plan.Failed {
					return g.Cmd.Display() + ": " + r.Title
				}
			}
			return fmt.Sprintf("%s (exit %d)", g.Cmd.Display(), g.ExitCode)
		}
	}
	return fmt.Sprintf("exit %d", m.exitCode)
}

func styleLog(l string) string {
	switch {
	case strings.HasPrefix(l, "== "):
		return sCyan.Bold(true).Render(l)
	case strings.HasPrefix(l, "$ "):
		return sOrange.Render(l)
	case strings.HasPrefix(l, "WARN"):
		return sYellow.Render(l)
	case strings.HasPrefix(l, "ERROR") || strings.HasPrefix(l, "exit "):
		return sRed.Render(l)
	case strings.HasPrefix(l, "   "):
		return sText.Render(l)
	}
	return sMuted.Render(l)
}

// ---------------------------------------------------------------- done

func (m Model) done(w, h int) string {
	v := m.values
	c, r := v["CLUSTER_NAME"], v["AWS_REGION"]
	acct := m.account
	if acct == "" {
		acct = "<account>"
	}
	kv := func(k, d string) string { return "    " + sBold.Render(pad(k, 15)) + sText.Render(d) }
	title := "  " + sGreen.Render(iconDone) + " " + sTitle.Render("Agent Substrate is running on "+c) +
		sMuted.Render(fmt.Sprintf("   %s · finished %s · %s in all", r, clock(m.runEnd), dur(m.runEnd.Sub(m.runStart))))
	leftW := 62
	left := []string{
		"  " + sSection.Render("Created"),
		kv("EKS cluster", c+", Kubernetes "+v["KUBERNETES_VERSION"]),
		kv("Node groups", fmt.Sprintf("system  %d × %s", m.nodes("system"), v["SYSTEM_INSTANCE_TYPE"])),
		kv("", fmt.Sprintf("gvisor  %d × %s", m.nodes("gvisor"), v["GVISOR_INSTANCE_TYPE"])),
		kv("", fmt.Sprintf("microvm %d × %s, nested virt", m.nodes("microvm"), v["MICROVM_INSTANCE_TYPE"])),
		kv("Amazon S3", c+"-snap-"+acct+"-"+r),
		kv("", c+"-assets-"+acct+"-"+r),
		kv("Amazon RDS", c+"-pg, PostgreSQL, private"),
		kv("Secrets", c+"/ate-owner"),
		kv("", c+"/ate-readwrite"),
		kv("Amazon ECR", c+"/counter"),
		kv("IAM roles", "ate-api-server, atelet, llm-gateway"),
		kv("Demo", "counter actors on gVisor and microVM"),
		"",
		"  " + sSection.Render("Reach it"),
		"    " + sCode.Render("cd "+m.opt.Repo.Dir),
		"    " + sCode.Render("export KUBECONFIG=$PWD/.work/kubeconfig"),
		"    " + sCode.Render("export PATH=$PWD/.work/bin:$PATH"),
		"    " + sCode.Render("kubectl get nodes -L ate.dev/pool"),
		"    " + sCode.Render("kubectl ate get actors -A"),
		"",
		"  " + sSection.Render("Verify"),
		"    " + sCode.Render("demos/counter/verify.sh") + sMuted.Render("   suspend/resume, both classes"),
		"    " + sCode.Render("bash aws/inventory.sh") + sMuted.Render("     what the scripts made in AWS"),
	}
	rightW := w - leftW - 4
	right := []string{"  " + sSection.Render("Step times") + "  " + sMuted.Render("from the scripts' Timings tables")}
	for _, g := range m.groups {
		total := g.Total
		if total == 0 && !g.End.IsZero() {
			total = int(g.End.Sub(g.Start).Seconds())
		}
		right = append(right, "    "+sCode.Render(pad(g.Cmd.Display(), rightW-14))+sBold.Render(fmt.Sprintf("%8s", secs(total))))
		for _, t := range g.Timings {
			right = append(right, "      "+sMuted.Render(pad(t.Phase, rightW-16))+sText.Render(fmt.Sprintf("%8s", secs(t.Secs))))
		}
	}
	right = append(right, "",
		"  "+sSection.Render("Cost"),
		"    "+sText.Render(cut(costShort(v), rightW-4)),
		"",
		"  "+sSection.Render("Teardown")+"  "+sMuted.Render("press t, then type the cluster name"),
		"    "+sCode.Render("./clean.sh --yes")+sMuted.Render("  then  ")+sCode.Render("./delete-cluster.sh --yes"),
	)
	cols := hjoin(2, fit(strings.Join(left, "\n"), leftW, h-3), fit(strings.Join(right, "\n"), rightW, h-3))
	return "\n" + pad(title, w) + "\n\n" + cols
}

func (m Model) teardownView(w, h int) string {
	c := m.values["CLUSTER_NAME"]
	lines := []string{"",
		"  " + sRed.Bold(true).Render("Tear down "+c),
		"",
		"  " + sText.Render("This runs, in ") + sCode.Render(m.opt.Repo.Dir) + sText.Render(":"),
		"",
	}
	for i, t := range plan.Teardown() {
		lines = append(lines, "    "+sOrange.Render(fmt.Sprintf("%d", i+1))+"  "+sCode.Render(pad(t.Display(), 28))+sText.Render(t.Summary))
	}
	lines = append(lines, "",
		"  "+sText.Render("Both end with aws/inventory.sh and fail if anything they made is left."),
		"  "+sText.Render("Nothing comes back: RDS is deleted without a final snapshot, the buckets with their contents."),
		"",
		"  "+sBold.Render("Type the cluster name ("+c+") and press enter:"),
		"  "+m.confirm.View(),
	)
	return strings.Join(lines, "\n")
}
