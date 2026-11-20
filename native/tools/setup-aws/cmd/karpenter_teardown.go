package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// Teardown of KARPENTER=on, in the node groups phase, before the node
// groups: the Karpenter controller runs on the system group. Every step is
// a cluster resource, so --keep-cluster keeps all of it, the Kubernetes
// objects too. In order:
//
//  1. the NodePools setup-aws made (ownership label, a Karpenter pool's
//     name), then a bounded wait until Karpenter has removed their
//     NodeClaims and instances;
//  2. every instance with the owner tag and a karpenter.sh/nodepool tag
//     that is not terminated: the controller is cut off first (its role's
//     inline policy is deleted, so it can launch no more), then the
//     instances are terminated, and any that appear while teardown waits
//     too. This is all that runs when the Kubernetes API fails, or
//     Karpenter did not remove its nodes in time; teardown never strips a
//     finalizer;
//  3. the launch templates with the owner tag and a
//     karpenter.k8s.aws/ec2nodeclass tag;
//  4. the NodeOverlays setup-aws made, of a cluster made with
//     KARPENTER_NODE_OVERLAY=on: they shape only what Karpenter launches
//     for the NodePools, which are gone;
//  5. the EC2NodeClasses setup-aws made, once no NodeClaim uses them (one
//     still in use goes with the cluster), with a bounded wait;
//  6. the EventBridge rules (their targets first), then the queue;
//  7. the Pod Identity association kube-system/karpenter, then the
//     controller role.
//
// The node role's instance profile and access entry, which Karpenter's
// nodes share with self-managed groups, are deleted where teardown deletes
// them for those (stepNodeAccessEntry, stepInstanceProfile).
//
// Inventory, which teardown runs first, says which steps run: the
// Kubernetes and association steps only for a cluster with the marker of
// KARPENTER=on (the NodeOverlay step only for the marker of
// KARPENTER_NODE_OVERLAY=on); the queue, rules and role steps also when the tagging API
// listed the queue or a rule; the instance and launch template steps also
// when an owner-tagged instance or launch template carries a Karpenter
// tag. A teardown of any other deployment makes no call that it did not
// make before KARPENTER.

// Bounds of the Kubernetes waits and retries; replaced in tests.
var (
	karpenterDrainTimeout = 20 * time.Minute
	karpenterClassTimeout = 5 * time.Minute
	// A Kubernetes request that fails for a moment (no answer, 429, a 5xx)
	// is tried karpenterKubeTries times, karpenterKubeRetry apart. One that
	// still fails marks the API down for the rest of the teardown
	// (tdEnv.kubeDown), so an API that does not answer costs one series.
	karpenterKubeTries = 3
	karpenterKubeRetry = 2 * time.Second
	// karpenterSettle is how long after the controller is cut off teardown
	// still looks for an instance it launched just before: IAM takes a
	// moment to stop a session that holds the policy.
	karpenterSettle = 45 * time.Second
)

// The tags Karpenter puts on what it launches, which teardown finds them by,
// and the one EKS Auto Mode puts on its own, which teardown leaves alone.
const (
	karpenterPoolTag  = "karpenter.sh/nodepool"
	karpenterClassTag = "karpenter.k8s.aws/ec2nodeclass"
	autoModePoolTag   = "eks:kubernetes-node-pool-name"
)

// Inventory kinds.
const (
	kQueue           = "SQS queue"
	kRule            = "EventBridge rule"
	kKubeNodePool    = "Karpenter NodePool"
	kKubeNodeClass   = "Karpenter EC2NodeClass"
	kKubeNodeOverlay = "Karpenter NodeOverlay"
)

// karpenterSteps are the steps above, in order.
func (e *tdEnv) karpenterSteps() []tdStep {
	return []tdStep{e.stepKarpenterNodePools, e.stepKarpenterInstances, e.stepKarpenterLaunchTemplates, e.stepKarpenterNodeOverlays,
		e.stepKarpenterNodeClasses, e.stepKarpenterRules, e.stepKarpenterQueue, e.stepKarpenterAssociation, e.stepKarpenterRole}
}

// karpenterMarked reports a cluster made with KARPENTER=on, with or without
// KARPENTER_NODE_OVERLAY=on.
func karpenterMarked(cl *ekstypes.Cluster) bool {
	return cl != nil && knownKarpenterMarker(cl.Tags[nodegroups.KarpenterTagKey])
}

