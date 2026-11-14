package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

const tdOwner = "demo.us-west-2"

func own(extra ...string) map[string]string {
	m := map[string]string{OwnerTagKey: tdOwner, tdCreatedByKey: "setup-aws", "project": "agent-substrate", "auto-delete": "no"}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func roleARN(n string) string { return "arn:aws:iam::111122223333:role/" + n }

// fullDeployment is an account holding everything create cluster, create
// rds and bootstrap make for cluster "demo" in us-west-2, plus resources of
// others that teardown must never touch.
func fullDeployment() *tdFake {
	f := newTDFake()
	p := "demo-us-west-2-"
	// Names as create cluster makes them (clusteriam.go, launchtemplate.go).
	f.put(&tdObj{kind: "cluster", id: "demo", role: roleARN(p + "eks-cluster"), autoRole: roleARN(p + "eks-auto-node"), vpc: "vpc-1", tags: own()})
	for _, ng := range []string{"system", "gvisor", "microvm"} {
		f.put(&tdObj{kind: "nodegroup", id: ng, role: roleARN(p + "eks-node"), tags: own()})
	}
	f.put(&tdObj{kind: "assoc", id: "a-api", ns: "ate-system", sa: "ate-api-server", role: roleARN(p + "ate-api-server"), tags: own()})
	f.put(&tdObj{kind: "assoc", id: "a-atelet", ns: "ate-system", sa: "atelet", role: roleARN(p + "atelet"), tags: own()})
	f.put(&tdObj{kind: "assoc", id: "a-ebs", ns: "kube-system", sa: "ebs-csi-controller-sa", role: roleARN(p + "ebs-csi-driver"), tags: own()})
	f.put(&tdObj{kind: "assoc", id: "a-app", ns: "myapp", sa: "app", role: roleARN("someone-elses"), tags: map[string]string{}})
	f.put(&tdObj{kind: "role", id: p + "ate-api-server", inline: []string{"access"}, tags: own()})
	f.put(&tdObj{kind: "role", id: p + "atelet", inline: []string{"access"}, tags: own()})
	f.put(&tdObj{kind: "role", id: p + "eks-cluster", attached: []string{"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"}, tags: own()})
	f.put(&tdObj{kind: "role", id: p + "eks-auto-node", attached: []string{"arn:aws:iam::aws:policy/AmazonEKSWorkerNodeMinimalPolicy"}, tags: own()})
	f.put(&tdObj{kind: "role", id: p + "eks-node", profiles: []string{"eks-node-profile"},
		attached: []string{"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy", "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"}, tags: own()})
	f.put(&tdObj{kind: "role", id: p + "ebs-csi-driver", attached: []string{"arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"}, tags: own()})
	// Not this deployment's: another cluster's role with the same name
	// prefix, and the untagged role an app's association uses.
	f.put(&tdObj{kind: "role", id: "demo-2-us-west-2-node", tags: map[string]string{OwnerTagKey: "demo-2.us-west-2"}})
	f.put(&tdObj{kind: "role", id: "someone-elses", tags: map[string]string{}})
	f.put(&tdObj{kind: "db", id: "demo-pg", tags: own()})
	f.put(&tdObj{kind: "subgrp", id: "demo-pg", tags: own()})
	f.put(&tdObj{kind: "secret", id: "demo/ate-owner", tags: own()})
	f.put(&tdObj{kind: "secret", id: "demo/ate-readwrite", tags: own()})
	// The master user secret RDS manages, which create rds tags.
	f.put(&tdObj{kind: "secret", id: "rds!db-0001", tags: own("aws:rds:primaryDBInstanceArn", "arn:aws:rds:us-west-2:111122223333:db:demo-pg")})
	f.put(&tdObj{kind: "bucket", id: "demo-snap-111122223333-us-west-2", objects: 5, tags: own()})
	f.put(&tdObj{kind: "bucket", id: "demo-assets-111122223333-us-west-2", objects: 3, tags: own()})
	f.put(&tdObj{kind: "repo", id: "demo/counter", tags: own()})
	f.put(&tdObj{kind: "repo", id: "other/app", tags: map[string]string{}})
	f.put(&tdObj{kind: kLT, id: "lt-1", name: "demo-microvm-nested-virt", tags: own()})
	f.put(&tdObj{kind: kLT, id: "lt-2", name: "demo-system-nodes", tags: own()})
	f.put(&tdObj{kind: kLT, id: "lt-3", name: "demo-gvisor-nodes", tags: own()})
	f.put(&tdObj{kind: kVPC, id: "vpc-1", tags: own()})
	f.put(&tdObj{kind: kVPC, id: "vpc-default", isDefault: true, tags: map[string]string{}})
	for _, s := range []string{"subnet-pub-a", "subnet-pub-b", "subnet-priv-a", "subnet-priv-b"} {
		f.put(&tdObj{kind: kSubnet, id: s, vpc: "vpc-1", tags: own()})
	}
	f.put(&tdObj{kind: kIGW, id: "igw-1", vpc: "vpc-1", tags: own()})
	f.put(&tdObj{kind: kNAT, id: "nat-1", vpc: "vpc-1", subnet: "subnet-pub-a", alloc: "eipalloc-1", state: "available", tags: own()})
	f.put(&tdObj{kind: kEIP, id: "eipalloc-1", tags: own()})
	f.put(&tdObj{kind: kRTB, id: "rtb-main", vpc: "vpc-1", main: true, tags: own()})
	f.put(&tdObj{kind: kRTB, id: "rtb-pub", vpc: "vpc-1", tags: own()})
	f.put(&tdObj{kind: kRTB, id: "rtb-priv", vpc: "vpc-1", tags: own()})
	// create cluster tags the default security group and the one EKS makes.
	f.put(&tdObj{kind: kSG, id: "sg-default", name: "default", vpc: "vpc-1", isDefault: true, tags: own()})
	f.put(&tdObj{kind: kSG, id: "sg-pg", name: "demo-pg", vpc: "vpc-1", tags: own()})
	f.put(&tdObj{kind: kSG, id: "sg-eks", name: "eks-cluster-sg-demo-1", vpc: "vpc-1", tags: own("aws:eks:cluster-name", "demo")})
	// A network interface the VPC CNI left behind, and a node's own (gone
	// with its node group, like the node and its root volume).
	f.put(&tdObj{kind: kENI, id: "eni-left", vpc: "vpc-1", subnet: "subnet-priv-a", state: "available", tags: map[string]string{"cluster.k8s.amazonaws.com/name": "demo"}})
	f.put(&tdObj{kind: kENI, id: "eni-node", vpc: "vpc-1", subnet: "subnet-priv-a", state: "in-use", tags: own("eks:nodegroup-name", "system")})
	f.put(&tdObj{kind: kInstance, id: "i-node", vpc: "vpc-1", state: "running", tags: own("eks:nodegroup-name", "system")})
	f.put(&tdObj{kind: kVolume, id: "vol-root", state: "in-use", tags: own("eks:nodegroup-name", "system")})
	f.put(&tdObj{kind: kVolume, id: "vol-pvc", state: "available", tags: own()})
	// The tagging API keeps listing some resources a while after their
	// delete; inventory must not count those.
	for _, arn := range []string{"arn:aws:eks:us-west-2:111122223333:cluster/demo", "arn:aws:ec2:us-west-2:111122223333:vpc/vpc-1",
		"arn:aws:rds:us-west-2:111122223333:db:demo-pg", "arn:aws:s3:::demo-snap-111122223333-us-west-2",
		"arn:aws:secretsmanager:us-west-2:111122223333:secret:demo/ate-owner-AbCdEf",
		"arn:aws:ecr:us-west-2:111122223333:repository/demo/counter", "arn:aws:ec2:us-west-2:111122223333:natgateway/nat-1",
		"arn:aws:ec2:us-west-2:111122223333:network-acl/acl-default", "arn:aws:eks:us-west-2:111122223333:addon/demo/vpc-cni/x",
		"arn:aws:secretsmanager:us-west-2:111122223333:secret:rds!db-0001-QwErTy"} {
		f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String(arn),
			Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
	}
	return f
}

// tdTestEnv points newTDEnv at f, with fast polling, and captures the log.
func tdTestEnv(t *testing.T, f *tdFake, plan, keep bool) (e *tdEnv, out, logs *bytes.Buffer) {
	t.Helper()
	cfg = Config{Region: "us-west-2", ClusterName: "demo", Tags: "project=agent-substrate auto-delete=no", Plan: plan}
	prevClients, prevPoll, prevTimeout, prevLog := newTDClients, tdPoll, tdTimeout, slog.Default()
	// The core teardown runs with no plugin registered: each plugin tests
	// its own steps against its own fakes (plugin_<id>_test.go), so a plugin
	// port never reaches AWS from these tests or changes their counts.
	prevPlugins := pluginDefs
	pluginDefs = map[string]pluginDef{}
	newTDClients = func(context.Context, Config) (*tdClients, error) { return f.clients(), nil }
	tdPoll, tdTimeout = time.Millisecond, 300*time.Millisecond
	logs = &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() {
		newTDClients, tdPoll, tdTimeout = prevClients, prevPoll, prevTimeout
		pluginDefs = prevPlugins
		slog.SetDefault(prevLog)
	})
	out = &bytes.Buffer{}
	e, err := newTDEnv(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	e.keepCluster = keep
	return e, out, logs
}

func (f *tdFake) remaining() []string {
	var out []string
	for k := range f.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func idx(calls []string, call string) int {
	for i, c := range calls {
		if c == call {
			return i
		}
	}
	return -1
}

func countCalls(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

// before asserts each call happened, a before b.
func before(t *testing.T, calls []string, a, b string) {
	t.Helper()
	ia, ib := idx(calls, a), idx(calls, b)
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("want %q (at %d) before %q (at %d)", a, ia, b, ib)
	}
}

func phaseOrder(t *testing.T, logs string) {
	t.Helper()
	at := -1
	for i := range phases.Teardown {
		j := strings.Index(logs, phases.Line(phases.Teardown, i))
		if j < 0 || j < at {
			t.Errorf("phase line %q missing or out of order", phases.Line(phases.Teardown, i))
		}
		at = j
	}
}

func TestTeardownDeletesEverythingInOrder(t *testing.T) {
	f := fullDeployment()
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	want := []string{"NAT gateway|nat-1", "VPC|vpc-default", "repo|other/app", "role|demo-2-us-west-2-node", "role|someone-elses"}
	if got := f.remaining(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("left after teardown:\n got %v\nwant %v (nat-1 stays listed as deleted, as EC2 does)", got, want)
	}
	if f.objs[kNAT+"|nat-1"].state != "deleted" {
		t.Errorf("nat-1 is %s", f.objs[kNAT+"|nat-1"].state)
	}
	c := f.calls
	t.Logf("writes, in order:\n  %s", strings.Join(c, "\n  "))
	// Phase order: Substrate's data first, then the cluster, then the network.
	before(t, c, "DeletePodIdentityAssociation a-api", "DeleteRole demo-us-west-2-ate-api-server")
	before(t, c, "DeleteRole demo-us-west-2-atelet", "DeleteDBInstance demo-pg")
	before(t, c, "DeleteSecret demo/ate-readwrite", "DeleteBucket demo-snap-111122223333-us-west-2")
	before(t, c, "DeleteBucket demo-assets-111122223333-us-west-2", "DeleteRepository demo/counter")
	before(t, c, "DeleteRepository demo/counter", "DeleteNodegroup gvisor")
	// Dependencies AWS enforces.
	for _, ng := range []string{"system", "gvisor", "microvm"} {
		before(t, c, "DeleteNodegroup "+ng, "DeleteCluster demo")
	}
	before(t, c, "DeleteDBInstance demo-pg", "DeleteDBSubnetGroup demo-pg")
	before(t, c, "DeleteDBInstance demo-pg", "DeleteSecurityGroup sg-pg")
	before(t, c, "DeleteCluster demo", "DeleteRole demo-us-west-2-eks-cluster")
	before(t, c, "DeleteCluster demo", "DeleteRole demo-us-west-2-eks-auto-node")
	before(t, c, "DeleteCluster demo", "DeleteRole demo-us-west-2-ebs-csi-driver")
	for _, lt := range []string{"lt-1", "lt-2", "lt-3"} {
		before(t, c, "DeleteCluster demo", "DeleteLaunchTemplate "+lt)
	}
	before(t, c, "DeleteNatGateway nat-1", "ReleaseAddress eipalloc-1")
	before(t, c, "DetachInternetGateway igw-1", "DeleteInternetGateway igw-1")
	before(t, c, "DeleteNetworkInterface eni-left", "DeleteSubnet subnet-priv-a")
	before(t, c, "DeleteSubnet subnet-pub-a", "DeleteRouteTable rtb-pub")
	before(t, c, "DeleteRouteTable rtb-priv", "DeleteVpc vpc-1")
	before(t, c, "DeleteVolume vol-pvc", "DeleteVpc vpc-1")
	// Each of these succeeded on its first try: what holds it was gone (the
	// node groups, the cluster's ENIs, the NAT gateway) before it ran.
	for _, call := range []string{"DeleteCluster demo", "DeleteSubnet subnet-priv-a", "DeleteSubnet subnet-pub-a", "DeleteVpc vpc-1", "DeleteSecurityGroup sg-pg"} {
		if n := countCalls(c, call); n != 1 {
			t.Errorf("%s ran %d times, want 1", call, n)
		}
	}
	if c[len(c)-1] != "DeleteVpc vpc-1" {
		t.Errorf("last write is %q, want the VPC", c[len(c)-1])
	}
	for _, call := range c {
		for _, never := range []string{"someone-elses", "demo-2-", "other/app", "a-app", "vpc-default", "sg-default", "rtb-main", "sg-eks"} {
			if strings.Contains(call, never) {
				t.Errorf("touched a resource that is not its own: %s", call)
			}
		}
	}
	phaseOrder(t, logs.String())
	if !strings.Contains(logs.String(), "Teardown complete") {
		t.Errorf("no completion line:\n%s", logs)
	}
}

func TestTeardownSecondRunDeletesNothing(t *testing.T) {
	f := fullDeployment()
	e, _, _ := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("second run wrote: %v", f.calls)
	}
	if n := strings.Count(logs.String(), "nothing to delete"); n != 8 {
		t.Errorf("want 'nothing to delete' for each of the 8 delete phases (plugins first), got %d", n)
	}
	phaseOrder(t, logs.String())
}

func TestTeardownPlanDeletesNothing(t *testing.T) {
	f := fullDeployment()
	e, out, logs := tdTestEnv(t, f, true, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("--plan wrote: %v", f.calls)
	}
	// It does report what a run would delete, masked.
	for _, want := range []string{"plan: would delete EKS cluster demo", "plan: would delete NAT gateway nat-1",
		"plan: would delete S3 bucket demo-snap-********3333-us-west-2", "plan: would delete IAM role demo-us-west-2-eks-node"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("plan output lacks %q", want)
		}
	}
	if strings.Contains(logs.String()+out.String(), "111122223333") {
		t.Error("account id in the plan output")
	}
	phaseOrder(t, logs.String())
}

