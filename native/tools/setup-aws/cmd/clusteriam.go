package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// clusterRole is one IAM role of the cluster, with the AWS managed policies
// EKS documents for it.
//   - cluster: https://docs.aws.amazon.com/eks/latest/userguide/cluster-iam-role.html
//     (Auto Mode adds the compute, block storage, load balancing and
//     networking policies: https://docs.aws.amazon.com/eks/latest/userguide/auto-cluster-iam-role.html)
//   - Auto Mode nodes: https://docs.aws.amazon.com/eks/latest/userguide/auto-create-node-role.html
//   - managed node groups: https://docs.aws.amazon.com/eks/latest/userguide/create-node-role.html
//   - EBS CSI driver (EKS Pod Identity): https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html
//
// Auto Mode nodes and the node groups use two roles: EKS gives each node
// role an access entry, of type EC2 for Auto Mode and EC2_LINUX for node
// groups, and one role can have only one.
type clusterRole struct {
	name, trust string
	policies    []string // under arn:<partition>:iam::aws:policy/
}

// shortcut: the service principals are the aws partition's; aws-cn spells
// ec2's differently. Upgrade if this tool is used in China Regions.
const (
	eksTrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`
	ec2TrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
)

func (r *clusterRun) roleName(part string) string { return r.e.names.IAMPrefix + "-" + part }

func (r *clusterRun) roleARN(name string) string {
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", r.e.names.Partition, r.e.names.Account, name)
}

func (r *clusterRun) clusterRoleName() string  { return r.roleName("eks-cluster") }
func (r *clusterRun) autoNodeRoleName() string { return r.roleName("eks-auto-node") }
func (r *clusterRun) nodeRoleName() string     { return r.roleName("eks-node") }
func (r *clusterRun) ebsRoleName() string      { return r.roleName("ebs-csi-driver") }

// neededRoles are the roles of what this run still has to create: the
// cluster (and Auto Mode's node role), a node group, the EBS CSI add-on. A
// complete deployment needs none, so a rerun reads no IAM at all.
func (r *clusterRun) neededRoles() []clusterRole {
	var out []clusterRole
	if r.cluster == nil {
		p := []string{"AmazonEKSClusterPolicy"}
		if r.auto {
			p = append(p, "AmazonEKSComputePolicy", "AmazonEKSBlockStoragePolicy", "AmazonEKSLoadBalancingPolicy", "AmazonEKSNetworkingPolicy")
		}
		out = append(out, clusterRole{r.clusterRoleName(), eksTrustPolicy, p})
		if r.auto {
			out = append(out, clusterRole{r.autoNodeRoleName(), ec2TrustPolicy,
				[]string{"AmazonEKSWorkerNodeMinimalPolicy", "AmazonEC2ContainerRegistryPullOnly"}})
		}
	}
	for _, g := range r.groups() {
		if r.nodegroups[g.Name] == nil {
			out = append(out, clusterRole{r.nodeRoleName(), ec2TrustPolicy,
				[]string{"AmazonEKSWorkerNodePolicy", "AmazonEKS_CNI_Policy", "AmazonEC2ContainerRegistryPullOnly"}})
			break
		}
	}
	if r.addons["aws-ebs-csi-driver"] == nil {
		// Only EKS Pod Identity, for this cluster, may assume it (iam.go).
		out = append(out, clusterRole{r.ebsRoleName(), r.e.trustPolicy(), []string{"service-role/AmazonEBSCSIDriverPolicy"}})
	}
	return out
}

func (r *clusterRun) ensureRoles(ctx context.Context) error {
	roles := r.neededRoles()
	if len(roles) == 0 {
		slog.Info("no IAM role needed: the cluster, node groups and add-ons exist")
	}
	for _, role := range roles {
		if err := r.ensureRole(ctx, role); err != nil {
			return fmt.Errorf("IAM role %s: %w", role.name, err)
		}
	}
	return nil
}

// ensureRole creates the role, or reconciles its trust policy and managed
// policies when it exists with this deployment's owner tag. ownedRole (iam.go)
// refuses a role without it.
func (r *clusterRun) ensureRole(ctx context.Context, role clusterRole) error {
	exists, err := r.e.ownedRole(ctx, role.name)
	if err != nil {
		return err
	}
	attached := map[string]bool{}
	if !exists {
		tags := make([]iamtypes.Tag, 0, len(r.tags))
		for _, k := range sortedKeys(r.tags) {
			tags = append(tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(r.tags[k])})
		}
		if err := r.create("IAM role "+role.name, func() error {
			_, err := r.c.IAM.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(role.name),
				AssumeRolePolicyDocument: aws.String(role.trust), Tags: tags,
				Description: aws.String("Agent Substrate on EKS: " + r.e.names.OwnerValue)})
			return err
		}); err != nil {
			return err
		}
	} else {
		got, err := r.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role.name)})
		if err != nil {
			return fmt.Errorf("get role: %w", err)
		}
		if !sameJSON(decodedPolicy(aws.ToString(got.Role.AssumeRolePolicyDocument)), role.trust) {
			if err := r.change("update the trust policy of "+role.name, func() error {
				_, err := r.c.IAM.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(role.name), PolicyDocument: aws.String(role.trust)})
				return err
			}); err != nil {
				return err
			}
		}
		p := iam.NewListAttachedRolePoliciesPaginator(r.c.IAM, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(role.name)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return fmt.Errorf("list attached policies: %w", err)
			}
			for _, a := range page.AttachedPolicies {
				attached[aws.ToString(a.PolicyArn)] = true
			}
		}
	}
	for _, name := range role.policies {
		arn := fmt.Sprintf("arn:%s:iam::aws:policy/%s", r.e.names.Partition, name)
		if attached[arn] {
			continue
		}
		do := func() error {
			_, err := r.c.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: aws.String(role.name), PolicyArn: aws.String(arn)})
			return err
		}
		what := "attach " + name + " to " + role.name
		// A new role's attachments are part of creating it; on an existing
		// role a missing one is a change.
		if exists {
			err = r.change(what, do)
		} else {
			err = r.e.act(what, do)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// decodedPolicy undoes the URL encoding IAM returns policy documents in.
func decodedPolicy(doc string) string {
	if d, err := url.QueryUnescape(doc); err == nil {
		return d
	}
	return doc
}

// sameJSON compares two JSON documents by value, not by spelling.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
