package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

const (
	nodeRoleARN = "arn:aws:iam::111122223333:role/demo-us-west-2-eks-node"
	nodeProfile = "demo-us-west-2-eks-node"
)

func selfManaged(c *clusterConfig) { c.NodeGroupType = nodegroups.SelfManaged }

// defaultGroups are the node groups NODE_GROUPS names by default: every
// group but nitro.
var defaultGroups, _ = nodegroups.Parse(nodegroups.Default)

// selfManagedCreates is what a fresh account gets with
// NODE_GROUP_TYPE=self-managed: freshCreates with an instance profile, the
// node role's access entry and an Auto Scaling group per group in place of
// the managed node groups.
func selfManagedCreates() map[string]int {
	m := maps.Clone(freshCreates)
	for _, g := range defaultGroups {
		delete(m, "CreateNodegroup "+g)
		m["CreateAutoScalingGroup demo-"+g] = 1
	}
	m["CreateInstanceProfile "+nodeProfile] = 1
	m["CreateAccessEntry "+nodeRoleARN] = 1
	return m
}

// captureLog sends the default logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	b := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// shortNodeWait bounds the waits for self-managed nodes for the test.
func shortNodeWait(t *testing.T) {
	t.Helper()
	prev := nodeWaitTimeout
	nodeWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { nodeWaitTimeout = prev })
}

// The default sends what it sent before self-managed node groups: every
// API they added is denied, so a stray call fails the run, and the
// Kubernetes API is never asked. The payloads are the managed ones, the
// phase lines are phases.Cluster, and a rerun writes nothing.
func TestCreateClusterManagedSendsNoNewCalls(t *testing.T) {
	f := newClusterFake()
	denyNewCalls(f)
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := creates(f.writes); !maps.Equal(got, freshCreates) {
		t.Errorf("creates:\n got %v\nwant %v", got, freshCreates)
	}
	for _, w := range f.writes {
		for _, op := range []string{"CreateInstanceProfile", "AddRoleToInstanceProfile", "CreateAutoScalingGroup"} {
			if strings.HasPrefix(w, op) {
				t.Errorf("a managed run wrote %s", w)
			}
		}
	}
	if f.kubeCalls != 0 || len(f.profiles) != 0 || len(f.asgs) != 0 {
		t.Errorf("Kubernetes API asked %d times, instance profiles %v, Auto Scaling groups %v", f.kubeCalls, f.profiles, f.asgs)
	}
	if want := []string{"arn:aws:iam::111122223333:role/Admin"}; !slices.Equal(sortedKeysOf(f.entries), want) || f.entryTypes[want[0]] != "STANDARD" {
		t.Errorf("access entries %v (%v), want the caller's only", sortedKeysOf(f.entries), f.entryTypes)
	}
	for _, lt := range []string{"demo-system-nodes", "demo-gvisor-nodes", "demo-microvm-nested-virt"} {
		d := latestData(t, f, lt)
		if d.IamInstanceProfile != nil || d.SecurityGroupIds != nil || d.ImageId != nil || d.UserData != nil || d.InstanceType != "" {
			t.Errorf("%s: profile %v, security groups %v, image %v, user data %v", lt, d.IamInstanceProfile, d.SecurityGroupIds, d.ImageId, d.UserData)
		}
	}
	for _, g := range defaultGroups {
		if f.ngIn[g].AmiType != ekstypes.AMITypesAl2023X8664Standard {
			t.Errorf("%s: AMI type %q", g, f.ngIn[g].AmiType)
		}
	}
	for i := range phases.Cluster {
		if !strings.Contains(logs.String(), phases.Line(phases.Cluster, i)) {
			t.Errorf("no line %q", phases.Line(phases.Cluster, i))
		}
	}
	if strings.Contains(logs.String(), "self-managed") {
		t.Errorf("a managed run logged self-managed lines:\n%s", logs)
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("rerun writes %v", f.writes)
	}
}

// denyNewCalls denies every read self-managed node groups added, the node
// role's access entry included, so a managed run that makes one fails.
func denyNewCalls(f *clusterFake) {
	for _, op := range []string{"GetParameter", "GetInstanceProfile", "ListInstanceProfileTags", "ListNodegroups",
		"DescribeAutoScalingGroups", "DescribeScalingActivities", "ListNodes", "DescribeAccessEntry " + nodeRoleARN} {
		f.deny[op] = "AccessDenied"
	}
}

