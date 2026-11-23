package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
)

// KARPENTER_NODE_OVERLAY=on reaches setup-aws and is shown after
// KARPENTER=on, as a knob a pasted command needs; the dry run replays the
// same Karpenter phases as KARPENTER=on alone. Both ways: with KARPENTER=on
// alone it is not shown and the environment carries it empty; without
// KARPENTER it is not shown either, since it does nothing then (Settings
// refuses it).
func TestCreateClusterShowsNodeOverlay(t *testing.T) {
	b, s := testBuilder()
	s.NewClusterConfig.AutoMode, s.NewClusterConfig.Karpenter, s.NewClusterConfig.KarpenterNodeOverlay = "", "on", "on"
	s.NewCluster("demo")
	for _, typ := range []string{"managed", "self-managed"} {
		s.NewClusterConfig.NodeGroupType = typ
		sp := b.createCluster(s)
		if !strings.HasPrefix(sp.Display, "NODE_GROUPS=system,gvisor,microvm ") || !strings.Contains(sp.Display, " KARPENTER=on KARPENTER_NODE_OVERLAY=on CLUSTER_NAME=demo ") ||
			!slices.Contains(sp.Env, "KARPENTER_NODE_OVERLAY=on") {
			t.Errorf("%s: shown %q, env %q", typ, sp.Display, sp.Env)
		}
		items, active, lit := steps.FromPhases(phases.ClusterFor(typ, true)), -1, 0
		for _, line := range sp.SimLines {
			if next := steps.Progress(items, active, line); next != active {
				active, lit = next, lit+1
			}
		}
		if lit != 11 {
			t.Errorf("%s: lit %d of %d rows", typ, lit, len(items))
		}
	}
	for _, karp := range []string{"on", ""} {
		s.NewClusterConfig.NodeGroupType, s.NewClusterConfig.Karpenter, s.NewClusterConfig.KarpenterNodeOverlay = "managed", karp, ""
		if sp := b.createCluster(s); strings.Contains(sp.Display, "KARPENTER_NODE_OVERLAY") || !slices.Contains(sp.Env, "KARPENTER_NODE_OVERLAY=") {
			t.Errorf("KARPENTER=%q, no overlay: shown %q, env %q", karp, sp.Display, sp.Env)
		}
	}
	s.NewClusterConfig.Karpenter, s.NewClusterConfig.KarpenterNodeOverlay = "", "on"
	if sp := b.createCluster(s); strings.Contains(sp.Display, "KARPENTER") {
		t.Errorf("the overlay without KARPENTER: shown %q", sp.Display)
	}
}

// The repository's config.env has KARPENTER_NODE_OVERLAY off (empty), right
// after KARPENTER, with the help Settings shows: that NodeOverlay is alpha
// and can change, what it makes, the node limits, what it needs, and that
// mode 3 cannot run then. A default config.local.env does not write it.
func TestConfigEnvKarpenterNodeOverlay(t *testing.T) {
	cfg, err := state.LoadConfig(filepath.Join("..", ".."), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cfg.Defaults.Get("KARPENTER_NODE_OVERLAY"); !ok || v != "" {
		t.Errorf("KARPENTER_NODE_OVERLAY=%q (set %v)", v, ok)
	}
	var keys []string
	for _, e := range cfg.Defaults.Entries {
		keys = append(keys, e.Key)
	}
	if i := slices.Index(keys, "KARPENTER"); i < 0 || i+1 >= len(keys) || keys[i+1] != "KARPENTER_NODE_OVERLAY" {
		t.Errorf("KARPENTER_NODE_OVERLAY is not right after KARPENTER: %q", keys)
	}
	doc := strings.Join(strings.Fields(cfg.Defaults.Doc("KARPENTER_NODE_OVERLAY")), " ")
	for _, want := range []string{"an alpha Karpenter API that can change", "with KARPENTER=on", "the microvm and nitro nodes in NodePools",
		"MICROVM_NODES and NITRO_NODES are then the NodePools' limits", "NITRO_COMPUTE=node-group and NITRO_AMI_ID",
		"The mode3 plugin cannot run then", "Fixed when the cluster is created."} {
		if !strings.Contains(doc, want) {
			t.Errorf("the help lacks %q: %s", want, doc)
		}
	}
	text, err := cfg.RenderLocal(state.ConfigValues(state.NewSetupFrom(cfg.Defaults.Get)))
	if err != nil || strings.Contains(text, "KARPENTER_NODE_OVERLAY") {
		t.Errorf("a default config.local.env: %v\n%s", err, text)
	}
}

// overlayPools are the NodePools KARPENTER_NODE_OVERLAY=on makes, as
// kubectl lists them.
var overlayPools = func() string {
	var items []string
	for _, p := range []string{"gvisor", "microvm", "nitro"} {
		items = append(items, `{"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"`+p+`"}},"spec":{"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}}}`)
	}
	return `get nodepools.karpenter.sh={"items":[` + strings.Join(items, ",") + `]}`
}()

// Through main(): mode 3 is refused, deploy and verify alike, before its
// first step, on a cluster whose gvisor, microvm and nitro pools are all
// Karpenter NodePools, as KARPENTER_NODE_OVERLAY=on makes them.
func TestCLIRefusesMode3OnTheOverlayPools(t *testing.T) {
	for _, action := range []string{"deploy", "verify"} {
		err, out := runMain(t, cluster(mode3.ID, mode3.Tag)+"\n"+karpenterCRDs+"\n"+overlayPools, "--plugin-action", action, "--plugins", mode3.ID)
		if err == nil || !strings.Contains(out, noKataRefusal) || strings.Contains(out, plugin.DeployLine) || strings.Contains(out, plugin.VerifyLine) {
			t.Errorf("mode 3 %s on the overlay's pools: %v\n%s", action, err, out)
		}
	}
}

// Through main(), mode3-iso on the nitro pool of KARPENTER_NODE_OVERLAY=on
// takes the architecture from the nitro NodePool though the pool has a node
// already (so it is not one of the pools with no node), and checks no
// capacity on that node, which is still starting. It goes on to make the
// RuntimeClass and read atelet.
func TestCLIMode3IsoOnANitroNodePoolWithANode(t *testing.T) {
	node := `{"items":[{"metadata":{"name":"n1","labels":{"ate.dev/pool":"nitro","kubernetes.io/arch":"amd64"}},"status":{"allocatable":{"cpu":"3920m"},"conditions":[{"type":"Ready","status":"False"}]}}]}`
	answers := "nitro -o json=" + node + "\n" + strings.ReplaceAll(cluster(mode3.ID, mode3.Tag), "=system gvisor microvm", "=system nitro") + "\n" + karpenterCRDs + "\n" + overlayPools
	digest, err := eksoverlay.Digest("../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tree := plugin.ChosenRelease(plugin.ParseRecord(mode3.ID), "v1.0.0", "ghcr.io/agent-substrate/substrate").Locate(root, digest, "us-west-2").Tree
	if err := os.MkdirAll(filepath.Join(tree, "demos", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, out := runMainAt(t, "", root, nil, answers, "--plugin-action", "deploy", "--plugins", "mode3-iso", "--nitro-compute", plugin.NitroNodeGroup)
	if !strings.Contains(out, "The Karpenter NodePool of the nitro pool has 1 amd64 nodes now") || !strings.Contains(out, "RuntimeClass isolated-sandbox: none on the cluster; making it") {
		t.Errorf("mode3-iso on a nitro NodePool with a node:\n%s", out)
	}
}
