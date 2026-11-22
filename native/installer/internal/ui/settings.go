package ui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// settingsScreen edits what config.env controls for the run: the new
// cluster's knobs (only when the run creates the cluster), the sandbox
// classes, the node groups they make (only those), TAGS, the release and
// its image repository, and the database. Values are checked with
// state.ValidateConfig; n applies them to the Setup and moves on, and the
// install writes them to config.local.env when it starts (Review).
type settingsScreen struct {
	fields  []field
	cursor  int
	values  map[string]string // config keys, plus dbKey
	errs    map[string]string
	editing bool
	input   string
	// nitro is the NITRO_COMPUTE=node-group Settings set for the nitro
	// class without Auto Mode (switchNitro), and nitroApplied the one n
	// applied with the values, which the next visit starts from.
	nitro, nitroApplied nitroSwitch
}

// nitroSwitch records a NITRO_COMPUTE=node-group that Settings set, not the
// operator, and the value it replaced (was).
type nitroSwitch struct {
	on  bool
	was string
}

type fieldKind int

const (
	fText fieldKind = iota
	fToggle
	fClass         // one sandbox class, on or off
	fDatabase      // RDS or the bundled PostgreSQL
	fNodeGroupType // NODE_GROUP_TYPE, managed or self-managed
	fNitroCompute  // NITRO_COMPUTE, auto-mode or node-group
)

// The values that are not edited as text: the classes (config.env's
// SANDBOX_CLASSES, toggled one class at a time) and the database.
const (
	classKey = "SANDBOX_CLASSES"
	dbKey    = "database"
)

// field is one focusable setting. Node group rows pair an instance type
// with a node count (group set).
type field struct {
	kind    fieldKind
	key     string // config key, or the sandbox class
	label   string
	section string
	group   string
}

// settingsFields are the fields for a new or connected cluster, with node
// group rows for groups, the node groups a new cluster gets. A new cluster
// gets the row that says where the nitro class runs (NITRO_COMPUTE) when
// that class is on or Auto Mode is off (nitroRow: off Auto Mode the class
// needs node-group), the nitro node group's AMI row when the group is made
// (nitroAMI), the Karpenter row when Auto Mode is off or KARPENTER is set
// to something other than off (karpenterRow: Karpenter needs Auto Mode
// off), and the Karpenter overlay row when KARPENTER is on or
// KARPENTER_NODE_OVERLAY is set to something other than off (overlayRow:
// the overlay needs Karpenter, and a value it refuses is shown).
func settingsFields(newCluster bool, groups []string, nitroRow, nitroAMI, karpenterRow, overlayRow bool) []field {
	var f []field
	if newCluster {
		f = append(f,
			field{kind: fText, key: "KUBERNETES_VERSION", label: "Kubernetes version", section: "New cluster"},
			field{kind: fToggle, key: "AUTO_MODE", label: "EKS Auto Mode", section: "New cluster"},
			field{kind: fText, key: "EKS_PUBLIC_ACCESS_CIDRS", label: "API access CIDRs", section: "New cluster"},
			field{kind: fText, key: "NODE_AMI_ID", label: "Node AMI", section: "New cluster"},
			field{kind: fNodeGroupType, key: "NODE_GROUP_TYPE", label: "Node group type", section: "New cluster"})
		if karpenterRow {
			f = append(f, field{kind: fToggle, key: "KARPENTER", label: "Karpenter", section: "New cluster"})
		}
		if overlayRow {
			f = append(f, field{kind: fToggle, key: "KARPENTER_NODE_OVERLAY", label: "Karpenter overlay", section: "New cluster"})
		}
	}
	for _, c := range state.Classes {
		f = append(f, field{kind: fClass, key: c, label: state.ClassTitle(c), section: "Sandbox classes"})
	}
	if newCluster && nitroRow {
		f = append(f, field{kind: fNitroCompute, key: "NITRO_COMPUTE", label: "Nitro sandbox on", section: "Sandbox classes"})
		if nitroAMI {
			f = append(f, field{kind: fText, key: "NITRO_AMI_ID", label: "Nitro AMI", section: "Sandbox classes"})
		}
	}
	if newCluster {
		for _, g := range groups {
			up := strings.ToUpper(g)
			f = append(f,
				field{kind: fText, key: up + "_INSTANCE_TYPE", label: "instance type", section: "Node groups", group: g},
				field{kind: fText, key: up + "_NODES", label: "nodes", section: "Node groups", group: g})
		}
	}
	return append(f,
		field{kind: fText, key: "TAGS", label: "Tags", section: "Every AWS resource"},
		field{kind: fText, key: "SUBSTRATE_TAG", label: "Release", section: "Agent Substrate"},
		field{kind: fText, key: "SUBSTRATE_IMAGE_REPO", label: "Image repository", section: "Agent Substrate"},
		field{kind: fDatabase, key: dbKey, label: "Database", section: "Agent Substrate"})
}

