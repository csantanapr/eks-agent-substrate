package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
)

// Tag keys inventory and teardown read besides OwnerTagKey.
const (
	// tdCreatedByKey marks a resource setup-aws created itself (the bash
	// scripts do not set it). Shown by inventory; ownership is OwnerTagKey.
	tdCreatedByKey = CreatedByTagKey
	// tdStackKey is set by CloudFormation on what a stack creates, here
	// eksctl's stacks from create-cluster.sh. teardown never deletes such a
	// resource: ./delete-cluster.sh does, through its stack.
	tdStackKey = "aws:cloudformation:stack-name"
	// eksClusterNameTag is on every volume EKS Auto Mode creates: its
	// managed policy allows CreateVolume only with it. pvcNamespaceTag is the
	// namespace of the claim the volume was made for; Auto Mode and the
	// aws-ebs-csi-driver add-on both set it.
	eksClusterNameTag = "eks:eks-cluster-name"
	pvcNamespaceTag   = "kubernetes.io/created-for/pvc/namespace"
	// substrateNS holds the bundled PostgreSQL's claim.
	substrateNS = "ate-system"
)

// The AWS API surfaces inventory and teardown use. Their own interfaces, so
// the tests run against an in-memory account.
type (
	tdTagAPI interface {
		GetResources(context.Context, *rgt.GetResourcesInput, ...func(*rgt.Options)) (*rgt.GetResourcesOutput, error)
	}
	tdEC2API interface {
		DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
		DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
		DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
		DescribeInternetGateways(context.Context, *ec2.DescribeInternetGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error)
		DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
		DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
		DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
		DescribeLaunchTemplates(context.Context, *ec2.DescribeLaunchTemplatesInput, ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error)
		DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
		DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
		DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
		DeleteVpc(context.Context, *ec2.DeleteVpcInput, ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error)
		DeleteSubnet(context.Context, *ec2.DeleteSubnetInput, ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error)
		DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
		DetachInternetGateway(context.Context, *ec2.DetachInternetGatewayInput, ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error)
		DeleteInternetGateway(context.Context, *ec2.DeleteInternetGatewayInput, ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error)
		DeleteNatGateway(context.Context, *ec2.DeleteNatGatewayInput, ...func(*ec2.Options)) (*ec2.DeleteNatGatewayOutput, error)
		ReleaseAddress(context.Context, *ec2.ReleaseAddressInput, ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
		DeleteRouteTable(context.Context, *ec2.DeleteRouteTableInput, ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error)
		DeleteLaunchTemplate(context.Context, *ec2.DeleteLaunchTemplateInput, ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error)
		DeleteNetworkInterface(context.Context, *ec2.DeleteNetworkInterfaceInput, ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error)
		DeleteVolume(context.Context, *ec2.DeleteVolumeInput, ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	}
	tdEKSAPI interface {
		DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
		ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
		DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
		ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error)
		DescribePodIdentityAssociation(context.Context, *eks.DescribePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error)
		DeletePodIdentityAssociation(context.Context, *eks.DeletePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error)
		DeleteNodegroup(context.Context, *eks.DeleteNodegroupInput, ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error)
		DeleteCluster(context.Context, *eks.DeleteClusterInput, ...func(*eks.Options)) (*eks.DeleteClusterOutput, error)
	}
	tdIAMAPI interface {
		GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
		ListRoles(context.Context, *iam.ListRolesInput, ...func(*iam.Options)) (*iam.ListRolesOutput, error)
		ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
		DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
		ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
		DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
		ListInstanceProfilesForRole(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
		RemoveRoleFromInstanceProfile(context.Context, *iam.RemoveRoleFromInstanceProfileInput, ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
		DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
	}
	tdRDSAPI interface {
		DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
		DeleteDBInstance(context.Context, *rds.DeleteDBInstanceInput, ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error)
		DescribeDBSubnetGroups(context.Context, *rds.DescribeDBSubnetGroupsInput, ...func(*rds.Options)) (*rds.DescribeDBSubnetGroupsOutput, error)
		DeleteDBSubnetGroup(context.Context, *rds.DeleteDBSubnetGroupInput, ...func(*rds.Options)) (*rds.DeleteDBSubnetGroupOutput, error)
		ListTagsForResource(context.Context, *rds.ListTagsForResourceInput, ...func(*rds.Options)) (*rds.ListTagsForResourceOutput, error)
		DescribeDBSnapshots(context.Context, *rds.DescribeDBSnapshotsInput, ...func(*rds.Options)) (*rds.DescribeDBSnapshotsOutput, error)
	}
	tdSecretsAPI interface {
		DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
		DeleteSecret(context.Context, *secretsmanager.DeleteSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
	}
	tdS3API interface {
		HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
		GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
		ListObjectVersions(context.Context, *s3.ListObjectVersionsInput, ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
		DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
		DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
	}
	tdECRAPI interface {
		DescribeRepositories(context.Context, *ecr.DescribeRepositoriesInput, ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
		ListTagsForResource(context.Context, *ecr.ListTagsForResourceInput, ...func(*ecr.Options)) (*ecr.ListTagsForResourceOutput, error)
		DeleteRepository(context.Context, *ecr.DeleteRepositoryInput, ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error)
	}
)

// tdClients bundles the clients of inventory and teardown. newTDClients is
// replaced in tests.
type tdClients struct {
	STS STSAPI
	Tag tdTagAPI
	EC2 tdEC2API
	EKS tdEKSAPI
	IAM tdIAMAPI
	RDS tdRDSAPI
	SM  tdSecretsAPI
	S3  tdS3API
	ECR tdECRAPI
}

var newTDClients = func(ctx context.Context, c Config) (*tdClients, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &tdClients{
		STS: sts.NewFromConfig(ac), Tag: rgt.NewFromConfig(ac), EC2: ec2.NewFromConfig(ac),
		EKS: eks.NewFromConfig(ac), IAM: iam.NewFromConfig(ac), RDS: rds.NewFromConfig(ac),
		SM: secretsmanager.NewFromConfig(ac), S3: s3.NewFromConfig(ac), ECR: ecr.NewFromConfig(ac),
	}, nil
}

// tdEnv is the run context of inventory and teardown.
type tdEnv struct {
	c                 *tdClients
	n                 Names
	plan, keepCluster bool
	out               io.Writer
	// refRoles are IAM roles the cluster, its node groups and its Pod
	// Identity associations point at, read before any of them is deleted.
	// teardown deletes the owned ones after the cluster, whatever their name.
	refRoles map[string]bool
}

func newTDEnv(ctx context.Context, out io.Writer) (*tdEnv, error) {
	if cfg.ClusterName == "" {
		return nil, errors.New("--cluster-name (CLUSTER_NAME) is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("--region (AWS_REGION) is required")
	}
	c, err := newTDClients(ctx, cfg)
	if err != nil {
		return nil, err
	}
	id, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("AWS credentials: %w", err)
	}
	parts := strings.Split(aws.ToString(id.Arn), ":")
	if len(parts) < 5 {
		return nil, errors.New("unexpected caller ARN")
	}
	return &tdEnv{c: c, n: deriveNames(parts[1], aws.ToString(id.Account), cfg.Region, cfg.ClusterName),
		plan: cfg.Plan, out: out, refRoles: map[string]bool{}}, nil
}

// mask replaces the account id with its last four digits (AC10.4): bucket
// names and ARNs carry it.
func (e *tdEnv) mask(s string) string {
	a := e.n.Account
	if len(a) <= 4 {
		return s
	}
	return strings.ReplaceAll(s, a, strings.Repeat("*", len(a)-4)+a[len(a)-4:])
}

// Derived names, matching lib/common.sh and the create commands.
func (e *tdEnv) dbID() string { return e.n.Cluster + "-pg" }

// ltNames are the node groups' launch templates create cluster makes (the
// microvm one is lib/common.sh's LT_NAME).
func (e *tdEnv) ltNames() []string {
	c := e.n.Cluster
	return []string{c + "-microvm-nested-virt", c + "-system-nodes", c + "-gvisor-nodes"}
}
func (e *tdEnv) secretNames() []string {
	return []string{e.n.Cluster + "/ate-owner", e.n.Cluster + "/ate-readwrite"}
}

// substrateRoles are the Pod Identity roles bootstrap makes, from the same
// list createIAM uses.
func (e *tdEnv) substrateRoles() []string {
	var out []string
	for _, p := range (&env{names: e.n}).podIdentities() {
		out = append(out, e.n.IAMPrefix+"-"+p.ServiceAccount)
	}
	return out
}

func (e *tdEnv) isSubstrateRole(name string) bool {
	for _, r := range e.substrateRoles() {
		if r == name {
			return true
		}
	}
	return false
}

func (e *tdEnv) owned(tags map[string]string) bool { return tags[OwnerTagKey] == e.n.OwnerValue }

// refuse is why kind/id may not be deleted, or "" when it may (AC2.2): it
// must carry this deployment's owner tag and not belong to a CloudFormation
// stack.
func (e *tdEnv) refuse(kind, id string, tags map[string]string) string {
	if s := tags[tdStackKey]; s != "" {
		return fmt.Sprintf("%s %s belongs to CloudFormation stack %s (eksctl, from create-cluster.sh): delete it with ./delete-cluster.sh, or run teardown --keep-cluster", kind, id, s)
	}
	if got := tags[OwnerTagKey]; got != e.n.OwnerValue {
		return fmt.Sprintf("%s %s is not owned by %s (%s=%q); teardown leaves it alone", kind, id, e.n.OwnerValue, OwnerTagKey, got)
	}
	return ""
}

// clusterMade reports tags EKS and the VPC CNI put on what they create for
// this cluster (its security group, node network interfaces).
func (e *tdEnv) clusterMade(tags map[string]string) bool {
	c := e.n.Cluster
	return tags["aws:eks:cluster-name"] == c || tags["eks:eks-cluster-name"] == c ||
		tags["cluster.k8s.amazonaws.com/name"] == c || tags["kubernetes.io/cluster/"+c] == "owned"
}

// clusterPVC reports a volume EKS Auto Mode made for a claim in ate-system of
// this cluster that carries no owner tag: on a cluster whose role does not
// let Auto Mode add custom tags, the bundled PostgreSQL's volume. A volume of
// another namespace, another owner or a CloudFormation stack is not one.
func (e *tdEnv) clusterPVC(tags map[string]string) bool {
	return tags[eksClusterNameTag] == e.n.Cluster && tags[pvcNamespaceTag] == substrateNS &&
		tags[OwnerTagKey] == "" && tags[tdStackKey] == ""
}

// volumes are this deployment's volumes matching f: the owner-tagged ones,
// then the clusterPVC ones.
func (e *tdEnv) volumes(ctx context.Context, f ...ec2types.Filter) ([]ec2Item, error) {
	owned, err := e.ec2List(ctx, kVolume, append([]ec2types.Filter{e.ownerFilter()}, f...)...)
	if err != nil {
		return nil, err
	}
	made, err := e.ec2List(ctx, kVolume, append([]ec2types.Filter{
		flt("tag:"+eksClusterNameTag, e.n.Cluster), flt("tag:"+pvcNamespaceTag, substrateNS)}, f...)...)
	if err != nil {
		return nil, err
	}
	for _, v := range made {
		if e.clusterPVC(v.tags) {
			owned = append(owned, v)
		}
	}
	return owned, nil
}

// tdGone reports an AWS "does not exist" error, across the services
// teardown calls. A denied or throttled call is not gone (AC2.3).
func tdGone(err error) bool {
	if err == nil {
		return false
	}
	if isNotFound(err) {
		return true
	}
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch c := ae.ErrorCode(); {
	case strings.HasSuffix(c, ".NotFound"), strings.HasSuffix(c, ".NotFoundException"):
		return true
	case c == "DBInstanceNotFound", c == "DBInstanceNotFoundFault", c == "DBSubnetGroupNotFoundFault", c == "NatGatewayNotFound":
		return true
	}
	return false
}

func hasCode(err error, codes ...string) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.ErrorCode() == c {
			return true
		}
	}
	return false
}

// Tag lists to maps, one adapter per service's Tag type.
func kvMap[T any](ts []T, kv func(T) (*string, *string)) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		k, v := kv(t)
		m[aws.ToString(k)] = aws.ToString(v)
	}
	return m
}

func ec2KV(t ec2types.Tag) (*string, *string) { return t.Key, t.Value }
func rdsKV(t rdstypes.Tag) (*string, *string) { return t.Key, t.Value }
func smKV(t smtypes.Tag) (*string, *string)   { return t.Key, t.Value }
func ecrKV(t ecrtypes.Tag) (*string, *string) { return t.Key, t.Value }
func s3KV(t s3types.Tag) (*string, *string)   { return t.Key, t.Value }
func iamKV(t iamtypes.Tag) (*string, *string) { return t.Key, t.Value }
func rgtKV(t rgttypes.Tag) (*string, *string) { return t.Key, t.Value }
func flt(name string, v ...string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String(name), Values: v}
}

// ---- reads ---------------------------------------------------------------

// cluster is the EKS cluster, or nil when it does not exist.
func (e *tdEnv) cluster(ctx context.Context) (*ekstypes.Cluster, error) {
	out, err := e.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(e.n.Cluster)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe cluster %s: %w", e.n.Cluster, err)
	}
	return out.Cluster, nil
}

