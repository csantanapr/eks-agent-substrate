package cmd

import (
	"context"
	"strings"
	"testing"
)

// stepENIs, both ways for each rule: an available interface this account
// manages (owner-tagged, the CNI's, a Lambda function's once detached) is
// deleted; one an AWS service manages (requester-managed, even available, or
// a Lambda function's in use) is waited for and never deleted; a node's in
// use, one without this deployment's tags, and one in a VPC it does not own
// are left alone.
func TestStepENIsDeletesOrWaitsByWhoManagesTheInterface(t *testing.T) {
	f := newTDFake()
	f.put(&tdObj{kind: kVPC, id: "vpc-1", tags: own()})
	f.put(&tdObj{kind: kVPC, id: "vpc-x", tags: map[string]string{}})
	eni := func(id, state, ifType, desc string, requester bool, tags map[string]string) {
		f.put(&tdObj{kind: kENI, id: id, vpc: "vpc-1", state: state, ifType: ifType, desc: desc, requester: requester, tags: tags})
	}
	eni("eni-avail", "available", "interface", "", false, own())
	eni("eni-cni", "available", "interface", "aws-K8S-i-1", false, map[string]string{"cluster.k8s.amazonaws.com/name": "demo"})
	eni("eni-fn-free", "available", "lambda", "AWS Lambda VPC ENI-demo-db-owner", false, own())
	eni("eni-eks", "in-use", "interface", "Amazon EKS demo", true, own())
	eni("eni-rds", "available", "interface", "RDSNetworkInterface", true, own())
	eni("eni-fn-busy", "in-use", "lambda", "AWS Lambda VPC ENI-demo-db-owner", false, own())
	eni("eni-node", "in-use", "interface", "", false, own())
	eni("eni-foreign", "available", "interface", "", false, map[string]string{})
	f.put(&tdObj{kind: kENI, id: "eni-x", vpc: "vpc-x", state: "available", tags: own()})
	e, _, _ := tdTestEnv(t, f, false, false)
	ts, rs, err := e.stepENIs(context.Background())
	if err != nil || len(rs) != 0 {
		t.Fatalf("err %v, refusals %v", err, rs)
	}
	want := map[string]string{"eni-avail": "delete", "eni-cni": "delete", "eni-fn-free": "delete",
		"eni-eks": "wait", "eni-rds": "wait", "eni-fn-busy": "wait"}
	got := map[string]string{}
	for _, x := range ts {
		id, _, _ := strings.Cut(x.id, " ")
		switch {
		case x.del != nil && x.wait == nil:
			got[id] = "delete"
		case x.del == nil && x.wait != nil:
			got[id] = "wait"
		default:
			got[id] = "both or neither"
		}
	}
	if len(got) != len(want) {
		t.Errorf("targets %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %q, want %q", id, got[id], w)
		}
	}
}

// serviceENIs adds what AWS made for the deployment in vpc-1: the cluster's
// control-plane interface, which EKS removes a few reads after the cluster,
// and the RDS instance's, which holds the RDS security group and goes a few
// reads after the instance.
func serviceENIs(f *tdFake, eksReads int) {
	f.put(&tdObj{kind: kENI, id: "eni-eks", vpc: "vpc-1", subnet: "subnet-priv-b", state: "in-use", requester: true,
		ifType: "interface", desc: "Amazon EKS demo", parent: "cluster|demo", pending: eksReads, tags: own()})
	f.put(&tdObj{kind: kENI, id: "eni-rds", vpc: "vpc-1", subnet: "subnet-priv-b", state: "in-use", requester: true,
		ifType: "interface", desc: "RDSNetworkInterface", group: "sg-pg", parent: "db|demo-pg", pending: 3, tags: own()})
}

