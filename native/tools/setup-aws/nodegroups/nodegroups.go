// Package nodegroups is the rule for which node groups `setup-aws create
// cluster` makes, and of which type, shared with the installer, which shows
// them on its Settings and Review screens and passes them as NODE_GROUPS and
// NODE_GROUP_TYPE. Like phases it has no dependencies, so the installer
// imports it through its replace directive without cobra or the AWS SDK.
//
// The gvisor, microvm and nitro groups run sandboxes: worker Pods of the
// gVisor, microVM (Kata) and AWS Nitro sandbox classes (nitro only with
// NITRO_COMPUTE=node-group; by default the Nitro sandbox runs on Auto
// Mode's nodes). The system group runs what is pinned to it (the control
// plane with RDS on Auto Mode, the plugins' controllers). A cluster without
// EKS Auto Mode has no other nodes, so it always needs the system group and
// a sandbox group. With Auto Mode, Auto Mode's nodes run everything else,
// so the system group is made only for what is pinned to it, and a cluster
// may have no node group at all (its sandboxes are Auto Mode's, such as the
// AWS Nitro sandbox class's).
package nodegroups

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The node groups, by name; each one's nodes carry ate.dev/pool=<name>.
const (
	System  = "system"
	GVisor  = "gvisor"
	MicroVM = "microvm"
	// Nitro runs the AWS Nitro sandbox class on nodes of a Nitro Sandbox
	// AMI (NITRO_AMI_ID), with Nitro Enclaves on.
	Nitro = "nitro"
)

// All are the node groups in the order they are listed and created, and
// the derived names (Auto Scaling groups, launch templates) teardown and
// inventory look for.
var All = []string{System, GVisor, MicroVM, Nitro}

// None is NODE_GROUPS for no node group (an empty value means the default).
const None = "none"

// Default is NODE_GROUPS when it is unset: system, gvisor and microvm, as
// before the rule. It is spelled out: the nitro group is never a default.
const Default = "system,gvisor,microvm"

// The node group types, NODE_GROUP_TYPE: EKS managed node groups, or
// self-managed ones, an Auto Scaling group per node group whose nodes join
// the cluster from their launch template. A cluster has one type.
const (
	Managed     = "managed"
	SelfManaged = "self-managed"
)

// Types are the node group types; Managed is the default.
var Types = []string{Managed, SelfManaged}

// CheckType refuses a NODE_GROUP_TYPE that is not one of Types.
func CheckType(t string) error {
	if !slices.Contains(Types, t) {
		return fmt.Errorf("NODE_GROUP_TYPE=%q is not a node group type (want %s)", t, strings.Join(Types, " or "))
	}
	return nil
}

// For is the node groups of a cluster whose sandboxes need the sandbox
// groups in sandbox (gvisor, microvm, nitro), in All's order: those, plus
// the system group when something runs on it:
//   - without Auto Mode, everything that is not a sandbox: there are no
//     other nodes;
//   - with Auto Mode and RDS (rds) and a sandbox group, the control plane,
//     which the install then cordons onto the system group
//     (--cordon-system-infra-components);
//   - pinned: the Pods of a chosen plugin (the LLM gateway, EFS, ...).
//
// With Auto Mode and the bundled PostgreSQL nothing else is pinned there:
// the control plane and its database run on Auto Mode's nodes, so without a
// pinned plugin no system group is made.
func For(autoMode, rds, pinned bool, sandbox []string) []string {
	var out []string
	for _, g := range All {
		if g == System {
			if !autoMode || pinned || (rds && hasSandbox(sandbox)) {
				out = append(out, g)
			}
			continue
		}
		if slices.Contains(sandbox, g) {
			out = append(out, g)
		}
	}
	return out
}

func hasSandbox(groups []string) bool {
	return slices.Contains(groups, GVisor) || slices.Contains(groups, MicroVM) || slices.Contains(groups, Nitro)
}

// Check refuses an unknown or repeated group, and without Auto Mode a list
// that lacks the system group or a sandbox group: such a cluster has no
// other nodes. With Auto Mode any of the groups, or none, makes a cluster
// that works: Auto Mode's nodes run what no group is made for, and the
// control plane is cordoned onto the system group only when it exists.
func Check(autoMode bool, groups []string) error {
	for i, g := range groups {
		if !slices.Contains(All, g) {
			return fmt.Errorf("unknown node group %q (want %s)", g, strings.Join(All, ", "))
		}
		if slices.Contains(groups[:i], g) {
			return fmt.Errorf("node group %s is named twice", g)
		}
	}
	sandbox := hasSandbox(groups)
	system := slices.Contains(groups, System)
	switch {
	case !autoMode && !sandbox:
		return errors.New("a cluster without EKS Auto Mode needs the gvisor, microvm or nitro node group: no other nodes run sandboxes")
	case !autoMode && !system:
		return errors.New("a cluster without EKS Auto Mode needs the system node group: no other nodes run the control plane")
	}
	return nil
}