// The one new call of a managed run: on a cluster that exists, with a node
// group still to make, one DescribeAutoScalingGroups to find self-managed
// groups. A denied one stops the run before it makes anything, as any
// denied read does. It reads the managed node groups it wants only.
func TestCreateClusterManagedAddingAGroupReadsTheAutoScalingGroupsOnce(t *testing.T) {
	f := newClusterFake()
	denyNewCalls(f)
	twoGroups := func(c *clusterConfig) { c.NodeGroups = "system,gvisor" }
	if err := clusterTestRun(t, f, false, twoGroups).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes, f.reads = nil, nil
	err := clusterTestRun(t, f, false).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "describe Auto Scaling groups") || !strings.Contains(err.Error(), "AccessDenied") || len(f.writes) != 0 {
		t.Fatalf("a denied DescribeAutoScalingGroups: %v, writes %v", err, f.writes)
	}
	delete(f.deny, "DescribeAutoScalingGroups")
	f.writes, f.reads = nil, nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countOf(f.reads, "DescribeAutoScalingGroups"); n != 1 || f.ngIn["microvm"] == nil {
		t.Errorf("DescribeAutoScalingGroups %d times, want 1; microvm group made %v", n, f.ngIn["microvm"] != nil)
	}
	if want := map[string]int{"CreateLaunchTemplate demo-microvm-nested-virt": 1, "CreateNodegroup microvm": 1}; !maps.Equal(creates(f.writes), want) {
		t.Errorf("creates %v, want %v", creates(f.writes), want)
	}
	// A complete cluster: no Auto Scaling group read, and no node group
	// read but those of the wanted groups.
	f.deny["DescribeAutoScalingGroups"] = "AccessDenied"
	f.writes, f.reads = nil, nil
	if err := clusterTestRun(t, f, false, twoGroups).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	for _, r := range f.reads {
		if g, ok := strings.CutPrefix(r, "DescribeNodegroup "); ok {
			read[g] = true
		}
	}
	if want := map[string]bool{"system": true, "gvisor": true}; !maps.Equal(read, want) || len(f.writes) != 0 {
		t.Errorf("node groups read %v, want %v; writes %v", read, want, f.writes)
	}
	if n := countOf(f.reads, "ListNodegroups"); n != 0 {
		t.Errorf("a managed run listed the node groups %d times", n)
	}
}

func TestPhasesClusterForEachType(t *testing.T) {
	sm := phases.ClusterFor(nodegroups.SelfManaged, false)
	if !slices.Equal(phases.ClusterFor(nodegroups.Managed, false), phases.Cluster) || !slices.Equal(phases.ClusterFor("", false), phases.Cluster) {
		t.Error("managed (or unset) is not phases.Cluster")
	}
	if len(sm) != len(phases.Cluster) || !slices.Equal(sm[:7], phases.Cluster[:7]) ||
		sm[7] != "Creating the self-managed node groups..." || sm[8] != "Waiting for the self-managed nodes to be Ready..." {
		t.Errorf("self-managed phases %q", sm)
	}
	if phases.Cluster[7] != "Creating the managed node groups..." {
		t.Errorf("phases.Cluster changed: %q", phases.Cluster)
	}
}

