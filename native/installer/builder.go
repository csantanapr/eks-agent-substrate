package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// builder makes the long steps' commands, substrate-gke's snapshot.Builder.
// Every Spec is an argv: a Go program (setup-aws) or this installer
// re-executed with a subcommand. None is `bash -c`.
type builder struct {
	self, setupAWS, overlay, cwd, exeDir, treeRoot string
}

// findSetupAWS returns native/tools/setup-aws, searched above cwd and the
// executable, like substrate-gke finds its tools/setup-gcp.
func (b builder) findSetupAWS() string {
	if b.setupAWS != "" {
		return b.setupAWS
	}
	for _, start := range []string{b.cwd, b.exeDir} {
		for dir := start; dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			c := filepath.Join(dir, "tools", "setup-aws")
			if _, err := os.Stat(filepath.Join(c, "go.mod")); err == nil {
				return c
			}
		}
	}
	return filepath.Join("..", "tools", "setup-aws")
}

func (b builder) env(s *state.Setup) []string {
	return []string{"CLUSTER_NAME=" + s.ClusterName, "AWS_REGION=" + s.Region, "AWS_PROFILE=" + s.Profile}
}

// bootstrap runs `setup-aws bootstrap` with `go -C … run .`, as substrate-gke
// runs setup-gcp.
func (b builder) bootstrap(s *state.Setup) execx.Spec {
	dir := b.findSetupAWS()
	sim := []string{"level=INFO msg=\"Starting full bootstrap...\" cluster=" + s.ClusterName + " region=" + s.Region}
	phases := []string{"Checking the EKS cluster...", "Creating S3 buckets for snapshots and microVM assets...",
		"Creating EKS Pod Identity roles for ate-api-server and atelet...", "Creating ECR repositories..."}
	for i, p := range phases {
		sim = append(sim, fmt.Sprintf("level=INFO msg=\"Step %d/%d: %s\"", i+1, len(phases), p))
	}
	sim = append(sim, "level=INFO msg=\"Bootstrap completed successfully.\"")
	return execx.Spec{
		Label:    "setup-aws bootstrap",
		Display:  "go -C " + dir + " run . bootstrap",
		Argv:     []string{"go", "-C", dir, "run", ".", "bootstrap"},
		Env:      b.env(s),
		SimLines: sim,
	}
}

// controlPlane re-executes the installer with --deploy-control-plane.
func (b builder) controlPlane(s *state.Setup) execx.Spec {
	argv := []string{b.self, "--deploy-control-plane",
		"--cluster", s.ClusterName, "--region", s.Region, "--profile", s.Profile,
		"--tag", s.SubstrateTag, "--tree-root", b.treeRoot,
		"--snap-bucket", s.SnapBucket(), "--asset-bucket", s.AssetBucket()}
	if b.overlay != "" {
		argv = append(argv, "--eks-overlay", b.overlay)
	}
	return execx.Spec{
		Label:   "ate-setup deploy ate-system",
		Display: "go run ./cmd/ate-setup deploy ate-system  (substrate " + s.SubstrateTag + " + deploy/eks)",
		Argv:    argv,
		SimLines: []string{
			controlplane.CachedLine + s.SubstrateTag,
			eksoverlay.LinePrefix + "added deploy/eks to " + eksoverlay.BaseKustomization,
			eksoverlay.LinePrefix + "applied code patch ate-setup-s3-assets-v1.0.0.patch",
			controlplane.KubeconfLine + "~/.cache/substrate-eks/kubeconfig/" + s.ClusterName + "." + s.Region + ".yaml",
			controlplane.PrereqLine + "native/deploy/eks/prereqs",
			"storageclass.storage.k8s.io/substrate-eks-auto-ebs created",
			"[step]: deploy_ate_system", "[step]: prepare_microvm_deps",
			"Uploading assets to s3://" + s.ClusterName + "-assets-…/microvm-assets/ ...",
			"[step]: deploy_crds", "[step]: ensure_apiserver_prerequisites",
			"[step]: Waiting for ATE system components to be ready...",
			`deployment "ate-api-server" successfully rolled out`,
			`daemon set "atelet" successfully rolled out`,
			controlplane.ReadyLine,
		},
	}
}
