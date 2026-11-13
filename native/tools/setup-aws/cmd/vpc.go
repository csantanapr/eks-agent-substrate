package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The VPC is eksctl's default layout: 192.168.0.0/16, one public and one
// private /19 per Availability Zone, and ONE NAT gateway for all private
// subnets (account Elastic IP and VPC quotas are tight; one NAT gateway per
// zone would survive a zone outage at three times the cost).
//
// Nodes and the cluster's network interfaces live in the private subnets
// only. The public subnets hold the NAT gateway. Auto Mode launches nodes in
// the cluster's subnets, so a cluster given public subnets would get nodes
// there (https://docs.aws.amazon.com/eks/latest/eksctl/auto-mode.html).
const vpcCIDR = "192.168.0.0/16"

// subnetCIDR is the i-th public (0-2) or private (3-5) /19 of vpcCIDR.
func subnetCIDR(public bool, i int) string {
	if !public {
		i += maxAZs
	}
	return fmt.Sprintf("192.168.%d.0/19", i*32)
}

// Names (the Name tag) of the network resources. Subnets are numbered, not
// named by zone, so a rerun finds each one even if zone offerings changed.
func (r *clusterRun) netName(part string) string { return r.e.names.Cluster + "-" + part }

func subnetKind(public bool) string {
	if public {
		return "public"
	}
	return "private"
}

func (r *clusterRun) ensureNetwork(ctx context.Context) error {
	if r.cluster != nil {
		return r.adoptClusterNetwork(ctx)
	}
	if err := r.ensureVPC(ctx); err != nil {
		return err
	}
	if err := r.ensureSubnets(ctx); err != nil {
		return err
	}
	igw, err := r.ensureInternetGateway(ctx)
	if err != nil {
		return err
	}
	nat, err := r.ensureNAT(ctx)
	if err != nil {
		return err
	}
	if err := r.ensureRouteTable(ctx, true, igw); err != nil {
		return err
	}
	return r.ensureRouteTable(ctx, false, nat)
}

// adoptClusterNetwork uses an existing cluster's VPC and private subnets
// (tagged kubernetes.io/role/internal-elb=1, as this tool and eksctl both
// tag them) for the node groups, and changes nothing.
func (r *clusterRun) adoptClusterNetwork(ctx context.Context) error {
	vc := r.cluster.ResourcesVpcConfig
	if vc == nil || len(vc.SubnetIds) == 0 {
		return fmt.Errorf("cluster %s reports no subnets", r.e.names.Cluster)
	}
	r.vpcID = aws.ToString(vc.VpcId)
	out, err := r.c.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: vc.SubnetIds})
	if err != nil {
		return fmt.Errorf("describe the cluster's subnets: %w", err)
	}
	for _, s := range out.Subnets {
		if ec2TagMap(s.Tags)["kubernetes.io/role/internal-elb"] == "1" {
			r.private = append(r.private, aws.ToString(s.SubnetId))
		} else {
			r.public = append(r.public, aws.ToString(s.SubnetId))
		}
	}
	if len(r.private) == 0 {
		return fmt.Errorf("none of cluster %s's subnets is tagged kubernetes.io/role/internal-elb=1, so the private ones for the node groups are unknown", r.e.names.Cluster)
	}
	slog.Info("using the cluster's network", "vpc", r.vpcID, "privateSubnets", strings.Join(r.private, ","))
	return nil
}

// findVPC returns the VPC an earlier run created for this deployment, or "".
func (r *clusterRun) findVPC(ctx context.Context) (string, error) {
	out, err := r.c.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: r.tagFilters()})
	if err != nil {
		return "", fmt.Errorf("look up the VPC: %w", err)
	}
	switch len(out.Vpcs) {
	case 0:
		return "", nil
	case 1:
		return aws.ToString(out.Vpcs[0].VpcId), nil
	}
	var ids []string
	for _, v := range out.Vpcs {
		ids = append(ids, aws.ToString(v.VpcId))
	}
	return "", fmt.Errorf("%d VPCs carry %s=%s (%s); expected one", len(ids), OwnerTagKey, r.e.names.OwnerValue, strings.Join(ids, ", "))
}

