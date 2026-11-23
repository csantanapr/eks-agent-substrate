package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
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
	// stepEnv is state.Config.StepEnv(state.RDSKeys): settings from the
	// config files that `setup-aws create rds` reads from its environment.
	// A Spec carries only the ones its step reads (settings).
	stepEnv []string
}

// settings are the entries of stepEnv whose key is one of keys
// (state.RDSKeys): a step gets only what it reads, so no other step's
// command shows it (review N2).
func (b builder) settings(keys []string) []string {
	var out []string
	for _, e := range b.stepEnv {
		if k, _, _ := strings.Cut(e, "="); slices.Contains(keys, k) {
			out = append(out, e)
		}
	}
	return out
}

// findSetupAWS returns --setup-aws when given (a source directory or a
// binary); else, when this executable is a release's (state.IsRelease), the
// release's setup-aws binary beside it; else native/tools/setup-aws,
// searched above cwd and the executable, like substrate-gke finds its
// tools/setup-gcp. A source build always runs that source with `go run`,
// even when a setup-aws binary was built beside it into native/bin, which
// could be stale.
func (b builder) findSetupAWS() string {
	if b.setupAWS != "" {
		return b.setupAWS
	}
	if state.IsRelease(b.exeDir) {
		return filepath.Join(b.exeDir, "setup-aws")
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
	env := append([]string{"CLUSTER_NAME=" + s.ClusterName, "AWS_REGION=" + s.Region}, awsx.ProfileEnv(s.Profile)...)
	return append(env, "TAGS="+s.Tags, "PLAN=false", "KEEP_CLUSTER="+strconv.FormatBool(s.KeepCluster))
}

// setupAWSSpec runs setup-aws with args: the release's binary, or `go -C
// <setup-aws> run .` from source (plugin.SetupAWSArgv), with sim replayed
// under --dry-run. settings are the step settings this subcommand reads
// (b.settings), shown and passed; env is passed only.
func (b builder) setupAWSSpec(s *state.Setup, label string, settings, env []string, sim []string, args ...string) execx.Spec {
	dir := b.findSetupAWS()
	argv := plugin.SetupAWSArgv(dir, args...)
	var shown []string
	// The step settings are shown too, so the pasted command runs the same
	// step: setup-aws reads no config file.
	for _, e := range append(slices.Clone(settings), b.env(s)...) {
		if k, v, _ := strings.Cut(e, "="); v != "" && k != "TAGS" && k != "PLAN" && k != "KEEP_CLUSTER" {
			shown = append(shown, k+"="+shellQuote(v))
		}
	}
	if argv[0] == "go" {
		shown = append(shown, "go", "-C", b.rel(dir), "run", ".")
	} else {
		shown = append(shown, b.rel(argv[0]))
	}
	return execx.Spec{
		Label:    label,
		Display:  strings.Join(append(shown, args...), " "),
		Argv:     argv,
		Env:      append(append(slices.Clone(settings), b.env(s)...), env...),
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

// createCluster runs `setup-aws create cluster` with config.env's knobs and
// the node groups the run's sandbox classes and plugins need (NODE_GROUPS,
// shown in the command: none on Auto Mode with only the AWS Nitro sandbox).
func (b builder) createCluster(s *state.Setup) execx.Spec {
	settings := []string{"NODE_GROUPS=" + nodegroups.Format(s.NodeGroups())}
	// A custom AMI is shown too: a pasted command without it would make the
	// groups from EKS's AMI.
	if s.NewClusterConfig.NodeAMIID != "" {
		settings = append(settings, "NODE_AMI_ID="+s.NewClusterConfig.NodeAMIID)
	}
	// So is a node group type other than the default.
	if t := s.NewClusterConfig.NodeGroupType; t != "" && t != nodegroups.Managed {
		settings = append(settings, "NODE_GROUP_TYPE="+t)
	}
	// And the nitro group's AMI, which it cannot be made without.
	if slices.Contains(s.NodeGroups(), nodegroups.Nitro) {
		settings = append(settings, "NITRO_AMI_ID="+s.NewClusterConfig.NitroAMIID)
	}
	// And Karpenter, which makes the gvisor group a NodePool, and its
	// NodeOverlay, which makes the microvm and nitro groups NodePools too.
	karpenter := s.NewClusterConfig.KarpenterOn()
	if karpenter {
		settings = append(settings, "KARPENTER="+nodegroups.KarpenterOn)
	}
	if s.NewClusterConfig.NodeOverlayOn() {
		settings = append(settings, "KARPENTER_NODE_OVERLAY="+nodegroups.KarpenterOn)
	}
	return b.setupAWSSpec(s, "setup-aws create cluster", settings, s.NewClusterConfig.Env(),
		simPhases(phases.ClusterFor(s.NewClusterConfig.NodeGroupType, karpenter), simStart(s, "Creating the cluster..."),
			`level=INFO msg="Cluster ready." cluster=`+s.ClusterName+` created=27 changed=0 took=19m40s`),
		"create", "cluster")
}

// bootstrap runs `setup-aws bootstrap`.
func (b builder) bootstrap(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws bootstrap", nil, nil,
		simPhases(phases.Bootstrap, simStart(s, "Starting full bootstrap..."), `level=INFO msg="Bootstrap completed successfully."`),
		"bootstrap")
}

// rds runs `setup-aws create rds`, the one setup-aws step that reads step
// settings (the DB_* ones).
func (b builder) rds(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws create rds", b.settings(state.RDSKeys), nil,
		simPhases(phases.RDS, simStart(s, "Starting RDS setup..."), `level=INFO msg="RDS setup completed successfully."`),
		"create", "rds")
}

// teardown runs `setup-aws teardown`, with --keep-cluster when s.KeepCluster.
func (b builder) teardown(s *state.Setup) execx.Spec {
	args := []string{"teardown"}
	if s.KeepCluster {
		args = append(args, "--keep-cluster")
	}
	return b.setupAWSSpec(s, "setup-aws "+strings.Join(args, " "), nil, nil,
		simPhases(phases.Teardown, `level=INFO msg="Starting teardown of `+s.ClusterName+"."+s.Region+`..."`,
			`level=INFO msg="Teardown complete: nothing left for `+s.ClusterName+"."+s.Region+`"`),
		args...)
}

// inventory is `setup-aws inventory`, which the teardown's Complete screen
// offers to show that nothing with the owner tag is left.
func (b builder) inventory(s *state.Setup) execx.Spec {
	return b.setupAWSSpec(s, "setup-aws inventory", nil, nil, nil, "inventory")
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
		Label:   label,
		Display: strings.Join(shown, " "),
		Argv:    append([]string{b.self}, args...),
		// The control plane's StorageClass and the plugins tag what they
		// make with TAGS, read from the environment: pin it to the run's
		// answer, which Settings may have changed.
		Env:      []string{"TAGS=" + s.Tags},
		SimLines: sim,
	}
}

