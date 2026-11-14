package mode3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/awsload"
)

// ecrAPI is the ECR calls Remove makes.
type ecrAPI interface {
	DescribeRepositories(ctx context.Context, in *ecr.DescribeRepositoriesInput, opts ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
	ListTagsForResource(ctx context.Context, in *ecr.ListTagsForResourceInput, opts ...func(*ecr.Options)) (*ecr.ListTagsForResourceOutput, error)
	BatchDeleteImage(ctx context.Context, in *ecr.BatchDeleteImageInput, opts ...func(*ecr.Options)) (*ecr.BatchDeleteImageOutput, error)
}

// ecrFor is the ECR client of o's profile and Region, loaded through
// awsload like every step's clients; tests replace it.
var ecrFor = func(ctx context.Context, o plugin.Options) (ecrAPI, error) {
	cfg, err := awsload.Config(ctx, o.Region, o.Profile)
	if err != nil {
		return nil, err
	}
	return ecr.NewFromConfig(cfg), nil
}

// counterRepo is the deployment's counter repository (setup-aws bootstrap
// creates it, and teardown deletes it); Deploy pushes <counterRepo>:<Tag>.
func counterRepo(o plugin.Options) string { return o.Cluster + "/counter" }

// deleteCounterImage deletes the tag Deploy pushed (ECR deletes the image
// with its last tag), only from a repository carrying this deployment's
// owner tag: one of that name with another owner, or none, is left alone
// (review R9). A repository or tag that is already gone is done.
func deleteCounterImage(ctx context.Context, w io.Writer, o plugin.Options) error {
	c, err := ecrFor(ctx, o)
	if err != nil {
		return err
	}
	repo := counterRepo(o)
	var nf *ecrtypes.RepositoryNotFoundException
	d, err := c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{repo}})
	if errors.As(err, &nf) || (err == nil && len(d.Repositories) == 0) {
		fmt.Fprintf(w, "No repository %s: no counter image to delete\n", repo)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the repository %s: %w", repo, err)
	}
	tags, err := c.ListTagsForResource(ctx, &ecr.ListTagsForResourceInput{ResourceArn: d.Repositories[0].RepositoryArn})
	if err != nil {
		return fmt.Errorf("read the tags of the repository %s: %w", repo, err)
	}
	owner, want := "", o.Cluster+"."+o.Region
	for _, t := range tags.Tags {
		if aws.ToString(t.Key) == awsx.OwnerTagKey {
			owner = aws.ToString(t.Value)
		}
	}
	if owner != want {
		fmt.Fprintf(w, "Repository %s is not owned by %s (%s=%q): its images are left alone\n", repo, want, awsx.OwnerTagKey, owner)
		return nil
	}
	out, err := c.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
		RepositoryName: aws.String(repo),
		ImageIds:       []ecrtypes.ImageIdentifier{{ImageTag: aws.String(Tag)}},
	})
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
