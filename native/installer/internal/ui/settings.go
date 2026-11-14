package ui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// settingsScreen edits what config.env controls for the run: the new
// cluster's knobs (only when the run creates the cluster), TAGS, the release and its image repository,
// the sandbox classes and the database. Values are checked with
// state.ValidateConfig; n applies them to the Setup and moves on, and the
// install writes them to config.local.env when it starts (Review).
type settingsScreen struct {
	fields  []field
	cursor  int
	values  map[string]string // config keys, plus classKey and dbKey
	errs    map[string]string
	editing bool
	input   string
}

type fieldKind int

const (
	fText fieldKind = iota
	fToggle
	fClass    // one sandbox class, on or off
	fDatabase // RDS or the bundled PostgreSQL
)

// The values that are not config.env keys.
const (
	classKey = "sandbox classes"
	dbKey    = "database"
)

// field is one focusable setting. Node group rows pair an instance type
// with a node count (group set); the class row pairs gvisor and microvm.
type field struct {
	kind    fieldKind
	key     string // config key, or the sandbox class
	label   string
	section string
	group   string
}

func settingsFields(newCluster bool) []field {
	var f []field
	if newCluster {
		f = append(f,
			field{kind: fText, key: "KUBERNETES_VERSION", label: "Kubernetes version", section: "New cluster"},
			field{kind: fToggle, key: "AUTO_MODE", label: "EKS Auto Mode", section: "New cluster"},
			field{kind: fText, key: "EKS_PUBLIC_ACCESS_CIDRS", label: "API access CIDRs", section: "New cluster"})
		for _, g := range []string{"system", "gvisor", "microvm"} {
			up := strings.ToUpper(g)
			f = append(f,
				field{kind: fText, key: up + "_INSTANCE_TYPE", label: "instance type", section: "Node groups", group: g},
				field{kind: fText, key: up + "_NODES", label: "nodes", section: "Node groups", group: g})
		}
	}
	return append(f,
		field{kind: fText, key: "TAGS", label: "Tags", section: "Every AWS resource"},
		field{kind: fText, key: "SUBSTRATE_TAG", label: "Release", section: "Agent Substrate"},
		field{kind: fText, key: "SUBSTRATE_IMAGE_REPO", label: "Image repository", section: "Agent Substrate"},
		field{kind: fClass, key: "gvisor", label: "gVisor", section: "Agent Substrate", group: "classes"},
		field{kind: fClass, key: "microvm", label: "microVM", section: "Agent Substrate", group: "classes"},
		field{kind: fDatabase, key: dbKey, label: "Database", section: "Agent Substrate"})
}

// help is a setting's explanation when config.env has no comment above it.
var help = map[string]string{
	"KUBERNETES_VERSION":      "The EKS version of the new cluster. Substrate needs 1.37 or newer (the PodCertificate APIs).",
	"AUTO_MODE":               "EKS Auto Mode, with its general-purpose and system NodePools next to the three node groups. Fixed when the cluster is created.",
	"EKS_PUBLIC_ACCESS_CIDRS": "Address ranges allowed to reach the Kubernetes API endpoint, space-separated. Requests are still authenticated by IAM.",
	"TAGS":                    "Space-separated key=value tags on every AWS resource the install creates. The owner tag agent-substrate-on-eks/deployment=<cluster>.<region> is always added.",
	"SUBSTRATE_TAG":           "The Agent Substrate release to install, such as v1.0.0.",
	"SUBSTRATE_IMAGE_REPO":    "Where the release's images come from.",
	"gvisor":                  "Sandbox classes ate-setup installs. gVisor runs actors in a user-space kernel on the gvisor node group; at least one class is installed.",
	"microvm":                 "Sandbox classes ate-setup installs. microVM runs actors in Kata microVMs on the microvm node group, which needs nested virtualization; at least one class is installed.",
	dbKey:                     "Where the control plane keeps its state: Amazon RDS for PostgreSQL (adds the Create the database step), or the bundled PostgreSQL in the cluster.",
	"SYSTEM_INSTANCE_TYPE":    "The system node group runs the Substrate control plane.",
	"GVISOR_INSTANCE_TYPE":    "The gvisor node group runs gVisor actors.",
	"MICROVM_INSTANCE_TYPE":   "The microvm node group runs microVM actors; its instance type needs nested virtualization (m8i, c8i, r8i).",
}

func (m *Model) enterSettings() {
	st := m.set
	st.fields = settingsFields(m.s.ClusterIsNew)
	st.values = state.ConfigValues(m.s)
	st.values[classKey] = m.s.SandboxClasses
	st.values[dbKey] = m.s.Database
	st.errs = map[string]string{}
	st.editing = false
	st.cursor = min(st.cursor, len(st.fields)-1)
}

