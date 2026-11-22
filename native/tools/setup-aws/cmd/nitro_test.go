package cmd

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// nitroAMI is a Nitro Sandbox AMI the fake account may launch.
const nitroAMI = "ami-0a17000000000000e"

// withNitro wants the nitro group next to the default three, on nitroAMI.
func withNitro(c *clusterConfig) {
	c.NodeGroups = "system,gvisor,microvm,nitro"
	c.NitroAMIID, c.NitroInstanceType, c.NitroNodes = nitroAMI, "m8i.xlarge", 1
}

// nitroFake is a fresh account that holds nitroAMI.
func nitroFake() *clusterFake {
	f := newClusterFake()
	im := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 30)
	im.ImageId, im.Name = aws.String(nitroAMI), aws.String("amazon-eks-node-al2023-x86_64-pod-isolation-1.37")
	f.images[nitroAMI] = im
	return f
}

// shortNitroWait bounds waitNitroCapacity for the test.
func shortNitroWait(t *testing.T) {
	t.Helper()
	prev := nitroWaitTimeout
	nitroWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { nitroWaitTimeout = prev })
}

// nitroCreates are what a fresh account gets with the nitro group too: its
// launch template, and its node group or Auto Scaling group.
func nitroCreates(base map[string]int, selfManaged bool) map[string]int {
	m := maps.Clone(base)
	m["CreateLaunchTemplate demo-nitro-nodes"] = 1
	if selfManaged {
		m["CreateAutoScalingGroup demo-nitro"] = 1
	} else {
		m["CreateNodegroup nitro"] = 1
	}
	return m
}

// nodeLabelsFlag is the --node-labels flag of a template's NodeConfig.
func nodeLabelsFlag(t *testing.T, d *ec2types.RequestLaunchTemplateData) string {
	t.Helper()
	_, nc := parseUserData(t, aws.ToString(d.UserData))
	flags := nc["spec"].(map[string]any)["kubelet"].(map[string]any)["flags"].([]any)
	if len(flags) != 1 {
		t.Fatalf("kubelet flags %v", flags)
	}
	return flags[0].(string)
}