// Both ways: an owner-tagged interface an AWS service still holds after its
// parent is gone is waited for, never deleted, and the teardown passes on its
// first run; one AWS never removes fails the run once tdTimeout has passed,
// naming it.
func TestTeardownWaitsForAServiceInterfaceToGoWithItsParent(t *testing.T) {
	f := fullDeployment()
	serviceENIs(f, 3)
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	for _, id := range []string{"eni-eks", "eni-rds"} {
		if countCalls(f.calls, "DeleteNetworkInterface "+id) != 0 {
			t.Errorf("deleted %s, which AWS manages", id)
		}
		if f.objs[kENI+"|"+id] != nil {
			t.Errorf("%s is left", id)
		}
	}
	if want := "VPC|vpc-default,repo|other/app,role|demo-2-us-west-2-node,role|someone-elses"; !strings.Contains(strings.Join(f.remaining(), ","), want) {
		t.Errorf("left: %v", f.remaining())
	}
	if !strings.Contains(logs.String(), "waiting for network interface eni-eks (Amazon EKS demo) to be deleted") {
		t.Errorf("no wait logged:\n%s", logs)
	}

	f = fullDeployment()
	serviceENIs(f, 1<<30)
	e, _, _ = tdTestEnv(t, f, false, false)
	err := e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wait for network interface eni-eks (Amazon EKS demo)") || !strings.Contains(err.Error(), "gave up after") {
		t.Fatalf("want the bound and the interface named, got %v", err)
	}
	if countCalls(f.calls, "DeleteNetworkInterface eni-eks") != 0 {
		t.Error("deleted the interface AWS still holds")
	}
}

// --keep-cluster, both ways: the cluster's interfaces (the control plane's,
// the NAT gateway's, a node's) are cluster rows, kept; the RDS instance's,
// which AWS still holds for a few reads after the instance, goes before the
// final check, which passes on the first run. A mount target's interface
// still there is Substrate data, and the final check names it.
func TestKeepClusterCountsDataInterfacesAsSubstrateData(t *testing.T) {
	f := fullDeployment()
	serviceENIs(f, 0)
	f.put(&tdObj{kind: kENI, id: "eni-nat", vpc: "vpc-1", subnet: "subnet-pub-a", state: "in-use", requester: true,
		ifType: "nat_gateway", desc: "Interface for NAT Gateway nat-1", tags: own()})
	e, _, logs := tdTestEnv(t, f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown --keep-cluster: %v\n%s", err, logs)
	}
	if f.objs[kENI+"|eni-rds"] != nil {
		t.Error("the RDS instance's interface is left")
	}
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cluster := map[string]bool{}
	for _, r := range rows {
		if r.Kind == kENI {
			cluster[r.ID] = r.Cluster
		}
	}
	if len(cluster) != 3 || !cluster["eni-eks"] || !cluster["eni-nat"] || !cluster["eni-node"] {
		t.Errorf("interface rows (ID -> cluster) %v, want eni-eks, eni-nat and eni-node as the cluster's", cluster)
	}

	f = fullDeployment()
	f.put(&tdObj{kind: kENI, id: "eni-efs", vpc: "vpc-1", subnet: "subnet-priv-a", state: "in-use", requester: true,
		ifType: "efs", desc: "EFS mount target for fs-1 (fsmt-1)", tags: own()})
	e, out, _ := tdTestEnv(t, f, false, true)
	err = e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 resources of demo.us-west-2 are left") || !strings.Contains(out.String(), "network interface  eni-efs") {
		t.Fatalf("want the final check to name eni-efs, got %v\n%s", err, out)
	}
}

// dataENI, both ways, on the interfaces seen on sub-e2e.
func TestDataENIIsWhatAServiceMadeForSubstrateData(t *testing.T) {
	for _, tc := range []struct {
		it   ec2Item
		want bool
	}{
		{ec2Item{ifType: "interface", desc: "RDSNetworkInterface", requester: true}, true},
		{ec2Item{ifType: "efs", desc: "EFS mount target for fs-1 (fsmt-1)", requester: true}, true},
		{ec2Item{ifType: "efs", desc: "S3 Files mount target for fs-1 (fsmt-1)", requester: true}, true},
		{ec2Item{ifType: "lambda", desc: "AWS Lambda VPC ENI-demo-db-owner"}, true},
		{ec2Item{ifType: "interface", desc: "Amazon EKS demo", requester: true}, false},
		{ec2Item{ifType: "nat_gateway", desc: "Interface for NAT Gateway nat-1", requester: true}, false},
		{ec2Item{ifType: "interface", desc: "aws-K8S-i-1"}, false},
		{ec2Item{ifType: "interface"}, false},
	} {
		if got := dataENI(tc.it); got != tc.want {
			t.Errorf("dataENI(%+v) = %v, want %v", tc.it, got, tc.want)
		}
	}
}
