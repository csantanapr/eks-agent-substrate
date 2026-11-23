package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// The kubectl answers of Karpenter on the cluster: both CRDs, or EKS Auto
// Mode's NodePool CRD alone, and the NodePools setup-aws made.
const (
	karpenterCRDs = "get crd nodepools.karpenter.sh=customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh " +
		"customresourcedefinition.apiextensions.k8s.io/ec2nodeclasses.karpenter.k8s.aws"
	autoModeCRD   = "get crd nodepools.karpenter.sh=customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh"
	gvisorPool    = `get nodepools.karpenter.sh={"items":[{"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"gvisor"}},"spec":{"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}}}]}`
	microvmPool   = `get nodepools.karpenter.sh={"items":[{"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"microvm"}},"spec":{"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}}}]}`
	noNodePools   = `get nodepools.karpenter.sh={"items":[]}`
	noKataRefusal = "plugin mode3 " + mode3.NoKarpenter + "; the microvm node group of demo is a Karpenter NodePool"
)

// Both ways, through main(): the counter demo on gvisor, whose pool is only
// a Karpenter NodePool (no node yet), goes on to its tree; with no NodePool
// for it, it is refused as on a cluster without Karpenter. A failed read of
// the CRDs or of the NodePools stops it before the release is read.
func TestCLIDemoOnANodePoolOnlyPool(t *testing.T) {
	nodes := "substrate-eks-plugins=\nget nodes -l ate.dev/pool=system microvm\n"
	err, out := runMain(t, nodes+karpenterCRDs+"\n"+gvisorPool, "--deploy-demo", "--sandbox-classes", "gvisor")
	if err == nil || strings.Contains(out, "has no node labeled") || !strings.Contains(out, "no substrate v1.0.0 tree with demos/counter") {
		t.Errorf("gvisor as a NodePool: %v\n%s", err, out)
	}
	err, out = runMain(t, nodes+karpenterCRDs+"\n"+noNodePools, "--deploy-demo", "--sandbox-classes", "gvisor")
	if err == nil || !strings.Contains(out, "the counter demo on gvisor runs on the gvisor node group, and demo has no node labeled ate.dev/pool=gvisor") {
		t.Errorf("no gvisor NodePool: %v\n%s", err, out)
	}
	for read, answer := range map[string]string{"the CRDs": "get crd nodepools.karpenter.sh=" + kubectlFails, "the NodePools": karpenterCRDs + "\nget nodepools.karpenter.sh=" + kubectlFails} {
		err, out := runMain(t, nodes+answer, "--deploy-demo", "--sandbox-classes", "gvisor")
		if err == nil || !strings.Contains(out, "read whether the CRDs nodepools.karpenter.sh and ec2nodeclasses.karpenter.k8s.aws exist") && !strings.Contains(out, "read the NodePools labeled app.kubernetes.io/managed-by=substrate-eks") ||
			strings.Contains(out, ReleaseLine) {
			t.Errorf("%s unread: %v\n%s", read, err, out)
		}
	}
}

// Both ways, through main(): mode 3 is refused, deploy and verify alike,
// with the kata-runtime label reason and before its first step, where its
// microvm node group is a Karpenter NodePool. KARPENTER=on makes only the
// gvisor pool a NodePool, and there it reaches the plugin, as it does with
// EKS Auto Mode's NodePool CRD alone or no CRD.
func TestCLIRefusesMode3OnAMicroVMNodePool(t *testing.T) {
	base := cluster(mode3.ID, mode3.Tag)
	for _, action := range []string{"deploy", "verify"} {
		err, out := runMain(t, base+"\n"+karpenterCRDs+"\n"+microvmPool, "--plugin-action", action, "--plugins", mode3.ID)
		if err == nil || !strings.Contains(out, noKataRefusal) || strings.Contains(out, plugin.DeployLine) || strings.Contains(out, plugin.VerifyLine) {
			t.Errorf("%s on a microvm NodePool: %v\n%s", action, err, out)
		}
	}
	for name, answers := range map[string]string{"KARPENTER=on's gvisor NodePool": base + "\n" + karpenterCRDs + "\n" + gvisorPool,
		"Auto Mode's CRD": base + "\n" + autoModeCRD + "\n" + noNodePools, "no CRD": base} {
		_, out := runMain(t, answers, "--plugin-action", "deploy", "--plugins", mode3.ID)
		if strings.Contains(out, "Karpenter NodePool") || !strings.Contains(out, plugin.DeployLine+mode3.ID) {
			t.Errorf("%s: did not reach the plugin:\n%s", name, out)
		}
	}
}

