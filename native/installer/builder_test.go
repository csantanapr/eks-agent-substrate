package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
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
// release: --image-repo and --sandbox-classes on the control plane, demo and
// verify steps when they are not the defaults, and on no other step; the
// defaults add nothing, so the shown command stays short (both directions).
func TestReleaseFlagsReachTheReleaseSteps(t *testing.T) {
	for _, custom := range []bool{false, true} {
		b, s := testBuilder()
		if custom {
			s.ImageRepo, s.SandboxClasses = "registry.example.com/substrate", "gvisor"
		}
		for name, spec := range allSpecs(b, s) {
			wants := custom && (name == "control plane" || name == "demo" || name == "verify")
			i := slices.Index(spec.Argv, "--image-repo")
			j := slices.Index(spec.Argv, "--sandbox-classes")
			if (i >= 0) != wants || (j >= 0) != wants {
				t.Errorf("custom=%v %s: argv %q", custom, name, spec.Argv)
				continue
			}
			if wants && (spec.Argv[i+1] != s.ImageRepo || spec.Argv[j+1] != "gvisor") {
				t.Errorf("%s: argv %q", name, spec.Argv)
			}
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