func TestTeardownKeepClusterLeavesTheCluster(t *testing.T) {
	f := fullDeployment()
	e, _, logs := tdTestEnv(t, f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown --keep-cluster: %v\n%s", err, logs)
	}
	for _, kept := range []string{"cluster|demo", "nodegroup|system", "nodegroup|gvisor", "nodegroup|microvm", "assoc|a-ebs",
		"role|demo-us-west-2-eks-cluster", "role|demo-us-west-2-eks-auto-node", "role|demo-us-west-2-eks-node",
		"role|demo-us-west-2-ebs-csi-driver", "launch template|lt-1", "launch template|lt-2",
		"VPC|vpc-1", "subnet|subnet-priv-a", "NAT gateway|nat-1", "Elastic IP|eipalloc-1", "internet gateway|igw-1", "EBS volume|vol-pvc"} {
		if f.objs[kept] == nil {
			t.Errorf("--keep-cluster deleted %s", kept)
		}
	}
	for _, gone := range []string{"assoc|a-api", "assoc|a-atelet", "role|demo-us-west-2-ate-api-server", "role|demo-us-west-2-atelet",
		"db|demo-pg", "subgrp|demo-pg", "security group|sg-pg", "secret|demo/ate-owner", "secret|demo/ate-readwrite",
		"bucket|demo-snap-111122223333-us-west-2", "bucket|demo-assets-111122223333-us-west-2", "repo|demo/counter"} {
		if f.objs[gone] != nil {
			t.Errorf("--keep-cluster left %s", gone)
		}
	}
	if n := strings.Count(logs.String(), "kept (--keep-cluster)"); n != 3 {
		t.Errorf("want the node group, cluster and network phases logged as kept, got %d", n)
	}
	phaseOrder(t, logs.String())
}

