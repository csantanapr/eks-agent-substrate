package cmd

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

func testClusterConfig() clusterConfig {
	return clusterConfig{KubernetesVersion: "1.37", AutoMode: "on", PublicAccessCIDRs: "203.0.113.0/24",
		SystemInstanceType: "m8i.xlarge", GvisorInstanceType: "m8i.2xlarge", MicrovmInstanceType: "m8i.2xlarge",
		SystemNodes: 2, GvisorNodes: 2, MicrovmNodes: 2}
}

// clusterTestRun is a `create cluster` run of cluster "demo" in us-west-2
// against f. The shared Clients get f's IAM too, so ownedRole reads it.
func clusterTestRun(t *testing.T, f *clusterFake, plan bool, edit ...func(*clusterConfig)) *clusterRun {
	t.Helper()
	pollEvery = 0
	// The fake cluster has no endpoint; testProbe stands in for the probe.
	probeCluster = func(_ context.Context, c *ekstypes.Cluster) error {
		if testProbe != nil {
			return testProbe(c)
		}
		return nil
	}
	cfg = Config{Region: "us-west-2", ClusterName: "demo", Tags: "project=agent-substrate auto-delete=no",
		MinKubernetesMinor: 37, Plan: plan}
	newClients = func(context.Context, Config) (*Clients, error) { return &Clients{STS: fsts{}, IAM: (*fcIAM)(f)}, nil }
	e, err := newEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cc := testClusterConfig()
	for _, fn := range edit {
		fn(&cc)
	}
	return newClusterRun(e, f.clients(), cc)
}

// creates counts the applied writes that make a resource, by resource.
func creates(writes []string) map[string]int {
	m := map[string]int{}
	for _, w := range writes {
		if strings.HasPrefix(w, "Create") || strings.HasPrefix(w, "Allocate") {
			if !strings.HasPrefix(w, "CreateTags ") && !strings.HasPrefix(w, "CreateOrUpdateTags ") && !strings.HasPrefix(w, "CreateRoute ") {
				m[w]++
			}
		}
	}
	return m
}

// freshCreates is what a fresh account gets: each resource once.
var freshCreates = map[string]int{
	"CreateVpc demo-vpc": 1, "CreateInternetGateway demo-igw": 1, "AllocateAddress demo-nat": 1, "CreateNatGateway demo-nat": 1,
	"CreateSubnet demo-public-1": 1, "CreateSubnet demo-public-2": 1, "CreateSubnet demo-public-3": 1,
	"CreateSubnet demo-private-1": 1, "CreateSubnet demo-private-2": 1, "CreateSubnet demo-private-3": 1,
	"CreateRouteTable demo-public": 1, "CreateRouteTable demo-private": 1,
	"CreateRole demo-us-west-2-eks-cluster": 1, "CreateRole demo-us-west-2-eks-auto-node": 1,
	"CreateRole demo-us-west-2-eks-node": 1, "CreateRole demo-us-west-2-ebs-csi-driver": 1,
	"CreateCluster demo": 1, "CreateAccessEntry arn:aws:iam::111122223333:role/Admin": 1,
	"CreateAddon vpc-cni": 1, "CreateAddon kube-proxy": 1, "CreateAddon eks-pod-identity-agent": 1,
	"CreateAddon coredns": 1, "CreateAddon aws-ebs-csi-driver": 1,
	"CreateLaunchTemplate demo-system-nodes": 1, "CreateLaunchTemplate demo-gvisor-nodes": 1, "CreateLaunchTemplate demo-microvm-nested-virt": 1,
	"CreateNodegroup system": 1, "CreateNodegroup gvisor": 1, "CreateNodegroup microvm": 1,
}

