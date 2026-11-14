package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strings"

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
	// inline are inline policies, name -> document.
	inline map[string]string
}

// volumeTagPolicyName is the Auto Mode cluster role's inline policy that
// lets EKS Auto Mode create EBS volumes with this deployment's tags.
const volumeTagPolicyName = "auto-mode-volume-tags"

// volumeTagPolicy is that policy. AmazonEKSBlockStoragePolicy allows
// ec2:CreateVolume only with Auto Mode's own tag keys (ForAllValues
// aws:TagKeys), so a StorageClass tagSpecification_N would be denied. This
// adds the one call, on volumes in this account and Region, requested for
// this cluster (eks:eks-cluster-name, the session tag EKS sets on the role)
// and carrying this deployment's owner tag. Tagging at create is already
// allowed by the managed policy (ec2:CreateTags with ec2:CreateAction).
//
//	https://docs.aws.amazon.com/eks/latest/userguide/auto-cluster-iam-role.html (Custom AWS tags for EKS Auto resources)
func (r *clusterRun) volumeTagPolicy() string {
	n := r.e.names
	doc := map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Sid": "CreateVolumesWithDeploymentTags", "Effect": "Allow", "Action": "ec2:CreateVolume",
		"Resource": fmt.Sprintf("arn:%s:ec2:%s:%s:volume/*", n.Partition, n.Region, n.Account),
		"Condition": map[string]any{"StringEquals": map[string]string{
			"aws:RequestTag/eks:eks-cluster-name": "${aws:PrincipalTag/eks:eks-cluster-name}",
			"aws:RequestTag/" + OwnerTagKey:       n.OwnerValue,
		}},
	}}}
	b, _ := json.Marshal(doc)
	return string(b)
}

// nodeTagPolicyName is the Auto Mode cluster role's inline policy that lets
// EKS Auto Mode launch nodes with this deployment's tags (a NodeClass whose
// spec.tags are the owner tag and TAGS, as the mode3-iso plugin's).
const nodeTagPolicyName = "auto-mode-node-tags"

// nodeTagPolicy is that policy: the documented compute and networking
// statements for user defined tags. AmazonEKSComputePolicy and
// AmazonEKSNetworkingPolicy allow these creates only with Auto Mode's own tag
// keys, so a tagged NodeClass fails validation ("Controller isn't authorized
// to call ec2:CreateLaunchTemplate") and launches no node. Like
// volumeTagPolicy, each statement covers only this account and Region's
// resources of the kinds the call creates, requested for this cluster and
// carrying this deployment's owner tag; the other resources a launch names
// (subnets, security groups, images) stay with the managed policies.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/auto-cluster-iam-role.html (Custom AWS tags for EKS Auto resources)
func (r *clusterRun) nodeTagPolicy() string {
	n := r.e.names
	arn := func(kinds ...string) []string {
		var out []string
		for _, k := range kinds {
			out = append(out, fmt.Sprintf("arn:%s:ec2:%s:%s:%s/*", n.Partition, n.Region, n.Account, k))
		}
		return out
	}
	cond := func(like map[string]string) map[string]any {
		return map[string]any{
			"StringEquals": map[string]string{
				"aws:RequestTag/eks:eks-cluster-name": "${aws:PrincipalTag/eks:eks-cluster-name}",
				"aws:RequestTag/" + OwnerTagKey:       n.OwnerValue,
			},
			"StringLike": like,
		}
	}
	doc := map[string]any{"Version": "2012-10-17", "Statement": []any{
		map[string]any{"Sid": "LaunchNodesWithDeploymentTags", "Effect": "Allow",
			"Action":   []string{"ec2:CreateFleet", "ec2:RunInstances", "ec2:CreateLaunchTemplate"},
			"Resource": arn("fleet", "instance", "launch-template", "network-interface", "volume"),
			"Condition": cond(map[string]string{"aws:RequestTag/eks:kubernetes-node-class-name": "*",
				"aws:RequestTag/eks:kubernetes-node-pool-name": "*"})},
		map[string]any{"Sid": "NodeInterfacesWithDeploymentTags", "Effect": "Allow",
			"Action": "ec2:CreateNetworkInterface", "Resource": arn("network-interface"),
			"Condition": cond(map[string]string{"aws:RequestTag/eks:kubernetes-cni-node-name": "*"})},
	}}
	b, _ := json.Marshal(doc)
	return string(b)
}

