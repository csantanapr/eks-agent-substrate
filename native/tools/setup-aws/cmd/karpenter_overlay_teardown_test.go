package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// nodeOverlayDeployment is karpenterDeployment made with
// KARPENTER_NODE_OVERLAY=on: the cluster marked node-overlay, the system
// node group alone, and in the cluster the gvisor, microvm and nitro
// NodePools and EC2NodeClasses of setup-aws, the microvm and nitro
// NodeOverlays of setup-aws, and a NodeOverlay of someone else. A nitro
// instance and NodeClaim go with the nitro NodePool.
func nodeOverlayDeployment(t *testing.T) *karpenterTD {
	kd := karpenterDeployment(t)
	f, k := kd.f, kd.kube
	f.objs["cluster|demo"].tags = own(nodegroups.KarpenterTagKey, nodegroups.KarpenterMarkerNodeOverlay)
	for _, key := range []string{"nodegroup|microvm", kLT + "|lt-1"} {
		delete(f.objs, key)
	}
	owned := map[string]any{managedByKey: managedByValue}
	for _, pool := range []string{"microvm", "nitro"} {
		k.put(kubeObject{"apiVersion": kindNodePool.APIVersion, "kind": kindNodePool.Kind, "metadata": map[string]any{"name": pool, "labels": owned}})
		k.put(kubeObject{"apiVersion": kindEC2NodeClass.APIVersion, "kind": kindEC2NodeClass.Kind, "metadata": map[string]any{"name": pool, "labels": owned}})
		k.put(kubeObject{"apiVersion": kindNodeOverlay.APIVersion, "kind": kindNodeOverlay.Kind, "metadata": map[string]any{"name": pool, "labels": owned}})
	}
	k.put(kubeObject{"apiVersion": kindNodeOverlay.APIVersion, "kind": kindNodeOverlay.Kind, "metadata": map[string]any{"name": "theirs"}})
	k.put(kubeObject{"apiVersion": kindNodeClaim.APIVersion, "kind": kindNodeClaim.Kind,
		"metadata": map[string]any{"name": "nitro-abcde", "labels": map[string]any{karpenterPoolTag: "nitro"}},
		"spec":     map[string]any{"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": "nitro"}}})
	f.put(&tdObj{kind: kInstance, id: "i-nitro", vpc: "vpc-1", state: "running",
		tags: own(karpenterPoolTag, "nitro", karpenterClassTag, "nitro", "kubernetes.io/cluster/demo", "owned")})
	return kd
}

