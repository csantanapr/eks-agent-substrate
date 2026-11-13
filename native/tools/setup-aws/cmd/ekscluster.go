package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// createClusterInput is the EKS cluster create-cluster.sh's eksctl config
// describes, with two deliberate differences: the cluster gets only the
// private subnets (see vpc.go), and the caller's admin access is an explicit
// access entry (ensureAccess) instead of EKS's creator bootstrap, so a rerun
// can check and repair it.
func (r *clusterRun) createClusterInput() *eks.CreateClusterInput {
	in := &eks.CreateClusterInput{
		Name:    aws.String(r.e.names.Cluster),
		Version: aws.String(r.cfg.KubernetesVersion),
		RoleArn: aws.String(r.roleARN(r.clusterRoleName())),
		// The API endpoint is public, limited to EKS_PUBLIC_ACCESS_CIDRS,
		// and private inside the VPC, so nodes reach it without the NAT.
		//   https://docs.aws.amazon.com/eks/latest/userguide/cluster-endpoint.html
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{
			SubnetIds:             r.private,
			EndpointPublicAccess:  aws.Bool(true),
			EndpointPrivateAccess: aws.Bool(true),
			PublicAccessCidrs:     strings.Fields(r.cfg.PublicAccessCIDRs),
		},
		// Access entries only (no aws-auth ConfigMap).
		//   https://docs.aws.amazon.com/eks/latest/userguide/access-entries.html
		AccessConfig: &ekstypes.CreateAccessConfigRequest{
			AuthenticationMode:                      ekstypes.AuthenticationModeApi,
			BootstrapClusterCreatorAdminPermissions: aws.Bool(false),
		},
		// The add-ons phase installs vpc-cni, kube-proxy and coredns as EKS
		// add-ons; Auto Mode requires this off.
		BootstrapSelfManagedAddons: aws.Bool(false),
		Tags:                       r.tags,
	}
	if r.auto {
		// Auto Mode turns on compute, load balancing and block storage
		// together, with its two built-in NodePools.
		//   https://docs.aws.amazon.com/eks/latest/userguide/set-builtin-node-pools.html
		in.ComputeConfig = &ekstypes.ComputeConfigRequest{Enabled: aws.Bool(true),
			NodePools: []string{"general-purpose", "system"}, NodeRoleArn: aws.String(r.roleARN(r.autoNodeRoleName()))}
		in.KubernetesNetworkConfig = &ekstypes.KubernetesNetworkConfigRequest{ElasticLoadBalancing: &ekstypes.ElasticLoadBalancing{Enabled: aws.Bool(true)}}
		in.StorageConfig = &ekstypes.StorageConfigRequest{BlockStorage: &ekstypes.BlockStorage{Enabled: aws.Bool(true)}}
	}
	return in
}

