package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// KARPENTER=on (nodegroups.CheckKarpenter is the rule). create cluster makes
// the gvisor group as a Karpenter NodePool (nodegroups.KarpenterPools); the
// microvm and nitro groups stay node groups, unless KARPENTER_NODE_OVERLAY=on
// (nodegroups.CheckNodeOverlay), which makes them NodePools too, each with a
// NodeOverlay (karpenter_overlay.go). That is two more phases after the node
// groups: Karpenter's AWS resources (this file), then Karpenter and its
// NodePools in the cluster (karpenter_kube.go). The resources are:
//
//   - the controller's IAM role <cluster>-<region>-karpenter, with the
//     Karpenter reference policy for this cluster as its inline policy, and
//     its EKS Pod Identity association for kube-system/karpenter;
//   - the node role's instance profile and EC2_LINUX access entry, which
//     self-managed node groups use too (selfmanaged.go);
//   - the interruption queue <cluster>-karpenter and five EventBridge rules
//     <cluster>-karpenter-<kind> that send it EC2 and AWS Health events.
//
// A cluster made with KARPENTER=on carries the tag KarpenterTagKey=on, or
// =node-overlay with KARPENTER_NODE_OVERLAY=on, and a run whose KARPENTER or
// KARPENTER_NODE_OVERLAY does not match the cluster is refused.
//
//	https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/
//	https://karpenter.sh/docs/reference/cloudformation/

// The controller's ServiceAccount, which the chart makes, and the name of
// the queue target on each rule (the reference's).
const (
	karpenterNamespace      = "kube-system"
	karpenterServiceAccount = "karpenter"
	karpenterQueueTarget    = "KarpenterInterruptionQueueTarget"
)

// karpenterRule is one EventBridge rule that sends interruption events to
// the queue: the reference's five, by the suffix of the rule's name.
type karpenterRule struct{ suffix, source, detailType string }

var karpenterRules = []karpenterRule{
	{"health", "aws.health", "AWS Health Event"},
	{"spot", "aws.ec2", "EC2 Spot Instance Interruption Warning"},
	{"rebalance", "aws.ec2", "EC2 Instance Rebalance Recommendation"},
	{"state-change", "aws.ec2", "EC2 Instance State-change Notification"},
	{"capacity-reservation", "aws.ec2", "EC2 Capacity Reservation Instance Interruption Warning"},
}

// pattern is the rule's event pattern, as the reference has it.
func (k karpenterRule) pattern() string {
	b, _ := json.Marshal(map[string][]string{"source": {k.source}, "detail-type": {k.detailType}})
	return string(b)
}

// Derived names: the queue and rules (EventBridge allows 64 characters: 21
// for the cluster, 11 for -karpenter-, 20 for the longest suffix), and the
// controller role.
func karpenterQueueName(cluster string) string { return cluster + "-karpenter" }
func karpenterRuleName(cluster, suffix string) string {
	return cluster + "-karpenter-" + suffix
}
func karpenterRoleName(n Names) string { return n.IAMPrefix + "-" + karpenterServiceAccount }

func karpenterRuleNames(cluster string) []string {
	var out []string
	for _, k := range karpenterRules {
		out = append(out, karpenterRuleName(cluster, k.suffix))
	}
	return out
}

func karpenterQueueARN(n Names) string {
	return fmt.Sprintf("arn:%s:sqs:%s:%s:%s", n.Partition, n.Region, n.Account, karpenterQueueName(n.Cluster))
}

// validateKarpenter refuses a KARPENTER or KARPENTER_NODE_OVERLAY the rules
// do not allow, a TAGS that sets a reserved key, and a NodePool without a
// node limit.
func (r *clusterRun) validateKarpenter() error {
	if err := nodegroups.CheckKarpenter(r.cfg.Karpenter, r.auto, r.wanted); err != nil {
		return err
	}
	if err := nodegroups.CheckNodeOverlay(r.cfg.Karpenter, r.cfg.KarpenterNodeOverlay); err != nil {
		return err
	}
	if err := nodegroups.CheckTags(r.e.cfg.Tags, r.karpenter); err != nil {
		return err
	}
	for _, p := range r.pools() {
		if p.Desired < 1 {
			return fmt.Errorf("the %s needs a limit of at least one node (%s_NODES)", p.noun(), strings.ToUpper(p.Name))
		}
	}
	return nil
}