func TestCreateClusterFreshAccount(t *testing.T) {
	f := newClusterFake()
	r := clusterTestRun(t, f, false)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := creates(f.writes); !maps.Equal(got, freshCreates) {
		t.Errorf("creates:\n got %v\nwant %v", got, freshCreates)
	}
	if len(r.creates) != len(freshCreates) {
		t.Errorf("logged %d creates, made %d", len(r.creates), len(freshCreates))
	}

	// The cluster: private subnets only, endpoint CIDRs, access entries, Auto Mode.
	in := f.clusterIn
	if len(in.ResourcesVpcConfig.SubnetIds) != 3 {
		t.Errorf("cluster subnets = %v", in.ResourcesVpcConfig.SubnetIds)
	}
	for _, s := range in.ResourcesVpcConfig.SubnetIds {
		if tagOf(f.subnets[s].Tags, "kubernetes.io/role/internal-elb") == nil {
			t.Errorf("cluster subnet %s is not a private subnet", s)
		}
	}
	if c := in.ResourcesVpcConfig; !slices.Equal(c.PublicAccessCidrs, []string{"203.0.113.0/24"}) || !aws.ToBool(c.EndpointPrivateAccess) {
		t.Errorf("endpoint: public CIDRs %v, private %v", c.PublicAccessCidrs, aws.ToBool(c.EndpointPrivateAccess))
	}
	if in.AccessConfig.AuthenticationMode != ekstypes.AuthenticationModeApi || aws.ToBool(in.AccessConfig.BootstrapClusterCreatorAdminPermissions) {
		t.Errorf("access config %+v", in.AccessConfig)
	}
	if !aws.ToBool(in.ComputeConfig.Enabled) || !slices.Equal(in.ComputeConfig.NodePools, []string{"general-purpose", "system"}) ||
		!aws.ToBool(in.StorageConfig.BlockStorage.Enabled) || !aws.ToBool(in.KubernetesNetworkConfig.ElasticLoadBalancing.Enabled) {
		t.Error("Auto Mode is not fully on")
	}
	if aws.ToString(in.Version) != "1.37" {
		t.Errorf("version %s", aws.ToString(in.Version))
	}
	// The caller's assumed-role session maps to its role, cluster-admin, cluster scope.
	pol := f.access["arn:aws:iam::111122223333:role/Admin"]
	if len(pol) != 1 || !strings.HasSuffix(aws.ToString(pol[0].PolicyArn), "/AmazonEKSClusterAdminPolicy") || pol[0].AccessScope.Type != ekstypes.AccessScopeTypeCluster {
		t.Errorf("caller access policies %+v", pol)
	}
	if !slices.Contains(f.roles["demo-us-west-2-eks-cluster"].attached, "arn:aws:iam::aws:policy/AmazonEKSComputePolicy") {
		t.Error("Auto Mode cluster role lacks AmazonEKSComputePolicy")
	}

	// Add-ons: NetworkPolicy on and the CNI's interfaces tagged, EBS CSI
	// through Pod Identity.
	if v := aws.ToString(f.addonIn["vpc-cni"].ConfigurationValues); v != `{"enableNetworkPolicy":"true","env":{"ADDITIONAL_ENI_TAGS":"{\"agent-substrate-on-eks/deployment\":\"demo.us-west-2\",\"auto-delete\":\"no\",\"project\":\"agent-substrate\"}"}}` {
		t.Errorf("vpc-cni config %s", v)
	}
	if p := f.addonIn["aws-ebs-csi-driver"].PodIdentityAssociations; len(p) != 1 || aws.ToString(p[0].ServiceAccount) != ebsControllerSA ||
		!strings.HasSuffix(aws.ToString(p[0].RoleArn), ":role/demo-us-west-2-ebs-csi-driver") {
		t.Errorf("EBS CSI Pod Identity %+v", p)
	}
	for _, a := range f.addons {
		if a.Status != ekstypes.AddonStatusActive {
			t.Errorf("add-on %s is %s", aws.ToString(a.AddonName), a.Status)
		}
	}

	// Node groups: labels, taints, launch templates.
	wantTaint := map[string]string{"system": "ate.dev/workloadType=ate-system-infra", "gvisor": "ate.dev/sandboxClass=gvisor", "microvm": "ate.dev/sandboxClass=microvm"}
	for name, ng := range f.ngIn {
		if ng.Labels["ate.dev/pool"] != name {
			t.Errorf("%s: label ate.dev/pool=%q", name, ng.Labels["ate.dev/pool"])
		}
		if len(ng.Taints) != 1 || aws.ToString(ng.Taints[0].Key)+"="+aws.ToString(ng.Taints[0].Value) != wantTaint[name] || ng.Taints[0].Effect != ekstypes.TaintEffectNoSchedule {
			t.Errorf("%s: taints %+v", name, ng.Taints)
		}
		if !slices.Equal(ng.Subnets, in.ResourcesVpcConfig.SubnetIds) {
			t.Errorf("%s: subnets %v, want the private ones", name, ng.Subnets)
		}
		lt := f.lts[aws.ToString(ng.LaunchTemplate.Id)]
		d := lt.versions[len(lt.versions)-1]
		if d.MetadataOptions.HttpTokens != ec2types.LaunchTemplateHttpTokensStateRequired || aws.ToInt32(d.MetadataOptions.HttpPutResponseHopLimit) != 1 {
			t.Errorf("%s: IMDS %+v", name, d.MetadataOptions)
		}
		if ebs := d.BlockDeviceMappings[0].Ebs; !aws.ToBool(ebs.Encrypted) || ebs.VolumeType != ec2types.VolumeTypeGp3 {
			t.Errorf("%s: root volume %+v", name, ebs)
		}
		nested := d.CpuOptions != nil && d.CpuOptions.NestedVirtualization == ec2types.NestedVirtualizationSpecificationEnabled
		if nested != (name == "microvm") {
			t.Errorf("%s: nested virtualization %v", name, nested)
		}
	}
	if aws.ToInt32(f.ngIn["system"].ScalingConfig.MinSize) != 2 || aws.ToInt32(f.ngIn["gvisor"].ScalingConfig.MaxSize) != 4 {
		t.Errorf("scaling: system %+v gvisor %+v", f.ngIn["system"].ScalingConfig, f.ngIn["gvisor"].ScalingConfig)
	}
	if got := aws.ToString(f.lts[aws.ToString(f.ngIn["microvm"].LaunchTemplate.Id)].lt.LaunchTemplateName); got != "demo-microvm-nested-virt" {
		t.Errorf("microvm launch template %s", got)
	}
	if f.ngIn["system"].AmiType != ekstypes.AMITypesAl2023X8664Standard {
		t.Errorf("AMI type %s", f.ngIn["system"].AmiType)
	}
	if !f.dnsHosts[aws.ToString(f.cluster.ResourcesVpcConfig.VpcId)] {
		t.Error("DNS hostnames are off in the VPC")
	}
}

