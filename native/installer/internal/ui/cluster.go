package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// clusterScreen is substrate-gke's cluster screen for EKS: the clusters in
// the Region plus a create-new row, each badged with what the reinstall guard
// found on it (issue #5 R5).
type clusterScreen struct {
	list   []awsx.Cluster
	err    error
	loaded bool
	// cursor is a row of list, or len(list) for "Create a new cluster".
	cursor int
	// mode: "list"; "name" (new cluster name input); "probing" (checking the
	// selection for an install); "installed" or "partial" (the guard: the
	// selection already runs Substrate, or has a bare ate-system namespace);
	// "remove-confirm" and "removing" (removing that install, only on a
	// cluster this installer created).
	mode    string
	name    string
	confirm string
	// notice is a one-line refusal under the list (a cluster below 1.37).
	notice string
	// probed caches each cluster's verdict, so browsing back and forth does
	// not pay the probe again; pending marks a probe in flight and failed
	// one that could not run. [r] on the guard re-probes.
	probed  map[string]probe.Result
	pending map[string]bool
	failed  map[string]error
	// removal is the guard's --remove-control-plane run.
	removal *task
}

func newClusterScreen() *clusterScreen {
	return &clusterScreen{mode: "list", probed: map[string]probe.Result{}, pending: map[string]bool{}, failed: map[string]error{}}
}

type (
	clustersMsg struct {
		list []awsx.Cluster
		err  error
	}
	// probeMsg carries one probe's verdict, from the background or the
	// foreground: both go through the same Runner and parser.
	probeMsg struct {
		key string
		res probe.Result
		err error
	}
)

func (s *clusterScreen) selected() (awsx.Cluster, bool) {
	if s.cursor < 0 || s.cursor >= len(s.list) {
		return awsx.Cluster{}, false
	}
	return s.list[s.cursor], true
}

func (m *Model) enterCluster() tea.Cmd {
	cl := m.cl
	cl.loaded, cl.err, cl.mode, cl.notice = false, nil, "list", ""
	list := m.d.Clusters
	return func() tea.Msg {
		l, err := list(context.Background())
		return clustersMsg{l, err}
	}
}

// clustersLoaded shows the list, with the cursor on the preselected cluster
// or, when it names none, on the create row with that name ready to type,
// and starts the background probes.
func (m *Model) clustersLoaded(msg clustersMsg) tea.Cmd {
	cl := m.cl
	cl.list, cl.err, cl.loaded = msg.list, msg.err, true
	cl.cursor, cl.name = len(cl.list), m.s.ClusterName
	for i, c := range cl.list {
		if c.Name == m.s.ClusterName {
			cl.cursor, cl.name = i, ""
		}
	}
	return m.bgProbes()
}

// bgProbes probes every cluster that could take an install, one command
// each, so the list renders at once and each row picks up its badge as its
// result lands (AC5.1). The Real runner runs them one at a time.
func (m *Model) bgProbes() tea.Cmd {
	var cmds []tea.Cmd
	for _, c := range m.cl.list {
		if !c.Supported() || m.cl.pending[c.Name] {
			continue
		}
		if _, ok := m.cl.probed[c.Name]; ok {
			continue
		}
		cmds = append(cmds, m.probeCmd(c.Name))
	}
	return tea.Batch(cmds...)
}

func (m *Model) probeCmd(name string) tea.Cmd {
	m.cl.pending[name] = true
	delete(m.cl.failed, name)
	spec, runner := m.d.Probe(name, m.s), m.d.Runner
	return func() tea.Msg {
		var lines []string
		for ev := range runner.Start(context.Background(), spec) {
			if ev.Line != "" {
				lines = append(lines, ev.Line)
			}
			if ev.Done && ev.Err != nil {
				// The probe's own error line says why (kubectl Unauthorized,
				// no endpoint access); the exit status alone does not.
				if len(lines) > 0 {
					return probeMsg{key: name, err: fmt.Errorf("%w: %s", ev.Err, lines[len(lines)-1])}
				}
				return probeMsg{key: name, err: ev.Err}
			}
		}
		res, err := probe.ParseInstalled(lines)
		return probeMsg{key: name, res: res, err: err}
	}
}

// probed records a verdict and, when the screen is waiting on it for the
// selection, acts on it.
func (m *Model) probed(msg probeMsg) tea.Cmd {
	cl := m.cl
	delete(cl.pending, msg.key)
	if msg.err != nil {
		cl.failed[msg.key] = msg.err
	} else {
		cl.probed[msg.key] = msg.res
	}
	sel, ok := cl.selected()
	if m.m.Current() != state.Cluster || cl.mode != "probing" || !ok || sel.Name != msg.key || msg.err != nil {
		return nil
	}
	return m.decide(sel, msg.res)
}