// A fresh self-managed cluster: the instance profile with the node role,
// the role's EC2_LINUX access entry, a launch template per group with the
// recommended AMI, the profile, the cluster security group and a NodeConfig
// with the group's labels (none of a managed group's), an Auto Scaling group
// per group, and a wait that reads the nodes from the Kubernetes API.
func TestCreateClusterSelfManaged(t *testing.T) {
	f := newClusterFake()
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got, want := creates(f.writes), selfManagedCreates(); !maps.Equal(got, want) {
		t.Errorf("creates:\n got %v\nwant %v", got, want)
	}
	if len(f.ngIn) != 0 {
		t.Errorf("made managed node groups %v", sortedKeysOf(f.ngIn))
	}
	want := map[string]string{OwnerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no", CreatedByTagKey: CreatedByTagValue}
	p := f.profiles[nodeProfile]
	if p == nil || !maps.Equal(p.tags, want) || !slices.Equal(p.roles, []string{"demo-us-west-2-eks-node"}) {
		t.Fatalf("instance profile %+v", p)
	}
	if f.entryTypes[nodeRoleARN] != "EC2_LINUX" || !maps.Equal(f.entries[nodeRoleARN], want) {
		t.Errorf("node access entry: type %q, tags %v", f.entryTypes[nodeRoleARN], f.entries[nodeRoleARN])
	}
	sg := aws.ToString(f.cluster.ResourcesVpcConfig.ClusterSecurityGroupId)
	private := strings.Join(f.clusterIn.ResourcesVpcConfig.SubnetIds, ",")
	for _, g := range []struct{ name, lt, taint, typ string }{
		{"system", "demo-system-nodes", "ate.dev/workloadType", "m8i.xlarge"},
		{"gvisor", "demo-gvisor-nodes", "ate.dev/sandboxClass", "m8i.2xlarge"},
		{"microvm", "demo-microvm-nested-virt", "ate.dev/sandboxClass", "m8i.2xlarge"},
	} {
		d := latestData(t, f, g.lt)
		// Auto Scaling takes the instance type from the template.
		if string(d.InstanceType) != g.typ {
			t.Errorf("%s: template instance type %q, want %s", g.name, d.InstanceType, g.typ)
		}
		if aws.ToString(d.ImageId) != eksAMIx86 || d.IamInstanceProfile == nil || aws.ToString(d.IamInstanceProfile.Name) != nodeProfile ||
			!slices.Equal(d.SecurityGroupIds, []string{sg}) {
			t.Errorf("%s: image %s, profile %v, security groups %v", g.name, aws.ToString(d.ImageId), d.IamInstanceProfile, d.SecurityGroupIds)
		}
		if aws.ToInt32(d.MetadataOptions.HttpPutResponseHopLimit) != 1 || d.MetadataOptions.HttpTokens != ec2types.LaunchTemplateHttpTokensStateRequired ||
			!aws.ToBool(d.BlockDeviceMappings[0].Ebs.Encrypted) || d.BlockDeviceMappings[0].Ebs.VolumeType != ec2types.VolumeTypeGp3 {
			t.Errorf("%s: IMDS %+v, root %+v", g.name, d.MetadataOptions, d.BlockDeviceMappings[0].Ebs)
		}
		if nested := d.CpuOptions != nil && d.CpuOptions.NestedVirtualization == ec2types.NestedVirtualizationSpecificationEnabled; nested != (g.name == "microvm") {
			t.Errorf("%s: nested virtualization %v", g.name, nested)
		}
		_, nc := parseUserData(t, aws.ToString(d.UserData))
		kl := nc["spec"].(map[string]any)["kubelet"].(map[string]any)
		flags := kl["flags"].([]any)
		if len(flags) != 1 || !strings.Contains(flags[0].(string), "ate.dev/pool="+g.name) || strings.Contains(flags[0].(string), "eks.amazonaws.com/") {
			t.Errorf("%s: kubelet flags %v", g.name, flags)
		}
		taints, _ := kl["config"].(map[string]any)["registerWithTaints"].([]any)
		if len(taints) != 1 || taints[0].(map[string]any)["key"] != g.taint {
			t.Errorf("%s: taints %v", g.name, kl["config"])
		}
		in := f.asgIn["demo-"+g.name]
		if in == nil {
			t.Fatalf("no Auto Scaling group demo-%s", g.name)
		}
		lt := f.lts[aws.ToString(in.LaunchTemplate.LaunchTemplateId)]
		if aws.ToString(lt.lt.LaunchTemplateName) != g.lt || aws.ToString(in.LaunchTemplate.Version) != "1" {
			t.Errorf("%s: launch template %s version %s", g.name, aws.ToString(lt.lt.LaunchTemplateName), aws.ToString(in.LaunchTemplate.Version))
		}
		if aws.ToString(in.VPCZoneIdentifier) != private || aws.ToString(in.HealthCheckType) != "EC2" {
			t.Errorf("%s: subnets %s, health check %s; want the private subnets %s", g.name, aws.ToString(in.VPCZoneIdentifier), aws.ToString(in.HealthCheckType), private)
		}
		tags := map[string]string{}
		for _, tg := range in.Tags {
			tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
			if !aws.ToBool(tg.PropagateAtLaunch) {
				t.Errorf("%s: tag %s does not propagate", g.name, aws.ToString(tg.Key))
			}
		}
		wantTags := maps.Clone(want)
		wantTags["Name"], wantTags["kubernetes.io/cluster/demo"] = "demo-"+g.name, "owned"
		if !maps.Equal(tags, wantTags) {
			t.Errorf("%s: Auto Scaling group tags %v, want %v", g.name, tags, wantTags)
		}
	}
	if in := f.asgIn["demo-system"]; aws.ToInt32(in.MinSize) != 2 || aws.ToInt32(in.DesiredCapacity) != 2 || aws.ToInt32(in.MaxSize) != 3 {
		t.Errorf("system size %d/%d/%d", aws.ToInt32(in.MinSize), aws.ToInt32(in.DesiredCapacity), aws.ToInt32(in.MaxSize))
	}
	if in := f.asgIn["demo-gvisor"]; aws.ToInt32(in.MinSize) != 1 || aws.ToInt32(in.DesiredCapacity) != 2 || aws.ToInt32(in.MaxSize) != 4 {
		t.Errorf("gvisor size %d/%d/%d", aws.ToInt32(in.MinSize), aws.ToInt32(in.DesiredCapacity), aws.ToInt32(in.MaxSize))
	}
	// One SSM read for the one architecture, one AMI read, one Kubernetes client.
	if n, m := countOf(f.reads, "GetParameter"), countOf(f.reads, "DescribeImages"); n != 1 || m != 1 || f.kubeCalls != 1 || countOf(f.reads, "ListNodes") < 3 {
		t.Errorf("GetParameter %d, DescribeImages %d, Kubernetes clients %d, node lists %d", n, m, f.kubeCalls, countOf(f.reads, "ListNodes"))
	}
	for _, a := range f.addons {
		if a.Status != ekstypes.AddonStatusActive {
			t.Errorf("add-on %s is %s", aws.ToString(a.AddonName), a.Status)
		}
	}
	sm := phases.ClusterFor(nodegroups.SelfManaged, false)
	for i := range sm {
		if !strings.Contains(logs.String(), phases.Line(sm, i)) {
			t.Errorf("no line %q", phases.Line(sm, i))
		}
	}
	if strings.Contains(logs.String(), "111122223333") {
		t.Error("the log carries the account id")
	}

	f.writes, f.reads = nil, nil
	r := clusterTestRun(t, f, false, selfManaged)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(r.creates)+len(r.changes) != 0 {
		t.Errorf("rerun: writes %v, creates %v, changes %v", f.writes, r.creates, r.changes)
	}
	// Every group exists: no AMI to resolve.
	if countOf(f.reads, "GetParameter") != 0 {
		t.Errorf("rerun read SSM: %v", f.reads)
	}
}

// Each architecture's recommended AMI, read once: the arm64 gvisor group
// gets the arm64 one, the x86_64 groups share theirs.
func TestCreateClusterSelfManagedAMIPerArchitecture(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, selfManaged, func(c *clusterConfig) { c.GvisorInstanceType = "m8g.2xlarge" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countOf(f.reads, "GetParameter"); n != 2 {
		t.Errorf("GetParameter %d times, want one per architecture", n)
	}
	for lt, want := range map[string]string{"demo-system-nodes": eksAMIx86, "demo-gvisor-nodes": eksAMIarm, "demo-microvm-nested-virt": eksAMIx86} {
		if got := aws.ToString(latestData(t, f, lt).ImageId); got != want {
			t.Errorf("%s: image %s, want %s", lt, got, want)
		}
	}
}

// On an existing cluster the AMI is the cluster's version's, not
// KUBERNETES_VERSION's.
func TestCreateClusterSelfManagedAMIOfTheClusterVersion(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cluster.Version = aws.String("1.38")
	newer := "ami-0e1e0000000000138"
	im := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
	im.ImageId = aws.String(newer)
	f.images[newer] = im
	f.params["/aws/service/eks/optimized-ami/1.38/amazon-linux-2023/x86_64/standard/recommended/image_id"] = newer
	delete(f.asgs, "demo-gvisor")
	f.writes = nil
	if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(latestData(t, f, "demo-gvisor-nodes").ImageId); got != newer {
		t.Errorf("gvisor template image %s, want the 1.38 AMI %s", got, newer)
	}
	if want := []string{"CreateLaunchTemplateVersion demo-gvisor-nodes", "CreateAutoScalingGroup demo-gvisor"}; !slices.Equal(f.writes, want) {
		t.Errorf("writes %v, want %v", f.writes, want)
	}
}

// NODE_AMI_ID on self-managed groups: that AMI, no SSM read, and still no
// managed group's labels.
func TestCreateClusterSelfManagedWithNodeAMI(t *testing.T) {
	f := newClusterFake()
	f.images[testAMI] = testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
	f.deny["GetParameter"] = "AccessDenied"
	if err := clusterTestRun(t, f, false, selfManaged, withAMI(testAMI)).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := latestData(t, f, "demo-gvisor-nodes")
	_, nc := parseUserData(t, aws.ToString(d.UserData))
	flags := nc["spec"].(map[string]any)["kubelet"].(map[string]any)["flags"].([]any)
	if aws.ToString(d.ImageId) != testAMI || strings.Contains(flags[0].(string), "eks.amazonaws.com/") {
		t.Errorf("image %s, flags %v", aws.ToString(d.ImageId), flags)
	}
}

// --plan on a fresh account writes nothing, reads the AMI, and lists what a
// real run creates.
func TestCreateClusterSelfManagedPlan(t *testing.T) {
	f := newClusterFake()
	p := clusterTestRun(t, f, true, selfManaged)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || f.kubeCalls != 0 || countOf(f.reads, "GetParameter") != 1 {
		t.Fatalf("plan: writes %v, Kubernetes clients %d, reads %v", f.writes, f.kubeCalls, f.reads)
	}
	real := clusterTestRun(t, newClusterFake(), false, selfManaged)
	if err := real.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.creates, real.creates) {
		t.Errorf("plan lists\n %v\nthe run created\n %v", p.creates, real.creates)
	}
}

// A run that stops part-way is completed by the next, each resource made
// once: the profile without its role, the access entry, a group's Auto
// Scaling group, all made but unanswered.
func TestCreateClusterSelfManagedInterruptedRunIsCompleted(t *testing.T) {
	for _, tc := range []struct{ before, after string }{
		{after: "CreateInstanceProfile"},
		{after: "AddRoleToInstanceProfile"},
		{after: "CreateAccessEntry " + nodeRoleARN},
		{after: "CreateLaunchTemplate demo-gvisor-nodes"},
		{after: "CreateAutoScalingGroup demo-gvisor"},
		{before: "CreateAutoScalingGroup demo-microvm"},
	} {
		t.Run(tc.before+tc.after, func(t *testing.T) {
			f := newClusterFake()
			f.failBefore, f.failAfter = tc.before, tc.after
			if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err == nil {
				t.Fatal("the interrupted run reported success")
			}
			if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
				t.Fatalf("rerun: %v", err)
			}
			if got, want := creates(f.writes), selfManagedCreates(); !maps.Equal(got, want) {
				t.Errorf("creates over both runs:\n got %v\nwant %v", got, want)
			}
			if p := f.profiles[nodeProfile]; p == nil || len(p.roles) != 1 {
				t.Errorf("instance profile %+v", p)
			}
		})
	}
}

