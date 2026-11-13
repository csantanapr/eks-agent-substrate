package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// builder makes the long steps' commands, substrate-gke's snapshot.Builder.
// Every Spec is an argv: a Go program (setup-aws) or this installer
// re-executed with a subcommand. None is `bash -c`.
//
// Display is the same command written so it can be pasted from cwd: the
// paths relative to it when they are below it, and the environment setup-aws
// reads written in front (an empty value is left out: it means the default).
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

// rel is p relative to cwd when it lies below it, for Display.
func (b builder) rel(p string) string {
	if b.cwd == "" || !filepath.IsAbs(p) {
		return p
	}
	r, err := filepath.Rel(b.cwd, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return p
	}
	if filepath.Base(r) == r {
		return "./" + r
	}
	return r
}

// env is what every setup-aws child gets on top of the inherited
// environment. setup-aws reads its flag defaults from the environment, so
// the wizard pins each knob that would change what a step does: an exported
// PLAN=true would otherwise turn every step into a plan that reports success
// and creates nothing, and KEEP_CLUSTER would override the teardown choice
// (review F4). teardown sets KEEP_CLUSTER from s.KeepCluster itself.
func (b builder) env(s *state.Setup) []string {
	return []string{"CLUSTER_NAME=" + s.ClusterName, "AWS_REGION=" + s.Region, "AWS_PROFILE=" + s.Profile, "TAGS=" + s.Tags,
		"PLAN=false", "KEEP_CLUSTER=" + strconv.FormatBool(s.KeepCluster)}
}

// setupAWS is `go -C <setup-aws> run . <args>`, as substrate-gke runs
// setup-gcp, with sim replayed under --dry-run.
func (b builder) setupAWSSpec(s *state.Setup, label string, env []string, sim []string, args ...string) execx.Spec {
	dir := b.findSetupAWS()
	argv := append([]string{"go", "-C", dir, "run", "."}, args...)
	var shown []string
	for _, e := range b.env(s) {
		if k, v, _ := strings.Cut(e, "="); v != "" && k != "TAGS" && k != "PLAN" && k != "KEEP_CLUSTER" {
			shown = append(shown, k+"="+shellQuote(v))
		}
	}
	shown = append(shown, "go", "-C", b.rel(dir), "run", ".")
	return execx.Spec{
		Label:    label,
		Display:  strings.Join(append(shown, args...), " "),
		Argv:     argv,
		Env:      append(b.env(s), env...),
		SimLines: sim,
	}
}

// simPhases is the dry-run replay of a setup-aws command: its phases'
// "Step i/N" lines from the shared contract, framed by first and last.
func simPhases(list []string, first, last string) []string {
	sim := []string{first}
	for i := range list {
		sim = append(sim, fmt.Sprintf("level=INFO msg=%q", phases.Line(list, i)))
	}
	return append(sim, last)
}

func simStart(s *state.Setup, what string) string {
	return fmt.Sprintf("level=INFO msg=%q cluster=%s region=%s", what, s.ClusterName, s.Region)
}

// createCluster runs `setup-aws create cluster` with config.env's knobs.
func (b builder) createCluster(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws create cluster", s.NewClusterConfig.Env(),
		simPhases(phases.Cluster, simStart(s, "Creating the cluster..."),
			`level=INFO msg="Cluster ready." cluster=`+s.ClusterName+` created=27 changed=0 took=19m40s`),
		"create", "cluster")
}

// bootstrap runs `setup-aws bootstrap`.
func (b builder) bootstrap(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws bootstrap", nil,
		simPhases(phases.Bootstrap, simStart(s, "Starting full bootstrap..."), `level=INFO msg="Bootstrap completed successfully."`),
		"bootstrap")
}

// rds runs `setup-aws create rds`.
func (b builder) rds(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws create rds", nil,
		simPhases(phases.RDS, simStart(s, "Starting RDS setup..."), `level=INFO msg="RDS setup completed successfully."`),
		"create", "rds")
}

// teardown runs `setup-aws teardown`, with --keep-cluster when s.KeepCluster.
func (b builder) teardown(s *state.Setup) execx.Spec {
	args := []string{"teardown"}
	if s.KeepCluster {
		args = append(args, "--keep-cluster")
	}
	return b.setupAWSSpec(s, "setup-aws "+strings.Join(args, " "), nil,
		simPhases(phases.Teardown, `level=INFO msg="Starting teardown of `+s.ClusterName+"."+s.Region+`..."`,
			`level=INFO msg="Teardown complete: nothing left for `+s.ClusterName+"."+s.Region+`"`),
		args...)
}

// inventory is `setup-aws inventory`, which the teardown's Complete screen
// offers to show that nothing with the owner tag is left.
func (b builder) inventory(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws inventory", nil, nil, "inventory")
}