// pick is enter on a cluster row: a cluster that cannot take Substrate is
// refused, one already probed is decided at once, and one whose probe is in
// flight waits for it rather than queueing a second.
func (m *Model) pick(c awsx.Cluster) tea.Cmd {
	cl := m.cl
	if !c.Supported() {
		cl.notice = fmt.Sprintf("%s is %s: Substrate needs an ACTIVE EKS 1.%d+ cluster.", c.Name, c.Badge(), awsx.MinMinor)
		return nil
	}
	cl.notice = ""
	if res, ok := cl.probed[c.Name]; ok {
		return m.decide(c, res)
	}
	cl.mode = "probing"
	if cl.pending[c.Name] {
		return nil
	}
	return m.probeCmd(c.Name)
}

// decide routes a verdict: a cluster with an install, whole or partial, is
// held on the guard (AC5.2), and a clean one is chosen.
func (m *Model) decide(c awsx.Cluster, res probe.Result) tea.Cmd {
	switch {
	case res.Partial():
		m.cl.mode = "partial"
	case res.Installed:
		m.cl.mode = "installed"
	default:
		m.cl.mode = "list"
		m.s.SelectCluster(c)
		return m.commit()
	}
	return nil
}

// nameProblem is why the typed name cannot be created, or "".
func (m *Model) nameProblem() string {
	name := strings.TrimSpace(m.cl.name)
	if err := awsx.ValidClusterName(name); err != nil {
		return strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."
	}
	for _, c := range m.cl.list {
		if c.Name == name {
			return name + " already exists in " + m.s.Region + ": choose it from the list."
		}
	}
	if v := m.s.NewClusterConfig.KubernetesVersion; !(awsx.Cluster{Version: v, Status: "ACTIVE"}).Supported() {
		return fmt.Sprintf("KUBERNETES_VERSION is %s; Substrate needs 1.%d or newer.", v, awsx.MinMinor)
	}
	return ""
}

func (m *Model) clusterKey(msg tea.KeyMsg) tea.Cmd {
	cl := m.cl
	key := msg.String()
	sel, _ := cl.selected()
	switch cl.mode {
	case "name":
		switch key {
		case "esc":
			cl.mode = "list"
		case "enter":
			if m.nameProblem() != "" {
				return nil
			}
			cl.mode = "list"
			m.s.NewCluster(strings.TrimSpace(cl.name))
			return m.commit()
		default:
			cl.name = edit(cl.name, msg)
		}
		return nil

	case "probing":
		switch key {
		case "r":
			if cl.failed[sel.Name] != nil {
				return m.probeCmd(sel.Name)
			}
		case "esc", "b":
			cl.mode = "list"
		}
		return nil

	case "installed", "partial":
		switch key {
		case "r":
			delete(cl.probed, sel.Name)
			cl.mode = "probing"
			return m.probeCmd(sel.Name)
		case "t":
			// Never offered for a cluster this installer did not create.
			if sel.CreatedByInstaller(m.s.Region) {
				cl.mode, cl.confirm = "remove-confirm", ""
			}
		case "esc", "b":
			cl.mode = "list"
		case "q":
			m.quitting = true
			return tea.Quit
		}
		return nil

	case "remove-confirm":
		switch key {
		case "esc":
			cl.mode = guardMode(cl.probed[sel.Name])
		case "enter":
			if strings.TrimSpace(cl.confirm) != sel.Name {
				return nil
			}
			return m.startRemoval(sel)
		default:
			cl.confirm = edit(cl.confirm, msg)
		}
		return nil

	case "removing":
		t := cl.removal
		switch {
		case !t.done:
		case key == "enter" && t.err != nil:
			return m.startRemoval(sel)
		case key == "esc" || key == "b":
			// The removal may have half-run: the cached verdict is the safe
			// answer until the cluster is probed again.
			cl.mode = "list"
		case key == "q":
			m.quitting = true
			return tea.Quit
		}
		return nil
	}

	// list mode
	switch key {
	case "up", "k":
		if cl.cursor > 0 {
			cl.cursor--
		}
		cl.notice = ""
	case "down", "j":
		if cl.cursor < len(cl.list) {
			cl.cursor++
		}
		cl.notice = ""
	case "G", "end":
		cl.cursor = len(cl.list)
	case "r":
		return m.enterCluster()
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	case "enter":
		// Not while the list failed or is loading: the create row's
		// duplicate-name check needs it.
		if !cl.loaded || cl.err != nil {
			return nil
		}
		if cl.cursor == len(cl.list) {
			cl.mode = "name"
			return nil
		}
		return m.pick(sel)
	}
	return nil
}

func guardMode(r probe.Result) string {
	if r.Partial() {
		return "partial"
	}
	return "installed"
}