// classesHelp is what every class's help ends with: the classes decide the
// node groups.
const classesHelp = " The classes decide a new cluster's node groups: gVisor and microVM make theirs. " +
	"The system group comes without Auto Mode, with either of them and RDS (the control plane is pinned there), " +
	"and with a plugin that runs on it. With neither class, an Auto Mode cluster gets a node group only from a plugin: " +
	"mode 3 adds the microvm group (and with RDS the system group), and a plugin that runs on the system group " +
	"(the LLM gateway, or login rotation with RDS) adds that one. Without such a plugin it gets none: " +
	"the control plane, the bundled PostgreSQL and every controller run on Auto Mode nodes. Press m for that minimal profile."

// help is a setting's explanation when config.env has no comment above it.
var help = map[string]string{
	"KUBERNETES_VERSION":      "The EKS version of the new cluster. Substrate needs 1.37 or newer (the PodCertificate APIs).",
	"AUTO_MODE":               "EKS Auto Mode, with its general-purpose and system NodePools next to the node groups. Fixed when the cluster is created. The AWS Nitro sandbox class needs it, unless it runs on the nitro node group (NITRO_COMPUTE=node-group).",
	"EKS_PUBLIC_ACCESS_CIDRS": "Address ranges allowed to reach the Kubernetes API endpoint, space-separated. Requests are still authenticated by IAM.",
	"NODE_AMI_ID":             "The AMI of the node groups. Empty: the Amazon EKS optimized Amazon Linux 2023 AMI, which EKS picks. An AMI you bring must join the cluster with nodeadm (Amazon Linux 2023).",
	"NODE_GROUP_TYPE":         "managed: EKS managed node groups. self-managed: an Auto Scaling group per node group, which you own, whose nodes join with the NodeConfig in their launch template. A cluster has one type.",
	"TAGS":                    "Space-separated key=value tags on every AWS resource the install creates. The owner tag agent-substrate-on-eks/deployment=<cluster>.<region> is always added.",
	"SUBSTRATE_TAG":           "The Agent Substrate release to install, such as v1.0.0. An install runs one release: with the AWS Nitro sandbox class (or the mode 3 plugin) it is the fork's release that class installs, and this one is kept for a run without it.",
	"SUBSTRATE_IMAGE_REPO":    "Where the release's images come from.",
	state.ClassGVisor:         "gVisor runs actors in a user-space kernel, on the gvisor node group." + classesHelp,
	state.ClassMicroVM:        "microVM runs actors in Kata microVMs on the microvm node group, whose instance type needs nested virtualization." + classesHelp,
	"KARPENTER": "on: Karpenter " + nodegroups.KarpenterVersion + " makes the gvisor nodes in a NodePool, from no node up to GVISOR_NODES, " +
		"only when a Pod needs one, and removes a node when it is empty. Karpenter runs on the system node group. Needs EKS Auto Mode off " +
		"and the gVisor class. The microvm and nitro classes stay on their node groups. Fixed when the cluster is created.",
	"KARPENTER_NODE_OVERLAY": "on: Karpenter also makes the microvm and nitro nodes in NodePools, up to MICROVM_NODES and NITRO_NODES, " +
		"with a NodeOverlay each that tells Karpenter the resource their worker Pods ask for. NodeOverlay is an alpha Karpenter API: it can change. " +
		"Needs Karpenter on. The mode 3 plugin cannot run then. Fixed when the cluster is created.",
	state.ClassNitro: "The AWS Nitro sandbox makes each worker Pod an EKS Auto Mode isolated sandbox (RuntimeClass isolated-sandbox) " +
		"on nodes Auto Mode launches for it, so it needs Auto Mode and makes no node group. With Nitro sandbox on node-group " +
		"(NITRO_COMPUTE) it runs on the nitro node group instead, from a Nitro Sandbox AMI you build, and needs no Auto Mode. " +
		"It installs the fork's release (one release per install), and is set up and checked in its own step after the control plane." + classesHelp,
	"NITRO_COMPUTE": "Where the AWS Nitro sandbox runs. auto-mode: on nodes EKS Auto Mode launches (NodePool ate-isolated), which needs Auto Mode. " +
		"node-group: on the nitro node group, of the node group type, or with the Karpenter overlay the nitro Karpenter NodePool, with Nitro Enclaves on, from a Nitro Sandbox AMI (Nitro AMI); no Auto Mode needed. " +
		"With Auto Mode off, Settings sets node-group when you turn the class on, and puts the old value back when you turn the class off or Auto Mode on, within one run. " +
		"A value you pick yourself stays, and so does one a run wrote to config.local.env.",
	// The panel adds the build command for the version Settings has
	// (nitroAMIBuild) and shows this, not config.env's comment, whose
	// command is for one version.
	"NITRO_AMI_ID": "The nitro node group's AMI: a Nitro Sandbox AMI, built with awslabs/amazon-eks-ami for the cluster's Kubernetes version " +
		"(enable_pod_isolation=true). The EKS optimized AMI cannot run the Nitro sandbox: create cluster waits until the nodes advertise " +
		"eks.amazonaws.com/nitro-sandbox, and stops when they do not.",
	"NITRO_INSTANCE_TYPE": "The nitro node group runs the AWS Nitro sandbox's worker Pods; its instance type needs Nitro Enclaves.",
	dbKey: "Where the control plane keeps its state: Amazon RDS for PostgreSQL (adds the Create the database step), or the bundled PostgreSQL in the cluster. " +
		"On a new Auto Mode cluster with a gVisor or microVM group, RDS pins the control plane to the system node group, which is made for it; " +
		"with the bundled PostgreSQL the control plane runs on Auto Mode nodes, and the system group is made only for a plugin that runs on it.",
	"SYSTEM_INSTANCE_TYPE":  "The system node group runs the controllers pinned to it: the control plane with RDS on Auto Mode, and the plugins' controllers.",
	"GVISOR_INSTANCE_TYPE":  "The gvisor node group runs gVisor actors.",
	"MICROVM_INSTANCE_TYPE": "The microvm node group runs microVM actors; its instance type needs nested virtualization (m8i, c8i, r8i).",
}