// The default (NODE_GROUPS unset) sends the same requests and writes
// whatever the nitro settings hold: they are read only for the nitro group.
// Nothing it makes has Nitro Enclaves, and the Kubernetes API is never
// asked. A fresh or complete default run reads no Auto Scaling group; one
// that adds a group to an existing cluster (checkGroupType) asks for every
// name in nodegroups.All, demo-nitro too: that read is the default's one
// new payload.
func TestCreateClusterNitroSettingsLeaveTheDefaultAlone(t *testing.T) {
	odd := func(c *clusterConfig) { c.NitroAMIID, c.NitroInstanceType, c.NitroNodes = "ami-bad", "t3.xlarge", 0 }
	run := func(f *clusterFake, edit ...func(*clusterConfig)) *clusterFake {
		if err := clusterTestRun(t, f, false, edit...).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return f
	}
	plain, oddRun := run(newClusterFake()), run(newClusterFake(), odd)
	if !slices.Equal(plain.reads, oddRun.reads) || !slices.Equal(plain.inputs, oddRun.inputs) || !slices.Equal(plain.writes, oddRun.writes) {
		t.Errorf("the nitro settings changed the default run:\nrequests %v\n      vs %v\nwrites %v\n    vs %v", plain.inputs, oddRun.inputs, plain.writes, oddRun.writes)
	}
	if len(plain.inputs) == 0 || countOf(plain.reads, "DescribeAutoScalingGroups") != 0 {
		t.Errorf("requests recorded %d, Auto Scaling group reads %d", len(plain.inputs), countOf(plain.reads, "DescribeAutoScalingGroups"))
	}
	if got := creates(plain.writes); !maps.Equal(got, freshCreates) {
		t.Errorf("creates:\n got %v\nwant %v", got, freshCreates)
	}
	for _, w := range plain.writes {
		if strings.Contains(w, "nitro") {
			t.Errorf("a default run wrote %s", w)
		}
	}
	for _, in := range plain.inputs {
		if strings.Contains(in, "nitro") {
			t.Errorf("a fresh default run asked for %s", in)
		}
	}
	for _, lt := range plain.lts {
		if d := lt.versions[len(lt.versions)-1]; d.EnclaveOptions != nil {
			t.Errorf("%s has enclave options", aws.ToString(lt.lt.LaunchTemplateName))
		}
	}
	if plain.kubeCalls != 0 || countOf(plain.reads, "DescribeImages") != 0 {
		t.Errorf("Kubernetes clients %d, DescribeImages %d", plain.kubeCalls, countOf(plain.reads, "DescribeImages"))
	}

	adds := func(edit ...func(*clusterConfig)) *clusterFake {
		f := run(newClusterFake(), func(c *clusterConfig) { c.NodeGroups = "system,gvisor" })
		f.reads, f.inputs, f.writes = nil, nil, nil
		return run(f, edit...)
	}
	a, b := adds(), adds(odd)
	if !slices.Equal(a.inputs, b.inputs) || !slices.Equal(a.writes, b.writes) || !slices.Equal(a.writes, []string{"CreateLaunchTemplate demo-microvm-nested-virt", "CreateNodegroup microvm", "CreateOrUpdateTags eks-microvm-asg"}) {
		t.Errorf("adding microvm:\nrequests %v\n      vs %v\nwrites %v vs %v", a.inputs, b.inputs, a.writes, b.writes)
	}
	var asg []string
	for _, in := range a.inputs {
		if strings.HasPrefix(in, "DescribeAutoScalingGroups ") {
			asg = append(asg, in)
		}
	}
	if want := `"AutoScalingGroupNames":["demo-system","demo-gvisor","demo-microvm","demo-nitro"]`; len(asg) != 1 || !strings.Contains(asg[0], want) {
		t.Errorf("Auto Scaling group reads %q, want one with %s", asg, want)
	}
}

