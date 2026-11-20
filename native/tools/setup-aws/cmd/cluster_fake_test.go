package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

// clusterFake is one in-memory account for `create cluster`: EC2, EKS, IAM,
// Auto Scaling and Service Quotas. It records every write, refuses
// duplicates the way AWS does (a second subnet with the same CIDR, a second
// NAT gateway on one Elastic IP, a second template with one name), and can
// fail a write before or after applying it, to stand in for an interrupted run.
type clusterFake struct {
	n      int
	writes []string
	// failBefore fails the first write that starts with it, unapplied;
	// failAfter applies it, then fails (the run died before the reply).
	failBefore, failAfter string
	deny                  map[string]string // read operation -> error code
	reads                 []string          // read operations, in order
	// inputs are the requests of the reads whose payload the node group
	// settings decide (readIn), "<op> <request as JSON>", in order.
	inputs []string

	vpcs      map[string]*ec2types.Vpc
	dnsHosts  map[string]bool
	subnets   map[string]*ec2types.Subnet
	igws      map[string]*ec2types.InternetGateway
	eips      map[string]*ec2types.Address
	nats      map[string]*ec2types.NatGateway
	rts       map[string]*ec2types.RouteTable
	sgs       map[string]*ec2types.SecurityGroup
	acls      map[string]*ec2types.NetworkAcl
	enis      map[string]*ec2types.NetworkInterface
	lts       map[string]*fakeLT // by id
	types     map[string]ec2types.InstanceTypeInfo
	images    map[string]ec2types.Image // AMIs this account may launch, by ID
	zones     []ec2types.AvailabilityZone
	offerings map[string][]string // zone -> instance types
	quotas    map[string]float64  // applied quota values; others are the default 5

	cluster   *ekstypes.Cluster
	clusterIn *eks.CreateClusterInput
	entries   map[string]map[string]string // principal -> tags
	access    map[string][]ekstypes.AssociatedAccessPolicy
	addons    map[string]*ekstypes.Addon
	addonIn   map[string]*eks.CreateAddonInput
	ngs       map[string]*ekstypes.Nodegroup
	ngIn      map[string]*eks.CreateNodegroupInput

	roles   map[string]*fakeRole
	asgTags map[string]map[string]string

	// Self-managed node groups: instance profiles, the access entries'
	// types, Auto Scaling groups (asgStuck keeps them from launching), their
	// failed scaling activities, SSM parameters, and the nodes the
	// Kubernetes API lists (notReady keeps them NotReady).
	profiles   map[string]*fakeProfile
	entryTypes map[string]string
	asgs       map[string]*asgtypes.AutoScalingGroup
	asgIn      map[string]*autoscaling.CreateAutoScalingGroupInput
	asgStuck   bool
	activities map[string][]asgtypes.Activity
	params     map[string]string
	notReady   bool
	kubeCalls  int
	// kubeErrs are the answers of the next node lists, in order; kubeDown
	// answers every one after them. roleLag is how many times
	// AddRoleToInstanceProfile does not find a role that exists yet (IAM's
	// lag after CreateRole).
	kubeErrs []error
	kubeDown error
	roleLag  int
	// asgLag is how many reads of DescribeAutoScalingGroups miss a group
	// after CreateAutoScalingGroup made it (Auto Scaling's lag); asgHidden
	// counts them down by group.
	asgLag    int
	asgHidden map[string]int
	// nitroAlloc is the allocatable eks.amazonaws.com/nitro-sandbox of each
	// node of the nitro group ("" for none advertised), and nitroNotReady
	// keeps those nodes NotReady: a node of a Nitro Sandbox AMI, or of
	// another AMI, as the Kubernetes API lists it.
	nitroAlloc    string
	nitroNotReady bool
	// nitroMissing nodes of the nitro group never join.
	nitroMissing int

	// KARPENTER=on (karpenter_fake_test.go): the SQS queues, EventBridge
	// rules and Pod Identity associations, and the Kubernetes API of the
	// cluster's objects (nil: none answers).
	queues  map[string]*fakeQueue
	rules   map[string]*fakeRule
	assocs  map[string]*ekstypes.PodIdentityAssociation
	objects *kubeFake
	// eksNodeEntry: CreateNodegroup gives the node role its EC2_LINUX
	// access entry, as EKS does (the tests before KARPENTER do without).
	eksNodeEntry bool
	// updateEnds: an UPDATING cluster is ACTIVE from the next
	// DescribeCluster on. Without it the cluster stays UPDATING, as under
	// another run's change that does not end (review F17).
	updateEnds bool
}

type fakeProfile struct {
	tags  map[string]string
	roles []string
	// tagsInGet: GetInstanceProfile returns the tags too; otherwise only
	// ListInstanceProfileTags does.
	tagsInGet bool
}

// The recommended EKS optimized AL2023 AMIs the fake's SSM names for 1.37.
const (
	eksAMIx86 = "ami-0e1e0000000000086"
	eksAMIarm = "ami-0e1e000000000000a"
)

type fakeLT struct {
	lt       ec2types.LaunchTemplate
	versions []*ec2types.RequestLaunchTemplateData
}

type fakeRole struct {
	arn, trust string
	tags       map[string]string
	attached   []string
	inline     map[string]string
}

func nestedVirtType(name string, nested bool) ec2types.InstanceTypeInfo {
	p := &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureTypeX8664}}
	if nested {
		p.SupportedFeatures = []ec2types.SupportedAdditionalProcessorFeature{ec2types.SupportedAdditionalProcessorFeatureNestedVirtualization}
	}
	return ec2types.InstanceTypeInfo{InstanceType: ec2types.InstanceType(name), ProcessorInfo: p, VCpuInfo: vcpus(name),
		NetworkInfo: &ec2types.NetworkInfo{EnaSupport: ec2types.EnaSupportRequired}, NitroEnclavesSupport: ec2types.NitroEnclavesSupportSupported}
}

// vcpus are the vCPUs of an m8i, m7i or m8g size.
func vcpus(name string) *ec2types.VCpuInfo {
	n := int32(4)
	if strings.HasSuffix(name, ".2xlarge") {
		n = 8
	}
	return &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(n)}
}

