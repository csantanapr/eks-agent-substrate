package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/spf13/cobra"
)

// ClusterInfo is what the other steps need to know about the cluster.
type ClusterInfo struct {
	Version     string
	AutoMode    bool
	PodIdentity bool // the EKS Pod Identity Agent runs (an add-on, or built into Auto Mode)
}

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Check that the EKS cluster can run Agent Substrate",
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		_, err = e.checkCluster(cmd.Context())
		return err
	},
}

// checkCluster reads the cluster and fails unless it is ACTIVE, recent
// enough, and runs the EKS Pod Identity Agent. It changes nothing.
//
// Cluster creation is not part of this POC: the installer connects to an
// existing cluster, the way substrate-gke's wizard can.
func (e *env) checkCluster(ctx context.Context) (ClusterInfo, error) {
	out, err := e.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(e.names.Cluster)})
	if err != nil {
		return ClusterInfo{}, fmt.Errorf("describe cluster %s: %w", e.names.Cluster, err)
	}
	c := out.Cluster
	if c.Status != ekstypes.ClusterStatusActive {
		return ClusterInfo{}, fmt.Errorf("cluster %s is %s; wait for ACTIVE", e.names.Cluster, c.Status)
	}
	info := ClusterInfo{Version: aws.ToString(c.Version)}
	if err := versionAtLeast(info.Version, e.cfg.MinKubernetesMinor); err != nil {
		return info, fmt.Errorf("cluster %s: %w", e.names.Cluster, err)
	}
	info.AutoMode = c.ComputeConfig != nil && aws.ToBool(c.ComputeConfig.Enabled)
	if info.AutoMode {
		// Auto Mode runs the Pod Identity Agent itself, with no add-on.
		info.PodIdentity = true
	} else {
		addons, err := e.c.EKS.ListAddons(ctx, &eks.ListAddonsInput{ClusterName: aws.String(e.names.Cluster)})
		if err != nil {
			return info, fmt.Errorf("list add-ons: %w", err)
		}
		for _, a := range addons.Addons {
			if a == "eks-pod-identity-agent" {
				info.PodIdentity = true
			}
		}
	}
	if !info.PodIdentity {
		return info, fmt.Errorf("cluster %s has no eks-pod-identity-agent add-on; ate-api-server and atelet get their AWS credentials through EKS Pod Identity", e.names.Cluster)
	}
	slog.Info("cluster ok", "cluster", e.names.Cluster, "version", info.Version, "autoMode", info.AutoMode)
	return info, nil
}

// versionAtLeast checks an EKS version "1.N" against the minimum minor.
func versionAtLeast(v string, minMinor int) error {
	major, minor, ok := strings.Cut(v, ".")
	m, err := strconv.Atoi(minor)
	if !ok || major != "1" || err != nil {
		return fmt.Errorf("unrecognised Kubernetes version %q", v)
	}
	if m < minMinor {
		return fmt.Errorf("Kubernetes %s is older than the supported 1.%d; upgrade the control plane", v, minMinor)
	}
	return nil
}
