package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3files"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/awsload"
)

// pluginClients are the AWS clients plugins use, beyond Clients and
// tdClients. Each plugin declares the narrow interface it calls in its own
// file (plugin_<id>.go) and takes its client from here, so tests replace
// newPluginClients with fakes and no plugin edits another's file. Every
// service a plugin needs is listed once here, which keeps go.mod in one
// place.
type pluginClients struct {
	CFN     *cloudformation.Client
	EC2     *ec2.Client
	EFS     *efs.Client
	EKS     *eks.Client
	IAM     *iam.Client
	RDS     *rds.Client
	S3      *s3.Client
	S3Files *s3files.Client
	SM      *secretsmanager.Client
}

// newPluginClients loads the config the way newClients does. Tests replace it
// (and plugins' own seams built on it).
var newPluginClients = func(ctx context.Context, c Config) (*pluginClients, error) {
	ac, err := loadAWSConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	return &pluginClients{
		CFN: cloudformation.NewFromConfig(ac), EC2: ec2.NewFromConfig(ac), EFS: efs.NewFromConfig(ac),
		EKS: eks.NewFromConfig(ac), IAM: iam.NewFromConfig(ac), RDS: rds.NewFromConfig(ac),
		S3: s3.NewFromConfig(ac), S3Files: s3files.NewFromConfig(ac), SM: secretsmanager.NewFromConfig(ac),
	}, nil
}

// loadAWSConfig is every setup-aws client's config: awsload's, whose
// clients outlive the session they start with (BUG5).
func loadAWSConfig(ctx context.Context, c Config) (aws.Config, error) {
	ac, err := awsload.Config(ctx, c.Region, c.Profile)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	return ac, nil
}