func newClusterFake() *clusterFake {
	f := &clusterFake{deny: map[string]string{},
		vpcs: map[string]*ec2types.Vpc{}, dnsHosts: map[string]bool{}, subnets: map[string]*ec2types.Subnet{},
		igws: map[string]*ec2types.InternetGateway{}, eips: map[string]*ec2types.Address{}, nats: map[string]*ec2types.NatGateway{},
		rts: map[string]*ec2types.RouteTable{}, sgs: map[string]*ec2types.SecurityGroup{}, acls: map[string]*ec2types.NetworkAcl{},
		enis: map[string]*ec2types.NetworkInterface{}, lts: map[string]*fakeLT{}, quotas: map[string]float64{},
		images: map[string]ec2types.Image{},
		types: map[string]ec2types.InstanceTypeInfo{
			"m8i.xlarge": nestedVirtType("m8i.xlarge", true), "m8i.2xlarge": nestedVirtType("m8i.2xlarge", true),
			"m7i.2xlarge": nestedVirtType("m7i.2xlarge", false), "m8g.2xlarge": armType("m8g.2xlarge"),
			// No Nitro Enclaves (and not offered in any zone).
			"t3.xlarge": {InstanceType: "t3.xlarge", NitroEnclavesSupport: ec2types.NitroEnclavesSupportUnsupported,
				ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureTypeX8664}},
				NetworkInfo:   &ec2types.NetworkInfo{EnaSupport: ec2types.EnaSupportRequired}}},
		offerings: map[string][]string{
			"us-west-2a": {"m8i.xlarge", "m8i.2xlarge", "m7i.2xlarge", "m8g.2xlarge"}, "us-west-2b": {"m8i.xlarge", "m8i.2xlarge", "m7i.2xlarge", "m8g.2xlarge"},
			"us-west-2c": {"m8i.xlarge", "m8i.2xlarge", "m7i.2xlarge", "m8g.2xlarge"}, "us-west-2d": {"m7i.2xlarge"}},
		entries: map[string]map[string]string{}, access: map[string][]ekstypes.AssociatedAccessPolicy{},
		addons: map[string]*ekstypes.Addon{}, addonIn: map[string]*eks.CreateAddonInput{},
		ngs: map[string]*ekstypes.Nodegroup{}, ngIn: map[string]*eks.CreateNodegroupInput{},
		roles: map[string]*fakeRole{
			// The caller's own role (fsts: assumed-role/Admin/me).
			"Admin": {arn: "arn:aws:iam::111122223333:role/Admin", tags: map[string]string{}}},
		asgTags:  map[string]map[string]string{},
		profiles: map[string]*fakeProfile{}, entryTypes: map[string]string{},
		asgs: map[string]*asgtypes.AutoScalingGroup{}, asgIn: map[string]*autoscaling.CreateAutoScalingGroupInput{},
		asgHidden: map[string]int{}, activities: map[string][]asgtypes.Activity{},
		params: map[string]string{
			"/aws/service/eks/optimized-ami/1.37/amazon-linux-2023/x86_64/standard/recommended/image_id": eksAMIx86,
			"/aws/service/eks/optimized-ami/1.37/amazon-linux-2023/arm64/standard/recommended/image_id":  eksAMIarm,
		},
		nitroAlloc: "2",
		queues:     map[string]*fakeQueue{}, rules: map[string]*fakeRule{}, assocs: map[string]*ekstypes.PodIdentityAssociation{},
	}
	for id, arch := range map[string]ec2types.ArchitectureValues{eksAMIx86: ec2types.ArchitectureValuesX8664, eksAMIarm: ec2types.ArchitectureValuesArm64} {
		im := testImage(arch, "/dev/xvda", 20)
		im.ImageId, im.Name = aws.String(id), aws.String("amazon-eks-node-al2023-"+string(arch)+"-standard-1.37-v20261101")
		f.images[id] = im
	}
	for i, z := range []string{"a", "b", "c", "d"} {
		f.zones = append(f.zones, ec2types.AvailabilityZone{ZoneName: aws.String("us-west-2" + z), ZoneId: aws.String(fmt.Sprintf("usw2-az%d", i+1))})
	}
	// Somebody else's VPC and address, for the quota counts.
	f.vpcs["vpc-other"] = &ec2types.Vpc{VpcId: aws.String("vpc-other")}
	f.eips["eipalloc-other"] = &ec2types.Address{AllocationId: aws.String("eipalloc-other"), Domain: ec2types.DomainTypeVpc}
	return f
}

func armType(name string) ec2types.InstanceTypeInfo {
	return ec2types.InstanceTypeInfo{InstanceType: ec2types.InstanceType(name),
		ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureTypeArm64}},
		VCpuInfo:      vcpus(name),
		NetworkInfo:   &ec2types.NetworkInfo{EnaSupport: ec2types.EnaSupportRequired}, NitroEnclavesSupport: ec2types.NitroEnclavesSupportSupported}
}

func (f *clusterFake) clients() *ClusterClients {
	return &ClusterClients{EC2: (*fcEC2)(f), EKS: (*fcEKS)(f), IAM: (*fcIAM)(f), ASG: (*fcASG)(f), Quotas: (*fcQuotas)(f),
		SSM: (*fcSSM)(f), Kube: f.kube, SQS: (*fcSQS)(f), Events: (*fcEvents)(f), Objects: f.kubeObjects}
}

func (f *clusterFake) id(prefix string) string { f.n++; return fmt.Sprintf("%s-%04d", prefix, f.n) }

func (f *clusterFake) begin(w string) error {
	if f.failBefore != "" && strings.HasPrefix(w, f.failBefore) {
		f.failBefore = ""
		return apiErr("InternalFailure")
	}
	f.writes = append(f.writes, w)
	return nil
}

func (f *clusterFake) end(w string) error {
	if f.failAfter != "" && strings.HasPrefix(w, f.failAfter) {
		f.failAfter = ""
		return apiErr("RequestTimeout")
	}
	return nil
}

func (f *clusterFake) read(op string) error {
	f.reads = append(f.reads, op)
	if code, ok := f.deny[op]; ok {
		return apiErr(code)
	}
	return nil
}

// readIn is read that records the request too (inputs).
func (f *clusterFake) readIn(op string, in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	f.inputs = append(f.inputs, op+" "+string(b))
	return f.read(op)
}

// --- EC2 ----------------------------------------------------------------------

type fcEC2 clusterFake

func (e *fcEC2) f() *clusterFake { return (*clusterFake)(e) }

func tagOf(tags []ec2types.Tag, k string) []string {
	for _, t := range tags {
		if aws.ToString(t.Key) == k {
			return []string{aws.ToString(t.Value)}
		}
	}
	return nil
}

// match applies EC2 filters; get returns a resource's values for a filter
// name, and panics on one the fake does not know (a test bug, not a miss).
func match(filters []ec2types.Filter, tags []ec2types.Tag, get map[string][]string) bool {
	for _, fl := range filters {
		name := aws.ToString(fl.Name)
		var have []string
		if k, ok := strings.CutPrefix(name, "tag:"); ok {
			have = tagOf(tags, k)
		} else if v, ok := get[name]; ok {
			have = v
		} else {
			panic("fake: unknown filter " + name)
		}
		hit := false
		for _, v := range fl.Values {
			hit = hit || slices.Contains(have, v)
		}
		if !hit {
			return false
		}
	}
	return true
}

func specTags(specs []ec2types.TagSpecification) []ec2types.Tag {
	var out []ec2types.Tag
	for _, s := range specs {
		out = append(out, s.Tags...)
	}
	return out
}

func nameOf(specs []ec2types.TagSpecification) string {
	if v := tagOf(specTags(specs), "Name"); v != nil {
		return v[0]
	}
	return ""
}

func (e *fcEC2) DescribeAvailabilityZones(_ context.Context, in *ec2.DescribeAvailabilityZonesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
	if err := e.f().read("DescribeAvailabilityZones"); err != nil {
		return nil, err
	}
	for _, fl := range in.Filters {
		if n := aws.ToString(fl.Name); n != "zone-type" && n != "state" {
			panic("fake: unknown filter " + n)
		}
	}
	return &ec2.DescribeAvailabilityZonesOutput{AvailabilityZones: e.zones}, nil
}

func (e *fcEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	if err := e.f().readIn("DescribeInstanceTypes", in); err != nil {
		return nil, err
	}
	out := &ec2.DescribeInstanceTypesOutput{}
	for _, t := range in.InstanceTypes {
		it, ok := e.types[string(t)]
		if !ok {
			return nil, apiErr("InvalidInstanceType")
		}
		out.InstanceTypes = append(out.InstanceTypes, it)
	}
	return out, nil
}

func (e *fcEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	if err := e.f().readIn("DescribeImages", in); err != nil {
		return nil, err
	}
	if len(in.Filters) > 0 || len(in.Owners) > 0 {
		panic("fake: DescribeImages takes image IDs only")
	}
	out := &ec2.DescribeImagesOutput{}
	for _, id := range in.ImageIds {
		im, ok := e.images[id]
		if !ok {
			return nil, apiErr("InvalidAMIID.NotFound")
		}
		out.Images = append(out.Images, im)
	}
	return out, nil
}

func (e *fcEC2) DescribeInstanceTypeOfferings(_ context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	if err := e.f().read("DescribeInstanceTypeOfferings"); err != nil {
		return nil, err
	}
	out := &ec2.DescribeInstanceTypeOfferingsOutput{}
	for _, z := range sortedKeysOf(e.offerings) {
		for _, t := range e.offerings[z] {
			if match(in.Filters, nil, map[string][]string{"instance-type": {t}}) {
				out.InstanceTypeOfferings = append(out.InstanceTypeOfferings, ec2types.InstanceTypeOffering{Location: aws.String(z), InstanceType: ec2types.InstanceType(t)})
			}
		}
	}
	return out, nil
}

func sortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (e *fcEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if err := e.f().read("DescribeVpcs"); err != nil {
		return nil, err
	}
	out := &ec2.DescribeVpcsOutput{}
	for _, id := range sortedKeysOf(e.vpcs) {
		v := e.vpcs[id]
		if match(in.Filters, v.Tags, map[string][]string{"vpc-id": {id}}) {
			out.Vpcs = append(out.Vpcs, *v)
		}
	}
	return out, nil
}

