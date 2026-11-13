package cmd

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// fakeAWS is one in-memory account. writes counts every mutating call, so a
// test can prove --plan and a second run change nothing.
type fakeAWS struct {
	mu       sync.Mutex
	version  string
	autoMode bool
	addons   []string
	buckets  map[string]map[string]string // name -> tags
	roles    map[string]map[string]string // name -> tags
	policies map[string]string            // role -> inline policy
	assocs   map[string]string            // ns/sa -> role ARN
	repos    map[string]bool
	writes   []string
	denyHead bool // HeadBucket returns AccessDenied
}

func newFake() *fakeAWS {
	return &fakeAWS{version: "1.37", autoMode: true,
		buckets: map[string]map[string]string{}, roles: map[string]map[string]string{},
		policies: map[string]string{}, assocs: map[string]string{}, repos: map[string]bool{}}
}

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

func (f *fakeAWS) write(s string) { f.writes = append(f.writes, s) }

func (f *fakeAWS) clients() *Clients {
	return &Clients{STS: fsts{}, EKS: (*feks)(f), S3: (*fs3)(f), IAM: (*fiam)(f), ECR: (*fecr)(f)}
}

type fsts struct{}

func (fsts) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String("111122223333"), Arn: aws.String("arn:aws:sts::111122223333:assumed-role/Admin/me")}, nil
}

type feks fakeAWS

func (f *feks) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{Name: in.Name, Status: ekstypes.ClusterStatusActive,
		Version: aws.String(f.version), ComputeConfig: &ekstypes.ComputeConfigResponse{Enabled: aws.Bool(f.autoMode)}}}, nil
}
func (f *feks) ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	return &eks.ListAddonsOutput{Addons: f.addons}, nil
}
func (f *feks) ListPodIdentityAssociations(_ context.Context, in *eks.ListPodIdentityAssociationsInput, _ ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	k := aws.ToString(in.Namespace) + "/" + aws.ToString(in.ServiceAccount)
	if _, ok := f.assocs[k]; !ok {
		return &eks.ListPodIdentityAssociationsOutput{}, nil
	}
	return &eks.ListPodIdentityAssociationsOutput{Associations: []ekstypes.PodIdentityAssociationSummary{{AssociationId: aws.String(k)}}}, nil
}
func (f *feks) DescribePodIdentityAssociation(_ context.Context, in *eks.DescribePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	return &eks.DescribePodIdentityAssociationOutput{Association: &ekstypes.PodIdentityAssociation{RoleArn: aws.String(f.assocs[aws.ToString(in.AssociationId)])}}, nil
}
func (f *feks) CreatePodIdentityAssociation(_ context.Context, in *eks.CreatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error) {
	k := aws.ToString(in.Namespace) + "/" + aws.ToString(in.ServiceAccount)
	f.assocs[k] = aws.ToString(in.RoleArn)
	(*fakeAWS)(f).write("CreatePodIdentityAssociation " + k)
	return &eks.CreatePodIdentityAssociationOutput{}, nil
}
func (f *feks) UpdatePodIdentityAssociation(_ context.Context, in *eks.UpdatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.UpdatePodIdentityAssociationOutput, error) {
	f.assocs[aws.ToString(in.AssociationId)] = aws.ToString(in.RoleArn)
	(*fakeAWS)(f).write("UpdatePodIdentityAssociation " + aws.ToString(in.AssociationId))
	return &eks.UpdatePodIdentityAssociationOutput{}, nil
}

type fs3 fakeAWS