// autoInline are the Auto Mode cluster role's inline policies.
func (r *clusterRun) autoInline() map[string]string {
	return map[string]string{volumeTagPolicyName: r.volumeTagPolicy(), nodeTagPolicyName: r.nodeTagPolicy()}
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
// complete deployment needs none; a rerun then reads only an Auto Mode
// cluster's role, for its inline policies (reconcileClusterRole).
func (r *clusterRun) neededRoles() []clusterRole {
	var out []clusterRole
	if r.cluster == nil {
		p := []string{"AmazonEKSClusterPolicy"}
		if r.auto {
			p = append(p, "AmazonEKSComputePolicy", "AmazonEKSBlockStoragePolicy", "AmazonEKSLoadBalancingPolicy", "AmazonEKSNetworkingPolicy")
		}
		cr := clusterRole{name: r.clusterRoleName(), trust: eksTrustPolicy, policies: p}
		if r.auto {
			cr.inline = r.autoInline()
		}
		out = append(out, cr)
		if r.auto {
			out = append(out, clusterRole{name: r.autoNodeRoleName(), trust: ec2TrustPolicy,
				policies: []string{"AmazonEKSWorkerNodeMinimalPolicy", "AmazonEC2ContainerRegistryPullOnly"}})
		}
	}
	for _, g := range r.groups() {
		if r.nodegroups[g.Name] == nil {
			out = append(out, clusterRole{name: r.nodeRoleName(), trust: ec2TrustPolicy,
				policies: []string{"AmazonEKSWorkerNodePolicy", "AmazonEKS_CNI_Policy", "AmazonEC2ContainerRegistryPullOnly"}})
			break
		}
	}
	if r.addons["aws-ebs-csi-driver"] == nil {
		// Only EKS Pod Identity, for this cluster, may assume it (iam.go).
		out = append(out, clusterRole{name: r.ebsRoleName(), trust: r.e.trustPolicy(), policies: []string{"service-role/AmazonEBSCSIDriverPolicy"}})
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
	return r.reconcileClusterRole(ctx)
}

// reconcileClusterRole puts a missing or different inline policy of an
// existing Auto Mode cluster's role, so a rerun brings a cluster made by an
// older setup-aws up to date. Only a role carrying this deployment's owner
// tag is changed; any other is refused (logged and left alone), since the
// cluster may be one setup-aws did not create (create-cluster.sh's).
func (r *clusterRun) reconcileClusterRole(ctx context.Context) error {
	if r.cluster == nil || !r.auto {
		return nil
	}
	arn := aws.ToString(r.cluster.RoleArn)
	role := arn[strings.LastIndex(arn, "/")+1:]
	if role == "" {
		return nil
	}
	got, err := r.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	if isNotFound(err) {
		slog.Warn("the cluster's IAM role is gone; its inline policies are not reconciled", "role", role)
		return nil
	}
	if err != nil {
		return fmt.Errorf("IAM role %s: get role: %w", role, err)
	}
	tags, err := r.c.IAM.ListRoleTags(ctx, &iam.ListRoleTagsInput{RoleName: got.Role.RoleName})
	if err != nil {
		return fmt.Errorf("IAM role %s: read tags: %w", role, err)
	}
	owner := ""
	for _, t := range tags.Tags {
		if aws.ToString(t.Key) == OwnerTagKey {
			owner = aws.ToString(t.Value)
		}
	}
	if owner != r.e.names.OwnerValue {
		slog.Warn("refused: the cluster's IAM role is not owned by this deployment, so its inline policies are left alone (tagged NodeClasses and StorageClasses get no deployment tags)",
			"role", role, "owner", owner, "want", r.e.names.OwnerValue)
		return nil
	}
	if err := r.putInline(ctx, role, r.autoInline(), true); err != nil {
		return fmt.Errorf("IAM role %s: %w", role, err)
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
	return r.putInline(ctx, role.name, role.inline, exists)
}

// putInline puts each inline policy of role; on an existing role only one
// that is missing or different, as a change.
func (r *clusterRun) putInline(ctx context.Context, role string, inline map[string]string, exists bool) error {
	for _, name := range sortedKeys(inline) {
		doc := inline[name]
		if exists {
			got, err := r.c.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(name)})
			if err != nil && !isNotFound(err) {
				return fmt.Errorf("get inline policy %s: %w", name, err)
			}
			if err == nil && sameJSON(decodedPolicy(aws.ToString(got.PolicyDocument)), doc) {
				continue
			}
		}
		do := func() error {
			_, err := r.c.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(name), PolicyDocument: aws.String(doc)})
			return err
		}
		what := "put inline policy " + name + " on " + role
		var err error
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
