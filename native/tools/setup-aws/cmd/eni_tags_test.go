package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Both directions: the CNI's tags are the owner tag and TAGS without the keys
// the CNI ignores or AWS and EKS reserve.
func TestCNITagsDropReservedKeys(t *testing.T) {
	got := cniTags(map[string]string{OwnerTagKey: "demo.us-west-2", "auto-delete": "no",
		"cluster.k8s.amazonaws.com/name": "x", "aws:cloudformation:stack-name": "s", "eks:x": "1",
		"kubernetes.io/cluster/demo": "owned", "karpenter.sh/nodepool": "p"})
	if len(got) != 2 || got[OwnerTagKey] != "demo.us-west-2" || got["auto-delete"] != "no" {
		t.Errorf("cniTags = %v", got)
	}
}

func vpcCNIEnv(t *testing.T, f *clusterFake) map[string]any {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal([]byte(aws.ToString(f.addons["vpc-cni"].ConfigurationValues)), &c); err != nil {
		t.Fatal(err)
	}
	env, _ := c["env"].(map[string]any)
	return env
}

// A rerun on a cluster whose vpc-cni already has an env object updates the
// add-on once, keeping its other env keys, and a second rerun writes
// nothing. On the old reconcile (have[k] != v) the nested env maps made
// this panic: "comparing uncomparable type map[string]interface {}".
func TestVPCCNIConfigIsReconciledOnARerun(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.addons["vpc-cni"].ConfigurationValues = aws.String(`{"enableNetworkPolicy":"true","env":{"WARM_IP_TARGET":"5"}}`)
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countCalls(f.writes, "UpdateAddon vpc-cni") != 1 {
		t.Errorf("writes %v", f.writes)
	}
	env := vpcCNIEnv(t, f)
	if env["WARM_IP_TARGET"] != "5" || env[cniTagsEnv] != `{"agent-substrate-on-eks/deployment":"demo.us-west-2","auto-delete":"no","project":"agent-substrate"}` {
		t.Errorf("env = %v", env)
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("matching config: err %v, writes %v", err, f.writes)
	}
}

// Both directions for mergeConfig: a different nested value changes, an
// equal one (by value) does not.
func TestMergeConfigComparesByValue(t *testing.T) {
	have := map[string]any{"a": "1", "env": map[string]any{"X": "1", "Y": "2"}}
	if mergeConfig(have, map[string]any{"a": "1", "env": map[string]any{"X": "1"}}) {
		t.Error("an equal config changed")
	}
	if !mergeConfig(have, map[string]any{"env": map[string]any{"X": "9"}}) || have["env"].(map[string]any)["X"] != "9" || have["env"].(map[string]any)["Y"] != "2" {
		t.Errorf("merged = %v", have)
	}
}

// logs captures slog for one test.
func logs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var b bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &b
}

func addENI(f *clusterFake, id, vpc, desc string, typ ec2types.NetworkInterfaceType, tags map[string]string) {
	var ts []ec2types.Tag
	for _, k := range sortedKeys(tags) {
		ts = append(ts, ec2types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	f.enis[id] = &ec2types.NetworkInterface{NetworkInterfaceId: aws.String(id), VpcId: aws.String(vpc),
		Description: aws.String(desc), InterfaceType: typ, TagSet: ts}
}

// create cluster's pass, both ways: in the deployment's VPC every interface
// without the owner tag (the control plane's, the NAT gateway's, one the VPC
// CNI made) gets the owner tag and TAGS, not the created-by tag, once; one in
// another VPC is not touched; a rerun writes nothing and says every interface
// carries the owner tag.
func TestCreateClusterTagsEveryInterfaceInItsVPC(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	vpc := aws.ToString(f.cluster.ResourcesVpcConfig.VpcId)
	addENI(f, "eni-eks", vpc, "Amazon EKS demo", ec2types.NetworkInterfaceTypeInterface, nil)
	// The type EC2 reports for a NAT gateway's interface (sub-p13).
	addENI(f, "eni-nat", vpc, "Interface for NAT Gateway nat-1", "nat_gateway", nil)
	addENI(f, "eni-cni", vpc, "aws-K8S-i-1", ec2types.NetworkInterfaceTypeInterface, nil)
	addENI(f, "eni-elsewhere", "vpc-other", "Amazon EKS demo", ec2types.NetworkInterfaceTypeInterface, nil)
	f.writes = nil
	l := logs(t)
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"eni-eks", "eni-nat", "eni-cni"} {
		if countCalls(f.writes, "CreateTags "+id) != 1 {
			t.Errorf("%s: writes %v", id, f.writes)
		}
		m := ec2TagMap(f.enis[id].TagSet)
		if len(m) != 3 || m[OwnerTagKey] != "demo.us-west-2" || m["auto-delete"] != "no" || m[CreatedByTagKey] != "" {
			t.Errorf("%s tags %v", id, m)
		}
	}
	if len(f.enis["eni-elsewhere"].TagSet) != 0 {
		t.Error("tagged an interface outside the VPC")
	}
	if !strings.Contains(l.String(), `msg="every network interface in the VPC carries the owner tag" vpc=`+vpc+` interfaces=3 tagged=3`) {
		t.Errorf("summary:\n%s", l.String())
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("rerun: err %v, writes %v", err, f.writes)
	}
}

// --plan reads and writes nothing.
func TestCreateClusterInterfacePassPlanWritesNothing(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	vpc := aws.ToString(f.cluster.ResourcesVpcConfig.VpcId)
	addENI(f, "eni-nat", vpc, "Interface for NAT Gateway nat-1", "nat_gateway", nil)
	f.writes = nil
	if err := clusterTestRun(t, f, true).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("plan: err %v, writes %v", err, f.writes)
	}
}

