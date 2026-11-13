// Package steps defines the live checklists for the long-running install
// commands, as substrate-gke's installer/internal/steps does: each item names
// a phase and matches the output line that marks the phase starting; when a
// later item matches, earlier items are complete. ChecklistItem and Progress
// are substrate-gke's, unchanged.
package steps

import (
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
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

// Bootstrap tracks `setup-aws bootstrap`, keyed off its "Step i/N:" lines
// (setup-aws cmd.Phases).
func Bootstrap() []ChecklistItem {
	return []ChecklistItem{
		{"Check the EKS cluster", contains("Checking the EKS cluster")},
		{"Create the S3 snapshot and asset buckets", contains("Creating S3 buckets")},
		{"Create EKS Pod Identity roles", contains("Creating EKS Pod Identity roles")},
		{"Create ECR repositories", contains("Creating ECR repositories")},
	}
}

// ControlPlane tracks the in-process deploy and `ate-setup deploy
// ate-system`'s `[step]:` markers, the same markers substrate-gke's Deploy
// checklist keys off.
func ControlPlane() []ChecklistItem {
	return []ChecklistItem{
		{"Fetch the pinned Substrate release", containsAny(controlplane.FetchLine, controlplane.CachedLine)},
		{"Add the EKS manifests (deploy/eks)", contains(eksoverlay.LinePrefix)},
		{"Write the installer's kubeconfig", contains(controlplane.KubeconfLine)},
		{"Apply cluster prerequisites (Auto Mode StorageClass)", contains(controlplane.PrereqLine)},
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
