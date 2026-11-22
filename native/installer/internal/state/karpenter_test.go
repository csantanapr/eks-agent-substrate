package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// s-kata2 cannot run on Karpenter's nodes (mode 3, on the microvm group),
// and s-on-kata2 requires it.
func init() {
	plugin.Register(plugin.Plugin{ID: "s-kata2", Order: 9, Nodes: []string{"microvm"}, AddsNodes: true, NoKarpenter: "needs a label kata-deploy sets"})
	plugin.Register(plugin.Plugin{ID: "s-on-kata2", Order: 10, Requires: []string{"s-kata2"}})
}

// KARPENTER is a cluster knob: empty by default and in Env, before
// KARPENTER_NODE_OVERLAY, so setup-aws gets the run's value; Settings writes it; on, off and empty
// are its values, and config.local.env holds it only when it is not
// config.env's.
func TestKarpenterKnob(t *testing.T) {
	c := ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	if env := c.Env(); c.Karpenter != "" || c.KarpenterOn() || env[len(env)-2] != "KARPENTER=" {
		t.Errorf("default %q, env %q", c.Karpenter, env)
	}
	c = ClusterConfigFromEnv(func(k string) (string, bool) { return "on", k == "KARPENTER" })
	if env := c.Env(); !c.KarpenterOn() || env[len(env)-2] != "KARPENTER=on" {
		t.Errorf("on: env %q", env)
	}
	for _, v := range []string{"off", "On", "yes", " on"} {
		if (ClusterConfig{Karpenter: v}).KarpenterOn() {
			t.Errorf("KARPENTER=%q reads as on", v)
		}
	}
	if !slices.Contains(ManagedKeys, "KARPENTER") {
		t.Error("Settings does not write KARPENTER")
	}
	good := ConfigValues(NewSetupFrom(func(string) (string, bool) { return "", false }))
	if bad := ValidateConfig(good); len(bad) != 0 {
		t.Fatalf("defaults refused: %v", bad)
	}
	for value, ok := range map[string]bool{"on": true, "off": true, "": true, "On": false, "yes": false, "true": false, " on": false} {
		v := map[string]string{}
		for k, x := range good {
			v[k] = x
		}
		v["KARPENTER"] = value
		msg, bad := ValidateConfig(v)["KARPENTER"]
		if bad == ok || bad && msg != "on, off, or empty for off" {
			t.Errorf("KARPENTER=%q: refused=%v (%q)", value, bad, msg)
		}
	}
	cfg := Config{Defaults: ConfigFile{Entries: []ConfigEntry{{Key: "KARPENTER", Value: ""}}}}
	for value, written := range map[string]bool{"": false, "on": true} {
		v := map[string]string{"KARPENTER": value}
		text, err := cfg.RenderLocal(v)
		if err != nil || strings.Contains(text, "KARPENTER=") != written {
			t.Errorf("KARPENTER=%q in config.local.env: %v\n%s", value, err, text)
		}
	}
}

// With KARPENTER=on the gvisor group is a NodePool, named with its node
// limit; the system, microvm and nitro groups stay node groups. Without it
// the line is what it was.
func TestGroupsLineKarpenter(t *testing.T) {
	c := ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	c.MicroVMNodes = "3"
	all := []string{"system", "gvisor", "microvm", "nitro"}
	if got := c.GroupsLine(all); got != "system 2×m8i.xlarge · gvisor 2×m8i.2xlarge · microvm 3×m8i.2xlarge · nitro 1×m8i.xlarge" {
		t.Errorf("off: %q", got)
	}
	if got := c.PoolsLine(all); got != "" {
		t.Errorf("off: PoolsLine %q", got)
	}
	c.Karpenter = "on"
	if got := c.GroupsLine(all); got != "system 2×m8i.xlarge · microvm 3×m8i.2xlarge · nitro 1×m8i.xlarge · Karpenter NodePool gvisor up to 2×m8i.2xlarge" {
		t.Errorf("on: %q", got)
	}
	if got := c.GroupsLine([]string{"system", "microvm", "nitro"}); strings.Contains(got, "Karpenter") || c.PoolsLine([]string{"system", "microvm"}) != "" {
		t.Errorf("on, no pool: %q", got)
	}
	if got := c.Summary([]string{"system", "gvisor"}); !strings.Contains(got, " · Karpenter NodePool gvisor up to 2×m8i.2xlarge · API from auto") {
		t.Errorf("summary %q", got)
	}
	if ngs, pools := c.SplitPools(all); !slices.Equal(ngs, []string{"system", "microvm", "nitro"}) || !slices.Equal(pools, []string{"gvisor"}) ||
		!c.IsPool("gvisor") || c.IsPool("microvm") {
		t.Errorf("SplitPools %q %q", ngs, pools)
	}
	c.Karpenter = "off"
	if ngs, pools := c.SplitPools(all); !slices.Equal(ngs, all) || pools != nil || c.IsPool("gvisor") {
		t.Errorf("off: SplitPools %q %q", ngs, pools)
	}
}