// eniFake is an account's VPCs and network interfaces, for the pass alone.
type eniFake struct {
	vpcs     map[string]map[string]string // id -> tags
	enis     map[string]*ec2types.NetworkInterface
	refuse   map[string]string // interface -> the error code CreateTags answers
	failRead bool              // DescribeNetworkInterfaces fails
	writes   []string
}

func newENIFake() *eniFake {
	return &eniFake{vpcs: map[string]map[string]string{}, enis: map[string]*ec2types.NetworkInterface{}, refuse: map[string]string{}}
}

func (f *eniFake) add(id, vpc, desc string, typ ec2types.NetworkInterfaceType, tags map[string]string) {
	f.enis[id] = &ec2types.NetworkInterface{NetworkInterfaceId: aws.String(id), VpcId: aws.String(vpc),
		Description: aws.String(desc), InterfaceType: typ, TagSet: tagsOf(tags)}
}

func (f *eniFake) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	out := &ec2.DescribeVpcsOutput{}
	want := filterValue(in.Filters, "vpc-id")
	for _, id := range sortedKeysOf(f.vpcs) {
		if want == "" || want == id {
			out.Vpcs = append(out.Vpcs, ec2types.Vpc{VpcId: aws.String(id), Tags: tagsOf(f.vpcs[id])})
		}
	}
	return out, nil
}

func (f *eniFake) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	if f.failRead {
		return nil, apiErr("Throttling")
	}
	if !aws.ToBool(in.IncludeManagedResources) {
		return nil, errors.New("network interfaces described without IncludeManagedResources: Auto Mode's are hidden")
	}
	out := &ec2.DescribeNetworkInterfacesOutput{}
	want := filterValue(in.Filters, "vpc-id")
	for _, id := range sortedKeysOf(f.enis) {
		if n := f.enis[id]; want == "" || aws.ToString(n.VpcId) == want {
			out.NetworkInterfaces = append(out.NetworkInterfaces, *n)
		}
	}
	return out, nil
}

