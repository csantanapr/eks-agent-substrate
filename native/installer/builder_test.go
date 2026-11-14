package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/steps"
)

func testBuilder() (builder, *state.Setup) {
	b := builder{self: "/w/native/bin/substrate-eks", setupAWS: "/w/native/tools/setup-aws", cwd: "/w/native", treeRoot: controlplane.DefaultTreeRoot()}
	s := state.NewSetup()
	s.Region, s.ClusterName, s.Profile, s.Account = "us-west-2", "demo", "eks-sandbox", "111122223333"
	return b, s
}

func allSpecs(b builder, s *state.Setup) map[string]execx.Spec {
	return map[string]execx.Spec{
		"create cluster": b.createCluster(s), "bootstrap": b.bootstrap(s), "rds": b.rds(s),
		"control plane": b.controlPlane(s), "demo": b.demoStep(s), "verify": b.verify(s),
		"remove": b.removeControlPlane(s), "teardown": b.teardown(s), "inventory": b.inventory(s),
		"probe": b.probe("other", s), "plugins": b.plugins(s),
	}
}

// Settings' release answers reach the children that install or read the
// release: --image-repo on the control plane, plugins, demo and verify steps
// and --sandbox-classes on all but plugins, when they are not the defaults,
// and on no other step; the defaults add nothing, so the shown command stays
// short (both directions).
func TestReleaseFlagsReachTheReleaseSteps(t *testing.T) {
	for _, custom := range []bool{false, true} {
		b, s := testBuilder()
		if custom {
			s.ImageRepo, s.SandboxClasses = "registry.example.com/substrate", "gvisor"
		}
		for name, spec := range allSpecs(b, s) {
			release := name == "control plane" || name == "demo" || name == "verify" || name == "plugins"
			i := slices.Index(spec.Argv, "--image-repo")
			j := slices.Index(spec.Argv, "--sandbox-classes")
			if (i >= 0) != (custom && release) || (j >= 0) != (custom && release && name != "plugins") {
				t.Errorf("custom=%v %s: argv %q", custom, name, spec.Argv)
				continue
			}
			if i >= 0 && spec.Argv[i+1] != s.ImageRepo || j >= 0 && spec.Argv[j+1] != "gvisor" {
				t.Errorf("%s: argv %q", name, spec.Argv)
			}
		}
	}
}