// The managed nitro group: NITRO_AMI_ID in its template with Nitro Enclaves
// on, no AMI type, its labels and taint, one node and room for two more;
// the run waits for the Nitro sandbox capacity; the other groups stay on
// EKS's AMI; a rerun writes nothing.
func TestCreateClusterNitroManaged(t *testing.T) {
	f := nitroFake()
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got, want := creates(f.writes), nitroCreates(freshCreates, false); !maps.Equal(got, want) {
		t.Errorf("creates:\n got %v\nwant %v", got, want)
	}
	in := f.ngIn["nitro"]
	if in.AmiType != "" || !maps.Equal(in.Labels, map[string]string{"ate.dev/pool": "nitro"}) || slices.Equal(in.InstanceTypes, []string{"m8i.xlarge"}) == false {
		t.Errorf("nitro: AMI type %q, labels %v, types %v", in.AmiType, in.Labels, in.InstanceTypes)
	}
	if len(in.Taints) != 1 || aws.ToString(in.Taints[0].Key) != "ate.dev/sandboxClass" || aws.ToString(in.Taints[0].Value) != "nitro" || in.Taints[0].Effect != ekstypes.TaintEffectNoSchedule {
		t.Errorf("nitro taints %+v", in.Taints)
	}
	if sc := in.ScalingConfig; aws.ToInt32(sc.MinSize) != 1 || aws.ToInt32(sc.DesiredSize) != 1 || aws.ToInt32(sc.MaxSize) != 3 {
		t.Errorf("nitro size %d/%d/%d", aws.ToInt32(sc.MinSize), aws.ToInt32(sc.DesiredSize), aws.ToInt32(sc.MaxSize))
	}
	d := latestData(t, f, "demo-nitro-nodes")
	if d.EnclaveOptions == nil || !aws.ToBool(d.EnclaveOptions.Enabled) || aws.ToString(d.ImageId) != nitroAMI || d.CpuOptions != nil {
		t.Errorf("nitro template: enclave %+v, image %s, cpu %+v", d.EnclaveOptions, aws.ToString(d.ImageId), d.CpuOptions)
	}
	if d.MetadataOptions.HttpTokens != ec2types.LaunchTemplateHttpTokensStateRequired || aws.ToInt32(d.MetadataOptions.HttpPutResponseHopLimit) != 1 {
		t.Errorf("nitro IMDS %+v", d.MetadataOptions)
	}
	if b := d.BlockDeviceMappings[0]; aws.ToString(b.DeviceName) != "/dev/xvda" || aws.ToInt32(b.Ebs.VolumeSize) != 100 || !aws.ToBool(b.Ebs.Encrypted) {
		t.Errorf("nitro root %s %+v", aws.ToString(b.DeviceName), b.Ebs)
	}
	flags := nodeLabelsFlag(t, d)
	for _, want := range []string{"ate.dev/pool=nitro", "eks.amazonaws.com/nitro-sandbox=true", "eks.amazonaws.com/nodegroup=nitro", "eks.amazonaws.com/nodegroup-image=" + nitroAMI} {
		if !strings.Contains(flags, want) {
			t.Errorf("nitro --node-labels lacks %s: %s", want, flags)
		}
	}
	_, nc := parseUserData(t, aws.ToString(d.UserData))
	taints, _ := nc["spec"].(map[string]any)["kubelet"].(map[string]any)["config"].(map[string]any)["registerWithTaints"].([]any)
	if len(taints) != 1 || taints[0].(map[string]any)["key"] != "ate.dev/sandboxClass" || taints[0].(map[string]any)["value"] != "nitro" {
		t.Errorf("nitro registerWithTaints %v", taints)
	}
	for _, g := range defaultGroups {
		if f.ngIn[g].AmiType != ekstypes.AMITypesAl2023X8664Standard {
			t.Errorf("%s: AMI type %q", g, f.ngIn[g].AmiType)
		}
		if lt := latestData(t, f, nodeTemplateName("demo", g)); lt.EnclaveOptions != nil || lt.ImageId != nil || lt.UserData != nil {
			t.Errorf("%s: enclave %v, image %v, user data %v", g, lt.EnclaveOptions, lt.ImageId, lt.UserData)
		}
	}
	if f.kubeCalls != 1 || countOf(f.reads, "ListNodes") < 1 || countOf(f.reads, "GetParameter") != 0 || countOf(f.reads, "DescribeImages") != 1 {
		t.Errorf("Kubernetes clients %d, node lists %d, GetParameter %d, DescribeImages %d",
			f.kubeCalls, countOf(f.reads, "ListNodes"), countOf(f.reads, "GetParameter"), countOf(f.reads, "DescribeImages"))
	}
	if !strings.Contains(logs.String(), "the nitro nodes advertise Nitro sandbox capacity") {
		t.Errorf("no capacity line:\n%s", logs)
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("rerun: %v, writes %v", err, f.writes)
	}
}