func (f *eniFake) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	ids := strings.Join(in.Resources, ",")
	f.writes = append(f.writes, "CreateTags "+ids)
	if code := f.refuse[ids]; code != "" {
		return nil, apiErr(code)
	}
	for _, id := range in.Resources {
		n := f.enis[id]
		m := ec2TagMap(n.TagSet)
		for _, t := range in.Tags {
			m[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		n.TagSet = tagsOf(m)
	}
	return &ec2.CreateTagsOutput{}, nil
}

// passWorld is the deployment's VPC vpc-1 and a second VPC vpc-2, also its own.
func passWorld() *eniFake {
	f := newENIFake()
	f.vpcs["vpc-1"] = map[string]string{OwnerTagKey: "demo.us-west-2"}
	f.vpcs["vpc-2"] = map[string]string{OwnerTagKey: "demo.us-west-2"}
	return f
}

// The pass, both ways: an interface in the VPC without the owner tag (RDS's)
// gets exactly the owner tag and TAGS; one with the owner tag but not all of
// TAGS gets the rest; one with all of them is not written; one with another
// deployment's owner tag keeps it and is named; one in another VPC is never
// touched, even a VPC this deployment owns. A rerun writes nothing.
func TestInterfacePassTagsWhatLacksTheOwnerTag(t *testing.T) {
	f := passWorld()
	f.add("eni-rds", "vpc-1", "RDSNetworkInterface", ec2types.NetworkInterfaceTypeInterface, nil)
	f.add("eni-half", "vpc-1", "aws-K8S-i-1", ec2types.NetworkInterfaceTypeInterface, map[string]string{OwnerTagKey: "demo.us-west-2"})
	f.add("eni-done", "vpc-1", "", ec2types.NetworkInterfaceTypeInterface, ownerTags())
	f.add("eni-theirs", "vpc-1", "", ec2types.NetworkInterfaceTypeInterface, map[string]string{OwnerTagKey: "other.us-west-2"})
	f.add("eni-other", "vpc-2", "", ec2types.NetworkInterfaceTypeLambda, nil)
	e := testEnv(t, newFake(), false)
	l := logs(t)
	if err := e.tagVPCInterfaces(context.Background(), f, "vpc-1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.writes, ","); got != "CreateTags eni-half,CreateTags eni-rds" {
		t.Errorf("writes %s", got)
	}
	for _, id := range []string{"eni-rds", "eni-half", "eni-done"} {
		if m := ec2TagMap(f.enis[id].TagSet); len(m) != 3 || m[OwnerTagKey] != "demo.us-west-2" || m["auto-delete"] != "no" || m["project"] != "agent-substrate" {
			t.Errorf("%s tags %v", id, m)
		}
	}
	if m := ec2TagMap(f.enis["eni-theirs"].TagSet); len(m) != 1 || m[OwnerTagKey] != "other.us-west-2" {
		t.Errorf("another deployment's interface was retagged: %v", m)
	}
	if len(f.enis["eni-other"].TagSet) != 0 {
		t.Error("tagged an interface in another VPC")
	}
	for _, want := range []string{`level=WARN msg="a network interface in the VPC carries another deployment's owner tag; it is left as it is" interface=eni-theirs owner=other.us-west-2`,
		`msg="network interfaces in the VPC" vpc=vpc-1 interfaces=4 tagged=2 untagged=eni-theirs`} {
		if !strings.Contains(l.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, l.String())
		}
	}
	f.writes = nil
	if err := e.tagVPCInterfaces(context.Background(), f, "vpc-1"); err != nil || len(f.writes) != 0 {
		t.Errorf("rerun: err %v, writes %v", err, f.writes)
	}
}

// A VPC with another deployment's owner tag, or none, is left alone.
func TestInterfacePassLeavesAVPCItDoesNotOwn(t *testing.T) {
	for _, tags := range []map[string]string{{OwnerTagKey: "other.us-west-2"}, {}} {
		f := newENIFake()
		f.vpcs["vpc-1"] = tags
		f.add("eni-rds", "vpc-1", "RDSNetworkInterface", ec2types.NetworkInterfaceTypeInterface, nil)
		l := logs(t)
		if err := testEnv(t, newFake(), false).tagVPCInterfaces(context.Background(), f, "vpc-1"); err != nil || len(f.writes) != 0 {
			t.Errorf("VPC tags %v: err %v, writes %v", tags, err, f.writes)
		}
		if !strings.Contains(l.String(), "the cluster's VPC is not this deployment's") {
			t.Errorf("no log line:\n%s", l.String())
		}
	}
}

// --plan names what it would tag and writes nothing.
func TestInterfacePassPlanWritesNothing(t *testing.T) {
	f := passWorld()
	f.add("eni-rds", "vpc-1", "RDSNetworkInterface", ec2types.NetworkInterfaceTypeInterface, nil)
	l := logs(t)
	if err := testEnv(t, newFake(), true).tagVPCInterfaces(context.Background(), f, "vpc-1"); err != nil || len(f.writes) != 0 {
		t.Errorf("plan: err %v, writes %v", err, f.writes)
	}
	if !strings.Contains(l.String(), `msg="plan: would tag network interface eni-rds (RDSNetworkInterface)"`) || len(f.enis["eni-rds"].TagSet) != 0 {
		t.Errorf("plan:\n%s", l.String())
	}
}