func (e *fcEC2) CreateVpc(_ context.Context, in *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	f := e.f()
	w := "CreateVpc " + nameOf(in.TagSpecifications)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("vpc")
	f.vpcs[id] = &ec2types.Vpc{VpcId: aws.String(id), CidrBlock: in.CidrBlock, Tags: specTags(in.TagSpecifications)}
	// EC2 makes these with every VPC, untagged.
	rt := f.id("rtb")
	f.rts[rt] = &ec2types.RouteTable{RouteTableId: aws.String(rt), VpcId: aws.String(id),
		Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(true), RouteTableId: aws.String(rt)}}}
	sg := f.id("sg")
	f.sgs[sg] = &ec2types.SecurityGroup{GroupId: aws.String(sg), GroupName: aws.String("default"), VpcId: aws.String(id)}
	acl := f.id("acl")
	f.acls[acl] = &ec2types.NetworkAcl{NetworkAclId: aws.String(acl), VpcId: aws.String(id), IsDefault: aws.Bool(true)}
	if err := f.end(w); err != nil {
		return nil, err
	}
	return &ec2.CreateVpcOutput{Vpc: f.vpcs[id]}, nil
}

func (e *fcEC2) DescribeVpcAttribute(_ context.Context, in *ec2.DescribeVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcAttributeOutput, error) {
	out := &ec2.DescribeVpcAttributeOutput{VpcId: in.VpcId}
	switch in.Attribute {
	case ec2types.VpcAttributeNameEnableDnsSupport:
		out.EnableDnsSupport = &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}
	case ec2types.VpcAttributeNameEnableDnsHostnames:
		out.EnableDnsHostnames = &ec2types.AttributeBooleanValue{Value: aws.Bool(e.dnsHosts[aws.ToString(in.VpcId)])}
	}
	return out, nil
}

func (e *fcEC2) ModifyVpcAttribute(_ context.Context, in *ec2.ModifyVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error) {
	f := e.f()
	w := "ModifyVpcAttribute " + aws.ToString(in.VpcId)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	if in.EnableDnsHostnames != nil {
		f.dnsHosts[aws.ToString(in.VpcId)] = aws.ToBool(in.EnableDnsHostnames.Value)
	}
	return &ec2.ModifyVpcAttributeOutput{}, f.end(w)
}

func (e *fcEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	out := &ec2.DescribeSubnetsOutput{}
	for _, id := range sortedKeysOf(e.subnets) {
		s := e.subnets[id]
		if len(in.SubnetIds) > 0 && !slices.Contains(in.SubnetIds, id) {
			continue
		}
		if match(in.Filters, s.Tags, map[string][]string{"vpc-id": {aws.ToString(s.VpcId)}}) {
			out.Subnets = append(out.Subnets, *s)
		}
	}
	return out, nil
}

func (e *fcEC2) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, _ ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error) {
	f := e.f()
	w := "CreateSubnet " + nameOf(in.TagSpecifications)
	for _, s := range f.subnets {
		if aws.ToString(s.VpcId) == aws.ToString(in.VpcId) && aws.ToString(s.CidrBlock) == aws.ToString(in.CidrBlock) {
			return nil, apiErr("InvalidSubnet.Conflict")
		}
	}
	if _, ok := f.vpcs[aws.ToString(in.VpcId)]; !ok {
		return nil, apiErr("InvalidVpcID.NotFound")
	}
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("subnet")
	f.subnets[id] = &ec2types.Subnet{SubnetId: aws.String(id), VpcId: in.VpcId, CidrBlock: in.CidrBlock,
		AvailabilityZone: in.AvailabilityZone, Tags: specTags(in.TagSpecifications)}
	if err := f.end(w); err != nil {
		return nil, err
	}
	return &ec2.CreateSubnetOutput{Subnet: f.subnets[id]}, nil
}

func (e *fcEC2) DescribeInternetGateways(_ context.Context, in *ec2.DescribeInternetGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	out := &ec2.DescribeInternetGatewaysOutput{}
	for _, id := range sortedKeysOf(e.igws) {
		g := e.igws[id]
		var vpcs []string
		for _, a := range g.Attachments {
			vpcs = append(vpcs, aws.ToString(a.VpcId))
		}
		if match(in.Filters, g.Tags, map[string][]string{"attachment.vpc-id": vpcs}) {
			out.InternetGateways = append(out.InternetGateways, *g)
		}
	}
	return out, nil
}

func (e *fcEC2) CreateInternetGateway(_ context.Context, in *ec2.CreateInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error) {
	f := e.f()
	w := "CreateInternetGateway " + nameOf(in.TagSpecifications)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("igw")
	f.igws[id] = &ec2types.InternetGateway{InternetGatewayId: aws.String(id), Tags: specTags(in.TagSpecifications)}
	if err := f.end(w); err != nil {
		return nil, err
	}
	return &ec2.CreateInternetGatewayOutput{InternetGateway: f.igws[id]}, nil
}

func (e *fcEC2) AttachInternetGateway(_ context.Context, in *ec2.AttachInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error) {
	f := e.f()
	g := f.igws[aws.ToString(in.InternetGatewayId)]
	if g == nil || len(g.Attachments) > 0 {
		return nil, apiErr("Resource.AlreadyAssociated")
	}
	w := "AttachInternetGateway " + aws.ToString(in.InternetGatewayId)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	g.Attachments = []ec2types.InternetGatewayAttachment{{VpcId: in.VpcId, State: ec2types.AttachmentStatusAttached}}
	return &ec2.AttachInternetGatewayOutput{}, f.end(w)
}

func (e *fcEC2) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	out := &ec2.DescribeAddressesOutput{}
	for _, id := range sortedKeysOf(e.eips) {
		a := e.eips[id]
		if match(in.Filters, a.Tags, map[string][]string{"domain": {string(a.Domain)}}) {
			out.Addresses = append(out.Addresses, *a)
		}
	}
	return out, nil
}

func (e *fcEC2) AllocateAddress(_ context.Context, in *ec2.AllocateAddressInput, _ ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	f := e.f()
	w := "AllocateAddress " + nameOf(in.TagSpecifications)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("eipalloc")
	f.eips[id] = &ec2types.Address{AllocationId: aws.String(id), Domain: in.Domain, Tags: specTags(in.TagSpecifications)}
	if err := f.end(w); err != nil {
		return nil, err
	}
	return &ec2.AllocateAddressOutput{AllocationId: aws.String(id)}, nil
}

func (e *fcEC2) DescribeNatGateways(_ context.Context, in *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	out := &ec2.DescribeNatGatewaysOutput{}
	for _, id := range sortedKeysOf(e.nats) {
		g := e.nats[id]
		if len(in.NatGatewayIds) > 0 && !slices.Contains(in.NatGatewayIds, id) {
			continue
		}
		if match(in.Filter, g.Tags, map[string][]string{"vpc-id": {aws.ToString(g.VpcId)}, "state": {string(g.State)}}) {
			out.NatGateways = append(out.NatGateways, *g)
			// A NAT gateway comes up between two reads.
			if g.State == ec2types.NatGatewayStatePending {
				g.State = ec2types.NatGatewayStateAvailable
			}
		}
	}
	return out, nil
}

func (e *fcEC2) CreateNatGateway(_ context.Context, in *ec2.CreateNatGatewayInput, _ ...func(*ec2.Options)) (*ec2.CreateNatGatewayOutput, error) {
	f := e.f()
	w := "CreateNatGateway " + nameOf(in.TagSpecifications)
	if _, ok := f.eips[aws.ToString(in.AllocationId)]; !ok {
		return nil, apiErr("InvalidAllocationID.NotFound")
	}
	for _, g := range f.nats {
		if g.State != ec2types.NatGatewayStateDeleted && aws.ToString(g.NatGatewayAddresses[0].AllocationId) == aws.ToString(in.AllocationId) {
			return nil, apiErr("Resource.AlreadyAssociated")
		}
	}
	s := f.subnets[aws.ToString(in.SubnetId)]
	if s == nil {
		return nil, apiErr("InvalidSubnetID.NotFound")
	}
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("nat")
	f.nats[id] = &ec2types.NatGateway{NatGatewayId: aws.String(id), VpcId: s.VpcId, SubnetId: in.SubnetId, State: ec2types.NatGatewayStatePending,
		NatGatewayAddresses: []ec2types.NatGatewayAddress{{AllocationId: in.AllocationId}}, Tags: specTags(in.TagSpecifications)}
	if err := f.end(w); err != nil {
		return nil, err
	}
	c := *f.nats[id]
	return &ec2.CreateNatGatewayOutput{NatGateway: &c}, nil
}