// An owned profile reused: one that lost its role gets it back (a change);
// one with another role is refused, as one owned by another deployment is.
func TestCreateClusterInstanceProfileReuse(t *testing.T) {
	t.Run("owned, no role", func(t *testing.T) {
		f := newClusterFake()
		f.profiles[nodeProfile] = &fakeProfile{tags: map[string]string{OwnerTagKey: "demo.us-west-2"}, tagsInGet: true}
		r := clusterTestRun(t, f, false, selfManaged)
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if creates(f.writes)["CreateInstanceProfile "+nodeProfile] != 0 || !slices.Contains(r.changes, "add role demo-us-west-2-eks-node to instance profile "+nodeProfile) {
			t.Errorf("writes %v, changes %v", f.writes, r.changes)
		}
		if countOf(f.reads, "ListInstanceProfileTags") != 0 {
			t.Error("read the tags again though GetInstanceProfile had them")
		}
	})
	for name, c := range map[string]struct {
		p    *fakeProfile
		want string
	}{
		"another deployment's": {&fakeProfile{tags: map[string]string{OwnerTagKey: "other.us-west-2"}}, "is not owned by demo.us-west-2"},
		"untagged":             {&fakeProfile{tags: map[string]string{}, roles: []string{"demo-us-west-2-eks-node"}}, "is not owned by demo.us-west-2"},
		"owned, another role":  {&fakeProfile{tags: map[string]string{OwnerTagKey: "demo.us-west-2"}, roles: []string{"someone-elses"}}, "holds role someone-elses"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newClusterFake()
			f.roles["someone-elses"] = &fakeRole{arn: "arn:aws:iam::111122223333:role/someone-elses", tags: map[string]string{}}
			f.profiles[nodeProfile] = c.p
			err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "instance profile "+nodeProfile) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming the profile (%q), got %v", c.want, err)
			}
			if strings.Contains(err.Error(), "delete") || strings.Contains(err.Error(), "remove it") || !strings.Contains(err.Error(), "use another CLUSTER_NAME") {
				t.Errorf("want another CLUSTER_NAME and no change by hand, got %v", err)
			}
			if c.p.tags[OwnerTagKey] == "demo.us-west-2" {
				wantRecreate(t, err)
			}
			for _, w := range f.writes {
				if strings.HasPrefix(w, "AddRoleToInstanceProfile") || strings.HasPrefix(w, "CreateAutoScalingGroup") || strings.HasPrefix(w, "CreateLaunchTemplate") {
					t.Errorf("wrote %s after the refusal", w)
				}
			}
		})
	}
}

