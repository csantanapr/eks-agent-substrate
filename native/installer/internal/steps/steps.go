// Package steps defines the live checklists for the long-running install
// commands, as substrate-gke's installer/internal/steps does: each item names
// a phase and matches the output line that marks the phase starting; when a
// later item matches, earlier items are complete. ChecklistItem and Progress
// are substrate-gke's, unchanged.
package steps

import (
	"slices"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// ChecklistItem is one row of a step's progress checklist.
type ChecklistItem struct {
	Label string
	// Match reports whether an output line marks this phase as started.
	Match func(line string) bool
}

func contains(sub string) func(string) bool {
	return func(line string) bool { return strings.Contains(line, sub) }
}

// exactly matches the whole line, so plugin "efs" is not lit by "efs-x".
func exactly(want string) func(string) bool {
	return func(line string) bool { return strings.TrimSpace(line) == want }
}

func containsAny(subs ...string) func(string) bool {
	return func(line string) bool {
		for _, s := range subs {
			if strings.Contains(line, s) {
				return true
			}
		}
		return false
	}
}

// FromPhases is the checklist of a setup-aws command: one row per phase in
// the shared contract (setup-aws/phases), lit by that phase's exact
// "Step i/N: <title>" line. The row label is the title without its "...".
func FromPhases(list []string) []ChecklistItem {
	items := make([]ChecklistItem, len(list))
	for i, title := range list {
		items[i] = ChecklistItem{strings.TrimSuffix(title, "..."), contains(phases.Line(list, i))}
	}
	return items
}

// Bootstrap tracks `setup-aws bootstrap`.
func Bootstrap() []ChecklistItem { return FromPhases(phases.Bootstrap) }

// Cluster tracks `setup-aws create cluster` for a node group type
// (NODE_GROUP_TYPE), whose node group rows differ, and KARPENTER=on
// (karpenter), which adds Karpenter's two rows at the end.
func Cluster(nodeGroupType string, karpenter bool) []ChecklistItem {
	return FromPhases(phases.ClusterFor(nodeGroupType, karpenter))
}

// RDS tracks `setup-aws create rds`.
func RDS() []ChecklistItem { return FromPhases(phases.RDS) }

// Teardown tracks `setup-aws teardown`.
func Teardown() []ChecklistItem { return FromPhases(phases.Teardown) }

// RemoveControlPlane tracks --remove-control-plane (controlplane.Delete),
// which removes every plugin the cluster records, then runs ate-setup
// delete, then waits for ate-system to go. recorded is the record, when the
// caller has read it (read, as the reinstall guard's probe does): one row per
// plugin, in the order they are removed, each lit by its own "Removing plugin"
// line, and no plugin row when the record lists none. Unread, or naming a
// plugin this installer does not know, one row stands for them all, lit by
// the line Delete prints as their removal starts.
func RemoveControlPlane(recorded []string, read bool) []ChecklistItem {
	var items []ChecklistItem
	if ps, known := RemovalOrder(recorded); read && known {
		for _, p := range ps {
			items = append(items, ChecklistItem{"Remove " + p.Title, exactly(plugin.RemoveLine + p.ID)})
		}
	} else {
		items = append(items, ChecklistItem{"Remove the recorded plugins", contains(controlplane.RemovePluginsLine)})
	}
	return append(items,
		ChecklistItem{"Delete ate-system with ate-setup", contains(controlplane.DeleteLine)},
		ChecklistItem{"Wait for the ate-system namespace to be gone", contains(controlplane.RemovedLine)})
}

// RemovalOrder is the plugins ids name in the order plugin.Run removes them,
// reverse install order (the reverse of plugin.All), and false when one is
// not registered: Run then refuses the removal before removing any.
func RemovalOrder(ids []string) ([]plugin.Plugin, bool) {
	for _, id := range ids {
		if _, ok := plugin.Get(id); !ok {
			return nil, false
		}
	}
	var out []plugin.Plugin
	all := plugin.All()
	for i := len(all) - 1; i >= 0; i-- {
		if slices.Contains(ids, all[i].ID) {
			out = append(out, all[i])
		}
	}
	return out, true
}

// Demo tracks --deploy-demo --verify: the counter demo deployed on each of
// deployed (the gvisor and microvm classes), then suspend and resume checked
// on each of checked, every sandbox class (demo.Deploy, demo.Verify). A
// checked class this step does not deploy is the AWS Nitro sandbox: its
// counter (counter-iso) came with its plugin, on the Set up Nitro sandboxes
// step, and its check is that plugin's, so its row names that step.
func Demo(deployed, checked []string) []ChecklistItem {
	var items []ChecklistItem
	for _, c := range deployed {
		items = append(items, ChecklistItem{"Deploy the counter demo on " + c, contains(demo.DeployLine + c)})
	}
	for _, c := range checked {
		label := "Check suspend and resume on " + c
		if !slices.Contains(deployed, c) {
			label += " (deployed by " + state.NitroSandbox.Title() + ")"
		}
		items = append(items, ChecklistItem{label, contains(demo.VerifyLine + c)})
	}
	return items
}

// ControlPlane tracks the in-process deploy and `ate-setup deploy
// ate-system`'s `[step]:` markers, the same markers substrate-gke's Deploy
// checklist keys off. The microVM assets row is there only when ate-setup
// stages them (microvm: its microvm class), so a run without it never shows
// the row ticked.
func ControlPlane(microvm bool) []ChecklistItem {
	items := controlPlane()
	if !microvm {
		items = slices.DeleteFunc(items, func(i ChecklistItem) bool { return i.Label == stageMicroVMLabel })
	}
	return items
}

const stageMicroVMLabel = "Stage microVM assets in S3"

func controlPlane() []ChecklistItem {
	return []ChecklistItem{
		// The kubeconfig first: Deploy reads the plugin record off the
		// cluster before it fetches the release (a plugin may replace it).
		{"Write the installer's kubeconfig", contains(controlplane.KubeconfLine)},
		{"Fetch the pinned Substrate release", containsAny(controlplane.FetchLine, controlplane.CachedLine)},
		{"Add the EKS manifests (deploy/eks)", contains(eksoverlay.LinePrefix)},
		// One row for whichever database path runs: the StorageClass the
		// bundled PostgreSQL uses (also a default class with RDS on a cluster
		// the installer made), then the RDS bootstrap.
		{"Prepare the database", containsAny(controlplane.DatabaseLine, controlplane.PrereqLine)},
		// ate-setup v1.0.0 stages the microVM assets first, before any
		// manifest is applied (deploy_ate_system -> prepare_microvm_deps ->
		// deploy_crds), so this row comes before CRDs: a later row that
		// matched first would tick the rows between as done.
		{stageMicroVMLabel, containsAny("prepare_microvm_deps", "Uploading assets to s3://")},
		{"Apply namespace, CRDs, and RBAC", contains("deploy_crds")},
		{"Generate CAs, JWT pools, and API server config", contains("ensure_apiserver_prerequisites")},
		{"Wait for components: postgres, api-server, controller, atenet, atelet", contains("Waiting for ATE system components")},
	}
}

// Plugins tracks --plugin-action deploy (plugin.Run): two rows per plugin, in
// install order, lit by its deploy and check lines.
func Plugins(ps []plugin.Plugin) []ChecklistItem {
	var items []ChecklistItem
	for _, p := range ps {
		items = append(items,
			ChecklistItem{"Deploy " + p.Title, exactly(plugin.DeployLine + p.ID)},
			ChecklistItem{"Check " + p.Title, exactly(plugin.VerifyLine + p.ID)})
	}
	return items
}

// Progress applies one output line to a checklist, returning the updated
// index of the active item (-1 when nothing has matched yet). Matches only
// ever move the cursor forward.
func Progress(items []ChecklistItem, active int, line string) int {
	for i := len(items) - 1; i > active; i-- {
		if items[i].Match(line) {
			return i
		}
	}
	return active
}