// AC2.1: every resource, including what AWS made for us, carries the owner
// tag, TAGS and the created-by tag.
func TestCreateClusterTagsEverything(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{OwnerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no", CreatedByTagKey: CreatedByTagValue}
	check := func(what string, have map[string]string) {
		t.Helper()
		for k, v := range want {
			if have[k] != v {
				t.Errorf("%s: tag %s=%q, want %q", what, k, have[k], v)
			}
		}
	}
	ec2 := map[string][]ec2types.Tag{}
	for id, v := range f.vpcs {
		if id != "vpc-other" {
			ec2[id] = v.Tags
		}
	}
	for id, v := range f.subnets {
		ec2[id] = v.Tags
	}
	for id, v := range f.igws {
		ec2[id] = v.Tags
	}
	for id, v := range f.eips {
		if id != "eipalloc-other" {
			ec2[id] = v.Tags
		}
	}
	for id, v := range f.nats {
		ec2[id] = v.Tags
	}
	for id, v := range f.rts {
		ec2[id] = v.Tags
	}
	for id, v := range f.sgs {
		ec2[id] = v.Tags
	}
	for id, v := range f.acls {
		ec2[id] = v.Tags
	}
	for id, v := range f.lts {
		ec2[id] = v.lt.Tags
		for _, s := range v.versions[0].TagSpecifications {
			check(id+" launches "+string(s.ResourceType), ec2TagMap(s.Tags))
		}
		if len(v.versions[0].TagSpecifications) != 3 {
			t.Errorf("%s tags %d resource types at launch, want instance, volume, network-interface", id, len(v.versions[0].TagSpecifications))
		}
	}
	if len(ec2) != 1+6+1+1+1+3+2+1+3 { // vpc, subnets, igw, eip, nat, route tables (main+2), SGs (default+cluster), acl, LTs
		t.Errorf("checked %d EC2 resources", len(ec2))
	}
	for id, tags := range ec2 {
		check(id, ec2TagMap(tags))
	}
	for name, r := range f.roles {
		if name != "Admin" {
			check("role "+name, r.tags)
		}
	}
	check("cluster", f.cluster.Tags)
	for p, tags := range f.entries {
		check("access entry "+p, tags)
	}
	for n, a := range f.addons {
		check("add-on "+n, a.Tags)
	}
	for n, ng := range f.ngs {
		check("node group "+n, ng.Tags)
	}
	if len(f.asgTags) != 3 {
		t.Errorf("tagged %d Auto Scaling groups, want 3", len(f.asgTags))
	}
	for n, tags := range f.asgTags {
		check("Auto Scaling group "+n, tags)
	}
}

// AC1.2: a second run makes no write at all, and logs zero creates.
func TestCreateClusterSecondRunCreatesNothing(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	r := clusterTestRun(t, f, false)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(r.creates) != 0 || len(r.changes) != 0 {
		t.Errorf("second run: writes %v, creates %v, changes %v", f.writes, r.creates, r.changes)
	}
}

// AC1.4: --plan writes nothing, and lists exactly what a real run creates.
func TestCreateClusterPlanWritesNothing(t *testing.T) {
	f := newClusterFake()
	p := clusterTestRun(t, f, true)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("--plan made %d writes: %v", len(f.writes), f.writes)
	}
	real := clusterTestRun(t, newClusterFake(), false)
	if err := real.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.creates, real.creates) {
		t.Errorf("plan lists\n %v\nthe run created\n %v", p.creates, real.creates)
	}
	// On a complete deployment, --plan has nothing to create.
	done := newClusterFake()
	if err := clusterTestRun(t, done, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done.writes = nil
	p = clusterTestRun(t, done, true)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(done.writes) != 0 || len(p.creates) != 0 {
		t.Errorf("plan on a complete deployment: writes %v, would create %v", done.writes, p.creates)
	}
}