// noGroupsLine is the Node groups section of a new cluster that gets none.
const noGroupsLine = "none: everything runs on EKS Auto Mode nodes"

// classNote is a class's note beside its box: where it runs.
var classNote = map[string]string{
	state.ClassGVisor:  "gvisor node group",
	state.ClassMicroVM: "microvm node group",
	state.ClassNitro:   "on Auto Mode nodes",
}

// nitroGroupNote is the nitro class's note on the nitro node group.
const nitroGroupNote = "nitro node group"

// nitroSwitchLine is the line under the nitro class while Settings' own
// NITRO_COMPUTE=node-group holds (switchNitro).
const nitroSwitchLine = "NITRO_COMPUTE is now node-group: Auto Mode is off"

// nitroSwitchHint is the nitro class's panel line while turning the class
// on would make that switch (nitroSwitches).
const nitroSwitchHint = "Turning the class on sets NITRO_COMPUTE=node-group: Auto Mode is off."

// nitroAMIBuild is the panel's line on how to build NITRO_AMI_ID for
// Kubernetes version, the KUBERNETES_VERSION Settings has now.
func nitroAMIBuild(version string) string {
	return "For Kubernetes " + version + ", build it " + nodegroups.NitroAMIBuild(version) + "."
}

// nitroSubtitle is the Set up Nitro sandboxes step's subtitle for
// NITRO_COMPUTE (compute; empty is auto-mode).
func nitroSubtitle(compute string) string {
	if compute == plugin.NitroNodeGroup {
		return "The AWS Nitro sandbox class: RuntimeClass isolated-sandbox, atelet-iso and a counter pool on the nitro node group (ate.dev/pool=nitro), checked across a suspend and resume"
	}
	return "The AWS Nitro sandbox class: NodePool ate-isolated on EKS Auto Mode, atelet-iso, and a counter pool on RuntimeClass isolated-sandbox, checked across a suspend and resume"
}

// nitroGroup reports NITRO_COMPUTE=node-group as Settings has it now.
func (st *settingsScreen) nitroGroup() bool {
	return st.values["NITRO_COMPUTE"] == plugin.NitroNodeGroup
}

// settingsKarpenter reports KARPENTER=on as Settings has it now. Only a new
// cluster's rows and findings read it.
func (m *Model) settingsKarpenter() bool {
	return m.set.values["KARPENTER"] == nodegroups.KarpenterOn
}

// settingsNodeOverlay reports KARPENTER_NODE_OVERLAY=on with KARPENTER=on, as
// Settings has them now.
func (m *Model) settingsNodeOverlay() bool {
	return m.settingsKarpenter() && m.set.values["KARPENTER_NODE_OVERLAY"] == nodegroups.KarpenterOn
}

// karpenterNotes are the notes of the node group rows Karpenter makes
// NodePools (a microvm row's keeps nested virtualization), and
// karpenterNodes the line under the rows' header: a NodePool's node count
// is its limit. The left column fits 17 cells of note.
var karpenterNotes = map[string]string{nodegroups.GVisor: " NodePool", nodegroups.MicroVM: " nested, NodePool", nodegroups.Nitro: " NodePool"}

const karpenterNodes = "  nodes of a NodePool: its limit"

// nitroCompute is the NITRO_COMPUTE the nitro class is judged on: Settings'
// value, except node-group while turning the class on sets it
// (nitroSwitches).
func (m *Model) nitroCompute() string {
	if m.nitroSwitches() {
		return plugin.NitroNodeGroup
	}
	return m.set.values["NITRO_COMPUTE"]
}

// nitroSwitches reports that turning the nitro class on now sets
// NITRO_COMPUTE=node-group (switchNitro): the class is off, Auto Mode is
// off, and the value is auto-mode or empty.
func (m *Model) nitroSwitches() bool {
	st := m.set
	return !st.classOn(state.ClassNitro) && !m.settingsAutoMode() && nitroSwitchable(st.values["NITRO_COMPUTE"])
}

// nitroSwitchable reports a NITRO_COMPUTE Settings may replace with
// node-group: auto-mode, or empty, which means auto-mode.
func nitroSwitchable(v string) bool { return v == plugin.NitroAutoMode || v == "" }