func (e *fcEC2) DescribeRouteTables(_ context.Context, in *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	out := &ec2.DescribeRouteTablesOutput{}
	for _, id := range sortedKeysOf(e.rts) {
		t := e.rts[id]
		main := "false"
		for _, a := range t.Associations {
			if aws.ToBool(a.Main) {
				main = "true"
			}
		}
		if match(in.Filters, t.Tags, map[string][]string{"vpc-id": {aws.ToString(t.VpcId)}, "association.main": {main}}) {
			out.RouteTables = append(out.RouteTables, *t)
		}
	}
	return out, nil
}

func (e *fcEC2) CreateRouteTable(_ context.Context, in *ec2.CreateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error) {
	f := e.f()
	w := "CreateRouteTable " + nameOf(in.TagSpecifications)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("rtb")
	f.rts[id] = &ec2types.RouteTable{RouteTableId: aws.String(id), VpcId: in.VpcId, Tags: specTags(in.TagSpecifications),
		Routes: []ec2types.Route{{DestinationCidrBlock: aws.String(vpcCIDR), GatewayId: aws.String("local"), State: ec2types.RouteStateActive}}}
	if err := f.end(w); err != nil {
		return nil, err
	}
	return &ec2.CreateRouteTableOutput{RouteTable: f.rts[id]}, nil
}

func (e *fcEC2) CreateRoute(_ context.Context, in *ec2.CreateRouteInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	f := e.f()
	t := f.rts[aws.ToString(in.RouteTableId)]
	if t == nil {
		return nil, apiErr("InvalidRouteTableID.NotFound")
	}
	for _, r := range t.Routes {
		if aws.ToString(r.DestinationCidrBlock) == aws.ToString(in.DestinationCidrBlock) {
			return nil, apiErr("RouteAlreadyExists")
		}
	}
	w := "CreateRoute " + aws.ToString(in.RouteTableId)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	t.Routes = append(t.Routes, ec2types.Route{DestinationCidrBlock: in.DestinationCidrBlock, GatewayId: in.GatewayId,
		NatGatewayId: in.NatGatewayId, State: ec2types.RouteStateActive})
	return &ec2.CreateRouteOutput{Return: aws.Bool(true)}, f.end(w)
}

func (e *fcEC2) ReplaceRoute(_ context.Context, in *ec2.ReplaceRouteInput, _ ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error) {
	f := e.f()
	w := "ReplaceRoute " + aws.ToString(in.RouteTableId)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	t := f.rts[aws.ToString(in.RouteTableId)]
	for i := range t.Routes {
		if aws.ToString(t.Routes[i].DestinationCidrBlock) == aws.ToString(in.DestinationCidrBlock) {
			t.Routes[i] = ec2types.Route{DestinationCidrBlock: in.DestinationCidrBlock, GatewayId: in.GatewayId,
				NatGatewayId: in.NatGatewayId, State: ec2types.RouteStateActive}
		}
	}
	return &ec2.ReplaceRouteOutput{}, f.end(w)
}

func (e *fcEC2) AssociateRouteTable(_ context.Context, in *ec2.AssociateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error) {
	f := e.f()
	for _, t := range f.rts {
		for _, a := range t.Associations {
			if aws.ToString(a.SubnetId) == aws.ToString(in.SubnetId) {
				return nil, apiErr("Resource.AlreadyAssociated")
			}
		}
	}
	w := "AssociateRouteTable " + aws.ToString(in.SubnetId)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	t := f.rts[aws.ToString(in.RouteTableId)]
	t.Associations = append(t.Associations, ec2types.RouteTableAssociation{RouteTableId: in.RouteTableId, SubnetId: in.SubnetId})
	return &ec2.AssociateRouteTableOutput{}, f.end(w)
}

func (e *fcEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, id := range sortedKeysOf(e.sgs) {
		g := e.sgs[id]
		if len(in.GroupIds) > 0 && !slices.Contains(in.GroupIds, id) {
			continue
		}
		if match(in.Filters, g.Tags, map[string][]string{"vpc-id": {aws.ToString(g.VpcId)}, "group-name": {aws.ToString(g.GroupName)}}) {
			out.SecurityGroups = append(out.SecurityGroups, *g)
		}
	}
	return out, nil
}

func (e *fcEC2) DescribeNetworkAcls(_ context.Context, in *ec2.DescribeNetworkAclsInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkAclsOutput, error) {
	out := &ec2.DescribeNetworkAclsOutput{}
	for _, id := range sortedKeysOf(e.acls) {
		a := e.acls[id]
		if match(in.Filters, a.Tags, map[string][]string{"vpc-id": {aws.ToString(a.VpcId)}, "default": {strconv.FormatBool(aws.ToBool(a.IsDefault))}}) {
			out.NetworkAcls = append(out.NetworkAcls, *a)
		}
	}
	return out, nil
}

// DescribeNetworkInterfaces lists the network interfaces tests add (the ones
// AWS makes for a cluster, a NAT gateway or a node), by VPC.
func (e *fcEC2) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	if err := e.f().read("DescribeNetworkInterfaces"); err != nil {
		return nil, err
	}
	out := &ec2.DescribeNetworkInterfacesOutput{}
	for _, id := range sortedKeysOf(e.enis) {
		n := e.enis[id]
		if match(in.Filters, n.TagSet, map[string][]string{"vpc-id": {aws.ToString(n.VpcId)}}) {
			out.NetworkInterfaces = append(out.NetworkInterfaces, *n)
		}
	}
	return out, nil
}

// tagsOf points at the tags of any taggable resource of the fake.
func (f *clusterFake) tagsOf(id string) *[]ec2types.Tag {
	switch {
	case f.enis[id] != nil:
		return &f.enis[id].TagSet
	case f.vpcs[id] != nil:
		return &f.vpcs[id].Tags
	case f.rts[id] != nil:
		return &f.rts[id].Tags
	case f.sgs[id] != nil:
		return &f.sgs[id].Tags
	case f.acls[id] != nil:
		return &f.acls[id].Tags
	case f.subnets[id] != nil:
		return &f.subnets[id].Tags
	}
	return nil
}