// Parse reads NODE_GROUPS: comma-separated names, or None for no group.
func Parse(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == None {
		return []string{}, nil
	}
	if s == "" {
		return nil, fmt.Errorf("NODE_GROUPS is empty: name the node groups (%s), or %s for no node group", strings.Join(All, ","), None)
	}
	var out []string
	for _, g := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(g))
	}
	return out, nil
}

// Format is groups as NODE_GROUPS: comma-separated, or None.
func Format(groups []string) string {
	if len(groups) == 0 {
		return None
	}
	return strings.Join(groups, ",")
}

// The nitro group's nodes come from a Nitro Sandbox AMI, which awslabs/
// amazon-eks-ami builds with enable_pod_isolation=true: its nodeadm drop-in
// (/etc/eks/nodeadm.d/10-pod-isolation.yaml) labels each node
// NitroSandboxLabel=true, points the kubelet at the sandbox's CRI proxy and
// registers the isolated-sandbox runtime handler, and the node advertises
// NitroSandboxResource, one per sandbox it can run. The EKS optimized AMI
// has none of this, so setup-aws and the installer both check the resource.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html
const (
	NitroSandboxLabel    = "eks.amazonaws.com/nitro-sandbox"
	NitroSandboxResource = "eks.amazonaws.com/nitro-sandbox"
	NitroAMIDoc          = "https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html"
)

// NitroAMIBuild is how to build a Nitro Sandbox AMI for Kubernetes version
// (such as 1.37) from awslabs/amazon-eks-ami.
func NitroAMIBuild(version string) string {
	return "in a clone of https://github.com/awslabs/amazon-eks-ami: make k8s=" + version + " os_distro=al2023 enable_pod_isolation=true"
}

// SandboxCount reads a node's allocatable NitroSandboxResource, a
// Kubernetes quantity of a whole number of sandboxes: "4", or "1k" for a
// thousand. ok is false for anything else, which counts as none.
func SandboxCount(q string) (n int64, ok bool) {
	mult := int64(1)
	for suffix, m := range map[string]int64{"k": 1e3, "M": 1e6, "G": 1e9} {
		if v, cut := strings.CutSuffix(q, suffix); cut {
			q, mult = v, m
			break
		}
	}
	n, err := strconv.ParseInt(q, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, false
	}
	return n * mult, true
}

// KARPENTER=on (a new cluster only, without EKS Auto Mode) makes the gvisor
// group NODE_GROUPS names as a Karpenter NodePool instead of a node group:
// the pool makes a node only when a Pod needs one, and removes it when it
// is empty, up to GVISOR_NODES nodes. The system, microvm and nitro groups
// stay node groups of NODE_GROUP_TYPE. The Karpenter controller runs on the
// system group. The microvm and nitro worker Pods request extended
// resources (ate.dev/kvm, eks.amazonaws.com/nitro-sandbox) that a node
// advertises only once it runs, so Karpenter 1.15.2 finds no instance type
// for them unless a NodeOverlay, an alpha API, tells it the capacity.
// KARPENTER_NODE_OVERLAY=on (with KARPENTER=on, on a new cluster too) turns
// on Karpenter's NodeOverlay feature gate, and makes the microvm and nitro
// groups NodePools as well, each with a NodeOverlay that adds that
// resource. The API of NodeOverlay is alpha and can change.
//
//	https://karpenter.sh/docs/concepts/nodepools/
//	https://karpenter.sh/docs/concepts/nodeoverlays/
const (
	// KarpenterOn is KARPENTER's one value that turns it on; "off" and
	// empty leave it off.
	KarpenterOn = "on"
	// KarpenterVersion is the Karpenter release setup-aws installs.
	KarpenterVersion = "1.15.2"
)

// KarpenterPools are the groups KARPENTER=on makes as Karpenter NodePools:
// gvisor. With nodeOverlay (KARPENTER_NODE_OVERLAY=on: Karpenter's
// NodeOverlay tells it the microvm nodes' ate.dev/kvm and the nitro nodes'
// eks.amazonaws.com/nitro-sandbox capacity) the microvm and nitro groups
// too.
func KarpenterPools(nodeOverlay bool) []string {
	if nodeOverlay {
		return []string{GVisor, MicroVM, Nitro}
	}
	return []string{GVisor}
}

// AnyKarpenterPools are the groups either Karpenter mode can make as
// NodePools: what teardown looks for.
func AnyKarpenterPools() []string { return KarpenterPools(true) }

// IsKarpenterPool reports whether KARPENTER=on makes group g as a NodePool.
func IsKarpenterPool(g string, nodeOverlay bool) bool {
	return slices.Contains(KarpenterPools(nodeOverlay), g)
}

// SplitKarpenter splits groups, in their order, into those made as node
// groups and those KARPENTER=on makes as NodePools.
func SplitKarpenter(groups []string, nodeOverlay bool) (nodeGroups, pools []string) {
	for _, g := range groups {
		if IsKarpenterPool(g, nodeOverlay) {
			pools = append(pools, g)
		} else {
			nodeGroups = append(nodeGroups, g)
		}
	}
	return nodeGroups, pools
}

