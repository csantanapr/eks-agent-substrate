package cmd

// recordTD wraps each teardown client (tdClients) to record every call, in
// order, with its input, for teardown_calls_test.go. A method added to one
// of the client interfaces needs its wrapper here: the build fails without
// it.

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// callLog is every call the recorded clients made: "<client>.<method>
// <input as JSON>".
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (r *callLog) add(op string, in any) {
	b, _ := json.Marshal(in)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, op+" "+string(b))
}

// recordTD wraps c so that every call is added to r.
func recordTD(c *tdClients, r *callLog) *tdClients {
	out := &tdClients{
		STS:    recSTS{c.STS, r},
		Tag:    recTag{c.Tag, r},
		EC2:    recEC2{c.EC2, r},
		EKS:    recEKS{c.EKS, r},
		IAM:    recIAM{c.IAM, r},
		RDS:    recRDS{c.RDS, r},
		SM:     recSM{c.SM, r},
		S3:     recS3{c.S3, r},
		ECR:    recECR{c.ECR, r},
		ASG:    recASG{c.ASG, r},
		SQS:    recSQS{c.SQS, r},
		Events: recEvents{c.Events, r},
	}
	objects := c.Objects
	out.Objects = func(ctx context.Context, cl *ekstypes.Cluster) (kubeObjects, error) {
		r.add("Objects", nil)
		return objects(ctx, cl)
	}
	return out
}

type recSTS struct {
	c STSAPI
	r *callLog
}

func (x recSTS) GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, o ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	x.r.add("STS.GetCallerIdentity", in)
	return x.c.GetCallerIdentity(ctx, in, o...)
}

type recTag struct {
	c tdTagAPI
	r *callLog
}

func (x recTag) GetResources(ctx context.Context, in *rgt.GetResourcesInput, o ...func(*rgt.Options)) (*rgt.GetResourcesOutput, error) {
	x.r.add("Tag.GetResources", in)
	return x.c.GetResources(ctx, in, o...)
}

type recEC2 struct {
	c tdEC2API
	r *callLog
}

func (x recEC2) DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, o ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	x.r.add("EC2.DescribeVpcs", in)
	return x.c.DescribeVpcs(ctx, in, o...)
}

func (x recEC2) DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, o ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	x.r.add("EC2.DescribeSubnets", in)
	return x.c.DescribeSubnets(ctx, in, o...)
}

func (x recEC2) DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, o ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	x.r.add("EC2.DescribeSecurityGroups", in)
	return x.c.DescribeSecurityGroups(ctx, in, o...)
}

func (x recEC2) DescribeInternetGateways(ctx context.Context, in *ec2.DescribeInternetGatewaysInput, o ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	x.r.add("EC2.DescribeInternetGateways", in)
	return x.c.DescribeInternetGateways(ctx, in, o...)
}

func (x recEC2) DescribeNatGateways(ctx context.Context, in *ec2.DescribeNatGatewaysInput, o ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	x.r.add("EC2.DescribeNatGateways", in)
	return x.c.DescribeNatGateways(ctx, in, o...)
}

func (x recEC2) DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, o ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	x.r.add("EC2.DescribeAddresses", in)
	return x.c.DescribeAddresses(ctx, in, o...)
}

func (x recEC2) DescribeRouteTables(ctx context.Context, in *ec2.DescribeRouteTablesInput, o ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	x.r.add("EC2.DescribeRouteTables", in)
	return x.c.DescribeRouteTables(ctx, in, o...)
}

func (x recEC2) DescribeLaunchTemplates(ctx context.Context, in *ec2.DescribeLaunchTemplatesInput, o ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	x.r.add("EC2.DescribeLaunchTemplates", in)
	return x.c.DescribeLaunchTemplates(ctx, in, o...)
}

func (x recEC2) DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, o ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	x.r.add("EC2.DescribeNetworkInterfaces", in)
	return x.c.DescribeNetworkInterfaces(ctx, in, o...)
}

