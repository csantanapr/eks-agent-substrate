// Package controlplane installs the Substrate control plane with upstream's
// ate-setup, the step substrate-gke's snapshot.Builder.DeployAteSystem builds.
//
// substrate-gke composes that step as a `bash -c` line (fetch the tree, add
// the overlay, export the kubeconfig, run ate-setup). Here each part is a Go
// function, and the installer runs them by re-executing itself
// (--deploy-control-plane), so the step has no shell text at all: every child
// process is started from an argv.
package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
)

// Lines the deploy prints for the progress checklist (steps.ControlPlane).
const (
	FetchLine    = "Fetching substrate "
	CachedLine   = "Using cached substrate "
	KubeconfLine = "Writing kubeconfig "
	PrereqLine   = "Applying cluster prerequisites from "
	BundledLine  = "Using the bundled PostgreSQL; nothing to prepare on a standard cluster"
	ReadyLine    = "Substrate control plane is ready"
)

// DefaultRepo is upstream's repository.
const DefaultRepo = "https://github.com/agent-substrate/substrate.git"

// completeMarker records a finished fetch, as substrate-gke's CompleteMarker.
const completeMarker = ".substrate-eks-fetched"

// Options are the install answers.
type Options struct {
	Cluster, Region, Profile string
	Tag, Repo, ImageRepo     string
	SnapBucket, AssetBucket  string
	SandboxClasses           string
	// AutoMode, read off the cluster by Deploy, pins the control plane to
	// the system node group (--cordon-system-infra-components) with RDS, and
	// puts the bundled PostgreSQL volume on Auto Mode's EBS driver
	// (deploy/eks/prereqs/storageclass-auto-ebs.yaml).
	AutoMode bool
	// Database is DatabaseRDS (the default, also when empty) or
	// DatabaseBundled; main.go sets it from --database.
	Database string
	// TreeRoot is where the pinned tree is cached; Overlay is deploy/eks.
	TreeRoot, Overlay string
	// IngressIDs are the SPIFFE IDs the ingress router admits.
	IngressIDs []string

	// caFile is the RDS CA bundle Deploy downloaded, for ate-setup.
	caFile string
}

// db is Database with the default applied.
func (o Options) db() string {
	if o.Database == "" {
		return DatabaseRDS
	}
	return o.Database
}

// TreeDir is the cached tree for a tag.
func TreeDir(root, tag string) string { return filepath.Join(root, "substrate-"+tag) }

// DefaultTreeRoot is under the user cache dir, like substrate-gke's managed tree.
func DefaultTreeRoot() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "substrate-eks", "trees")
}

// command is one child process: an argv, never shell text. A secret reaches
// a child only through Env or Stdin.
type command struct {
	Dir   string
	Env   []string
	Stdin []byte
	// Stdout, when set, takes stdout instead of the deploy's writer.
	Stdout io.Writer
	Argv   []string
}

// runCommand starts every child process the deploy makes. Tests replace it
// to see each argv, environment and stdin.
var runCommand = func(ctx context.Context, w io.Writer, c command) error {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = c.Dir, w, w
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	cmd.Env = append(os.Environ(), c.Env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(c.Argv[:min(len(c.Argv), 3)], " "), err)
	}
	return nil
}

func run(ctx context.Context, w io.Writer, dir string, env []string, argv ...string) error {
	return runCommand(ctx, w, command{Dir: dir, Env: env, Argv: argv})
}