// CheckKarpenter refuses a KARPENTER value other than "on", "off" or empty,
// and KARPENTER=on with Auto Mode (Auto Mode runs its own Karpenter, and the
// two cannot share the karpenter.sh resources) or with groups that do not
// name gvisor (there would be no NodePool).
func CheckKarpenter(karpenter string, autoMode bool, groups []string) error {
	switch karpenter {
	case KarpenterOn:
	case "off", "":
		return nil
	default:
		return fmt.Errorf(`KARPENTER must be "on", "off" or empty, not %q`, karpenter)
	}
	if autoMode {
		return errors.New("KARPENTER=on needs AUTO_MODE off: EKS Auto Mode runs its own Karpenter, and the two cannot share the karpenter.sh resources")
	}
	if !slices.Contains(groups, GVisor) {
		return fmt.Errorf("KARPENTER=on makes a NodePool for the gvisor sandbox class, and NODE_GROUPS=%s does not name gvisor", Format(groups))
	}
	return nil
}

// CheckNodeOverlay refuses a KARPENTER_NODE_OVERLAY value other than "on",
// "off" or empty, and KARPENTER_NODE_OVERLAY=on without KARPENTER=on.
func CheckNodeOverlay(karpenter, nodeOverlay string) error {
	switch nodeOverlay {
	case KarpenterOn:
	case "off", "":
		return nil
	default:
		return fmt.Errorf(`KARPENTER_NODE_OVERLAY must be "on", "off" or empty, not %q`, nodeOverlay)
	}
	if karpenter != KarpenterOn {
		return fmt.Errorf("KARPENTER_NODE_OVERLAY=on needs KARPENTER=on, and KARPENTER=%q: it makes the microvm and nitro groups Karpenter NodePools too", karpenter)
	}
	return nil
}

// The values of KarpenterTagKey, fixed when the cluster is made: a cluster
// made with KARPENTER=on, and one made with KARPENTER_NODE_OVERLAY=on too.
const (
	KarpenterMarkerOn          = "on"
	KarpenterMarkerNodeOverlay = "node-overlay"
)

// KarpenterMarker is the value of KarpenterTagKey a cluster made with
// karpenter (KARPENTER=on) and nodeOverlay (KARPENTER_NODE_OVERLAY=on)
// carries, or "" for none.
func KarpenterMarker(karpenter, nodeOverlay bool) string {
	switch {
	case karpenter && nodeOverlay:
		return KarpenterMarkerNodeOverlay
	case karpenter:
		return KarpenterMarkerOn
	}
	return ""
}

// Tag keys that TAGS may not set. The marker is always reserved: create
// cluster sets it on a cluster made with KARPENTER=on, and a cluster is
// refused when it does not match. With KARPENTER=on, the owner and
// created-by keys are reserved too (Karpenter's instances carry TAGS), and
// so are the keys Karpenter sets itself, which an EC2NodeClass refuses.
const (
	// KarpenterTagKey marks a cluster made with KARPENTER=on: value "on",
	// or "node-overlay" with KARPENTER_NODE_OVERLAY=on (KarpenterMarker).
	KarpenterTagKey = "agent-substrate-on-eks/karpenter"
	ownerTagKey     = "agent-substrate-on-eks/deployment"
	createdByTagKey = "agent-substrate-on-eks/created-by"
)

// CheckTags refuses a TAGS (space-separated key=value) that sets a reserved
// key: KarpenterTagKey always; with karpenter also the owner and created-by
// keys, and the keys that start with kubernetes.io/cluster (as the
// EC2NodeClass refuses them), karpenter.sh/*, karpenter.k8s.aws/* and
// eks:eks-cluster-name. A pair that is not key=value is left to the caller.
func CheckTags(tags string, karpenter bool) error {
	for _, kv := range strings.Fields(tags) {
		k, _, _ := strings.Cut(kv, "=")
		reserved := k == KarpenterTagKey
		if karpenter {
			// The EC2NodeClass CRD refuses any tag key that starts with
			// kubernetes.io/cluster, with or without a slash
			// (!k.startsWith('kubernetes.io/cluster')), so the check here has none.
			reserved = reserved || k == ownerTagKey || k == createdByTagKey || k == "eks:eks-cluster-name" ||
				strings.HasPrefix(k, "kubernetes.io/cluster") || strings.HasPrefix(k, "karpenter.sh/") || strings.HasPrefix(k, "karpenter.k8s.aws/")
		}
		if !reserved {
			continue
		}
		if k == KarpenterTagKey || k == ownerTagKey || k == createdByTagKey {
			return fmt.Errorf("TAGS sets %s, which setup-aws sets itself; remove it from TAGS", k)
		}
		return fmt.Errorf("TAGS sets %s, a key Karpenter sets itself; KARPENTER=on puts TAGS on Karpenter's nodes, so remove it from TAGS", k)
	}
	return nil
}
