package state

// Sandbox classes and the node groups they make. The wizard's Settings
// screen and --sandbox-classes (config.env's SANDBOX_CLASSES) choose any
// non-empty combination of three classes:
//
//   - gvisor: actors in gVisor on the gvisor node group;
//   - microvm: actors in Kata microVMs on the microvm node group (nested
//     virtualization);
//   - nitro, the AWS Nitro sandbox: each worker Pod an EKS Auto Mode
//     isolated sandbox (RuntimeClass isolated-sandbox) on nodes Auto Mode
//     launches, with no node group. It needs Auto Mode, and it is installed
//     by the plugin whose Class it is (mode3-iso), which also sets the
//     install's release to the fork that plugin installs. With
//     NITRO_COMPUTE=node-group it runs on the nitro node group instead, of
//     a Nitro Sandbox AMI (NITRO_AMI_ID), and needs no Auto Mode.
//
// A cluster the run creates gets the node groups its classes, database and
// plugins need, by the rule setup-aws applies (nodegroups.For): the gvisor
// and microvm groups of those classes, the nitro group of the nitro class
// with NITRO_COMPUTE=node-group, plus the microvm group for a plugin that
// adds it (mode 3), plus the system group without Auto Mode, with RDS and a
// sandbox group (the control plane is cordoned onto it), or with a plugin
// that runs on it. So Auto Mode with the bundled PostgreSQL makes only the
// sandbox groups, and with the nitro class alone on Auto Mode's nodes no
// node group.
// With KARPENTER=on (no Auto Mode) the rule is the same, and setup-aws makes
// the gvisor group as a Karpenter NodePool, and with KARPENTER_NODE_OVERLAY=on
// the microvm and nitro groups too.
// A connected cluster's node groups are the ones its nodes are labeled with,
// plus the Karpenter NodePools this installer made, which can have no node
// yet (Setup.ClusterPools), and a class or plugin whose nodes are missing
// is refused.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// The sandbox classes, in the order they are listed.
const (
	ClassGVisor  = "gvisor"
	ClassMicroVM = "microvm"
	ClassNitro   = "nitro"
)

// Classes are the sandbox classes, in the order they are listed.
var Classes = []string{ClassGVisor, ClassMicroVM, ClassNitro}

// ClassTitle is a class's name on screen.
func ClassTitle(c string) string {
	switch c {
	case ClassGVisor:
		return "gVisor"
	case ClassMicroVM:
		return "microVM (Kata)"
	case ClassNitro:
		return "AWS Nitro sandbox"
	}
	return c
}

