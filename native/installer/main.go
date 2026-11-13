// The substrate-eks installer: an interactive wizard that provisions AWS
// resources (setup-aws) and installs the Agent Substrate control plane onto an
// EKS cluster with upstream's ate-setup, from a pinned release tree it fetches
// on demand. It follows substrate-gke's installer architecture package for
// package; see native/README.md for the mapping.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/ui"
)

func main() {
	var (
		doctorMode = flag.Bool("doctor", false, "run the preflight checks and exit")
		dryRun     = flag.Bool("dry-run", false, "walk the full wizard without touching AWS (simulated commands)")
		profile    = flag.String("profile", os.Getenv("AWS_PROFILE"), "AWS shared config profile (AWS_PROFILE)")
		region     = flag.String("region", os.Getenv("AWS_REGION"), "AWS Region (AWS_REGION)")
		cluster    = flag.String("cluster", os.Getenv("CLUSTER_NAME"), "preselect this cluster (CLUSTER_NAME)")
		tag        = flag.String("tag", "v1.0.0", "Substrate release to install")
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
	)
	flag.Parse()

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

	switch {
	case *addOverlay != "":
		exitOn(eksoverlay.Apply(os.Stdout, absPath(*addOverlay), findOverlay(), *region))
		return
	case *deployCP:
		exitOn(controlplane.Deploy(context.Background(), os.Stdout, controlplane.Options{
			Cluster: *cluster, Region: *region, Profile: *profile,
			Tag: *tag, Repo: controlplane.DefaultRepo, ImageRepo: "ghcr.io/agent-substrate/substrate",
			SnapBucket: *snapBucket, AssetBucket: *assetBkt, SandboxClasses: "gvisor,microvm",
			TreeRoot: *treeRoot, Overlay: findOverlay(),
		}))
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
	s := state.NewSetup()
	s.Profile, s.Region, s.ClusterName, s.SubstrateTag = *profile, *region, *cluster, *tag

	if *logPath == "" {
		dir, _ := os.UserCacheDir()
		*logPath = filepath.Join(dir, "substrate-eks", "logs", "installer-"+time.Now().Format("20060102-150405")+".log")
	}
	logger, err := execx.NewLogger(*logPath)
	exitOn(err)
	defer logger.Close()

	self, _ := os.Executable()
	b := builder{self: self, setupAWS: *setupAWS, overlay: *overlay, cwd: cwd, exeDir: exeDir, treeRoot: *treeRoot}
	d := ui.Deps{Bootstrap: b.bootstrap, ControlPlane: b.controlPlane, LogPath: logger.Path(), DryRun: *dryRun}
	if *dryRun {
		if s.Region == "" {
			s.Region = "us-west-2"
		}
		d.Runner = execx.DryRun{Log: logger}
		d.Checks = simChecks()
		d.Identity = func(context.Context) (string, string, error) {
			return "111122223333", "arn:aws:sts::111122223333:assumed-role/Admin/you", nil
		}
		d.Clusters = func(context.Context) ([]awsx.Cluster, error) {
			return []awsx.Cluster{
				{Name: "legacy-136", Version: "1.36", Status: "ACTIVE"},
				{Name: "sub-native", Version: "1.37", Status: "ACTIVE", AutoMode: true},
			}, nil
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
	}
	if _, err := tea.NewProgram(ui.New(d, s), tea.WithAltScreen()).Run(); err != nil {
		exitOn(err)
	}
}

func simChecks() []doctor.Check {
	ok := func(name, detail string) doctor.Check {
		return doctor.Check{Name: name, Run: func(context.Context) doctor.Result { return doctor.Result{Status: doctor.OK, Detail: detail} }}
	}
	return []doctor.Check{
		ok("AWS credentials", "arn:aws:sts::111122223333:assumed-role/Admin/you"),
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

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