// knownKarpenterMarker reports a value of the cluster marker that one of the
// two Karpenter modes sets.
func knownKarpenterMarker(v string) bool {
	return v == nodegroups.KarpenterMarkerOn || v == nodegroups.KarpenterMarkerNodeOverlay
}

// nodeOverlayMarked reports a cluster made with KARPENTER_NODE_OVERLAY=on:
// only it has NodeOverlays of setup-aws.
func nodeOverlayMarked(cl *ekstypes.Cluster) bool {
	return cl != nil && cl.Tags[nodegroups.KarpenterTagKey] == nodegroups.KarpenterMarkerNodeOverlay
}

// karpenterAWS reports a deployment whose queue, rules and role teardown
// looks for: its cluster carries the marker, or the tagging API listed its
// queue or one of its rules (inventory).
func (e *tdEnv) karpenterAWS() bool { return e.karpenterCluster != nil || e.karpenterTagged }

// karpenterPoolName reports a NodePool or EC2NodeClass name of a pool
// either Karpenter mode makes: Auto Mode's NodePools carry the ownership
// label too, and are not these.
func karpenterPoolName(name string) bool {
	return slices.Contains(nodegroups.AnyKarpenterPools(), name)
}

// karpenterLaunched reports what Karpenter launched for an EC2NodeClass,
// by its tags: tag (karpenterPoolTag or karpenterClassTag), and not one of
// EKS Auto Mode's.
func karpenterLaunched(tags map[string]string, tag string) bool {
	_, k := tags[tag]
	_, auto := tags[autoModePoolTag]
	return k && !auto
}

// karpenterARN reports the tagging API's ARN of the Karpenter queue or one
// of its rules, which inventory lists itself (karpenterRows).
func (e *tdEnv) karpenterARN(svc, rtype, id string) bool {
	switch svc {
	case "sqs":
		// arn:...:sqs:<region>:<account>:<name>: arnType gives the name as the type.
		return rtype == karpenterQueueName(e.n.Cluster) && id == ""
	case "events":
		return rtype == "rule" && slices.Contains(karpenterRuleNames(e.n.Cluster), id)
	}
	return false
}

// karpenterKube is the Kubernetes API of the marked cluster, or nil when
// there is none to use (said in the log, with then: what teardown does
// instead), as when it failed before in this teardown.
func (e *tdEnv) karpenterKube(ctx context.Context, then string) kubeObjects {
	cl := e.karpenterCluster
	if cl == nil || cl.Status == ekstypes.ClusterStatusDeleting {
		return nil
	}
	if e.kubeDown != nil {
		e.say("the Kubernetes API of cluster %s failed before (%v); %s", e.n.Cluster, e.kubeDown, then)
		return nil
	}
	k, err := e.c.Objects(ctx, cl)
	if err != nil {
		e.kubeFailed(err, then)
		return nil
	}
	return k
}

// kubeFailed says how the Kubernetes API failed, and what teardown does
// instead (then); the rest of the teardown does without the API.
func (e *tdEnv) kubeFailed(err error, then string) {
	e.kubeDown = err
	e.say("the Kubernetes API of cluster %s %s (%v); %s", e.n.Cluster, kubeFailure(err), err, then)
}

// kubeFailure names how a Kubernetes request failed: the status the API
// answered, or that it did not answer.
func kubeFailure(err error) string {
	var ke *kubeError
	switch {
	case !errors.As(err, &ke):
		return "could not be reached"
	case ke.Status > 0:
		return fmt.Sprintf("answered %d %s", ke.Status, http.StatusText(ke.Status))
	case ke.Status < 0:
		return "failed the certificate check"
	}
	return "did not answer"
}

// retryKube runs read until it succeeds or fails with more than a blip of
// the Kubernetes API (kubeTransient), at most karpenterKubeTries times.
func retryKube(ctx context.Context, read func() error) error {
	var err error
	for i := 0; i < karpenterKubeTries; i++ {
		if err = read(); err == nil || !kubeTransient(err) {
			return err
		}
		if i < karpenterKubeTries-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(karpenterKubeRetry):
			}
		}
	}
	return err
}

