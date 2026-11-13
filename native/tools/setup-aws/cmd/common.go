package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// OwnerTagKey marks every resource this tool creates. A resource with a
// global name (bucket, IAM role) is reused or changed only when it carries
// this tag with this deployment's value. It matches lib/common.sh, so the
// scripts and this tool recognise each other's resources.
const OwnerTagKey = "agent-substrate-on-eks/deployment"

// Config holds the settings every subcommand reads. Each has a flag, and the
// flag's default comes from the environment variable named in its help, the
// same convention setup-gcp uses.
type Config struct {
	Region      string
	Profile     string
	ClusterName string
	// Tags are extra key=value tags on every resource, besides the owner tag.
	Tags string
	// ECRRepositories are the private repositories made for demo images.
	ECRRepositories string
	// MinKubernetesMinor is the oldest EKS minor version accepted (1.MINOR).
	MinKubernetesMinor int
	// Plan reports what each step would do and changes nothing.
	Plan bool
}

var cfg Config

// Names are the resource names derived from the config and the caller's
// account. They match lib/common.sh.
type Names struct {
	Partition, Account, Region, Cluster string
	SnapBucket, AssetBucket             string
	IAMPrefix, ECRPrefix                string
	OwnerValue                          string
}

func deriveNames(partition, account, region, cluster string) Names {
	return Names{
		Partition:   partition,
		Account:     account,
		Region:      region,
		Cluster:     cluster,
		SnapBucket:  fmt.Sprintf("%s-snap-%s-%s", cluster, account, region),
		AssetBucket: fmt.Sprintf("%s-assets-%s-%s", cluster, account, region),
		IAMPrefix:   fmt.Sprintf("%s-%s", cluster, region),
		ECRPrefix:   cluster,
		OwnerValue:  fmt.Sprintf("%s.%s", cluster, region),
	}
}

// Tags returns the owner tag plus cfg.Tags, sorted by key.
func (n Names) TagMap(extra string) (map[string]string, error) {
	m := map[string]string{OwnerTagKey: n.OwnerValue}
	for _, kv := range strings.Fields(extra) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("tag %q is not key=value", kv)
		}
		m[k] = v
	}
	return m, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The AWS API surfaces each step uses. Interfaces, so tests run against
// in-memory fakes and never reach AWS.
type (
	STSAPI interface {
		GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
	}
	EKSAPI interface {
		DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
		ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error)
		ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error)
		DescribePodIdentityAssociation(context.Context, *eks.DescribePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error)
		CreatePodIdentityAssociation(context.Context, *eks.CreatePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error)
		UpdatePodIdentityAssociation(context.Context, *eks.UpdatePodIdentityAssociationInput, ...func(*eks.Options)) (*eks.UpdatePodIdentityAssociationOutput, error)
	}
	S3API interface {
		HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
		CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
		GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
		PutBucketTagging(context.Context, *s3.PutBucketTaggingInput, ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
		PutPublicAccessBlock(context.Context, *s3.PutPublicAccessBlockInput, ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
		PutBucketEncryption(context.Context, *s3.PutBucketEncryptionInput, ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error)
		PutBucketPolicy(context.Context, *s3.PutBucketPolicyInput, ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
		PutBucketLifecycleConfiguration(context.Context, *s3.PutBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	}
	IAMAPI interface {
		GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
		ListRoleTags(context.Context, *iam.ListRoleTagsInput, ...func(*iam.Options)) (*iam.ListRoleTagsOutput, error)
		CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
		UpdateAssumeRolePolicy(context.Context, *iam.UpdateAssumeRolePolicyInput, ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
		PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	}
	ECRAPI interface {
		DescribeRepositories(context.Context, *ecr.DescribeRepositoriesInput, ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
		CreateRepository(context.Context, *ecr.CreateRepositoryInput, ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
		ListTagsForResource(context.Context, *ecr.ListTagsForResourceInput, ...func(*ecr.Options)) (*ecr.ListTagsForResourceOutput, error)
	}
)

// Clients bundles the API clients. newClients is replaced in tests.
type Clients struct {
	STS STSAPI
	EKS EKSAPI
	S3  S3API
	IAM IAMAPI
	ECR ECRAPI
}

var newClients = func(ctx context.Context, c Config) (*Clients, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	if c.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &Clients{
		STS: sts.NewFromConfig(ac),
		EKS: eks.NewFromConfig(ac),
		S3:  s3.NewFromConfig(ac),
		IAM: iam.NewFromConfig(ac),
		ECR: ecr.NewFromConfig(ac),
	}, nil
}

// env is the run context every step receives.
type env struct {
	cfg   Config
	c     *Clients
	names Names
	tags  map[string]string
}

func newEnv(ctx context.Context) (*env, error) {
	if cfg.ClusterName == "" {
		return nil, errors.New("--cluster-name (CLUSTER_NAME) is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("--region (AWS_REGION) is required")
	}
	c, err := newClients(ctx, cfg)
	if err != nil {
		return nil, err
	}
	id, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("AWS credentials: %w", err)
	}
	arn := aws.ToString(id.Arn)
	parts := strings.Split(arn, ":")
	if len(parts) < 5 {
		return nil, fmt.Errorf("unexpected caller ARN %q", arn)
	}
	n := deriveNames(parts[1], aws.ToString(id.Account), cfg.Region, cfg.ClusterName)
	tags, err := n.TagMap(cfg.Tags)
	if err != nil {
		return nil, err
	}
	return &env{cfg: cfg, c: c, names: n, tags: tags}, nil
}

// act runs do, or under --plan logs what it would do and skips it.
func (e *env) act(what string, do func() error) error {
	if e.cfg.Plan {
		slog.Info("plan: would " + what)
		return nil
	}
	slog.Info(what)
	return do()
}

// isNotFound reports whether err is an AWS "does not exist" error. Anything
// else, a denied or throttled read among them, is a real error: treating it
// as "not found" would create a duplicate or hide a permissions problem.
func isNotFound(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "NotFound", "NoSuchBucket", "NoSuchEntity", "ResourceNotFoundException",
		"RepositoryNotFoundException", "NoSuchTagSet":
		return true
	}
	return false
}

type envType interface{ string | int | bool }

// getEnv returns the environment variable key parsed as T, or fallback.
func getEnv[T envType](key string, fallback T) T {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	var out any = fallback
	switch any(fallback).(type) {
	case string:
		out = v
	case int:
		if n, err := strconv.Atoi(v); err == nil {
			out = n
		}
	case bool:
		if b, err := strconv.ParseBool(v); err == nil {
			out = b
		}
	}
	return out.(T)
}