// marker is the value of KarpenterTagKey this run's cluster carries: "on",
// "node-overlay", or "" without Karpenter.
func (r *clusterRun) marker() string { return nodegroups.KarpenterMarker(r.karpenter, r.nodeOverlay) }

// checkKarpenterMarker refuses an existing cluster whose marker does not
// match KARPENTER and KARPENTER_NODE_OVERLAY: both are set when the cluster
// is made.
func (r *clusterRun) checkKarpenterMarker(c *ekstypes.Cluster) error {
	have, want := c.Tags[nodegroups.KarpenterTagKey], r.marker()
	name := r.e.names.Cluster
	known := knownKarpenterMarker(have)
	switch {
	case have == want:
		return nil
	case !known && want == "":
		// Only the values setup-aws sets mark a cluster, as before
		// KARPENTER_NODE_OVERLAY (karpenterMarked too).
		return nil
	case !known && have != "":
		return fmt.Errorf("cluster %s carries %s=%q, which setup-aws does not set; %s", name, nodegroups.KarpenterTagKey, have, recreate)
	case have == "":
		return fmt.Errorf("cluster %s was made without Karpenter, and KARPENTER=on applies to a new cluster only; set KARPENTER to off or empty to match, or %s",
			name, recreate)
	case want == "" && have == nodegroups.KarpenterMarkerNodeOverlay:
		return fmt.Errorf("cluster %s was made with KARPENTER=on and KARPENTER_NODE_OVERLAY=on, but KARPENTER=%q; set KARPENTER=on and KARPENTER_NODE_OVERLAY=on to match, or %s",
			name, r.cfg.Karpenter, recreate)
	case want == "":
		return fmt.Errorf("cluster %s was made with KARPENTER=on, but KARPENTER=%q; set KARPENTER=on to match, or %s",
			name, r.cfg.Karpenter, recreate)
	case have == nodegroups.KarpenterMarkerNodeOverlay:
		return fmt.Errorf("cluster %s was made with KARPENTER_NODE_OVERLAY=on, but KARPENTER_NODE_OVERLAY=%q; set KARPENTER_NODE_OVERLAY=on to match, or %s",
			name, r.cfg.KarpenterNodeOverlay, recreate)
	}
	return fmt.Errorf("cluster %s was made without KARPENTER_NODE_OVERLAY, which applies to a new cluster only; set KARPENTER_NODE_OVERLAY to off or empty to match, or %s",
		name, recreate)
}

// clusterTags are the tags of a new cluster: r.tags, and with KARPENTER=on
// the marker.
func (r *clusterRun) clusterTags() map[string]string {
	if !r.karpenter {
		return r.tags
	}
	m := map[string]string{nodegroups.KarpenterTagKey: r.marker()}
	for k, v := range r.tags {
		m[k] = v
	}
	return m
}

// nodeKinds names what uses the node role's access entry, for messages.
func (r *clusterRun) nodeKinds() string {
	switch {
	case r.karpenter && r.selfManaged():
		return "self-managed and Karpenter nodes"
	case r.karpenter:
		return "Karpenter nodes"
	}
	return "self-managed nodes"
}

// kubeVersion is the cluster's Kubernetes version, or KUBERNETES_VERSION
// for a new cluster.
func (r *clusterRun) kubeVersion() string { return r.nitroVersion() }