// The self-managed nitro group: an Auto Scaling group from a template with
// NITRO_AMI_ID, Nitro Enclaves, the instance profile and the NodeConfig
// labels; the other groups get the SSM AMI, read once; the wait checks the
// capacity after the nodes are Ready.
func TestCreateClusterNitroSelfManaged(t *testing.T) {
	f := nitroFake()
	if err := clusterTestRun(t, f, false, selfManaged, withNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := creates(f.writes), nitroCreates(selfManagedCreates(), true); !maps.Equal(got, want) {
		t.Errorf("creates:\n got %v\nwant %v", got, want)
	}
	d := latestData(t, f, "demo-nitro-nodes")
	if d.EnclaveOptions == nil || !aws.ToBool(d.EnclaveOptions.Enabled) || aws.ToString(d.ImageId) != nitroAMI ||
		d.IamInstanceProfile == nil || aws.ToString(d.IamInstanceProfile.Name) != nodeProfile {
		t.Errorf("nitro template: enclave %+v, image %s, profile %v", d.EnclaveOptions, aws.ToString(d.ImageId), d.IamInstanceProfile)
	}
	flags := nodeLabelsFlag(t, d)
	if !strings.Contains(flags, "ate.dev/pool=nitro") || !strings.Contains(flags, "eks.amazonaws.com/nitro-sandbox=true") || strings.Contains(flags, "eks.amazonaws.com/nodegroup") {
		t.Errorf("nitro --node-labels %s", flags)
	}
	if aws.ToString(latestData(t, f, "demo-gvisor-nodes").ImageId) != eksAMIx86 || latestData(t, f, "demo-gvisor-nodes").EnclaveOptions != nil {
		t.Error("the gvisor group lost the EKS optimized AMI or got enclaves")
	}
	in := f.asgIn["demo-nitro"]
	if in == nil || aws.ToInt32(in.MinSize) != 1 || aws.ToInt32(in.DesiredCapacity) != 1 || aws.ToInt32(in.MaxSize) != 3 {
		t.Fatalf("Auto Scaling group demo-nitro %+v", in)
	}
	if n := countOf(f.reads, "GetParameter"); n != 1 {
		t.Errorf("GetParameter %d times, want 1 (the nitro group reads none)", n)
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false, selfManaged, withNitro).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("rerun: %v, writes %v", err, f.writes)
	}
}

// NITRO_AMI_ID empty with the nitro group wanted stops the run right after
// the cluster read, with the build command for the cluster's version and
// the AWS doc; nothing is written.
func TestCreateClusterNitroNeedsItsAMI(t *testing.T) {
	f := nitroFake()
	err := clusterTestRun(t, f, false, withNitro, func(c *clusterConfig) { c.NitroAMIID = "" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "NITRO_AMI_ID is empty") ||
		!strings.Contains(err.Error(), ": build one in a clone of https://github.com/awslabs/amazon-eks-ami: make k8s=1.37 os_distro=al2023 enable_pod_isolation=true, see "+nodegroups.NitroAMIDoc+"; then set NITRO_AMI_ID") {
		t.Fatalf("no NITRO_AMI_ID: %v", err)
	}
	if len(f.writes) != 0 || !slices.Equal(f.reads, []string{"DescribeCluster"}) {
		t.Errorf("writes %v, reads %v", f.writes, f.reads)
	}
	// An existing cluster: the build is for its version.
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cluster.Version, f.writes = aws.String("1.38"), nil
	err = clusterTestRun(t, f, false, withNitro, func(c *clusterConfig) { c.NitroAMIID = " " }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "make k8s=1.38 ") || len(f.writes) != 0 {
		t.Errorf("existing 1.38 cluster: %v, writes %v", err, f.writes)
	}
}

// Preflight refuses, before any write: an instance type without Nitro
// Enclaves (with the command that lists those with them), an AMI that is
// missing, malformed, of the other architecture or too big for the root
// volume, a denied AMI read, and no nodes.
func TestCreateClusterNitroPreflightRefusals(t *testing.T) {
	for _, c := range []struct {
		name, want string
		edit       func(*clusterFake, *clusterConfig)
	}{
		{"no Nitro Enclaves", "t3.xlarge does not support Nitro Enclaves, which the nitro node group needs",
			func(_ *clusterFake, c *clusterConfig) { c.NitroInstanceType = "t3.xlarge" }},
		{"no Nitro Enclaves, the list command", "Name=nitro-enclaves-support,Values=supported",
			func(_ *clusterFake, c *clusterConfig) { c.NitroInstanceType = "t3.xlarge" }},
		{"no such AMI", "NITRO_AMI_ID=ami-0b17000000000000e: no such AMI",
			func(_ *clusterFake, c *clusterConfig) { c.NitroAMIID = "ami-0b17000000000000e" }},
		{"not an AMI ID", "NITRO_AMI_ID=nitro is not an AMI ID",
			func(_ *clusterFake, c *clusterConfig) { c.NitroAMIID = "nitro" }},
		{"another architecture", "NITRO_AMI_ID=" + nitroAMI + " is arm64, but the nitro node group's m8i.xlarge runs x86_64",
			func(f *clusterFake, _ *clusterConfig) {
				im := f.images[nitroAMI]
				im.Architecture = ec2types.ArchitectureValuesArm64
				f.images[nitroAMI] = im
			}},
		{"a root snapshot over 100 GB", "more than the nitro node group's 100 GB root volume",
			func(f *clusterFake, _ *clusterConfig) {
				im := f.images[nitroAMI]
				im.BlockDeviceMappings[0].Ebs.VolumeSize = aws.Int32(120)
				f.images[nitroAMI] = im
			}},
		{"a denied AMI read", "AccessDenied", func(f *clusterFake, _ *clusterConfig) { f.deny["DescribeImages"] = "AccessDenied" }},
		{"no nodes", "the nitro node group needs at least one node", func(_ *clusterFake, c *clusterConfig) { c.NitroNodes = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := nitroFake()
			err := clusterTestRun(t, f, false, withNitro, func(cc *clusterConfig) { c.edit(f, cc) }).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want %q, got %v", c.want, err)
			}
			if len(f.writes) != 0 {
				t.Errorf("writes before the refusal: %v", f.writes)
			}
		})
	}
	// The same AMI and instance type pass: the refusals are the edits'.
	if err := clusterTestRun(t, nitroFake(), true, withNitro).run(context.Background()); err != nil {
		t.Errorf("plan with a good nitro group: %v", err)
	}
}