func (st *settingsScreen) classOn(c string) bool {
	return slices.Contains(strings.Split(st.values[classKey], ","), c)
}

// toggleClass turns a sandbox class on or off, keeping at least one and the
// order gvisor,microvm.
func (st *settingsScreen) toggleClass(c string) string {
	var on []string
	for _, k := range []string{"gvisor", "microvm"} {
		if st.classOn(k) != (k == c) {
			on = append(on, k)
		}
	}
	if len(on) == 0 {
		return "At least one sandbox class is installed."
	}
	st.values[classKey] = strings.Join(on, ",")
	return ""
}

// check validates every value, for the screen and for n.
func (st *settingsScreen) check() {
	st.errs = state.ValidateConfig(st.values)
	if err := state.ValidSandboxClasses(st.values[classKey]); err != nil {
		st.errs[classKey] = err.Error()
	}
}

// apply writes the answers to the Setup. A new cluster is recorded again,
// so its version and Auto Mode follow the edited knobs (review F13).
func (m *Model) applySettings() {
	v := m.set.values
	c := &m.s.NewClusterConfig
	for _, k := range []struct {
		key string
		dst *string
	}{
		{"KUBERNETES_VERSION", &c.KubernetesVersion}, {"AUTO_MODE", &c.AutoMode}, {"EKS_PUBLIC_ACCESS_CIDRS", &c.PublicAccessCIDRs},
		{"SYSTEM_INSTANCE_TYPE", &c.SystemInstanceType}, {"GVISOR_INSTANCE_TYPE", &c.GVisorInstanceType}, {"MICROVM_INSTANCE_TYPE", &c.MicroVMInstanceType},
		{"SYSTEM_NODES", &c.SystemNodes}, {"GVISOR_NODES", &c.GVisorNodes}, {"MICROVM_NODES", &c.MicroVMNodes},
		{"TAGS", &m.s.Tags}, {"SUBSTRATE_TAG", &m.s.SubstrateTag}, {"SUBSTRATE_IMAGE_REPO", &m.s.ImageRepo},
		{classKey, &m.s.SandboxClasses}, {dbKey, &m.s.Database},
	} {
		*k.dst = v[k.key]
	}
	if m.s.ClusterIsNew {
		m.s.NewCluster(m.s.ClusterName)
	}
}

func (m *Model) settingsKey(msg tea.KeyMsg) tea.Cmd {
	st := m.set
	f := st.fields[st.cursor]
	key := msg.String()
	if st.editing {
		switch key {
		case "enter":
			st.values[f.key] = strings.TrimSpace(st.input)
			st.editing = false
			st.check()
		case "esc":
			st.editing = false
		default:
			st.input = editN(st.input, msg, 200)
		}
		return nil
	}
	switch key {
	case "up", "k", "shift+tab":
		st.cursor = (st.cursor + len(st.fields) - 1) % len(st.fields)
	case "down", "j", "tab":
		st.cursor = (st.cursor + 1) % len(st.fields)
	case "left", "right":
		// Across a paired row: the instance type and its node count, or
		// the two sandbox classes.
		if f.group != "" {
			if key == "left" && st.cursor > 0 && st.fields[st.cursor-1].group == f.group {
				st.cursor--
			} else if key == "right" && st.cursor+1 < len(st.fields) && st.fields[st.cursor+1].group == f.group {
				st.cursor++
			}
		}
	case "enter", " ":
		switch f.kind {
		case fToggle:
			if st.values[f.key] == "on" {
				st.values[f.key] = ""
			} else {
				st.values[f.key] = "on"
			}
		case fClass:
			if why := st.toggleClass(f.key); why != "" {
				m.notice = why
			}
		case fDatabase:
			if st.values[dbKey] == state.DatabaseRDS {
				st.values[dbKey] = state.DatabaseBundled
			} else {
				st.values[dbKey] = state.DatabaseRDS
			}
		default:
			if key == "enter" {
				st.editing, st.input = true, st.values[f.key]
			}
		}
		st.check()
	case "n":
		st.check()
		if len(st.errs) > 0 {
			m.notice = fmt.Sprintf("%d setting(s) need a fix before the review", len(st.errs))
			return nil
		}
		m.applySettings()
		return m.commit()
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	}
	return nil
}

func (m Model) settingsHints() []string {
	if m.set.editing {
		return []string{"enter save", "esc cancel"}
	}
	return []string{"↑/↓ move", "enter edit / toggle", "n review", "esc back", "q quit"}
}

