// The substrate-eks installer: an interactive wizard that creates or connects
// an EKS cluster, provisions the AWS resources Substrate uses (setup-aws),
// and installs the Agent Substrate control plane with upstream's ate-setup,
// from a pinned release tree it fetches on demand; and the teardown that
// removes all of it. It follows substrate-gke's installer architecture
// package for package; see native/README.md for the mapping.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/ui"
)

func main() {
	var (
		doctorMode = flag.Bool("doctor", false, "run the preflight checks and exit")
		dryRun     = flag.Bool("dry-run", false, "walk the full wizard without touching AWS (simulated commands)")
		profile    = flag.String("profile", os.Getenv("AWS_PROFILE"), "AWS shared config profile (AWS_PROFILE)")
		region     = flag.String("region", os.Getenv("AWS_REGION"), "AWS Region (AWS_REGION)")
		cluster    = flag.String("cluster", os.Getenv("CLUSTER_NAME"), "preselect this cluster, or the name of a new one (CLUSTER_NAME)")
		tag        = flag.String("tag", "v1.0.0", "Substrate release to install")
		database   = flag.String("database", controlplane.DatabaseRDS, "the control plane's database: rds (Amazon RDS for PostgreSQL) or bundled (in-cluster PostgreSQL)")
		demoMode   = flag.Bool("demo", false, "end the install with the counter demo on gVisor and microVM, and check it")
		teardown   = flag.Bool("teardown", false, "open the wizard on the teardown of --cluster: remove Substrate, then its AWS resources")
		keepClust  = flag.Bool("keep-cluster", false, "with --teardown: remove Substrate and its AWS data but never offer to delete the cluster")
		setupAWS   = flag.String("setup-aws", "", "path to native/tools/setup-aws (default: found above the working directory or the executable)")
		overlay    = flag.String("eks-overlay", "", "path to native/deploy/eks (default: found above the working directory or the executable)")
		treeRoot   = flag.String("tree-root", controlplane.DefaultTreeRoot(), "where fetched Substrate trees are cached")
		logPath    = flag.String("log", "", "log file (default: a timestamped file under the user cache dir)")

		// Subcommands the wizard runs by re-executing itself, as substrate-gke's
		// --add-gke-overlay: each is a Go function, so no step is a shell script.
		addOverlay = flag.String("add-eks-overlay", "", "add deploy/eks to the substrate tree in this directory and exit (needs --region)")
		deployCP   = flag.Bool("deploy-control-plane", false, "fetch the tree, add deploy/eks and run ate-setup deploy ate-system, then exit")
		snapBucket = flag.String("snap-bucket", "", "with --deploy-control-plane: the snapshot bucket")
		assetBkt   = flag.String("asset-bucket", "", "with --deploy-control-plane: the microVM asset bucket")
		removeCP   = flag.Bool("remove-control-plane", false, "remove Substrate from --cluster with ate-setup delete ate-system, keeping the cluster, then exit")
		deployDemo = flag.Bool("deploy-demo", false, "deploy the counter demo on --cluster, then exit (with --verify: check it after)")
		verify     = flag.Bool("verify", false, "check that suspend and resume continue the counter demo on every sandbox class, then exit")
		probeInst  = flag.Bool("probe-installed", false, "print whether --cluster already runs Substrate (the wizard's reinstall guard), then exit")
	)
	flag.Parse()

	if *database != controlplane.DatabaseRDS && *database != controlplane.DatabaseBundled {
		exitOn(fmt.Errorf("--database is %q; want %s or %s", *database, controlplane.DatabaseRDS, controlplane.DatabaseBundled))
	}

	cwd, _ := os.Getwd()
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	findOverlay := func() string {
		dir, err := eksoverlay.Find(*overlay, cwd, exeDir)
		exitOn(err)
		return dir
	}
	cpOptions := func() controlplane.Options {
		return controlplane.Options{
			Cluster: *cluster, Region: *region, Profile: *profile,
			Tag: *tag, Repo: controlplane.DefaultRepo, ImageRepo: "ghcr.io/agent-substrate/substrate",
			SnapBucket: *snapBucket, AssetBucket: *assetBkt, SandboxClasses: "gvisor,microvm",
			Database: *database, TreeRoot: *treeRoot, Overlay: findOverlay(),
		}
	}
	demoOptions := demo.Options{
		Cluster: *cluster, Region: *region, Profile: *profile, Tag: *tag, TreeRoot: *treeRoot,
		ImageRepo: "ghcr.io/agent-substrate/substrate", SandboxClasses: "gvisor,microvm",
	}

	switch {
	case *addOverlay != "":
		exitOn(eksoverlay.Apply(os.Stdout, absPath(*addOverlay), findOverlay(), *region))
		return
	case *deployCP:
		exitOn(controlplane.Deploy(context.Background(), os.Stdout, cpOptions()))
		return
	case *removeCP:
		exitOn(controlplane.Delete(context.Background(), os.Stdout, cpOptions()))
		return
	case *deployDemo || *verify:
		if *deployDemo {
			exitOn(demo.Deploy(context.Background(), os.Stdout, demoOptions))
		}
		if *verify {
			exitOn(demo.Verify(context.Background(), os.Stdout, demoOptions))
		}
		return
	case *probeInst:
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		exitOn(probe.Run(ctx, os.Stdout, *profile, *region, *cluster))
		return
	case *doctorMode:
		fmt.Println("substrate-eks preflight doctor")
		fmt.Println()
		if n := doctor.RunCLI(context.Background(), doctor.Checks(*profile, *region)); n > 0 {
			fmt.Printf("\n%d fatal problem(s) found.\n", n)
			os.Exit(1)
		}
		fmt.Println("\nAll good — run the installer.")
		return
	}

	if *region == "" && !*dryRun {
		exitOn(fmt.Errorf("--region (AWS_REGION) is required"))
	}
	if *teardown && *cluster == "" {
		exitOn(fmt.Errorf("--teardown needs --cluster (CLUSTER_NAME): the cluster whose install to remove"))
	}
	s := state.NewSetup()
	s.Profile, s.Region, s.ClusterName, s.SubstrateTag = *profile, *region, *cluster, *tag
	s.Database, s.DemoMode, s.Teardown = *database, *demoMode, *teardown
	s.KeepClusterOnly = *keepClust

	if *logPath == "" {
		dir, _ := os.UserCacheDir()
		*logPath = filepath.Join(dir, "substrate-eks", "logs", "installer-"+time.Now().Format("20060102-150405")+".log")
	}
	// The log holds full command lines and output, which name the account
	// (bucket names); it holds no secret. execx is a verbatim substrate-gke
	// copy that opens the file 0644, so create it 0600 first: OpenFile keeps
	// an existing file's mode (review F7).
	exitOn(createPrivate(*logPath))
	logger, err := execx.NewLogger(*logPath)
	exitOn(err)
	defer logger.Close()

	self, _ := os.Executable()
	b := builder{self: self, setupAWS: *setupAWS, overlay: *overlay, cwd: cwd, exeDir: exeDir, treeRoot: *treeRoot}
	d := ui.Deps{
		Probe: b.probe, CreateCluster: b.createCluster, Bootstrap: b.bootstrap, RDS: b.rds,
		ControlPlane: b.controlPlane, Demo: b.demoStep, Verify: b.verify,
		RemoveControlPlane: b.removeControlPlane, Teardown: b.teardown, Inventory: b.inventory,
		LogPath: logger.Path(), DryRun: *dryRun,
	}
	if *dryRun {
		if s.Region == "" {
			s.Region = "us-west-2"
		}
		d.Runner = execx.DryRun{Log: logger}
		d.Checks = simChecks()
		d.Identity = func(context.Context) (string, string, error) {
			return "111122223333", "arn:aws:sts::111122223333:assumed-role/Admin/you", nil
		}
		fixtures := simClusters(s.Region)
		d.Clusters = func(context.Context) ([]awsx.Cluster, error) { return fixtures, nil }
		d.Describe = func(_ context.Context, name string) (awsx.Cluster, error) {
			for _, c := range fixtures {
				if c.Name == name {
					return c, nil
				}
			}
			// A name the dry run does not list is one it "created".
			return simCluster(name, s.Region, true), nil
		}
	} else {
		real := &execx.Real{Log: logger}
		defer real.Drain()
		d.Runner = real
		d.Checks = doctor.Checks(s.Profile, s.Region)
		var client *awsx.Client
		getClient := func(ctx context.Context) (*awsx.Client, error) {
			if client != nil {
				return client, nil
			}
			c, err := awsx.New(ctx, s.Profile, s.Region)
			client = c
			return c, err
		}
		d.Identity = func(ctx context.Context) (string, string, error) {
			c, err := getClient(ctx)
			if err != nil {
				return "", "", err
			}
			return c.Identity(ctx)
		}
		d.Clusters = func(ctx context.Context) ([]awsx.Cluster, error) {
			c, err := getClient(ctx)
			if err != nil {
				return nil, err
			}
			return c.ListClusters(ctx)
		}
		d.Describe = func(ctx context.Context, name string) (awsx.Cluster, error) {
			c, err := getClient(ctx)
			if err != nil {
				return awsx.Cluster{}, err
			}
			return c.Describe(ctx, name)
		}
	}
	final, err := tea.NewProgram(ui.New(d, s), tea.WithAltScreen()).Run()
	exitOn(err)
	if fm, ok := final.(ui.Model); ok {
		for _, l := range fm.ExitNotes() {
			fmt.Println(l)
		}
	}
}