func (e *tdEnv) nodegroups(ctx context.Context) ([]*ekstypes.Nodegroup, error) {
	var out []*ekstypes.Nodegroup
	p := eks.NewListNodegroupsPaginator(e.c.EKS, &eks.ListNodegroupsInput{ClusterName: aws.String(e.n.Cluster)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if tdGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list node groups of %s: %w", e.n.Cluster, err)
		}
		for _, name := range page.Nodegroups {
			d, err := e.c.EKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(e.n.Cluster), NodegroupName: aws.String(name)})
			if tdGone(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("describe node group %s: %w", name, err)
			}
			out = append(out, d.Nodegroup)
		}
	}
	return out, nil
}

func (e *tdEnv) associations(ctx context.Context) ([]*ekstypes.PodIdentityAssociation, error) {
	var out []*ekstypes.PodIdentityAssociation
	p := eks.NewListPodIdentityAssociationsPaginator(e.c.EKS, &eks.ListPodIdentityAssociationsInput{ClusterName: aws.String(e.n.Cluster)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if tdGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list Pod Identity associations of %s: %w", e.n.Cluster, err)
		}
		for _, s := range page.Associations {
			d, err := e.c.EKS.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{ClusterName: aws.String(e.n.Cluster), AssociationId: s.AssociationId})
			if tdGone(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("describe Pod Identity association %s: %w", aws.ToString(s.AssociationId), err)
			}
			out = append(out, d.Association)
		}
	}
	return out, nil
}

