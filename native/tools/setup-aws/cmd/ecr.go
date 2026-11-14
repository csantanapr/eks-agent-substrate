package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/spf13/cobra"
)

var ecrCmd = &cobra.Command{
	Use:   "ecr",
	Short: "Create private ECR repositories for demo images (scan on push)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		return e.createECR(cmd.Context())
	},
}

func (e *env) createECR(ctx context.Context) error {
	tags := make([]ecrtypes.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		tags = append(tags, ecrtypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	for _, r := range strings.Fields(e.cfg.ECRRepositories) {
		repo := e.names.ECRPrefix + "/" + r
		out, err := e.c.ECR.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{repo}})
		switch {
		case err == nil && len(out.Repositories) > 0:
			// Reused only when it is this deployment's, as buckets and roles
			// are (AC2.2, review F6): teardown refuses a foreign repository,
			// so reusing one here would block the whole teardown later.
			t, err := e.c.ECR.ListTagsForResource(ctx, &ecr.ListTagsForResourceInput{ResourceArn: out.Repositories[0].RepositoryArn})
			if err != nil {
				return fmt.Errorf("read tags of repository %s: %w", repo, err)
			}
			got := ""
			for _, tag := range t.Tags {
				if aws.ToString(tag.Key) == OwnerTagKey {
					got = aws.ToString(tag.Value)
				}
			}
			if got != e.names.OwnerValue {
				return fmt.Errorf("ECR repository %s exists but is not owned by %s (%s=%q)", repo, e.names.OwnerValue, OwnerTagKey, got)
			}
			slog.Info("repository exists", "repository", repo)
			continue
		case err == nil:
			return fmt.Errorf("describe repository %s: no repository returned", repo)
		case !isNotFound(err):
			return fmt.Errorf("describe repository %s: %w", repo, err)
		}
		if err := e.act("create repository "+repo, func() error {
			_, err := e.c.ECR.CreateRepository(ctx, &ecr.CreateRepositoryInput{
				RepositoryName:             aws.String(repo),
				ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{ScanOnPush: true},
				EncryptionConfiguration:    &ecrtypes.EncryptionConfiguration{EncryptionType: ecrtypes.EncryptionTypeAes256},
				Tags:                       tags,
			})
			return err
		}); err != nil {
			return fmt.Errorf("create repository %s: %w", repo, err)
		}
	}
	return nil
}