// Both ways: an interface AWS refuses to tag is one warning naming it and
// the reason, and the pass goes on to the next and succeeds; a failed read
// is an error, never "nothing to tag".
func TestInterfacePassRefusedTagIsAWarning(t *testing.T) {
	f := passWorld()
	f.add("eni-efs", "vpc-1", "EFS mount target for fs-1 (fsmt-1)", "efs", nil)
	f.add("eni-fn", "vpc-1", "AWS Lambda VPC ENI-demo-db-owner", ec2types.NetworkInterfaceTypeLambda, nil)
	f.refuse["eni-efs"] = "UnauthorizedOperation"
	l := logs(t)
	e := testEnv(t, newFake(), false)
	if err := e.tagVPCInterfaces(context.Background(), f, "vpc-1"); err != nil {
		t.Fatalf("a refused tag failed the step: %v", err)
	}
	if ec2TagMap(f.enis["eni-fn"].TagSet)[OwnerTagKey] != "demo.us-west-2" || len(f.enis["eni-efs"].TagSet) != 0 {
		t.Errorf("eni-fn %v, eni-efs %v", f.enis["eni-fn"].TagSet, f.enis["eni-efs"].TagSet)
	}
	warn := `level=WARN msg="AWS refused to tag a network interface; it is left without the owner tag" interface=eni-efs kind="EFS mount target for fs-1 (fsmt-1)" reason=`
	if strings.Count(l.String(), "level=WARN") != 1 || !strings.Contains(l.String(), warn) || !strings.Contains(l.String(), "UnauthorizedOperation") {
		t.Errorf("want one warning naming eni-efs and the reason:\n%s", l.String())
	}
	f.failRead = true
	if err := e.tagVPCInterfaces(context.Background(), f, "vpc-1"); err == nil || !strings.Contains(err.Error(), "Throttling") {
		t.Errorf("a failed read: %v", err)
	}
}

// Each step that makes something AWS gives a network interface runs the
// pass last: bootstrap (EKS's late control-plane interface), create rds, and
// the efs, s3files and db-rotation plugins. Each case adds an untagged
// interface to the deployment's VPC before the step, and finds it tagged.
func TestEachStepRunsTheInterfacePass(t *testing.T) {
	untagged := func(db *fakeDB, desc string, typ ec2types.NetworkInterfaceType) {
		db.vpcOwner = "demo.us-west-2"
		db.enis["eni-new"] = &ec2types.NetworkInterface{NetworkInterfaceId: aws.String("eni-new"), VpcId: aws.String(fakeVPC),
			Description: aws.String(desc), InterfaceType: typ}
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) *fakeDB
	}{
		{"bootstrap", func(t *testing.T) *fakeDB {
			db, e := newFakeDB(), rdsEnv(t, false)
			e.c.EC2 = (*fec2)(db)
			untagged(db, "Amazon EKS demo", ec2types.NetworkInterfaceTypeInterface)
			if err := e.bootstrap(context.Background()); err != nil {
				t.Fatal(err)
			}
			return db
		}},
		{"create rds", func(t *testing.T) *fakeDB {
			db, e := newFakeDB(), rdsEnv(t, false)
			untagged(db, "RDSNetworkInterface", ec2types.NetworkInterfaceTypeInterface)
			if err := e.createRDS(context.Background(), db.clients()); err != nil {
				t.Fatal(err)
			}
			return db
		}},
		{"plugin efs", func(t *testing.T) *fakeDB {
			f, db := newFakeNFS(), newFakeDB()
			installNFS(t, f, db)
			untagged(db, "EFS mount target for fs-1", "efs")
			if err := createEFS(context.Background(), testEnv(t, newFake(), false)); err != nil {
				t.Fatal(err)
			}
			return db
		}},
		{"plugin s3files", func(t *testing.T) *fakeDB {
			f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
			aws_.roles[s3fCtrl] = map[string]string{OwnerTagKey: "demo.us-west-2"}
			installNFS(t, f, db)
			untagged(db, "S3 Files mount target for fs-1", "efs")
			if err := createS3Files(context.Background(), testEnv(t, aws_, false)); err != nil {
				t.Fatal(err)
			}
			return db
		}},
		{"plugin db-rotation", func(t *testing.T) *fakeDB {
			e, db, rc, _ := rotationWorld(t)
			untagged(db, "AWS Lambda VPC ENI-demo-db-owner", ec2types.NetworkInterfaceTypeLambda)
			if err := e.createDBRotation(context.Background(), rc); err != nil {
				t.Fatal(err)
			}
			return db
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.run(t)
			if m := ec2TagMap(db.enis["eni-new"].TagSet); m[OwnerTagKey] != "demo.us-west-2" || m["auto-delete"] != "no" {
				t.Errorf("the step left its interface untagged: %v (writes %v)", m, db.writes)
			}
		})
	}
}