func assocLabel(a *ekstypes.PodIdentityAssociation) string {
	return aws.ToString(a.Namespace) + "/" + aws.ToString(a.ServiceAccount) + " (" + aws.ToString(a.AssociationId) + ")"
}

func roleName(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// noteRefs records the roles the cluster, node groups and associations use.
func (e *tdEnv) noteRefs(cl *ekstypes.Cluster, ngs []*ekstypes.Nodegroup, as []*ekstypes.PodIdentityAssociation) {
	if cl != nil && aws.ToString(cl.RoleArn) != "" {
		e.refRoles[roleName(aws.ToString(cl.RoleArn))] = true
	}
	if cl != nil && cl.ComputeConfig != nil && aws.ToString(cl.ComputeConfig.NodeRoleArn) != "" {
		e.refRoles[roleName(aws.ToString(cl.ComputeConfig.NodeRoleArn))] = true // Auto Mode's node role
	}
	for _, ng := range ngs {
		if aws.ToString(ng.NodeRole) != "" {
			e.refRoles[roleName(aws.ToString(ng.NodeRole))] = true
		}
	}
	for _, a := range as {
		if aws.ToString(a.RoleArn) != "" {
			e.refRoles[roleName(aws.ToString(a.RoleArn))] = true
		}
	}
}

// roles reads the candidate IAM roles: the Substrate roles, the roles the
// cluster pointed at, and every role named "<cluster>-...". IAM is not in
// the tagging API, so the owner tag is read role by role. Name -> tags.
func (e *tdEnv) roles(ctx context.Context) (map[string]map[string]string, error) {
	names := map[string]bool{}
	for r := range e.refRoles {
		names[r] = true
	}
	for _, r := range e.substrateRoles() {
		names[r] = true
	}
	p := iam.NewListRolesPaginator(e.c.IAM, &iam.ListRolesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list IAM roles: %w", err)
		}
		for _, r := range page.Roles {
			if strings.HasPrefix(aws.ToString(r.RoleName), e.n.Cluster+"-") {
				names[aws.ToString(r.RoleName)] = true
			}
		}
	}
	out := map[string]map[string]string{}
	for _, n := range sortedSet(names) {
		g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(n)})
		if tdGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get role %s: %w", n, err)
		}
		out[n] = kvMap(g.Role.Tags, iamKV)
	}
	return out, nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ec2Item is one EC2 resource, the fields teardown needs from any kind.