// releaseFlags decide the release a step installs or uses, the same on the
// control plane, plugins, demo and verify steps: --tag, --image-repo when it
// is not the default, and --plugins, whose Source replaces the --tag release
// (plugin.ReadRelease, with the recorded plugins). So a run that chooses
// mode 3, or the AWS Nitro sandbox class, is on its fork from the control
// plane step on. ids are the plugins the step names: every plugin the
// install carries (s.InstallPlugins) on the control plane, demo and verify
// steps, and the ones a plugin step deploys on the two plugin steps.
func releaseFlags(s *state.Setup, ids []string) []string {
	f := []string{"--tag", s.SubstrateTag}
	if s.ImageRepo != state.DefaultImageRepo {
		f = append(f, "--image-repo", s.ImageRepo)
	}
	if len(ids) > 0 {
		f = append(f, "--plugins", plugin.FormatRecord(ids))
	}
	return f
}

// classFlags are --sandbox-classes, always: a child's default is
// SANDBOX_CLASSES from the config files, which the run's Settings may have
// changed.
func classFlags(s *state.Setup) []string {
	return []string{"--sandbox-classes", s.SandboxClasses}
}

// nitroFlags are --nitro-compute for the children that deploy, check or
// remove the AWS Nitro sandbox class's plugin, when the run's NITRO_COMPUTE
// is not the default, or when the child would read another one from the
// environment it inherits (Settings may have changed it). The default
// passes nothing, so the default commands stay as they were; a child reads
// config.local.env, which the install writes with the run's answers. An
// empty NITRO_COMPUTE in the environment is unset to the child, as to this
// run (conf, NewSetupFrom).
func nitroFlags(s *state.Setup) []string {
	v := s.NewClusterConfig.NitroCompute
	if v == "" {
		v = plugin.NitroAutoMode
	}
	if env, ok := os.LookupEnv("NITRO_COMPUTE"); v == plugin.NitroAutoMode && (!ok || env == "" || env == v) {
		return nil
	}
	return []string{"--nitro-compute", v}
}