func (r *clusterRun) ensureVPC(ctx context.Context) error {
	if r.vpcID == "" {
		r.vpcID = plannedPrefix + "vpc"
		if err := r.create("VPC "+r.netName("vpc")+" ("+vpcCIDR+")", func() error {
			out, err := r.c.EC2.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String(vpcCIDR),
				TagSpecifications: r.tagSpec(ec2types.ResourceTypeVpc, r.netName("vpc"))})
			if err != nil {
				return err
			}
			r.vpcID = aws.ToString(out.Vpc.VpcId)
			return nil
		}); err != nil {
			return fmt.Errorf("create VPC: %w", err)
		}
	} else {
		slog.Info("VPC exists", "vpc", r.vpcID)
	}
	if planned(r.vpcID) {
		r.changes = append(r.changes, "VPC DNS hostnames", "tags of the VPC's default route table, security group and network ACL")
		slog.Info("plan: would turn on DNS hostnames and tag the VPC's default route table, security group and network ACL")
		return nil
	}
	// Nodes register with the cluster only when the VPC resolves DNS and
	// gives instances DNS hostnames.
	for _, attr := range []ec2types.VpcAttributeName{ec2types.VpcAttributeNameEnableDnsSupport, ec2types.VpcAttributeNameEnableDnsHostnames} {
		out, err := r.c.EC2.DescribeVpcAttribute(ctx, &ec2.DescribeVpcAttributeInput{VpcId: aws.String(r.vpcID), Attribute: attr})
		if err != nil {
			return fmt.Errorf("read %s of %s: %w", attr, r.vpcID, err)
		}
		on := out.EnableDnsSupport
		if attr == ec2types.VpcAttributeNameEnableDnsHostnames {
			on = out.EnableDnsHostnames
		}
		if on != nil && aws.ToBool(on.Value) {
			continue
		}
		in := &ec2.ModifyVpcAttributeInput{VpcId: aws.String(r.vpcID)}
		if attr == ec2types.VpcAttributeNameEnableDnsSupport {
			in.EnableDnsSupport = &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}
		} else {
			in.EnableDnsHostnames = &ec2types.AttributeBooleanValue{Value: aws.Bool(true)}
		}
		if err := r.change(fmt.Sprintf("turn on %s of %s", attr, r.vpcID), func() error {
			_, err := r.c.EC2.ModifyVpcAttribute(ctx, in)
			return err
		}); err != nil {
			return err
		}
	}
	return r.tagVPCDefaults(ctx)
}

// tagVPCDefaults tags the main route table, default security group and
// default network ACL that EC2 made with the VPC.
func (r *clusterRun) tagVPCDefaults(ctx context.Context) error {
	vpc := filter("vpc-id", r.vpcID)
	rts, err := r.c.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: []ec2types.Filter{vpc, filter("association.main", "true")}})
	if err != nil {
		return fmt.Errorf("describe the main route table of %s: %w", r.vpcID, err)
	}
	for _, t := range rts.RouteTables {
		if err := r.addMissingTags(ctx, "main route table", aws.ToString(t.RouteTableId), t.Tags); err != nil {
			return err
		}
	}
	sgs, err := r.c.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []ec2types.Filter{vpc, filter("group-name", "default")}})
	if err != nil {
		return fmt.Errorf("describe the default security group of %s: %w", r.vpcID, err)
	}
	for _, g := range sgs.SecurityGroups {
		if err := r.addMissingTags(ctx, "default security group", aws.ToString(g.GroupId), g.Tags); err != nil {
			return err
		}
	}
	acls, err := r.c.EC2.DescribeNetworkAcls(ctx, &ec2.DescribeNetworkAclsInput{Filters: []ec2types.Filter{vpc, filter("default", "true")}})
	if err != nil {
		return fmt.Errorf("describe the default network ACL of %s: %w", r.vpcID, err)
	}
	for _, a := range acls.NetworkAcls {
		if err := r.addMissingTags(ctx, "default network ACL", aws.ToString(a.NetworkAclId), a.Tags); err != nil {
			return err
		}
	}
	return nil
}

