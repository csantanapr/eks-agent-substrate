package ui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

func deps(checkStatus doctor.Status, clusters []awsx.Cluster) Deps {
	return Deps{
		Runner: execx.DryRun{Delay: 1},
		Checks: []doctor.Check{{Name: "aws", Run: func(context.Context) doctor.Result { return doctor.Result{Status: checkStatus} }}},
		Identity: func(context.Context) (string, string, error) {
			return "111122223333", "arn:aws:sts::111122223333:assumed-role/Admin/x", nil
		},
		Clusters: func(context.Context) ([]awsx.Cluster, error) { return clusters, nil },
		Bootstrap: func(*state.Setup) execx.Spec {
			return execx.Spec{SimLines: []string{"Step 1/4: Checking the EKS cluster..."}}
		},
		ControlPlane: func(*state.Setup) execx.Spec { return execx.Spec{SimLines: []string{"[step]: deploy_crds"}} },
	}
}

// drive presses enter and runs every returned command to completion, the way
// the Bubble Tea runtime would, until the model stops producing messages.
func drive(t *testing.T, m Model, key string) Model {
	t.Helper()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if key != "enter" {
		next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	}
	m = next.(Model)
	for cmd != nil {
		msg := cmd()
		if _, ok := msg.(tickMsg); ok {
			break
		}
		next, cmd = m.Update(msg)
		m = next.(Model)
	}
	return m
}

var good = []awsx.Cluster{{Name: "old", Version: "1.36", Status: "ACTIVE"}, {Name: "new", Version: "1.37", Status: "ACTIVE", AutoMode: true}}

func TestHappyPathReachesComplete(t *testing.T) {
	s := state.NewSetup()
	s.Region, s.ClusterName = "us-west-2", "new"
	m := New(deps(doctor.OK, good), s)
	for range 6 {
		m = drive(t, m, "enter")
	}
	if got := m.m.Current(); got != state.Complete {
		t.Fatalf("stopped at %s", got.Title())
	}
	if s.ClusterName != "new" || !s.AutoMode {
		t.Errorf("setup = %+v", s)
	}
	if !strings.Contains(m.View(), "Agent Substrate is running on new") {
		t.Error("complete screen missing")
	}
}

func TestFailedCheckBlocksTheWizard(t *testing.T) {
	m := New(deps(doctor.Fail, good), state.NewSetup())
	m = drive(t, m, "enter") // to CheckSetup
	m = drive(t, m, "enter") // must stay
	if got := m.m.Current(); got != state.CheckSetup {
		t.Fatalf("advanced past a failed check to %s", got.Title())
	}
}

func TestUnsupportedClusterCannotBeSelected(t *testing.T) {
	s := state.NewSetup()
	s.ClusterName = "old"
	m := New(deps(doctor.OK, good), s)
	for range 3 {
		m = drive(t, m, "enter") // CheckSetup, Account, Cluster
	}
	m = drive(t, m, "enter") // "old" is selected; must stay
	if got := m.m.Current(); got != state.Cluster {
		t.Fatalf("selected an unsupported cluster and moved to %s", got.Title())
	}
}

func TestAccountIsMaskedOnScreen(t *testing.T) {
	m := New(deps(doctor.OK, good), state.NewSetup())
	m = drive(t, m, "enter")
	m = drive(t, m, "enter")
	if v := m.View(); strings.Contains(v, "111122223333") {
		t.Error("account id shown in full")
	}
}
