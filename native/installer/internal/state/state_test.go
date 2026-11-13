package state

import (
	"slices"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

func TestInstallOrderAddsTheOptionalSteps(t *testing.T) {
	for _, tc := range []struct {
		s    Setup
		want []Step
	}{
		{Setup{Database: DatabaseRDS}, Order},
		{Setup{Database: DatabaseBundled}, []Step{Welcome, CheckSetup, Account, Cluster, Database, Provision, ControlPlane, Complete}},
		{Setup{Database: DatabaseRDS, ClusterIsNew: true, DemoMode: true},
			[]Step{Welcome, CheckSetup, Account, Cluster, Database, CreateCluster, Provision, RDS, ControlPlane, Demo, Complete}},
	} {
		if got := InstallOrder(&tc.s); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.s, got, tc.want)
		}
	}
}

// The rail is 30 columns and a two-digit step's prefix takes seven.
func TestStepTitlesFitTheRail(t *testing.T) {
	for s := Welcome; s <= TeardownAWS; s++ {
		if w := lipgloss.Width(s.Title()); s.Title() == "?" || w > 23 {
			t.Errorf("step %d title %q (%d columns)", s, s.Title(), w)
		}
	}
}

func TestClusterConfigFromEnv(t *testing.T) {
	env := map[string]string{"KUBERNETES_VERSION": "1.38", "AUTO_MODE": "", "MICROVM_NODES": "3"}
	c := ClusterConfigFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if c.KubernetesVersion != "1.38" || c.AutoMode != "" || c.MicroVMNodes != "3" || c.SystemInstanceType != "m8i.xlarge" {
		t.Errorf("config %+v", c)
	}
	if !slices.Contains(c.Env(), "AUTO_MODE=") || !slices.Contains(c.Env(), "EKS_PUBLIC_ACCESS_CIDRS=0.0.0.0/0") {
		t.Errorf("env %v", c.Env())
	}
}

// SelectCluster and NewCluster write every Cluster* field, so an abandoned
// choice cannot leak into the other path.
func TestSelectAndNewClusterOverwriteEachOther(t *testing.T) {
	s := &Setup{Region: "us-west-2", NewClusterConfig: ClusterConfig{KubernetesVersion: "1.37", AutoMode: "on"}}
	s.NewCluster("made")
	s.SelectCluster(awsx.Cluster{Name: "brought", Version: "1.38"})
	if s.ClusterIsNew || s.ClusterOwned || s.AutoMode || s.ClusterVersion != "1.38" {
		t.Errorf("after SelectCluster: %+v", s)
	}
	s.NewCluster("made")
	if !s.ClusterIsNew || !s.ClusterOwned || !s.AutoMode || s.ClusterName != "made" {
		t.Errorf("after NewCluster: %+v", s)
	}
}