// Review F17: --plan against a cluster that is UPDATING (another run's
// change) reports it and returns; it never waits up to 30 minutes for it.
func TestCreateClusterPlanDoesNotWaitOnABusyCluster(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cluster.Status = ekstypes.ClusterStatusUpdating
	f.writes = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clusterTestRun(t, f, true).run(ctx); err != nil {
		t.Fatalf("plan on an UPDATING cluster: %v", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("plan wrote %v", f.writes)
	}
}

// AC1.5: a run that stops part-way is completed by the next one, with each
// resource created once. failAfter is the hard case: the create happened but
// the run never saw the reply.
func TestCreateClusterInterruptedRunIsCompleted(t *testing.T) {
	for _, tc := range []struct{ before, after string }{
		{before: "CreateNodegroup gvisor"},
		{after: "CreateNodegroup gvisor"},
		{after: "CreateSubnet demo-private-2"},
		{after: "AllocateAddress"},
		{after: "CreateNatGateway"},
		{after: "CreateRole demo-us-west-2-eks-node"},
		{after: "CreateCluster"},
		{after: "CreateAddon coredns"},
		{after: "CreateLaunchTemplate demo-microvm-nested-virt"},
		{before: "CreateOrUpdateTags"},
	} {
		t.Run(tc.before+tc.after, func(t *testing.T) {
			f := newClusterFake()
			f.failBefore, f.failAfter = tc.before, tc.after
			if err := clusterTestRun(t, f, false).run(context.Background()); err == nil {
				t.Fatal("the interrupted run reported success")
			}
			if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
				t.Fatalf("rerun: %v", err)
			}
			if got := creates(f.writes); !maps.Equal(got, freshCreates) {
				t.Errorf("creates over both runs:\n got %v\nwant %v", got, freshCreates)
			}
			if len(f.asgTags) != 3 {
				t.Errorf("%d Auto Scaling groups tagged", len(f.asgTags))
			}
		})
	}
}

