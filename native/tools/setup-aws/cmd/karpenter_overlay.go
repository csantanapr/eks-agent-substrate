package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// KARPENTER_NODE_OVERLAY=on: the microvm and nitro groups are Karpenter
// NodePools too, and Karpenter's alpha NodeOverlay tells it the extended
// resource their worker Pods request.
//
// Karpenter launches a node only for a Pod whose requests one of the
// NodePool's instance types holds, and an EC2 instance type has no extended
// resources. A microvm worker container has a limit of ate.dev/kvm: 1
// (Substrate's atecontroller adds it), which atelet's device plugin
// advertises once it runs on the node: 4096 on an m8i.2xlarge. A nitro
// worker Pod runs under RuntimeClass isolated-sandbox, whose overhead is
// eks.amazonaws.com/nitro-sandbox: 1, which the Nitro Sandbox runtime of the
// node's AMI advertises: 20 slots, measured on an m8i.xlarge. Karpenter
// counts a Pod's overhead with its requests. A NodeOverlay adds that
// capacity to the instance types Karpenter simulates; it changes nothing on
// a node, where kube-scheduler uses what the node advertises.
//
// Each NodeOverlay applies to its own NodePool only: its one requirement is
// karpenter.sh/nodepool In [<pool>]. Karpenter v1.15.1's NodeOverlay
// controller matches an overlay's requirements against each NodePool's
// instance types with karpenter.sh/nodepool=<that NodePool's name> and the
// NodePool's template labels added (getOverlaidOfferings in
// pkg/controllers/nodeoverlay/controller.go), so another pool of the same
// instance type does not get the capacity. Karpenter applies overlays only
// with the NodeOverlay feature gate on (pkg/controllers/controllers.go),
// which this mode sets in the controller's FEATURE_GATES. An overlay that
// conflicts with another of the same weight is not applied, and is not
// Ready: the phase waits until each one is.
//
//	https://karpenter.sh/docs/concepts/nodeoverlays/

// kindNodeOverlay is Karpenter's NodeOverlay, an alpha API.
var kindNodeOverlay = kubeKind{"karpenter.sh/v1alpha1", "NodeOverlay", "nodeoverlays", false}

// nodeOverlay is the NodeOverlay of pool: capacity of an extended resource
// on each of its nodes.
type nodeOverlay struct{ pool, resource, capacity string }

// nodeOverlays are the NodeOverlays of the pools that need one, by pool.
var nodeOverlays = []nodeOverlay{
	{nodegroups.MicroVM, "ate.dev/kvm", "4096"},
	{nodegroups.Nitro, nodegroups.NitroSandboxResource, "20"},
}

// overlayOf is pool's NodeOverlay, and whether it has one.
func overlayOf(pool string) (nodeOverlay, bool) {
	for _, o := range nodeOverlays {
		if o.pool == pool {
			return o, true
		}
	}
	return nodeOverlay{}, false
}

// overlays are the NodeOverlays of this run's NodePools: none without
// KARPENTER_NODE_OVERLAY=on, since only it makes the microvm and nitro
// groups NodePools (r.pools).
func (r *clusterRun) overlays() []nodeOverlay {
	var out []nodeOverlay
	for _, p := range r.pools() {
		if o, ok := overlayOf(p.Name); ok {
			out = append(out, o)
		}
	}
	return out
}

// object is o as the Kubernetes object, named after its pool and labeled
// as setup-aws's.
func (o nodeOverlay) object() kubeObject {
	return kubeObject{"apiVersion": kindNodeOverlay.APIVersion, "kind": kindNodeOverlay.Kind,
		"metadata": map[string]any{"name": o.pool, "labels": map[string]any{managedByKey: managedByValue}},
		"spec": map[string]any{
			"requirements": []any{map[string]any{"key": karpenterPoolTag, "operator": "In", "values": []any{o.pool}}},
			"capacity":     map[string]any{o.resource: o.capacity},
		}}
}

// featureGate is the controller's environment variable of Karpenter's
// feature gates, and the NodeOverlay gate's value in the rendered chart.
const (
	featureGate        = "FEATURE_GATES"
	nodeOverlayGateOff = "NodeOverlay=false"
	nodeOverlayGateOn  = "NodeOverlay=true"
)

// setNodeOverlayGate turns on the NodeOverlay feature gate in the
// controller's FEATURE_GATES, which the chart renders with the gate off.
func setNodeOverlayGate(dep kubeObject) error {
	containers, _ := dep.field("spec", "template", "spec", "containers").([]any)
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		if cm["name"] != "controller" {
			continue
		}
		envs, _ := cm["env"].([]any)
		for _, e := range envs {
			em, _ := e.(map[string]any)
			if em["name"] != featureGate {
				continue
			}
			v, _ := em["value"].(string)
			gates := strings.Split(v, ",")
			n := 0
			for i, g := range gates {
				if g == nodeOverlayGateOff {
					gates[i] = nodeOverlayGateOn
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("internal: karpenter-chart.yaml: the controller's %s has %d of %s, not one", featureGate, n, nodeOverlayGateOff)
			}
			em["value"] = strings.Join(gates, ",")
			return nil
		}
	}
	return errors.New("internal: karpenter-chart.yaml: the controller has no env " + featureGate)
}
