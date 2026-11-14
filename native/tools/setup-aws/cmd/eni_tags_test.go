package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
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

// Rule (2), both ways: in the deployment's VPC, the cluster's control-plane
// interfaces and the NAT gateway's get the owner tag and TAGS (not the
// created-by tag), once; an interface AWS made elsewhere, or a node's, is
// not tagged; the warning names what is still untagged in the VPC, and
// none once all are.
func TestImplicitENIsAreTaggedAndTheRestReported(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	vpc := aws.ToString(f.cluster.ResourcesVpcConfig.VpcId)
	f.vpcs["vpc-other"] = &ec2types.Vpc{VpcId: aws.String("vpc-other")}
	addENI(f, "eni-eks", vpc, "Amazon EKS demo", ec2types.NetworkInterfaceTypeInterface, nil)
	// The type EC2 reports for a NAT gateway's interface, not the SDK's
	// "natGateway" constant (sub-p13).
	addENI(f, "eni-nat", vpc, "Interface for NAT Gateway nat-1", "nat_gateway", nil)
	addENI(f, "eni-cni", vpc, "aws-K8S-i-1", ec2types.NetworkInterfaceTypeInterface, nil)
	addENI(f, "eni-elsewhere", "vpc-other", "Amazon EKS demo", ec2types.NetworkInterfaceTypeInterface, nil)
	f.writes = nil
	l := logs(t)
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"CreateTags eni-eks", "CreateTags eni-nat"} {
		if countCalls(f.writes, w) != 1 {
			t.Errorf("%s: writes %v", w, f.writes)
		}
	}
	for _, id := range []string{"eni-eks", "eni-nat"} {
		m := ec2TagMap(f.enis[id].TagSet)
		if len(m) != 3 || m[OwnerTagKey] != "demo.us-west-2" || m["auto-delete"] != "no" || m[CreatedByTagKey] != "" {
			t.Errorf("%s tags %v", id, m)
		}
	}
	if len(f.enis["eni-cni"].TagSet) != 0 || len(f.enis["eni-elsewhere"].TagSet) != 0 {
		t.Error("tagged a node's interface or one outside the VPC")
	}
	if !strings.Contains(l.String(), `level=WARN msg="network interfaces in the VPC without the owner tag`) || !strings.Contains(l.String(), "untagged=eni-cni") {
		t.Errorf("warning:\n%s", l.String())
	}

	// Once the CNI tags its own, a rerun tags nothing and warns of none.
	f.enis["eni-cni"].TagSet = []ec2types.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String("demo.us-west-2")}}
	f.writes = nil
	l.Reset()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || strings.Contains(l.String(), "level=WARN msg=\"network interfaces") ||
		!strings.Contains(l.String(), "every network interface in the VPC carries the owner tag") {
		t.Errorf("all tagged: writes %v\n%s", f.writes, l.String())
	}
}

// A VPC that is not this deployment's: its interfaces are left alone.
func TestImplicitENIsOnlyInAnOwnedVPC(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	vpc := aws.ToString(f.cluster.ResourcesVpcConfig.VpcId)
	addENI(f, "eni-eks", vpc, "Amazon EKS demo", ec2types.NetworkInterfaceTypeInterface, nil)
	f.vpcs[vpc].Tags = []ec2types.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String("other.us-west-2")}}
	r := clusterTestRun(t, f, false)
	r.vpcID = vpc
	f.writes = nil
	if err := r.tagImplicitENIs(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("foreign VPC: err %v, writes %v", err, f.writes)
	}
}

// --plan reads and writes nothing.
func TestImplicitENIsPlanWritesNothing(t *testing.T) {
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