// startRemoval runs --remove-control-plane against the guarded cluster. The
// selection is not committed to Setup yet, so it runs from a copy naming it.
func (m *Model) startRemoval(c awsx.Cluster) tea.Cmd {
	found := *m.s
	found.ClusterName = c.Name
	t := newTask(m.d.RemoveControlPlane(&found), steps.RemoveControlPlane())
	m.cl.removal, m.cl.mode = t, "removing"
	return m.run(t)
}

// removalDone probes the cluster again after a removal, and the verdict
// decides as any other: a clean cluster is chosen and the install continues
// there. The dry run's probe would replay "installed" forever, so its story
// is a clean cluster.
func (m *Model) removalDone() tea.Cmd {
	cl := m.cl
	sel, ok := cl.selected()
	if cl.removal.err != nil || !ok {
		return nil
	}
	delete(cl.probed, sel.Name)
	if m.d.DryRun {
		cl.probed[sel.Name] = probe.Result{}
		return m.decide(sel, probe.Result{})
	}
	cl.mode = "probing"
	return m.probeCmd(sel.Name)
}

func (m Model) clusterHints() []string {
	cl := m.cl
	sel, _ := cl.selected()
	switch cl.mode {
	case "name":
		return []string{"enter create with this name", "esc back to list", "ctrl+c quit"}
	case "probing":
		if cl.failed[sel.Name] != nil {
			return []string{"r retry", "esc choose another", "ctrl+c quit"}
		}
		return []string{"esc cancel", "ctrl+c quit"}
	case "installed", "partial":
		h := []string{"esc choose another", "r re-probe", "q quit"}
		if sel.CreatedByInstaller(m.s.Region) {
			h = append([]string{"t remove Substrate from it"}, h...)
		}
		return h
	case "remove-confirm":
		return []string{"enter remove it", "esc back", "ctrl+c quit"}
	case "removing":
		if cl.removal.done && cl.removal.err != nil {
			return []string{"enter retry", "esc back to list", "ctrl+c quit"}
		}
		return []string{"ctrl+c quit"}
	}
	return []string{"↑/↓ select", "enter confirm", "G new cluster", "r reload", "esc back", "q quit"}
}

// badge is a row's state: capability (Cluster.Badge) for a cluster that
// cannot take Substrate, otherwise what the guard found on it.
func (m Model) badge(c awsx.Cluster) string {
	cl := m.cl
	switch {
	case !c.Supported():
		return theme.Bad.Render(theme.GlyphFail + " " + c.Badge())
	case cl.pending[c.Name]:
		return theme.Fainted.Render("checking…")
	case cl.failed[c.Name] != nil:
		return theme.Warning.Render(theme.GlyphWarn + " not checked")
	}
	res, ok := cl.probed[c.Name]
	if !ok {
		return ""
	}
	if res.Installed {
		return theme.Warning.Render(theme.GlyphWarn + " " + res.Badge())
	}
	return theme.Good.Render(theme.GlyphDone + " " + c.Badge())
}

func (m Model) clusterView() string {
	cl := m.cl
	var b strings.Builder
	b.WriteString(theme.Title.Render("Connect your cluster") + "\n")
	b.WriteString(theme.Subtle.Render("EKS clusters in "+m.s.Region+", or a new one setup-aws creates") + "\n\n")
	switch {
	case !cl.loaded:
		b.WriteString(m.spinner() + " eks:ListClusters in " + m.s.Region + "…\n")
		return b.String()
	case cl.err != nil:
		b.WriteString(theme.Bad.Render(cl.err.Error()) + "\n" + theme.Subtle.Render("Press r to retry.") + "\n")
		return b.String()
	}
	rows := make([]string, 0, len(cl.list)+1)
	for _, c := range cl.list {
		mark := " "
		if c.CreatedByInstaller(m.s.Region) {
			mark = "*"
		}
		rows = append(rows, fmt.Sprintf("%-24s %-5s %s %s", c.Name, c.Version, mark, m.badge(c)))
	}
	rows = append(rows, "+ Create a new cluster")
	for i, r := range rows {
		if i == cl.cursor {
			b.WriteString(theme.Selected.Render("› "+r) + "\n")
		} else {
			b.WriteString("  " + r + "\n")
		}
	}
	b.WriteString(m.clusterPanel())
	return b.String()
}

