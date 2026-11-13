package main

import (
	"slices"
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
		"probe": b.probe("other", s),
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
	s.Profile = ""
	if d := b.verify(s).Display; strings.Contains(d, "--profile") || strings.Contains(d, "AWS_PROFILE") {
		t.Errorf("empty profile shown: %s", d)
	}
	if got := shellQuote("project=a auto-delete=no"); got != "'project=a auto-delete=no'" {
		t.Errorf("shellQuote = %s", got)
	}
}
