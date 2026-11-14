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