// An access entry of the node role that EKS made (type EC2_LINUX, no tags)
// is kept; one of another type is refused.
func TestCreateClusterNodeAccessEntryReuse(t *testing.T) {
	for typ, ok := range map[string]bool{"EC2_LINUX": true, "STANDARD": false} {
		t.Run(typ, func(t *testing.T) {
			f := newClusterFake()
			if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.entries[nodeRoleARN], f.entryTypes[nodeRoleARN] = map[string]string{}, typ
			f.writes = nil
			err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
			if ok && (err != nil || len(f.writes) != 0) {
				t.Errorf("EKS's EC2_LINUX entry: %v, writes %v", err, f.writes)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "type STANDARD")) {
				t.Errorf("a STANDARD entry: %v", err)
			}
			if !ok {
				wantRecreate(t, err)
			}
		})
	}
}

// An Auto Scaling group of a derived name: another owner's, or one being
// deleted, stops the run before any write.
func TestCreateClusterRefusesForeignAutoScalingGroups(t *testing.T) {
	for name, c := range map[string]struct {
		a    *asgtypes.AutoScalingGroup
		want string
	}{
		"another owner's": {&asgtypes.AutoScalingGroup{AutoScalingGroupName: aws.String("demo-gvisor"),
			Tags: []asgtypes.TagDescription{{Key: aws.String(OwnerTagKey), Value: aws.String("other.us-west-2")}}}, "is not owned by demo.us-west-2"},
		"untagged": {&asgtypes.AutoScalingGroup{AutoScalingGroupName: aws.String("demo-gvisor")}, "is not owned by demo.us-west-2"},
		"being deleted": {&asgtypes.AutoScalingGroup{AutoScalingGroupName: aws.String("demo-gvisor"), Status: aws.String("Delete in progress"),
			Tags: []asgtypes.TagDescription{{Key: aws.String(OwnerTagKey), Value: aws.String("demo.us-west-2")}}}, "wait for it to be gone and run again"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newClusterFake()
			if err := clusterTestRun(t, f, false, selfManaged, func(c *clusterConfig) { c.NodeGroups = "system,microvm" }).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.asgs["demo-gvisor"] = c.a
			f.writes, f.kubeCalls = nil, 0
			err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "Auto Scaling group demo-gvisor") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming demo-gvisor (%q), got %v", c.want, err)
			}
			if strings.Contains(err.Error(), "delete or rename") {
				t.Errorf("says to delete by hand: %v", err)
			}
			if len(f.writes) != 0 || f.kubeCalls != 0 {
				t.Errorf("went past preflight: writes %v, Kubernetes clients %d", f.writes, f.kubeCalls)
			}
		})
	}
}

// wantRecreate checks that a refusal says to go on through the installer's
// teardown, or with another CLUSTER_NAME, and never to delete by hand.
func wantRecreate(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "tear the deployment down (--remove-control-plane, then setup-aws teardown) and create it again") ||
		!strings.Contains(err.Error(), "use another CLUSTER_NAME") || strings.Contains(err.Error(), "delete") || strings.Contains(err.Error(), "remove it") {
		t.Errorf("want the teardown way on and no delete by hand, got %v", err)
	}
}

// A cluster has one node group type: a self-managed run on a cluster with
// managed node groups, and a managed run on one with self-managed groups,
// stop before any write and say how to go on. A managed run reads the Auto
// Scaling groups only when it would add a group to an existing cluster.
func TestCreateClusterRefusesMixedNodeGroupTypes(t *testing.T) {
	t.Run("self-managed on managed", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.writes = nil
		err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "managed node groups gvisor, microvm, system") || !strings.Contains(err.Error(), "NODE_GROUP_TYPE=managed") {
			t.Fatalf("want a mixed-type refusal, got %v", err)
		}
		wantRecreate(t, err)
		if len(f.writes) != 0 {
			t.Errorf("writes %v", f.writes)
		}
	})
	t.Run("managed on self-managed", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, selfManaged, func(c *clusterConfig) { c.NodeGroups = "system,gvisor" }).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.writes, f.reads = nil, nil
		err := clusterTestRun(t, f, false).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "self-managed node groups demo-system, demo-gvisor") || !strings.Contains(err.Error(), "NODE_GROUP_TYPE=self-managed") {
			t.Fatalf("want a mixed-type refusal, got %v", err)
		}
		wantRecreate(t, err)
		if len(f.writes) != 0 {
			t.Errorf("writes %v", f.writes)
		}
	})
	t.Run("managed, another owner's group of a derived name", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.NodeGroups = "system,gvisor" }).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.asgs["demo-microvm"] = &asgtypes.AutoScalingGroup{AutoScalingGroupName: aws.String("demo-microvm")}
		f.reads = nil
		if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
			t.Fatalf("a managed run stopped on an Auto Scaling group it does not own: %v", err)
		}
		if countOf(f.reads, "DescribeAutoScalingGroups") != 1 || f.ngIn["microvm"] == nil {
			t.Errorf("reads %v, microvm group %v", f.reads, f.ngIn["microvm"] != nil)
		}
	})
}

