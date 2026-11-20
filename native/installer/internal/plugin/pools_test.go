package plugin

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// nodePoolsJSON is OwnedNodePoolsArgs' answer: setup-aws's gvisor (amd64)
// and microvm (arm64) NodePools, and one of ours that labels no pool.
const nodePoolsJSON = `{"items":[
 {"metadata":{"name":"gvisor"},"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"gvisor"}},
  "spec":{"requirements":[{"key":"node.kubernetes.io/instance-type","operator":"In","values":["m8i.2xlarge"]},
   {"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}}},
 {"metadata":{"name":"microvm"},"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"microvm"}},
  "spec":{"requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["arm64"]},
   {"key":"kubernetes.io/arch","operator":"NotIn","values":["amd64"]}]}}}},
 {"metadata":{"name":"substrate-system"},"spec":{"template":{"metadata":{"labels":{"team":"a"}},"spec":{}}}}]}`

const (
	crdNodePools      = "customresourcedefinition.apiextensions.k8s.io/" + NodePoolCRD
	crdEC2NodeClasses = "customresourcedefinition.apiextensions.k8s.io/" + EC2NodeClassCRD
)

// fakeReader answers ReadInventory's three reads by their argv and logs
// each; a read in fail fails.
type fakeReader struct {
	nodes, crds, nodePools string
	fail                   string // "nodes", "crds" or "nodepools"
	log                    []string
}

func (f *fakeReader) read(_ context.Context, args ...string) (string, error) {
	argv := strings.Join(args, " ")
	f.log = append(f.log, argv)
	var which, out string
	switch argv {
	case strings.Join(PoolsArgs, " "):
		which, out = "nodes", f.nodes
	case strings.Join(KarpenterCRDArgs, " "):
		which, out = "crds", f.crds
	case strings.Join(OwnedNodePoolsArgs, " "):
		which, out = "nodepools", f.nodePools
	default:
		return "", errors.New("unexpected read: " + argv)
	}
	if f.fail == which {
		return "", errors.New("Forbidden")
	}
	return out, nil
}

// The inventory, both ways. A cluster without nodepools.karpenter.sh has
// its nodes' pools only, and its NodePools are not read. With the CRD (EKS
// Auto Mode has it too) the NodePools labeled managed-by=substrate-eks
// add the pools their nodes get, with their kubernetes.io/arch In values;
// one that labels no pool adds none. Karpenter is the EC2NodeClass CRD
// alone. A pool with a node is not nodeless.
func TestReadInventory(t *testing.T) {
	f := &fakeReader{nodes: "system gvisor system"}
	inv, err := ReadInventory(context.Background(), f.read)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv, Inventory{Nodes: []string{"gvisor", "system"}}) || len(f.log) != 2 {
		t.Errorf("node-only: %+v, reads %q", inv, f.log)
	}

	f = &fakeReader{nodes: "system", crds: crdNodePools + "\n", nodePools: `{"items":[{"spec":{"template":{"metadata":{"labels":{}}}}}]}`}
	if inv, err = ReadInventory(context.Background(), f.read); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv, Inventory{Nodes: []string{"system"}}) || len(f.log) != 3 {
		t.Errorf("Auto Mode's CRD, no pool: %+v, reads %q", inv, f.log)
	}

	f = &fakeReader{nodes: "system microvm", crds: crdNodePools + "\n" + crdEC2NodeClasses + "\n", nodePools: nodePoolsJSON}
	if inv, err = ReadInventory(context.Background(), f.read); err != nil {
		t.Fatal(err)
	}
	want := Inventory{Nodes: []string{"microvm", "system"}, NodePools: map[string][]string{"gvisor": {"amd64"}, "microvm": {"arm64"}}, Karpenter: true}
	if !reflect.DeepEqual(inv, want) {
		t.Errorf("Karpenter: %+v, want %+v", inv, want)
	}
	if got := inv.Pools(); !slices.Equal(got, []string{"gvisor", "microvm", "system"}) {
		t.Errorf("Pools %q", got)
	}
	if got := inv.NodePoolNames(); !slices.Equal(got, []string{"gvisor", "microvm"}) {
		t.Errorf("NodePoolNames %q", got)
	}

	// The EC2NodeClass CRD without the NodePool one: Karpenter, no NodePool
	// read.
	f = &fakeReader{crds: crdEC2NodeClasses}
	if inv, err = ReadInventory(context.Background(), f.read); err != nil || !inv.Karpenter || inv.NodePools != nil || len(f.log) != 2 {
		t.Errorf("EC2NodeClass CRD only: %+v %v, reads %q", inv, err, f.log)
	}
}