// FetchTree clones repo at tag into dir unless a finished fetch is there. It
// clones into a staging directory and renames it, so an interrupted fetch is
// never mistaken for a finished one.
func FetchTree(ctx context.Context, w io.Writer, dir, repo, tag string) error {
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err == nil {
		fmt.Fprintln(w, CachedLine+tag+" at "+dir)
		return nil
	}
	fmt.Fprintln(w, FetchLine+tag+" from "+repo)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := run(ctx, w, "", nil, "git", "clone", "--quiet", "--depth", "1", "--branch", tag, repo, stage); err != nil {
		return err
	}
	// Keep the marker out of git status, so builds are not stamped -dirty.
	if f, err := os.OpenFile(filepath.Join(stage, ".git", "info", "exclude"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
		fmt.Fprintln(f, "/"+completeMarker)
		f.Close()
	}
	if err := os.WriteFile(filepath.Join(stage, completeMarker), []byte(tag+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(stage, dir)
}

// AteSetupArgs are the `ate-setup deploy ate-system` flags for EKS, the same
// ones eks-talk's substrate/install.sh passes. The connection strings are not
// among them: they carry passwords, so they go in the environment.
func AteSetupArgs(o Options, kubeContext string) []string {
	ids := o.IngressIDs
	if len(ids) == 0 {
		ids = []string{"spiffe://cluster.local/ns/ate-system/sa/atenet-client"}
	}
	var extra []string
	switch {
	case o.db() == DatabaseRDS:
		extra = append(extra, "--ateapi-postgres-server-ca-file", o.caFile)
		// The Auto Mode system node group is tainted, so other pods go to
		// Auto Mode nodes; this pins every control plane Deployment there,
		// with the matching toleration (install.sh, AUTO_MODE=on). Not with
		// the bundled PostgreSQL: ate-setup then also needs a dedicated
		// ate.dev/workloadType=ate-postgres pool, which no cluster here has.
		if o.AutoMode {
			extra = append(extra, "--cordon-system-infra-components")
		}
	case o.AutoMode:
		extra = append(extra, "--ateapi-postgres-storage-class", AutoModeStorageClass)
	}
	return append([]string{
		"run", "./cmd/ate-setup", "deploy", "ate-system",
		"--context", kubeContext,
		"--image-repo", o.ImageRepo, "--image-tag", o.Tag,
		"--storage-bucket-name", o.SnapBucket,
		"--snapshot-location", "s3://" + o.SnapBucket + "/",
		"--credential-provider", `{"name":"k8s.io"}`,
		"--ingress-allowed-spiffe-ids", strings.Join(ids, ","),
		"--sandbox-classes", o.SandboxClasses,
		"--rollout-timeout", "5m",
	}, extra...)
}

// Database choices for Options.Database.
const (
	DatabaseRDS     = "rds"
	DatabaseBundled = "bundled"
)

// AutoModeStorageClass is the StorageClass in deploy/eks/prereqs.
const AutoModeStorageClass = "substrate-eks-auto-ebs"

// AteSetupEnv is the environment of the ate-setup run. It holds names only,
// never a secret; with RDS, Deploy adds the connection strings to it.
func AteSetupEnv(o Options, kubeconfig string) []string {
	return []string{
		"KUBECONFIG=" + kubeconfig,
		"AWS_REGION=" + o.Region,
		"AWS_PROFILE=" + o.Profile,
		// The EKS code patch (deploy/eks/patches): stage microVM assets in S3.
		"ATE_INSTALL_STORAGE_BACKEND=s3",
		"ATE_ASSET_BUCKET=" + o.AssetBucket,
		"NO_DEV_ENV=1",
		// ate-setup would otherwise prefer an exported context over --context.
		"KUBECTL_CONTEXT=",
		// Carlos's build hosts sinkhole proxy.golang.org.
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=vendor",
	}
}

// Deploy fetches the tree, adds deploy/eks, writes the installer's own
// kubeconfig and runs ate-setup, on RDS (bootstrapped first) or the bundled
// PostgreSQL.
func Deploy(ctx context.Context, w io.Writer, o Options) error {
	if o.Cluster == "" || o.Region == "" || o.SnapBucket == "" || o.Overlay == "" {
		return errors.New("cluster, region, snapshot bucket and overlay are required")
	}
	if db := o.db(); db != DatabaseRDS && db != DatabaseBundled {
		return fmt.Errorf("database %q is not %s or %s", db, DatabaseRDS, DatabaseBundled)
	}
	tree := TreeDir(o.TreeRoot, o.Tag)
	if err := FetchTree(ctx, w, tree, o.Repo, o.Tag); err != nil {
		return err
	}
	if err := eksoverlay.Apply(w, tree, o.Overlay, o.Region); err != nil {
		return err
	}
	c, err := awsx.New(ctx, o.Profile, o.Region)
	if err != nil {
		return err
	}
	cl, err := c.Describe(ctx, o.Cluster)
	if err != nil {
		return err
	}
	// Read Auto Mode off the cluster itself rather than trusting a flag, so a
	// direct --deploy-control-plane run cannot pick the wrong StorageClass or
	// cordon setting.
	o.AutoMode = cl.AutoMode
	kc := awsx.KubeconfigPath(o.Cluster, o.Region)
	fmt.Fprintln(w, KubeconfLine+kc)
	if err := awsx.WriteKubeconfig(kc, cl, o.Region, o.Profile); err != nil {
		return err
	}
	return install(ctx, w, o, cl.Name, kc, tree)
}

// install is the deploy after the kubeconfig: the database, then ate-setup.
func install(ctx context.Context, w io.Writer, o Options, kubeContext, kc, tree string) error {
	env := AteSetupEnv(o, kc)
	switch o.db() {
	case DatabaseRDS:
		dsnEnv, cleanup, err := prepareRDS(ctx, w, &o, kc, tree)
		defer cleanup()
		if err != nil {
			return err
		}
		env = append(env, dsnEnv...)
	case DatabaseBundled:
		if !o.AutoMode {
			// Printed so the checklist's database row lights on what ran.
			fmt.Fprintln(w, BundledLine)
			break
		}
		prereqs := filepath.Join(o.Overlay, "prereqs")
		fmt.Fprintln(w, PrereqLine+prereqs)
		if err := run(ctx, w, "", []string{"KUBECONFIG=" + kc}, "kubectl", "apply", "-f", prereqs); err != nil {
			return err
		}
	}
	argv := append([]string{"go"}, AteSetupArgs(o, kubeContext)...)
	fmt.Fprintln(w, "$ "+strings.Join(argv, " "))
	if err := run(ctx, w, tree, env, argv...); err != nil {
		return err
	}
	fmt.Fprintln(w, ReadyLine)
	return nil
}
