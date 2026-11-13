package cmd

import (
	"context"
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

	vpcs      map[string]*ec2types.Vpc
	dnsHosts  map[string]bool
	subnets   map[string]*ec2types.Subnet
	igws      map[string]*ec2types.InternetGateway
	eips      map[string]*ec2types.Address
	nats      map[string]*ec2types.NatGateway
	rts       map[string]*ec2types.RouteTable
	sgs       map[string]*ec2types.SecurityGroup
	acls      map[string]*ec2types.NetworkAcl
	lts       map[string]*fakeLT // by id
	types     map[string]ec2types.InstanceTypeInfo
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
}

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
	return ec2types.InstanceTypeInfo{InstanceType: ec2types.InstanceType(name), ProcessorInfo: p}
}

func newClusterFake() *clusterFake {
	f := &clusterFake{deny: map[string]string{},
		vpcs: map[string]*ec2types.Vpc{}, dnsHosts: map[string]bool{}, subnets: map[string]*ec2types.Subnet{},
		igws: map[string]*ec2types.InternetGateway{}, eips: map[string]*ec2types.Address{}, nats: map[string]*ec2types.NatGateway{},
		rts: map[string]*ec2types.RouteTable{}, sgs: map[string]*ec2types.SecurityGroup{}, acls: map[string]*ec2types.NetworkAcl{},
		lts: map[string]*fakeLT{}, quotas: map[string]float64{},
		types: map[string]ec2types.InstanceTypeInfo{
			"m8i.xlarge": nestedVirtType("m8i.xlarge", true), "m8i.2xlarge": nestedVirtType("m8i.2xlarge", true),
			"m7i.2xlarge": nestedVirtType("m7i.2xlarge", false)},
		offerings: map[string][]string{
			"us-west-2a": {"m8i.xlarge", "m8i.2xlarge", "m7i.2xlarge"}, "us-west-2b": {"m8i.xlarge", "m8i.2xlarge", "m7i.2xlarge"},
			"us-west-2c": {"m8i.xlarge", "m8i.2xlarge", "m7i.2xlarge"}, "us-west-2d": {"m7i.2xlarge"}},
		entries: map[string]map[string]string{}, access: map[string][]ekstypes.AssociatedAccessPolicy{},
		addons: map[string]*ekstypes.Addon{}, addonIn: map[string]*eks.CreateAddonInput{},
		ngs: map[string]*ekstypes.Nodegroup{}, ngIn: map[string]*eks.CreateNodegroupInput{},
		roles: map[string]*fakeRole{
			// The caller's own role (fsts: assumed-role/Admin/me).
			"Admin": {arn: "arn:aws:iam::111122223333:role/Admin", tags: map[string]string{}}},
		asgTags: map[string]map[string]string{},
	}
	for i, z := range []string{"a", "b", "c", "d"} {
		f.zones = append(f.zones, ec2types.AvailabilityZone{ZoneName: aws.String("us-west-2" + z), ZoneId: aws.String(fmt.Sprintf("usw2-az%d", i+1))})
	}
	// Somebody else's VPC and address, for the quota counts.
	f.vpcs["vpc-other"] = &ec2types.Vpc{VpcId: aws.String("vpc-other")}
	f.eips["eipalloc-other"] = &ec2types.Address{AllocationId: aws.String("eipalloc-other"), Domain: ec2types.DomainTypeVpc}
	return f
}

func (f *clusterFake) clients() *ClusterClients {
	return &ClusterClients{EC2: (*fcEC2)(f), EKS: (*fcEKS)(f), IAM: (*fcIAM)(f), ASG: (*fcASG)(f), Quotas: (*fcQuotas)(f)}
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
	if code, ok := f.deny[op]; ok {
		return apiErr(code)
	}
	return nil
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

func (e *fcEC2) DescribeInstanceTypeOfferings(_ context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
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

// tagsOf points at the tags of any taggable resource of the fake.
func (f *clusterFake) tagsOf(id string) *[]ec2types.Tag {
	switch {
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
	if err := e.f().read("DescribeLaunchTemplates"); err != nil {
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
	out := &ec2types.ResponseLaunchTemplateData{}
	if d.CpuOptions != nil {
		out.CpuOptions = &ec2types.LaunchTemplateCpuOptions{NestedVirtualization: d.CpuOptions.NestedVirtualization}
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
	// The cluster becomes ACTIVE between two reads.
	if e.cluster.Status == ekstypes.ClusterStatusCreating {
		e.cluster.Status = ekstypes.ClusterStatusActive
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
	if _, ok := e.entries[aws.ToString(in.PrincipalArn)]; !e.clusterExists(in.ClusterName) || !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	return &eks.DescribeAccessEntryOutput{AccessEntry: &ekstypes.AccessEntry{PrincipalArn: in.PrincipalArn}}, nil
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
	w := "CreateAccessEntry " + p
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.entries[p] = in.Tags
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

func (e *fcEKS) anyNodegroupActive() bool {
	for _, ng := range e.ngs {
		if ng.Status == ekstypes.NodegroupStatusActive {
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
	// driver stay DEGRADED until there are nodes (create-cluster.sh's trap).
	if a.Status == ekstypes.AddonStatusCreating || a.Status == ekstypes.AddonStatusDegraded {
		switch {
		case aws.ToString(a.AddonName) != "coredns" && aws.ToString(a.AddonName) != "aws-ebs-csi-driver", e.anyNodegroupActive():
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
	f.ngs[name] = &ekstypes.Nodegroup{NodegroupName: in.NodegroupName, Status: ekstypes.NodegroupStatusCreating, Tags: in.Tags}
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