func (e *fcEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f := e.f()
	w := "CreateTags " + strings.Join(in.Resources, ",")
	if err := f.begin(w); err != nil {
		return nil, err
	}
	for _, id := range in.Resources {
		p := f.tagsOf(id)
		if p == nil {
			return nil, apiErr("InvalidID")
		}
		m := ec2TagMap(*p)
		for _, t := range in.Tags {
			m[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		*p = nil
		for _, k := range sortedKeys(m) {
			*p = append(*p, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
		}
	}
	return &ec2.CreateTagsOutput{}, f.end(w)
}

func (e *fcEC2) DescribeLaunchTemplates(_ context.Context, in *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	if err := e.f().readIn("DescribeLaunchTemplates", in); err != nil {
		return nil, err
	}
	out := &ec2.DescribeLaunchTemplatesOutput{}
	for _, id := range sortedKeysOf(e.lts) {
		lt := e.lts[id].lt
		if match(in.Filters, nil, map[string][]string{"launch-template-name": {aws.ToString(lt.LaunchTemplateName)}}) {
			out.LaunchTemplates = append(out.LaunchTemplates, lt)
		}
	}
	return out, nil
}

// responseData is what DescribeLaunchTemplateVersions returns for the
// fields the tool compares.
func responseData(d *ec2types.RequestLaunchTemplateData) *ec2types.ResponseLaunchTemplateData {
	out := &ec2types.ResponseLaunchTemplateData{ImageId: d.ImageId, UserData: d.UserData, SecurityGroupIds: d.SecurityGroupIds, InstanceType: d.InstanceType}
	if d.IamInstanceProfile != nil {
		out.IamInstanceProfile = &ec2types.LaunchTemplateIamInstanceProfileSpecification{Name: d.IamInstanceProfile.Name}
	}
	if d.CpuOptions != nil {
		out.CpuOptions = &ec2types.LaunchTemplateCpuOptions{NestedVirtualization: d.CpuOptions.NestedVirtualization}
	}
	if d.EnclaveOptions != nil {
		out.EnclaveOptions = &ec2types.LaunchTemplateEnclaveOptions{Enabled: d.EnclaveOptions.Enabled}
	}
	if m := d.MetadataOptions; m != nil {
		out.MetadataOptions = &ec2types.LaunchTemplateInstanceMetadataOptions{HttpTokens: m.HttpTokens,
			HttpPutResponseHopLimit: m.HttpPutResponseHopLimit, HttpEndpoint: m.HttpEndpoint}
	}
	for _, b := range d.BlockDeviceMappings {
		bm := ec2types.LaunchTemplateBlockDeviceMapping{DeviceName: b.DeviceName}
		if b.Ebs != nil {
			bm.Ebs = &ec2types.LaunchTemplateEbsBlockDevice{VolumeSize: b.Ebs.VolumeSize, VolumeType: b.Ebs.VolumeType,
				Encrypted: b.Ebs.Encrypted, DeleteOnTermination: b.Ebs.DeleteOnTermination}
		}
		out.BlockDeviceMappings = append(out.BlockDeviceMappings, bm)
	}
	return out
}

func (e *fcEC2) DescribeLaunchTemplateVersions(_ context.Context, in *ec2.DescribeLaunchTemplateVersionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	if err := e.f().readIn("DescribeLaunchTemplateVersions", in); err != nil {
		return nil, err
	}
	lt := e.lts[aws.ToString(in.LaunchTemplateId)]
	if lt == nil {
		return nil, apiErr("InvalidLaunchTemplateId.NotFound")
	}
	out := &ec2.DescribeLaunchTemplateVersionsOutput{}
	for _, v := range in.Versions {
		n, _ := strconv.Atoi(v)
		if n >= 1 && n <= len(lt.versions) {
			out.LaunchTemplateVersions = append(out.LaunchTemplateVersions, ec2types.LaunchTemplateVersion{
				VersionNumber: aws.Int64(int64(n)), LaunchTemplateData: responseData(lt.versions[n-1])})
		}
	}
	return out, nil
}

func (e *fcEC2) CreateLaunchTemplate(_ context.Context, in *ec2.CreateLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateOutput, error) {
	f := e.f()
	for _, lt := range f.lts {
		if aws.ToString(lt.lt.LaunchTemplateName) == aws.ToString(in.LaunchTemplateName) {
			return nil, apiErr("InvalidLaunchTemplateName.AlreadyExistsException")
		}
	}
	w := "CreateLaunchTemplate " + aws.ToString(in.LaunchTemplateName)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("lt")
	f.lts[id] = &fakeLT{lt: ec2types.LaunchTemplate{LaunchTemplateId: aws.String(id), LaunchTemplateName: in.LaunchTemplateName,
		LatestVersionNumber: aws.Int64(1), Tags: specTags(in.TagSpecifications)}, versions: []*ec2types.RequestLaunchTemplateData{in.LaunchTemplateData}}
	if err := f.end(w); err != nil {
		return nil, err
	}
	c := f.lts[id].lt
	return &ec2.CreateLaunchTemplateOutput{LaunchTemplate: &c}, nil
}

func (e *fcEC2) CreateLaunchTemplateVersion(_ context.Context, in *ec2.CreateLaunchTemplateVersionInput, _ ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateVersionOutput, error) {
	f := e.f()
	lt := f.lts[aws.ToString(in.LaunchTemplateId)]
	w := "CreateLaunchTemplateVersion " + aws.ToString(lt.lt.LaunchTemplateName)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	lt.versions = append(lt.versions, in.LaunchTemplateData)
	n := int64(len(lt.versions))
	lt.lt.LatestVersionNumber = aws.Int64(n)
	if err := f.end(w); err != nil {
		return nil, err
	}
	return &ec2.CreateLaunchTemplateVersionOutput{LaunchTemplateVersion: &ec2types.LaunchTemplateVersion{VersionNumber: aws.Int64(n)}}, nil
}

// --- EKS ----------------------------------------------------------------------

type fcEKS clusterFake

func (e *fcEKS) f() *clusterFake { return (*clusterFake)(e) }

func (e *fcEKS) clusterExists(name *string) bool {
	return e.cluster != nil && aws.ToString(e.cluster.Name) == aws.ToString(name)
}

func (e *fcEKS) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if err := e.f().read("DescribeCluster"); err != nil {
		return nil, err
	}
	if !e.clusterExists(in.Name) {
		return nil, apiErr("ResourceNotFoundException")
	}
	c := *e.cluster
	// An update ends between two reads, when the test says so.
	if e.cluster.Status == ekstypes.ClusterStatusUpdating && e.updateEnds {
		e.cluster.Status = ekstypes.ClusterStatusActive
	}
	// The cluster becomes ACTIVE between two reads, with what a node needs
	// to join it.
	if e.cluster.Status == ekstypes.ClusterStatusCreating {
		e.cluster.Status = ekstypes.ClusterStatusActive
		e.cluster.Endpoint = aws.String("https://FAKE.gr7.us-west-2.eks.amazonaws.com")
		e.cluster.CertificateAuthority = &ekstypes.Certificate{Data: aws.String("RkFLRS1DQQ==")}
		e.cluster.KubernetesNetworkConfig = &ekstypes.KubernetesNetworkConfigResponse{ServiceIpv4Cidr: aws.String("10.100.0.0/16")}
	}
	return &eks.DescribeClusterOutput{Cluster: &c}, nil
}

func (e *fcEKS) CreateCluster(_ context.Context, in *eks.CreateClusterInput, _ ...func(*eks.Options)) (*eks.CreateClusterOutput, error) {
	f := e.f()
	if f.cluster != nil {
		return nil, apiErr("ResourceInUseException")
	}
	role := strings.TrimPrefix(aws.ToString(in.RoleArn), "arn:aws:iam::111122223333:role/")
	if f.roles[role] == nil {
		return nil, apiErr("InvalidParameterException")
	}
	vpc := ""
	for _, s := range in.ResourcesVpcConfig.SubnetIds {
		sub := f.subnets[s]
		if sub == nil {
			return nil, apiErr("InvalidParameterException")
		}
		vpc = aws.ToString(sub.VpcId)
	}
	w := "CreateCluster " + aws.ToString(in.Name)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.clusterIn = in
	sg := f.id("sg")
	f.sgs[sg] = &ec2types.SecurityGroup{GroupId: aws.String(sg), GroupName: aws.String("eks-cluster-sg-" + aws.ToString(in.Name)), VpcId: aws.String(vpc)}
	f.cluster = &ekstypes.Cluster{Name: in.Name, Status: ekstypes.ClusterStatusCreating, Version: in.Version, Tags: in.Tags,
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{VpcId: aws.String(vpc), SubnetIds: in.ResourcesVpcConfig.SubnetIds,
			ClusterSecurityGroupId: aws.String(sg), PublicAccessCidrs: in.ResourcesVpcConfig.PublicAccessCidrs}}
	if in.ComputeConfig != nil {
		f.cluster.ComputeConfig = &ekstypes.ComputeConfigResponse{Enabled: in.ComputeConfig.Enabled}
	}
	if err := f.end(w); err != nil {
		return nil, err
	}
	c := *f.cluster
	return &eks.CreateClusterOutput{Cluster: &c}, nil
}

func (e *fcEKS) DescribeAccessEntry(_ context.Context, in *eks.DescribeAccessEntryInput, _ ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error) {
	// deny takes "DescribeAccessEntry <principal>" too, for one entry.
	if err := e.f().read("DescribeAccessEntry"); err != nil {
		return nil, err
	}
	if code, ok := e.deny["DescribeAccessEntry "+aws.ToString(in.PrincipalArn)]; ok {
		return nil, apiErr(code)
	}
	tags, ok := e.entries[aws.ToString(in.PrincipalArn)]
	if !e.clusterExists(in.ClusterName) || !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	t := e.entryTypes[aws.ToString(in.PrincipalArn)]
	if t == "" {
		t = "STANDARD"
	}
	return &eks.DescribeAccessEntryOutput{AccessEntry: &ekstypes.AccessEntry{PrincipalArn: in.PrincipalArn, Type: aws.String(t), Tags: tags}}, nil
}

func (e *fcEKS) CreateAccessEntry(_ context.Context, in *eks.CreateAccessEntryInput, _ ...func(*eks.Options)) (*eks.CreateAccessEntryOutput, error) {
	f := e.f()
	p := aws.ToString(in.PrincipalArn)
	if _, ok := f.entries[p]; ok {
		return nil, apiErr("ResourceInUseException")
	}
	if !strings.Contains(p, ":role/") && !strings.Contains(p, ":user/") {
		return nil, apiErr("InvalidParameterException")
	}
	if t := aws.ToString(in.Type); t != "STANDARD" && t != "EC2_LINUX" {
		return nil, apiErr("InvalidParameterException")
	}
	if t := aws.ToString(in.Type); t == "EC2_LINUX" && f.roles[strings.TrimPrefix(p, "arn:aws:iam::111122223333:role/")] == nil {
		return nil, apiErr("InvalidParameterException")
	}
	w := "CreateAccessEntry " + p
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.entries[p] = in.Tags
	f.entryTypes[p] = aws.ToString(in.Type)
	return &eks.CreateAccessEntryOutput{}, f.end(w)
}

func (e *fcEKS) ListAssociatedAccessPolicies(_ context.Context, in *eks.ListAssociatedAccessPoliciesInput, _ ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return &eks.ListAssociatedAccessPoliciesOutput{AssociatedAccessPolicies: e.access[aws.ToString(in.PrincipalArn)]}, nil
}

func (e *fcEKS) AssociateAccessPolicy(_ context.Context, in *eks.AssociateAccessPolicyInput, _ ...func(*eks.Options)) (*eks.AssociateAccessPolicyOutput, error) {
	f := e.f()
	p := aws.ToString(in.PrincipalArn)
	if _, ok := f.entries[p]; !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	w := "AssociateAccessPolicy " + aws.ToString(in.PolicyArn)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.access[p] = append(f.access[p], ekstypes.AssociatedAccessPolicy{PolicyArn: in.PolicyArn, AccessScope: in.AccessScope})
	return &eks.AssociateAccessPolicyOutput{}, f.end(w)
}

// anyNodegroupActive: a managed node group is ACTIVE, or a self-managed
// group has an instance in service.
func (e *fcEKS) anyNodegroupActive() bool {
	for _, ng := range e.ngs {
		if ng.Status == ekstypes.NodegroupStatusActive {
			return true
		}
	}
	for _, a := range e.asgs {
		if len(a.Instances) > 0 && !e.notReady {
			return true
		}
	}
	return false
}

func (e *fcEKS) DescribeAddon(_ context.Context, in *eks.DescribeAddonInput, _ ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	a := e.addons[aws.ToString(in.AddonName)]
	if !e.clusterExists(in.ClusterName) || a == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	c := *a
	// The networking add-ons come up on their own; coredns and the EBS CSI
	// driver stay DEGRADED until there are nodes, as on EKS: a node group's,
	// or on Auto Mode the nodes Auto Mode launches for their Pods.
	auto := e.cluster != nil && e.cluster.ComputeConfig != nil && aws.ToBool(e.cluster.ComputeConfig.Enabled)
	if a.Status == ekstypes.AddonStatusCreating || a.Status == ekstypes.AddonStatusDegraded {
		switch {
		case aws.ToString(a.AddonName) != "coredns" && aws.ToString(a.AddonName) != "aws-ebs-csi-driver", e.anyNodegroupActive(), auto:
			a.Status = ekstypes.AddonStatusActive
		default:
			a.Status = ekstypes.AddonStatusDegraded
		}
	}
	return &eks.DescribeAddonOutput{Addon: &c}, nil
}

func (e *fcEKS) CreateAddon(_ context.Context, in *eks.CreateAddonInput, _ ...func(*eks.Options)) (*eks.CreateAddonOutput, error) {
	f := e.f()
	name := aws.ToString(in.AddonName)
	if !e.clusterExists(in.ClusterName) {
		return nil, apiErr("ResourceNotFoundException")
	}
	if f.addons[name] != nil {
		return nil, apiErr("ResourceInUseException")
	}
	for _, p := range in.PodIdentityAssociations {
		if f.roles[strings.TrimPrefix(aws.ToString(p.RoleArn), "arn:aws:iam::111122223333:role/")] == nil {
			return nil, apiErr("InvalidParameterException")
		}
	}
	w := "CreateAddon " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.addonIn[name] = in
	f.addons[name] = &ekstypes.Addon{AddonName: in.AddonName, Status: ekstypes.AddonStatusCreating, ConfigurationValues: in.ConfigurationValues, Tags: in.Tags}
	return &eks.CreateAddonOutput{}, f.end(w)
}

func (e *fcEKS) UpdateAddon(_ context.Context, in *eks.UpdateAddonInput, _ ...func(*eks.Options)) (*eks.UpdateAddonOutput, error) {
	f := e.f()
	w := "UpdateAddon " + aws.ToString(in.AddonName)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.addons[aws.ToString(in.AddonName)].ConfigurationValues = in.ConfigurationValues
	return &eks.UpdateAddonOutput{}, f.end(w)
}

func (e *fcEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	// Recorded by node group, so a test can deny one and count them.
	if err := e.f().readIn("DescribeNodegroup "+aws.ToString(in.NodegroupName), in); err != nil {
		return nil, err
	}
	ng := e.ngs[aws.ToString(in.NodegroupName)]
	if !e.clusterExists(in.ClusterName) || ng == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	c := *ng
	if ng.Status == ekstypes.NodegroupStatusCreating {
		ng.Status = ekstypes.NodegroupStatusActive
		ng.Resources = &ekstypes.NodegroupResources{AutoScalingGroups: []ekstypes.AutoScalingGroup{{Name: aws.String("eks-" + aws.ToString(ng.NodegroupName) + "-asg")}}}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: &c}, nil
}

// ListNodegroups lists the managed node groups by name, two to a page, so
// a caller that reads only the first page misses some.
func (e *fcEKS) ListNodegroups(_ context.Context, in *eks.ListNodegroupsInput, _ ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	if err := e.f().readIn("ListNodegroups", in); err != nil {
		return nil, err
	}
	if !e.clusterExists(in.ClusterName) {
		return nil, apiErr("ResourceNotFoundException")
	}
	names := sortedKeysOf(e.ngs)
	start, _ := strconv.Atoi(aws.ToString(in.NextToken))
	end := min(start+2, len(names))
	out := &eks.ListNodegroupsOutput{Nodegroups: names[start:end]}
	if end < len(names) {
		out.NextToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (e *fcEKS) CreateNodegroup(_ context.Context, in *eks.CreateNodegroupInput, _ ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error) {
	f := e.f()
	name := aws.ToString(in.NodegroupName)
	if !e.clusterExists(in.ClusterName) || f.cluster.Status != ekstypes.ClusterStatusActive {
		return nil, apiErr("ResourceNotFoundException")
	}
	if f.ngs[name] != nil {
		return nil, apiErr("ResourceInUseException")
	}
	if f.roles[strings.TrimPrefix(aws.ToString(in.NodeRole), "arn:aws:iam::111122223333:role/")] == nil {
		return nil, apiErr("InvalidParameterException")
	}
	lt := f.lts[aws.ToString(in.LaunchTemplate.Id)]
	if v, _ := strconv.Atoi(aws.ToString(in.LaunchTemplate.Version)); lt == nil || v < 1 || v > len(lt.versions) {
		return nil, apiErr("InvalidParameterException")
	}
	w := "CreateNodegroup " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.ngIn[name] = in
	// EKS gives the node role an EC2_LINUX access entry of its own, without
	// the owner tag (when the test asks for it: eksNodeEntry).
	if nr := aws.ToString(in.NodeRole); nr != "" && f.eksNodeEntry {
		if _, ok := f.entries[nr]; !ok {
			f.entries[nr], f.entryTypes[nr] = nil, "EC2_LINUX"
		}
	}
	f.ngs[name] = &ekstypes.Nodegroup{NodegroupName: in.NodegroupName, Status: ekstypes.NodegroupStatusCreating, Tags: in.Tags,
		AmiType: in.AmiType, LaunchTemplate: &ekstypes.LaunchTemplateSpecification{Id: in.LaunchTemplate.Id, Version: in.LaunchTemplate.Version}}
	if in.AmiType == "" {
		f.ngs[name].AmiType = ekstypes.AMITypesCustom
	}
	return &eks.CreateNodegroupOutput{}, f.end(w)
}

// --- IAM ----------------------------------------------------------------------

type fcIAM clusterFake

func (e *fcIAM) f() *clusterFake { return (*clusterFake)(e) }

func (e *fcIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if err := e.f().read("GetRole"); err != nil {
		return nil, err
	}
	r := e.roles[aws.ToString(in.RoleName)]
	if r == nil {
		return nil, apiErr("NoSuchEntity")
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Arn: aws.String(r.arn),
		AssumeRolePolicyDocument: aws.String(url.QueryEscape(r.trust))}}, nil
}

func (e *fcIAM) ListRoleTags(_ context.Context, in *iam.ListRoleTagsInput, _ ...func(*iam.Options)) (*iam.ListRoleTagsOutput, error) {
	out := &iam.ListRoleTagsOutput{}
	for k, v := range e.roles[aws.ToString(in.RoleName)].tags {
		out.Tags = append(out.Tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (e *fcIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f := e.f()
	name := aws.ToString(in.RoleName)
	if f.roles[name] != nil {
		return nil, apiErr("EntityAlreadyExists")
	}
	w := "CreateRole " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.roles[name] = &fakeRole{arn: "arn:aws:iam::111122223333:role/" + name, trust: aws.ToString(in.AssumeRolePolicyDocument), tags: tags}
	return &iam.CreateRoleOutput{}, f.end(w)
}

func (e *fcIAM) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f := e.f()
	w := "UpdateAssumeRolePolicy " + aws.ToString(in.RoleName)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.roles[aws.ToString(in.RoleName)].trust = aws.ToString(in.PolicyDocument)
	return &iam.UpdateAssumeRolePolicyOutput{}, f.end(w)
}

func (e *fcIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f := e.f()
	r := f.roles[aws.ToString(in.RoleName)]
	if r == nil {
		return nil, apiErr("NoSuchEntity")
	}
	w := "PutRolePolicy " + aws.ToString(in.RoleName) + " " + aws.ToString(in.PolicyName)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	if r.inline == nil {
		r.inline = map[string]string{}
	}
	r.inline[aws.ToString(in.PolicyName)] = aws.ToString(in.PolicyDocument)
	return &iam.PutRolePolicyOutput{}, f.end(w)
}

// GetRolePolicy returns the document URL-encoded, as IAM does.
func (e *fcIAM) GetRolePolicy(_ context.Context, in *iam.GetRolePolicyInput, _ ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	if err := e.f().read("GetRolePolicy"); err != nil {
		return nil, err
	}
	r := e.roles[aws.ToString(in.RoleName)]
	if r == nil || r.inline[aws.ToString(in.PolicyName)] == "" {
		return nil, apiErr("NoSuchEntity")
	}
	return &iam.GetRolePolicyOutput{PolicyDocument: aws.String(url.QueryEscape(r.inline[aws.ToString(in.PolicyName)]))}, nil
}

func (e *fcIAM) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, p := range e.roles[aws.ToString(in.RoleName)].attached {
		out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{PolicyArn: aws.String(p)})
	}
	return out, nil
}

func (e *fcIAM) AttachRolePolicy(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f := e.f()
	r := f.roles[aws.ToString(in.RoleName)]
	if r == nil {
		return nil, apiErr("NoSuchEntity")
	}
	w := "AttachRolePolicy " + aws.ToString(in.RoleName) + " " + aws.ToString(in.PolicyArn)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	if !slices.Contains(r.attached, aws.ToString(in.PolicyArn)) {
		r.attached = append(r.attached, aws.ToString(in.PolicyArn))
	}
	return &iam.AttachRolePolicyOutput{}, f.end(w)
}

// --- Auto Scaling and Service Quotas -------------------------------------------

type fcASG clusterFake

func (e *fcASG) DescribeTags(_ context.Context, in *autoscaling.DescribeTagsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeTagsOutput, error) {
	out := &autoscaling.DescribeTagsOutput{}
	name := in.Filters[0].Values[0]
	for _, k := range sortedKeys(e.asgTags[name]) {
		out.Tags = append(out.Tags, asgtypes.TagDescription{ResourceId: aws.String(name), Key: aws.String(k), Value: aws.String(e.asgTags[name][k])})
	}
	return out, nil
}

func (e *fcASG) CreateOrUpdateTags(_ context.Context, in *autoscaling.CreateOrUpdateTagsInput, _ ...func(*autoscaling.Options)) (*autoscaling.CreateOrUpdateTagsOutput, error) {
	f := (*clusterFake)(e)
	name := aws.ToString(in.Tags[0].ResourceId)
	w := "CreateOrUpdateTags " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	if f.asgTags[name] == nil {
		f.asgTags[name] = map[string]string{}
	}
	for _, t := range in.Tags {
		if !aws.ToBool(t.PropagateAtLaunch) {
			return nil, fmt.Errorf("tag %s does not propagate to instances", aws.ToString(t.Key))
		}
		f.asgTags[name][aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return &autoscaling.CreateOrUpdateTagsOutput{}, f.end(w)
}

type fcQuotas clusterFake

func (e *fcQuotas) GetServiceQuota(_ context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	v, ok := e.quotas[aws.ToString(in.QuotaCode)]
	if !ok {
		return nil, apiErr("NoSuchResourceException")
	}
	return &servicequotas.GetServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: aws.Float64(v)}}, nil
}

func (e *fcQuotas) GetAWSDefaultServiceQuota(_ context.Context, in *servicequotas.GetAWSDefaultServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetAWSDefaultServiceQuotaOutput, error) {
	return &servicequotas.GetAWSDefaultServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: aws.Float64(5)}}, nil
}

// --- self-managed node groups ---------------------------------------------------

func (e *fcIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	if err := e.f().read("GetInstanceProfile"); err != nil {
		return nil, err
	}
	name := aws.ToString(in.InstanceProfileName)
	p := e.profiles[name]
	if p == nil {
		return nil, apiErr("NoSuchEntity")
	}
	ip := &iamtypes.InstanceProfile{InstanceProfileName: in.InstanceProfileName}
	for _, r := range p.roles {
		ip.Roles = append(ip.Roles, iamtypes.Role{RoleName: aws.String(r)})
	}
	if p.tagsInGet {
		for _, k := range sortedKeys(p.tags) {
			ip.Tags = append(ip.Tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(p.tags[k])})
		}
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: ip}, nil
}