// karpenterPolicy is the controller's inline policy: the statements of the
// Karpenter 1.15 reference CloudFormation template (NodeLifecyclePolicy,
// IAMIntegrationPolicy, EKSIntegrationPolicy, InterruptionPolicy and
// ResourceDiscoveryPolicy) for this cluster, Region, account and partition,
// as they are, but for two removals: the statements that let Karpenter
// create, tag, change and delete instance profiles (each EC2NodeClass names
// the node role's instance profile, which create cluster makes), and the
// zonal shift statement (enableZonalShift is off). iam:PassRole is on the
// node role only.
func karpenterPolicy(n Names, nodeRoleARN string) string {
	p, r, a, c := n.Partition, n.Region, n.Account, n.Cluster
	ec2 := func(kinds ...string) []string {
		var out []string
		for _, k := range kinds {
			out = append(out, fmt.Sprintf("arn:%s:ec2:%s:*:%s/*", p, r, k))
		}
		return out
	}
	tagged := []string{"fleet", "instance", "volume", "network-interface", "launch-template", "spot-instances-request"}
	cluster := "kubernetes.io/cluster/" + c
	stmt := func(sid string, resource, action any, cond map[string]any) map[string]any {
		s := map[string]any{"Sid": sid, "Effect": "Allow", "Resource": resource, "Action": action}
		if cond != nil {
			s["Condition"] = cond
		}
		return s
	}
	doc := map[string]any{"Version": "2012-10-17", "Statement": []any{
		// NodeLifecyclePolicy
		stmt("AllowScopedEC2InstanceAccessActions", []string{
			fmt.Sprintf("arn:%s:ec2:%s::image/*", p, r), fmt.Sprintf("arn:%s:ec2:%s::snapshot/*", p, r),
			fmt.Sprintf("arn:%s:ec2:%s:*:security-group/*", p, r), fmt.Sprintf("arn:%s:ec2:%s:*:subnet/*", p, r),
			fmt.Sprintf("arn:%s:ec2:%s:*:capacity-reservation/*", p, r), fmt.Sprintf("arn:%s:ec2:%s:*:placement-group/*", p, r),
		}, []string{"ec2:RunInstances", "ec2:CreateFleet"}, nil),
		stmt("AllowScopedEC2LaunchTemplateAccessActions", fmt.Sprintf("arn:%s:ec2:%s:*:launch-template/*", p, r),
			[]string{"ec2:RunInstances", "ec2:CreateFleet"}, map[string]any{
				"StringEquals": map[string]string{"aws:ResourceTag/" + cluster: "owned"},
				"StringLike":   map[string]string{"aws:ResourceTag/karpenter.sh/nodepool": "*"}}),
		stmt("AllowScopedEC2InstanceActionsWithTags", ec2(tagged...),
			[]string{"ec2:RunInstances", "ec2:CreateFleet", "ec2:CreateLaunchTemplate"}, map[string]any{
				"StringEquals": map[string]string{"aws:RequestTag/" + cluster: "owned", "aws:RequestTag/eks:eks-cluster-name": c},
				"StringLike":   map[string]string{"aws:RequestTag/karpenter.sh/nodepool": "*"}}),
		stmt("AllowScopedResourceCreationTagging", ec2(tagged...), "ec2:CreateTags", map[string]any{
			"StringEquals": map[string]any{"aws:RequestTag/" + cluster: "owned", "aws:RequestTag/eks:eks-cluster-name": c,
				"ec2:CreateAction": []string{"RunInstances", "CreateFleet", "CreateLaunchTemplate"}},
			"StringLike": map[string]string{"aws:RequestTag/karpenter.sh/nodepool": "*"}}),
		stmt("AllowScopedResourceTagging", fmt.Sprintf("arn:%s:ec2:%s:*:instance/*", p, r), "ec2:CreateTags", map[string]any{
			"StringEquals":              map[string]string{"aws:ResourceTag/" + cluster: "owned"},
			"StringLike":                map[string]string{"aws:ResourceTag/karpenter.sh/nodepool": "*"},
			"StringEqualsIfExists":      map[string]string{"aws:RequestTag/eks:eks-cluster-name": c},
			"ForAllValues:StringEquals": map[string][]string{"aws:TagKeys": {"eks:eks-cluster-name", "karpenter.sh/nodeclaim", "Name"}}}),
		stmt("AllowScopedDeletion", ec2("instance", "launch-template"), []string{"ec2:TerminateInstances", "ec2:DeleteLaunchTemplate"}, map[string]any{
			"StringEquals": map[string]string{"aws:ResourceTag/" + cluster: "owned"},
			"StringLike":   map[string]string{"aws:ResourceTag/karpenter.sh/nodepool": "*"}}),
		// IAMIntegrationPolicy, without its instance profile statements
		stmt("AllowPassingInstanceRole", nodeRoleARN, "iam:PassRole", map[string]any{
			"StringEquals": map[string][]string{"iam:PassedToService": {"ec2.amazonaws.com", "ec2.amazonaws.com.cn"}}}),
		// EKSIntegrationPolicy
		stmt("AllowAPIServerEndpointDiscovery", fmt.Sprintf("arn:%s:eks:%s:%s:cluster/%s", p, r, a, c), "eks:DescribeCluster", nil),
		// InterruptionPolicy
		stmt("AllowInterruptionQueueActions", karpenterQueueARN(n), []string{"sqs:DeleteMessage", "sqs:GetQueueUrl", "sqs:ReceiveMessage"}, nil),
		// ResourceDiscoveryPolicy
		stmt("AllowRegionalReadActions", "*", []string{"ec2:DescribeCapacityReservations", "ec2:DescribeImages", "ec2:DescribeInstances",
			"ec2:DescribeInstanceStatus", "ec2:DescribeInstanceTypeOfferings", "ec2:DescribeInstanceTypes", "ec2:DescribeLaunchTemplates",
			"ec2:DescribePlacementGroups", "ec2:DescribeSecurityGroups", "ec2:DescribeSpotPriceHistory", "ec2:DescribeSubnets"},
			map[string]any{"StringEquals": map[string]string{"aws:RequestedRegion": r}}),
		stmt("AllowSSMReadActions", fmt.Sprintf("arn:%s:ssm:%s::parameter/aws/service/*", p, r), "ssm:GetParameter", nil),
		stmt("AllowPricingReadActions", "*", "pricing:GetProducts", nil),
		stmt("AllowUnscopedInstanceProfileListAction", "*", "iam:ListInstanceProfiles", nil),
		stmt("AllowInstanceProfileReadActions", fmt.Sprintf("arn:%s:iam::%s:instance-profile/*", p, a), "iam:GetInstanceProfile", nil),
	}}
	b, _ := json.Marshal(doc)
	return string(b)
}