// ParseClasses splits a comma-separated class list.
func ParseClasses(classes string) []string {
	var out []string
	for _, c := range strings.Split(classes, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// HasClass reports whether classes names c.
func HasClass(classes, c string) bool { return slices.Contains(ParseClasses(classes), c) }

// FormatClasses lists the classes of on in Classes' order.
func FormatClasses(on func(string) bool) string {
	var out []string
	for _, c := range Classes {
		if on(c) {
			out = append(out, c)
		}
	}
	return strings.Join(out, ",")
}

// ValidSandboxClasses reports why classes cannot be installed: it must name
// one or more of gvisor, microvm and nitro, each once, comma-separated.
func ValidSandboxClasses(classes string) error {
	seen := map[string]bool{}
	for _, c := range strings.Split(classes, ",") {
		if !slices.Contains(Classes, c) {
			return fmt.Errorf("sandbox classes %q: want one or more of %s, comma-separated", classes, strings.Join(Classes, ", "))
		}
		if seen[c] {
			return fmt.Errorf("sandbox classes %q name %s twice", classes, c)
		}
		seen[c] = true
	}
	return nil
}

// AteSetupClasses is the --sandbox-classes ate-setup gets, of its own two.
// In ate-setup v1.0.0 (cmd/ate-setup/internal/steps, parseSandboxClasses
// and applyInitialSandboxConfigs) gvisor applies one cluster-wide object,
// the gvisor-default SandboxConfig, and microvm stages the Kata assets in
// S3 and applies the microvm SandboxConfig; an empty value means both. Every
// gVisor-class worker pool resolves gvisor-default whatever nodes it runs
// on: the gVisor demo's, the AWS Nitro sandbox's counter-iso and mode 3's
// (sandboxClasses: gvisor, no configRef). So gvisor is always installed, and
// microvm with the microvm class only.
func AteSetupClasses(classes string) string {
	if HasClass(classes, ClassMicroVM) {
		return ClassGVisor + "," + ClassMicroVM
	}
	return ClassGVisor
}

// DemoClasses are the classes of classes the counter demo runs on: gvisor
// and microvm. The AWS Nitro sandbox's counter (counter-iso) comes with its
// class's plugin, and --verify checks it through that plugin.
func DemoClasses(classes string) []string {
	var out []string
	for _, c := range ParseClasses(classes) {
		if c == ClassGVisor || c == ClassMicroVM {
			out = append(out, c)
		}
	}
	return out
}

// ClassPlugins are the IDs of the plugins classes install (plugin.Class), in
// install order.
func ClassPlugins(classes string) []string {
	var out []string
	for _, c := range ParseClasses(classes) {
		for _, p := range plugin.ForClass(c) {
			out = append(out, p.ID)
		}
	}
	return out
}

// WithClassPlugins is ids plus the plugins classes install, once each.
func WithClassPlugins(ids []string, classes string) []string {
	out := slices.Clone(ids)
	for _, id := range ClassPlugins(classes) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// NewNodeGroups are the node groups `setup-aws create cluster` makes for a
// cluster with autoMode, this database (DatabaseRDS, also when empty, or
// DatabaseBundled), these classes, this NITRO_COMPUTE and these chosen
// plugins (and what they require): the gvisor and microvm groups of the
// classes and of the plugins that add theirs (plugin.AddsNodes), the nitro
// group of the nitro class with nitroCompute plugin.NitroNodeGroup, and the
// system group as nodegroups.For decides, pinned when one of the plugins
// runs on it.
func NewNodeGroups(autoMode bool, database, classes, nitroCompute string, plugins []string) []string {
	sandbox := DemoClasses(classes)
	if HasClass(classes, ClassNitro) && nitroCompute == plugin.NitroNodeGroup {
		sandbox = append(sandbox, nodegroups.Nitro)
	}
	pinned := false
	ps, _ := plugin.Resolve(WithClassPlugins(plugins, classes))
	for _, p := range ps {
		pinned = pinned || slices.Contains(p.Nodes, nodegroups.System)
		if !p.AddsNodes {
			continue
		}
		for _, n := range p.Nodes {
			if (n == nodegroups.GVisor || n == nodegroups.MicroVM) && !slices.Contains(sandbox, n) {
				sandbox = append(sandbox, n)
			}
		}
	}
	return nodegroups.For(autoMode, database != DatabaseBundled, pinned, sandbox)
}

// InstallPlugins are the plugins the install carries: the chosen ones and
// the ones its sandbox classes install. They decide the release, and the
// control plane carries their needs.
func (s *Setup) InstallPlugins() []string { return WithClassPlugins(s.Plugins, s.SandboxClasses) }

// NodeGroups are the node groups of the run's cluster: the ones a cluster
// the run creates gets (NewNodeGroups), or the ones a connected cluster's
// nodes come from.
func (s *Setup) NodeGroups() []string {
	if s.ClusterIsNew {
		return NewNodeGroups(s.AutoMode, s.Database, s.SandboxClasses, s.NewClusterConfig.NitroCompute, s.Plugins)
	}
	var out []string
	for _, g := range nodegroups.All {
		if slices.Contains(s.ClusterPools, g) {
			out = append(out, g)
		}
	}
	return out
}

// karpenterNodes are the node groups plugin p runs on that are Karpenter
// NodePools, when p cannot run on Karpenter's nodes (NoKarpenter): those
// KARPENTER makes NodePools on a new cluster, those a connected cluster has
// as NodePools.
func (s *Setup) karpenterNodes(p plugin.Plugin) []string {
	if p.NoKarpenter == "" {
		return nil
	}
	var out []string
	for _, n := range p.Nodes {
		if (s.ClusterIsNew && s.NewClusterConfig.IsPool(n)) || (!s.ClusterIsNew && slices.Contains(s.ClusterNodePools, n)) {
			out = append(out, n)
		}
	}
	return out
}

// ConnectedGroups splits a connected cluster's node groups (NodeGroups) into
// the node groups and the ones that are Karpenter NodePools
// (ClusterNodePools).
func (s *Setup) ConnectedGroups() (groups, nodePools []string) {
	for _, g := range s.NodeGroups() {
		if slices.Contains(s.ClusterNodePools, g) {
			nodePools = append(nodePools, g)
		} else {
			groups = append(groups, g)
		}
	}
	return groups, nodePools
}

// TagsProblem is why `setup-aws create cluster` refuses TAGS for a cluster
// with KARPENTER=on (karpenter) or without (nodegroups.CheckTags), short
// enough for the Settings row, or "": a key setup-aws sets itself, or with
// karpenter a key Karpenter sets on its nodes.
func TagsProblem(tags string, karpenter bool) string {
	for _, kv := range strings.Fields(tags) {
		if nodegroups.CheckTags(kv, karpenter) == nil {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "agent-substrate-on-eks/") {
			return k + " is a key setup-aws sets itself: remove it"
		}
		return k + " is a key Karpenter sets itself: remove it"
	}
	return ""
}

// ClassProblem is why class c cannot be chosen for a cluster with autoMode
// and this NITRO_COMPUTE, or "": the AWS Nitro sandbox needs Auto Mode
// (nitroCompute plugin.NitroAutoMode), and on a connected cluster (pools are
// its nodes' node groups; newCluster false) gvisor and microvm need their
// node group's nodes, and so does nitro with plugin.NitroNodeGroup. A new
// cluster makes the nitro group with plugin.NitroNodeGroup (NewNodeGroups).
func ClassProblem(c string, autoMode, newCluster bool, cluster string, pools []string, nitroCompute string) string {
	group := c != ClassNitro || nitroCompute == plugin.NitroNodeGroup
	switch {
	case !group && !autoMode && !newCluster && slices.Contains(pools, nodegroups.Nitro):
		return fmt.Sprintf("needs EKS Auto Mode: its isolated sandboxes run on nodes Auto Mode launches; %s has a nitro node group, which NITRO_COMPUTE=node-group uses", cluster)
	case !group && !autoMode:
		return "needs EKS Auto Mode: its isolated sandboxes run on nodes Auto Mode launches (NITRO_COMPUTE=node-group runs them on a nitro node group instead)"
	case group && !newCluster && !slices.Contains(pools, c):
		why := fmt.Sprintf("%s has no %s node group (no node labeled %s=%s)", cluster, c, plugin.PoolLabel, c)
		if c == ClassNitro && autoMode {
			// The cluster has Auto Mode, so auto-mode is the other way out.
			why += "; with NITRO_COMPUTE=auto-mode (--nitro-compute auto-mode) it runs on nodes Auto Mode launches instead"
		}
		return why
	}
	return ""
}

// PluginProblem is why plugin p cannot be chosen, with chosen already
// chosen, on s's cluster with s's sandbox classes, or "": it needs Auto
// Mode, or cannot run on Karpenter's nodes (NoKarpenter) and a node group it
// runs on is a Karpenter NodePool, or needs the RDS database when the run
// uses the bundled
// PostgreSQL, or nodes of a node group the cluster lacks or the run does
// not make, or (a plugin built on the counter demo) a gvisor or microvm
// class to run the demo on. What p requires counts too. A new cluster is
// never short of the system group: a plugin that runs on it makes it
// (nodegroups.For). A pool a Karpenter NodePool makes counts as a node
// group: its nodes come when the Pods need them.
func (s *Setup) PluginProblem(p plugin.Plugin, chosen []string) string {
	if why := p.AutoModeProblem(plugin.Options{AutoMode: s.AutoMode, NitroCompute: s.NewClusterConfig.NitroCompute}); why != "" {
		return why + "; " + s.ClusterName + " has Auto Mode off"
	}
	groups := s.NodeGroups()
	if s.ClusterIsNew {
		groups = NewNodeGroups(s.AutoMode, s.Database, s.SandboxClasses, s.NewClusterConfig.NitroCompute, append(slices.Clone(chosen), p.ID))
	}
	ps, err := plugin.Resolve([]string{p.ID})
	if err != nil {
		return err.Error()
	}
	for _, q := range ps {
		if pools := s.karpenterNodes(q); len(pools) > 0 {
			if s.ClusterIsNew {
				// KARPENTER makes only gvisor a NodePool; the others are
				// KARPENTER_NODE_OVERLAY's.
				setting, row := "KARPENTER", "Karpenter"
				if slices.ContainsFunc(pools, func(g string) bool { return !nodegroups.IsKarpenterPool(g, false) }) {
					setting, row = "KARPENTER_NODE_OVERLAY", "Karpenter overlay"
				}
				return fmt.Sprintf("%s; %s makes the %s node group a Karpenter NodePool (Settings: %s)", q.NoKarpenter, setting, strings.Join(pools, " and "), row)
			}
			return fmt.Sprintf("%s; the %s node group of %s is a Karpenter NodePool", q.NoKarpenter, strings.Join(pools, " and "), s.ClusterName)
		}
		if q.NeedsRDS != "" && s.Database == DatabaseBundled {
			return q.NeedsRDS + "; the run uses the bundled PostgreSQL (Settings: Database)"
		}
		miss := plugin.MissingNodes(q, groups)
		switch {
		case len(miss) == 0:
		case !s.ClusterIsNew:
			return fmt.Sprintf("runs on the %s node group, which %s does not have", strings.Join(miss, " and "), s.ClusterName)
		default:
			return fmt.Sprintf("runs on the %s node group: turn on its sandbox class in Settings", strings.Join(miss, " and "))
		}
		if q.NeedsDemo && len(DemoClasses(s.SandboxClasses)) == 0 {
			return "builds on the counter demo, which runs on the gVisor or microVM sandbox class: turn one on in Settings"
		}
	}
	return ""
}