// switchNitro sets NITRO_COMPUTE=node-group when the nitro class is on with
// Auto Mode off and NITRO_COMPUTE is auto-mode or empty: without Auto Mode
// the class runs only on the nitro node group. It runs when the class is
// turned on, when Auto Mode is turned off and when Settings opens, and
// keeps the value it replaced for restoreNitro.
func (m *Model) switchNitro() {
	st := m.set
	if v := st.values["NITRO_COMPUTE"]; st.classOn(state.ClassNitro) && !m.settingsAutoMode() && nitroSwitchable(v) {
		st.values["NITRO_COMPUTE"], st.nitro = plugin.NitroNodeGroup, nitroSwitch{on: true, was: v}
	}
}

// restoreNitro puts back the NITRO_COMPUTE switchNitro replaced once the
// switch is not needed: the nitro class is off or Auto Mode is on. It
// returns the notice, or "". A value the operator set is never changed.
// The value put back is auto-mode or empty, which means auto-mode, so the
// notice says auto-mode.
func (m *Model) restoreNitro() string {
	st := m.set
	if !st.nitro.on || (st.classOn(state.ClassNitro) && !m.settingsAutoMode()) {
		return ""
	}
	st.values["NITRO_COMPUTE"], st.nitro = st.nitro.was, nitroSwitch{}
	return "Back to NITRO_COMPUTE=" + plugin.NitroAutoMode
}

// fitNitro follows the nitro class or Auto Mode turned on or off: it sets
// NITRO_COMPUTE=node-group, or puts back the value Settings replaced.
func (m *Model) fitNitro() {
	m.switchNitro()
	if back := m.restoreNitro(); back != "" {
		m.notice = back
	}
}

// classShort is the note of a class this run cannot have; the panel beside
// says why in full.
func (m *Model) classShort(c string) string {
	if c == state.ClassNitro && m.nitroCompute() != plugin.NitroNodeGroup {
		return "needs EKS Auto Mode"
	}
	return "no " + c + " node group"
}

func (m *Model) enterSettings() {
	st := m.set
	st.values = state.ConfigValues(m.s)
	st.values[dbKey] = m.s.Database
	st.errs = map[string]string{}
	st.editing = false
	// The values are the Setup's again, and so is Settings' switch. A
	// cluster with Auto Mode, connected or new, does not need it, so the
	// old value comes back. Without Auto Mode, a class already on with
	// auto-mode (from another cluster, config.local.env or
	// --sandbox-classes) gets node-group, as when it is turned on.
	st.nitro = st.nitroApplied
	if back := m.restoreNitro(); back != "" {
		m.notice = back
	}
	m.switchNitro()
	m.refreshFields()
	st.cursor = min(st.cursor, len(st.fields)-1)
	m.checkSettings()
}

// autoMode is the run's Auto Mode as Settings has it now: the AUTO_MODE
// value for a new cluster, the cluster's own otherwise.
func (m *Model) settingsAutoMode() bool {
	if m.s.ClusterIsNew {
		return m.set.values["AUTO_MODE"] == "on"
	}
	return m.s.AutoMode
}

// settingsGroups are the node groups of the cluster as Settings has it now.
func (m *Model) settingsGroups() []string {
	if m.s.ClusterIsNew {
		return state.NewNodeGroups(m.settingsAutoMode(), m.set.values[dbKey], m.set.values[classKey], m.set.values["NITRO_COMPUTE"], m.s.Plugins)
	}
	return m.s.NodeGroups()
}

// refreshFields rebuilds the fields for the current values, whose classes
// and Auto Mode decide the node group rows, keeping the cursor on its key.
func (m *Model) refreshFields() {
	st := m.set
	at := ""
	if st.cursor >= 0 && st.cursor < len(st.fields) {
		at = st.fields[st.cursor].key
	}
	nitro := st.classOn(state.ClassNitro)
	k, o := st.values["KARPENTER"], st.values["KARPENTER_NODE_OVERLAY"]
	st.fields = settingsFields(m.s.ClusterIsNew, m.settingsGroups(), nitro || !m.settingsAutoMode(), nitro && st.nitroGroup(),
		!m.settingsAutoMode() || (k != "" && k != "off"), m.settingsKarpenter() || (o != "" && o != "off"))
	for i, f := range st.fields {
		if f.key == at {
			st.cursor = i
		}
	}
	st.cursor = min(st.cursor, len(st.fields)-1)
}

func (st *settingsScreen) classOn(c string) bool { return state.HasClass(st.values[classKey], c) }

// classProblem is why class c cannot be on for this run, or "". The nitro
// class is judged on nitroCompute: off Auto Mode, on the node group that
// turning it on sets.
func (m *Model) classProblem(c string) string {
	return state.ClassProblem(c, m.settingsAutoMode(), m.s.ClusterIsNew, m.s.ClusterName, m.s.ClusterPools, m.nitroCompute())
}

// toggleClass turns a sandbox class on or off, keeping at least one, in
// state.Classes' order; a class this run cannot have is not turned on.
func (m *Model) toggleClass(c string) string {
	st := m.set
	if !st.classOn(c) {
		if why := m.classProblem(c); why != "" {
			return state.ClassTitle(c) + " " + why
		}
	}
	on := state.FormatClasses(func(k string) bool { return st.classOn(k) != (k == c) })
	if on == "" {
		return "At least one sandbox class is installed."
	}
	st.values[classKey] = on
	return ""
}

