package cmd

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// The fakes KARPENTER=on adds: SQS, EventBridge and Pod Identity
// associations in the create cluster account (clusterFake), and SQS,
// EventBridge, instance termination and the Kubernetes API in the teardown
// account (tdFake).

type fakeQueue struct {
	attrs, tags map[string]string
}

type fakeRule struct {
	pattern string
	state   ebtypes.RuleState
	tags    map[string]string
	targets map[string]string // id -> ARN
}

const fakeQueueURL = "https://sqs.us-west-2.amazonaws.com/111122223333/"

// --- create cluster: SQS ------------------------------------------------------

type fcSQS clusterFake

func (q *fcSQS) f() *clusterFake { return (*clusterFake)(q) }

func (q *fcSQS) queue(url *string) (string, *fakeQueue, error) {
	name := strings.TrimPrefix(aws.ToString(url), fakeQueueURL)
	if fq := q.queues[name]; fq != nil {
		return name, fq, nil
	}
	return name, nil, &sqstypes.QueueDoesNotExist{Message: aws.String("The specified queue does not exist.")}
}

func (q *fcSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	if err := q.f().readIn("GetQueueUrl", in); err != nil {
		return nil, err
	}
	if q.queues[aws.ToString(in.QueueName)] == nil {
		return nil, &sqstypes.QueueDoesNotExist{Message: aws.String("The specified queue does not exist.")}
	}
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(fakeQueueURL + aws.ToString(in.QueueName))}, nil
}

