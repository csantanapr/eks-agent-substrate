package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
)

// autoNode adds an Auto Mode node of NodePool substrate-general-purpose (the
// installer's tagged NodeClass, so it carries the owner tag) in state: its
// instance, and while it is not terminated its network interface and root
// volume.
func autoNode(f *tdFake, state string) {
	tags := own("eks:eks-cluster-name", "demo")
	tags["eks:kubernetes-node-pool-name"] = "substrate-general-purpose"
	tags["eks:kubernetes-node-class-name"] = "substrate-eks"
	f.put(&tdObj{kind: kInstance, id: "i-auto", vpc: "vpc-1", state: state, tags: tags})
	if state != "terminated" {
		f.put(&tdObj{kind: kENI, id: "eni-auto", vpc: "vpc-1", subnet: "subnet-priv-a", state: "in-use", tags: tags})
		f.put(&tdObj{kind: kVolume, id: "vol-auto", state: "in-use", tags: tags})
	}
}

// Rule 1, both ways: with --keep-cluster, a running tagged Auto Mode node,
// its network interface and its volume are cluster rows, kept and not
// counted as left; the full teardown counts the same running instance as
// left.
func TestTaggedAutoModeNodesAreClusterRows(t *testing.T) {
	f := fullDeployment()
	autoNode(f, "running")
	e, _, keepLogs := tdTestEnv(t, f, false, true)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"i-auto", "eni-auto", "vol-auto"} {
		found := false
		for _, r := range rows {
			if r.ID == id {
				found = true
				if !r.Cluster {
					t.Errorf("%s %s is not a cluster row", r.Kind, id)
				}
			}
		}
		if !found {
			t.Errorf("inventory does not list %s", id)
		}
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown --keep-cluster: %v\n%s", err, keepLogs)
	}
	for _, kept := range []string{kInstance + "|i-auto", kENI + "|eni-auto", kVolume + "|vol-auto"} {
		if f.objs[kept] == nil {
			t.Errorf("--keep-cluster deleted %s", kept)
		}
	}

	// The full teardown: an instance still running at the end is left.
	f = fullDeployment()
	// No VPC link: the fake would hold the VPC for it, which only a test of
	// the VPC delete needs.
	f.put(&tdObj{kind: kInstance, id: "i-auto", state: "running", tags: own("eks:kubernetes-node-pool-name", "substrate-general-purpose")})
	e, out, logs := tdTestEnv(t, f, false, false)
	err = e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 resources of demo.us-west-2 are left") || !strings.Contains(out.String()+logs.String(), "i-auto") {
		t.Errorf("a running tagged node after the teardown: %v\n%s", err, logs)
	}
}

// Rule 2, both ways: after the full teardown a tagged Auto Mode instance
// that EC2 reports terminated, and that the tagging API still lists with its
// network interface and volume, is not left; the same instance reported
// shutting-down is.
func TestTerminatedAutoModeNodesListedByTaggingDoNotFailTheTeardown(t *testing.T) {
	for _, tc := range []struct {
		state string
		left  bool
	}{{"terminated", false}, {"shutting-down", true}} {
		f := fullDeployment()
		f.put(&tdObj{kind: kInstance, id: "i-auto", state: tc.state, tags: own("eks:kubernetes-node-pool-name", "substrate-general-purpose")})
		for _, arn := range []string{"arn:aws:ec2:us-west-2:111122223333:instance/i-auto",
			"arn:aws:ec2:us-west-2:111122223333:network-interface/eni-auto", "arn:aws:ec2:us-west-2:111122223333:volume/vol-auto"} {
			f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String(arn),
				Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
		}
		e, _, logs := tdTestEnv(t, f, false, false)
		err := e.teardown(context.Background())
		if tc.left != (err != nil) {
			t.Errorf("%s instance: teardown err %v\n%s", tc.state, err, logs)
		}
		if err != nil && !strings.Contains(err.Error(), "1 resources of demo.us-west-2 are left") {
			t.Errorf("%s instance: %v", tc.state, err)
		}
	}
}

// autoFleet adds an instant fleet Auto Mode left for a tagged NodeClass (the
// tagging API lists it; EC2 answers for it by ID), in fleet state, with its
// instance in instState ("" for no instance object, as EC2 forgets one).
func autoFleet(f *tdFake, id, state, instState string) {
	inst := "i-" + id
	f.put(&tdObj{kind: "fleet", id: id, name: inst, state: state, tags: own()})
	if instState != "" {
		f.put(&tdObj{kind: kInstance, id: inst, state: instState, tags: own("eks:kubernetes-node-pool-name", "substrate-general-purpose")})
	}
	f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String("arn:aws:ec2:us-west-2:111122223333:fleet/" + id),
		Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
}

func fleetRows(t *testing.T, e *tdEnv) []tdRow {
	t.Helper()
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []tdRow
	for _, r := range rows {
		if r.Kind == kFleet {
			out = append(out, r)
		}
	}
	return out
}

// Both directions for Auto Mode's instant fleets, which only EKS may delete:
// a fleet whose instance runs is a cluster row (kept by --keep-cluster, left
// after a full teardown); one whose instance is terminated or forgotten, or
// that is deleted or unknown to EC2, is no row, so the full teardown ends
// with nothing left.
func TestAutoModeFleetsCountOnlyWhileTheirInstanceRuns(t *testing.T) {
	f := fullDeployment()
	autoFleet(f, "fleet-run", "active", "running")
	autoFleet(f, "fleet-done", "active", "terminated")
	autoFleet(f, "fleet-forgotten", "active", "")
	autoFleet(f, "fleet-deleted", "deleted", "running")
	f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String("arn:aws:ec2:us-west-2:111122223333:fleet/fleet-unknown"),
		Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
	e, _, logs := tdTestEnv(t, f, false, true)
	if rows := fleetRows(t, e); len(rows) != 1 || rows[0].ID != "fleet-run" || !rows[0].Cluster {
		t.Errorf("fleet rows %+v, want fleet-run as a cluster row", rows)
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown --keep-cluster: %v\n%s", err, logs)
	}

	// Full teardown: once the cluster has terminated the instance, nothing
	// is left; while it still runs, its fleet and instance are.
	for _, tc := range []struct {
		inst string
		left bool
	}{{"terminated", false}, {"running", true}} {
		f := fullDeployment()
		autoFleet(f, "fleet-a", "active", tc.inst)
		e, _, logs := tdTestEnv(t, f, false, false)
		err := e.teardown(context.Background())
		if tc.left != (err != nil) {
			t.Errorf("instance %s: teardown err %v\n%s", tc.inst, err, logs)
		}
		if tc.left && (err == nil || !strings.Contains(err.Error(), "2 resources of demo.us-west-2 are left")) {
			t.Errorf("instance %s: %v", tc.inst, err)
		}
	}
}
