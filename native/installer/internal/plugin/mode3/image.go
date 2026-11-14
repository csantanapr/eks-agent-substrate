package mode3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// ecrAPI is the ECR call Remove makes.
type ecrAPI interface {
	BatchDeleteImage(ctx context.Context, in *ecr.BatchDeleteImageInput, opts ...func(*ecr.Options)) (*ecr.BatchDeleteImageOutput, error)
}

// ecrFor is the ECR client of o's profile and Region; tests replace it.
var ecrFor = func(ctx context.Context, o plugin.Options) (ecrAPI, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
	if o.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.Profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return ecr.NewFromConfig(cfg), nil
}

// counterRepo is the deployment's counter repository (setup-aws bootstrap
// creates it, and teardown deletes it); Deploy pushes <counterRepo>:<Tag>.
func counterRepo(o plugin.Options) string { return o.Cluster + "/counter" }

// deleteCounterImage deletes the tag Deploy pushed (ECR deletes the image
// with its last tag). A repository or tag that is already gone is done.
func deleteCounterImage(ctx context.Context, w io.Writer, o plugin.Options) error {
	c, err := ecrFor(ctx, o)
	if err != nil {
		return err
	}
	repo := counterRepo(o)
	out, err := c.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
		RepositoryName: aws.String(repo),
		ImageIds:       []ecrtypes.ImageIdentifier{{ImageTag: aws.String(Tag)}},
	})
	var nf *ecrtypes.RepositoryNotFoundException
	if errors.As(err, &nf) {
		fmt.Fprintf(w, "No repository %s: no counter image to delete\n", repo)
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete the counter image %s:%s: %w", repo, Tag, err)
	}
	for _, f := range out.Failures {
		if f.FailureCode == ecrtypes.ImageFailureCodeImageNotFound {
			fmt.Fprintf(w, "No counter image %s:%s to delete\n", repo, Tag)
			return nil
		}
		return fmt.Errorf("delete the counter image %s:%s: %s: %s", repo, Tag, f.FailureCode, aws.ToString(f.FailureReason))
	}
	fmt.Fprintf(w, "Deleted the counter image %s:%s\n", repo, Tag)
	return nil
}