// Every failed read stops the inventory, naming what it read; a NodePool
// list that is not JSON does too. None reads as "no pool". The nodes' read
// gives its error as it is, as the probe printed it before Karpenter
// (NodePools names the read).
func TestReadInventoryFailedReadsStop(t *testing.T) {
	for which, want := range map[string]string{
		"nodes":     "Forbidden",
		"crds":      "read whether the CRDs nodepools.karpenter.sh and ec2nodeclasses.karpenter.k8s.aws exist: Forbidden",
		"nodepools": "read the NodePools labeled app.kubernetes.io/managed-by=substrate-eks: Forbidden",
	} {
		f := &fakeReader{nodes: "system", crds: crdNodePools, nodePools: nodePoolsJSON, fail: which}
		if inv, err := ReadInventory(context.Background(), f.read); err == nil || err.Error() != want {
			t.Errorf("%s: %+v, %v; want %q", which, inv, err, want)
		}
	}
	f := &fakeReader{crds: crdNodePools, nodePools: "not json"}
	if _, err := ReadInventory(context.Background(), f.read); err == nil || !strings.Contains(err.Error(), "read the NodePools labeled") {
		t.Errorf("bad JSON: %v", err)
	}
}

// The reads are the ones setup-aws's objects answer: the two CRDs by name,
// missing ones ignored, and only the NodePools this installer labeled.
func TestInventoryArgs(t *testing.T) {
	if got := strings.Join(KarpenterCRDArgs, " "); got != "get crd nodepools.karpenter.sh ec2nodeclasses.karpenter.k8s.aws --ignore-not-found -o name" {
		t.Errorf("CRD read %q", got)
	}
	if got := strings.Join(OwnedNodePoolsArgs, " "); got != "get nodepools.karpenter.sh -l app.kubernetes.io/managed-by=substrate-eks -o json" {
		t.Errorf("NodePool read %q", got)
	}
}

// NodePools reads the inventory through Kubectl, with the installer's
// kubeconfig.
func TestNodePoolsReadsThroughKubectl(t *testing.T) {
	prev := Exec
	t.Cleanup(func() { Exec = prev })
	var argvs []string
	Exec = func(_ context.Context, _ io.Writer, c Cmd) error {
		argv := strings.Join(c.Argv, " ")
		argvs = append(argvs, argv)
		switch {
		case strings.HasSuffix(argv, strings.Join(PoolsArgs, " ")):
			io.WriteString(c.Stdout, "system")
		case strings.HasSuffix(argv, strings.Join(KarpenterCRDArgs, " ")):
			io.WriteString(c.Stdout, crdNodePools+" "+crdEC2NodeClasses)
		case strings.HasSuffix(argv, strings.Join(OwnedNodePoolsArgs, " ")):
			io.WriteString(c.Stdout, nodePoolsJSON)
		}
		return nil
	}
	inv, err := NodePools(context.Background(), io.Discard, Options{Kubeconfig: "/kc"})
	if err != nil || !inv.Karpenter || !slices.Equal(inv.Pools(), []string{"gvisor", "microvm", "system"}) {
		t.Fatalf("%+v %v", inv, err)
	}
	for _, a := range argvs {
		if !strings.HasPrefix(a, "kubectl --kubeconfig /kc ") {
			t.Errorf("read %q", a)
		}
	}
	if len(argvs) != 3 {
		t.Errorf("reads %q", argvs)
	}
}

// A failed read through NodePools names it as before Karpenter: the nodes'
// labels, or the CRDs and NodePools ReadInventory names.
func TestNodePoolsFailedReads(t *testing.T) {
	prev := Exec
	t.Cleanup(func() { Exec = prev })
	for fail, want := range map[string]string{
		strings.Join(PoolsArgs, " "):          "read the nodes' ate.dev/pool labels: exit status 1",
		strings.Join(KarpenterCRDArgs, " "):   "read whether the CRDs nodepools.karpenter.sh and ec2nodeclasses.karpenter.k8s.aws exist: exit status 1",
		strings.Join(OwnedNodePoolsArgs, " "): "read the NodePools labeled app.kubernetes.io/managed-by=substrate-eks: exit status 1",
	} {
		Exec = func(_ context.Context, _ io.Writer, c Cmd) error {
			argv := strings.Join(c.Argv, " ")
			if strings.HasSuffix(argv, fail) {
				return errors.New("exit status 1")
			}
			if strings.HasSuffix(argv, strings.Join(KarpenterCRDArgs, " ")) {
				io.WriteString(c.Stdout, crdNodePools)
			}
			return nil
		}
		if _, err := NodePools(context.Background(), io.Discard, Options{Kubeconfig: "/kc"}); err == nil || err.Error() != want {
			t.Errorf("%s fails: %v, want %q", fail, err, want)
		}
	}
}

