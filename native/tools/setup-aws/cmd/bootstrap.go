package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Run every setup step in order",
	Long:  `Checks the cluster, then creates the S3 buckets, the EKS Pod Identity roles and associations, and the ECR repositories. Idempotent; --plan changes nothing.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		return e.bootstrap(cmd.Context())
	},
}

func (e *env) bootstrap(ctx context.Context) error {
	if e.cfg.Plan {
		slog.Info("plan mode: reading only; nothing will be created or changed")
	}
	slog.Info("Starting full bootstrap...", "cluster", e.names.Cluster, "region", e.names.Region)
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			info, err := e.checkCluster(ctx)
			if err != nil {
				return err
			}
			// EKS attaches a control-plane interface after create cluster's
			// pass (eni_tags.go); this one tags it.
			return e.tagVPCInterfaces(ctx, e.c.EC2, info.VPC)
		},
		e.createBuckets,
		e.createIAM,
		e.createECR,
	}
	if len(steps) != len(phases.Bootstrap) {
		return fmt.Errorf("internal: %d bootstrap steps for %d phases", len(steps), len(phases.Bootstrap))
	}
	for i, run := range steps {
		slog.Info(phases.Line(phases.Bootstrap, i))
		if err := run(ctx); err != nil {
			return err
		}
	}
	slog.Info("Bootstrap completed successfully.")
	return nil
}

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Print the derived resource names as JSON (read-only)",
	Long:  `Prints the names bootstrap uses, which the installer passes to ate-setup: the snapshot and asset buckets, the IAM role prefix and the ECR prefix.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]string{
			"snapBucket":  e.names.SnapBucket,
			"assetBucket": e.names.AssetBucket,
			"iamPrefix":   e.names.IAMPrefix,
			"ecrPrefix":   e.names.ECRPrefix,
			"owner":       e.names.OwnerValue,
		})
	},
}
