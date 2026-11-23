package cmd

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The AWS API surfaces `create cluster` uses, beyond the shared Clients.
// Interfaces, so the tests run against an in-memory account (cluster_fake_test.go).
type (
	clusterEC2API interface {
		DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)
		DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
		DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
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
		DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
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
		// ListNodegroups finds every managed node group of a cluster that
		// a self-managed run would add groups to.
		ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
	}
	// clusterIAMAPI extends the shared IAMAPI, so ownedRole (iam.go) checks
	// the cluster's roles the same way it checks the Pod Identity roles.
	clusterIAMAPI interface {
		IAMAPI
		GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
		ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
		AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)

		GetInstanceProfile(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
		ListInstanceProfileTags(context.Context, *iam.ListInstanceProfileTagsInput, ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error)
		CreateInstanceProfile(context.Context, *iam.CreateInstanceProfileInput, ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
		AddRoleToInstanceProfile(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
	}
	clusterASGAPI interface {
		DescribeTags(context.Context, *autoscaling.DescribeTagsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeTagsOutput, error)
		CreateOrUpdateTags(context.Context, *autoscaling.CreateOrUpdateTagsInput, ...func(*autoscaling.Options)) (*autoscaling.CreateOrUpdateTagsOutput, error)

		DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
		CreateAutoScalingGroup(context.Context, *autoscaling.CreateAutoScalingGroupInput, ...func(*autoscaling.Options)) (*autoscaling.CreateAutoScalingGroupOutput, error)
		DescribeScalingActivities(context.Context, *autoscaling.DescribeScalingActivitiesInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error)
	}
	// clusterSSMAPI reads the recommended EKS optimized AMI of
	// self-managed node groups.
	clusterSSMAPI interface {
		GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	}
	// clusterSQSAPI and clusterEventsAPI make Karpenter's interruption
	// queue and the EventBridge rules that send to it (karpenter.go).
	clusterSQSAPI interface {
		queueAPI
		GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
		CreateQueue(context.Context, *sqs.CreateQueueInput, ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error)
		SetQueueAttributes(context.Context, *sqs.SetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.SetQueueAttributesOutput, error)
	}
	clusterEventsAPI interface {
		DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error)
		ListTagsForResource(context.Context, *eventbridge.ListTagsForResourceInput, ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error)
		PutRule(context.Context, *eventbridge.PutRuleInput, ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error)
		ListTargetsByRule(context.Context, *eventbridge.ListTargetsByRuleInput, ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error)
		PutTargets(context.Context, *eventbridge.PutTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error)
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
	SSM    clusterSSMAPI
	// Kube is the Kubernetes API of an ACTIVE cluster, which the wait for
	// self-managed nodes reads (kubeapi.go).
	Kube func(context.Context, *ekstypes.Cluster) (nodeLister, error)
	// SQS, Events and Objects make what KARPENTER=on needs: the queue, the
	// rules, and the Kubernetes objects of Karpenter and its NodePools.
	SQS     clusterSQSAPI
	Events  clusterEventsAPI
	Objects func(context.Context, *ekstypes.Cluster) (kubeObjects, error)
}

var newClusterClients = func(ctx context.Context, c Config) (*ClusterClients, error) {
	ac, err := loadAWSConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	return &ClusterClients{
		EC2:    ec2.NewFromConfig(ac),
		EKS:    eks.NewFromConfig(ac),
		IAM:    iam.NewFromConfig(ac),
		ASG:    autoscaling.NewFromConfig(ac),
		Quotas: servicequotas.NewFromConfig(ac),
		SSM:    ssm.NewFromConfig(ac),
		Kube: func(_ context.Context, c *ekstypes.Cluster) (nodeLister, error) {
			return newKubeClient(c, sts.NewPresignClient(sts.NewFromConfig(ac)))
		},
		SQS:    sqs.NewFromConfig(ac),
		Events: eventbridge.NewFromConfig(ac),
		Objects: func(_ context.Context, c *ekstypes.Cluster) (kubeObjects, error) {
			return newKubeClient(c, sts.NewPresignClient(sts.NewFromConfig(ac)))
		},
	}, nil
}