// listKarpenter lists the objects of kind k that setup-aws made for a
// Karpenter pool (the ownership label and a pool's name), retrying a blip.
func listKarpenter(ctx context.Context, kube kubeObjects, k kubeKind) ([]kubeObject, error) {
	var all []kubeObject
	err := retryKube(ctx, func() error {
		var err error
		all, err = kube.List(ctx, k, "", managedByKey+"="+managedByValue)
		return err
	})
	if err != nil {
		return nil, err
	}
	var out []kubeObject
	for _, o := range all {
		if karpenterPoolName(o.name()) {
			out = append(out, o)
		}
	}
	return out, nil
}

// karpenterInstances are the instances Karpenter launched for this
// deployment that are not terminated: the owner tag (from the
// EC2NodeClasses' tags) and a karpenter.sh/nodepool tag, and not EKS Auto
// Mode's.
func (e *tdEnv) karpenterInstances(ctx context.Context) ([]ec2Item, error) {
	all, err := e.ec2List(ctx, kInstance, e.ownerFilter(), flt("tag-key", karpenterPoolTag),
		flt("instance-state-name", "pending", "running", "stopping", "stopped", "shutting-down"))
	if err != nil {
		return nil, err
	}
	var out []ec2Item
	for _, it := range all {
		if karpenterLaunched(it.tags, karpenterPoolTag) {
			out = append(out, it)
		}
	}
	return out, nil
}

// stepKarpenterNodePools deletes the NodePools setup-aws made, and waits
// (karpenterDrainTimeout) until Karpenter has removed their NodeClaims and
// instances. A Kubernetes API that fails skips it, and so does a NodePool
// that could not be deleted: the next step cuts the controller off and
// terminates the instances.
func (e *tdEnv) stepKarpenterNodePools(ctx context.Context) ([]tdTarget, []string, error) {
	const then = "teardown terminates the Karpenter instances itself"
	kube := e.karpenterKube(ctx, then)
	if kube == nil {
		return nil, nil, nil
	}
	pools, err := listKarpenter(ctx, kube, kindNodePool)
	switch {
	case kubeNotFound(err):
		return nil, nil, nil
	case err != nil:
		e.kubeFailed(err, then)
		return nil, nil, nil
	}
	var ts []tdTarget
	var names []string
	failed := false
	for _, p := range pools {
		name := p.name()
		names = append(names, name)
		ts = append(ts, tdTarget{kind: kKubeNodePool, id: name, del: func(ctx context.Context) error {
			if err := retryKube(ctx, func() error { return kube.Delete(ctx, kindNodePool, "", name) }); err != nil {
				e.kubeFailed(err, "NodePool "+name+" stays; "+then)
				failed = true
			}
			return nil
		}})
	}
	if len(ts) > 0 {
		ts[0].wait = func(ctx context.Context) error {
			if failed {
				return nil
			}
			return e.waitDrained(ctx, kube, names)
		}
	}
	return ts, nil, nil
}

// waitDrained waits until no NodeClaim of the pools and no Karpenter
// instance is left. It gives up, with a note and no error, when that takes
// more than karpenterDrainTimeout or the Kubernetes API fails: the
// instances are then terminated by the next step.
func (e *tdEnv) waitDrained(ctx context.Context, kube kubeObjects, pools []string) error {
	sel := karpenterPoolTag + " in (" + strings.Join(pools, ",") + ")"
	deadline := time.Now().Add(karpenterDrainTimeout)
	for {
		claims, err := kube.List(ctx, kindNodeClaim, "", sel)
		switch {
		case kubeNotFound(err):
			claims, err = nil, nil
		case err != nil && !kubeTransient(err):
			e.kubeFailed(err, "teardown terminates the Karpenter instances itself")
			return nil
		}
		insts, ierr := e.karpenterInstances(ctx)
		if ierr != nil {
			return ierr
		}
		if err == nil && len(claims) == 0 && len(insts) == 0 {
			e.say("Karpenter removed the nodes of NodePools %s", strings.Join(pools, ", "))
			e.karpenterDrained = true
			return nil
		}
		if time.Now().After(deadline) {
			e.say("%d NodeClaims and %d instances of NodePools %s are left after %s; teardown terminates the instances itself",
				len(claims), len(insts), strings.Join(pools, ", "), karpenterDrainTimeout)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tdPoll):
		}
	}
}