func (m Model) settingsView() string {
	st := m.set
	total := m.bodyWidth() - 2
	rightW := max(total-54, 24)
	leftW := total - rightW - 2
	src := "built-in defaults (no config.env found)"
	if d := m.d.Config.Dir; d != "" {
		src = "defaults from " + tildify(filepath.Join(d, "config.env")) + " and config.local.env"
	}
	left := []string{theme.Title.Render("Settings") + "  " + theme.Subtle.Render(truncate(src, leftW-10))}
	if !m.s.ClusterIsNew {
		auto := "Auto Mode off"
		if m.s.AutoMode {
			auto = "Auto Mode on"
		}
		left = append(left, "", theme.Title.Render("Cluster"),
			"  "+m.s.ClusterName+theme.Subtle.Render(fmt.Sprintf(" · EKS %s · %s", m.s.ClusterVersion, auto)),
			theme.Fainted.Render(indent(lipgloss.NewStyle().Width(leftW-2).Render(
				"connected, not created: the new-cluster settings apply only to a cluster this run creates."), "  ")))
	}
	cell := func(i, w int) string {
		f := st.fields[i]
		shown := st.values[f.key]
		style := lipgloss.NewStyle()
		switch f.kind {
		case fToggle:
			shown = "off"
			if st.values[f.key] == "on" {
				shown = "on"
			}
		case fClass:
			box := "[ ]"
			if st.classOn(f.key) {
				box = "[" + theme.GlyphDone + "]"
			}
			shown = box + " " + f.label
		case fDatabase:
			shown = "Amazon RDS for PostgreSQL"
			if st.values[dbKey] == state.DatabaseBundled {
				shown = "Bundled PostgreSQL"
			}
		}
		if st.editing && i == st.cursor {
			in := []rune(st.input)
			if len(in) > w-3 {
				in = in[len(in)-(w-3):] // the end, where the cursor is
			}
			return theme.Selected.Render(pad(" "+string(in)+"▏", w))
		}
		if _, bad := st.errs[f.key]; bad {
			style = theme.Bad
		}
		if f.kind == fClass {
			if _, bad := st.errs[classKey]; bad {
				style = theme.Bad
			}
		}
		if i == st.cursor {
			return theme.Selected.Render(pad(" "+truncate(shown, w-2), w))
		}
		return " " + style.Render(pad(truncate(shown, w-2), w-1))
	}
	section := ""
	for i := 0; i < len(st.fields); i++ {
		f := st.fields[i]
		if f.section != section {
			section = f.section
			left = append(left, "", theme.Title.Render(section))
			if section == "Node groups" {
				left = append(left, theme.Fainted.Render("  group     instance type      nodes"))
			}
		}
		switch {
		case f.group == "classes":
			left = append(left, "  "+pad("Sandbox classes", 18)+cell(i, 13)+" "+cell(i+1, 14))
			i++
		case f.group != "":
			note := ""
			if f.group == "microvm" {
				note = theme.Fainted.Render(" nested virt")
			}
			left = append(left, "  "+pad(f.group, 8)+cell(i, 18)+" "+cell(i+1, 5)+note)
			i++
		default:
			left = append(left, "  "+pad(f.label, 18)+cell(i, leftW-20))
		}
		if msg, bad := st.errs[f.key]; bad {
			left = append(left, "  "+pad("", 18)+theme.Bad.Render(truncate(msg, leftW-20)))
		}
	}
	if msg, bad := st.errs[classKey]; bad {
		left = append(left, "  "+theme.Bad.Render(truncate(msg, leftW-2)))
	}

	f := st.fields[st.cursor]
	right := []string{theme.Title.Render("About this setting")}
	text := help[f.key]
	if doc := m.d.Config.Defaults.Doc(f.key); doc != "" {
		text = doc
	}
	para := lipgloss.NewStyle().Width(rightW)
	right = append(right, theme.Subtle.Render(para.Render(joinParagraphs(text))))
	if f.kind == fText || f.kind == fToggle {
		right = append(right, "", theme.Fainted.Render(para.Render("Now: "+f.key+"="+st.values[f.key])))
		if def, ok := m.d.Config.Defaults.Get(f.key); ok {
			right = append(right, theme.Fainted.Render(para.Render("config.env: "+f.key+"="+def)))
		}
	}
	if st.editing {
		right = append(right, "", theme.Subtle.Render("Typing: enter saves, esc keeps the old value."))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftW).Render(strings.Join(left, "\n")), "  ",
		lipgloss.NewStyle().Width(rightW).Render(strings.Join(right, "\n")))
}

// joinParagraphs makes a config.env comment block flow: its lines join into
// one paragraph, and an indented line (a command, a link) is dropped, since
// the panel is narrow.
func joinParagraphs(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, " ") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(strings.Fields(strings.Join(out, " ")), " ")
}

// pad cuts or pads s (which may hold color codes) to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}
