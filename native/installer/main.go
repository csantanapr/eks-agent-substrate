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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/doctor"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/ui"
)

func main() {
	cwd, _ := os.Getwd()
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	// The repository's config.env and config.local.env, under the
	// environment, give the defaults; Settings writes config.local.env.
	cfg, err := state.FindConfig(os.LookupEnv, cwd, exeDir)
	exitOn(err)
	conf := func(key, def string) string {
		if v, ok := cfg.Lookup(key); ok && v != "" {
			return v
		}
		return def
	}
	var (
		doctorMode = flag.Bool("doctor", false, "run the preflight checks and exit")
		dryRun     = flag.Bool("dry-run", false, "walk the full wizard without touching AWS (simulated commands)")
		profile    = flag.String("profile", conf("AWS_PROFILE", ""), "AWS shared config profile (AWS_PROFILE)")
		region     = flag.String("region", conf("AWS_REGION", ""), "AWS Region (AWS_REGION)")
		cluster    = flag.String("cluster", os.Getenv("CLUSTER_NAME"), "preselect this cluster, or the name of a new one (CLUSTER_NAME; the wizard also offers config.env's)")
		tag        = flag.String("tag", conf("SUBSTRATE_TAG", state.DefaultTag), "Substrate release to install (SUBSTRATE_TAG)")
		imageRepo  = flag.String("image-repo", conf("SUBSTRATE_IMAGE_REPO", state.DefaultImageRepo), "where the release's images come from (SUBSTRATE_IMAGE_REPO)")
		classes    = flag.String("sandbox-classes", state.DefaultSandboxClasses, "sandbox classes to install and check: gvisor, microvm or both")
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
		plugins    = flag.String("plugins", "", "comma-separated plugin IDs: the wizard preselects them, --deploy-control-plane carries their needs, --plugin-action acts on them, and with the recorded ones they decide the release every step uses")
		pluginAct  = flag.String("plugin-action", "", "deploy, verify or remove the --plugins on --cluster (remove with no --plugins: every recorded plugin), then exit")

		redact  = flag.Bool("redact", false, "also mask the rest of each account id, ARNs, bucket names and phone numbers, on screen, in snapshots and in the exit notes")
		snapDir = flag.String("snapshot-dir", "", "where s saves the screen as .ansi, .txt and .html (default: under the user cache dir)")
	)
	flag.Parse()
	// One identity for the whole run (review R15): every child (setup-aws,
	// kubectl's `aws eks get-token`, ate-setup, the aws CLI) and every
	// in-process AWS SDK load inherit AWS_PROFILE, so it is set to --profile,
	// or unset when --profile is empty, before anything runs. Otherwise
	// --profile "" with AWS_PROFILE exported ran setup-aws as the default
	// profile and everything else as the exported one. The aws CLI reads
	// AWS_DEFAULT_PROFILE before AWS_PROFILE, so a --profile drops it too;
	// with no --profile it stays, as the SDK and the CLI then both use it.
	if *profile == "" {
		exitOn(os.Unsetenv("AWS_PROFILE"))
	} else {
		exitOn(os.Setenv("AWS_PROFILE", *profile))
		exitOn(os.Unsetenv("AWS_DEFAULT_PROFILE"))
	}

	if *database != controlplane.DatabaseRDS && *database != controlplane.DatabaseBundled {
		exitOn(fmt.Errorf("--database is %q; want %s or %s", *database, controlplane.DatabaseRDS, controlplane.DatabaseBundled))
	}
	exitOn(state.ValidSandboxClasses(*classes))

	findOverlay := func() string {
		dir, err := eksoverlay.Find(*overlay, cwd, exeDir)
		exitOn(err)
		return dir
	}
	pluginIDs := plugin.ParseRecord(*plugins)
	for _, id := range pluginIDs {
		if _, ok := plugin.Get(id); !ok {
			exitOn(fmt.Errorf("--plugins: unknown plugin %q", id))
		}
	}
	pluginOptions := func() plugin.Options {
		tags, ok := os.LookupEnv("TAGS")
		if !ok {
			tags = controlplane.DefaultTags
		}
		b := builder{setupAWS: *setupAWS, cwd: cwd, exeDir: exeDir}
		return plugin.Options{SetupAWS: absPath(b.findSetupAWS()), Tags: tags}
	}
	cpOptions := func() controlplane.Options {
		return controlplane.Options{
			Cluster: *cluster, Region: *region, Profile: *profile,
			Tag: *tag, ImageRepo: *imageRepo,
			SnapBucket: *snapBucket, AssetBucket: *assetBkt, SandboxClasses: *classes,
			Database: *database, TreeRoot: *treeRoot, Overlay: findOverlay(),
			Plugins: pluginIDs, Plugin: pluginOptions(),
		}
	}
	// clusterOptions are the plugin options of --cluster on the --tag
	// release, which clusterRelease replaces with the install's.
	clusterOptions := func() plugin.Options {
		po := pluginOptions()
		po.Cluster, po.Region, po.Profile, po.TreeRoot = *cluster, *region, *profile, *treeRoot
		po.Release = plugin.Release{Tag: *tag, ImageRepo: *imageRepo}
		// The trees are keyed by the overlay applied to them.
		digest, err := eksoverlay.Digest(findOverlay())
		exitOn(err)
		po.OverlayDigest = digest
		return po
	}
	demoOptions := demo.Options{
		Cluster: *cluster, Region: *region, Profile: *profile, Tag: *tag,
		ImageRepo: *imageRepo, SandboxClasses: *classes,
	}

	// A subcommand's ctx ends on SIGINT (ctrl+c) or SIGTERM instead of the
	// process dying, so its children are stopped and its cleanup runs: the
	// database bootstrap deletes the Secret holding the logins. The wizard,
	// which reads ctrl+c as a key, keeps the default handlers (default case).
	switch ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM); {
	case *addOverlay != "":
		exitOn(eksoverlay.Apply(os.Stdout, absPath(*addOverlay), findOverlay(), *region))
		return
	case *deployCP:
		exitOn(controlplane.Deploy(ctx, os.Stdout, cpOptions()))
		return
	case *removeCP:
		exitOn(controlplane.Delete(ctx, os.Stdout, cpOptions()))
		return
	case *deployDemo || *verify:
		// The demo runs on the install's release: a fork's when a recorded
		// or chosen plugin installed one.
		po, err := clusterRelease(ctx, clusterOptions(), pluginIDs, *deployDemo)
		exitOn(err)
		demoOptions.Tag, demoOptions.ImageRepo, demoOptions.Tree = po.Tag, po.ImageRepo, po.Tree
		if *deployDemo {
			exitOn(demo.Deploy(ctx, os.Stdout, demoOptions))
		}
		if *verify {
			exitOn(demo.Verify(ctx, os.Stdout, demoOptions))
		}
		return
	case *pluginAct != "":
		exitOn(runPlugins(ctx, *pluginAct, pluginIDs, clusterOptions()))
		return
	case *probeInst:
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		exitOn(probe.Run(ctx, os.Stdout, *profile, *region, *cluster))
		return
	case *doctorMode:
		fmt.Println("substrate-eks preflight doctor")
		fmt.Println()
		if n := doctor.RunCLI(ctx, doctor.Checks(*profile, *region)); n > 0 {
			fmt.Printf("\n%d fatal problem(s) found.\n", n)
			os.Exit(1)
		}
		fmt.Println("\nAll good — run the installer.")
		return
	default:
		stop()
	}

	if *region == "" && !*dryRun {
		exitOn(fmt.Errorf("--region (AWS_REGION) is required"))
	}
	if *teardown && *cluster == "" {
		exitOn(fmt.Errorf("--teardown needs --cluster (CLUSTER_NAME): the cluster whose install to remove"))
	}
	s := state.NewSetupFrom(cfg.Lookup)
	s.Profile, s.Region, s.ClusterName, s.SubstrateTag = *profile, *region, *cluster, *tag
	s.ImageRepo, s.SandboxClasses = *imageRepo, *classes
	if s.ClusterName == "" && !*teardown {
		// config.local.env's cluster, ready to pick or create; never a teardown's target, which is named.
		s.ClusterName = conf("CLUSTER_NAME", "")
	}
	s.Database, s.DemoMode, s.Teardown = *database, *demoMode, *teardown
	s.KeepClusterOnly = *keepClust
	s.Plugins = pluginIDs

	cache, _ := os.UserCacheDir()
	if *logPath == "" {
		*logPath = filepath.Join(cache, "substrate-eks", "logs", "installer-"+time.Now().Format("20060102-150405")+".log")
	}
	if *snapDir == "" {
		*snapDir = filepath.Join(cache, "substrate-eks", "snapshots")
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
	b := builder{self: self, setupAWS: *setupAWS, overlay: *overlay, cwd: cwd, exeDir: exeDir, treeRoot: *treeRoot,
		stepEnv: cfg.StepEnv()}
	d := ui.Deps{
		Probe: b.probe, CreateCluster: b.createCluster, Bootstrap: b.bootstrap, RDS: b.rds,
		ControlPlane: b.controlPlane, Plugins: b.plugins, Demo: b.demoStep, Verify: b.verify,
		RemoveControlPlane: b.removeControlPlane, Teardown: b.teardown, Inventory: b.inventory,
		LogPath: logger.Path(), DryRun: *dryRun,
		Splash: true, SnapshotDir: *snapDir, Redact: *redact, Config: cfg,
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
		d.StopTree = ui.StopProcessTree
		if cfg.Dir != "" {
			d.SaveConfig = func(s *state.Setup) (string, error) { return saveConfig(cfg, s, cache) }
		}
		d.Checks = doctor.Checks(s.Profile, s.Region)
		reads := awsx.Fresh{Profile: s.Profile, Region: s.Region}
		d.Identity, d.Clusters, d.Describe = reads.Identity, reads.ListClusters, reads.Describe
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

// runPlugins is --plugin-action: the plugins, on the install's release
// (clusterRelease; a deploy is refused on a release the control plane does
// not run).
func runPlugins(ctx context.Context, action string, ids []string, o plugin.Options) error {
	if o.Cluster == "" || o.Region == "" {
		return fmt.Errorf("--plugin-action needs --cluster and --region")
	}
	if action != plugin.ActionRemove && len(ids) == 0 {
		return fmt.Errorf("--plugin-action %s needs --plugins", action)
	}
	po, err := clusterRelease(ctx, o, ids, action == plugin.ActionDeploy)
	if err != nil {
		return err
	}
	return plugin.Run(ctx, os.Stdout, action, ids, po)
}

// ReleaseLine is printed with the release a subcommand decided to use.
const ReleaseLine = "Substrate release: "

// clusterRelease writes the installer's own kubeconfig for o's cluster, as
// the control plane deploy does, and returns o for that cluster on the
// install's one release: the Source of a recorded or chosen plugin, else o's
// --tag release (plugin.ReadRelease). A deploy (deploying) is refused, before
// it applies anything, on a release other than the one the control plane
// runs (controlplane.SameRelease): its worker pools would not run there.
func clusterRelease(ctx context.Context, o plugin.Options, chosen []string, deploying bool) (plugin.Options, error) {
	if o.Cluster == "" || o.Region == "" {
		return o, fmt.Errorf("--cluster and --region are required")
	}
	c, err := awsx.New(ctx, o.Profile, o.Region)
	if err != nil {
		return o, err
	}
	cl, err := c.Describe(ctx, o.Cluster)
	if err != nil {
		return o, err
	}
	o.AutoMode, o.Kubeconfig = cl.AutoMode, awsx.KubeconfigPath(o.Cluster, o.Region)
	fmt.Println(controlplane.KubeconfLine + o.Kubeconfig)
	if err := awsx.WriteKubeconfig(o.Kubeconfig, cl, o.Region, o.Profile); err != nil {
		return o, err
	}
	if o.Release, err = plugin.ReadRelease(ctx, os.Stdout, o, chosen); err != nil {
		return o, err
	}
	fmt.Println(ReleaseLine + o.Tag + " (" + o.ImageRepo + ")")
	// What the control plane runs: a plugin removal on another release
	// gives the reinstall as the fix, and a deploy there is refused.
	if o.Running, err = controlplane.InstalledRelease(ctx, o.Kubeconfig); err != nil {
		return o, fmt.Errorf("read the release %s runs: %w", o.Cluster, err)
	}
	if deploying {
		if err := controlplane.CheckRelease(o.Cluster, o.Running, o.Release); err != nil {
			return o, err
		}
	}
	return o, nil
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	exitOn(err)
	return a
}

// saveConfig writes s's Settings answers to cfg's config.local.env (0600),
// keeping a different earlier file as a backup under the user cache dir, and
// returns its path. It is written when the install starts, so the next
// wizard run starts from the same answers.
func saveConfig(cfg state.Config, s *state.Setup, cache string) (string, error) {
	text, err := cfg.RenderLocal(state.ConfigValues(s))
	if err != nil {
		return "", err
	}
	backup := filepath.Join(cache, "substrate-eks", "config-backups", "config.local.env."+time.Now().Format("20060102-150405"))
	return cfg.LocalPath(), cfg.WriteLocal(text, backup)
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