// minimal is the m key: the minimal profile, Auto Mode with the AWS Nitro
// sandbox class only and the bundled PostgreSQL. A new cluster then gets no
// node group, and everything runs on Auto Mode nodes: a chosen plugin that
// would add one is cleared, and the notice names it.
func (m *Model) minimal() string {
	st := m.set
	if m.s.ClusterIsNew {
		// Karpenter needs Auto Mode off, and its overlay Karpenter.
		st.values["AUTO_MODE"], st.values["KARPENTER"], st.values["KARPENTER_NODE_OVERLAY"] = "on", "", ""
	} else if !m.s.AutoMode {
		return "The minimal profile needs EKS Auto Mode, and " + m.s.ClusterName + " has it off."
	}
	// The minimal profile runs the Nitro sandbox on Auto Mode's nodes. It
	// sets NITRO_COMPUTE itself, so no switch of Settings' is left to undo.
	st.values[classKey], st.values[dbKey], st.values["NITRO_COMPUTE"] = state.ClassNitro, state.DatabaseBundled, plugin.NitroAutoMode
	st.nitro = nitroSwitch{}
	if !m.s.ClusterIsNew {
		return "Minimal profile: the AWS Nitro sandbox only and the bundled PostgreSQL."
	}
	if cleared := m.clearGroupPlugins(); len(cleared) > 0 {
		return "Cleared " + andList(cleared) + " for the minimal profile (no node group); choose again on the Plugins screen to keep the group"
	}
	return "Minimal profile: EKS Auto Mode, the AWS Nitro sandbox only, the bundled PostgreSQL; no node group."
}

// clearGroupPlugins clears every chosen plugin that would give the minimal
// profile's new cluster a node group (mode 3's microvm group, the system
// group of a plugin that runs on it), so the profile keeps its promise, and
// returns their IDs. Plugins that add no group stay chosen. A selection the
// Plugins screen has not made yet is only the defaults, which it then
// preselects for the minimal profile's settings (enterPlugins).
func (m *Model) clearGroupPlugins() []string {
	ids := m.s.Plugins
	if m.pl.shown {
		ids = m.chosenIDs()
	}
	var cleared []string
	for _, id := range ids {
		if len(state.NewNodeGroups(true, state.DatabaseBundled, state.ClassNitro, plugin.NitroAutoMode, []string{id})) > 0 {
			cleared = append(cleared, id)
		}
	}
	if len(cleared) == 0 {
		return nil
	}
	if m.pl.shown {
		for _, id := range cleared {
			m.setPlugin(id, false)
		}
		m.s.Plugins = m.chosenIDs()
	} else {
		m.s.Plugins = slices.DeleteFunc(slices.Clone(m.s.Plugins), func(id string) bool { return slices.Contains(cleared, id) })
	}
	return cleared
}

// checkSettings validates every value, for the screen and for n: the config
// keys that have a row (only the node groups the cluster gets), each class
// against the cluster, and the node group rule.
func (m *Model) checkSettings() {
	st := m.set
	st.errs = state.ValidateConfig(st.values)
	// A key with no row (a connected cluster's cluster keys, the node
	// groups the cluster does not get) is not used, so it cannot need a fix.
	// The classes have a row each.
	shown := map[string]bool{classKey: true}
	for _, f := range st.fields {
		shown[f.key] = true
	}
	for k := range st.errs {
		if !shown[k] {
			delete(st.errs, k)
		}
	}
	// The nitro node group has no AMI to fall back to: setup-aws refuses it
	// without one, so Settings does first.
	if _, bad := st.errs["NITRO_AMI_ID"]; shown["NITRO_AMI_ID"] && !bad && strings.TrimSpace(st.values["NITRO_AMI_ID"]) == "" {
		st.errs["NITRO_AMI_ID"] = "a Nitro Sandbox AMI is needed: build one with awslabs/amazon-eks-ami (enable_pod_isolation=true)"
	}
	m.checkKarpenter()
	if _, bad := st.errs[classKey]; bad {
		return
	}
	// Short, to fit under the classes; the class's panel says why in full.
	for _, c := range state.ParseClasses(st.values[classKey]) {
		if m.classProblem(c) != "" {
			st.errs[classKey] = state.ClassTitle(c) + ": " + m.classShort(c)
			return
		}
	}
	// Defensive: cannot fire today, as classProblem above refuses the only set Check refuses (nitro alone, Auto Mode off).
	if m.s.ClusterIsNew {
		if err := nodegroups.Check(m.settingsAutoMode(), m.settingsGroups()); err != nil {
			st.errs[classKey] = err.Error()
		}
	}
}