func (x recEC2) DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, o ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	x.r.add("EC2.DescribeVolumes", in)
	return x.c.DescribeVolumes(ctx, in, o...)
}

func (x recEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, o ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	x.r.add("EC2.DescribeInstances", in)
	return x.c.DescribeInstances(ctx, in, o...)
}

func (x recEC2) DescribeFleets(ctx context.Context, in *ec2.DescribeFleetsInput, o ...func(*ec2.Options)) (*ec2.DescribeFleetsOutput, error) {
	x.r.add("EC2.DescribeFleets", in)
	return x.c.DescribeFleets(ctx, in, o...)
}

func (x recEC2) DeleteVpc(ctx context.Context, in *ec2.DeleteVpcInput, o ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error) {
	x.r.add("EC2.DeleteVpc", in)
	return x.c.DeleteVpc(ctx, in, o...)
}

func (x recEC2) DeleteSubnet(ctx context.Context, in *ec2.DeleteSubnetInput, o ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error) {
	x.r.add("EC2.DeleteSubnet", in)
	return x.c.DeleteSubnet(ctx, in, o...)
}

func (x recEC2) DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, o ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	x.r.add("EC2.DeleteSecurityGroup", in)
	return x.c.DeleteSecurityGroup(ctx, in, o...)
}

func (x recEC2) DetachInternetGateway(ctx context.Context, in *ec2.DetachInternetGatewayInput, o ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error) {
	x.r.add("EC2.DetachInternetGateway", in)
	return x.c.DetachInternetGateway(ctx, in, o...)
}

func (x recEC2) DeleteInternetGateway(ctx context.Context, in *ec2.DeleteInternetGatewayInput, o ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error) {
	x.r.add("EC2.DeleteInternetGateway", in)
	return x.c.DeleteInternetGateway(ctx, in, o...)
}

func (x recEC2) DeleteNatGateway(ctx context.Context, in *ec2.DeleteNatGatewayInput, o ...func(*ec2.Options)) (*ec2.DeleteNatGatewayOutput, error) {
	x.r.add("EC2.DeleteNatGateway", in)
	return x.c.DeleteNatGateway(ctx, in, o...)
}

func (x recEC2) ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, o ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	x.r.add("EC2.ReleaseAddress", in)
	return x.c.ReleaseAddress(ctx, in, o...)
}

func (x recEC2) DeleteRouteTable(ctx context.Context, in *ec2.DeleteRouteTableInput, o ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error) {
	x.r.add("EC2.DeleteRouteTable", in)
	return x.c.DeleteRouteTable(ctx, in, o...)
}

func (x recEC2) DeleteLaunchTemplate(ctx context.Context, in *ec2.DeleteLaunchTemplateInput, o ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error) {
	x.r.add("EC2.DeleteLaunchTemplate", in)
	return x.c.DeleteLaunchTemplate(ctx, in, o...)
}

func (x recEC2) DeleteNetworkInterface(ctx context.Context, in *ec2.DeleteNetworkInterfaceInput, o ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error) {
	x.r.add("EC2.DeleteNetworkInterface", in)
	return x.c.DeleteNetworkInterface(ctx, in, o...)
}

func (x recEC2) DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, o ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	x.r.add("EC2.DeleteVolume", in)
	return x.c.DeleteVolume(ctx, in, o...)
}

func (x recEC2) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, o ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	x.r.add("EC2.TerminateInstances", in)
	return x.c.TerminateInstances(ctx, in, o...)
}

type recEKS struct {
	c tdEKSAPI
	r *callLog
}

func (x recEKS) DescribeCluster(ctx context.Context, in *eks.DescribeClusterInput, o ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	x.r.add("EKS.DescribeCluster", in)
	return x.c.DescribeCluster(ctx, in, o...)
}

func (x recEKS) ListNodegroups(ctx context.Context, in *eks.ListNodegroupsInput, o ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	x.r.add("EKS.ListNodegroups", in)
	return x.c.ListNodegroups(ctx, in, o...)
}