// karpenterQueuePolicy is the interruption queue's policy, as the reference
// has it: EventBridge (and SQS) may send messages to it, and nothing may use
// it without TLS.
func karpenterQueuePolicy(n Names) string {
	arn := karpenterQueueARN(n)
	doc := map[string]any{"Version": "2012-10-17", "Id": "EC2InterruptionPolicy", "Statement": []any{
		map[string]any{"Effect": "Allow", "Principal": map[string][]string{"Service": {"events.amazonaws.com", "sqs.amazonaws.com"}},
			"Action": "sqs:SendMessage", "Resource": arn},
		map[string]any{"Sid": "DenyHTTP", "Effect": "Deny", "Action": "sqs:*", "Resource": arn,
			"Condition": map[string]any{"Bool": map[string]string{"aws:SecureTransport": "false"}}, "Principal": "*"},
	}}
	b, _ := json.Marshal(doc)
	return string(b)
}

// karpenterQueueAttributes are the queue's settings: messages kept 5
// minutes, SSE-SQS encryption, and the policy.
func karpenterQueueAttributes(n Names) map[string]string {
	return map[string]string{
		string(sqstypes.QueueAttributeNameMessageRetentionPeriod): "300",
		string(sqstypes.QueueAttributeNameSqsManagedSseEnabled):   "true",
		string(sqstypes.QueueAttributeNamePolicy):                 karpenterQueuePolicy(n),
	}
}

// --- phase 10: Karpenter's AWS resources ----------------------------------------

// ensureKarpenterAWS makes, or checks and reconciles, Karpenter's AWS
// resources: the node role's access entry (EKS made one already for a
// managed group's role, which is kept; the access phase made it for
// self-managed groups), the controller role and its association, the
// queue, then the rules that target it.
func (r *clusterRun) ensureKarpenterAWS(ctx context.Context) error {
	switch {
	case r.selfManaged() && len(r.groups()) > 0:
	case r.plan() && r.managedGroupToMake():
		// --plan makes no node group, so the entry is not there yet; the
		// real run finds the one EKS makes with them.
		slog.Info("plan: EKS makes the node role's EC2_LINUX access entry with the managed node groups")
	default:
		if err := r.nodeAccessEntry(ctx); err != nil {
			return err
		}
	}
	if err := r.ensureKarpenterRole(ctx); err != nil {
		return err
	}
	if err := r.ensureKarpenterQueue(ctx); err != nil {
		return err
	}
	return r.ensureKarpenterRules(ctx)
}

// managedGroupToMake reports a managed node group this run is still to
// make: EKS gives the node role its EC2_LINUX access entry when it makes one.
func (r *clusterRun) managedGroupToMake() bool {
	if r.selfManaged() {
		return false
	}
	for _, g := range r.groups() {
		if !r.groupExists(g.Name) {
			return true
		}
	}
	return false
}