func (f *fs3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if f.denyHead {
		return nil, apiErr("AccessDenied")
	}
	if _, ok := f.buckets[aws.ToString(in.Bucket)]; !ok {
		return nil, apiErr("NotFound")
	}
	return &s3.HeadBucketOutput{}, nil
}
func (f *fs3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	if in.ObjectOwnership != s3types.ObjectOwnershipBucketOwnerEnforced {
		return nil, fmt.Errorf("bucket %s created without BucketOwnerEnforced", aws.ToString(in.Bucket))
	}
	f.buckets[aws.ToString(in.Bucket)] = map[string]string{}
	(*fakeAWS)(f).write("CreateBucket " + aws.ToString(in.Bucket))
	return &s3.CreateBucketOutput{}, nil
}
func (f *fs3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	tags := f.buckets[aws.ToString(in.Bucket)]
	if len(tags) == 0 {
		return nil, apiErr("NoSuchTagSet")
	}
	out := &s3.GetBucketTaggingOutput{}
	for k, v := range tags {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}
func (f *fs3) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	m := map[string]string{}
	for _, t := range in.Tagging.TagSet {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.buckets[aws.ToString(in.Bucket)] = m
	(*fakeAWS)(f).write("PutBucketTagging " + aws.ToString(in.Bucket))
	return &s3.PutBucketTaggingOutput{}, nil
}
func (f *fs3) PutPublicAccessBlock(_ context.Context, in *s3.PutPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	c := in.PublicAccessBlockConfiguration
	if !aws.ToBool(c.BlockPublicAcls) || !aws.ToBool(c.IgnorePublicAcls) || !aws.ToBool(c.BlockPublicPolicy) || !aws.ToBool(c.RestrictPublicBuckets) {
		return nil, fmt.Errorf("public access block is not fully on")
	}
	(*fakeAWS)(f).write("PutPublicAccessBlock " + aws.ToString(in.Bucket))
	return &s3.PutPublicAccessBlockOutput{}, nil
}
func (f *fs3) PutBucketEncryption(_ context.Context, in *s3.PutBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	(*fakeAWS)(f).write("PutBucketEncryption " + aws.ToString(in.Bucket))
	return &s3.PutBucketEncryptionOutput{}, nil
}
func (f *fs3) PutBucketPolicy(_ context.Context, in *s3.PutBucketPolicyInput, _ ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	(*fakeAWS)(f).write("PutBucketPolicy " + aws.ToString(in.Bucket))
	return &s3.PutBucketPolicyOutput{}, nil
}
func (f *fs3) PutBucketLifecycleConfiguration(_ context.Context, in *s3.PutBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	(*fakeAWS)(f).write("PutBucketLifecycleConfiguration " + aws.ToString(in.Bucket))
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}

type fiam fakeAWS

func (f *fiam) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if _, ok := f.roles[aws.ToString(in.RoleName)]; !ok {
		return nil, apiErr("NoSuchEntity")
	}
	return &iam.GetRoleOutput{}, nil
}
func (f *fiam) ListRoleTags(_ context.Context, in *iam.ListRoleTagsInput, _ ...func(*iam.Options)) (*iam.ListRoleTagsOutput, error) {
	out := &iam.ListRoleTagsOutput{}
	for k, v := range f.roles[aws.ToString(in.RoleName)] {
		out.Tags = append(out.Tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}
func (f *fiam) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	m := map[string]string{}
	for _, t := range in.Tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.roles[aws.ToString(in.RoleName)] = m
	(*fakeAWS)(f).write("CreateRole " + aws.ToString(in.RoleName))
	return &iam.CreateRoleOutput{}, nil
}
func (f *fiam) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	(*fakeAWS)(f).write("UpdateAssumeRolePolicy " + aws.ToString(in.RoleName))
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}
func (f *fiam) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.policies[aws.ToString(in.RoleName)] = aws.ToString(in.PolicyDocument)
	(*fakeAWS)(f).write("PutRolePolicy " + aws.ToString(in.RoleName))
	return &iam.PutRolePolicyOutput{}, nil
}

type fecr fakeAWS

func (f *fecr) DescribeRepositories(_ context.Context, in *ecr.DescribeRepositoriesInput, _ ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	if !f.repos[in.RepositoryNames[0]] {
		return nil, apiErr("RepositoryNotFoundException")
	}
	return &ecr.DescribeRepositoriesOutput{}, nil
}
func (f *fecr) CreateRepository(_ context.Context, in *ecr.CreateRepositoryInput, _ ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error) {
	if !in.ImageScanningConfiguration.ScanOnPush || in.EncryptionConfiguration.EncryptionType != ecrtypes.EncryptionTypeAes256 {
		return nil, fmt.Errorf("repository without scan on push or encryption")
	}
	f.repos[aws.ToString(in.RepositoryName)] = true
	(*fakeAWS)(f).write("CreateRepository " + aws.ToString(in.RepositoryName))
	return &ecr.CreateRepositoryOutput{}, nil
}