// simCluster is a dry-run cluster: ACTIVE 1.37 Auto Mode, carrying the tags
// `setup-aws create cluster` sets when owned.
func simCluster(name, region string, owned bool) awsx.Cluster {
	c := awsx.Cluster{Name: name, Version: "1.37", Status: "ACTIVE", AutoMode: true}
	if owned {
		c.Tags = map[string]string{awsx.CreatedByTagKey: awsx.CreatedByValue, awsx.OwnerTagKey: name + "." + region}
	}
	return c
}

// simClusters cover every badge and both sides of the reinstall guard; the
// probe's dry-run answer is picked by name (probe.SimLine).
func simClusters(region string) []awsx.Cluster {
	legacy := simCluster("legacy-136", region, false)
	legacy.Version, legacy.AutoMode = "1.36", false
	return []awsx.Cluster{
		legacy,
		simCluster("shared-installed", region, false),
		simCluster("sub-native", region, true),
		simCluster("substrate-installed", region, true),
		simCluster("team-partial", region, false),
	}
}

func simChecks() []doctor.Check {
	ok := func(name, detail string) doctor.Check {
		return doctor.Check{Name: name, Run: func(context.Context) doctor.Result { return doctor.Result{Status: doctor.OK, Detail: detail} }}
	}
	return []doctor.Check{
		ok("AWS credentials", doctor.MaskArn("arn:aws:sts::111122223333:assumed-role/Admin/you")),
		ok("go", "go version go1.27.2 linux/amd64"),
		ok("git", "git version 2.47.0"),
		ok("kubectl", "Client Version: v1.37.0"),
		ok("aws", "aws-cli/2.31.0"),
	}
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	exitOn(err)
	return a
}

// createPrivate makes path a 0600 file in a 0700 directory, keeping what an
// existing file holds but narrowing its mode.
func createPrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
