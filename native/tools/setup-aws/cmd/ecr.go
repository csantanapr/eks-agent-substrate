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
		_, err := e.c.ECR.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{repo}})
		switch {
		case err == nil:
			slog.Info("repository exists", "repository", repo)
			continue
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