func (r *clusterRun) ensureSubnets(ctx context.Context) error {
	have := map[string]ec2types.Subnet{}
	if !planned(r.vpcID) {
		out, err := r.c.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: r.tagFilters(filter("vpc-id", r.vpcID))})
		if err != nil {
			return fmt.Errorf("describe the subnets of %s: %w", r.vpcID, err)
		}
		for _, s := range out.Subnets {
			have[ec2TagMap(s.Tags)["Name"]] = s
		}
	}
	for i := range r.azs {
		az := r.azs[i]
		// A pair keeps the zone an earlier run gave either of its subnets.
		for _, public := range []bool{true, false} {
			if s, ok := have[r.netName(fmt.Sprintf("%s-%d", subnetKind(public), i+1))]; ok {
				az = aws.ToString(s.AvailabilityZone)
			}
		}
		for _, public := range []bool{true, false} {
			name := r.netName(fmt.Sprintf("%s-%d", subnetKind(public), i+1))
			id := ""
			if s, ok := have[name]; ok {
				id = aws.ToString(s.SubnetId)
			} else {
				id = plannedPrefix + name
				role := "kubernetes.io/role/internal-elb"
				if public {
					role = "kubernetes.io/role/elb"
				}
				tags := r.ec2Tags(map[string]string{"Name": name, role: "1", "kubernetes.io/cluster/" + r.e.names.Cluster: "shared"})
				cidr := subnetCIDR(public, i)
				if err := r.create(fmt.Sprintf("subnet %s (%s, %s)", name, cidr, az), func() error {
					out, err := r.c.EC2.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: aws.String(r.vpcID), CidrBlock: aws.String(cidr),
						AvailabilityZone: aws.String(az), TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSubnet, Tags: tags}}})
					if err != nil {
						return err
					}
					id = aws.ToString(out.Subnet.SubnetId)
					return nil
				}); err != nil {
					return fmt.Errorf("create subnet %s: %w", name, err)
				}
			}
			if public {
				r.public = append(r.public, id)
			} else {
				r.private = append(r.private, id)
			}
		}
	}
	return nil
}

// ensureInternetGateway returns the gateway's ID, attached to the VPC.
func (r *clusterRun) ensureInternetGateway(ctx context.Context) (string, error) {
	name := r.netName("igw")
	out, err := r.c.EC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: r.tagFilters()})
	if err != nil {
		return "", fmt.Errorf("look up the internet gateway: %w", err)
	}
	var id string
	attached := false
	switch len(out.InternetGateways) {
	case 0:
		id = plannedPrefix + "igw"
		if err := r.create("internet gateway "+name, func() error {
			o, err := r.c.EC2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{TagSpecifications: r.tagSpec(ec2types.ResourceTypeInternetGateway, name)})
			if err != nil {
				return err
			}
			id = aws.ToString(o.InternetGateway.InternetGatewayId)
			return nil
		}); err != nil {
			return "", fmt.Errorf("create internet gateway: %w", err)
		}
	case 1:
		g := out.InternetGateways[0]
		id = aws.ToString(g.InternetGatewayId)
		for _, a := range g.Attachments {
			if aws.ToString(a.VpcId) == r.vpcID {
				attached = true
			} else {
				return "", fmt.Errorf("internet gateway %s is attached to %s, not %s", id, aws.ToString(a.VpcId), r.vpcID)
			}
		}
	default:
		return "", fmt.Errorf("%d internet gateways carry this deployment's tags; expected one", len(out.InternetGateways))
	}
	if !attached {
		if err := r.change(fmt.Sprintf("attach internet gateway %s to %s", id, r.vpcID), func() error {
			_, err := r.c.EC2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(id), VpcId: aws.String(r.vpcID)})
			return err
		}); err != nil {
			return "", fmt.Errorf("attach internet gateway: %w", err)
		}
	}
	return id, nil
}