// argvRelease is the release a re-executed step's subcommand decides from its
// argv, on a cluster with nothing recorded: plugin.ReadRelease's rule, from
// the --tag, --image-repo and --plugins the step was given.
func argvRelease(t *testing.T, argv []string) plugin.Release {
	t.Helper()
	val := func(name, def string) string {
		if i := slices.Index(argv, name); i >= 0 && i+1 < len(argv) {
			return argv[i+1]
		}
		return def
	}
	ps, err := plugin.Resolve(plugin.ParseRecord(val("--plugins", "")))
	if err != nil {
		t.Fatal(err)
	}
	r, err := plugin.ReleaseOf(ps, val("--tag", "unset"), val("--image-repo", state.DefaultImageRepo))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Both ways: with mode3 chosen, every step that installs or uses the release
// (the control plane, the demo, its check and the plugins) carries the fork's
// release from the first of them, and the control plane step says so; without
// it, each carries v1.0.0. The control plane removal is left out: it reads the
// release of the install it removes off the cluster's record (the reinstall
// guard runs it on a found install, which the chosen plugins say nothing about).
func TestEveryStepCarriesTheForkReleaseWithMode3(t *testing.T) {
	for _, chosen := range [][]string{{"mode3", "efs", "credprovider-aws"}, {"efs", "credprovider-aws"}} {
		b, s := testBuilder()
		s.Plugins = chosen
		fork := slices.Contains(chosen, mode3.ID)
		want := plugin.Release{Tag: state.DefaultTag, ImageRepo: state.DefaultImageRepo, Repo: plugin.UpstreamRepo, Ref: state.DefaultTag}
		if fork {
			want = plugin.Release{Tag: mode3.Tag, ImageRepo: mode3.ImageRepo, Repo: mode3.Repo, Ref: mode3.Commit}
		}
		specs := allSpecs(b, s)
		for _, name := range []string{"control plane", "plugins", "demo", "verify"} {
			if got := argvRelease(t, specs[name].Argv); got != want {
				t.Errorf("chosen %v, %s: release %+v, want %+v (argv %q)", chosen, name, got, want, specs[name].Argv)
			}
		}
		cp := specs["control plane"]
		if !strings.Contains(cp.Display, "substrate "+want.Tag+" ") || !slices.Contains(cp.SimLines, controlplane.CachedLine+want.Tag) {
			t.Errorf("chosen %v: the control plane step shows %q / %q, not %s", chosen, cp.Display, cp.SimLines, want.Tag)
		}
	}
}

// The control plane and plugin children read TAGS from their environment,
// so each re-executed step pins it to the run's answer, which Settings may
// have changed from the inherited one.
func TestSelfSpecsPinTags(t *testing.T) {
	b, s := testBuilder()
	s.Tags = "team=a auto-delete=no"
	for name, spec := range allSpecs(b, s) {
		if spec.Argv[0] == b.self && !slices.Contains(spec.Env, "TAGS=team=a auto-delete=no") {
			t.Errorf("%s: env %q", name, spec.Env)
		}
	}
	s.Tags = ""
	if env := b.plugins(s).Env; !slices.Contains(env, "TAGS=") {
		t.Errorf("empty TAGS not pinned: %q", env)
	}
}

// Review F4: an exported PLAN or KEEP_CLUSTER must not change what a wizard
// step does. Every setup-aws child gets both pinned (the Spec's Env is laid
// over the inherited environment, so these win), and KEEP_CLUSTER follows
// the teardown choice in both directions.
func TestSetupAWSStepsPinPlanAndKeepCluster(t *testing.T) {
	for _, keep := range []bool{false, true} {
		b, s := testBuilder()
		s.KeepCluster = keep
		for name, spec := range allSpecs(b, s) {
			if spec.Argv[0] != "go" {
				continue
			}
			want := []string{"PLAN=false", "KEEP_CLUSTER=" + strconv.FormatBool(keep)}
			for _, w := range want {
				if !slices.Contains(spec.Env, w) {
					t.Errorf("keep=%v %s: env lacks %s: %v", keep, name, w, spec.Env)
				}
			}
			if strings.Contains(spec.Display, "PLAN=") || strings.Contains(spec.Display, "KEEP_CLUSTER=") {
				t.Errorf("%s: the shown command carries the pinned knobs: %s", name, spec.Display)
			}
		}
	}
}

// Review F9 and F11: every re-executed child gets --profile, even an empty
// one (so an exported AWS_PROFILE cannot swap the identity), and the
// release-reading ones get --tag.
func TestSelfSpecsCarryProfileAndTag(t *testing.T) {
	b, s := testBuilder()
	s.Profile, s.SubstrateTag = "", "v1.0.1"
	for name, spec := range allSpecs(b, s) {
		if spec.Argv[0] != b.self {
			continue
		}
		i := slices.Index(spec.Argv, "--profile")
		if i < 0 || i+1 >= len(spec.Argv) || spec.Argv[i+1] != "" {
			t.Errorf("%s: no empty --profile: %v", name, spec.Argv)
		}
		if name == "verify" || name == "demo" || name == "remove" {
			if j := slices.Index(spec.Argv, "--tag"); j < 0 || spec.Argv[j+1] != "v1.0.1" {
				t.Errorf("%s: no --tag v1.0.1: %v", name, spec.Argv)
			}
		}
	}
}

// No step is a shell: each starts go (setup-aws) or the installer itself.
func TestEverySpecIsAnArgvNotAShell(t *testing.T) {
	b, s := testBuilder()
	for name, spec := range allSpecs(b, s) {
		if spec.Argv[0] != "go" && spec.Argv[0] != b.self {
			t.Errorf("%s starts %q", name, spec.Argv[0])
		}
		for _, a := range spec.Argv {
			if a == "bash" || a == "sh" || a == "-c" {
				t.Errorf("%s argv has %q: %q", name, a, spec.Argv)
			}
		}
	}
}

func TestDatabaseReachesTheControlPlaneStep(t *testing.T) {
	b, s := testBuilder()
	for _, db := range []string{state.DatabaseRDS, state.DatabaseBundled} {
		s.Database = db
		argv := b.controlPlane(s).Argv
		if i := slices.Index(argv, "--database"); i < 0 || argv[i+1] != db {
			t.Errorf("%s: argv %q", db, argv)
		}
	}
}

func TestKeepClusterPassesThroughToSetupAWS(t *testing.T) {
	b, s := testBuilder()
	for _, keep := range []bool{false, true} {
		s.KeepCluster = keep
		sp := b.teardown(s)
		if slices.Contains(sp.Argv, "--keep-cluster") != keep || strings.Contains(sp.Display, "--keep-cluster") != keep {
			t.Errorf("keep=%v: argv %q display %q", keep, sp.Argv, sp.Display)
		}
	}
}

func TestCreateClusterCarriesTheConfigKnobs(t *testing.T) {
	b, s := testBuilder()
	env := b.createCluster(s).Env
	for _, want := range []string{"CLUSTER_NAME=demo", "AWS_PROFILE=eks-sandbox", "KUBERNETES_VERSION=" + s.NewClusterConfig.KubernetesVersion,
		"TAGS=" + s.Tags, "EKS_PUBLIC_ACCESS_CIDRS=" + s.NewClusterConfig.PublicAccessCIDRs} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, env)
		}
	}
}

// AC8.5: each setup-aws step's dry-run replay lights every row of its
// checklist in order, because both come from the same phase list.
func TestDryRunLinesWalkEveryPhase(t *testing.T) {
	b, s := testBuilder()
	for name, tc := range map[string]struct {
		spec  execx.Spec
		phase []string
	}{
		"create cluster": {b.createCluster(s), phases.Cluster},
		"bootstrap":      {b.bootstrap(s), phases.Bootstrap},
		"rds":            {b.rds(s), phases.RDS},
		"teardown":       {b.teardown(s), phases.Teardown},
	} {
		items, active, lit := steps.FromPhases(tc.phase), -1, 0
		for _, line := range tc.spec.SimLines {
			if next := steps.Progress(items, active, line); next != active {
				if next != active+1 {
					t.Errorf("%s: %q jumped from row %d to %d", name, line, active, next)
				}
				active, lit = next, lit+1
			}
		}
		if lit != len(items) {
			t.Errorf("%s: lit %d of %d rows", name, lit, len(items))
		}
	}
}