// The one-type rule holds across every node group name, not only the ones a
// run wants: on Auto Mode, a run of one type with other NODE_GROUPS than the
// last run of the other type stops too, and a self-managed run finds managed
// node groups of any name, on every page of the list. A denied list stops a
// self-managed run; another owner's Auto Scaling group of a name it does not
// make is left alone.
func TestCreateClusterRefusesMixedNodeGroupTypesOfOtherNames(t *testing.T) {
	groups := func(g string) func(*clusterConfig) { return func(c *clusterConfig) { c.NodeGroups = g } }
	t.Run("self-managed gvisor on managed system", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, groups("system")).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.writes = nil
		err := clusterTestRun(t, f, false, selfManaged, groups("gvisor")).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "has the managed node groups system,") || len(f.writes) != 0 {
			t.Fatalf("want a mixed-type refusal naming system, got %v (writes %v)", err, f.writes)
		}
	})
	t.Run("managed gvisor on self-managed system", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, selfManaged, groups("system")).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.writes = nil
		err := clusterTestRun(t, f, false, groups("gvisor")).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "has the self-managed node groups demo-system (") || len(f.writes) != 0 {
			t.Fatalf("want a mixed-type refusal naming demo-system, got %v (writes %v)", err, f.writes)
		}
	})
	t.Run("self-managed on managed groups of other names", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, selfManaged, groups("system,gvisor")).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Made outside the installer, with names it never uses; the last
		// is on the list's second page.
		for _, g := range []string{"batch", "ingest", "workers"} {
			f.ngs[g] = &ekstypes.Nodegroup{NodegroupName: aws.String(g), Status: ekstypes.NodegroupStatusActive}
		}
		f.writes, f.reads = nil, nil
		err := clusterTestRun(t, f, false, selfManaged, groups("system,gvisor")).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "has the managed node groups batch, ingest, workers,") || len(f.writes) != 0 {
			t.Fatalf("want a mixed-type refusal naming every managed group, got %v (writes %v)", err, f.writes)
		}
		wantRecreate(t, err)
		if n := countOf(f.reads, "ListNodegroups"); n != 2 {
			t.Errorf("ListNodegroups %d times, want 2 (two pages)", n)
		}
	})
	t.Run("a denied list of the node groups", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, selfManaged, groups("system,gvisor")).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.deny["ListNodegroups"] = "AccessDenied"
		f.writes = nil
		err := clusterTestRun(t, f, false, selfManaged, groups("system,gvisor")).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "list the node groups of cluster demo") || !strings.Contains(err.Error(), "AccessDenied") || len(f.writes) != 0 {
			t.Fatalf("want the denied read, got %v (writes %v)", err, f.writes)
		}
	})
	t.Run("self-managed, another owner's group of a name not wanted", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, selfManaged, groups("system,gvisor")).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.asgs["demo-microvm"] = &asgtypes.AutoScalingGroup{AutoScalingGroupName: aws.String("demo-microvm"),
			Tags: []asgtypes.TagDescription{{Key: aws.String(OwnerTagKey), Value: aws.String("other.us-west-2")}}}
		f.writes = nil
		if err := clusterTestRun(t, f, false, selfManaged, groups("system,gvisor")).run(context.Background()); err != nil || len(f.writes) != 0 {
			t.Errorf("stopped on, or touched, an Auto Scaling group it does not make: %v, writes %v", err, f.writes)
		}
	})
}

// Owned Auto Scaling groups of a cluster that is gone cannot join a new one:
// the run stops before it makes anything.
func TestCreateClusterSelfManagedRefusesGroupsWithoutTheirCluster(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cluster = nil
	f.writes = nil
	err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cluster demo does not") || !strings.Contains(err.Error(), "setup-aws teardown") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("writes %v", f.writes)
	}
	// Groups the run does not want are left behind just the same.
	err = clusterTestRun(t, f, false, selfManaged, func(c *clusterConfig) { c.NodeGroups = "system" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the Auto Scaling groups demo-system, demo-gvisor, demo-microvm exist") || len(f.writes) != 0 {
		t.Errorf("want a refusal naming every group, got %v (writes %v)", err, f.writes)
	}
}

// A denied or throttled read of a self-managed resource stops the run, and
// is never taken for "not found" and followed by a create.
func TestCreateClusterSelfManagedDeniedReadStops(t *testing.T) {
	for op, never := range map[string]string{
		"GetInstanceProfile":                 "CreateInstanceProfile",
		"ListInstanceProfileTags":            "AddRoleToInstanceProfile",
		"DescribeAutoScalingGroups":          "Create",
		"GetParameter":                       "Create",
		"DescribeAccessEntry " + nodeRoleARN: "CreateAccessEntry " + nodeRoleARN,
		"ListNodes":                          "",
		"DescribeScalingActivities":          "",
	} {
		t.Run(op, func(t *testing.T) {
			f := newClusterFake()
			if op == "ListInstanceProfileTags" || strings.HasPrefix(op, "DescribeAccessEntry") {
				// Reads that happen on a rerun: the profile and the entries exist.
				if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
					t.Fatal(err)
				}
				f.profiles[nodeProfile].roles = nil
				delete(f.entries, nodeRoleARN)
				f.writes = nil
			}
			if op == "DescribeScalingActivities" {
				shortNodeWait(t)
				f.asgStuck = true
			}
			f.deny[op] = "AccessDenied"
			err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
				t.Fatalf("want AccessDenied, got %v", err)
			}
			for w := range creates(f.writes) {
				if never != "" && strings.HasPrefix(w, never) {
					t.Errorf("wrote %s after a denied %s", w, op)
				}
			}
			if op == "ListInstanceProfileTags" && slices.ContainsFunc(f.writes, func(w string) bool { return strings.HasPrefix(w, "AddRoleToInstanceProfile") }) {
				t.Errorf("added the role to a profile whose owner it could not read: %v", f.writes)
			}
		})
	}
}