type ec2Item struct {
	id, vpc, name, state, alloc string
	main, isDefault             bool
	tags                        map[string]string
}

// EC2 kinds, also the TYPE column of the inventory.
const (
	kVPC      = "VPC"
	kSubnet   = "subnet"
	kSG       = "security group"
	kIGW      = "internet gateway"
	kNAT      = "NAT gateway"
	kEIP      = "Elastic IP"
	kRTB      = "route table"
	kLT       = "launch template"
	kENI      = "network interface"
	kVolume   = "EBS volume"
	kInstance = "EC2 instance"
)

// ec2List describes one kind of EC2 resource matching filters. Network
// interfaces, volumes and instances include EKS Auto Mode's managed ones,
// which EC2 hides otherwise.
func (e *tdEnv) ec2List(ctx context.Context, kind string, f ...ec2types.Filter) ([]ec2Item, error) {
	var out []ec2Item
	var err error
	managed := aws.Bool(true)
	switch kind {
	case kVPC:
		p := ec2.NewDescribeVpcsPaginator(e.c.EC2, &ec2.DescribeVpcsInput{Filters: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeVpcsOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.Vpcs {
					out = append(out, ec2Item{id: aws.ToString(v.VpcId), isDefault: aws.ToBool(v.IsDefault), tags: kvMap(v.Tags, ec2KV)})
				}
			}
		}
	case kSubnet:
		p := ec2.NewDescribeSubnetsPaginator(e.c.EC2, &ec2.DescribeSubnetsInput{Filters: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeSubnetsOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.Subnets {
					out = append(out, ec2Item{id: aws.ToString(v.SubnetId), vpc: aws.ToString(v.VpcId), tags: kvMap(v.Tags, ec2KV)})
				}
			}
		}
	case kSG:
		p := ec2.NewDescribeSecurityGroupsPaginator(e.c.EC2, &ec2.DescribeSecurityGroupsInput{Filters: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeSecurityGroupsOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.SecurityGroups {
					out = append(out, ec2Item{id: aws.ToString(v.GroupId), vpc: aws.ToString(v.VpcId), name: aws.ToString(v.GroupName),
						isDefault: aws.ToString(v.GroupName) == "default", tags: kvMap(v.Tags, ec2KV)})
				}
			}
		}
	case kIGW:
		p := ec2.NewDescribeInternetGatewaysPaginator(e.c.EC2, &ec2.DescribeInternetGatewaysInput{Filters: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeInternetGatewaysOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.InternetGateways {
					it := ec2Item{id: aws.ToString(v.InternetGatewayId), tags: kvMap(v.Tags, ec2KV)}
					if len(v.Attachments) > 0 {
						it.vpc = aws.ToString(v.Attachments[0].VpcId)
					}
					out = append(out, it)
				}
			}
		}
	case kNAT:
		p := ec2.NewDescribeNatGatewaysPaginator(e.c.EC2, &ec2.DescribeNatGatewaysInput{Filter: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeNatGatewaysOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.NatGateways {
					it := ec2Item{id: aws.ToString(v.NatGatewayId), vpc: aws.ToString(v.VpcId), state: string(v.State), tags: kvMap(v.Tags, ec2KV)}
					if len(v.NatGatewayAddresses) > 0 {
						it.alloc = aws.ToString(v.NatGatewayAddresses[0].AllocationId)
					}
					out = append(out, it)
				}
			}
		}
	case kEIP:
		var pg *ec2.DescribeAddressesOutput
		if pg, err = e.c.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: f}); err == nil {
			for _, v := range pg.Addresses {
				out = append(out, ec2Item{id: aws.ToString(v.AllocationId), state: aws.ToString(v.AssociationId), tags: kvMap(v.Tags, ec2KV)})
			}
		}
	case kRTB:
		p := ec2.NewDescribeRouteTablesPaginator(e.c.EC2, &ec2.DescribeRouteTablesInput{Filters: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeRouteTablesOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.RouteTables {
					it := ec2Item{id: aws.ToString(v.RouteTableId), vpc: aws.ToString(v.VpcId), tags: kvMap(v.Tags, ec2KV)}
					for _, a := range v.Associations {
						it.main = it.main || aws.ToBool(a.Main)
					}
					out = append(out, it)
				}
			}
		}
	case kLT:
		p := ec2.NewDescribeLaunchTemplatesPaginator(e.c.EC2, &ec2.DescribeLaunchTemplatesInput{Filters: f})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeLaunchTemplatesOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.LaunchTemplates {
					out = append(out, ec2Item{id: aws.ToString(v.LaunchTemplateId), name: aws.ToString(v.LaunchTemplateName), tags: kvMap(v.Tags, ec2KV)})
				}
			}
		}
	case kENI:
		p := ec2.NewDescribeNetworkInterfacesPaginator(e.c.EC2, &ec2.DescribeNetworkInterfacesInput{Filters: f, IncludeManagedResources: managed})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeNetworkInterfacesOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.NetworkInterfaces {
					out = append(out, ec2Item{id: aws.ToString(v.NetworkInterfaceId), vpc: aws.ToString(v.VpcId), state: string(v.Status), tags: kvMap(v.TagSet, ec2KV)})
				}
			}
		}
	case kVolume:
		p := ec2.NewDescribeVolumesPaginator(e.c.EC2, &ec2.DescribeVolumesInput{Filters: f, IncludeManagedResources: managed})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeVolumesOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, v := range pg.Volumes {
					out = append(out, ec2Item{id: aws.ToString(v.VolumeId), state: string(v.State), tags: kvMap(v.Tags, ec2KV)})
				}
			}
		}
	case kInstance:
		p := ec2.NewDescribeInstancesPaginator(e.c.EC2, &ec2.DescribeInstancesInput{Filters: f, IncludeManagedResources: managed})
		for p.HasMorePages() && err == nil {
			var pg *ec2.DescribeInstancesOutput
			if pg, err = p.NextPage(ctx); err == nil {
				for _, r := range pg.Reservations {
					for _, v := range r.Instances {
						it := ec2Item{id: aws.ToString(v.InstanceId), vpc: aws.ToString(v.VpcId), tags: kvMap(v.Tags, ec2KV)}
						if v.State != nil {
							it.state = string(v.State.Name)
						}
						out = append(out, it)
					}
				}
			}
		}
	default:
		return nil, fmt.Errorf("internal: unknown EC2 kind %q", kind)
	}
	if err != nil {
		return nil, fmt.Errorf("describe %ss: %w", kind, err)
	}
	return out, nil
}