// selfSpec re-executes the installer with a subcommand flag for s's cluster.
// --tree-root and --eks-overlay are passed only when they are not the
// default, so the command shown is the one run and short enough to paste.
func (b builder) selfSpec(s *state.Setup, cluster, label string, sim []string, flags ...string) execx.Spec {
	// --profile always, even empty: the child's flag defaults to an exported
	// AWS_PROFILE, so leaving it out would run it as another identity than
	// the setup-aws steps beside it (review F9).
	args := append(append([]string{}, flags...), "--cluster", cluster, "--region", s.Region, "--profile", s.Profile)
	if b.treeRoot != controlplane.DefaultTreeRoot() {
		args = append(args, "--tree-root", b.treeRoot)
	}
	if b.overlay != "" {
		args = append(args, "--eks-overlay", b.overlay)
	}
	shown := []string{b.rel(b.self)}
	for _, a := range args {
		shown = append(shown, shellQuote(a))
	}
	return execx.Spec{
		Label:    label,
		Display:  strings.Join(shown, " "),
		Argv:     append([]string{b.self}, args...),
		SimLines: sim,
	}
}

// controlPlane re-executes the installer with --deploy-control-plane.
func (b builder) controlPlane(s *state.Setup) execx.Spec {
	sim := []string{
		controlplane.CachedLine + s.SubstrateTag,
		eksoverlay.LinePrefix + "added deploy/eks to " + eksoverlay.BaseKustomization,
		eksoverlay.LinePrefix + "applied code patch ate-setup-s3-assets-v1.0.0.patch",
		controlplane.KubeconfLine + "~/.cache/substrate-eks/kubeconfig/" + s.ClusterName + "." + s.Region + ".yaml",
	}
	// The StorageClass prerequisite, as applyStorageClass prints it: always
	// for the bundled database, and on a cluster this installer created.
	if s.Database == state.DatabaseBundled || s.ClusterOwned {
		sc, prov := controlplane.StorageClassName(s.AutoMode), "ebs.csi.aws.com"
		if s.AutoMode {
			prov = "ebs.csi.eks.amazonaws.com"
		}
		sim = append(sim, controlplane.PrereqLine+"StorageClass "+sc+" ("+prov+", encrypted gp3, 3 volume tags)",
			"storageclass.storage.k8s.io/"+sc+" created")
	}
	sim = append(sim,
		"[step]: deploy_ate_system", "[step]: prepare_microvm_deps",
		"Uploading assets to s3://"+s.AssetBucket()+"/microvm-assets/ ...",
		"[step]: deploy_crds", "[step]: ensure_apiserver_prerequisites",
		"[step]: Waiting for ATE system components to be ready...",
		`deployment "ate-api-server" successfully rolled out`,
		`daemon set "atelet" successfully rolled out`,
		controlplane.ReadyLine,
	)
	spec := b.selfSpec(s, s.ClusterName, "ate-setup deploy ate-system", sim,
		"--deploy-control-plane", "--database", s.Database, "--tag", s.SubstrateTag,
		"--snap-bucket", s.SnapBucket(), "--asset-bucket", s.AssetBucket())
	spec.Display = "go run ./cmd/ate-setup deploy ate-system  (substrate " + s.SubstrateTag + " + deploy/eks, database " + s.Database + ")"
	spec.DisplayIsSummary = true
	return spec
}

// demoStep re-executes the installer with --deploy-demo --verify: the counter
// demo on every sandbox class, then the suspend/resume check on each.
func (b builder) demoStep(s *state.Setup) execx.Spec {
	var sim []string
	classes := strings.Split(s.SandboxClasses, ",")
	for _, c := range classes {
		sim = append(sim, demo.DeployLine+c, "actor counter-"+c+" is Running")
	}
	for _, c := range classes {
		sim = append(sim, demo.VerifyLine+c, "suspended counter-"+c+"; snapshot in s3://"+s.SnapBucket()+"/",
			demo.VerifyPassLine+c+": memory, file and rootfs counters continued after resume")
	}
	return b.selfSpec(s, s.ClusterName, "counter demo", sim, "--deploy-demo", "--verify", "--tag", s.SubstrateTag)
}

// verify is `substrate-eks --verify`, the user-facing check (AC7.2), shown
// on the Complete screen of a demo run.
func (b builder) verify(s *state.Setup) execx.Spec {
	// --tag, as demoStep and removeControlPlane pass it: verify reads the
	// tree of the release that was installed (review F11).
	return b.selfSpec(s, s.ClusterName, "verify", nil, "--verify", "--tag", s.SubstrateTag)
}

// removeControlPlane re-executes the installer with --remove-control-plane
// for cluster: the guard runs it on a found install, the teardown flow on
// the run's own.
func (b builder) removeControlPlane(s *state.Setup) execx.Spec {
	return b.selfSpec(s, s.ClusterName, "ate-setup delete ate-system", []string{
		controlplane.CachedLine + s.SubstrateTag,
		"$ go run ./cmd/ate-setup delete ate-system --context " + s.ClusterName,
		`namespace "ate-system" deleted`,
		controlplane.RemovedLine,
	}, "--remove-control-plane", "--tag", s.SubstrateTag)
}

// probe re-executes the installer with --probe-installed for cluster.
func (b builder) probe(cluster string, s *state.Setup) execx.Spec {
	return b.selfSpec(s, cluster, "check "+cluster+" for an existing install", []string{probe.SimLine(cluster)}, "--probe-installed")
}

// shellQuote quotes s for a POSIX shell when it needs it, so a Display line
// pastes as the argv it shows.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,@+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