// AC2.2: a resource with a global name that exists without this
// deployment's owner tag is not reused or changed; the run stops and names it.
func TestCreateClusterRefusesForeignResources(t *testing.T) {
	t.Run("launch template", func(t *testing.T) {
		f := newClusterFake()
		f.lts["lt-foreign"] = &fakeLT{lt: ec2types.LaunchTemplate{LaunchTemplateId: aws.String("lt-foreign"),
			LaunchTemplateName: aws.String("demo-microvm-nested-virt"), LatestVersionNumber: aws.Int64(1),
			Tags: []ec2types.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String("other.us-west-2")}}},
			versions: []*ec2types.RequestLaunchTemplateData{{}}}
		err := clusterTestRun(t, f, false).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "demo-microvm-nested-virt") || !strings.Contains(err.Error(), "not owned by") {
			t.Fatalf("want a not-owned error naming the template, got %v", err)
		}
		if len(f.lts["lt-foreign"].versions) != 1 || creates(f.writes)["CreateNodegroup microvm"] != 0 {
			t.Errorf("used or changed a foreign template: %v", f.writes)
		}
	})
	t.Run("IAM role", func(t *testing.T) {
		f := newClusterFake()
		f.roles["demo-us-west-2-eks-cluster"] = &fakeRole{arn: "arn:aws:iam::111122223333:role/demo-us-west-2-eks-cluster", tags: map[string]string{}}
		err := clusterTestRun(t, f, false).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "demo-us-west-2-eks-cluster") || !strings.Contains(err.Error(), "not owned by") {
			t.Fatalf("want a not-owned error naming the role, got %v", err)
		}
		for _, w := range f.writes {
			if strings.Contains(w, "demo-us-west-2-eks-cluster") || strings.HasPrefix(w, "CreateCluster") {
				t.Errorf("changed or used a foreign role: %s", w)
			}
		}
	})
	t.Run("cluster", func(t *testing.T) {
		f := newClusterFake()
		f.cluster = &ekstypes.Cluster{Name: aws.String("demo"), Status: ekstypes.ClusterStatusActive, Tags: map[string]string{}}
		err := clusterTestRun(t, f, false).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not owned by") {
			t.Fatalf("want a not-owned error, got %v", err)
		}
		if len(f.writes) != 0 {
			t.Errorf("wrote to another deployment's cluster: %v", f.writes)
		}
	})
}

// AC2.3: a denied or throttled read stops the run; it is never taken for
// "not found" and followed by a create.
func TestCreateClusterDeniedReadStops(t *testing.T) {
	for op, code := range map[string]string{
		"DescribeCluster": "AccessDeniedException", "DescribeVpcs": "UnauthorizedOperation",
		"GetRole": "AccessDenied", "DescribeLaunchTemplates": "RequestLimitExceeded",
	} {
		t.Run(op, func(t *testing.T) {
			f := newClusterFake()
			f.deny[op] = code
			err := clusterTestRun(t, f, false).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Fatalf("want %s, got %v", code, err)
			}
			for w := range creates(f.writes) {
				switch {
				case op == "DescribeCluster", op == "DescribeVpcs":
					t.Errorf("created after a failed %s: %s", op, w)
				case op == "GetRole" && strings.HasPrefix(w, "CreateRole"):
					t.Errorf("created a role after a failed GetRole: %s", w)
				case op == "DescribeLaunchTemplates" && strings.HasPrefix(w, "CreateLaunchTemplate"):
					t.Errorf("created a template after a failed read: %s", w)
				}
			}
		})
	}
}