// Both directions, on both group types: nodes that advertise
// eks.amazonaws.com/nitro-sandbox pass; Ready nodes that advertise none, or
// 0, stop the run with what NITRO_AMI_ID must be. Nodes that are only not
// Ready, or have not all joined, stop it too, without blaming the AMI.
func TestCreateClusterNitroWaitsForSandboxCapacity(t *testing.T) {
	shortNitroWait(t)
	const verdict = "the nodes of AMI " + nitroAMI + " advertise no Nitro sandbox capacity; NITRO_AMI_ID must be a Nitro Sandbox AMI " +
		"(build one in a clone of https://github.com/awslabs/amazon-eks-ami: make k8s=1.37 os_distro=al2023 enable_pod_isolation=true, see " + nodegroups.NitroAMIDoc + ")"
	const notYet = "the nitro node group's nodes have not all joined and become Ready; run create cluster again to wait for them"
	for _, c := range []struct {
		name     string
		self     bool
		alloc    string
		notReady bool
		missing  int
		want     string // "" passes
		ami      bool   // the AMI is blamed
	}{
		{"managed, capacity", false, "1", false, 0, "", false},
		{"self-managed, capacity", true, "4", false, 0, "", false},
		{"managed, none advertised", false, "", false, 0, "node ip-nitro-0 has no allocatable eks.amazonaws.com/nitro-sandbox", true},
		{"self-managed, none advertised", true, "", false, 0, "has no allocatable eks.amazonaws.com/nitro-sandbox", true},
		{"managed, zero", false, "0", false, 0, `node ip-nitro-0 has allocatable eks.amazonaws.com/nitro-sandbox "0"`, true},
		{"self-managed, zero", true, "0", false, 0, `has allocatable eks.amazonaws.com/nitro-sandbox "0"`, true},
		{"managed, not Ready", false, "1", true, 0, "node ip-nitro-0 is not Ready", false},
		{"managed, not Ready and none advertised", false, "", true, 0, "node ip-nitro-0 is not Ready", false},
		{"managed, a node short", false, "1", false, 1, "1 of 2 nodes have joined", false},
		{"managed, a node short and none advertised", false, "", false, 1, "1 of 2 nodes have joined; node ip-nitro-0 has no allocatable", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := nitroFake()
			f.nitroAlloc, f.nitroNotReady = c.alloc, c.notReady
			edit := []func(*clusterConfig){withNitro}
			if c.missing > 0 {
				f.nitroMissing = c.missing
				edit = append(edit, func(cc *clusterConfig) { cc.NitroNodes = 2 })
			}
			if c.self {
				edit = append(edit, selfManaged)
			}
			err := clusterTestRun(t, f, false, edit...).run(context.Background())
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if blamed := strings.Contains(err.Error(), verdict); blamed != c.ami || strings.Contains(err.Error(), "NITRO_AMI_ID") != c.ami {
				t.Errorf("AMI blamed %v, want %v: %v", blamed, c.ami, err)
			}
			if said := strings.Contains(err.Error(), notYet); said == c.ami {
				t.Errorf("not-yet advice %v, want %v: %v", said, !c.ami, err)
			}
		})
	}
	// A node list the API refuses stops the wait at once; a blip does not.
	f := nitroFake()
	f.kubeErrs = []error{&kubeError{Status: http.StatusForbidden, err: fmt.Errorf("list the nodes of cluster demo: the Kubernetes API answered 403 Forbidden")}}
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err == nil || !strings.Contains(err.Error(), "403") || countOf(f.reads, "ListNodes") != 1 {
		t.Errorf("403: %v after %d lists", err, countOf(f.reads, "ListNodes"))
	}
	f = nitroFake()
	f.kubeErrs = []error{&kubeError{Status: http.StatusServiceUnavailable, err: fmt.Errorf("503")}}
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil || countOf(f.reads, "ListNodes") != 2 {
		t.Errorf("a 503 blip: %v after %d lists", err, countOf(f.reads, "ListNodes"))
	}
}