func (e *fcIAM) ListInstanceProfileTags(_ context.Context, in *iam.ListInstanceProfileTagsInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error) {
	if err := e.f().read("ListInstanceProfileTags"); err != nil {
		return nil, err
	}
	p := e.profiles[aws.ToString(in.InstanceProfileName)]
	if p == nil {
		return nil, apiErr("NoSuchEntity")
	}
	out := &iam.ListInstanceProfileTagsOutput{}
	for _, k := range sortedKeys(p.tags) {
		out.Tags = append(out.Tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(p.tags[k])})
	}
	return out, nil
}

func (e *fcIAM) CreateInstanceProfile(_ context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	f := e.f()
	name := aws.ToString(in.InstanceProfileName)
	if f.profiles[name] != nil {
		return nil, apiErr("EntityAlreadyExists")
	}
	w := "CreateInstanceProfile " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.profiles[name] = &fakeProfile{tags: tags}
	return &iam.CreateInstanceProfileOutput{}, f.end(w)
}

func (e *fcIAM) AddRoleToInstanceProfile(_ context.Context, in *iam.AddRoleToInstanceProfileInput, _ ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	f := e.f()
	p := f.profiles[aws.ToString(in.InstanceProfileName)]
	if p == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchEntity", Message: "Instance Profile " + aws.ToString(in.InstanceProfileName) + " cannot be found."}
	}
	if f.roleLag > 0 || f.roles[aws.ToString(in.RoleName)] == nil {
		f.roleLag = max(f.roleLag-1, 0)
		return nil, &smithy.GenericAPIError{Code: "NoSuchEntity", Message: "The role with name " + aws.ToString(in.RoleName) + " cannot be found."}
	}
	if len(p.roles) > 0 {
		return nil, apiErr("LimitExceeded")
	}
	w := "AddRoleToInstanceProfile " + aws.ToString(in.InstanceProfileName) + " " + aws.ToString(in.RoleName)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	p.roles = append(p.roles, aws.ToString(in.RoleName))
	return &iam.AddRoleToInstanceProfileOutput{}, f.end(w)
}