func (r *clusterRun) ensureCluster(ctx context.Context) error {
	name := r.e.names.Cluster
	if r.cluster == nil {
		in := r.createClusterInput()
		if err := r.create(fmt.Sprintf("EKS cluster %s (Kubernetes %s, Auto Mode %s)", name, r.cfg.KubernetesVersion, onOff(r.auto)), func() error {
			return retryNewRole(ctx, func() error {
				out, err := r.c.EKS.CreateCluster(ctx, in)
				if err == nil {
					r.cluster = out.Cluster
				}
				return err
			})
		}); err != nil {
			return fmt.Errorf("create cluster %s: %w", name, err)
		}
		if r.plan() {
			return nil
		}
	}
	if r.cluster.Status != ekstypes.ClusterStatusActive {
		if err := waitFor(ctx, "cluster "+name+" to be ACTIVE (about 10 minutes)", 30*time.Minute, func() (bool, error) {
			out, err := r.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
			if err != nil {
				return false, fmt.Errorf("describe cluster %s: %w", name, err)
			}
			r.cluster = out.Cluster
			switch out.Cluster.Status {
			case ekstypes.ClusterStatusActive:
				return true, nil
			case ekstypes.ClusterStatusCreating, ekstypes.ClusterStatusUpdating:
				return false, nil
			}
			return false, fmt.Errorf("cluster %s is %s", name, out.Cluster.Status)
		}); err != nil {
			return err
		}
	}
	vc := r.cluster.ResourcesVpcConfig
	if vc != nil && !slices.Equal(sortedCopy(vc.PublicAccessCidrs), sortedCopy(strings.Fields(r.cfg.PublicAccessCIDRs))) {
		slog.Warn("the cluster's public endpoint allows other address ranges than EKS_PUBLIC_ACCESS_CIDRS; change them with aws eks update-cluster-config --resources-vpc-config publicAccessCidrs=...",
			"cluster", strings.Join(vc.PublicAccessCidrs, " "), "configured", r.cfg.PublicAccessCIDRs)
	}
	// EKS makes the cluster security group, which every node uses, and
	// cannot tag it at create time. Only a cluster this tool created gets
	// the created-by tag on it; eksctl's keeps its own tags.
	if vc != nil && aws.ToString(vc.ClusterSecurityGroupId) != "" && r.cluster.Tags[CreatedByTagKey] == CreatedByTagValue {
		id := aws.ToString(vc.ClusterSecurityGroupId)
		out, err := r.c.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
		if err != nil {
			return fmt.Errorf("describe the cluster security group %s: %w", id, err)
		}
		for _, g := range out.SecurityGroups {
			if err := r.addMissingTags(ctx, "cluster security group", id, g.Tags); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// clusterAdminPolicy is the EKS access policy the caller gets.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/access-policy-permissions.html
func (r *clusterRun) clusterAdminPolicy() string {
	return fmt.Sprintf("arn:%s:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy", r.e.names.Partition)
}

// callerPrincipal is the IAM principal of the caller's credentials. An
// assumed-role session maps to its role, whose ARN (with any path, which
// access entries accept) comes from IAM.
func (r *clusterRun) callerPrincipal(ctx context.Context) (string, error) {
	id, err := r.e.c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("AWS credentials: %w", err)
	}
	arn := aws.ToString(id.Arn)
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", fmt.Errorf("unexpected caller ARN %q", r.mask(arn))
	}
	res := parts[5]
	switch {
	case parts[2] == "iam" && (strings.HasPrefix(res, "user/") || strings.HasPrefix(res, "role/")):
		return arn, nil
	case parts[2] == "sts" && strings.HasPrefix(res, "assumed-role/"):
		role, _, _ := strings.Cut(strings.TrimPrefix(res, "assumed-role/"), "/")
		out, err := r.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
		if err != nil {
			return "", fmt.Errorf("get the caller's role %s: %w", role, err)
		}
		return aws.ToString(out.Role.Arn), nil
	}
	return "", fmt.Errorf("the caller %s is not an IAM user or role, so it cannot get an access entry", r.mask(arn))
}

// mask hides the account id in what is logged (last four digits kept).
func (r *clusterRun) mask(s string) string {
	a := r.e.names.Account
	if len(a) < 4 {
		return s
	}
	return strings.ReplaceAll(s, a, "********"+a[len(a)-4:])
}

// ensureAccess gives the caller an access entry with AmazonEKSClusterAdminPolicy
// for the whole cluster, so the installer and kubectl can reach it.
func (r *clusterRun) ensureAccess(ctx context.Context) error {
	principal, err := r.callerPrincipal(ctx)
	if err != nil {
		return err
	}
	who := r.mask(principal)
	cluster := aws.String(r.e.names.Cluster)
	entry := true
	if r.cluster != nil {
		_, err := r.c.EKS.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{ClusterName: cluster, PrincipalArn: aws.String(principal)})
		switch {
		case isNotFound(err):
			entry = false
		case err != nil:
			return fmt.Errorf("describe the access entry of %s: %w", who, err)
		}
	} else {
		entry = false
	}
	if !entry {
		if err := r.create("access entry for "+who, func() error {
			_, err := r.c.EKS.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{ClusterName: cluster,
				PrincipalArn: aws.String(principal), Type: aws.String("STANDARD"), Tags: r.tags})
			return err
		}); err != nil {
			return fmt.Errorf("access entry of %s: %w", who, err)
		}
	}
	if entry {
		out, err := r.c.EKS.ListAssociatedAccessPolicies(ctx, &eks.ListAssociatedAccessPoliciesInput{ClusterName: cluster, PrincipalArn: aws.String(principal)})
		if err != nil {
			return fmt.Errorf("list the access policies of %s: %w", who, err)
		}
		for _, p := range out.AssociatedAccessPolicies {
			if aws.ToString(p.PolicyArn) == r.clusterAdminPolicy() && p.AccessScope != nil && p.AccessScope.Type == ekstypes.AccessScopeTypeCluster {
				slog.Info("cluster access ok", "principal", who)
				return nil
			}
		}
	}
	do := func() error {
		_, err := r.c.EKS.AssociateAccessPolicy(ctx, &eks.AssociateAccessPolicyInput{ClusterName: cluster,
			PrincipalArn: aws.String(principal), PolicyArn: aws.String(r.clusterAdminPolicy()),
			AccessScope: &ekstypes.AccessScope{Type: ekstypes.AccessScopeTypeCluster}})
		return err
	}
	what := "associate AmazonEKSClusterAdminPolicy with " + who
	if entry {
		err = r.change(what, do)
	} else {
		err = r.e.act(what, do)
	}
	if err != nil {
		return fmt.Errorf("access policy of %s: %w", who, err)
	}
	return nil
}