func (q *fcSQS) ListQueueTags(_ context.Context, in *sqs.ListQueueTagsInput, _ ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error) {
	if err := q.f().read("ListQueueTags"); err != nil {
		return nil, err
	}
	_, fq, err := q.queue(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	return &sqs.ListQueueTagsOutput{Tags: maps.Clone(fq.tags)}, nil
}

func (q *fcSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if err := q.f().read("GetQueueAttributes"); err != nil {
		return nil, err
	}
	_, fq, err := q.queue(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, n := range in.AttributeNames {
		if v, ok := fq.attrs[string(n)]; ok {
			out[string(n)] = v
		}
	}
	return &sqs.GetQueueAttributesOutput{Attributes: out}, nil
}

func (q *fcSQS) CreateQueue(_ context.Context, in *sqs.CreateQueueInput, _ ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	f := q.f()
	name := aws.ToString(in.QueueName)
	if f.queues[name] != nil {
		return nil, apiErr("QueueAlreadyExists")
	}
	w := "CreateQueue " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	f.queues[name] = &fakeQueue{attrs: maps.Clone(in.Attributes), tags: maps.Clone(in.Tags)}
	return &sqs.CreateQueueOutput{QueueUrl: aws.String(fakeQueueURL + name)}, f.end(w)
}

func (q *fcSQS) SetQueueAttributes(_ context.Context, in *sqs.SetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.SetQueueAttributesOutput, error) {
	f := q.f()
	name, fq, err := q.queue(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	w := "SetQueueAttributes " + name + " " + strings.Join(sortedKeys(in.Attributes), ",")
	if err := f.begin(w); err != nil {
		return nil, err
	}
	maps.Copy(fq.attrs, in.Attributes)
	return &sqs.SetQueueAttributesOutput{}, f.end(w)
}

// --- create cluster: EventBridge ------------------------------------------------

type fcEvents clusterFake

func (v *fcEvents) f() *clusterFake { return (*clusterFake)(v) }

func ruleARN(name string) string { return "arn:aws:events:us-west-2:111122223333:rule/" + name }

func (v *fcEvents) DescribeRule(_ context.Context, in *eventbridge.DescribeRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	if err := v.f().read("DescribeRule"); err != nil {
		return nil, err
	}
	r := v.rules[aws.ToString(in.Name)]
	if r == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	return &eventbridge.DescribeRuleOutput{Name: in.Name, Arn: aws.String(ruleARN(aws.ToString(in.Name))), EventPattern: aws.String(r.pattern), State: r.state}, nil
}

func (v *fcEvents) ListTagsForResource(_ context.Context, in *eventbridge.ListTagsForResourceInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error) {
	if err := v.f().read("ListTagsForResource"); err != nil {
		return nil, err
	}
	r := v.rules[strings.TrimPrefix(aws.ToString(in.ResourceARN), ruleARN(""))]
	if r == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	var tags []ebtypes.Tag
	for _, k := range sortedKeys(r.tags) {
		tags = append(tags, ebtypes.Tag{Key: aws.String(k), Value: aws.String(r.tags[k])})
	}
	return &eventbridge.ListTagsForResourceOutput{Tags: tags}, nil
}

func (v *fcEvents) PutRule(_ context.Context, in *eventbridge.PutRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error) {
	f := v.f()
	name := aws.ToString(in.Name)
	w := "PutRule " + name
	if err := f.begin(w); err != nil {
		return nil, err
	}
	r := f.rules[name]
	if r == nil {
		// Tags apply when PutRule makes the rule only.
		r = &fakeRule{tags: map[string]string{}, targets: map[string]string{}}
		for _, t := range in.Tags {
			r.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		f.rules[name] = r
	}
	r.pattern, r.state = aws.ToString(in.EventPattern), in.State
	return &eventbridge.PutRuleOutput{RuleArn: aws.String(ruleARN(name))}, f.end(w)
}

func (v *fcEvents) ListTargetsByRule(_ context.Context, in *eventbridge.ListTargetsByRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error) {
	if err := v.f().read("ListTargetsByRule"); err != nil {
		return nil, err
	}
	r := v.rules[aws.ToString(in.Rule)]
	if r == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &eventbridge.ListTargetsByRuleOutput{}
	for _, id := range sortedKeys(r.targets) {
		out.Targets = append(out.Targets, ebtypes.Target{Id: aws.String(id), Arn: aws.String(r.targets[id])})
	}
	return out, nil
}

func (v *fcEvents) PutTargets(_ context.Context, in *eventbridge.PutTargetsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error) {
	f := v.f()
	r := f.rules[aws.ToString(in.Rule)]
	if r == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	w := "PutTargets " + aws.ToString(in.Rule)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	for _, t := range in.Targets {
		r.targets[aws.ToString(t.Id)] = aws.ToString(t.Arn)
	}
	return &eventbridge.PutTargetsOutput{}, f.end(w)
}

// --- create cluster: Pod Identity associations, the Kubernetes API ---------------

func (e *fcEKS) ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	return &eks.ListAddonsOutput{}, nil
}

func (e *fcEKS) ListPodIdentityAssociations(_ context.Context, in *eks.ListPodIdentityAssociationsInput, _ ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	if err := e.f().read("ListPodIdentityAssociations"); err != nil {
		return nil, err
	}
	if !e.clusterExists(in.ClusterName) {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &eks.ListPodIdentityAssociationsOutput{}
	for _, id := range sortedKeysOf(e.assocs) {
		a := e.assocs[id]
		if aws.ToString(a.Namespace) == aws.ToString(in.Namespace) && aws.ToString(a.ServiceAccount) == aws.ToString(in.ServiceAccount) {
			out.Associations = append(out.Associations, ekstypes.PodIdentityAssociationSummary{AssociationId: a.AssociationId,
				Namespace: a.Namespace, ServiceAccount: a.ServiceAccount})
		}
	}
	return out, nil
}

func (e *fcEKS) DescribePodIdentityAssociation(_ context.Context, in *eks.DescribePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	if err := e.f().read("DescribePodIdentityAssociation"); err != nil {
		return nil, err
	}
	a := e.assocs[aws.ToString(in.AssociationId)]
	if a == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	c := *a
	return &eks.DescribePodIdentityAssociationOutput{Association: &c}, nil
}

func (e *fcEKS) CreatePodIdentityAssociation(_ context.Context, in *eks.CreatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error) {
	f := e.f()
	if !e.clusterExists(in.ClusterName) {
		return nil, apiErr("ResourceNotFoundException")
	}
	if f.roles[strings.TrimPrefix(aws.ToString(in.RoleArn), "arn:aws:iam::111122223333:role/")] == nil {
		return nil, apiErr("InvalidParameterException")
	}
	w := "CreatePodIdentityAssociation " + aws.ToString(in.Namespace) + "/" + aws.ToString(in.ServiceAccount)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	id := f.id("a")
	f.assocs[id] = &ekstypes.PodIdentityAssociation{AssociationId: aws.String(id), Namespace: in.Namespace,
		ServiceAccount: in.ServiceAccount, RoleArn: in.RoleArn, Tags: maps.Clone(in.Tags)}
	return &eks.CreatePodIdentityAssociationOutput{}, f.end(w)
}

func (e *fcEKS) UpdatePodIdentityAssociation(_ context.Context, in *eks.UpdatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.UpdatePodIdentityAssociationOutput, error) {
	f := e.f()
	a := f.assocs[aws.ToString(in.AssociationId)]
	if a == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	w := "UpdatePodIdentityAssociation " + aws.ToString(in.AssociationId)
	if err := f.begin(w); err != nil {
		return nil, err
	}
	a.RoleArn = in.RoleArn
	return &eks.UpdatePodIdentityAssociationOutput{}, f.end(w)
}

// kubeObjects is the cluster's Kubernetes object API: f.objects, of an
// ACTIVE cluster.
func (f *clusterFake) kubeObjects(_ context.Context, c *ekstypes.Cluster) (kubeObjects, error) {
	f.reads = append(f.reads, "KubeObjects")
	if c == nil || c.Status != ekstypes.ClusterStatusActive {
		return nil, fmt.Errorf("fake: the Kubernetes API of a cluster that is not ACTIVE")
	}
	if f.objects == nil {
		return nil, fmt.Errorf("fake: no Kubernetes API for objects")
	}
	return f.objects.client(), nil
}

// --- teardown: SQS, EventBridge, instances, the Kubernetes API -------------------

type tfSQS tdFake

func (q *tfSQS) f() *tdFake { return (*tdFake)(q) }

func (q *tfSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	f := q.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, "GetQueueUrl")
	if f.deny["GetQueueUrl"] {
		return nil, apiErr("AccessDenied")
	}
	if f.get("queue", aws.ToString(in.QueueName)) == nil {
		return nil, &sqstypes.QueueDoesNotExist{}
	}
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(fakeQueueURL + aws.ToString(in.QueueName))}, nil
}

func (q *tfSQS) ListQueueTags(_ context.Context, in *sqs.ListQueueTagsInput, _ ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error) {
	f := q.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.get("queue", strings.TrimPrefix(aws.ToString(in.QueueUrl), fakeQueueURL))
	if o == nil {
		return nil, &sqstypes.QueueDoesNotExist{}
	}
	return &sqs.ListQueueTagsOutput{Tags: maps.Clone(o.tags)}, nil
}

func (q *tfSQS) DeleteQueue(_ context.Context, in *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	f := q.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	name := strings.TrimPrefix(aws.ToString(in.QueueUrl), fakeQueueURL)
	if err := f.write("DeleteQueue", name, "queue", "QueueDoesNotExist"); err != nil {
		return nil, err
	}
	o := f.objs["queue|"+name]
	if o == nil {
		return nil, &sqstypes.QueueDoesNotExist{}
	}
	// SQS takes a while to delete a queue: one more read finds it.
	o.deleting, o.pending = true, 1
	return &sqs.DeleteQueueOutput{}, nil
}

type tfEvents tdFake

func (v *tfEvents) f() *tdFake { return (*tdFake)(v) }

func (v *tfEvents) DescribeRule(_ context.Context, in *eventbridge.DescribeRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	f := v.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, "DescribeRule")
	if f.deny["DescribeRule"] {
		return nil, apiErr("AccessDenied")
	}
	if f.get("rule", aws.ToString(in.Name)) == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	return &eventbridge.DescribeRuleOutput{Name: in.Name, Arn: aws.String(ruleARN(aws.ToString(in.Name)))}, nil
}

func (v *tfEvents) ListTagsForResource(_ context.Context, in *eventbridge.ListTagsForResourceInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListTagsForResourceOutput, error) {
	f := v.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.get("rule", strings.TrimPrefix(aws.ToString(in.ResourceARN), ruleARN("")))
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	var tags []ebtypes.Tag
	for _, k := range sortedKeys(o.tags) {
		tags = append(tags, ebtypes.Tag{Key: aws.String(k), Value: aws.String(o.tags[k])})
	}
	return &eventbridge.ListTagsForResourceOutput{Tags: tags}, nil
}

func (v *tfEvents) ListTargetsByRule(_ context.Context, in *eventbridge.ListTargetsByRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListTargetsByRuleOutput, error) {
	f := v.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.get("rule", aws.ToString(in.Rule))
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &eventbridge.ListTargetsByRuleOutput{}
	for _, id := range o.attached {
		out.Targets = append(out.Targets, ebtypes.Target{Id: aws.String(id), Arn: aws.String("arn:aws:sqs:us-west-2:111122223333:demo-karpenter")})
	}
	return out, nil
}

func (v *tfEvents) RemoveTargets(_ context.Context, in *eventbridge.RemoveTargetsInput, _ ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error) {
	f := v.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.Rule)
	if err := f.write("RemoveTargets", name, "rule", "ResourceNotFoundException"); err != nil {
		return nil, err
	}
	o := f.objs["rule|"+name]
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	for _, id := range in.Ids {
		o.attached = without(o.attached, id)
	}
	return &eventbridge.RemoveTargetsOutput{}, nil
}

