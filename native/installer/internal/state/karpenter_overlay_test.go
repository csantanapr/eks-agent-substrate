package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// KARPENTER_NODE_OVERLAY is a cluster knob: empty by default and last in
// Env, so setup-aws gets the run's value; Settings writes it; on, off and
// empty are its values, config.local.env holds it only when it is not
// config.env's, and it is on only with KARPENTER=on.
func TestKarpenterNodeOverlayKnob(t *testing.T) {
	c := ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	if env := c.Env(); c.KarpenterNodeOverlay != "" || c.NodeOverlayOn() || env[len(env)-1] != "KARPENTER_NODE_OVERLAY=" {
		t.Errorf("default %q, env %q", c.KarpenterNodeOverlay, env)
	}
	c = ClusterConfigFromEnv(func(k string) (string, bool) { return "on", k == "KARPENTER" || k == "KARPENTER_NODE_OVERLAY" })
	if env := c.Env(); !c.NodeOverlayOn() || env[len(env)-1] != "KARPENTER_NODE_OVERLAY=on" {
		t.Errorf("on: env %q", env)
	}
	for _, cc := range []ClusterConfig{{KarpenterNodeOverlay: "on"}, {Karpenter: "off", KarpenterNodeOverlay: "on"}, {Karpenter: "on", KarpenterNodeOverlay: "On"}, {Karpenter: "on", KarpenterNodeOverlay: "off"}} {
		if cc.NodeOverlayOn() {
			t.Errorf("%+v reads as on", cc)
		}
	}
	if !slices.Contains(ManagedKeys, "KARPENTER_NODE_OVERLAY") {
		t.Error("Settings does not write KARPENTER_NODE_OVERLAY")
	}
	good := ConfigValues(NewSetupFrom(func(string) (string, bool) { return "", false }))
	for value, ok := range map[string]bool{"on": true, "off": true, "": true, "On": false, "yes": false} {
		v := map[string]string{}
		for k, x := range good {
			v[k] = x
		}
		v["KARPENTER_NODE_OVERLAY"] = value
		msg, bad := ValidateConfig(v)["KARPENTER_NODE_OVERLAY"]
		if bad == ok || bad && msg != "on, off, or empty for off" {
			t.Errorf("KARPENTER_NODE_OVERLAY=%q: refused=%v (%q)", value, bad, msg)
		}
	}
	cfg := Config{Defaults: ConfigFile{Entries: []ConfigEntry{{Key: "KARPENTER_NODE_OVERLAY", Value: ""}}}}
	for value, written := range map[string]bool{"": false, "on": true} {
		text, err := cfg.RenderLocal(map[string]string{"KARPENTER_NODE_OVERLAY": value})
		if err != nil || strings.Contains(text, "KARPENTER_NODE_OVERLAY=") != written {
			t.Errorf("KARPENTER_NODE_OVERLAY=%q in config.local.env: %v\n%s", value, err, text)
		}
	}
}

// With KARPENTER_NODE_OVERLAY=on the gvisor, microvm and nitro groups are
// NodePools, named with their node limits, and only the system group stays
// a node group. Without KARPENTER the overlay does nothing.
func TestGroupsLineNodeOverlay(t *testing.T) {
	c := ClusterConfigFromEnv(func(string) (string, bool) { return "", false })
	c.Karpenter, c.KarpenterNodeOverlay, c.MicroVMNodes = "on", "on", "3"
	all := []string{"system", "gvisor", "microvm", "nitro"}
	if ngs, pools := c.SplitPools(all); !slices.Equal(ngs, []string{"system"}) || !slices.Equal(pools, []string{"gvisor", "microvm", "nitro"}) ||
		!c.IsPool("nitro") || !c.IsPool("microvm") || c.IsPool("system") {
		t.Errorf("SplitPools %q %q", ngs, pools)
	}
	if got := c.GroupsLine(all); got != "system 2×m8i.xlarge · Karpenter NodePools gvisor up to 2×m8i.2xlarge, microvm up to 3×m8i.2xlarge, nitro up to 1×m8i.xlarge" {
		t.Errorf("GroupsLine %q", got)
	}
	c.MicroVMNodes = "2"
	if got := c.PoolsLine([]string{"gvisor", "microvm"}); got != "NodePools gvisor, microvm, up to 2×m8i.2xlarge each" {
		t.Errorf("PoolsLine %q", got)
	}
	c.Karpenter = ""
	if ngs, pools := c.SplitPools(all); !slices.Equal(ngs, all) || pools != nil {
		t.Errorf("overlay without KARPENTER: %q %q", ngs, pools)
	}
}

// Both ways: a plugin that cannot run on Karpenter's nodes on the microvm
// group (mode 3) is refused on a new cluster with KARPENTER_NODE_OVERLAY=on,
// naming that setting, and offered with KARPENTER=on alone. A connected
// cluster whose nitro pool exists only as a NodePool offers the nitro class
// on node groups.
func TestPluginProblemNodeOverlay(t *testing.T) {
	get := func(id string) plugin.Plugin { p, _ := plugin.Get(id); return p }
	s := &Setup{NewClusterConfig: ClusterConfig{Karpenter: "on", KarpenterNodeOverlay: "on"}, SandboxClasses: "gvisor,microvm"}
	s.NewCluster("c")
	want := "needs a label kata-deploy sets; KARPENTER_NODE_OVERLAY makes the microvm node group a Karpenter NodePool (Settings: Karpenter overlay)"
	for _, id := range []string{"s-kata2", "s-on-kata2"} {
		if why := s.PluginProblem(get(id), nil); why != want {
			t.Errorf("new, overlay, %s: %q", id, why)
		}
	}
	s.NewClusterConfig.KarpenterNodeOverlay = ""
	if why := s.PluginProblem(get("s-kata2"), nil); why != "" {
		t.Errorf("new, KARPENTER=on alone: %q", why)
	}
	s.SelectCluster(awsxCluster("c"), []string{"gvisor", "microvm", "nitro", "system"}, []string{"gvisor", "microvm", "nitro"}, true)
	if why := ClassProblem(ClassNitro, false, false, "c", s.ClusterPools, plugin.NitroNodeGroup); why != "" {
		t.Errorf("nitro on a NodePool: %q", why)
	}
	if why := s.PluginProblem(get("s-kata2"), nil); why != "needs a label kata-deploy sets; the microvm node group of c is a Karpenter NodePool" {
		t.Errorf("connected, microvm a NodePool: %q", why)
	}
}