// WithInventory hands every pool field to the steps. PoolArchs and
// NodePoolArchs have each NodePool's architectures, a pool with a node too:
// Karpenter can remove that node before a step reads the nodes.
func TestWithInventory(t *testing.T) {
	inv := Inventory{Nodes: []string{"nitro", "system"}, NodePools: map[string][]string{"gvisor": {"amd64"}, "nitro": {"arm64"}}, Karpenter: true}
	o := Options{Cluster: "demo"}.WithInventory(inv)
	if o.Cluster != "demo" || !slices.Equal(o.Pools, []string{"gvisor", "nitro", "system"}) || !slices.Equal(o.KarpenterPools, []string{"gvisor", "nitro"}) ||
		!reflect.DeepEqual(o.PoolArchs, inv.NodePools) || !reflect.DeepEqual(o.NodePoolArchs, inv.NodePools) {
		t.Errorf("%+v", o)
	}
	if o := (Options{}).WithInventory(Inventory{Nodes: []string{"system"}}); !slices.Equal(o.Pools, []string{"system"}) || o.PoolArchs != nil || o.NodePoolArchs != nil || o.KarpenterPools != nil {
		t.Errorf("no NodePool: %+v", o)
	}
}

func TestArchsOf(t *testing.T) {
	m := map[string][]string{"gvisor": {"amd64"}, "microvm": {"arm64", "amd64"}}
	if got := ArchsOf(m, "system", "gvisor", "microvm"); !slices.Equal(got, []string{"amd64", "arm64"}) {
		t.Errorf("ArchsOf = %q", got)
	}
	if got := ArchsOf(nil, "gvisor"); len(got) != 0 {
		t.Errorf("ArchsOf(nil) = %q", got)
	}
}

// Both ways: a plugin that cannot run on Karpenter's nodes is refused,
// deploy and verify alike, where its node group is a Karpenter NodePool,
// before anything runs, with its reason; and so is one that requires it.
// Where only another pool is a NodePool (KARPENTER=on's gvisor), or there
// is none, it deploys and checks, and a removal is never refused for it.
func TestRunRefusesNoKarpenterOnANodePool(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "kata", Order: 4, Nodes: []string{"microvm"}, NoKarpenter: "needs a label kata-deploy sets on a running node",
		Deploy: hook(f, "deploy kata", nil), Verify: hook(f, "verify kata", nil), Remove: hook(f, "remove kata", nil)})
	Register(Plugin{ID: "on-kata", Order: 5, Requires: []string{"kata"}})
	o.Pools, o.KarpenterPools = []string{"gvisor", "microvm", "system"}, []string{"gvisor", "microvm"}
	for _, action := range []string{ActionDeploy, ActionVerify} {
		for _, id := range []string{"kata", "on-kata"} {
			err := Run(context.Background(), io.Discard, action, []string{id}, o)
			if err == nil || err.Error() != "plugin kata needs a label kata-deploy sets on a running node; the microvm node group of demo is a Karpenter NodePool" {
				t.Errorf("%s %s: %v", action, id, err)
			}
		}
	}
	if len(f.log) != 0 {
		t.Errorf("ran after the refusal: %q", f.log)
	}
	f.record = "kata"
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"kata"}, o); err != nil || !slices.Contains(f.log, "remove kata") {
		t.Errorf("remove on a NodePool: %v, %q", err, f.log)
	}
	for _, pools := range [][]string{{"gvisor"}, nil} {
		f.log = nil
		o.KarpenterPools = pools
		if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"kata"}, o); err != nil || !slices.Contains(f.log, "deploy kata") {
			t.Errorf("deploy with NodePools %v: %v, %q", pools, err, f.log)
		}
	}
	if KarpenterProblem(Plugin{ID: "x", Nodes: []string{"microvm"}}, "demo", []string{"microvm"}) != nil {
		t.Error("a plugin without NoKarpenter is refused")
	}
}

// A pool that exists only as a Karpenter NodePool (no node yet) is a pool
// Run deploys and checks a plugin on: its nodes come when the Pods need
// them.
func TestRunTakesAPoolWithANodePoolOnly(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "pinned", Order: 4, Nodes: []string{"system", "gvisor"}, Deploy: hook(f, "deploy pinned", nil), Verify: hook(f, "verify pinned", nil)})
	inv := Inventory{Nodes: []string{"system"}, NodePools: map[string][]string{"gvisor": {"amd64"}}, Karpenter: true}
	o.Pools = inv.Pools()
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"pinned"}, o); err != nil || !slices.Contains(f.log, "verify pinned") {
		t.Errorf("deploy on a NodePool-only pool: %v, %q", err, f.log)
	}
	o.Pools = inv.Nodes
	if err := Run(context.Background(), io.Discard, ActionVerify, []string{"pinned"}, o); err == nil || !strings.Contains(err.Error(), "runs on the gvisor node group") {
		t.Errorf("without the NodePool: %v", err)
	}
}