// A wait that gets only blips times out with the last one, and blames
// neither the AMI nor the nodes, which it never saw; a blip before an answer
// is forgotten, and the timeout names what that answer showed.
func TestCreateClusterNitroWaitTimesOutOnBlips(t *testing.T) {
	shortNitroWait(t)
	blip := &kubeError{Status: http.StatusServiceUnavailable, err: fmt.Errorf("list the nodes of cluster demo: the Kubernetes API answered 503 Service Unavailable")}
	f := nitroFake()
	f.kubeDown = blip
	err := clusterTestRun(t, f, false, withNitro).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") ||
		!strings.Contains(err.Error(), "the last list of the nodes failed: list the nodes of cluster demo: the Kubernetes API answered 503 Service Unavailable") ||
		strings.Contains(err.Error(), "NITRO_AMI_ID") || strings.Contains(err.Error(), "have not all joined") {
		t.Errorf("want a timeout with the last failed list, got %v", err)
	}
	if n := countOf(f.reads, "ListNodes"); n < 2 {
		t.Errorf("stopped after %d lists", n)
	}

	f = nitroFake()
	f.nitroAlloc = ""
	f.kubeErrs = []error{blip}
	err = clusterTestRun(t, f, false, withNitro).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "node ip-nitro-0 has no allocatable eks.amazonaws.com/nitro-sandbox") ||
		!strings.Contains(err.Error(), "NITRO_AMI_ID must be a Nitro Sandbox AMI") || strings.Contains(err.Error(), "the last list") {
		t.Errorf("want a timeout naming the last answer, got %v", err)
	}
}

// An owned nitro template without Nitro Enclaves (made by hand, or by an
// older run) gets a version with them: the drift check compares them.
func TestCreateClusterNitroTemplateDrift(t *testing.T) {
	f := nitroFake()
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := latestData(t, f, "demo-nitro-nodes")
	d.EnclaveOptions = nil
	delete(f.ngs, "nitro")
	delete(f.ngIn, "nitro")
	f.writes = nil
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"CreateLaunchTemplateVersion demo-nitro-nodes", "CreateNodegroup nitro"}; !slices.Equal(f.writes, want) {
		t.Errorf("writes %v, want %v", f.writes, want)
	}
	if d := latestData(t, f, "demo-nitro-nodes"); d.EnclaveOptions == nil || !aws.ToBool(d.EnclaveOptions.Enabled) {
		t.Error("the new version has no Nitro Enclaves")
	}
	if v := aws.ToString(f.ngIn["nitro"].LaunchTemplate.Version); v != "2" {
		t.Errorf("nitro node group on version %s, want 2", v)
	}
}