func (e *tdEnv) ownerFilter() ec2types.Filter { return flt("tag:"+OwnerTagKey, e.n.OwnerValue) }

// ownedVPCs are the VPCs with this deployment's owner tag. A default VPC is
// never one of them.
func (e *tdEnv) ownedVPCs(ctx context.Context) ([]ec2Item, error) {
	vs, err := e.ec2List(ctx, kVPC, e.ownerFilter())
	if err != nil {
		return nil, err
	}
	out := vs[:0]
	for _, v := range vs {
		if !v.isDefault {
			out = append(out, v)
		}
	}
	return out, nil
}

func (e *tdEnv) bucketTags(ctx context.Context, b string) (exists bool, tags map[string]string, err error) {
	owner := aws.String(e.n.Account)
	if _, err := e.c.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b), ExpectedBucketOwner: owner}); err != nil {
		if tdGone(err) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("head bucket %s: %w", b, err)
	}
	out, err := e.c.S3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(b), ExpectedBucketOwner: owner})
	switch {
	case hasCode(err, "NoSuchTagSet"):
		return true, map[string]string{}, nil
	case tdGone(err):
		return false, nil, nil
	case err != nil:
		return false, nil, fmt.Errorf("read tags of bucket %s: %w", b, err)
	}
	return true, kvMap(out.TagSet, s3KV), nil
}