func TestCreateClusterPreflight(t *testing.T) {
	t.Run("no nested virtualization", func(t *testing.T) {
		f := newClusterFake()
		err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.MicrovmInstanceType = "m7i.2xlarge" }).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "nested virtualization") || len(f.writes) != 0 {
			t.Fatalf("want a nested virtualization error before any write, got %v (writes %v)", err, f.writes)
		}
	})
	t.Run("VPC quota full", func(t *testing.T) {
		f := newClusterFake()
		f.quotas[quotaVPCs] = 1 // vpc-other uses it
		err := clusterTestRun(t, f, false).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), quotaVPCs) || len(f.writes) != 0 {
			t.Fatalf("want a VPC quota error before any write, got %v (writes %v)", err, f.writes)
		}
	})
	t.Run("zones", func(t *testing.T) {
		f := newClusterFake()
		r := clusterTestRun(t, f, true)
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		// us-west-2d does not offer m8i.
		if !slices.Equal(r.azs, []string{"us-west-2a", "us-west-2b", "us-west-2c"}) {
			t.Errorf("zones %v", r.azs)
		}
	})
	t.Run("Auto Mode mismatch", func(t *testing.T) {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.writes = nil
		err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.AutoMode = "" }).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Auto Mode on") || len(f.writes) != 0 {
			t.Fatalf("want an Auto Mode mismatch error, got %v", err)
		}
	})
}

func TestCreateClusterStandardMode(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.AutoMode = "" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.clusterIn.ComputeConfig != nil || f.roles["demo-us-west-2-eks-auto-node"] != nil {
		t.Error("a standard cluster got Auto Mode settings")
	}
	if len(f.ngIn["system"].Taints) != 0 {
		t.Errorf("standard system group taints %+v", f.ngIn["system"].Taints)
	}
	if got := f.roles["demo-us-west-2-eks-cluster"].attached; !slices.Equal(got, []string{"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"}) {
		t.Errorf("standard cluster role policies %v", got)
	}
}

// An owned template whose latest version lost a setting (hop limit 2 here)
// gets a new version, and the node group uses it.
func TestCreateClusterLaunchTemplateDriftAddsVersion(t *testing.T) {
	f := newClusterFake()
	r := clusterTestRun(t, f, false)
	g := r.groups()[2]
	d := r.launchTemplateData(g)
	d.MetadataOptions.HttpPutResponseHopLimit = aws.Int32(2)
	f.lts["lt-mine"] = &fakeLT{lt: ec2types.LaunchTemplate{LaunchTemplateId: aws.String("lt-mine"),
		LaunchTemplateName: aws.String("demo-microvm-nested-virt"), LatestVersionNumber: aws.Int64(1),
		Tags: []ec2types.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String("demo.us-west-2")}}},
		versions: []*ec2types.RequestLaunchTemplateData{d}}
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.lts["lt-mine"].versions); n != 2 {
		t.Fatalf("template has %d versions, want 2", n)
	}
	if v := aws.ToString(f.ngIn["microvm"].LaunchTemplate.Version); v != "2" {
		t.Errorf("microvm node group uses version %s, want 2", v)
	}
}

// An existing vpc-cni add-on without enableNetworkPolicy gets it, and keeps
// its other settings.
func TestCreateClusterTurnsOnNetworkPolicy(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.addons["vpc-cni"].ConfigurationValues = aws.String(`{"env":{"WARM_IP_TARGET":"5"}}`)
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.writes, []string{"UpdateAddon vpc-cni"}) {
		t.Errorf("writes %v", f.writes)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(aws.ToString(f.addons["vpc-cni"].ConfigurationValues)), &got); err != nil || got["enableNetworkPolicy"] != "true" || got["env"] == nil {
		t.Errorf("vpc-cni config %v (%v)", got, err)
	}
}