// withAutomatedSnapshot adds RDS's first automated backup of demo-pg, with
// the instance's tags (CopyTagsToSnapshot) and its tagging-API entry, which
// RDS removes reads reads of it after the instance is deleted.
func withAutomatedSnapshot(f *tdFake, reads int) string {
	const id = "rds:demo-pg-2026-11-14-03-27"
	f.put(&tdObj{kind: "rds snapshot", id: id, name: "demo-pg", state: "available", pending: reads, tags: own()})
	f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String("arn:aws:rds:us-west-2:111122223333:snapshot:" + id),
		Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
	return id
}

// Live on sub-r17, RDS still listed the deleted instance's automated
// snapshot as available when a keep-cluster teardown's final check ran, 30 s
// after the instance was gone. Teardown waits for RDS to remove it, so the
// check passes on the first run, with or without the cluster.
func TestTeardownWaitsForRDSToRemoveTheAutomatedSnapshots(t *testing.T) {
	for _, keep := range []bool{true, false} {
		t.Run(fmt.Sprintf("keep-cluster=%v", keep), func(t *testing.T) {
			f := fullDeployment()
			snap := withAutomatedSnapshot(f, 3)
			e, _, logs := tdTestEnv(t, f, false, keep)
			if err := e.teardown(context.Background()); err != nil {
				t.Fatalf("teardown: %v\n%s", err, logs)
			}
			if f.objs["rds snapshot|"+snap] != nil {
				t.Errorf("the automated snapshot is still there")
			}
			for _, want := range []string{"waiting for RDS to remove the automated snapshots of demo-pg",
				"RDS removed the automated snapshots of demo-pg"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log lacks %q", want)
				}
			}
			// The wait comes after the instance is gone and before the subnet
			// group, which is the next step.
			l := logs.String()
			if a, b := strings.Index(l, "waiting for RDS to remove"), strings.Index(l, "delete RDS subnet group demo-pg"); a < 0 || b < a {
				t.Errorf("the snapshot wait is not before the subnet group delete:\n%s", l)
			}
		})
	}
}