// stepKarpenterInstances terminates the Karpenter instances that are left,
// and waits until they are terminated. It needs neither the cluster nor
// the marker: the tags are enough. Before it terminates any, it cuts the
// controller off (karpenterCutOff), so the controller cannot launch new
// ones for the Pods the terminated nodes held; one it launched before that
// took effect is terminated while teardown waits.
func (e *tdEnv) stepKarpenterInstances(ctx context.Context) ([]tdTarget, []string, error) {
	if !e.karpenterAWS() && !e.karpenterEC2 {
		return nil, nil, nil
	}
	insts, err := e.karpenterInstances(ctx)
	if err != nil {
		return nil, nil, err
	}
	var rs, all, ids []string
	for _, it := range insts {
		if r := e.refuse(kInstance, it.id, it.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		all = append(all, it.id)
		if it.state != "shutting-down" {
			ids = append(ids, it.id)
		}
	}
	// With no instance now, the controller still needs cutting off when
	// Karpenter did not confirm the drain (the Kubernetes API failed, a
	// NodePool delete failed, or the drain timed out): it may still run, and
	// launch one after this look.
	late := len(all) == 0 && e.karpenterAWS() && !e.karpenterDrained
	if len(all) == 0 && !late {
		return nil, rs, nil
	}
	ts, err := e.karpenterCutOff(ctx)
	if err != nil {
		return nil, nil, err
	}
	if late && len(ts) == 0 {
		// No controller policy to cut off: the role or its policy is gone.
		return nil, rs, nil
	}
	var cutAt time.Time
	for i := range ts {
		del := ts[i].del
		ts[i].del = func(ctx context.Context) error {
			err := del(ctx)
			cutAt = time.Now()
			return err
		}
	}
	terminate := func(ctx context.Context, ids []string) error {
		_, err := e.c.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids})
		return err
	}
	id := strings.Join(all, ", ")
	if late {
		id = "launched after the controller is cut off, if any"
	}
	t := tdTarget{kind: "Karpenter instances", id: id, wait: func(ctx context.Context) error {
		return poll(ctx, "the Karpenter instances to be terminated", func(ctx context.Context) (bool, error) {
			left, err := e.karpenterInstances(ctx)
			if err != nil {
				return false, err
			}
			var lateIDs []string
			for _, it := range left {
				if it.state != "shutting-down" && e.refuse(kInstance, it.id, it.tags) == "" {
					lateIDs = append(lateIDs, it.id)
				}
			}
			if len(lateIDs) > 0 {
				e.say("terminate Karpenter instances %s, launched while teardown ran", strings.Join(lateIDs, ", "))
				if err := terminate(ctx, lateIDs); err != nil && !tdGone(err) {
					return false, fmt.Errorf("terminate %s: %w", strings.Join(lateIDs, ", "), err)
				}
			}
			settled := cutAt.IsZero() || time.Since(cutAt) >= karpenterSettle
			return len(left) == 0 && settled, nil
		})
	}}
	switch {
	case len(ids) > 0:
		t.del = func(ctx context.Context) error { return terminate(ctx, ids) }
	case late:
		// Nothing to terminate yet: the wait looks for late ones.
		t.del = func(context.Context) error { return nil }
	}
	if e.plan && !late {
		// --plan deletes no NodePool, so the instances are still there. A
		// real run drains them first, and does this only for those left.
		for i := range ts {
			ts[i].id += ", only if instances are left after the NodePools drain"
		}
		t.id += ", only if instances are left after the NodePools drain"
	}
	return append(ts, t), rs, nil
}

// karpenterCutOff is the delete of the controller role's inline policy
// access, when the role is this deployment's and has it: without it the
// controller can launch no instance. A role without the owner tag is left
// to stepKarpenterRole, which refuses it.
func (e *tdEnv) karpenterCutOff(ctx context.Context) ([]tdTarget, error) {
	role := karpenterRoleName(e.n)
	g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get role %s: %w", role, err)
	}
	if !e.owned(kvMap(g.Role.Tags, iamKV)) {
		return nil, nil
	}
	ps, err := e.c.IAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(role)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the inline policies of role %s: %w", role, err)
	}
	if !slices.Contains(ps.PolicyNames, "access") {
		return nil, nil
	}
	return []tdTarget{{kind: "inline policy", id: "access of IAM role " + role + " (Karpenter's controller)", del: func(ctx context.Context) error {
		_, err := e.c.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String("access")})
		return err
	}}}, nil
}