func (x recEKS) DescribeNodegroup(ctx context.Context, in *eks.DescribeNodegroupInput, o ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	x.r.add("EKS.DescribeNodegroup", in)
	return x.c.DescribeNodegroup(ctx, in, o...)
}

func (x recEKS) ListPodIdentityAssociations(ctx context.Context, in *eks.ListPodIdentityAssociationsInput, o ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	x.r.add("EKS.ListPodIdentityAssociations", in)
	return x.c.ListPodIdentityAssociations(ctx, in, o...)
}

func (x recEKS) DescribePodIdentityAssociation(ctx context.Context, in *eks.DescribePodIdentityAssociationInput, o ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	x.r.add("EKS.DescribePodIdentityAssociation", in)
	return x.c.DescribePodIdentityAssociation(ctx, in, o...)
}

func (x recEKS) DeletePodIdentityAssociation(ctx context.Context, in *eks.DeletePodIdentityAssociationInput, o ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error) {
	x.r.add("EKS.DeletePodIdentityAssociation", in)
	return x.c.DeletePodIdentityAssociation(ctx, in, o...)
}

func (x recEKS) DeleteNodegroup(ctx context.Context, in *eks.DeleteNodegroupInput, o ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error) {
	x.r.add("EKS.DeleteNodegroup", in)
	return x.c.DeleteNodegroup(ctx, in, o...)
}

func (x recEKS) DeleteCluster(ctx context.Context, in *eks.DeleteClusterInput, o ...func(*eks.Options)) (*eks.DeleteClusterOutput, error) {
	x.r.add("EKS.DeleteCluster", in)
	return x.c.DeleteCluster(ctx, in, o...)
}

func (x recEKS) DescribeAccessEntry(ctx context.Context, in *eks.DescribeAccessEntryInput, o ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error) {
	x.r.add("EKS.DescribeAccessEntry", in)
	return x.c.DescribeAccessEntry(ctx, in, o...)
}

func (x recEKS) DeleteAccessEntry(ctx context.Context, in *eks.DeleteAccessEntryInput, o ...func(*eks.Options)) (*eks.DeleteAccessEntryOutput, error) {
	x.r.add("EKS.DeleteAccessEntry", in)
	return x.c.DeleteAccessEntry(ctx, in, o...)
}

type recIAM struct {
	c tdIAMAPI
	r *callLog
}

func (x recIAM) GetRole(ctx context.Context, in *iam.GetRoleInput, o ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	x.r.add("IAM.GetRole", in)
	return x.c.GetRole(ctx, in, o...)
}

func (x recIAM) ListRoles(ctx context.Context, in *iam.ListRolesInput, o ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	x.r.add("IAM.ListRoles", in)
	return x.c.ListRoles(ctx, in, o...)
}

func (x recIAM) ListRolePolicies(ctx context.Context, in *iam.ListRolePoliciesInput, o ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	x.r.add("IAM.ListRolePolicies", in)
	return x.c.ListRolePolicies(ctx, in, o...)
}

func (x recIAM) DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, o ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	x.r.add("IAM.DeleteRolePolicy", in)
	return x.c.DeleteRolePolicy(ctx, in, o...)
}

func (x recIAM) ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, o ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	x.r.add("IAM.ListAttachedRolePolicies", in)
	return x.c.ListAttachedRolePolicies(ctx, in, o...)
}

func (x recIAM) DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, o ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	x.r.add("IAM.DetachRolePolicy", in)
	return x.c.DetachRolePolicy(ctx, in, o...)
}

func (x recIAM) ListInstanceProfilesForRole(ctx context.Context, in *iam.ListInstanceProfilesForRoleInput, o ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	x.r.add("IAM.ListInstanceProfilesForRole", in)
	return x.c.ListInstanceProfilesForRole(ctx, in, o...)
}