// findEIP returns the NAT gateway's Elastic IP from an earlier run, or "".
func (r *clusterRun) findEIP(ctx context.Context) (string, error) {
	out, err := r.c.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: r.tagFilters(filter("tag:Name", r.netName("nat")))})
	if err != nil {
		return "", fmt.Errorf("look up the NAT gateway's Elastic IP: %w", err)
	}
	switch len(out.Addresses) {
	case 0:
		return "", nil
	case 1:
		return aws.ToString(out.Addresses[0].AllocationId), nil
	}
	return "", fmt.Errorf("%d Elastic IPs are named %s; expected one", len(out.Addresses), r.netName("nat"))
}

// ensureNAT returns the ID of the NAT gateway in the first public subnet,
// once it is available.
func (r *clusterRun) ensureNAT(ctx context.Context) (string, error) {
	name := r.netName("nat")
	var nat ec2types.NatGateway
	if !planned(r.vpcID) {
		out, err := r.c.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{Filter: r.tagFilters(
			filter("vpc-id", r.vpcID), filter("state", "pending", "available"))})
		if err != nil {
			return "", fmt.Errorf("look up the NAT gateway: %w", err)
		}
		if len(out.NatGateways) > 1 {
			return "", fmt.Errorf("%d NAT gateways carry this deployment's tags in %s; expected one", len(out.NatGateways), r.vpcID)
		}
		if len(out.NatGateways) == 1 {
			nat = out.NatGateways[0]
		}
	}
	id := aws.ToString(nat.NatGatewayId)
	if id == "" {
		if r.eipID == "" {
			r.eipID = plannedPrefix + "eip"
			if err := r.create("Elastic IP "+name, func() error {
				o, err := r.c.EC2.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeVpc,
					TagSpecifications: r.tagSpec(ec2types.ResourceTypeElasticIp, name)})
				if err != nil {
					return err
				}
				r.eipID = aws.ToString(o.AllocationId)
				return nil
			}); err != nil {
				return "", fmt.Errorf("allocate the NAT gateway's Elastic IP: %w", err)
			}
		}
		id = plannedPrefix + "nat"
		if err := r.create("NAT gateway "+name, func() error {
			o, err := r.c.EC2.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{SubnetId: aws.String(r.public[0]),
				AllocationId: aws.String(r.eipID), ConnectivityType: ec2types.ConnectivityTypePublic,
				TagSpecifications: r.tagSpec(ec2types.ResourceTypeNatgateway, name)})
			if err != nil {
				return err
			}
			nat, id = *o.NatGateway, aws.ToString(o.NatGateway.NatGatewayId)
			return nil
		}); err != nil {
			return "", fmt.Errorf("create NAT gateway: %w", err)
		}
	}
	if r.plan() || nat.State == ec2types.NatGatewayStateAvailable {
		return id, nil
	}
	err := waitFor(ctx, "NAT gateway "+id, 10*time.Minute, func() (bool, error) {
		out, err := r.c.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{NatGatewayIds: []string{id}})
		if err != nil {
			return false, fmt.Errorf("describe NAT gateway %s: %w", id, err)
		}
		if len(out.NatGateways) != 1 {
			return false, fmt.Errorf("NAT gateway %s not found", id)
		}
		g := out.NatGateways[0]
		switch g.State {
		case ec2types.NatGatewayStateAvailable:
			return true, nil
		case ec2types.NatGatewayStatePending:
			return false, nil
		}
		return false, fmt.Errorf("NAT gateway %s is %s: %s", id, g.State, aws.ToString(g.FailureMessage))
	})
	return id, err
}