// stepKarpenterLaunchTemplates deletes the launch templates Karpenter made
// for the EC2NodeClasses: the owner tag and a karpenter.k8s.aws/ec2nodeclass
// tag.
func (e *tdEnv) stepKarpenterLaunchTemplates(ctx context.Context) ([]tdTarget, []string, error) {
	if !e.karpenterAWS() && !e.karpenterEC2 {
		return nil, nil, nil
	}
	lts, err := e.ec2List(ctx, kLT, e.ownerFilter(), flt("tag-key", karpenterClassTag))
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, lt := range lts {
		if !karpenterLaunched(lt.tags, karpenterClassTag) {
			continue
		}
		label := lt.name + " (" + lt.id + ")"
		if r := e.refuse(kLT, label, lt.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := lt.id
		ts = append(ts, tdTarget{kind: kLT, id: label, del: func(ctx context.Context) error {
			_, err := e.c.EC2.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)})
			return err
		}})
	}
	return ts, rs, nil
}

// stepKarpenterNodeOverlays deletes the NodeOverlays setup-aws made in a
// cluster made with KARPENTER_NODE_OVERLAY=on, after the NodePools they
// apply to and before the EC2NodeClasses. A Kubernetes API that fails skips
// them: they go with the cluster.
func (e *tdEnv) stepKarpenterNodeOverlays(ctx context.Context) ([]tdTarget, []string, error) {
	if !nodeOverlayMarked(e.karpenterCluster) {
		return nil, nil, nil
	}
	const then = "the NodeOverlays go with the cluster"
	kube := e.karpenterKube(ctx, then)
	if kube == nil {
		return nil, nil, nil
	}
	overlays, err := listKarpenter(ctx, kube, kindNodeOverlay)
	switch {
	case kubeNotFound(err):
		return nil, nil, nil
	case err != nil:
		e.kubeFailed(err, then)
		return nil, nil, nil
	}
	var ts []tdTarget
	for _, o := range overlays {
		name := o.name()
		ts = append(ts, tdTarget{kind: kKubeNodeOverlay, id: name, del: func(ctx context.Context) error {
			if err := retryKube(ctx, func() error { return kube.Delete(ctx, kindNodeOverlay, "", name) }); err != nil {
				e.kubeFailed(err, "NodeOverlay "+name+" goes with the cluster")
			}
			return nil
		}})
	}
	return ts, nil, nil
}

// stepKarpenterNodeClasses deletes the EC2NodeClasses setup-aws made once
// no NodeClaim names them, and waits (karpenterClassTimeout) for Karpenter
// to release them. One still in use, or one Karpenter does not release in
// time, goes with the cluster; a Kubernetes API that fails skips them.
func (e *tdEnv) stepKarpenterNodeClasses(ctx context.Context) ([]tdTarget, []string, error) {
	const then = "the EC2NodeClasses go with the cluster"
	kube := e.karpenterKube(ctx, then)
	if kube == nil {
		return nil, nil, nil
	}
	classes, err := listKarpenter(ctx, kube, kindEC2NodeClass)
	switch {
	case kubeNotFound(err):
		return nil, nil, nil
	case err != nil:
		e.kubeFailed(err, then)
		return nil, nil, nil
	}
	var ts []tdTarget
	for _, c := range classes {
		name := c.name()
		skipped := false
		ts = append(ts, tdTarget{kind: kKubeNodeClass, id: name, del: func(ctx context.Context) error {
			var claims []kubeObject
			err := retryKube(ctx, func() error {
				var err error
				claims, err = kube.List(ctx, kindNodeClaim, "", "")
				return err
			})
			switch {
			case kubeNotFound(err):
			case err != nil:
				e.kubeFailed(err, "EC2NodeClass "+name+" goes with the cluster")
				skipped = true
				return nil
			}
			for _, cl := range claims {
				if cl.str("spec", "nodeClassRef", "name") == name {
					e.say("EC2NodeClass %s still has NodeClaims; it is left to go with the cluster", name)
					skipped = true
					return nil
				}
			}
			if err := retryKube(ctx, func() error { return kube.Delete(ctx, kindEC2NodeClass, "", name) }); err != nil {
				e.kubeFailed(err, "EC2NodeClass "+name+" goes with the cluster")
				skipped = true
			}
			return nil
		}, wait: func(ctx context.Context) error {
			if skipped {
				return nil
			}
			deadline := time.Now().Add(karpenterClassTimeout)
			for {
				o, err := kube.Get(ctx, kindEC2NodeClass, "", name)
				switch {
				case err == nil && o == nil:
					return nil
				case err != nil && !kubeTransient(err):
					e.kubeFailed(err, "EC2NodeClass "+name+" goes with the cluster")
					return nil
				}
				if time.Now().After(deadline) {
					e.say("EC2NodeClass %s is still there after %s; it is left to go with the cluster", name, karpenterClassTimeout)
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(tdPoll):
				}
			}
		}})
	}
	return ts, nil, nil
}