// No recommended AMI for the version (or a NODE_GROUP_TYPE that is not a
// type) stops the run before any write.
func TestCreateClusterSelfManagedPreflightRefusals(t *testing.T) {
	f := newClusterFake()
	err := clusterTestRun(t, f, false, selfManaged, func(c *clusterConfig) { c.KubernetesVersion = "1.99" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no EKS optimized AL2023 AMI for Kubernetes 1.99") || !strings.Contains(err.Error(), "NODE_AMI_ID") || len(f.writes) != 0 {
		t.Errorf("no SSM parameter: %v (writes %v)", err, f.writes)
	}
	f = newClusterFake()
	err = clusterTestRun(t, f, false, func(c *clusterConfig) { c.NodeGroupType = "self_managed" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `NODE_GROUP_TYPE="self_managed"`) || len(f.writes) != 0 {
		t.Errorf("unknown type: %v (writes %v)", err, f.writes)
	}
	// The AMI SSM names is checked like NODE_AMI_ID.
	f = newClusterFake()
	im := f.images[eksAMIx86]
	im.EnaSupport = aws.Bool(false)
	f.images[eksAMIx86] = im
	err = clusterTestRun(t, f, false, selfManaged).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ENA") || len(f.writes) != 0 {
		t.Errorf("an AMI without ENA: %v (writes %v)", err, f.writes)
	}
}

// Self-managed nodes join from a NodeConfig, which carries an IPv4 service
// CIDR: an IPv6 cluster is refused before any write.
func TestCreateClusterSelfManagedRefusesIPv6(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, selfManaged, func(c *clusterConfig) { c.NodeGroups = "system,gvisor" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cluster.KubernetesNetworkConfig.IpFamily = ekstypes.IpFamilyIpv6
	f.writes = nil
	err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "IPv6") || len(f.writes) != 0 {
		t.Errorf("IPv6 cluster: %v (writes %v)", err, f.writes)
	}
}

// The wait: instances that never come InService name the last failed
// scaling activity; nodes that never get Ready say how many are.
func TestCreateClusterSelfManagedWaitTimesOut(t *testing.T) {
	shortNodeWait(t)
	t.Run("no instances", func(t *testing.T) {
		f := newClusterFake()
		f.asgStuck = true
		f.activities["demo-system"] = []asgtypes.Activity{
			{StatusCode: asgtypes.ScalingActivityStatusCodeInProgress, StatusMessage: aws.String("launching")},
			{StatusCode: asgtypes.ScalingActivityStatusCodeFailed, StatusMessage: aws.String("We currently do not have sufficient m8i.xlarge capacity")},
		}
		err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "sufficient m8i.xlarge capacity") {
			t.Fatalf("want a timeout naming the failed activity, got %v", err)
		}
	})
	t.Run("nodes not Ready", func(t *testing.T) {
		f := newClusterFake()
		f.notReady = true
		err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "0 of 2 nodes ate.dev/pool=system are Ready") {
			t.Fatalf("want a timeout counting the Ready nodes, got %v", err)
		}
	})
}

// DescribeAutoScalingGroups can miss a group for a while after
// CreateAutoScalingGroup: the wait goes on, and a group it never returns is
// named at the timeout, not at the first read.
func TestCreateClusterSelfManagedWaitsForTheNewAutoScalingGroup(t *testing.T) {
	t.Run("returned late", func(t *testing.T) {
		f := newClusterFake()
		f.asgLag = 3
		if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
			t.Fatalf("a group not returned yet stopped the wait: %v", err)
		}
		for g, n := range f.asgHidden {
			if n != 0 {
				t.Errorf("%s: %d reads left that miss it", g, n)
			}
		}
	})
	t.Run("never returned", func(t *testing.T) {
		shortNodeWait(t)
		f := newClusterFake()
		f.asgLag = 1 << 30
		err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "DescribeAutoScalingGroups does not return Auto Scaling group demo-system") {
			t.Fatalf("want a timeout naming the group, got %v", err)
		}
		if n := countOf(f.reads, "DescribeAutoScalingGroups"); n < 3 {
			t.Errorf("stopped after %d reads", n)
		}
	})
}