// ensureRouteTable makes the public (default route to the internet gateway)
// or private (default route to the NAT gateway) route table and associates
// it with its subnets.
func (r *clusterRun) ensureRouteTable(ctx context.Context, public bool, target string) error {
	name := r.netName(subnetKind(public))
	subnets := r.private
	if public {
		subnets = r.public
	}
	var rt ec2types.RouteTable
	if !planned(r.vpcID) {
		out, err := r.c.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: r.tagFilters(
			filter("vpc-id", r.vpcID), filter("tag:Name", name))})
		if err != nil {
			return fmt.Errorf("look up route table %s: %w", name, err)
		}
		if len(out.RouteTables) > 1 {
			return fmt.Errorf("%d route tables are named %s in %s; expected one", len(out.RouteTables), name, r.vpcID)
		}
		if len(out.RouteTables) == 1 {
			rt = out.RouteTables[0]
		}
	}
	id := aws.ToString(rt.RouteTableId)
	if id == "" {
		id = plannedPrefix + name
		if err := r.create("route table "+name, func() error {
			o, err := r.c.EC2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: aws.String(r.vpcID),
				TagSpecifications: r.tagSpec(ec2types.ResourceTypeRouteTable, name)})
			if err != nil {
				return err
			}
			id = aws.ToString(o.RouteTable.RouteTableId)
			return nil
		}); err != nil {
			return fmt.Errorf("create route table %s: %w", name, err)
		}
	}

	route := &ec2types.Route{}
	for i := range rt.Routes {
		if aws.ToString(rt.Routes[i].DestinationCidrBlock) == "0.0.0.0/0" {
			route = &rt.Routes[i]
		}
	}
	cur := aws.ToString(route.GatewayId)
	if !public {
		cur = aws.ToString(route.NatGatewayId)
	}
	switch {
	case cur == target && route.State != ec2types.RouteStateBlackhole:
	case aws.ToString(route.DestinationCidrBlock) == "":
		if err := r.change(fmt.Sprintf("add the default route of %s via %s", name, target), func() error {
			in := &ec2.CreateRouteInput{RouteTableId: aws.String(id), DestinationCidrBlock: aws.String("0.0.0.0/0")}
			if public {
				in.GatewayId = aws.String(target)
			} else {
				in.NatGatewayId = aws.String(target)
			}
			_, err := r.c.EC2.CreateRoute(ctx, in)
			return err
		}); err != nil {
			return fmt.Errorf("default route of %s: %w", name, err)
		}
	default:
		// A blackhole route (to a NAT gateway that is gone) or another target.
		if err := r.change(fmt.Sprintf("point the default route of %s at %s", name, target), func() error {
			in := &ec2.ReplaceRouteInput{RouteTableId: aws.String(id), DestinationCidrBlock: aws.String("0.0.0.0/0")}
			if public {
				in.GatewayId = aws.String(target)
			} else {
				in.NatGatewayId = aws.String(target)
			}
			_, err := r.c.EC2.ReplaceRoute(ctx, in)
			return err
		}); err != nil {
			return fmt.Errorf("default route of %s: %w", name, err)
		}
	}

	associated := map[string]bool{}
	for _, a := range rt.Associations {
		associated[aws.ToString(a.SubnetId)] = true
	}
	for _, s := range subnets {
		if associated[s] {
			continue
		}
		if err := r.change(fmt.Sprintf("associate %s with %s", name, s), func() error {
			_, err := r.c.EC2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{RouteTableId: aws.String(id), SubnetId: aws.String(s)})
			return err
		}); err != nil {
			return fmt.Errorf("associate %s with %s: %w", name, s, err)
		}
	}
	return nil
}