// DescribeAutoScalingGroups finds groups by name or by tag filter. A group
// launches its instances, InService, between two reads (unless asgStuck).
func (e *fcASG) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f := (*clusterFake)(e)
	if err := f.readIn("DescribeAutoScalingGroups", in); err != nil {
		return nil, err
	}
	if len(in.AutoScalingGroupNames) == 0 && len(in.Filters) == 0 {
		panic("fake: DescribeAutoScalingGroups with no names or filters lists every group in the Region")
	}
	out := &autoscaling.DescribeAutoScalingGroupsOutput{}
	for _, name := range sortedKeysOf(f.asgs) {
		a := f.asgs[name]
		if len(in.AutoScalingGroupNames) > 0 && !slices.Contains(in.AutoScalingGroupNames, name) {
			continue
		}
		if f.asgHidden[name] > 0 {
			f.asgHidden[name]--
			continue
		}
		tags := map[string]string{}
		for _, t := range a.Tags {
			tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		hit := true
		for _, fl := range in.Filters {
			k, ok := strings.CutPrefix(aws.ToString(fl.Name), "tag:")
			if !ok {
				panic("fake: unknown Auto Scaling filter " + aws.ToString(fl.Name))
			}
			hit = hit && slices.Contains(fl.Values, tags[k])
		}
		if !hit {
			continue
		}
		c := *a
		c.Instances = slices.Clone(a.Instances)
		out.AutoScalingGroups = append(out.AutoScalingGroups, c)
		for len(a.Instances) < int(aws.ToInt32(a.DesiredCapacity)) && !f.asgStuck {
			a.Instances = append(a.Instances, asgtypes.Instance{InstanceId: aws.String(f.id("i")), LifecycleState: asgtypes.LifecycleStateInService})
		}
	}
	return out, nil
}

