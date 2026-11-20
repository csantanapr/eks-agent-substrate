package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

func TestInstallOrderAddsTheOptionalSteps(t *testing.T) {
	choose := []Step{Welcome, CheckSetup, Account, Cluster, Database, Settings, Plugins, Review}
	for _, tc := range []struct {
		s    Setup
		want []Step
	}{
		{Setup{Database: DatabaseRDS}, Order},
		{Setup{Database: DatabaseBundled}, append(slices.Clone(choose), Provision, ControlPlane, Complete)},
		{Setup{Database: DatabaseRDS, ClusterIsNew: true, DemoMode: true},
			append(slices.Clone(choose), CreateCluster, Provision, RDS, ControlPlane, Demo, Complete)},
		// A chosen plugin adds its step last, after the demo (efs builds on
		// the counter demo's templates); an empty choice adds none.
		{Setup{Database: DatabaseBundled, Plugins: []string{"efs"}, DemoMode: true},
			append(slices.Clone(choose), Provision, ControlPlane, Demo, InstallPlugins, Complete)},
		{Setup{Database: DatabaseBundled, Plugins: []string{"efs"}},
			append(slices.Clone(choose), Provision, ControlPlane, InstallPlugins, Complete)},
		{Setup{Database: DatabaseBundled, Plugins: []string{}}, append(slices.Clone(choose), Provision, ControlPlane, Complete)},
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
	if !slices.Contains(c.Env(), "AUTO_MODE=") || !slices.Contains(c.Env(), "EKS_PUBLIC_ACCESS_CIDRS=auto") {
		t.Errorf("env %v", c.Env())
	}
}

// NODE_GROUP_TYPE defaults to managed and reaches setup-aws's environment;
// the summary says self-managed only when it is, and only with node groups.
func TestClusterConfigNodeGroupType(t *testing.T) {
	c := ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	if c.NodeGroupType != "managed" || !slices.Contains(c.Env(), "NODE_GROUP_TYPE=managed") {
		t.Errorf("default: %q, env %v", c.NodeGroupType, c.Env())
	}
	groups := []string{"system", "gvisor"}
	if got := c.Summary(groups); strings.Contains(got, "self-managed") {
		t.Errorf("managed summary %q", got)
	}
	c = ClusterConfigFromEnv(func(k string) (string, bool) { return "self-managed", k == "NODE_GROUP_TYPE" })
	if got := c.Summary(groups); !strings.HasSuffix(got, " · self-managed node groups") {
		t.Errorf("self-managed summary %q", got)
	}
	if got := c.Summary(nil); strings.Contains(got, "self-managed") {
		t.Errorf("self-managed with no node group: %q", got)
	}
	if !slices.Contains(ManagedKeys, "NODE_GROUP_TYPE") {
		t.Error("Settings does not write NODE_GROUP_TYPE")
	}
}

// The release, its image repository and TAGS come from the lookup (config.env
// under the environment); an empty TAGS stays empty, an empty release does
// not.
func TestNewSetupFromReadsTheReleaseAndTags(t *testing.T) {
	env := map[string]string{"SUBSTRATE_TAG": "v1.0.1", "SUBSTRATE_IMAGE_REPO": "registry.example.com/substrate", "TAGS": ""}
	s := NewSetupFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if s.SubstrateTag != "v1.0.1" || s.ImageRepo != "registry.example.com/substrate" || s.Tags != "" || s.SandboxClasses != DefaultSandboxClasses {
		t.Errorf("setup %+v", s)
	}
	s = NewSetupFrom(func(k string) (string, bool) { return "", k == "SUBSTRATE_TAG" })
	if s.SubstrateTag != DefaultTag || s.ImageRepo != DefaultImageRepo || s.Tags != DefaultTags {
		t.Errorf("defaults %+v", s)
	}
}

func TestValidSandboxClasses(t *testing.T) {
	for _, ok := range []string{"gvisor", "microvm", "nitro", "gvisor,microvm", "microvm,gvisor", "gvisor,nitro", "gvisor,microvm,nitro"} {
		if err := ValidSandboxClasses(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "kata", "gvisor,gvisor", "gvisor,", "gvisor microvm", "nitro,nitro", "Nitro"} {
		if ValidSandboxClasses(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// SelectCluster and NewCluster write every Cluster* field, so an abandoned
// choice cannot leak into the other path.
func TestSelectAndNewClusterOverwriteEachOther(t *testing.T) {
	s := &Setup{Region: "us-west-2", NewClusterConfig: ClusterConfig{KubernetesVersion: "1.37", AutoMode: "on"}}
	s.NewCluster("made")
	s.SelectCluster(awsx.Cluster{Name: "brought", Version: "1.38"}, []string{"gvisor"}, nil, false)
	if s.ClusterIsNew || s.ClusterOwned || s.AutoMode || s.ClusterVersion != "1.38" || len(s.ClusterPools) != 1 {
		t.Errorf("after SelectCluster: %+v", s)
	}
	s.NewCluster("made")
	if !s.ClusterIsNew || !s.ClusterOwned || !s.AutoMode || s.ClusterName != "made" || s.ClusterPools != nil {
		t.Errorf("after NewCluster: %+v", s)
	}
}

// The nitro knobs: config.env's defaults, in Env after the others; the
// nitro group's nodes and type; and the Summary names the Nitro AMI only
// when the nitro group is made (and NODE_AMI_ID only for the other groups).
func TestNitroKnobs(t *testing.T) {
	c := ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	if c.NitroCompute != plugin.NitroAutoMode || c.NitroAMIID != "" || c.NitroInstanceType != "m8i.xlarge" || c.NitroNodes != "1" {
		t.Errorf("defaults %+v", c)
	}
	env := c.Env()
	if want := []string{"NODE_GROUP_TYPE=managed", "NITRO_COMPUTE=auto-mode", "NITRO_AMI_ID=", "NITRO_INSTANCE_TYPE=m8i.xlarge", "NITRO_NODES=1", "KARPENTER=",
		"KARPENTER_NODE_OVERLAY="}; !slices.Equal(env[len(env)-7:], want) {
		t.Errorf("env %q", env)
	}
	c = ClusterConfigFromEnv(func(k string) (string, bool) {
		return map[string]string{"NITRO_COMPUTE": "node-group", "NITRO_AMI_ID": "ami-0a17000000000000e", "NITRO_INSTANCE_TYPE": "m8i.2xlarge", "NITRO_NODES": "3",
			"NODE_AMI_ID": "ami-0123456789abcdef0"}[k], k == "NITRO_COMPUTE" || k == "NITRO_AMI_ID" || k == "NITRO_INSTANCE_TYPE" || k == "NITRO_NODES" || k == "NODE_AMI_ID"
	})
	if n, typ := c.Group("nitro"); n != "3" || typ != "m8i.2xlarge" {
		t.Errorf("Group(nitro) = %s, %s", n, typ)
	}
	if got := c.GroupsLine([]string{"system", "nitro"}); got != "system 2×m8i.xlarge · nitro 3×m8i.2xlarge" {
		t.Errorf("GroupsLine %q", got)
	}
	if got := c.Summary([]string{"system", "nitro"}); !strings.Contains(got, " · node AMI ami-0123456789abcdef0 · Nitro AMI ami-0a17000000000000e") {
		t.Errorf("Summary %q", got)
	}
	if got := c.Summary([]string{"nitro"}); strings.Contains(got, "node AMI") || !strings.HasSuffix(got, " · Nitro AMI ami-0a17000000000000e") {
		t.Errorf("Summary of the nitro group alone %q", got)
	}
	if got := c.Summary([]string{"system", "gvisor"}); strings.Contains(got, "Nitro") {
		t.Errorf("Summary without the nitro group %q", got)
	}
}