func TestTeardownGivesUpOnAnAutomatedSnapshotRDSNeverRemoves(t *testing.T) {
	f := fullDeployment()
	snap := withAutomatedSnapshot(f, 1<<30)
	e, _, logs := tdTestEnv(t, f, false, true)
	prev := dbSnapshotTimeout
	dbSnapshotTimeout = 20 * time.Millisecond
	t.Cleanup(func() { dbSnapshotTimeout = prev })
	err := e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "RDS still lists the automated snapshots "+snap+" of the deleted instance demo-pg after 20ms") {
		t.Fatalf("want the bound and the snapshot named, got %v\n%s", err, logs)
	}
}

// AC2.2: a resource with this deployment's name but no owner tag, or another
// owner's, is never deleted, and the run stops before deleting anything.
func TestTeardownStopsOnAResourceItDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(*tdFake)
		keepOK     bool // --keep-cluster does not reach it, so it does not stop that run
	}{
		{"bucket of another deployment", "S3 bucket demo-snap-111122223333-us-west-2 is not owned by demo.us-west-2",
			func(f *tdFake) {
				f.objs["bucket|demo-snap-111122223333-us-west-2"].tags = map[string]string{OwnerTagKey: "other.us-west-2"}
			}, false},
		{"untagged Substrate role", "IAM role demo-us-west-2-ate-api-server is not owned",
			func(f *tdFake) { f.objs["role|demo-us-west-2-ate-api-server"].tags = map[string]string{} }, false},
		{"database login of another owner", "secret demo/ate-owner is not owned",
			func(f *tdFake) { f.objs["secret|demo/ate-owner"].tags = map[string]string{OwnerTagKey: "x"} }, false},
		{"someone else's subnet in the VPC", "subnet subnet-x is not owned",
			func(f *tdFake) { f.put(&tdObj{kind: kSubnet, id: "subnet-x", vpc: "vpc-1", tags: map[string]string{}}) }, true},
		{"someone else's node group in the cluster", "EKS node group theirs is not owned",
			func(f *tdFake) { f.put(&tdObj{kind: "nodegroup", id: "theirs", tags: map[string]string{}}) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fullDeployment()
			tc.edit(f)
			e, _, _ := tdTestEnv(t, f, false, false)
			err := e.teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming it (%q), got %v", tc.want, err)
			}
			if len(f.calls) != 0 {
				t.Errorf("deleted before stopping: %v", f.calls)
			}
			if tc.keepOK {
				f := fullDeployment()
				tc.edit(f)
				e, _, _ := tdTestEnv(t, f, false, true)
				if err := e.teardown(context.Background()); err != nil {
					t.Errorf("--keep-cluster does not touch it, yet stopped: %v", err)
				}
			}
		})
	}
}