// checkKarpenter adds the findings of KARPENTER and KARPENTER_NODE_OVERLAY
// on a new cluster. TAGS may not set a key setup-aws sets itself, nor with
// KARPENTER=on one Karpenter sets (state.TagsProblem). KARPENTER=on needs
// Auto Mode off, and the gvisor group to make a NodePool for;
// KARPENTER_NODE_OVERLAY=on needs KARPENTER=on.
func (m *Model) checkKarpenter() {
	st := m.set
	if !m.s.ClusterIsNew {
		return
	}
	if _, bad := st.errs["TAGS"]; !bad {
		if why := state.TagsProblem(st.values["TAGS"], m.settingsKarpenter()); why != "" {
			st.errs["TAGS"] = why
		}
	}
	if _, bad := st.errs["KARPENTER_NODE_OVERLAY"]; !bad && st.values["KARPENTER_NODE_OVERLAY"] == nodegroups.KarpenterOn && !m.settingsKarpenter() {
		st.errs["KARPENTER_NODE_OVERLAY"] = "needs Karpenter on"
	}
	if _, bad := st.errs["KARPENTER"]; bad || !m.settingsKarpenter() {
		return
	}
	switch {
	case m.settingsAutoMode():
		st.errs["KARPENTER"] = "needs EKS Auto Mode off: Auto Mode runs its own Karpenter"
	case nodegroups.CheckKarpenter(nodegroups.KarpenterOn, false, m.settingsGroups()) != nil:
		st.errs["KARPENTER"] = "makes a NodePool for gVisor: turn that class on"
	}
}

// release is the release the run installs with the current classes, and
// what sets it when a class or plugin replaces SUBSTRATE_TAG's ("" when
// none does).
func (m *Model) settingsRelease() (plugin.Release, string) {
	st := m.set
	ids := state.WithClassPlugins(m.s.Plugins, st.values[classKey])
	rel := plugin.ChosenRelease(ids, st.values["SUBSTRATE_TAG"], st.values["SUBSTRATE_IMAGE_REPO"])
	if rel.Repo == plugin.UpstreamRepo {
		return rel, ""
	}
	if st.classOn(state.ClassNitro) {
		return rel, "the AWS Nitro sandbox class"
	}
	return rel, "a chosen plugin"
}

// apply writes the answers to the Setup. A new cluster is recorded again,
// so its version and Auto Mode follow the edited knobs (review F13).
func (m *Model) applySettings() {
	v := m.set.values
	c := &m.s.NewClusterConfig
	for _, k := range []struct {
		key string
		dst *string
	}{
		{"KUBERNETES_VERSION", &c.KubernetesVersion}, {"AUTO_MODE", &c.AutoMode}, {"EKS_PUBLIC_ACCESS_CIDRS", &c.PublicAccessCIDRs},
		{"SYSTEM_INSTANCE_TYPE", &c.SystemInstanceType}, {"GVISOR_INSTANCE_TYPE", &c.GVisorInstanceType}, {"MICROVM_INSTANCE_TYPE", &c.MicroVMInstanceType},
		{"SYSTEM_NODES", &c.SystemNodes}, {"GVISOR_NODES", &c.GVisorNodes}, {"MICROVM_NODES", &c.MicroVMNodes},
		{"NODE_AMI_ID", &c.NodeAMIID}, {"NODE_GROUP_TYPE", &c.NodeGroupType},
		{"NITRO_COMPUTE", &c.NitroCompute}, {"NITRO_AMI_ID", &c.NitroAMIID},
		{"NITRO_INSTANCE_TYPE", &c.NitroInstanceType}, {"NITRO_NODES", &c.NitroNodes}, {"KARPENTER", &c.Karpenter},
		{"KARPENTER_NODE_OVERLAY", &c.KarpenterNodeOverlay},
		{"TAGS", &m.s.Tags}, {"SUBSTRATE_TAG", &m.s.SubstrateTag}, {"SUBSTRATE_IMAGE_REPO", &m.s.ImageRepo},
		{classKey, &m.s.SandboxClasses}, {dbKey, &m.s.Database},
	} {
		*k.dst = v[k.key]
	}
	m.set.nitroApplied = m.set.nitro
	if m.s.ClusterIsNew {
		m.s.NewCluster(m.s.ClusterName)
	}
}

func (m *Model) settingsKey(msg tea.KeyMsg) tea.Cmd {
	st := m.set
	f := st.fields[st.cursor]
	key := msg.String()
	if st.editing {
		switch key {
		case "enter":
			st.values[f.key] = strings.TrimSpace(st.input)
			st.editing = false
			m.checkSettings()
		case "esc":
			st.editing = false
		default:
			st.input = editN(st.input, msg, 200)
		}
		return nil
	}
	switch key {
	case "up", "k", "shift+tab":
		st.cursor = (st.cursor + len(st.fields) - 1) % len(st.fields)
	case "down", "j", "tab":
		st.cursor = (st.cursor + 1) % len(st.fields)
	case "left", "right":
		// Across a paired row: the instance type and its node count.
		if f.group != "" {
			if key == "left" && st.cursor > 0 && st.fields[st.cursor-1].group == f.group {
				st.cursor--
			} else if key == "right" && st.cursor+1 < len(st.fields) && st.fields[st.cursor+1].group == f.group {
				st.cursor++
			}
		}
	case "m":
		m.notice = m.minimal()
		m.refreshFields()
		m.checkSettings()
	case "enter", " ":
		switch f.kind {
		case fToggle:
			if st.values[f.key] == "on" {
				st.values[f.key] = ""
				// The overlay needs Karpenter: it goes with it.
				if f.key == "KARPENTER" {
					st.values["KARPENTER_NODE_OVERLAY"] = ""
				}
			} else {
				st.values[f.key] = "on"
			}
			if f.key == "AUTO_MODE" {
				m.fitNitro()
			}
		case fClass:
			if why := m.toggleClass(f.key); why != "" {
				m.notice = why
			} else if f.key == state.ClassNitro {
				m.fitNitro()
			}
		case fDatabase:
			if st.values[dbKey] == state.DatabaseRDS {
				st.values[dbKey] = state.DatabaseBundled
			} else {
				st.values[dbKey] = state.DatabaseRDS
			}
		case fNodeGroupType:
			if st.values[f.key] == nodegroups.SelfManaged {
				st.values[f.key] = nodegroups.Managed
			} else {
				st.values[f.key] = nodegroups.SelfManaged
			}
		case fNitroCompute:
			if st.nitroGroup() {
				st.values[f.key] = plugin.NitroAutoMode
			} else {
				st.values[f.key] = plugin.NitroNodeGroup
			}
			// The operator's value now, which Settings never changes back.
			st.nitro = nitroSwitch{}
		default:
			if rel, by := m.settingsRelease(); f.key == "SUBSTRATE_TAG" && by != "" {
				m.notice = by + " installs " + rel.Tag + " (one release per install); turn it off to choose the release"
				break
			}
			if key == "enter" {
				st.editing, st.input = true, st.values[f.key]
			}
		}
		m.refreshFields()
		m.checkSettings()
	case "n":
		m.checkSettings()
		if len(st.errs) > 0 {
			m.notice = fmt.Sprintf("%d setting(s) need a fix before the review", len(st.errs))
			return nil
		}
		m.applySettings()
		return m.commit()
	case "esc", "b":
		m.m.Prev()
	case "q":
		m.quitting = true
		return tea.Quit
	}
	return nil
}