// release is the release s's steps install and use (releaseFlags).
func release(s *state.Setup) plugin.Release {
	return plugin.ChosenRelease(s.InstallPlugins(), s.SubstrateTag, s.ImageRepo)
}

// controlPlane re-executes the installer with --deploy-control-plane.
func (b builder) controlPlane(s *state.Setup) execx.Spec {
	sim := []string{
		controlplane.KubeconfLine + "~/.cache/substrate-eks/kubeconfig/" + s.ClusterName + "." + s.Region + ".yaml",
	}
	ids := s.InstallPlugins()
	if len(ids) > 0 {
		sim = append(sim, controlplane.PluginsLine+strings.Join(resolvedIDs(ids), ", "))
	}
	rel := release(s)
	sim = append(sim,
		controlplane.CachedLine+rel.Tag,
		eksoverlay.LinePrefix+"added deploy/eks to "+eksoverlay.BaseKustomization,
		eksoverlay.LinePrefix+"applied code patch ate-setup-s3-assets-v1.0.0.patch",
	)
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
	sim = append(sim, "[step]: deploy_ate_system")
	// ate-setup stages the microVM assets only for its microvm class.
	ateClasses := state.AteSetupClasses(s.SandboxClasses)
	if state.HasClass(ateClasses, state.ClassMicroVM) {
		sim = append(sim, "[step]: prepare_microvm_deps", "Uploading assets to s3://"+s.AssetBucket()+"/microvm-assets/ ...")
	}
	sim = append(sim,
		"[step]: deploy_crds", "[step]: ensure_apiserver_prerequisites",
		"[step]: Waiting for ATE system components to be ready...",
		`deployment "ate-api-server" successfully rolled out`,
		`daemon set "atelet" successfully rolled out`,
		controlplane.ReadyLine,
	)
	flags := []string{"--deploy-control-plane", "--database", s.Database,
		"--snap-bucket", s.SnapBucket(), "--asset-bucket", s.AssetBucket()}
	flags = append(append(append(flags, releaseFlags(s, ids)...), classFlags(s)...), nitroFlags(s)...)
	spec := b.selfSpec(s, s.ClusterName, "ate-setup deploy ate-system", sim, flags...)
	spec.Display = "go run ./cmd/ate-setup deploy ate-system  (substrate " + rel.Tag + " + deploy/eks, " + ateClasses + ", database " + s.Database + ")"
	spec.DisplayIsSummary = true
	return spec
}