// tdEventsAPI and tdSQSAPI read and delete the rules and the queue.
type (
	tdEventsAPI interface {
		DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error)
		ListTagsForResource(context.Context, *eventbridge.ListTagsForResourceInput, ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error)
		ListTargetsByRule(context.Context, *eventbridge.ListTargetsByRuleInput, ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error)
		RemoveTargets(context.Context, *eventbridge.RemoveTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error)
		DeleteRule(context.Context, *eventbridge.DeleteRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error)
	}
	tdSQSAPI interface {
		queueAPI
		DeleteQueue(context.Context, *sqs.DeleteQueueInput, ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error)
	}
)

// karpenterRule is rule name's tags, or nil when it does not exist.
func (e *tdEnv) karpenterRule(ctx context.Context, name string) (map[string]string, error) {
	d, err := e.c.Events.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String(name)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe EventBridge rule %s: %w", name, err)
	}
	t, err := e.c.Events.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: d.Arn})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the tags of EventBridge rule %s: %w", name, err)
	}
	return kvMap(t.Tags, ebKV), nil
}

// stepKarpenterRules deletes the EventBridge rules, each with its targets
// first, as EventBridge requires.
func (e *tdEnv) stepKarpenterRules(ctx context.Context) ([]tdTarget, []string, error) {
	if !e.karpenterAWS() {
		return nil, nil, nil
	}
	var ts []tdTarget
	var rs []string
	for _, n := range karpenterRuleNames(e.n.Cluster) {
		tags, err := e.karpenterRule(ctx, n)
		if err != nil {
			return nil, nil, err
		}
		if tags == nil {
			continue
		}
		if r := e.refuse(kRule, n, tags); r != "" {
			rs = append(rs, r)
			continue
		}
		name := n
		ts = append(ts, tdTarget{kind: kRule, id: name + " (and its targets)", del: func(ctx context.Context) error {
			out, err := e.c.Events.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String(name)})
			if err != nil {
				return err
			}
			var ids []string
			for _, t := range out.Targets {
				ids = append(ids, aws.ToString(t.Id))
			}
			if len(ids) > 0 {
				rm, err := e.c.Events.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{Rule: aws.String(name), Ids: ids})
				if err != nil {
					return err
				}
				if rm.FailedEntryCount > 0 {
					return fmt.Errorf("%d targets not removed, first: %s", rm.FailedEntryCount, aws.ToString(rm.FailedEntries[0].ErrorMessage))
				}
			}
			_, err = e.c.Events.DeleteRule(ctx, &eventbridge.DeleteRuleInput{Name: aws.String(name)})
			return err
		}})
	}
	return ts, rs, nil
}

// stepKarpenterQueue deletes the interruption queue, once no rule sends to
// it, and waits until SQS no longer finds it.
func (e *tdEnv) stepKarpenterQueue(ctx context.Context) ([]tdTarget, []string, error) {
	if !e.karpenterAWS() {
		return nil, nil, nil
	}
	name := karpenterQueueName(e.n.Cluster)
	url, tags, err := readQueue(ctx, e.c.SQS, e.n.Account, name)
	if err != nil || url == "" {
		return nil, nil, err
	}
	if r := e.refuse(kQueue, name, tags); r != "" {
		return nil, []string{r}, nil
	}
	return []tdTarget{{kind: kQueue, id: name, del: func(ctx context.Context) error {
		_, err := e.c.SQS.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
		if sqsQueueMissing(err) {
			return nil
		}
		return err
	}, wait: gone(func(ctx context.Context) (bool, error) {
		u, _, err := readQueue(ctx, e.c.SQS, e.n.Account, name)
		return u != "", err
	})}}, nil, nil
}