// Another NITRO_AMI_ID on an existing nitro group keeps the group and says
// so, naming NITRO_AMI_ID and both AMIs.
func TestCreateClusterNitroGroupKeepsItsAMI(t *testing.T) {
	f := nitroFake()
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	newer := "ami-0a17000000000000f"
	im := f.images[nitroAMI]
	im.ImageId = aws.String(newer)
	f.images[newer] = im
	f.writes = nil
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withNitro, func(c *clusterConfig) { c.NitroAMIID = newer }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || !strings.Contains(logs.String(), "another AMI than NITRO_AMI_ID") ||
		!strings.Contains(logs.String(), "runs="+nitroAMI) || !strings.Contains(logs.String(), "NITRO_AMI_ID="+newer) {
		t.Errorf("writes %v\n%s", f.writes, logs)
	}
}

// When an existing custom-AMI group's launch template version is gone, its
// AMI cannot be read: the warning says so, and does not say the group runs
// EKS's own AMI.
func TestCreateClusterGroupLaunchTemplateGone(t *testing.T) {
	f := nitroFake()
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := aws.ToString(f.ngIn["nitro"].LaunchTemplate.Id)
	if f.lts[id] == nil {
		t.Fatalf("no launch template %s", id)
	}
	delete(f.lts, id)
	f.writes = nil
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || !strings.Contains(logs.String(), "launch template version is gone, so which AMI it runs is unknown") ||
		strings.Contains(logs.String(), "another AMI than") || !strings.Contains(logs.String(), "launchTemplate="+id) {
		t.Errorf("writes %v\n%s", f.writes, logs)
	}
}

// Teardown and inventory find the nitro group by its derived names: the
// managed node group, its launch template, and a template of that name
// without the owner tag, which stops the teardown before any delete.
func TestTeardownFindsTheNitroNodeGroup(t *testing.T) {
	f := fullDeployment()
	f.put(&tdObj{kind: "nodegroup", id: "nitro", role: roleARN("demo-us-west-2-eks-node"), tags: own()})
	f.put(&tdObj{kind: kLT, id: "lt-4", name: "demo-nitro-nodes", tags: own()})
	e, _, logs := tdTestEnv(t, f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"EKS node group nitro", "launch template demo-nitro-nodes (lt-4)"} {
		if !hasRow(rows, want) {
			t.Errorf("inventory lacks %q", want)
		}
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	before(t, f.calls, "DeleteNodegroup nitro", "DeleteCluster demo")
	before(t, f.calls, "DeleteNodegroup nitro", "DeleteLaunchTemplate lt-4")
	if f.objs["nodegroup|nitro"] != nil || f.objs[kLT+"|lt-4"] != nil {
		t.Errorf("left: %v", f.remaining())
	}

	// Found by name alone: a template without the owner tag.
	f = fullDeployment()
	f.put(&tdObj{kind: kLT, id: "lt-9", name: "demo-nitro-nodes", tags: map[string]string{}})
	e, _, _ = tdTestEnv(t, f, false, false)
	if rows, err := e.inventory(context.Background()); err != nil || !hasRow(rows, "launch template demo-nitro-nodes (lt-9)") {
		t.Errorf("inventory of a foreign demo-nitro-nodes: %v", err)
	}
	err = e.teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "demo-nitro-nodes (lt-9)") || !strings.Contains(err.Error(), "not owned") || len(f.calls) != 0 {
		t.Errorf("a foreign demo-nitro-nodes: %v, calls %v", err, f.calls)
	}

	// The self-managed nitro group's Auto Scaling group (selfManagedDeployment
	// has one per group create cluster knows).
	f = selfManagedDeployment()
	e, _, logs = tdTestEnv(t, f, false, false)
	if rows, err := e.inventory(context.Background()); err != nil || !hasRow(rows, kASG+" demo-nitro") {
		t.Errorf("inventory lacks Auto Scaling group demo-nitro: %v", err)
	}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	before(t, f.calls, "DeleteAutoScalingGroup demo-nitro", "DeleteCluster demo")
	if !slices.Contains(e.ltNames(), "demo-nitro-nodes") || !slices.Contains(e.asgNames(), "demo-nitro") {
		t.Errorf("derived names %v %v", e.ltNames(), e.asgNames())
	}
}