// clusterPanel is what the mode puts under the list.
func (m Model) clusterPanel() string {
	cl := m.cl
	sel, _ := cl.selected()
	w := m.bodyWidth() - 6
	para := lipgloss.NewStyle().Width(w)
	var lines []string
	switch cl.mode {
	case "name":
		lines = []string{
			theme.Title.Render("New cluster name"),
			theme.Selected.Render("› " + cl.name + "▏"),
		}
		if p := m.nameProblem(); p != "" {
			lines = append(lines, theme.Bad.Render(p))
		}
		lines = append(lines, "",
			theme.Subtle.Render("Created in "+m.s.Region+" by setup-aws create cluster, with:"),
			para.Render(m.s.NewClusterConfig.Summary()),
			theme.Subtle.Render("Tags: "+m.s.Tags+" (from TAGS), plus the deployment owner tag."),
			theme.Fainted.Render("Change these with KUBERNETES_VERSION, AUTO_MODE, *_INSTANCE_TYPE, *_NODES, EKS_PUBLIC_ACCESS_CIDRS."))
	case "probing":
		if err := cl.failed[sel.Name]; err != nil {
			lines = []string{
				theme.Bad.Render("Could not check " + sel.Name + " for an existing Substrate install:"),
				para.Render(err.Error()),
				"",
				para.Render("A cluster the installer cannot read is not offered for install. Check your access to it (an EKS access entry for your role, and its API endpoint), then press r."),
			}
		} else {
			lines = []string{m.spinner() + " Checking " + sel.Name + " for an existing Substrate install…"}
		}
	case "installed", "partial":
		res := cl.probed[sel.Name]
		head := "Cluster " + sel.Name + " already runs Substrate."
		if res.Version != "" {
			head = "Cluster " + sel.Name + " already runs Substrate " + res.Version + "."
		}
		why := "Installing over it is not supported: it would leave a mixed-version cluster. To upgrade it, follow upstream's upgrade guide."
		if res.Partial() {
			head = fmt.Sprintf("Cluster %s has an ate-system namespace but no atelet.", sel.Name)
			why = "That is an interrupted install or an unfinished delete. Remove what is left before installing here."
		}
		lines = []string{theme.Bad.Render(head), "", para.Render(why), ""}
		if sel.CreatedByInstaller(m.s.Region) {
			lines = append(lines, para.Render("This installer created "+sel.Name+", so it can remove Substrate from it: "+
				theme.Key.Render("t")+" "+removalText(res)+" The install then continues here."))
		} else {
			lines = append(lines,
				para.Render("This installer did not create "+sel.Name+", so it will not remove anything from it: the cluster may run other people's workloads. Choose another cluster, or create a new one."),
				theme.Fainted.Render("(no "+awsx.CreatedByTagKey+"="+awsx.CreatedByValue+" tag on "+sel.Name+")"))
		}
	case "remove-confirm":
		lines = []string{
			theme.Warning.Render("Remove Substrate from " + sel.Name + "?"),
			"",
			"Type " + theme.Key.Render(sel.Name) + " to confirm:",
			theme.Selected.Render("› " + cl.confirm + "▏"),
			"",
			para.Render(confirmText(cl.probed[sel.Name])),
		}
	case "removing":
		lines = []string{"Removing Substrate from " + sel.Name + "…", "", m.taskView(cl.removal)}
	default:
		if cl.notice != "" {
			lines = append(lines, theme.Bad.Render(cl.notice))
		}
		lines = append(lines, theme.Subtle.Render(fmt.Sprintf("Substrate needs EKS 1.%d or newer (the PodCertificate APIs). * created by this installer.", awsx.MinMinor)))
	}
	return "\n" + strings.Join(lines, "\n") + "\n"
}

// pluginsRemoved names what --remove-control-plane removes before ate-system,
// as the probe read the plugin record: the recorded plugins, "every
// installed plugin" when the record could not be read, and "" when it lists
// none.
func pluginsRemoved(r probe.Result) string {
	switch n := len(r.Plugins); {
	case !r.PluginsRead:
		return "every installed plugin"
	case n == 0:
		return ""
	case n == 1:
		return "the installed plugin " + r.Plugins[0]
	default:
		return "the installed plugins " + strings.Join(r.Plugins[:n-1], ", ") + " and " + r.Plugins[n-1]
	}
}

// removalText is what t does on a guarded cluster, after "t ".
func removalText(r probe.Result) string {
	const del = "runs ate-setup delete ate-system, which deletes every actor, template and the control plane, and keeps the cluster and its "
	if p := pluginsRemoved(r); p != "" {
		return "removes " + p + ", Kubernetes objects and AWS resources alike, then " + del + "other AWS resources."
	}
	return del + "AWS resources."
}

// confirmText is the removal's confirmation detail.
func confirmText(r probe.Result) string {
	const del = "ate-setup delete ate-system. It deletes every actor (running, suspended or crashed), every ActorTemplate, atespace and WorkerPool, and the control plane. The cluster, its node groups and its "
	if p := pluginsRemoved(r); p != "" {
		return "Removes " + p + " (Kubernetes objects and AWS resources), then runs " + del + "other AWS resources are kept."
	}
	return "Runs " + del + "AWS resources are kept."
}