func TestTeardownRefusesAnEksctlCluster(t *testing.T) {
	stack := func(f *tdFake) {
		f.objs["cluster|demo"].tags = own(tdStackKey, "eksctl-demo-cluster")
	}
	f := fullDeployment()
	stack(f)
	e, _, _ := tdTestEnv(t, f, false, false)
	err := e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "EKS cluster demo belongs to CloudFormation stack eksctl-demo-cluster") ||
		!strings.Contains(err.Error(), "eksctl delete cluster --name demo --region us-west-2") ||
		strings.Contains(err.Error(), "delete-cluster.sh") {
		t.Fatalf("want a refusal pointing at eksctl delete cluster, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("deleted before refusing: %v", f.calls)
	}
	// --keep-cluster removes Substrate from it and leaves the cluster to eksctl.
	f = fullDeployment()
	stack(f)
	e, _, _ = tdTestEnv(t, f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("--keep-cluster on an eksctl cluster: %v", err)
	}
	if f.objs["cluster|demo"] == nil || f.objs["db|demo-pg"] != nil {
		t.Error("--keep-cluster should keep the eksctl cluster and delete the database")
	}
}

func TestTeardownLeavesAProtectedDatabase(t *testing.T) {
	f := fullDeployment()
	f.objs["db|demo-pg"].protect = true
	e, _, _ := tdTestEnv(t, f, false, false)
	err := e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "deletion protection") {
		t.Fatalf("want a deletion-protection refusal, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("deleted before refusing: %v", f.calls)
	}
}

