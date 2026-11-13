package cmd

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "setup-aws",
	Short: "Provision the AWS resources Agent Substrate uses on Amazon EKS",
	Long: `setup-aws provisions the AWS resources Agent Substrate needs on an EKS
cluster: the snapshot and microVM asset buckets, the EKS Pod Identity roles of
ate-api-server and atelet, and private ECR repositories. Every step is
idempotent, and --plan reports what a step would do without changing anything.`,
	SilenceUsage: true,
}

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create one kind of resource",
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Check a resource without changing it",
}

// Execute runs the root command.
func Execute() error { return rootCmd.Execute() }

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	f := rootCmd.PersistentFlags()
	f.StringVar(&cfg.Region, "region", getEnv("AWS_REGION", ""), "AWS Region (AWS_REGION)")
	f.StringVar(&cfg.Profile, "profile", getEnv("AWS_PROFILE", ""), "shared config profile (AWS_PROFILE); empty uses the default credential chain")
	f.StringVar(&cfg.ClusterName, "cluster-name", getEnv("CLUSTER_NAME", ""), "EKS cluster name (CLUSTER_NAME)")
	f.StringVar(&cfg.Tags, "tags", getEnv("TAGS", "project=agent-substrate auto-delete=no"), "space-separated key=value tags for every resource (TAGS)")
	f.StringVar(&cfg.ECRRepositories, "ecr-repositories", getEnv("ECR_REPOSITORIES", "counter"), "space-separated ECR repositories to create under <cluster>/ (ECR_REPOSITORIES)")
	f.IntVar(&cfg.MinKubernetesMinor, "min-kubernetes-minor", getEnv("MIN_KUBERNETES_MINOR", 37), "oldest EKS version accepted, as 1.N (MIN_KUBERNETES_MINOR); Substrate needs the PodCertificate APIs, which substrate-gke and eks-talk both take from 1.37")
	f.BoolVar(&cfg.Plan, "plan", getEnv("PLAN", false), "report what each step would do and change nothing (PLAN)")

	rootCmd.AddCommand(createCmd, checkCmd, bootstrapCmd, envCmd)
	createCmd.AddCommand(bucketCmd, iamCmd, ecrCmd)
	checkCmd.AddCommand(clusterCmd)
}
