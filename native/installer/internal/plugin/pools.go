package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A cluster's pools are the PoolLabel values of its nodes and of the
// Karpenter NodePools this installer made. setup-aws's KARPENTER=on makes
// the gvisor pool as a NodePool, which starts with no node: Karpenter makes
// one when a Pod needs it. So a pool can exist with no node, and its nodes'
// CPU architecture is the one its NodePool requires. ReadInventory reads
// both, for plugin.NodePools (Options.Pools) and for the wizard's probe
// (probe.Check). Reading them needs the right to read CRDs and NodePools,
// not only nodes.

// Karpenter's CRDs: NodePools (EKS Auto Mode has this one too) and
// EC2NodeClasses, which only the Karpenter AWS provider installs.
const (
	NodePoolCRD     = "nodepools.karpenter.sh"
	EC2NodeClassCRD = "ec2nodeclasses.karpenter.k8s.aws"
)

// ManagedBy is the label this installer puts on what it makes in the
// cluster, Karpenter's NodePools among them.
const ManagedBy = "app.kubernetes.io/managed-by=substrate-eks"

// archLabel is the node label a NodePool requirement names to choose the
// CPU architecture of its nodes.
const archLabel = "kubernetes.io/arch"

// KarpenterCRDArgs are the kubectl arguments that name which of Karpenter's
// two CRDs exist.
var KarpenterCRDArgs = []string{"get", "crd", NodePoolCRD, EC2NodeClassCRD, "--ignore-not-found", "-o", "name"}

// OwnedNodePoolsArgs are the kubectl arguments that list the NodePools this
// installer made, as JSON.
var OwnedNodePoolsArgs = []string{"get", NodePoolCRD, "-l", ManagedBy, "-o", "json"}

// Inventory is what ReadInventory found.
type Inventory struct {
	// Nodes are the PoolLabel values of the nodes (PoolsArgs).
	Nodes []string
	// NodePools are, by pool, the CPU architectures its NodePools require:
	// the NodePools labeled ManagedBy whose template labels their nodes
	// PoolLabel=<pool>, with their kubernetes.io/arch In values.
	NodePools map[string][]string
	// Karpenter reports the CRD ec2nodeclasses.karpenter.k8s.aws: the
	// Karpenter AWS provider is installed (Auto Mode's Karpenter is not
	// this one).
	Karpenter bool
}

// Reader runs one kubectl read with args and returns its stdout; a failed
// read is an error, never an empty answer.
type Reader func(ctx context.Context, args ...string) (string, error)

// ReadInventory reads a cluster's pools: the nodes' PoolLabel values, then
// which of Karpenter's CRDs exist, then, when nodepools.karpenter.sh does,
// the NodePools this installer made. A cluster without that CRD has only
// its nodes' pools. Any failed read (denied, throttled, unreachable) is an
// error: a pool that cannot be read is not shown to be absent. The nodes'
// read fails with read's own error, as it did before Karpenter: the probe
// prints it as it is, and NodePools says what it read.
func ReadInventory(ctx context.Context, read Reader) (Inventory, error) {
	nodes, err := read(ctx, PoolsArgs...)
	if err != nil {
		return Inventory{}, err
	}
	inv := Inventory{Nodes: ParsePools(nodes)}
	crds, err := read(ctx, KarpenterCRDArgs...)
	if err != nil {
		return Inventory{}, fmt.Errorf("read whether the CRDs %s and %s exist: %w", NodePoolCRD, EC2NodeClassCRD, err)
	}
	nodePools := false
	for _, l := range strings.Fields(crds) {
		_, name, _ := strings.Cut(l, "/")
		switch name {
		case NodePoolCRD:
			nodePools = true
		case EC2NodeClassCRD:
			inv.Karpenter = true
		}
	}
	if !nodePools {
		return inv, nil
	}
	out, err := read(ctx, OwnedNodePoolsArgs...)
	if err != nil {
		return Inventory{}, fmt.Errorf("read the NodePools labeled %s: %w", ManagedBy, err)
	}
	if inv.NodePools, err = parseNodePools(out); err != nil {
		return Inventory{}, fmt.Errorf("read the NodePools labeled %s: %w", ManagedBy, err)
	}
	return inv, nil
}

// parseNodePools reads OwnedNodePoolsArgs' JSON into Inventory.NodePools.
// A NodePool whose nodes get no PoolLabel is not a pool.
func parseNodePools(out string) (map[string][]string, error) {
	var list struct {
		Items []struct {
			Spec struct {
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
					Spec struct {
						Requirements []struct {
							Key      string   `json:"key"`
							Operator string   `json:"operator"`
							Values   []string `json:"values"`
						} `json:"requirements"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	pools := map[string][]string{}
	for _, np := range list.Items {
		pool := np.Spec.Template.Metadata.Labels[PoolLabel]
		if pool == "" {
			continue
		}
		archs := pools[pool]
		for _, r := range np.Spec.Template.Spec.Requirements {
			if r.Key == archLabel && r.Operator == "In" {
				archs = append(archs, r.Values...)
			}
		}
		slices.Sort(archs)
		pools[pool] = slices.Compact(archs)
	}
	if len(pools) == 0 {
		return nil, nil
	}
	return pools, nil
}

// Pools are the pools of the nodes and of the NodePools, sorted, once each
// (Options.Pools).
func (inv Inventory) Pools() []string {
	out := slices.Concat(inv.Nodes, slices.Collect(maps.Keys(inv.NodePools)))
	slices.Sort(out)
	return slices.Compact(out)
}

// NodePoolNames are the pools that have a NodePool, sorted.
func (inv Inventory) NodePoolNames() []string {
	return slices.Sorted(maps.Keys(inv.NodePools))
}

// WithInventory is o with the pools of inv: Pools, PoolArchs,
// NodePoolArchs and KarpenterPools.
func (o Options) WithInventory(inv Inventory) Options {
	o.Pools, o.PoolArchs, o.NodePoolArchs, o.KarpenterPools = inv.Pools(), inv.NodePools, inv.NodePools, inv.NodePoolNames()
	return o
}

// ArchsOf are the CPU architectures archs (Options.PoolArchs) gives pools,
// sorted, once each: what a caller that reads its nodes' architecture adds
// for the pools that have a Karpenter NodePool.
func ArchsOf(archs map[string][]string, pools ...string) []string {
	var out []string
	for _, p := range pools {
		out = append(out, archs[p]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// KarpenterProblem is why plugin p cannot run on cluster, whose pools with
// a Karpenter NodePool of this installer are nodePools: p cannot run on
// Karpenter's nodes (NoKarpenter), and a node group it runs on is one of
// them. It is nil otherwise.
func KarpenterProblem(p Plugin, cluster string, nodePools []string) error {
	if p.NoKarpenter == "" {
		return nil
	}
	var hit []string
	for _, n := range p.Nodes {
		if slices.Contains(nodePools, n) {
			hit = append(hit, n)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return fmt.Errorf("plugin %s %s; the %s node group of %s is a Karpenter NodePool", p.ID, p.NoKarpenter, strings.Join(hit, " and "), cluster)
}