// repositories are this deployment's ECR repositories (name prefix
// "<cluster>/"), name -> tags.
func (e *tdEnv) repositories(ctx context.Context) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	p := ecr.NewDescribeRepositoriesPaginator(e.c.ECR, &ecr.DescribeRepositoriesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list ECR repositories: %w", err)
		}
		for _, r := range page.Repositories {
			name := aws.ToString(r.RepositoryName)
			if !strings.HasPrefix(name, e.n.ECRPrefix+"/") {
				continue
			}
			t, err := e.c.ECR.ListTagsForResource(ctx, &ecr.ListTagsForResourceInput{ResourceArn: r.RepositoryArn})
			if tdGone(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read tags of repository %s: %w", name, err)
			}
			out[name] = kvMap(t.Tags, ecrKV)
		}
	}
	return out, nil
}

func (e *tdEnv) dbInstance(ctx context.Context) (*rdstypes.DBInstance, error) {
	out, err := e.c.RDS.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(e.dbID())})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe RDS instance %s: %w", e.dbID(), err)
	}
	if len(out.DBInstances) == 0 {
		return nil, nil
	}
	return &out.DBInstances[0], nil
}

func (e *tdEnv) dbSubnetGroup(ctx context.Context) (exists bool, tags map[string]string, err error) {
	out, err := e.c.RDS.DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: aws.String(e.dbID())})
	if tdGone(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("describe RDS subnet group %s: %w", e.dbID(), err)
	}
	if len(out.DBSubnetGroups) == 0 {
		return false, nil, nil
	}
	t, err := e.c.RDS.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: out.DBSubnetGroups[0].DBSubnetGroupArn})
	if err != nil {
		return false, nil, fmt.Errorf("read tags of RDS subnet group %s: %w", e.dbID(), err)
	}
	return true, kvMap(t.TagList, rdsKV), nil
}

func (e *tdEnv) secret(ctx context.Context, name string) (*secretsmanager.DescribeSecretOutput, error) {
	out, err := e.c.SM.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe secret %s: %w", name, err)
	}
	return out, nil
}

// dbSnapshotExists asks RDS whether snapshot id exists and is not being
// deleted. RDS names an automated snapshot "rds:<instance>-<date>" and finds
// it only when asked for SnapshotType automated, so the type follows the
// name. Gone is false; any other error is returned (AC2.3).
func (e *tdEnv) dbSnapshotExists(ctx context.Context, id string) (bool, error) {
	in := &rds.DescribeDBSnapshotsInput{DBSnapshotIdentifier: aws.String(id)}
	if strings.HasPrefix(id, "rds:") {
		in.SnapshotType = aws.String("automated")
	}
	out, err := e.c.RDS.DescribeDBSnapshots(ctx, in)
	if tdGone(err) || isDBSnapshotNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("describe RDS snapshot %s: %w", id, err)
	}
	for _, s := range out.DBSnapshots {
		if aws.ToString(s.Status) != "deleting" {
			return true, nil
		}
	}
	return false, nil
}

func isDBSnapshotNotFound(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && (ae.ErrorCode() == "DBSnapshotNotFound" || ae.ErrorCode() == "DBSnapshotNotFoundFault")
}

