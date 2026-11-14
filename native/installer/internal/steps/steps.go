// Package steps defines the live checklists for the long-running install
// commands, as substrate-gke's installer/internal/steps does: each item names
// a phase and matches the output line that marks the phase starting; when a
// later item matches, earlier items are complete. ChecklistItem and Progress
// are substrate-gke's, unchanged.
package steps

import (
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
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

// Cluster tracks `setup-aws create cluster`.
func Cluster() []ChecklistItem { return FromPhases(phases.Cluster) }

// RDS tracks `setup-aws create rds`.
func RDS() []ChecklistItem { return FromPhases(phases.RDS) }

// Teardown tracks `setup-aws teardown`.
func Teardown() []ChecklistItem { return FromPhases(phases.Teardown) }

// RemoveControlPlane tracks --remove-control-plane (controlplane.Delete). Its
// only marker is the last line, so the first row lights on any output: the
// step is under way, and the spinner says so.
func RemoveControlPlane() []ChecklistItem {
	return []ChecklistItem{
		{"Delete ate-system with ate-setup", func(string) bool { return true }},
		{"Wait for the ate-system namespace to be gone", contains(controlplane.RemovedLine)},
	}
}

// Demo tracks --deploy-demo --verify: the counter demo deployed on each
// class, then suspend and resume checked on each (demo.Deploy, demo.Verify).
func Demo(classes []string) []ChecklistItem {
	var items []ChecklistItem
	for _, c := range classes {
		items = append(items, ChecklistItem{"Deploy the counter demo on " + c, contains(demo.DeployLine + c)})
	}
	for _, c := range classes {
		items = append(items, ChecklistItem{"Check suspend and resume on " + c, contains(demo.VerifyLine + c)})
	}
	return items
}

// ControlPlane tracks the in-process deploy and `ate-setup deploy
// ate-system`'s `[step]:` markers, the same markers substrate-gke's Deploy
// checklist keys off.
func ControlPlane() []ChecklistItem {
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
		{"Stage microVM assets in S3", containsAny("prepare_microvm_deps", "Uploading assets to s3://")},
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