// ensureKarpenterRole is roleForSA's work for kube-system/karpenter, with
// each write counted for --plan and nothing written when nothing differs:
// the association must be this deployment's (ownedAssociation) before
// anything is written; the role, its trust (EKS Pod Identity for this
// cluster only) and its inline policy access are made or reconciled
// (ensureRole); then the association is made, or repointed at the role.
func (r *clusterRun) ensureKarpenterRole(ctx context.Context) error {
	role := karpenterRoleName(r.e.names)
	sa := karpenterNamespace + "/" + karpenterServiceAccount
	// A cluster this run is still to make (--plan) has no association.
	var assoc *ekstypes.PodIdentityAssociation
	if r.cluster != nil {
		var err error
		if assoc, err = r.e.ownedAssociation(ctx, karpenterNamespace, karpenterServiceAccount); err != nil {
			return err
		}
	}
	cr := clusterRole{name: role, trust: r.e.trustPolicy(),
		inline: map[string]string{"access": karpenterPolicy(r.e.names, r.roleARN(r.nodeRoleName()))}}
	if err := r.ensureRole(ctx, cr); err != nil {
		return fmt.Errorf("IAM role %s: %w", role, err)
	}
	arn := r.roleARN(role)
	cluster := aws.String(r.e.names.Cluster)
	var err error
	switch {
	case assoc == nil:
		err = r.create("Pod Identity association "+sa+" with "+role, func() error {
			return retryNewRole(ctx, func() error {
				_, err := r.e.c.EKS.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{ClusterName: cluster,
					Namespace: aws.String(karpenterNamespace), ServiceAccount: aws.String(karpenterServiceAccount),
					RoleArn: aws.String(arn), Tags: r.tags})
				return err
			})
		})
	case aws.ToString(assoc.RoleArn) != arn:
		slog.Warn("association points at another role; repointing it", "sa", sa, "was", r.mask(aws.ToString(assoc.RoleArn)))
		err = r.change("repoint association "+aws.ToString(assoc.AssociationId)+" at "+role, func() error {
			_, err := r.e.c.EKS.UpdatePodIdentityAssociation(ctx, &eks.UpdatePodIdentityAssociationInput{ClusterName: cluster,
				AssociationId: assoc.AssociationId, RoleArn: aws.String(arn)})
			return err
		})
	default:
		slog.Info("pod identity ok", "sa", sa, "role", role)
	}
	if err != nil {
		return fmt.Errorf("association of %s: %w", sa, err)
	}
	return nil
}

// sqsQueueMissing reports SQS's answer for a queue that does not exist. Any
// other failed read is an error.
func sqsQueueMissing(err error) bool {
	var qe *sqstypes.QueueDoesNotExist
	return errors.As(err, &qe) || hasCode(err, "QueueDoesNotExist", "AWS.SimpleQueueService.NonExistentQueue")
}

// queueAPI reads an SQS queue: the clients of create cluster and teardown.
type queueAPI interface {
	GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	ListQueueTags(context.Context, *sqs.ListQueueTagsInput, ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error)
}

// readQueue is the URL and tags of queue name in this account, or "" when
// it does not exist.
func readQueue(ctx context.Context, c queueAPI, account, name string) (string, map[string]string, error) {
	out, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name), QueueOwnerAWSAccountId: aws.String(account)})
	if sqsQueueMissing(err) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("get the URL of SQS queue %s: %w", name, err)
	}
	url := aws.ToString(out.QueueUrl)
	t, err := c.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(url)})
	if sqsQueueMissing(err) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("read the tags of SQS queue %s: %w", name, err)
	}
	if t.Tags == nil {
		return url, map[string]string{}, nil
	}
	return url, t.Tags, nil
}

// ensureKarpenterQueue makes the interruption queue with its settings and
// tags, or, when it exists with this deployment's owner tag, sets the
// settings that differ. One without the owner tag is never used.
func (r *clusterRun) ensureKarpenterQueue(ctx context.Context) error {
	name := karpenterQueueName(r.e.names.Cluster)
	want := karpenterQueueAttributes(r.e.names)
	url, tags, err := readQueue(ctx, r.c.SQS, r.e.names.Account, name)
	if err != nil {
		return err
	}
	if url == "" {
		return r.create("SQS queue "+name, func() error {
			_, err := r.c.SQS.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: want, Tags: r.tags})
			if err != nil {
				return fmt.Errorf("create SQS queue %s: %w", name, err)
			}
			return nil
		})
	}
	if owner := tags[OwnerTagKey]; owner != r.e.names.OwnerValue {
		return fmt.Errorf("SQS queue %s exists but is not owned by %s (%s=%q); use another CLUSTER_NAME", name, r.e.names.OwnerValue, OwnerTagKey, owner)
	}
	var names []sqstypes.QueueAttributeName
	for _, k := range sortedKeys(want) {
		names = append(names, sqstypes.QueueAttributeName(k))
	}
	got, err := r.c.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url), AttributeNames: names})
	if err != nil {
		return fmt.Errorf("read the attributes of SQS queue %s: %w", name, err)
	}
	set := map[string]string{}
	for _, k := range sortedKeys(want) {
		have := got.Attributes[k]
		same := have == want[k]
		if k == string(sqstypes.QueueAttributeNamePolicy) {
			same = sameJSON(have, want[k])
		}
		if !same {
			set[k] = want[k]
		}
	}
	if len(set) == 0 {
		slog.Info("SQS queue ok", "queue", name)
		return nil
	}
	return r.change(fmt.Sprintf("set %s of SQS queue %s", strings.Join(sortedKeys(set), ", "), name), func() error {
		_, err := r.c.SQS.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{QueueUrl: aws.String(url), Attributes: set})
		return err
	})
}