// A cluster eksctl made, missing its microvm node group:
// the run uses the cluster's private subnets, creates only the missing
// group with what it needs, and puts the created-by tag only on that.
func TestCreateClusterCompletesAnEksctlCluster(t *testing.T) {
	f := newClusterFake()
	owner := map[string]string{OwnerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no"}
	tags := func(extra map[string]string) []ec2types.Tag {
		m := maps.Clone(owner)
		maps.Copy(m, extra)
		var out []ec2types.Tag
		for _, k := range sortedKeys(m) {
			out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
		}
		return out
	}
	f.vpcs["vpc-eksctl"] = &ec2types.Vpc{VpcId: aws.String("vpc-eksctl"), Tags: tags(nil)}
	f.subnets["subnet-pub"] = &ec2types.Subnet{SubnetId: aws.String("subnet-pub"), VpcId: aws.String("vpc-eksctl"), Tags: tags(map[string]string{"kubernetes.io/role/elb": "1"})}
	f.subnets["subnet-priv"] = &ec2types.Subnet{SubnetId: aws.String("subnet-priv"), VpcId: aws.String("vpc-eksctl"), Tags: tags(map[string]string{"kubernetes.io/role/internal-elb": "1"})}
	f.sgs["sg-eksctl"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-eksctl"), VpcId: aws.String("vpc-eksctl")}
	f.cluster = &ekstypes.Cluster{Name: aws.String("demo"), Status: ekstypes.ClusterStatusActive, Version: aws.String("1.37"), Tags: owner,
		ComputeConfig: &ekstypes.ComputeConfigResponse{Enabled: aws.Bool(true)},
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{VpcId: aws.String("vpc-eksctl"), SubnetIds: []string{"subnet-pub", "subnet-priv"},
			ClusterSecurityGroupId: aws.String("sg-eksctl"), PublicAccessCidrs: []string{"203.0.113.0/24"}}}
	for _, a := range clusterAddons {
		f.addons[a.Name] = &ekstypes.Addon{AddonName: aws.String(a.Name), Status: ekstypes.AddonStatusActive, ConfigurationValues: aws.String(a.Config), Tags: owner}
	}
	for _, ng := range []string{"system", "gvisor"} {
		f.ngs[ng] = &ekstypes.Nodegroup{NodegroupName: aws.String(ng), Status: ekstypes.NodegroupStatusCreating, Tags: owner}
	}
	// eksctl's creator bootstrap gave the caller cluster admin.
	admin := "arn:aws:iam::111122223333:role/Admin"
	f.entries[admin] = map[string]string{}
	f.access[admin] = []ekstypes.AssociatedAccessPolicy{{PolicyArn: aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"),
		AccessScope: &ekstypes.AccessScope{Type: ekstypes.AccessScopeTypeCluster}}}

	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"CreateRole demo-us-west-2-eks-node": 1, "CreateLaunchTemplate demo-microvm-nested-virt": 1, "CreateNodegroup microvm": 1}
	if got := creates(f.writes); !maps.Equal(got, want) {
		t.Errorf("creates %v, want %v", got, want)
	}
	if s := f.ngIn["microvm"].Subnets; !slices.Equal(s, []string{"subnet-priv"}) {
		t.Errorf("microvm subnets %v, want the cluster's private subnet", s)
	}
	for ng, tags := range f.asgTags {
		ours := ng == "eks-microvm-asg"
		if got := tags[CreatedByTagKey] == CreatedByTagValue; got != ours || tags[OwnerTagKey] != "demo.us-west-2" {
			t.Errorf("%s: tags %v", ng, tags)
		}
	}
	if len(f.asgTags) != 3 {
		t.Errorf("tagged %d Auto Scaling groups", len(f.asgTags))
	}
	if tagOf(f.sgs["sg-eksctl"].Tags, CreatedByTagKey) != nil {
		t.Error("tagged eksctl's cluster security group as created by setup-aws")
	}
}