// Through main(), a pool that has a Karpenter NodePool and no node yet
// passes the demo's node check: with no gvisor node, the demo goes on to
// the tree's .ko.yaml (absent here, so nothing is built); with no NodePool
// it is refused for want of a node. The architecture value itself (arm64
// here) is checked by TestWithInventory and TestDemoOptionsCarryTheNodelessPools.
func TestCLIDemoGetsTheNodePoolArchitecture(t *testing.T) {
	digest, err := eksoverlay.Digest("../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	rel := plugin.ChosenRelease(nil, "v1.0.0", "ghcr.io/agent-substrate/substrate")
	tree := rel.Locate(root, digest, "us-west-2").Tree
	if err := os.MkdirAll(filepath.Join(tree, "demos", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	arm := strings.ReplaceAll(gvisorPool, "amd64", "arm64")
	// The demo's read of the gvisor nodes' architecture finds none.
	nodes := "io/arch}=\nsubstrate-eks-plugins=\nget nodes -l ate.dev/pool=system microvm\n"
	err, out := runMainAt(t, "", root, nil, nodes+karpenterCRDs+"\n"+arm, "--deploy-demo", "--sandbox-classes", "gvisor")
	if err == nil || !strings.Contains(out, filepath.Join(tree, ".ko.yaml")) || strings.Contains(out, "to run the counter demo on") {
		t.Errorf("a nodeless arm64 NodePool: %v\n%s", err, out)
	}
	err, out = runMainAt(t, "", root, nil, nodes+karpenterCRDs+"\n"+noNodePools, "--deploy-demo", "--sandbox-classes", "gvisor")
	if err == nil || !strings.Contains(out, "has no node labeled ate.dev/pool=gvisor") {
		t.Errorf("no NodePool: %v\n%s", err, out)
	}
}

// The demo runs on the cluster's release, and builds its counter for the
// architecture of a pool with a NodePool and no node yet.
func TestDemoOptionsCarryTheNodelessPools(t *testing.T) {
	o := plugin.Options{Release: plugin.Release{Tag: "v9", ImageRepo: "r", Tree: "/t"}, PoolArchs: map[string][]string{"gvisor": {"arm64"}}}
	do := demoOptions(o, demo.Options{Cluster: "demo", SandboxClasses: "gvisor"})
	if do.Tag != "v9" || do.ImageRepo != "r" || do.Tree != "/t" || do.Cluster != "demo" || !reflect.DeepEqual(do.PoolArchs, o.PoolArchs) {
		t.Errorf("%+v", do)
	}
}

// Of the plugins, only mode 3 cannot run on Karpenter's nodes, and it runs
// on the microvm group, which KARPENTER=on keeps a node group: the nitro
// class (mode3-iso) stays on its node group too, and the others' Pods need
// only a pool's label, which a NodePool gives its nodes.
func TestOnlyMode3IsRefusedWithKarpenter(t *testing.T) {
	for _, p := range plugin.All() {
		if (p.NoKarpenter != "") != (p.ID == mode3.ID) {
			t.Errorf("%s: NoKarpenter %q", p.ID, p.NoKarpenter)
		}
		if p.NoKarpenter != "" && !slices.Equal(p.Nodes, []string{"microvm"}) {
			t.Errorf("%s runs on %v", p.ID, p.Nodes)
		}
	}
}

// The repository's config.env has KARPENTER off (empty), right after
// AUTO_MODE, with the help Settings shows: what it makes, the node limit,
// what stays a node group, what it needs, and every TAGS key it refuses. A
// default config.local.env does not write it.
func TestConfigEnvKarpenter(t *testing.T) {
	cfg, err := state.LoadConfig(filepath.Join("..", ".."), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cfg.Defaults.Get("KARPENTER"); !ok || v != "" {
		t.Errorf("KARPENTER=%q (set %v)", v, ok)
	}
	var keys []string
	for _, e := range cfg.Defaults.Entries {
		keys = append(keys, e.Key)
	}
	if i := slices.Index(keys, "AUTO_MODE"); i < 0 || i+1 >= len(keys) || keys[i+1] != "KARPENTER" {
		t.Errorf("KARPENTER is not right after AUTO_MODE: %q", keys)
	}
	doc := strings.Join(strings.Fields(cfg.Defaults.Doc("KARPENTER")), " ")
	for _, want := range []string{"Karpenter 1.15.2", "GVISOR_NODES is then the NodePool's limit", "AUTO_MODE empty",
		"The microvm and nitro classes stay on their node groups", "The mode3 plugin works as before.",
		"agent-substrate-on-eks/deployment, agent-substrate-on-eks/created-by,", "starts with kubernetes.io/cluster,",
		"With it or without it, TAGS may not set agent-substrate-on-eks/karpenter", "Fixed when the cluster is created."} {
		if !strings.Contains(doc, want) {
			t.Errorf("the help lacks %q: %s", want, doc)
		}
	}
	text, err := cfg.RenderLocal(state.ConfigValues(state.NewSetupFrom(cfg.Defaults.Get)))
	if err != nil || strings.Contains(text, "KARPENTER") {
		t.Errorf("a default config.local.env: %v\n%s", err, text)
	}
}