// resolvedIDs is ids plus what they require, in install order (ids as given
// when one is unknown: main has already refused that).
func resolvedIDs(ids []string) []string {
	ps, err := plugin.Resolve(ids)
	if err != nil {
		return ids
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// plugins re-executes the installer with --plugin-action deploy for the
// chosen plugins, after the control plane carries their needs.
func (b builder) plugins(s *state.Setup) execx.Spec {
	return b.pluginStep(s, "plugins", s.Plugins)
}

// nitro is the AWS Nitro sandbox class's step: --plugin-action deploy for
// the plugin the class installs (state.ClassPlugins), after the control
// plane carries its needs and before the demo checks the class.
func (b builder) nitro(s *state.Setup) execx.Spec {
	return b.pluginStep(s, "AWS Nitro sandbox", state.ClassPlugins(s.SandboxClasses))
}

// pluginStep is --plugin-action deploy for ids. The release is the
// install's: what the record lists and ids decide it, as on every step.
func (b builder) pluginStep(s *state.Setup, label string, ids []string) execx.Spec {
	var sim []string
	for _, id := range resolvedIDs(ids) {
		sim = append(sim, plugin.DeployLine+id, plugin.VerifyLine+id, plugin.ReadyLine+id)
	}
	// No plugin settings (state.PluginKeys) in its Env: --plugin-action
	// reads them from the config files itself (loadPluginSettings), so the
	// log, which prints each Spec's Env, never holds WHATSAPP_PEER.
	return b.selfSpec(s, s.ClusterName, label, sim, append(append([]string{"--plugin-action", plugin.ActionDeploy}, releaseFlags(s, ids)...), nitroFlags(s)...)...)
}

// demoStep re-executes the installer with --deploy-demo --verify: the counter
// demo on every gvisor and microvm class, then the suspend/resume check on
// each class, the AWS Nitro sandbox's through its plugin's counter check.
func (b builder) demoStep(s *state.Setup) execx.Spec {
	var sim []string
	for _, c := range state.DemoClasses(s.SandboxClasses) {
		sim = append(sim, demo.DeployLine+c, "actor counter-"+c+" is Running")
	}
	for _, c := range state.ParseClasses(s.SandboxClasses) {
		if c == state.ClassNitro {
			sim = append(sim, demo.VerifyLine+c)
			for _, id := range resolvedIDs(state.ClassPlugins(c)) {
				sim = append(sim, plugin.VerifyLine+id, plugin.ReadyLine+id)
			}
			continue
		}
		sim = append(sim, demo.VerifyLine+c, "suspended counter-"+c+"; snapshot in s3://"+s.SnapBucket()+"/",
			demo.VerifyPassLine+c+": memory, file and rootfs counters continued after resume")
	}
	flags := append(append(append([]string{"--deploy-demo", "--verify"}, releaseFlags(s, s.InstallPlugins())...), classFlags(s)...), nitroFlags(s)...)
	return b.selfSpec(s, s.ClusterName, "counter demo", sim, flags...)
}

// verify is `substrate-eks --verify`, the user-facing check (AC7.2), shown
// on the Complete screen of a demo run.
func (b builder) verify(s *state.Setup) execx.Spec {
	// The release flags, as demoStep passes them: verify reads the tree of
	// the release that was installed (review F11).
	return b.selfSpec(s, s.ClusterName, "verify", nil, append(append(append([]string{"--verify"}, releaseFlags(s, s.InstallPlugins())...), classFlags(s)...), nitroFlags(s)...)...)
}

// removeControlPlane re-executes the installer with --remove-control-plane
// for cluster: the guard runs it on a found install, the teardown flow on
// the run's own. The dry run replays the removal of simRecord's plugins
// first, in the order and with the lines plugin.Run's remove prints.
func (b builder) removeControlPlane(s *state.Setup) execx.Spec {
	sim := []string{controlplane.RemovePluginsLine}
	ps, _ := steps.RemovalOrder(simRecord(s))
	for _, p := range ps {
		if p.SetupAWS {
			sim = append(sim, plugin.PlanLine+p.ID)
		}
	}
	for _, p := range ps {
		sim = append(sim, plugin.RemoveLine+p.ID, plugin.RemovedLine+p.ID)
	}
	sim = append(sim,
		controlplane.CachedLine+s.SubstrateTag,
		controlplane.DeleteLine+" --context "+s.ClusterName,
		`namespace "ate-system" deleted`,
		controlplane.RemovedLine,
	)
	return b.selfSpec(s, s.ClusterName, "ate-setup delete ate-system", sim, append([]string{"--remove-control-plane", "--tag", s.SubstrateTag}, nitroFlags(s)...)...)
}

// simRecord is the plugin record of s's cluster in the dry run's story: the
// one its probe answer lists on an installed cluster (probe.SimLine), else
// the plugins the run installs, added to what a record-only cluster's
// record lists.
func simRecord(s *state.Setup) []string {
	r, err := probe.ParseInstalled([]string{probe.SimLine(s.ClusterName)})
	if err == nil && r.Installed && r.PluginsRead {
		return r.Plugins
	}
	return plugin.Record{Plugins: append(r.Removed(), resolvedIDs(s.InstallPlugins())...)}.All()
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
