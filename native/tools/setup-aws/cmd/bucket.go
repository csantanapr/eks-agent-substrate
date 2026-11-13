package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/cobra"
)

var bucketCmd = &cobra.Command{
	Use:   "bucket",
	Short: "Create the private snapshot and microVM asset buckets",
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		return e.createBuckets(cmd.Context())
	},
}

// createBuckets makes both buckets and reconciles their settings: Block
// Public Access, BucketOwnerEnforced, SSE-S3, a TLS-only bucket policy and a
// lifecycle rule for incomplete multipart uploads. It ports aws/infra.sh.
func (e *env) createBuckets(ctx context.Context) error {
	for _, b := range []string{e.names.SnapBucket, e.names.AssetBucket} {
		if err := e.ensureBucket(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func (e *env) ensureBucket(ctx context.Context, b string) error {
	exists, err := e.ownedBucket(ctx, b)
	if err != nil {
		return err
	}
	owner := aws.String(e.names.Account)
	tagSet := make([]s3types.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		tagSet = append(tagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	if !exists {
		if err := e.act("create bucket "+b, func() error {
			// Tagged in the create call itself (s3:TagResource): a bucket
			// created untagged by a run that then failed would be refused
			// as foreign by every later bootstrap and teardown (review F5).
			// us-east-1 takes no location constraint; every other Region needs one.
			conf := &s3types.CreateBucketConfiguration{Tags: tagSet}
			if e.names.Region != "us-east-1" {
				conf.LocationConstraint = s3types.BucketLocationConstraint(e.names.Region)
			}
			in := &s3.CreateBucketInput{Bucket: aws.String(b), ObjectOwnership: s3types.ObjectOwnershipBucketOwnerEnforced,
				CreateBucketConfiguration: conf}
			if _, err := e.c.S3.CreateBucket(ctx, in); err != nil {
				return fmt.Errorf("create bucket %s: %w", b, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"DenyInsecureTransport","Effect":"Deny","Principal":"*",`+
		`"Action":"s3:*","Resource":["arn:%[1]s:s3:::%[2]s","arn:%[1]s:s3:::%[2]s/*"],`+
		`"Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`, e.names.Partition, b)
	return e.act("reconcile "+b+" (tags, public access block, SSE-S3, TLS-only policy, lifecycle)", func() error {
		// Reconcile the tags too: TAGS may have changed since the create.
		if _, err := e.c.S3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(b), ExpectedBucketOwner: owner, Tagging: &s3types.Tagging{TagSet: tagSet}}); err != nil {
			return fmt.Errorf("tag %s: %w", b, err)
		}
		t := aws.Bool(true)
		if _, err := e.c.S3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{Bucket: aws.String(b), ExpectedBucketOwner: owner,
			PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{BlockPublicAcls: t, IgnorePublicAcls: t, BlockPublicPolicy: t, RestrictPublicBuckets: t}}); err != nil {
			return fmt.Errorf("public access block %s: %w", b, err)
		}
		if _, err := e.c.S3.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String(b), ExpectedBucketOwner: owner,
			ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{SSEAlgorithm: s3types.ServerSideEncryptionAes256}}}}}); err != nil {
			return fmt.Errorf("encryption %s: %w", b, err)
		}
		if _, err := e.c.S3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(b), ExpectedBucketOwner: owner, Policy: aws.String(policy)}); err != nil {
			return fmt.Errorf("policy %s: %w", b, err)
		}
		if _, err := e.c.S3.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String(b), ExpectedBucketOwner: owner,
			LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: []s3types.LifecycleRule{{
				ID: aws.String("abort-incomplete-mpu"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{},
				AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)}}}}}); err != nil {
			return fmt.Errorf("lifecycle %s: %w", b, err)
		}
		slog.Info("bucket ready", "bucket", b)
		return nil
	})
}

// ownedBucket reports whether b exists in this account. An existing bucket
// without this deployment's owner tag is an error, never reused.
func (e *env) ownedBucket(ctx context.Context, b string) (bool, error) {
	owner := aws.String(e.names.Account)
	if _, err := e.c.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b), ExpectedBucketOwner: owner}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("head bucket %s: %w", b, err)
	}
	out, err := e.c.S3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(b), ExpectedBucketOwner: owner})
	if err != nil && !isNotFound(err) {
		return false, fmt.Errorf("read tags of bucket %s: %w", b, err)
	}
	got := ""
	if out != nil {
		for _, t := range out.TagSet {
			if aws.ToString(t.Key) == OwnerTagKey {
				got = aws.ToString(t.Value)
			}
		}
	}
	if got != e.names.OwnerValue {
		return false, fmt.Errorf("bucket %s exists but is not owned by %s (%s=%q)", b, e.names.OwnerValue, OwnerTagKey, got)
	}
	return true, nil
}