// ---- inventory -----------------------------------------------------------

// tdRow is one inventory line. Cluster marks what --keep-cluster keeps.
type tdRow struct {
	Kind, ID string
	Tags     map[string]string
	Cluster  bool
}

// tdKnown are the tagging-API resource types inventory reads per service,
// where the service's own answer is authoritative: the tagging API can still
// list a resource for a while after it is deleted, so its entries for these
// types are not used. Every other type it returns (an EFS file system from a
// plugin script, say) is listed as it comes.
var tdKnown = map[string]bool{
	"ec2:vpc": true, "ec2:subnet": true, "ec2:security-group": true, "ec2:security-group-rule": true,
	"ec2:internet-gateway": true, "ec2:natgateway": true, "ec2:elastic-ip": true, "ec2:route-table": true,
	"ec2:launch-template": true, "ec2:network-interface": true, "ec2:volume": true, "ec2:instance": true,
	// create cluster tags the VPC's default network ACL, which goes with the
	// VPC (listed itself).
	"ec2:network-acl": true,
	"rds:db":          true, "rds:subgrp": true, "ecr:repository": true,
}

// arnType splits an ARN into its service, resource type and id.
func arnType(arn string) (service, rtype, id string) {
	p := strings.SplitN(arn, ":", 6)
	if len(p) < 6 {
		return "", "", arn
	}
	res := p[5]
	if i := strings.IndexAny(res, "/:"); i >= 0 {
		return p[2], res[:i], res[i+1:]
	}
	return p[2], res, ""
}