// The commands the Complete screen prints paste from cwd as the argv run.
func TestDisplayIsThePasteableCommand(t *testing.T) {
	b, s := testBuilder()
	if got, want := b.removeControlPlane(s).Display, "bin/substrate-eks --remove-control-plane --tag v1.0.0 --cluster demo --region us-west-2 --profile eks-sandbox"; got != want {
		t.Errorf("remove display\n got %s\nwant %s", got, want)
	}
	s.KeepCluster = true
	if got, want := b.teardown(s).Display, "CLUSTER_NAME=demo AWS_REGION=us-west-2 AWS_PROFILE=eks-sandbox go -C tools/setup-aws run . teardown --keep-cluster"; got != want {
		t.Errorf("teardown display\n got %s\nwant %s", got, want)
	}
	// An empty profile is passed, and so shown, as --profile '': the pasted
	// command must not pick up an exported AWS_PROFILE the run did not use.
	s.Profile = ""
	if d := b.verify(s).Display; !strings.HasSuffix(d, "--profile ''") || strings.Contains(d, "AWS_PROFILE") {
		t.Errorf("empty profile display: %s", d)
	}
	if got := shellQuote("project=a auto-delete=no"); got != "'project=a auto-delete=no'" {
		t.Errorf("shellQuote = %s", got)
	}
}

// Both ways (review N2): `create rds` carries and shows the DB_* settings;
// no other step carries or shows any, the plugins step included, even with
// plugin settings among the config files' values: --plugin-action reads
// those itself (TestPluginActionReadsPluginSettingsFromTheConfigFiles), so
// WHATSAPP_PEER is in no Spec's Env or Display, and so in no log line. The
// run's TAGS still wins over a TAGS of the same name.
func TestStepSettingsReachOnlyTheirSteps(t *testing.T) {
	const peer = "+15555550100"
	b, s := testBuilder()
	b.stepEnv = []string{"DB_MULTI_AZ=true", "DB_INSTANCE_CLASS=db.r8g.large",
		"BEDROCK_INFERENCE_PROFILE=us.example.model", "EFS_ADDON_VERSION=v9.9.9-eksbuild.1",
		"OPENCLAW_GATEWAY_IMAGE=example.com/gw@sha256:00", "OPENCLAW_WORKERS=2", "WHATSAPP_PEER=" + peer, "TAGS=from-config"}
	isSetting := func(e string) bool {
		k, _, _ := strings.Cut(e, "=")
		return slices.Contains(state.StepKeys, k)
	}
	for name, spec := range allSpecs(b, s) {
		var carried []string
		for _, e := range spec.Env {
			if isSetting(e) {
				carried = append(carried, e)
			}
		}
		var want []string
		if name == "rds" {
			want = []string{"DB_MULTI_AZ=true", "DB_INSTANCE_CLASS=db.r8g.large"}
		}
		if !slices.Equal(carried, want) {
			t.Errorf("%s: carries step settings %q, want %q", name, carried, want)
		}
		for what, s := range map[string]string{"Env": strings.Join(spec.Env, " "), "Display": spec.Display} {
			if strings.Contains(s, "WHATSAPP_PEER") || strings.Contains(s, peer) {
				t.Errorf("%s: %s holds the phone number: %s", name, what, s)
			}
		}
		shows := strings.Contains(spec.Display, "DB_MULTI_AZ=true") && strings.Contains(spec.Display, "DB_INSTANCE_CLASS=db.r8g.large")
		if shows != (name == "rds") || strings.Contains(spec.Display, "BEDROCK_") || strings.Contains(spec.Display, "EFS_ADDON") || strings.Contains(spec.Display, "OPENCLAW_") {
			t.Errorf("%s: Display %s", name, spec.Display)
		}
		// exec takes the last of repeated keys: the run's TAGS must be last.
		var last string
		for _, e := range spec.Env {
			if strings.HasPrefix(e, "TAGS=") {
				last = e
			}
		}
		if last != "TAGS="+s.Tags {
			t.Errorf("%s: TAGS resolves to %q, want the run's %q", name, last, "TAGS="+s.Tags)
		}
	}
	if d := b.teardown(s).Display; d != "CLUSTER_NAME=demo AWS_REGION=us-west-2 AWS_PROFILE=eks-sandbox go -C tools/setup-aws run . teardown" {
		t.Errorf("teardown shows a step setting: %s", d)
	}
	b.stepEnv = nil
	for name, spec := range allSpecs(b, s) {
		if slices.ContainsFunc(spec.Env, isSetting) {
			t.Errorf("%s: carries a step setting with none configured: %v", name, spec.Env)
		}
	}
}