func (v *tfEvents) DeleteRule(_ context.Context, in *eventbridge.DeleteRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	f := v.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.Name)
	if err := f.write("DeleteRule", name, "rule", "ResourceNotFoundException"); err != nil {
		return nil, err
	}
	o := f.objs["rule|"+name]
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	if len(o.attached) > 0 {
		return nil, apiErr("ValidationException") // Rule can't be deleted since it has targets.
	}
	f.remove(o)
	return &eventbridge.DeleteRuleOutput{}, nil
}

// TerminateInstances marks the instances terminated; one already
// terminated, or not there, is refused as EC2 does for an unknown ID.
func (t *tfEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	defer t.lock()()
	f := t.f()
	ids := slices.Clone(in.InstanceIds)
	slices.Sort(ids)
	if err := f.write("TerminateInstances", strings.Join(ids, ","), kInstance, "InvalidInstanceID.NotFound"); err != nil {
		return nil, err
	}
	for _, id := range ids {
		o := f.objs[kInstance+"|"+id]
		if o == nil {
			return nil, apiErr("InvalidInstanceID.NotFound")
		}
		o.state = "terminated"
	}
	if f.onTerminate != nil {
		f.onTerminate()
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

// objects is the teardown account's Kubernetes API: f.kube, or none.
func (f *tdFake) objects(_ context.Context, cl *ekstypes.Cluster) (kubeObjects, error) {
	f.mu.Lock()
	f.reads = append(f.reads, "KubeObjects")
	k := f.kube
	f.mu.Unlock()
	if k == nil {
		return nil, fmt.Errorf("fake: no Kubernetes API")
	}
	return k, nil
}
