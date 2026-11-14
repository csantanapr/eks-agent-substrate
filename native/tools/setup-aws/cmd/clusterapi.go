package cmd

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
)

// The AWS API surfaces `create cluster` uses, beyond the shared Clients.
// Interfaces, so the tests run against an in-memory account (cluster_fake_test.go).
type (
	clusterEC2API interface {
		DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)
		DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
		DescribeInstanceTypeOfferings(context.Context, *ec2.DescribeInstanceTypeOfferingsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)

		DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
		CreateVpc(context.Context, *ec2.CreateVpcInput, ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error)
		DescribeVpcAttribute(context.Context, *ec2.DescribeVpcAttributeInput, ...func(*ec2.Options)) (*ec2.DescribeVpcAttributeOutput, error)
		ModifyVpcAttribute(context.Context, *ec2.ModifyVpcAttributeInput, ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error)
		DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
		CreateSubnet(context.Context, *ec2.CreateSubnetInput, ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error)
		DescribeInternetGateways(context.Context, *ec2.DescribeInternetGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error)
		CreateInternetGateway(context.Context, *ec2.CreateInternetGatewayInput, ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error)
		AttachInternetGateway(context.Context, *ec2.AttachInternetGatewayInput, ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error)
		DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
		AllocateAddress(context.Context, *ec2.AllocateAddressInput, ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
		DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
		CreateNatGateway(context.Context, *ec2.CreateNatGatewayInput, ...func(*ec2.Options)) (*ec2.CreateNatGatewayOutput, error)
		DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
		CreateRouteTable(context.Context, *ec2.CreateRouteTableInput, ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error)
		CreateRoute(context.Context, *ec2.CreateRouteInput, ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error)
		ReplaceRoute(context.Context, *ec2.ReplaceRouteInput, ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error)
		AssociateRouteTable(context.Context, *ec2.AssociateRouteTableInput, ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error)
		DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
		DescribeNetworkAcls(context.Context, *ec2.DescribeNetworkAclsInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkAclsOutput, error)
		CreateTags(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)

		DescribeLaunchTemplates(context.Context, *ec2.DescribeLaunchTemplatesInput, ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error)
		DescribeLaunchTemplateVersions(context.Context, *ec2.DescribeLaunchTemplateVersionsInput, ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
		CreateLaunchTemplate(context.Context, *ec2.CreateLaunchTemplateInput, ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateOutput, error)
		CreateLaunchTemplateVersion(context.Context, *ec2.CreateLaunchTemplateVersionInput, ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateVersionOutput, error)
	}
	clusterEKSAPI interface {
		DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
		CreateCluster(context.Context, *eks.CreateClusterInput, ...func(*eks.Options)) (*eks.CreateClusterOutput, error)
		UpdateClusterConfig(context.Context, *eks.UpdateClusterConfigInput, ...func(*eks.Options)) (*eks.UpdateClusterConfigOutput, error)
		DescribeUpdate(context.Context, *eks.DescribeUpdateInput, ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error)
		DescribeAccessEntry(context.Context, *eks.DescribeAccessEntryInput, ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error)
		CreateAccessEntry(context.Context, *eks.CreateAccessEntryInput, ...func(*eks.Options)) (*eks.CreateAccessEntryOutput, error)
		ListAssociatedAccessPolicies(context.Context, *eks.ListAssociatedAccessPoliciesInput, ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error)
		AssociateAccessPolicy(context.Context, *eks.AssociateAccessPolicyInput, ...func(*eks.Options)) (*eks.AssociateAccessPolicyOutput, error)
		DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
		CreateAddon(context.Context, *eks.CreateAddonInput, ...func(*eks.Options)) (*eks.CreateAddonOutput, error)
		UpdateAddon(context.Context, *eks.UpdateAddonInput, ...func(*eks.Options)) (*eks.UpdateAddonOutput, error)
		DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
		CreateNodegroup(context.Context, *eks.CreateNodegroupInput, ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error)
	}
	// clusterIAMAPI extends the shared IAMAPI, so ownedRole (iam.go) checks
	// the cluster's roles the same way it checks the Pod Identity roles.
	clusterIAMAPI interface {
		IAMAPI
		GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
		ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
		AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	}
	clusterASGAPI interface {
		DescribeTags(context.Context, *autoscaling.DescribeTagsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeTagsOutput, error)
		CreateOrUpdateTags(context.Context, *autoscaling.CreateOrUpdateTagsInput, ...func(*autoscaling.Options)) (*autoscaling.CreateOrUpdateTagsOutput, error)
	}
	quotasAPI interface {
		GetServiceQuota(context.Context, *servicequotas.GetServiceQuotaInput, ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error)
		GetAWSDefaultServiceQuota(context.Context, *servicequotas.GetAWSDefaultServiceQuotaInput, ...func(*servicequotas.Options)) (*servicequotas.GetAWSDefaultServiceQuotaOutput, error)
	}
)

// ClusterClients are the clients of `create cluster`. newClusterClients is
// replaced in tests.
type ClusterClients struct {
	EC2    clusterEC2API
	EKS    clusterEKSAPI
	IAM    clusterIAMAPI
	ASG    clusterASGAPI
	Quotas quotasAPI
}

var newClusterClients = func(ctx context.Context, c Config) (*ClusterClients, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &ClusterClients{
		EC2:    ec2.NewFromConfig(ac),
		EKS:    eks.NewFromConfig(ac),
		IAM:    iam.NewFromConfig(ac),
		ASG:    autoscaling.NewFromConfig(ac),
		Quotas: servicequotas.NewFromConfig(ac),
	}, nil
}