func (x recIAM) RemoveRoleFromInstanceProfile(ctx context.Context, in *iam.RemoveRoleFromInstanceProfileInput, o ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	x.r.add("IAM.RemoveRoleFromInstanceProfile", in)
	return x.c.RemoveRoleFromInstanceProfile(ctx, in, o...)
}

func (x recIAM) DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, o ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	x.r.add("IAM.DeleteRole", in)
	return x.c.DeleteRole(ctx, in, o...)
}

func (x recIAM) GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, o ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	x.r.add("IAM.GetInstanceProfile", in)
	return x.c.GetInstanceProfile(ctx, in, o...)
}

func (x recIAM) ListInstanceProfileTags(ctx context.Context, in *iam.ListInstanceProfileTagsInput, o ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error) {
	x.r.add("IAM.ListInstanceProfileTags", in)
	return x.c.ListInstanceProfileTags(ctx, in, o...)
}

func (x recIAM) DeleteInstanceProfile(ctx context.Context, in *iam.DeleteInstanceProfileInput, o ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	x.r.add("IAM.DeleteInstanceProfile", in)
	return x.c.DeleteInstanceProfile(ctx, in, o...)
}

type recRDS struct {
	c tdRDSAPI
	r *callLog
}

func (x recRDS) DescribeDBInstances(ctx context.Context, in *rds.DescribeDBInstancesInput, o ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	x.r.add("RDS.DescribeDBInstances", in)
	return x.c.DescribeDBInstances(ctx, in, o...)
}

func (x recRDS) DeleteDBInstance(ctx context.Context, in *rds.DeleteDBInstanceInput, o ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
	x.r.add("RDS.DeleteDBInstance", in)
	return x.c.DeleteDBInstance(ctx, in, o...)
}

func (x recRDS) DescribeDBSubnetGroups(ctx context.Context, in *rds.DescribeDBSubnetGroupsInput, o ...func(*rds.Options)) (*rds.DescribeDBSubnetGroupsOutput, error) {
	x.r.add("RDS.DescribeDBSubnetGroups", in)
	return x.c.DescribeDBSubnetGroups(ctx, in, o...)
}

func (x recRDS) DeleteDBSubnetGroup(ctx context.Context, in *rds.DeleteDBSubnetGroupInput, o ...func(*rds.Options)) (*rds.DeleteDBSubnetGroupOutput, error) {
	x.r.add("RDS.DeleteDBSubnetGroup", in)
	return x.c.DeleteDBSubnetGroup(ctx, in, o...)
}

func (x recRDS) ListTagsForResource(ctx context.Context, in *rds.ListTagsForResourceInput, o ...func(*rds.Options)) (*rds.ListTagsForResourceOutput, error) {
	x.r.add("RDS.ListTagsForResource", in)
	return x.c.ListTagsForResource(ctx, in, o...)
}

func (x recRDS) DescribeDBSnapshots(ctx context.Context, in *rds.DescribeDBSnapshotsInput, o ...func(*rds.Options)) (*rds.DescribeDBSnapshotsOutput, error) {
	x.r.add("RDS.DescribeDBSnapshots", in)
	return x.c.DescribeDBSnapshots(ctx, in, o...)
}

type recSM struct {
	c tdSecretsAPI
	r *callLog
}

func (x recSM) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	x.r.add("SM.DescribeSecret", in)
	return x.c.DescribeSecret(ctx, in, o...)
}

func (x recSM) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	x.r.add("SM.DeleteSecret", in)
	return x.c.DeleteSecret(ctx, in, o...)
}

type recS3 struct {
	c tdS3API
	r *callLog
}

func (x recS3) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, o ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	x.r.add("S3.HeadBucket", in)
	return x.c.HeadBucket(ctx, in, o...)
}

func (x recS3) GetBucketTagging(ctx context.Context, in *s3.GetBucketTaggingInput, o ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	x.r.add("S3.GetBucketTagging", in)
	return x.c.GetBucketTagging(ctx, in, o...)
}