// stepKarpenterAssociation deletes the Pod Identity association of
// kube-system/karpenter of a cluster with the marker, when it carries the
// owner tag, and refuses one whose role is not this deployment's. One
// without the owner tag goes with the cluster, as the EBS CSI driver's
// does.
func (e *tdEnv) stepKarpenterAssociation(ctx context.Context) ([]tdTarget, []string, error) {
	if e.karpenterCluster == nil {
		return nil, nil, nil
	}
	as, err := e.associations(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, a := range as {
		if aws.ToString(a.Namespace) != karpenterNamespace || aws.ToString(a.ServiceAccount) != karpenterServiceAccount || !e.owned(a.Tags) {
			continue
		}
		label := assocLabel(a)
		if role := roleName(aws.ToString(a.RoleArn)); role != "" {
			g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
			switch {
			case tdGone(err):
			case err != nil:
				return nil, nil, fmt.Errorf("get role %s: %w", role, err)
			default:
				if r := e.refuse("IAM role", role+" of Pod Identity association "+label, kvMap(g.Role.Tags, iamKV)); r != "" {
					rs = append(rs, r)
					continue
				}
			}
		}
		id := a.AssociationId
		ts = append(ts, tdTarget{kind: "Pod Identity association", id: label, del: func(ctx context.Context) error {
			_, err := e.c.EKS.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(e.n.Cluster), AssociationId: id})
			return err
		}})
	}
	return ts, rs, nil
}

// stepKarpenterRole deletes the controller role. One without the owner tag
// is a refusal.
func (e *tdEnv) stepKarpenterRole(ctx context.Context) ([]tdTarget, []string, error) {
	if !e.karpenterAWS() {
		return nil, nil, nil
	}
	return e.roleTargets(ctx, []string{karpenterRoleName(e.n)}, true)
}

// karpenterRows are the inventory rows of the queue and the rules that
// carry this deployment's owner tag, and of a marked cluster's NodePools,
// EC2NodeClasses and NodeOverlays (karpenterKubeRows). The controller role,
// its association, the instance profile, and Karpenter's instances and
// launch templates have their rows already, as owner-tagged resources of
// their kinds.
func (e *tdEnv) karpenterRows(ctx context.Context) ([]tdRow, error) {
	rows := e.karpenterKubeRows(ctx)
	name := karpenterQueueName(e.n.Cluster)
	url, tags, err := readQueue(ctx, e.c.SQS, e.n.Account, name)
	if err != nil {
		return nil, err
	}
	if url != "" && e.owned(tags) {
		rows = append(rows, tdRow{kQueue, name, tags, true})
	}
	for _, n := range karpenterRuleNames(e.n.Cluster) {
		tags, err := e.karpenterRule(ctx, n)
		if err != nil {
			return nil, err
		}
		if tags != nil && e.owned(tags) {
			rows = append(rows, tdRow{kRule, n, tags, true})
		}
	}
	return rows, nil
}

// karpenterKubeRows are the rows of the NodePools and EC2NodeClasses
// setup-aws made in a marked cluster, and of its NodeOverlays in one made
// with KARPENTER_NODE_OVERLAY=on, as the cluster's. They are read best
// effort: when the Kubernetes API fails, inventory says so and lists the
// AWS resources only, since the objects go with the cluster.
func (e *tdEnv) karpenterKubeRows(ctx context.Context) []tdRow {
	type kindRow struct {
		kind kubeKind
		row  string
	}
	kinds := []kindRow{{kindNodePool, kKubeNodePool}, {kindEC2NodeClass, kKubeNodeClass}}
	then := "inventory does not list its NodePools and EC2NodeClasses"
	if nodeOverlayMarked(e.karpenterCluster) {
		kinds = append(kinds, kindRow{kindNodeOverlay, kKubeNodeOverlay})
		then = "inventory does not list its NodePools, EC2NodeClasses and NodeOverlays"
	}
	kube := e.karpenterKube(ctx, then)
	if kube == nil {
		return nil
	}
	var rows []tdRow
	for _, k := range kinds {
		objs, err := listKarpenter(ctx, kube, k.kind)
		switch {
		case kubeNotFound(err):
			continue
		case err != nil:
			e.kubeFailed(err, then)
			return nil
		}
		for _, o := range objs {
			rows = append(rows, tdRow{k.row, o.name(), nil, true})
		}
	}
	return rows
}