// The node lists of the wait: a blip (no answer, 429, a 5xx) is "not yet"
// and the wait goes on; 401 or 403 stops it at once; a wait that only gets
// blips times out with the last one.
func TestCreateClusterSelfManagedWaitRidesOutBlips(t *testing.T) {
	answered := func(status int) error {
		return &kubeError{Status: status, err: fmt.Errorf("list the nodes of cluster demo: the Kubernetes API answered %d %s", status, http.StatusText(status))}
	}
	t.Run("blips", func(t *testing.T) {
		f := newClusterFake()
		f.kubeErrs = []error{answered(503), answered(429), &kubeError{err: errors.New("list the nodes of cluster demo: connection reset by peer")}, answered(500)}
		if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
			t.Fatalf("a blip stopped the wait: %v", err)
		}
		if len(f.kubeErrs) != 0 {
			t.Errorf("%d answers not read", len(f.kubeErrs))
		}
	})
	for _, status := range []int{401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newClusterFake()
			f.kubeErrs = []error{answered(status)}
			err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) || countOf(f.reads, "ListNodes") != 1 {
				t.Errorf("want the %d at once, got %v after %d lists", status, err, countOf(f.reads, "ListNodes"))
			}
		})
	}
	t.Run("a blip, then nodes not Ready", func(t *testing.T) {
		shortNodeWait(t)
		f := newClusterFake()
		f.kubeErrs, f.notReady = []error{answered(503)}, true
		err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "0 of 2 nodes ate.dev/pool=system are Ready") || strings.Contains(err.Error(), "the last list") {
			t.Errorf("want a timeout counting the Ready nodes of the last answer, got %v", err)
		}
	})
	t.Run("only blips", func(t *testing.T) {
		shortNodeWait(t)
		f := newClusterFake()
		f.kubeDown = answered(503)
		err := clusterTestRun(t, f, false, selfManaged).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "the last list of the nodes failed") ||
			!strings.Contains(err.Error(), "503 Service Unavailable") || strings.Contains(err.Error(), "are Ready") {
			t.Errorf("want a timeout with the last failed list, got %v", err)
		}
	})
}

// A template of a self-managed group still to make that lost its instance
// profile or security group gets a new version with them.
func TestCreateClusterSelfManagedTemplateDrift(t *testing.T) {
	for name, edit := range map[string]func(*ec2types.RequestLaunchTemplateData){
		"instance profile": func(d *ec2types.RequestLaunchTemplateData) { d.IamInstanceProfile = nil },
		"security group":   func(d *ec2types.RequestLaunchTemplateData) { d.SecurityGroupIds = []string{"sg-other"} },
		"instance type":    func(d *ec2types.RequestLaunchTemplateData) { d.InstanceType = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newClusterFake()
			if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, lt := range f.lts {
				if aws.ToString(lt.lt.LaunchTemplateName) == "demo-gvisor-nodes" {
					d := *lt.versions[0]
					edit(&d)
					lt.versions[0] = &d
				}
			}
			delete(f.asgs, "demo-gvisor")
			f.writes = nil
			if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if want := []string{"CreateLaunchTemplateVersion demo-gvisor-nodes", "CreateAutoScalingGroup demo-gvisor"}; !slices.Equal(f.writes, want) {
				t.Errorf("writes %v, want %v", f.writes, want)
			}
			if v := aws.ToString(f.asgIn["demo-gvisor"].LaunchTemplate.Version); v != "2" {
				t.Errorf("the Auto Scaling group uses version %s, want 2", v)
			}
		})
	}
}

// IAM may not find the node role for a while after CreateRole: the first
// run waits it out and adds the role to the new profile.
func TestCreateClusterSelfManagedWaitsForTheNewRole(t *testing.T) {
	f := newClusterFake()
	f.roleLag = 3
	if err := clusterTestRun(t, f, false, selfManaged).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := f.profiles[nodeProfile]; p == nil || !slices.Equal(p.roles, []string{"demo-us-west-2-eks-node"}) || f.roleLag != 0 {
		t.Errorf("instance profile %+v, lag left %d", p, f.roleLag)
	}
}

// retryRoleNotFound retries a NoSuchEntity for the role only: not one for
// the profile, which has the role's name, even when that name has "role" in
// it, and not another error.
func TestRetryRoleNotFound(t *testing.T) {
	pollEvery = 0
	for _, role := range []string{"demo-us-west-2-eks-node", "myrole-us-west-2-eks-node"} {
		calls := 0
		err := retryRoleNotFound(context.Background(), role, func() error {
			if calls++; calls < 3 {
				return &smithy.GenericAPIError{Code: "NoSuchEntity", Message: "The role with name " + role + " cannot be found."}
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Errorf("%s: err %v after %d calls, want success on the third", role, err, calls)
		}
		for _, other := range []error{
			&smithy.GenericAPIError{Code: "NoSuchEntity", Message: "Instance Profile " + role + " cannot be found."},
			&smithy.GenericAPIError{Code: "NoSuchEntity", Message: "The role with name someone-elses cannot be found."},
			&smithy.GenericAPIError{Code: "LimitExceeded", Message: "Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1 (role " + role + ")"},
		} {
			calls = 0
			if err := retryRoleNotFound(context.Background(), role, func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
				t.Errorf("%s: %v after %d calls, want it at once", role, err, calls)
			}
		}
	}
}

// retryNewProfile retries only Auto Scaling's "invalid instance profile".
func TestRetryNewProfile(t *testing.T) {
	pollEvery = 0
	calls := 0
	err := retryNewProfile(context.Background(), func() error {
		if calls++; calls < 3 {
			return &smithy.GenericAPIError{Code: "ValidationError", Message: "Value (x) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name"}
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("err %v after %d calls, want success on the third", err, calls)
	}
	calls = 0
	other := &smithy.GenericAPIError{Code: "ValidationError", Message: "Max bound, 4, must be greater than or equal to min bound, 5"}
	if err := retryNewProfile(context.Background(), func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Errorf("another ValidationError: %v after %d calls, want it at once", err, calls)
	}
}
