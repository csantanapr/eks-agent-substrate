package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
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
			// --sandbox-classes always: a child's default is SANDBOX_CLASSES
			// from the config files, which Settings may have changed.
			if (i >= 0) != (custom && release) || (j >= 0) != (release && name != "plugins") {
				t.Errorf("custom=%v %s: argv %q", custom, name, spec.Argv)
				continue
			}
			if i >= 0 && spec.Argv[i+1] != s.ImageRepo || j >= 0 && spec.Argv[j+1] != s.SandboxClasses {
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

// Both ways: with mode3 or mode3-iso chosen (alone or together), every step
// that installs or uses the release (the control plane, the demo, its check
// and the plugins) carries the fork's release from the first of them, and the
// control plane step says so; without them, each carries v1.0.0. The control plane removal is left out: it reads the
// release of the install it removes off the cluster's record (the reinstall
// guard runs it on a found install, which the chosen plugins say nothing about).
func TestEveryStepCarriesTheForkReleaseWithMode3(t *testing.T) {
	for _, chosen := range [][]string{{"mode3", "efs", "credprovider-aws"}, {"mode3-iso"}, {"mode3", "mode3-iso"}, {"efs", "credprovider-aws"}} {
		b, s := testBuilder()
		s.Plugins = chosen
		fork := slices.Contains(chosen, mode3.ID) || slices.Contains(chosen, "mode3-iso")
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

// create cluster gets, shown and passed, the node groups of the run's
// classes, database and plugins: the default three, unchanged; with RDS,
// system and gvisor for the gVisor class alone, system and microvm for
// mode 3 with the AWS Nitro sandbox; none for the Nitro class alone on Auto
// Mode (the minimal profile); with the bundled PostgreSQL, gvisor alone for
// the gVisor class, the system group too with the LLM gateway, which runs
// on it, and microvm alone for mode 3 with the Nitro sandbox. With the Nitro class the control plane carries mode3-iso and
// gets ate-setup's gvisor class only, its own step deploys mode3-iso, and
// the plugins step deploys the chosen plugins alone.
func TestCreateClusterGetsTheNodeGroupsOfTheSelection(t *testing.T) {
	for _, tc := range []struct {
		database string
		classes  string
		plugins  []string
		want     string
	}{
		{state.DatabaseRDS, state.DefaultSandboxClasses, nil, "system,gvisor,microvm"},
		{state.DatabaseRDS, "gvisor", nil, "system,gvisor"},
		{state.DatabaseRDS, "nitro", []string{mode3.ID}, "system,microvm"},
		{state.DatabaseRDS, "nitro", nil, "none"},
		{state.DatabaseBundled, "gvisor", nil, "gvisor"},
		{state.DatabaseBundled, "gvisor", []string{"llm-gateway"}, "system,gvisor"},
		{state.DatabaseBundled, "nitro", []string{mode3.ID}, "microvm"},
	} {
		b, s := testBuilder()
		s.Database, s.SandboxClasses, s.Plugins = tc.database, tc.classes, tc.plugins
		s.NewCluster("demo")
		spec := b.createCluster(s)
		if !slices.Contains(spec.Env, "NODE_GROUPS="+tc.want) || !strings.HasPrefix(spec.Display, "NODE_GROUPS="+tc.want+" ") {
			t.Errorf("%s %s %v: env %q, shown %q; want NODE_GROUPS=%s", tc.database, tc.classes, tc.plugins, spec.Env, spec.Display, tc.want)
		}
	}
	b, s := testBuilder()
	s.SandboxClasses, s.Plugins = "nitro", []string{"efs"}
	cp := b.controlPlane(s)
	if v := cp.Argv[slices.Index(cp.Argv, "--plugins")+1]; v != "efs,mode3-iso" || !strings.Contains(cp.Display, ", gvisor, database") ||
		slices.Contains(cp.SimLines, "[step]: prepare_microvm_deps") {
		t.Errorf("control plane: --plugins %s, shown %q", v, cp.Display)
	}
	if n := b.nitro(s); n.Argv[slices.Index(n.Argv, "--plugins")+1] != "mode3-iso" || !slices.Contains(n.Argv, "--plugin-action") {
		t.Errorf("nitro step argv %q", n.Argv)
	}
	if p := b.plugins(s); p.Argv[slices.Index(p.Argv, "--plugins")+1] != "efs" {
		t.Errorf("plugins step argv %q", p.Argv)
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

// NODE_AMI_ID reaches setup-aws, and the shown command carries it only
// when it is set: the default command is unchanged.
func TestCreateClusterShowsACustomAMIOnly(t *testing.T) {
	b, s := testBuilder()
	sp := b.createCluster(s)
	if strings.Contains(sp.Display, "NODE_AMI_ID") || !slices.Contains(sp.Env, "NODE_AMI_ID=") {
		t.Errorf("default: shown %q, env %q", sp.Display, sp.Env)
	}
	s.NewClusterConfig.NodeAMIID = "ami-0123456789abcdef0"
	sp = b.createCluster(s)
	if !strings.Contains(sp.Display, "NODE_AMI_ID=ami-0123456789abcdef0 ") || !slices.Contains(sp.Env, "NODE_AMI_ID=ami-0123456789abcdef0") {
		t.Errorf("custom AMI: shown %q, env %q", sp.Display, sp.Env)
	}
}

// NODE_GROUP_TYPE reaches setup-aws; the shown command carries it only
// when it is not the default, and the dry-run replays the type's phases.
func TestCreateClusterShowsSelfManagedOnly(t *testing.T) {
	b, s := testBuilder()
	sp := b.createCluster(s)
	if strings.Contains(sp.Display, "NODE_GROUP_TYPE") || !slices.Contains(sp.Env, "NODE_GROUP_TYPE=managed") ||
		!slices.Contains(sp.SimLines, fmt.Sprintf("level=INFO msg=%q", phases.Line(phases.Cluster, 7))) {
		t.Errorf("default: shown %q, env %q, sim %q", sp.Display, sp.Env, sp.SimLines)
	}
	s.NewClusterConfig.NodeGroupType = "self-managed"
	sp = b.createCluster(s)
	if !strings.Contains(sp.Display, "NODE_GROUP_TYPE=self-managed ") || !slices.Contains(sp.Env, "NODE_GROUP_TYPE=self-managed") {
		t.Errorf("self-managed: shown %q, env %q", sp.Display, sp.Env)
	}
	for i := range phases.ClusterSelfManaged {
		if !slices.Contains(sp.SimLines, fmt.Sprintf("level=INFO msg=%q", phases.Line(phases.ClusterSelfManaged, i))) {
			t.Errorf("the dry-run lacks %q", phases.Line(phases.ClusterSelfManaged, i))
		}
	}
	// Empty is no setting of its own either (setup-aws takes it as
	// managed): the environment carries it once, as every knob.
	s.NewClusterConfig.NodeGroupType = ""
	sp = b.createCluster(s)
	n := 0
	for _, e := range sp.Env {
		if strings.HasPrefix(e, "NODE_GROUP_TYPE=") {
			n++
		}
	}
	if n != 1 || strings.Contains(sp.Display, "NODE_GROUP_TYPE") {
		t.Errorf("empty: %d NODE_GROUP_TYPE entries in %q, shown %q", n, sp.Env, sp.Display)
	}
}

// KARPENTER=on reaches setup-aws and is shown, as a knob a pasted command
// needs; the dry run replays Karpenter's two phases after the type's, both
// node group types alike. Without it nothing is shown, the environment
// carries it empty, and the phases are the type's (TestDefaultSpecsUnchanged).
func TestCreateClusterShowsKarpenterOnly(t *testing.T) {
	b, s := testBuilder()
	s.NewClusterConfig.AutoMode, s.NewClusterConfig.Karpenter = "", ""
	s.NewCluster("demo")
	sp := b.createCluster(s)
	if strings.Contains(sp.Display, "KARPENTER") || !slices.Contains(sp.Env, "KARPENTER=") || strings.Contains(strings.Join(sp.SimLines, "\n"), "Karpenter") {
		t.Errorf("off: shown %q, env %q", sp.Display, sp.Env)
	}
	for _, typ := range []string{"managed", "self-managed"} {
		s.NewClusterConfig.NodeGroupType, s.NewClusterConfig.Karpenter = typ, "on"
		sp = b.createCluster(s)
		if !strings.HasPrefix(sp.Display, "NODE_GROUPS=system,gvisor,microvm ") || !strings.Contains(sp.Display, " KARPENTER=on CLUSTER_NAME=demo ") || !slices.Contains(sp.Env, "KARPENTER=on") {
			t.Errorf("%s: shown %q, env %q", typ, sp.Display, sp.Env)
		}
		list := phases.ClusterFor(typ, true)
		items, active, lit := steps.FromPhases(list), -1, 0
		for _, line := range sp.SimLines {
			if next := steps.Progress(items, active, line); next != active {
				active, lit = next, lit+1
			}
		}
		if lit != 11 || !slices.Contains(sp.SimLines, `level=INFO msg="Step 11/11: Installing Karpenter and its NodePools..."`) {
			t.Errorf("%s: lit %d of %d rows: %q", typ, lit, len(items), sp.SimLines)
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

// AC8.5 for Remove Substrate: the dry-run replay lights every row of each
// checklist the wizard gives the step, one at a time and in order: the
// reinstall guard's, with a row per plugin its probe answer records
// (efs and llm-gateway on substrate-installed); the teardown flow's, with
// one row for the plugins, with and without plugins in the run; and one
// whose record lists none.
func TestDryRunLinesWalkEveryRemovalRow(t *testing.T) {
	guard, err := probe.ParseInstalled([]string{probe.SimLine("substrate-installed")})
	if err != nil || len(guard.Plugins) == 0 {
		t.Fatalf("the dry run's installed cluster records %v (%v)", guard.Plugins, err)
	}
	for _, tc := range []struct {
		name, cluster string
		plugins       []string
		items         []steps.ChecklistItem
		rows          int
	}{
		{"guard", "substrate-installed", nil, steps.RemoveControlPlane(guard.Plugins, guard.PluginsRead), len(guard.Plugins) + 2},
		{"teardown with plugins", "my-substrate", []string{"llm-gateway", "efs"}, steps.RemoveControlPlane(nil, false), 3},
		{"teardown without plugins", "my-substrate", nil, steps.RemoveControlPlane(nil, false), 3},
		{"none recorded", "my-substrate", nil, steps.RemoveControlPlane(nil, true), 2},
	} {
		b, s := testBuilder()
		s.ClusterName, s.Plugins, s.SandboxClasses = tc.cluster, tc.plugins, "gvisor"
		if len(tc.items) != tc.rows {
			t.Fatalf("%s: rows %d, want %d", tc.name, len(tc.items), tc.rows)
		}
		active, lit := -1, 0
		for _, line := range b.removeControlPlane(s).SimLines {
			if next := steps.Progress(tc.items, active, line); next != active {
				if next != active+1 {
					t.Errorf("%s: %q jumped from row %d to %d", tc.name, line, active, next)
				}
				active, lit = next, lit+1
			}
		}
		if lit != len(tc.items) {
			t.Errorf("%s: lit %d of %d rows", tc.name, lit, len(tc.items))
		}
	}
}

// Issue #22: the dry run lists a record-only cluster, and its teardown
// story removes what the record lists and what the run installs, once
// each; an installed cluster's story is its record, as before.
func TestDryRunRecordOnlyCluster(t *testing.T) {
	n := 0
	for _, c := range simClusters("us-west-2") {
		if r, err := probe.ParseInstalled([]string{probe.SimLine(c.Name)}); err == nil && r.RecordOnly() {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d record-only dry-run clusters, want 1", n)
	}
	_, s := testBuilder()
	s.ClusterName, s.Plugins, s.SandboxClasses = "nitro-leftover", []string{"llm-gateway"}, "gvisor"
	if got := strings.Join(simRecord(s), ","); got != "efs,llm-gateway,mode3-iso" {
		t.Errorf("record-only: %s", got)
	}
	s.ClusterName = "substrate-installed"
	if got := strings.Join(simRecord(s), ","); got != "efs,llm-gateway" {
		t.Errorf("installed: %s", got)
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

// Both ways: from a release (its setup-aws binary beside config.env and the
// executable), every setup-aws step runs that binary with the same
// arguments and environment, shown as the binary; from a source build, the
// steps run `go -C <tools/setup-aws> run .` from the checkout, as they
// always did, even with a setup-aws binary built beside the executable
// into native/bin, which may be stale.
func TestSetupAWSStepsRunTheSiblingBinaryOrGoRun(t *testing.T) {
	release := t.TempDir()
	bin := filepath.Join(release, "setup-aws")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "config.env"), []byte("SUBSTRATE_TAG=v2.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	co := t.TempDir()
	src := filepath.Join(co, "native", "tools", "setup-aws")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(co, "native", "bin", "setup-aws")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, s := testBuilder()
	for _, tc := range []struct {
		name      string
		b         builder
		argv0     []string
		displayed string
	}{
		{"release", builder{self: filepath.Join(release, "substrate-eks"), cwd: release, exeDir: release},
			[]string{bin}, "./setup-aws"},
		{"checkout", builder{self: filepath.Join(co, "native", "bin", "substrate-eks"), cwd: filepath.Join(co, "native"), exeDir: filepath.Join(co, "native", "bin")},
			[]string{"go", "-C", src, "run", "."}, "go -C tools/setup-aws run ."},
	} {
		specs := map[string]struct {
			spec execx.Spec
			args []string
		}{
			"create cluster": {tc.b.createCluster(s), []string{"create", "cluster"}},
			"bootstrap":      {tc.b.bootstrap(s), []string{"bootstrap"}},
			"rds":            {tc.b.rds(s), []string{"create", "rds"}},
			"teardown":       {tc.b.teardown(s), []string{"teardown"}},
			"inventory":      {tc.b.inventory(s), []string{"inventory"}},
		}
		for name, c := range specs {
			if want := append(slices.Clone(tc.argv0), c.args...); !slices.Equal(c.spec.Argv, want) {
				t.Errorf("%s %s: argv %q, want %q", tc.name, name, c.spec.Argv, want)
			}
			if want := tc.displayed + " " + strings.Join(c.args, " "); !strings.HasSuffix(c.spec.Display, want) {
				t.Errorf("%s %s: display %q, want it to end %q", tc.name, name, c.spec.Display, want)
			}
			if !slices.Contains(c.spec.Env, "PLAN=false") || !slices.Contains(c.spec.Env, "CLUSTER_NAME=demo") {
				t.Errorf("%s %s: env %q", tc.name, name, c.spec.Env)
			}
		}
	}
}

// NITRO_COMPUTE: the default adds --nitro-compute to no step and the nitro
// knobs to no shown command; node-group reaches every child that deploys,
// checks or removes the Nitro sandbox's plugin, and only those, and a new
// cluster's command shows the nitro group and its AMI. An exported
// NITRO_COMPUTE that differs from the run's is overridden.
func TestNitroComputeReachesThePluginChildrenOnly(t *testing.T) {
	takes := map[string]bool{"control plane": true, "demo": true, "verify": true, "remove": true, "plugins": true, "nitro": true}
	specs := func(s *state.Setup) map[string]execx.Spec {
		b, _ := testBuilder()
		m := allSpecs(b, s)
		m["nitro"] = b.nitro(s)
		return m
	}
	// The default, with NITRO_COMPUTE unset, exported empty (which the run
	// and its children read as unset), or exported as the default.
	for _, env := range []string{"unset", "", plugin.NitroAutoMode} {
		t.Setenv("NITRO_COMPUTE", env)
		if env == "unset" {
			os.Unsetenv("NITRO_COMPUTE")
		}
		for _, classes := range []string{state.DefaultSandboxClasses, "gvisor,nitro", "nitro"} {
			_, s := testBuilder()
			s.SandboxClasses = classes
			s.NewCluster("demo")
			for name, sp := range specs(s) {
				if slices.Contains(sp.Argv, "--nitro-compute") || strings.Contains(sp.Display, "NITRO_") {
					t.Errorf("default, NITRO_COMPUTE %q, %s: %s: argv %q, shown %q", env, classes, name, sp.Argv, sp.Display)
				}
			}
		}
	}
	t.Setenv("NITRO_COMPUTE", "")
	os.Unsetenv("NITRO_COMPUTE")

	_, s := testBuilder()
	s.SandboxClasses = "gvisor,microvm,nitro"
	s.NewClusterConfig.NitroCompute, s.NewClusterConfig.NitroAMIID = plugin.NitroNodeGroup, "ami-0a17000000000000e"
	s.NewCluster("demo")
	for name, sp := range specs(s) {
		i := slices.Index(sp.Argv, "--nitro-compute")
		if (i >= 0) != takes[name] || i >= 0 && sp.Argv[i+1] != plugin.NitroNodeGroup {
			t.Errorf("node-group: %s: argv %q", name, sp.Argv)
		}
	}
	cc := specs(s)["create cluster"]
	if !strings.HasPrefix(cc.Display, "NODE_GROUPS=system,gvisor,microvm,nitro NITRO_AMI_ID=ami-0a17000000000000e ") ||
		!slices.Contains(cc.Env, "NITRO_AMI_ID=ami-0a17000000000000e") || !slices.Contains(cc.Env, "NITRO_INSTANCE_TYPE=m8i.xlarge") ||
		!slices.Contains(cc.Env, "NITRO_NODES=1") {
		t.Errorf("create cluster: shown %q, env %q", cc.Display, cc.Env)
	}

	t.Setenv("NITRO_COMPUTE", plugin.NitroNodeGroup)
	_, s = testBuilder()
	s.SandboxClasses = "nitro"
	s.NewClusterConfig.NitroCompute = plugin.NitroAutoMode
	if n := specs(s)["nitro"]; !strings.Contains(strings.Join(n.Argv, " "), "--nitro-compute auto-mode") {
		t.Errorf("an exported node-group under the run's auto-mode: argv %q", n.Argv)
	}
}
