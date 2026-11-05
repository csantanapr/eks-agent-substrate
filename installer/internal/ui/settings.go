package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/config"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
)

type fieldKind int

const (
	fText fieldKind = iota
	fDemo
)

// field is one focusable setting: a config key, or a demo checkbox.
type field struct {
	kind    fieldKind
	key     string // config key, or the demo id
	label   string
	section string
	group   string // node group rows: system, gvisor, microvm
}

func settingsFields() []field {
	f := []field{
		{kind: fText, key: "CLUSTER_NAME", label: "Cluster name", section: "Cluster"},
		{kind: fText, key: "AWS_REGION", label: "AWS Region", section: "Cluster"},
		{kind: fText, key: "AWS_PROFILE", label: "AWS profile", section: "Cluster"},
		{kind: fText, key: "KUBERNETES_VERSION", label: "Kubernetes version", section: "Cluster"},
		{kind: fText, key: "TAGS", label: "Tags", section: "Cluster"},
		{kind: fText, key: "EKS_PUBLIC_ACCESS_CIDRS", label: "API access CIDRs", section: "Cluster"},
	}
	for _, g := range []string{"system", "gvisor", "microvm"} {
		up := strings.ToUpper(g)
		f = append(f,
			field{kind: fText, key: up + "_INSTANCE_TYPE", label: "instance type", section: "Node groups", group: g},
			field{kind: fText, key: up + "_NODES", label: "nodes", section: "Node groups", group: g})
	}
	f = append(f, field{kind: fText, key: "SUBSTRATE_TAG", label: "Release tag", section: "Agent Substrate"})
	for _, d := range plan.Demos {
		f = append(f, field{kind: fDemo, key: d.ID, label: d.Title, section: "Demos"})
	}
	return f
}

// validateOne returns the validation message for key alone, if any.
func validateOne(values map[string]string, key string) map[string]string {
	if msg, ok := config.Validate(values)[key]; ok {
		return map[string]string{key: msg}
	}
	return nil
}

func demoByID(id string) plan.Demo {
	for _, d := range plan.Demos {
		if d.ID == id {
			return d
		}
	}
	return plan.Demo{}
}

func (m Model) settingsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	f := m.fields[m.cursor]
	if m.edit {
		switch key {
		case "enter":
			m.values[f.key] = strings.TrimSpace(m.input.Value())
			m.edit = false
			m.input.Blur()
			delete(m.errs, f.key)
			for ek, ev := range validateOne(m.values, f.key) {
				m.errs[ek] = ev
			}
			return m, nil
		case "esc":
			m.edit = false
			m.input.Blur()
			return m, nil
		}
		var c tea.Cmd
		m.input, c = m.input.Update(k)
		return m, c
	}
	switch key {
	case "up", "k", "shift+tab":
		m.cursor = (m.cursor + len(m.fields) - 1) % len(m.fields)
	case "down", "j", "tab":
		m.cursor = (m.cursor + 1) % len(m.fields)
	case "left", "right":
		if f.group != "" {
			if key == "left" && strings.HasSuffix(f.key, "_NODES") {
				m.cursor--
			} else if key == "right" && strings.HasSuffix(f.key, "_INSTANCE_TYPE") {
				m.cursor++
			}
		}
	case "enter", " ", "space":
		if f.kind == fDemo {
			d := demoByID(f.key)
			switch {
			case d.Locked:
				m.msg = d.Title + ": " + d.Note
			case d.Disabled != "":
				m.msg = d.Title + ": " + d.Disabled
			default:
				m.demos[f.key] = !m.demos[f.key]
				if f.key == "s3files" && m.demos["s3files"] {
					m.demos["efs"] = true
				}
				if f.key == "efs" && !m.demos["efs"] {
					m.demos["s3files"] = false
				}
			}
			return m, nil
		}
		if key == " " || key == "space" {
			return m, nil
		}
		m.edit = true
		m.input.SetValue(m.values[f.key])
		m.input.CursorEnd()
		m.input.Width = 40
		m.input.Focus()
		return m, textinput.Blink
	case "n":
		return m.toReview()
	case "b", "esc":
		m.screen = sCheck
	case "q":
		return m, tea.Quit
	}
	return m, nil
}