func (e *fcASG) CreateAutoScalingGroup(_ context.Context, in *autoscaling.CreateAutoScalingGroupInput, _ ...func(*autoscaling.Options)) (*autoscaling.CreateAutoScalingGroupOutput, error) {
	f := (*clusterFake)(e)
	name := aws.ToString(in.AutoScalingGroupName)
	if f.asgs[name] != nil {
		return nil, apiErr("AlreadyExists")
	}
	lt := f.lts[aws.ToString(in.LaunchTemplate.LaunchTemplateId)]
	v, _ := strconv.Atoi(aws.ToString(in.LaunchTemplate.Version))
	if lt == nil || v < 1 || v > len(lt.versions) {
		return nil, apiErr("ValidationError")
	}
	// Without a mixed instances policy the template must name the instance
	// type (InvalidQueryParameter, as AWS answers).
	if in.MixedInstancesPolicy == nil && lt.versions[v-1].InstanceType == "" {
		return nil, &smithy.GenericAPIError{Code: "InvalidQueryParameter", Message: "An instance type does not exist in the specified launch template."}
	}
	// Auto Scaling checks the template's instance profile, as RunInstances would.
	if p := lt.versions[v-1].IamInstanceProfile; p == nil || f.profiles[aws.ToString(p.Name)] == nil {
		return nil, &smithy.GenericAPIError{Code: "ValidationError", Message: "Invalid IAM Instance Profile name"}
	}
	for _, s := range strings.Split(aws.ToString(in.VPCZoneIdentifier), ",") {
		if f.subnets[s] == nil {
			return nil, apiErr("ValidationError")
		}
	}
	for _, t := range in.Tags {
		if !aws.ToBool(t.PropagateAtLaunch) {
			return nil, fmt.Errorf("tag %s does not propagate to instances", aws.ToString(t.Key))
		}
	}
	w := "CreateAutoScalingGroup " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.asgIn[name] = in
	a := &asgtypes.AutoScalingGroup{AutoScalingGroupName: in.AutoScalingGroupName, DesiredCapacity: in.DesiredCapacity,
		MinSize: in.MinSize, MaxSize: in.MaxSize}
	for _, t := range in.Tags {
		a.Tags = append(a.Tags, asgtypes.TagDescription{ResourceId: in.AutoScalingGroupName, Key: t.Key, Value: t.Value, PropagateAtLaunch: t.PropagateAtLaunch})
	}
	f.asgs[name] = a
	f.asgHidden[name] = f.asgLag
	return &autoscaling.CreateAutoScalingGroupOutput{}, f.end(w)
}

func (e *fcASG) DescribeScalingActivities(_ context.Context, in *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	f := (*clusterFake)(e)
	if err := f.read("DescribeScalingActivities"); err != nil {
		return nil, err
	}
	return &autoscaling.DescribeScalingActivitiesOutput{Activities: f.activities[aws.ToString(in.AutoScalingGroupName)]}, nil
}

type fcSSM clusterFake

func (e *fcSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f := (*clusterFake)(e)
	if err := f.readIn("GetParameter", in); err != nil {
		return nil, err
	}
	v, ok := f.params[aws.ToString(in.Name)]
	if !ok {
		return nil, apiErr("ParameterNotFound")
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Name: in.Name, Value: aws.String(v)}}, nil
}

// kube is the cluster's Kubernetes API: a node per instance in service of
// each Auto Scaling group, and DesiredSize nodes per ACTIVE managed node
// group, labeled with the pool of its group; the nitro group's nodes
// advertise nitroAlloc.
func (f *clusterFake) kube(_ context.Context, c *ekstypes.Cluster) (nodeLister, error) {
	f.kubeCalls++
	if c == nil || c.Status != ekstypes.ClusterStatusActive {
		return nil, fmt.Errorf("fake: the Kubernetes API of a cluster that is not ACTIVE")
	}
	return fakeNodes{f}, nil
}

type fakeNodes struct{ f *clusterFake }

func (n fakeNodes) Nodes(_ context.Context, sel string) ([]kubeNode, error) {
	if err := n.f.read("ListNodes"); err != nil {
		return nil, err
	}
	if len(n.f.kubeErrs) > 0 {
		err := n.f.kubeErrs[0]
		n.f.kubeErrs = n.f.kubeErrs[1:]
		return nil, err
	}
	if n.f.kubeDown != nil {
		return nil, n.f.kubeDown
	}
	k, v, ok := strings.Cut(sel, "=")
	if !ok {
		panic("fake: label selector " + sel)
	}
	node := func(name, pool string) kubeNode {
		kn := kubeNode{Name: name, Labels: map[string]string{"ate.dev/pool": pool}, Ready: !n.f.notReady}
		if pool == "nitro" {
			kn.Ready = kn.Ready && !n.f.nitroNotReady
			if n.f.nitroAlloc != "" {
				kn.Allocatable = map[string]string{"cpu": "3920m", "eks.amazonaws.com/nitro-sandbox": n.f.nitroAlloc}
			}
		}
		return kn
	}
	var out []kubeNode
	for _, name := range sortedKeysOf(n.f.asgs) {
		pool := strings.TrimPrefix(name, "demo-")
		for _, i := range n.f.asgs[name].Instances {
			if kn := node(aws.ToString(i.InstanceId), pool); kn.Labels[k] == v {
				out = append(out, kn)
			}
		}
	}
	for _, name := range sortedKeysOf(n.f.ngs) {
		in := n.f.ngIn[name]
		if n.f.ngs[name].Status != ekstypes.NodegroupStatusActive || in == nil {
			continue
		}
		count := aws.ToInt32(in.ScalingConfig.DesiredSize)
		if name == "nitro" {
			count -= int32(n.f.nitroMissing)
		}
		for i := range count {
			if kn := node(fmt.Sprintf("ip-%s-%d", name, i), in.Labels["ate.dev/pool"]); kn.Labels[k] == v {
				out = append(out, kn)
			}
		}
	}
	return out, nil
}