// ensureKarpenterRules makes each EventBridge rule with its pattern and
// tags and its target, the queue; or, for a rule that exists with this
// deployment's owner tag, puts the pattern and the target when they
// differ. A rule without the owner tag is never used.
func (r *clusterRun) ensureKarpenterRules(ctx context.Context) error {
	queue := karpenterQueueARN(r.e.names)
	target := []ebtypes.Target{{Id: aws.String(karpenterQueueTarget), Arn: aws.String(queue)}}
	var tags []ebtypes.Tag
	for _, k := range sortedKeys(r.tags) {
		tags = append(tags, ebtypes.Tag{Key: aws.String(k), Value: aws.String(r.tags[k])})
	}
	for _, k := range karpenterRules {
		name := karpenterRuleName(r.e.names.Cluster, k.suffix)
		put := &eventbridge.PutRuleInput{Name: aws.String(name), EventPattern: aws.String(k.pattern()), State: ebtypes.RuleStateEnabled,
			Description: aws.String("Karpenter interruption events for " + r.e.names.OwnerValue)}
		putTargets := func() error {
			out, err := r.c.Events.PutTargets(ctx, &eventbridge.PutTargetsInput{Rule: aws.String(name), Targets: target})
			if err == nil && out.FailedEntryCount > 0 {
				err = fmt.Errorf("%d targets not put, first: %s", out.FailedEntryCount, aws.ToString(out.FailedEntries[0].ErrorMessage))
			}
			return err
		}
		d, err := r.c.Events.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String(name)})
		if isNotFound(err) {
			put.Tags = tags
			if err := r.create("EventBridge rule "+name, func() error {
				_, err := r.c.Events.PutRule(ctx, put)
				return err
			}); err != nil {
				return fmt.Errorf("EventBridge rule %s: %w", name, err)
			}
			// The target is part of making the rule.
			if err := r.e.act("send rule "+name+" to the queue", putTargets); err != nil {
				return fmt.Errorf("EventBridge rule %s: %w", name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("describe EventBridge rule %s: %w", name, err)
		}
		t, err := r.c.Events.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: d.Arn})
		if err != nil {
			return fmt.Errorf("read the tags of EventBridge rule %s: %w", name, err)
		}
		if owner := kvMap(t.Tags, ebKV)[OwnerTagKey]; owner != r.e.names.OwnerValue {
			return fmt.Errorf("EventBridge rule %s exists but is not owned by %s (%s=%q); use another CLUSTER_NAME", name, r.e.names.OwnerValue, OwnerTagKey, owner)
		}
		if !sameJSON(aws.ToString(d.EventPattern), k.pattern()) || d.State != ebtypes.RuleStateEnabled {
			if err := r.change("put the pattern of EventBridge rule "+name, func() error {
				_, err := r.c.Events.PutRule(ctx, put)
				return err
			}); err != nil {
				return fmt.Errorf("EventBridge rule %s: %w", name, err)
			}
		}
		ts, err := r.c.Events.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String(name)})
		if err != nil {
			return fmt.Errorf("list the targets of EventBridge rule %s: %w", name, err)
		}
		ok := false
		for _, have := range ts.Targets {
			ok = ok || (aws.ToString(have.Id) == karpenterQueueTarget && aws.ToString(have.Arn) == queue)
		}
		if !ok {
			if err := r.change("send rule "+name+" to the queue", putTargets); err != nil {
				return fmt.Errorf("EventBridge rule %s: %w", name, err)
			}
		}
	}
	return nil
}

func ebKV(t ebtypes.Tag) (*string, *string) { return t.Key, t.Value }
