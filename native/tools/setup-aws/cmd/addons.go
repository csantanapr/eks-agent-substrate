package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// clusterAddon is one EKS add-on of cluster.yaml.
type clusterAddon struct {
	Name string
	// Config is the add-on's configuration values (JSON), if any.
	Config string
}

// clusterAddons, in create order: the networking add-ons the nodes need to
// become Ready first, then coredns and the EBS CSI driver, which stay
// DEGRADED until nodes exist. Creating an add-on does not wait for it, so
// all five are created before the node groups and waited for after them
// (waitReady), which is the ordering create-cluster.sh had to work around.
//
// The VPC CNI enforces NetworkPolicy (Substrate ships default-deny policies
// for ate-system and one per WorkerPool) only with enableNetworkPolicy on.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/cni-network-policy.html
var clusterAddons = []clusterAddon{
	{Name: "vpc-cni", Config: `{"enableNetworkPolicy":"true"}`},
	{Name: "kube-proxy"},
	{Name: "eks-pod-identity-agent"},
	{Name: "coredns"},
	{Name: "aws-ebs-csi-driver"},
}

// ebsControllerSA is the EBS CSI controller's ServiceAccount, which gets the
// driver's role through EKS Pod Identity (eksctl's useDefaultPodIdentityAssociations).
const ebsControllerSA = "ebs-csi-controller-sa"

func (r *clusterRun) ensureAddons(ctx context.Context) error {
	for _, a := range clusterAddons {
		cur := r.addons[a.Name]
		if cur == nil {
			in := &eks.CreateAddonInput{ClusterName: aws.String(r.e.names.Cluster), AddonName: aws.String(a.Name),
				ResolveConflicts: ekstypes.ResolveConflictsOverwrite, Tags: r.tags}
			if a.Config != "" {
				in.ConfigurationValues = aws.String(a.Config)
			}
			if a.Name == "aws-ebs-csi-driver" {
				in.PodIdentityAssociations = []ekstypes.AddonPodIdentityAssociations{{
					ServiceAccount: aws.String(ebsControllerSA), RoleArn: aws.String(r.roleARN(r.ebsRoleName()))}}
			}
			if err := r.create("add-on "+a.Name, func() error {
				return retryNewRole(ctx, func() error {
					_, err := r.c.EKS.CreateAddon(ctx, in)
					return err
				})
			}); err != nil {
				return fmt.Errorf("create add-on %s: %w", a.Name, err)
			}
			continue
		}
		if cur.Status == ekstypes.AddonStatusCreateFailed {
			return fmt.Errorf("add-on %s is CREATE_FAILED (%s); delete it (aws eks delete-addon --cluster-name %s --addon-name %s) and run again",
				a.Name, addonIssues(cur), r.e.names.Cluster, a.Name)
		}
		if a.Config != "" {
			if err := r.reconcileAddonConfig(ctx, a, cur); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileAddonConfig sets each key of a.Config that the add-on's current
// configuration lacks or has another value for, keeping the others.
func (r *clusterRun) reconcileAddonConfig(ctx context.Context, a clusterAddon, cur *ekstypes.Addon) error {
	have := map[string]any{}
	if s := aws.ToString(cur.ConfigurationValues); s != "" {
		if err := json.Unmarshal([]byte(s), &have); err != nil {
			return fmt.Errorf("add-on %s: its configuration is not a JSON object: %w", a.Name, err)
		}
	}
	want := map[string]any{}
	if err := json.Unmarshal([]byte(a.Config), &want); err != nil {
		return fmt.Errorf("internal: add-on %s config: %w", a.Name, err)
	}
	changed := false
	for k, v := range want {
		if have[k] != v {
			have[k], changed = v, true
		}
	}
	if !changed {
		return nil
	}
	merged, err := json.Marshal(have)
	if err != nil {
		return err
	}
	return r.change(fmt.Sprintf("set %s on add-on %s", a.Config, a.Name), func() error {
		_, err := r.c.EKS.UpdateAddon(ctx, &eks.UpdateAddonInput{ClusterName: aws.String(r.e.names.Cluster),
			AddonName: aws.String(a.Name), ConfigurationValues: aws.String(string(merged)), ResolveConflicts: ekstypes.ResolveConflictsOverwrite})
		return err
	})
}

// waitAddons waits until every add-on is ACTIVE. Only CREATE_FAILED ends
// the wait early: DEGRADED is normal while the nodes come up.
func (r *clusterRun) waitAddons(ctx context.Context) error {
	for _, a := range clusterAddons {
		var last *ekstypes.Addon
		err := waitFor(ctx, "add-on "+a.Name+" to be ACTIVE", 20*time.Minute, func() (bool, error) {
			out, err := r.c.EKS.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(r.e.names.Cluster), AddonName: aws.String(a.Name)})
			if err != nil {
				return false, fmt.Errorf("describe add-on %s: %w", a.Name, err)
			}
			last = out.Addon
			switch out.Addon.Status {
			case ekstypes.AddonStatusActive:
				return true, nil
			case ekstypes.AddonStatusCreateFailed, ekstypes.AddonStatusDeleting, ekstypes.AddonStatusDeleteFailed:
				return false, fmt.Errorf("add-on %s is %s (%s)", a.Name, out.Addon.Status, addonIssues(out.Addon))
			}
			return false, nil
		})
		if err != nil {
			if last != nil {
				err = fmt.Errorf("%w; last status %s (%s); see aws eks describe-addon --cluster-name %s --addon-name %s",
					err, last.Status, addonIssues(last), r.e.names.Cluster, a.Name)
			}
			return err
		}
		slog.Info("add-on ACTIVE", "addon", a.Name)
	}
	return nil
}

func addonIssues(a *ekstypes.Addon) string {
	if a == nil || a.Health == nil || len(a.Health.Issues) == 0 {
		return "no health issues reported"
	}
	var s []string
	for _, i := range a.Health.Issues {
		s = append(s, fmt.Sprintf("%s: %s", i.Code, aws.ToString(i.Message)))
	}
	return strings.Join(s, "; ")
}