func (x recS3) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, o ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	x.r.add("S3.ListObjectVersions", in)
	return x.c.ListObjectVersions(ctx, in, o...)
}

func (x recS3) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, o ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	x.r.add("S3.DeleteObjects", in)
	return x.c.DeleteObjects(ctx, in, o...)
}

func (x recS3) DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, o ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	x.r.add("S3.DeleteBucket", in)
	return x.c.DeleteBucket(ctx, in, o...)
}

type recECR struct {
	c tdECRAPI
	r *callLog
}

func (x recECR) DescribeRepositories(ctx context.Context, in *ecr.DescribeRepositoriesInput, o ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	x.r.add("ECR.DescribeRepositories", in)
	return x.c.DescribeRepositories(ctx, in, o...)
}

func (x recECR) ListTagsForResource(ctx context.Context, in *ecr.ListTagsForResourceInput, o ...func(*ecr.Options)) (*ecr.ListTagsForResourceOutput, error) {
	x.r.add("ECR.ListTagsForResource", in)
	return x.c.ListTagsForResource(ctx, in, o...)
}

func (x recECR) DeleteRepository(ctx context.Context, in *ecr.DeleteRepositoryInput, o ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error) {
	x.r.add("ECR.DeleteRepository", in)
	return x.c.DeleteRepository(ctx, in, o...)
}

type recASG struct {
	c tdASGAPI
	r *callLog
}

func (x recASG) DescribeAutoScalingGroups(ctx context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, o ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	x.r.add("ASG.DescribeAutoScalingGroups", in)
	return x.c.DescribeAutoScalingGroups(ctx, in, o...)
}

func (x recASG) DeleteAutoScalingGroup(ctx context.Context, in *autoscaling.DeleteAutoScalingGroupInput, o ...func(*autoscaling.Options)) (*autoscaling.DeleteAutoScalingGroupOutput, error) {
	x.r.add("ASG.DeleteAutoScalingGroup", in)
	return x.c.DeleteAutoScalingGroup(ctx, in, o...)
}

type recSQS struct {
	c tdSQSAPI
	r *callLog
}

func (x recSQS) GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput, o ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	x.r.add("SQS.GetQueueUrl", in)
	return x.c.GetQueueUrl(ctx, in, o...)
}

func (x recSQS) ListQueueTags(ctx context.Context, in *sqs.ListQueueTagsInput, o ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error) {
	x.r.add("SQS.ListQueueTags", in)
	return x.c.ListQueueTags(ctx, in, o...)
}

func (x recSQS) DeleteQueue(ctx context.Context, in *sqs.DeleteQueueInput, o ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	x.r.add("SQS.DeleteQueue", in)
	return x.c.DeleteQueue(ctx, in, o...)
}

type recEvents struct {
	c tdEventsAPI
	r *callLog
}

func (x recEvents) DescribeRule(ctx context.Context, in *eventbridge.DescribeRuleInput, o ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	x.r.add("Events.DescribeRule", in)
	return x.c.DescribeRule(ctx, in, o...)
}

func (x recEvents) ListTagsForResource(ctx context.Context, in *eventbridge.ListTagsForResourceInput, o ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error) {
	x.r.add("Events.ListTagsForResource", in)
	return x.c.ListTagsForResource(ctx, in, o...)
}

func (x recEvents) ListTargetsByRule(ctx context.Context, in *eventbridge.ListTargetsByRuleInput, o ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error) {
	x.r.add("Events.ListTargetsByRule", in)
	return x.c.ListTargetsByRule(ctx, in, o...)
}

func (x recEvents) RemoveTargets(ctx context.Context, in *eventbridge.RemoveTargetsInput, o ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error) {
	x.r.add("Events.RemoveTargets", in)
	return x.c.RemoveTargets(ctx, in, o...)
}

func (x recEvents) DeleteRule(ctx context.Context, in *eventbridge.DeleteRuleInput, o ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	x.r.add("Events.DeleteRule", in)
	return x.c.DeleteRule(ctx, in, o...)
}