// A default deployment, which never had a nitro group, is torn down and
// inventoried with the nitro group's derived names in each read of derived
// names: its Auto Scaling group <cluster>-nitro and its launch template
// <cluster>-nitro-nodes, after the other groups', in nodegroups.All's order.
// That is what lets teardown find a nitro group whatever the run that made
// it was told; it is also why a foreign template of that name stops it
// (TestTeardownFindsTheNitroNodeGroup).
func TestTeardownAsksForTheNitroNames(t *testing.T) {
	wantASG := []string{"demo-system", "demo-gvisor", "demo-microvm", "demo-nitro"}
	wantLT := []string{"demo-system-nodes", "demo-gvisor-nodes", "demo-microvm-nested-virt", "demo-nitro-nodes"}
	for _, op := range []string{"inventory", "teardown"} {
		f := fullDeployment()
		e, _, logs := tdTestEnv(t, f, false, false)
		var err error
		if op == "inventory" {
			_, err = e.inventory(context.Background())
		} else {
			err = e.teardown(context.Background())
		}
		if err != nil {
			t.Fatalf("%s: %v\n%s", op, err, logs)
		}
		for read, want := range map[string][]string{"DescribeAutoScalingGroups": wantASG, "DescribeLaunchTemplates": wantLT} {
			got := f.names[read]
			if len(got) == 0 {
				t.Errorf("%s: no %s by name", op, read)
			}
			for _, names := range got {
				if !slices.Equal(names, want) {
					t.Errorf("%s: %s asks for %v, want %v", op, read, names, want)
				}
			}
		}
	}
}

// The nitro group never reads the EKS optimized AMI from SSM: a
// self-managed nitro group alone (Auto Mode) works with SSM denied, and an
// arm64 nitro group next to an x86_64 system group reads the x86_64 AMI
// only.
func TestCreateClusterNitroReadsNoSSM(t *testing.T) {
	f := nitroFake()
	f.deny["GetParameter"] = "AccessDenied"
	if err := clusterTestRun(t, f, false, selfManaged, withNitro, func(c *clusterConfig) { c.NodeGroups = "nitro" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countOf(f.reads, "GetParameter") != 0 || aws.ToString(latestData(t, f, "demo-nitro-nodes").ImageId) != nitroAMI || f.asgIn["demo-nitro"] == nil {
		t.Errorf("reads %v", f.reads)
	}
	f = nitroFake()
	im := f.images[nitroAMI]
	im.Architecture = ec2types.ArchitectureValuesArm64
	f.images[nitroAMI] = im
	if err := clusterTestRun(t, f, false, selfManaged, withNitro, func(c *clusterConfig) {
		c.NodeGroups, c.NitroInstanceType = "system,nitro", "m8g.2xlarge"
	}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countOf(f.reads, "GetParameter"); n != 1 || aws.ToString(latestData(t, f, "demo-system-nodes").ImageId) != eksAMIx86 ||
		aws.ToString(latestData(t, f, "demo-nitro-nodes").ImageId) != nitroAMI {
		t.Errorf("GetParameter %d times, want the x86_64 one only", n)
	}
}

// The nitro group's template is never made without its AMI: before
// preflight has read NITRO_AMI_ID there is none, and that is an error, not
// a template on EKS's AMI.
func TestNitroTemplateNeedsItsAMI(t *testing.T) {
	r := clusterTestRun(t, nitroFake(), true, withNitro)
	gs := r.groups()
	g := gs[len(gs)-1]
	if g.Name != "nitro" || g.Image != nil {
		t.Fatalf("group %s, image %v", g.Name, g.Image)
	}
	if _, err := r.launchTemplateData(g); err == nil || !strings.Contains(err.Error(), "internal: node group nitro has no AMI") {
		t.Errorf("no AMI: %v", err)
	}
}