// The run's Karpenter: KARPENTER for a new cluster, the probe's answer for a
// connected one. SelectCluster and NewCluster both write the connected
// cluster's NodePools and Karpenter, so neither leaks into the other path.
func TestSetupKarpenter(t *testing.T) {
	s := NewSetupFrom(func(k string) (string, bool) { return "on", k == "KARPENTER" })
	s.NewCluster("made")
	if !s.Karpenter() {
		t.Error("new cluster with KARPENTER=on")
	}
	s.SelectCluster(awsxCluster("c"), []string{"gvisor", "system"}, []string{"gvisor"}, false, plugin.Record{})
	if s.Karpenter() || !slices.Equal(s.ClusterNodePools, []string{"gvisor"}) {
		t.Errorf("connected without Karpenter: %+v", s)
	}
	s.SelectCluster(awsxCluster("c"), []string{"gvisor", "system"}, []string{"gvisor"}, true, plugin.Record{})
	if !s.Karpenter() {
		t.Error("connected with Karpenter")
	}
	s.NewClusterConfig.Karpenter = ""
	s.NewCluster("made")
	if s.Karpenter() || s.ClusterKarpenter || s.ClusterNodePools != nil {
		t.Errorf("after NewCluster: %+v", s)
	}
}

// Both ways: a plugin that cannot run on Karpenter's nodes, or one that
// requires it, is refused where its node group (microvm) is a Karpenter
// NodePool: a connected cluster whose microvm pool is one. KARPENTER=on
// makes only the gvisor group a NodePool, so on a new cluster with it, and
// on a connected cluster whose NodePool is gvisor's, it is offered.
func TestPluginProblemNoKarpenter(t *testing.T) {
	get := func(id string) plugin.Plugin { p, _ := plugin.Get(id); return p }
	s := &Setup{NewClusterConfig: ClusterConfig{Karpenter: "on"}, SandboxClasses: "gvisor"}
	s.NewCluster("c")
	for _, id := range []string{"s-kata2", "s-on-kata2", "s-claw"} {
		if why := s.PluginProblem(get(id), nil); why != "" {
			t.Errorf("new, on, %s: %q", id, why)
		}
	}
	s.SelectCluster(awsxCluster("c"), []string{"microvm", "system"}, []string{"microvm"}, true, plugin.Record{})
	for _, id := range []string{"s-kata2", "s-on-kata2"} {
		if why := s.PluginProblem(get(id), nil); why != "needs a label kata-deploy sets; the microvm node group of c is a Karpenter NodePool" {
			t.Errorf("connected, microvm a NodePool, %s: %q", id, why)
		}
	}
	s.SelectCluster(awsxCluster("c"), []string{"gvisor", "microvm", "system"}, []string{"gvisor"}, true, plugin.Record{})
	if why := s.PluginProblem(get("s-kata2"), nil); why != "" {
		t.Errorf("connected, gvisor a NodePool: %q", why)
	}
	s.SelectCluster(awsxCluster("c"), []string{"microvm", "system"}, nil, false, plugin.Record{})
	if why := s.PluginProblem(get("s-kata2"), nil); why != "" {
		t.Errorf("connected without Karpenter: %q", why)
	}
}