func (m Model) settingsHints() []string {
	if m.set.editing {
		return []string{"enter save", "esc cancel"}
	}
	return []string{"↑/↓ move", "enter edit / toggle", "n review", "esc back", "q quit"}
}

func (m Model) settingsView() string {
	st := m.set
	total := m.bodyWidth() - 2
	rightW := max(total-54, 24)
	leftW := total - rightW - 2
	src := "built-in defaults (no config.env found)"
	if d := m.d.Config.Dir; d != "" {
		src = "defaults from " + tildify(filepath.Join(d, "config.env")) + " and config.local.env"
	}
	left := []string{theme.Title.Render("Settings") + "  " + theme.Subtle.Render(truncate(src, leftW-10))}
	if !m.s.ClusterIsNew {
		auto := "Auto Mode off"
		if m.s.AutoMode {
			auto = "Auto Mode on"
		}
		groups := "no node groups: Auto Mode nodes only"
		if g := m.s.NodeGroups(); len(g) > 0 {
			groups = "node groups: " + strings.Join(g, ", ")
		}
		// Karpenter's NodePools, on a line of their own.
		var pools []string
		if ngs, nps := m.s.ConnectedGroups(); len(nps) > 0 {
			groups = "node groups: " + strings.Join(ngs, ", ")
			pools = append(pools, "  "+theme.Subtle.Render(truncate("Karpenter NodePools: "+strings.Join(nps, ", "), leftW-4)))
		}
		left = append(left, "", theme.Title.Render("Cluster"),
			"  "+m.s.ClusterName+theme.Subtle.Render(fmt.Sprintf(" · EKS %s · %s", m.s.ClusterVersion, auto)),
			"  "+theme.Subtle.Render(truncate(groups, leftW-4)))
		left = append(append(left, pools...),
			theme.Fainted.Render(indent(lipgloss.NewStyle().Width(leftW-2).Render(
				"connected, not created: the new-cluster settings apply only to a cluster this run creates."), "  ")))
	}
	rel, relBy := m.settingsRelease()
	cell := func(i, w int) string {
		f := st.fields[i]
		shown := st.values[f.key]
		style := lipgloss.NewStyle()
		switch f.kind {
		case fToggle:
			shown = "off"
			if st.values[f.key] == "on" {
				shown = "on"
			}
		case fClass:
			box := "[ ]"
			switch {
			case st.classOn(f.key):
				box = "[" + theme.GlyphDone + "]"
			case m.classProblem(f.key) != "":
				box = "[–]"
			}
			shown = box + " " + f.label
		case fDatabase:
			shown = "Amazon RDS for PostgreSQL"
			if st.values[dbKey] == state.DatabaseBundled {
				shown = "Bundled PostgreSQL"
			}
		}
		if f.key == "SUBSTRATE_TAG" && relBy != "" {
			shown = rel.Tag + " (fork)"
		}
		if f.key == "NODE_AMI_ID" && shown == "" {
			shown = "EKS optimized AL2023"
		}
		if f.key == "NITRO_AMI_ID" && shown == "" {
			shown = "none"
		}
		if st.editing && i == st.cursor {
			in := []rune(st.input)
			if len(in) > w-3 {
				in = in[len(in)-(w-3):] // the end, where the cursor is
			}
			return theme.Selected.Render(pad(" "+string(in)+"▏", w))
		}
		if _, bad := st.errs[f.key]; bad {
			style = theme.Bad
		}
		if f.kind == fClass {
			if _, bad := st.errs[classKey]; bad && st.classOn(f.key) {
				style = theme.Bad
			}
		}
		if i == st.cursor {
			return theme.Selected.Render(pad(" "+truncate(shown, w-2), w))
		}
		return " " + style.Render(pad(truncate(shown, w-2), w-1))
	}
	section := ""
	for i := 0; i < len(st.fields); i++ {
		f := st.fields[i]
		if f.section != section {
			// A new cluster without node groups says so where their rows
			// would be.
			if f.section == "Every AWS resource" && m.s.ClusterIsNew && len(m.settingsGroups()) == 0 {
				left = append(left, "", theme.Title.Render("Node groups"), "  "+theme.Subtle.Render(truncate(noGroupsLine, leftW-4)))
			}
			section = f.section
			left = append(left, "", theme.Title.Render(section))
			if section == "Node groups" {
				left = append(left, theme.Fainted.Render("  group     instance type      nodes"))
				if m.settingsKarpenter() {
					left = append(left, theme.Fainted.Render(karpenterNodes))
				}
			}
			if section == "Sandbox classes" {
				left = append(left, theme.Fainted.Render(truncate("  m: minimal, Auto Mode with the Nitro sandbox only", leftW)))
			}
		}
		switch {
		case f.kind == fClass:
			note := classNote[f.key]
			if f.key == state.ClassNitro && m.nitroCompute() == plugin.NitroNodeGroup {
				note = nitroGroupNote
			}
			if m.classProblem(f.key) != "" {
				note = m.classShort(f.key)
			}
			left = append(left, " "+cell(i, 24)+" "+theme.Fainted.Render(truncate(note, leftW-27)))
		case f.group != "":
			note := ""
			switch f.group {
			case nodegroups.MicroVM:
				note = " nested virt"
			case nodegroups.Nitro:
				note = " Nitro Enclaves"
			}
			if k, ok := karpenterNotes[f.group]; ok && m.settingsKarpenter() && nodegroups.IsKarpenterPool(f.group, m.settingsNodeOverlay()) {
				note = k
			}
			if note != "" {
				note = theme.Fainted.Render(note)
			}
			left = append(left, "  "+pad(f.group, 8)+cell(i, 18)+" "+cell(i+1, 5)+note)
			i++
		default:
			left = append(left, "  "+pad(f.label, 18)+cell(i, leftW-20))
		}
		if msg, bad := st.errs[f.key]; bad {
			left = append(left, "  "+pad("", 18)+theme.Bad.Render(truncate(msg, leftW-20)))
		}
		if f.key == state.ClassNitro {
			if msg, bad := st.errs[classKey]; bad {
				left = append(left, "  "+theme.Bad.Render(truncate(msg, leftW-2)))
			}
			if st.nitro.on {
				left = append(left, "  "+theme.Fainted.Render(truncate(nitroSwitchLine, leftW-2)))
			}
		}
	}

	f := st.fields[st.cursor]
	right := []string{theme.Title.Render("About this setting")}
	text := help[f.key]
	if doc := m.d.Config.Defaults.Doc(f.key); doc != "" {
		text = doc
	}
	if f.key == "NITRO_AMI_ID" {
		text = help[f.key] + "\n" + nitroAMIBuild(st.values["KUBERNETES_VERSION"])
	}
	if f.key == "SUBSTRATE_TAG" && relBy != "" {
		text = "The install runs " + rel.Tag + " from " + rel.Repo + ": " + relBy + " installs that fork's release, one release per install. " +
			"SUBSTRATE_TAG (" + st.values["SUBSTRATE_TAG"] + ") is kept for a run without it."
	}
	para := lipgloss.NewStyle().Width(rightW)
	// First, as the class's help is long: what turning it on changes.
	if f.key == state.ClassNitro && m.nitroSwitches() && m.classProblem(f.key) == "" {
		right = append(right, theme.Fainted.Render(para.Render(nitroSwitchHint)))
	}
	right = append(right, theme.Subtle.Render(para.Render(joinParagraphs(text))))
	if f.kind == fText || f.kind == fToggle || f.kind == fNodeGroupType || f.kind == fNitroCompute {
		right = append(right, "", theme.Fainted.Render(para.Render("Now: "+f.key+"="+st.values[f.key])))
		if def, ok := m.d.Config.Defaults.Get(f.key); ok {
			right = append(right, theme.Fainted.Render(para.Render("config.env: "+f.key+"="+def)))
		}
	}
	if f.kind == fClass {
		right = append(right, "", theme.Fainted.Render(para.Render("Now: "+classKey+"="+st.values[classKey])))
		if why := m.classProblem(f.key); why != "" {
			right = append(right, theme.Warning.Render(para.Render("Not available: "+why+".")))
		}
	}
	if st.editing {
		right = append(right, "", theme.Subtle.Render("Typing: enter saves, esc keeps the old value."))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftW).Render(strings.Join(left, "\n")), "  ",
		lipgloss.NewStyle().Width(rightW).Render(strings.Join(right, "\n")))
}

// joinParagraphs makes a config.env comment block flow: its lines join into
// one paragraph, and an indented line (a command, a link) is dropped, since
// the panel is narrow.
func joinParagraphs(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, " ") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(strings.Fields(strings.Join(out, " ")), " ")
}

// pad cuts or pads s (which may hold color codes) to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}
