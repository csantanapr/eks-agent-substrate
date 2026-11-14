package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// pluginsScreen chooses the optional parts of the install (plugin.All):
// selecting one selects what it requires, and clearing one clears what
// requires it. The choice is state.Setup.Plugins; the control plane step
// carries their needs and the Install the plugins step deploys them.
type pluginsScreen struct {
	cursor int
	chosen map[string]bool
	// shown: the defaults (or --plugins) have been applied once.
	shown bool
}

// resolved is ids with what they require, in install order; nil for an
// unknown ID (main refuses those before the wizard starts).
func resolved(ids []string) []plugin.Plugin {
	ps, err := plugin.Resolve(ids)
	if err != nil {
		return nil
	}
	return ps
}

func pluginTitles(ps []plugin.Plugin) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Title
	}
	return out
}

// unavailable is why p cannot be chosen for this run's cluster, or "".
func (m Model) unavailable(p plugin.Plugin) string {
	if p.NeedsAutoMode != "" && !m.s.AutoMode {
		return p.NeedsAutoMode + "; " + m.s.ClusterName + " has Auto Mode off"
	}
	return ""
}

// enterPlugins preselects, the first time, --plugins or else the Default
// plugins, with what they require. A preselected plugin this cluster cannot
// take is dropped, and the screen says so.
func (m *Model) enterPlugins() {
	pl := m.pl
	if !pl.shown {
		pl.shown = true
		ids := m.s.Plugins
		if ids == nil {
			for _, p := range plugin.All() {
				if p.Default {
					ids = append(ids, p.ID)
				}
			}
		}
		pl.chosen = map[string]bool{}
		for _, p := range resolved(ids) {
			pl.chosen[p.ID] = true
		}
	}
	var dropped []string
	for _, p := range plugin.All() {
		if pl.chosen[p.ID] && m.unavailable(p) != "" {
			dropped = append(dropped, p.ID)
			m.setPlugin(p.ID, false)
		}
	}
	if len(dropped) > 0 {
		m.notice = "not selected: " + strings.Join(dropped, ", ") + " (needs EKS Auto Mode)"
	}
	pl.cursor = min(pl.cursor, max(len(plugin.All())-1, 0))
}

// setPlugin selects id with everything it requires, or clears it with
// everything that requires it.
func (m *Model) setPlugin(id string, on bool) {
	pl := m.pl
	if on {
		for _, p := range resolved([]string{id}) {
			pl.chosen[p.ID] = true
		}
		return
	}
	pl.chosen[id] = false
	for _, p := range plugin.All() {
		if !pl.chosen[p.ID] || p.ID == id {
			continue
		}
		for _, r := range resolved([]string{p.ID}) {
			if r.ID == id {
				m.setPlugin(p.ID, false)
			}
		}
	}
}

// chosenIDs is the selection in install order, never nil: an empty choice is
// a choice, unlike a Setup that was never shown this screen.
func (m Model) chosenIDs() []string {
	ids := []string{}
	for _, p := range plugin.All() {
		if m.pl.chosen[p.ID] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

func (m *Model) pluginsKey(msg tea.KeyMsg) tea.Cmd {
	all := plugin.All()
	pl := m.pl
	switch msg.String() {
	case "up", "k":
		if pl.cursor > 0 {
			pl.cursor--
		}
	case "down", "j":
		if pl.cursor < len(all)-1 {
			pl.cursor++
		}
	case " ", "x":
		if len(all) == 0 {
			return nil
		}
		p := all[pl.cursor]
		if why := m.unavailable(p); why != "" {
			m.notice = p.Title + " " + why
			return nil
		}
		m.setPlugin(p.ID, !pl.chosen[p.ID])
	case "enter":
		m.s.Plugins = m.chosenIDs()
		return m.commit()
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	}
	return nil
}

// requiredBy names the chosen plugins that require id.
func (m Model) requiredBy(id string) []string {
	var out []string
	for _, p := range plugin.All() {
		if m.pl.chosen[p.ID] && p.ID != id && slices.ContainsFunc(resolved([]string{p.ID}), func(r plugin.Plugin) bool { return r.ID == id }) {
			out = append(out, p.ID)
		}
	}
	return out
}

func (m Model) pluginsView() string {
	all := plugin.All()
	var b strings.Builder
	b.WriteString(theme.Title.Render("Choose plugins") + "\n")
	b.WriteString(theme.Subtle.Render("Optional parts of the install. The control plane is installed once with what they need, then each is deployed and checked.") + "\n\n")
	if len(all) == 0 {
		b.WriteString("No plugin is built into this installer. Press enter to continue.\n")
		return b.String()
	}
	w := m.bodyWidth() - 2
	for i, p := range all {
		box := "[ ]"
		why := m.unavailable(p)
		switch {
		case why != "":
			box = "[–]"
		case m.pl.chosen[p.ID]:
			box = "[" + theme.GlyphDone + "]"
		}
		cost := "–"
		if p.HourlyUSD > 0 {
			cost = fmt.Sprintf("$%.2f/h", p.HourlyUSD)
		}
		note := ""
		switch {
		case why != "":
			note = "needs Auto Mode"
		case len(m.requiredBy(p.ID)) > 0:
			note = "for " + strings.Join(m.requiredBy(p.ID), ", ")
		case p.Default:
			note = "default"
		}
		row := box + " " + pad(truncate(p.Title, w-40), w-40) + " " + pad(truncate(note, 15), 15) + " " +
			pad(fmt.Sprintf("~%d min", p.Minutes), 7) + " " + cost
		switch {
		case i == m.pl.cursor:
			b.WriteString(theme.Selected.Render("› "+row) + "\n")
		case why != "":
			b.WriteString(theme.Fainted.Render("  "+row) + "\n")
		default:
			b.WriteString("  " + row + "\n")
		}
	}
	p := all[m.pl.cursor]
	para := lipgloss.NewStyle().Width(w - 4)
	b.WriteString("\n" + theme.Title.Render(p.Title) + "\n")
	b.WriteString(theme.Subtle.Render(indent(para.Render(p.Summary), "  ")) + "\n")
	if len(p.Requires) > 0 {
		b.WriteString(theme.Subtle.Render("  Requires "+strings.Join(p.Requires, ", ")+", which is selected with it.") + "\n")
	}
	if why := m.unavailable(p); why != "" {
		b.WriteString(theme.Warning.Render(indent(para.Render("Not available: "+why+"."), "  ")) + "\n")
	}
	ps := resolved(m.chosenIDs())
	minutes, usd := 0, 0.0
	for _, q := range ps {
		minutes += q.Minutes
		usd += q.HourlyUSD
	}
	b.WriteString("\n" + theme.Subtle.Render(fmt.Sprintf("Selected: %d plugin(s) · about %d min · %s", len(ps), minutes, perHour(usd))) + "\n")
	return b.String()
}

// perHour is a plugin's or a step's added cost, for the lists.
func perHour(usd float64) string {
	if usd == 0 {
		return "no hourly cost"
	}
	return fmt.Sprintf("+$%.2f an hour", usd)
}