// A teardown of a KARPENTER_NODE_OVERLAY=on deployment deletes the three
// NodePools, waits until Karpenter removed their nodes, then the two
// NodeOverlays of setup-aws, then the three EC2NodeClasses; someone else's
// NodeOverlay is left to the cluster, and nothing else is left.
func TestTeardownNodeOverlay(t *testing.T) {
	kd := nodeOverlayDeployment(t)
	e, _, logs := tdTestEnv(t, kd.f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	var want []string
	for _, k := range []kubeKind{kindNodePool, kindNodeOverlay, kindEC2NodeClass} {
		for _, pool := range []string{"gvisor", "microvm", "nitro"} {
			if k == kindNodeOverlay && pool == "gvisor" {
				continue
			}
			want = append(want, "DELETE "+k.path("", pool))
		}
	}
	if w := kd.kube.writes(); !slices.Equal(w, want) {
		t.Errorf("Kubernetes writes:\n  %s\nwant\n  %s", strings.Join(w, "\n  "), strings.Join(want, "\n  "))
	}
	if kd.kube.get(kindNodeOverlay, "", "theirs") == nil {
		t.Error("deleted someone else's NodeOverlay")
	}
	for _, x := range kd.f.calls {
		if strings.HasPrefix(x, "TerminateInstances") {
			t.Errorf("terminated instances Karpenter removed: %s", x)
		}
	}
	if !strings.Contains(logs.String(), "Karpenter removed the nodes of NodePools gvisor, microvm, nitro") {
		t.Errorf("no drain line:\n%s", logs)
	}
	phaseOrder(t, logs.String())
}

// --keep-cluster keeps the NodeOverlays with the rest, and the final check
// passes; --plan names them and deletes nothing.
func TestTeardownNodeOverlayKeepClusterAndPlan(t *testing.T) {
	kd := nodeOverlayDeployment(t)
	e, _, logs := tdTestEnv(t, kd.f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	// Inventory lists them, before and after, as the cluster's.
	if w := kd.kube.writes(); len(w) != 0 {
		t.Errorf("--keep-cluster changed the cluster: %v", w)
	}
	if strings.Contains(logs.String(), "delete Karpenter NodeOverlay") {
		t.Errorf("--keep-cluster deletes a NodeOverlay:\n%s", logs)
	}
	kd = nodeOverlayDeployment(t)
	e, _, logs = tdTestEnv(t, kd.f, true, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if len(kd.f.calls) != 0 || len(kd.kube.writes()) != 0 {
		t.Errorf("--plan wrote %v, Kubernetes %v", kd.f.calls, kd.kube.writes())
	}
	for _, want := range []string{"plan: would delete Karpenter NodeOverlay microvm", "plan: would delete Karpenter NodeOverlay nitro",
		"plan: would delete Karpenter NodePool nitro", "plan: would delete Karpenter EC2NodeClass nitro"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no line %q", want)
		}
	}
	if strings.Contains(logs.String(), "NodeOverlay theirs") {
		t.Error("--plan names someone else's NodeOverlay")
	}
}

// Inventory lists the NodeOverlays of setup-aws as the cluster's, with the
// three pools' NodePools and EC2NodeClasses; not someone else's. An API that
// fails lists none, with a note that names them.
func TestInventoryNodeOverlay(t *testing.T) {
	kd := nodeOverlayDeployment(t)
	e, _, _ := tdTestEnv(t, kd.f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Kind+" "+r.ID] = r.Cluster
	}
	for _, w := range []string{"Karpenter NodeOverlay microvm", "Karpenter NodeOverlay nitro", "Karpenter NodePool nitro",
		"Karpenter EC2NodeClass nitro", "Karpenter NodePool microvm", "Karpenter EC2NodeClass gvisor", "EC2 instance i-nitro"} {
		if cluster, ok := have[w]; !ok || !cluster {
			t.Errorf("row %q: listed %v, cluster %v", w, ok, cluster)
		}
	}
	if _, ok := have["Karpenter NodeOverlay theirs"]; ok {
		t.Error("a row of someone else's NodeOverlay")
	}
	kd.f.kube = unreachableKube(t)
	e, _, logs := tdTestEnv(t, kd.f, false, false)
	if rows, err = e.inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Kind, "Karpenter ") {
			t.Errorf("row %s %s from an API that does not answer", r.Kind, r.ID)
		}
	}
	if !strings.Contains(logs.String(), "inventory does not list its NodePools, EC2NodeClasses and NodeOverlays") {
		t.Errorf("no note:\n%s", logs)
	}
}

// When the Kubernetes API does not answer, teardown terminates the nitro
// instance with the others and touches no NodeOverlay; when it refuses the
// NodeOverlay deletes, they go with the cluster and teardown goes on.
func TestTeardownNodeOverlayKubernetesFails(t *testing.T) {
	kd := nodeOverlayDeployment(t)
	kd.f.kube = unreachableKube(t)
	e, _, logs := tdTestEnv(t, kd.f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	cutOffFirst(t, kd.f.calls, "TerminateInstances i-gvisor,i-nitro")
	if len(kd.kube.calls) != 0 {
		t.Errorf("reached the other API: %v", kd.kube.calls)
	}

	kd = nodeOverlayDeployment(t)
	kd.kube.deny["DELETE "+kindNodeOverlay.path("", "microvm")] = 403
	e, _, logs = tdTestEnv(t, kd.f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "NodeOverlay microvm goes with the cluster") || kd.f.get("cluster", "demo") != nil {
		t.Errorf("cluster left, or no note:\n%s", logs)
	}
	if kd.kube.get(kindNodeOverlay, "", "microvm") == nil {
		t.Error("the denied NodeOverlay is gone")
	}
}

// A teardown of a KARPENTER=on deployment without the overlay never reads
// NodeOverlays, though the cluster serves the kind (the chart has its CRD):
// only the marker of KARPENTER_NODE_OVERLAY=on says there are any.
func TestTeardownKarpenterOnReadsNoNodeOverlay(t *testing.T) {
	kd := karpenterDeployment(t)
	e, _, logs := tdTestEnv(t, kd.f, false, false)
	if _, err := e.inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	for _, c := range kd.kube.calls {
		if strings.Contains(c, kindNodeOverlay.Resource) {
			t.Errorf("read the NodeOverlays: %s", c)
		}
	}
}