// On a connected cluster a pool that exists only as a Karpenter NodePool is
// a node group: its class and the plugins on it are offered, and it is
// named a NodePool. Without NodePools every group is a node group.
func TestConnectedNodePoolOnlyPool(t *testing.T) {
	get := func(id string) plugin.Plugin { p, _ := plugin.Get(id); return p }
	s := &Setup{SandboxClasses: "gvisor"}
	s.SelectCluster(awsxCluster("c"), []string{"gvisor", "microvm", "system"}, []string{"gvisor", "microvm"}, true, plugin.Record{})
	if why := ClassProblem(ClassGVisor, false, false, "c", s.ClusterPools, ""); why != "" {
		t.Errorf("gvisor class: %q", why)
	}
	if why := s.PluginProblem(get("s-claw"), nil); why != "" {
		t.Errorf("a plugin on gvisor: %q", why)
	}
	if groups, pools := s.ConnectedGroups(); !slices.Equal(groups, []string{"system"}) || !slices.Equal(pools, []string{"gvisor", "microvm"}) {
		t.Errorf("groups %q, NodePools %q", groups, pools)
	}
	s.SelectCluster(awsxCluster("c"), []string{"gvisor", "microvm", "system"}, nil, false, plugin.Record{})
	if groups, pools := s.ConnectedGroups(); !slices.Equal(groups, s.NodeGroups()) || pools != nil {
		t.Errorf("no NodePools: groups %q, NodePools %q", groups, pools)
	}
	s.SelectCluster(awsxCluster("c"), []string{"system"}, nil, false, plugin.Record{})
	if why := ClassProblem(ClassGVisor, false, false, "c", s.ClusterPools, ""); why == "" {
		t.Error("gvisor class with neither nodes nor a NodePool")
	}
}

// Both ways: TAGS may not set a key setup-aws sets itself (the Karpenter
// marker always; the owner and created-by keys with Karpenter), nor with
// Karpenter a key Karpenter sets on its nodes. Other keys are fine.
func TestTagsProblem(t *testing.T) {
	for _, c := range []struct {
		tags      string
		karpenter bool
		want      string
	}{
		{DefaultTags, false, ""},
		{DefaultTags, true, ""},
		{"a=b agent-substrate-on-eks/karpenter=off", false, "agent-substrate-on-eks/karpenter is a key setup-aws sets itself: remove it"},
		{"agent-substrate-on-eks/deployment=x", false, ""},
		{"agent-substrate-on-eks/deployment=x", true, "agent-substrate-on-eks/deployment is a key setup-aws sets itself: remove it"},
		{"agent-substrate-on-eks/created-by=x", true, "agent-substrate-on-eks/created-by is a key setup-aws sets itself: remove it"},
		{"a=b karpenter.sh/nodepool=x", false, ""},
		{"a=b karpenter.sh/nodepool=x", true, "karpenter.sh/nodepool is a key Karpenter sets itself: remove it"},
		{"kubernetes.io/cluster/c=owned", true, "kubernetes.io/cluster/c is a key Karpenter sets itself: remove it"},
		{"kubernetes.io/cluster=x", true, "kubernetes.io/cluster is a key Karpenter sets itself: remove it"},
		{"eks:eks-cluster-name=c", true, "eks:eks-cluster-name is a key Karpenter sets itself: remove it"},
		{"karpenter.k8s.aws/ec2nodeclass=x", true, "karpenter.k8s.aws/ec2nodeclass is a key Karpenter sets itself: remove it"},
		{"agent-substrate-on-eks/other=x karpenter=on", true, ""},
	} {
		if got := TagsProblem(c.tags, c.karpenter); got != c.want {
			t.Errorf("TagsProblem(%q, %v) = %q, want %q", c.tags, c.karpenter, got, c.want)
		}
	}
}