// inventory lists every resource of this deployment: what carries its owner
// tag, and what has one of its derived names whatever its tags. It also
// records the roles the cluster points at (refRoles). Read-only.
func (e *tdEnv) inventory(ctx context.Context) ([]tdRow, error) {
	var rows []tdRow
	seen := map[string]bool{}
	add := func(r tdRow) {
		if k := r.Kind + " " + r.ID; !seen[k] {
			seen[k] = true
			rows = append(rows, r)
		}
	}

	cl, err := e.cluster(ctx)
	if err != nil {
		return nil, err
	}
	if cl != nil {
		add(tdRow{"EKS cluster", e.n.Cluster, cl.Tags, true})
		ngs, err := e.nodegroups(ctx)
		if err != nil {
			return nil, err
		}
		as, err := e.associations(ctx)
		if err != nil {
			return nil, err
		}
		e.noteRefs(cl, ngs, as)
		for _, ng := range ngs {
			add(tdRow{"EKS node group", aws.ToString(ng.NodegroupName), ng.Tags, true})
		}
		for _, a := range as {
			if e.owned(a.Tags) || aws.ToString(a.Namespace) == "ate-system" {
				add(tdRow{"Pod Identity association", assocLabel(a), a.Tags, aws.ToString(a.Namespace) != "ate-system"})
			}
		}
	}

	roles, err := e.roles(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range tdKeys(roles) {
		if e.owned(roles[name]) || e.isSubstrateRole(name) {
			add(tdRow{"IAM role", name, roles[name], !e.isSubstrateRole(name)})
		}
	}

	db, err := e.dbInstance(ctx)
	if err != nil {
		return nil, err
	}
	if db != nil {
		add(tdRow{"RDS instance", e.dbID(), kvMap(db.TagList, rdsKV), false})
	}
	if ok, tags, err := e.dbSubnetGroup(ctx); err != nil {
		return nil, err
	} else if ok {
		add(tdRow{"RDS subnet group", e.dbID(), tags, false})
	}
	dbSGs, err := e.ec2List(ctx, kSG, flt("group-name", e.dbID()))
	if err != nil {
		return nil, err
	}
	for _, sg := range dbSGs {
		add(tdRow{kSG, sg.name + " (" + sg.id + ")", sg.tags, false})
	}
	for _, s := range e.secretNames() {
		out, err := e.secret(ctx, s)
		if err != nil {
			return nil, err
		}
		if out != nil {
			add(tdRow{"secret", s, kvMap(out.Tags, smKV), false})
		}
	}
	for _, b := range []string{e.n.SnapBucket, e.n.AssetBucket} {
		ok, tags, err := e.bucketTags(ctx, b)
		if err != nil {
			return nil, err
		}
		if ok {
			add(tdRow{"S3 bucket", b, tags, false})
		}
	}
	repos, err := e.repositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range tdKeys(repos) {
		add(tdRow{"ECR repository", r, repos[r], false})
	}

	// EC2: the derived launch template names, then everything with the owner tag.
	lts, err := e.ec2List(ctx, kLT, flt("launch-template-name", e.ltNames()...))
	if err != nil {
		return nil, err
	}
	for _, lt := range lts {
		add(tdRow{kLT, lt.name + " (" + lt.id + ")", lt.tags, true})
	}
	for _, kind := range []string{kVPC, kSubnet, kSG, kIGW, kNAT, kEIP, kRTB, kLT, kENI, kInstance} {
		items, err := e.ec2List(ctx, kind, e.ownerFilter())
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if (kind == kNAT && (it.state == "deleted" || it.state == "failed")) || (kind == kInstance && it.state == "terminated") {
				continue
			}
			id := it.id
			if it.name != "" {
				id = it.name + " (" + it.id + ")"
			}
			// The RDS security group is Substrate data; the rest is the cluster's.
			add(tdRow{kind, id, it.tags, !(kind == kSG && it.name == e.dbID())})
		}
	}
	// Volumes: the owner-tagged ones, and the ate-system claims' volumes
	// Auto Mode made for this cluster without the tag, as teardown finds them.
	vols, err := e.volumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		add(tdRow{kVolume, v.id, v.tags, true})
	}

	// Every other type that carries the owner tag (a plugin's EFS file
	// system, a CloudFormation stack).
	p := rgt.NewGetResourcesPaginator(e.c.Tag, &rgt.GetResourcesInput{
		TagFilters: []rgttypes.TagFilter{{Key: aws.String(OwnerTagKey), Values: []string{e.n.OwnerValue}}}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list resources tagged %s=%s: %w", OwnerTagKey, e.n.OwnerValue, err)
		}
		for _, m := range page.ResourceTagMappingList {
			svc, rtype, id := arnType(aws.ToString(m.ResourceARN))
			if svc == "eks" || svc == "iam" || svc == "s3" || tdKnown[svc+":"+rtype] {
				continue
			}
			// Secrets beyond the two logins (the master user secret RDS
			// makes carries the instance's tags, copied by RDS): read each,
			// and skip one gone or scheduled for deletion.
			if svc == "secretsmanager" && rtype == "secret" {
				s, err := e.secret(ctx, aws.ToString(m.ResourceARN))
				if err != nil {
					return nil, err
				}
				if s != nil && s.DeletedDate == nil {
					add(tdRow{"secret", aws.ToString(s.Name), kvMap(s.Tags, smKV), false})
				}
				continue
			}
			// RDS snapshots: the tagging API keeps listing the instance's
			// automated snapshot for minutes after RDS deleted it with the
			// instance (DeleteAutomatedBackups), which failed the final check
			// of a --keep-cluster teardown on a live run. Ask RDS: list one
			// that still exists (a manual snapshot someone took is data, and
			// teardown must not pretend it is gone); skip one that is gone.
			if svc == "rds" && rtype == "snapshot" {
				ok, err := e.dbSnapshotExists(ctx, id)
				if err != nil {
					return nil, err
				}
				if ok {
					add(tdRow{svc + " " + rtype, id, kvMap(m.Tags, rgtKV), false})
				}
				continue
			}
			// eksctl's own stacks go with the cluster (./delete-cluster.sh).
			eksctl := svc == "cloudformation" && strings.HasPrefix(id, "eksctl-"+e.n.Cluster+"-")
			add(tdRow{svc + " " + rtype, id, kvMap(m.Tags, rgtKV), eksctl})
		}
	}
	return rows, nil
}

func tdKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printRows writes the inventory table, account ids masked.
func (e *tdEnv) printRows(rows []tdRow) {
	tw := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tNAME/ID\tOWNER\tCREATED-BY")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Kind, e.mask(r.ID), dash(r.Tags[OwnerTagKey]), dash(r.Tags[tdCreatedByKey]))
	}
	tw.Flush()
}

// tdExit is os.Exit, replaced in tests.
var tdExit = os.Exit

var inventoryCmd = &cobra.Command{
	Use:   "inventory",
	Short: "List every resource of this deployment (read-only); exit 3 when any is left",
	Long: `Lists what carries this deployment's owner tag (` + OwnerTagKey + `=<cluster>.<region>),
and what has one of its derived names whatever its tags: type, name or id,
owner tag and created-by. Account ids are masked to their last four digits.
Exits 0 when nothing is left and 3 when something is; a failed AWS read is an
error (exit 1), never "nothing left".`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newTDEnv(cmd.Context(), cmd.OutOrStdout())
		if err != nil {
			return err
		}
		rows, err := e.inventory(cmd.Context())
		if err != nil {
			return errors.New(e.mask(err.Error()))
		}
		if len(rows) == 0 {
			fmt.Fprintf(e.out, "nothing left for %s\n", e.n.OwnerValue)
			return nil
		}
		e.printRows(rows)
		fmt.Fprintf(e.out, "%d resources of %s are left\n", len(rows), e.n.OwnerValue)
		tdExit(3)
		return nil
	},
}