// A resource deleted by someone else between the read and the delete is
// not an error: the run goes on and ends clean.
func TestTeardownResourceAlreadyGoneIsNotAnError(t *testing.T) {
	f := fullDeployment()
	for _, op := range []string{"DeletePodIdentityAssociation", "DeleteRolePolicy", "DeleteDBInstance", "DeleteSecret",
		"DeleteObjects", "DeleteRepository", "DeleteNodegroup", "DeleteNatGateway", "DeleteVolume"} {
		f.vanish[op] = true
	}
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if n := strings.Count(logs.String(), "is already gone"); n < 9 {
		t.Errorf("want each vanished resource logged as already gone, got %d", n)
	}
	// Only the resources of others are left (the vanished NAT gateway too is gone).
	if got := strings.Join(f.remaining(), ","); got != "VPC|vpc-default,repo|other/app,role|demo-2-us-west-2-node,role|someone-elses" {
		t.Errorf("left: %v", got)
	}
}

// AC2.3 on the delete side: AccessDenied stops the run, and nothing after it runs.
func TestTeardownAccessDeniedOnADeleteIsAnError(t *testing.T) {
	f := fullDeployment()
	f.deny["DeleteBucket"] = true
	e, _, _ := tdTestEnv(t, f, false, false)
	err := e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want AccessDenied, got %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DeleteRepository") || strings.HasPrefix(c, "DeleteNodegroup") || strings.HasPrefix(c, "DeleteCluster") {
			t.Errorf("kept going after a denied delete: %s", c)
		}
	}
	if f.objs["bucket|demo-snap-111122223333-us-west-2"] == nil {
		t.Error("the denied bucket is gone")
	}
}

func TestTeardownCommandMasksTheAccount(t *testing.T) {
	f := fullDeployment()
	f.objs["bucket|demo-snap-111122223333-us-west-2"].tags = map[string]string{}
	tdTestEnv(t, f, false, false)
	teardownCmd.SetContext(context.Background())
	teardownCmd.SetOut(&bytes.Buffer{})
	err := teardownCmd.RunE(teardownCmd, nil)
	if err == nil || strings.Contains(err.Error(), "111122223333") || !strings.Contains(err.Error(), "demo-snap-********3333-us-west-2") {
		t.Fatalf("want a masked refusal, got %v", err)
	}
}

func TestGoneIsOnlyNotFound(t *testing.T) {
	for code, want := range map[string]bool{
		"InvalidVpcID.NotFound": true, "InvalidLaunchTemplateName.NotFoundException": true, "DBInstanceNotFound": true,
		"DBSubnetGroupNotFoundFault": true, "ResourceNotFoundException": true, "NoSuchEntity": true, "NoSuchBucket": true,
		"AccessDenied": false, "AccessDeniedException": false, "Throttling": false, "DependencyViolation": false, "UnauthorizedOperation": false,
	} {
		if got := tdGone(apiErr(code)); got != want {
			t.Errorf("tdGone(%s) = %v, want %v", code, got, want)
		}
	}
}
