package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// nodeGroupSpec is one managed node group of the cluster.
type nodeGroupSpec struct {
	Name              string
	InstanceType      string
	Min, Desired, Max int32
	VolumeGB          int32
	NestedVirt        bool
	Labels            map[string]string
	Taints            []ekstypes.Taint
}

// groups are the system, gvisor and microvm node groups. The gvisor and
// microvm groups are tainted ate.dev/sandboxClass, so only the worker pods
// that tolerate it land there. ate.dev/workloadType=ate-system-infra is the
// label ate-setup pins the control plane to under
// --cordon-system-infra-components; with Auto Mode the system group is also
// tainted with it, so any other pod runs on a node Auto Mode launches.
//
//	https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
func (r *clusterRun) groups() []nodeGroupSpec {
	sandbox := func(class string) []ekstypes.Taint {
		return []ekstypes.Taint{{Key: aws.String("ate.dev/sandboxClass"), Value: aws.String(class), Effect: ekstypes.TaintEffectNoSchedule}}
	}
	system := nodeGroupSpec{Name: "system", InstanceType: r.cfg.SystemInstanceType, VolumeGB: 100,
		Min: int32(r.cfg.SystemNodes), Desired: int32(r.cfg.SystemNodes), Max: int32(r.cfg.SystemNodes) + 1,
		Labels: map[string]string{"ate.dev/pool": "system", "ate.dev/workloadType": "ate-system-infra"}}
	if r.auto {
		system.Taints = []ekstypes.Taint{{Key: aws.String("ate.dev/workloadType"), Value: aws.String("ate-system-infra"), Effect: ekstypes.TaintEffectNoSchedule}}
	}
	return []nodeGroupSpec{
		system,
		{Name: "gvisor", InstanceType: r.cfg.GvisorInstanceType, VolumeGB: 200,
			Min: 1, Desired: int32(r.cfg.GvisorNodes), Max: int32(r.cfg.GvisorNodes) + 2,
			Labels: map[string]string{"ate.dev/pool": "gvisor"}, Taints: sandbox("gvisor")},
		{Name: "microvm", InstanceType: r.cfg.MicrovmInstanceType, VolumeGB: 200, NestedVirt: true,
			Min: 1, Desired: int32(r.cfg.MicrovmNodes), Max: int32(r.cfg.MicrovmNodes) + 2,
			Labels: map[string]string{"ate.dev/pool": "microvm"}, Taints: sandbox("microvm")},
	}
}

func (r *clusterRun) createNodegroupInput(g nodeGroupSpec) *eks.CreateNodegroupInput {
	lt := r.templates[g.Name]
	return &eks.CreateNodegroupInput{
		ClusterName:   aws.String(r.e.names.Cluster),
		NodegroupName: aws.String(g.Name),
		NodeRole:      aws.String(r.roleARN(r.nodeRoleName())),
		Subnets:       r.private,
		AmiType:       r.amiTypes[g.InstanceType],
		CapacityType:  ekstypes.CapacityTypesOnDemand,
		InstanceTypes: []string{g.InstanceType},
		ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: aws.Int32(g.Min), DesiredSize: aws.Int32(g.Desired), MaxSize: aws.Int32(g.Max)},
		LaunchTemplate: &ekstypes.LaunchTemplateSpecification{Id: aws.String(lt.id),
			Version: aws.String(strconv.FormatInt(lt.version, 10))},
		Labels: g.Labels,
		Taints: g.Taints,
		Tags:   r.tags,
	}
}

// ensureNodegroups creates the node groups that do not exist. It does not
// wait for them, so the three come up together (waitReady).
func (r *clusterRun) ensureNodegroups(ctx context.Context) error {
	for _, g := range r.groups() {
		if ng := r.nodegroups[g.Name]; ng != nil {
			if err := nodegroupFailed(r.e.names.Cluster, ng); err != nil {
				return err
			}
			slog.Info("node group exists", "nodegroup", g.Name, "status", ng.Status)
			continue
		}
		in := r.createNodegroupInput(g)
		if err := r.create(fmt.Sprintf("node group %s (%d x %s)", g.Name, g.Desired, g.InstanceType), func() error {
			return retryNewRole(ctx, func() error {
				_, err := r.c.EKS.CreateNodegroup(ctx, in)
				return err
			})
		}); err != nil {
			return fmt.Errorf("create node group %s: %w", g.Name, err)
		}
	}
	return nil
}

// nodegroupFailed is an error, with the way out, for a node group that
// cannot become ACTIVE by waiting.
func nodegroupFailed(cluster string, ng *ekstypes.Nodegroup) error {
	switch ng.Status {
	case ekstypes.NodegroupStatusCreateFailed, ekstypes.NodegroupStatusDegraded,
		ekstypes.NodegroupStatusDeleting, ekstypes.NodegroupStatusDeleteFailed:
	default:
		return nil
	}
	var issues []string
	if ng.Health != nil {
		for _, i := range ng.Health.Issues {
			issues = append(issues, fmt.Sprintf("%s: %s", i.Code, aws.ToString(i.Message)))
		}
	}
	return fmt.Errorf("node group %s is %s (%s); fix the cause, delete it (aws eks delete-nodegroup --cluster-name %s --nodegroup-name %s) and run again",
		aws.ToString(ng.NodegroupName), ng.Status, strings.Join(issues, "; "), cluster, aws.ToString(ng.NodegroupName))
}

// waitReady waits for the node groups, tags their Auto Scaling groups, and
// then waits for the add-ons, which need the nodes.
func (r *clusterRun) waitReady(ctx context.Context) error {
	if r.plan() {
		slog.Info("plan: would wait for the node groups and add-ons to be ACTIVE, and tag the node groups' Auto Scaling groups")
		return nil
	}
	for _, g := range r.groups() {
		var ng *ekstypes.Nodegroup
		if err := waitFor(ctx, "node group "+g.Name+" to be ACTIVE", 30*time.Minute, func() (bool, error) {
			out, err := r.c.EKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(r.e.names.Cluster), NodegroupName: aws.String(g.Name)})
			if err != nil {
				return false, fmt.Errorf("describe node group %s: %w", g.Name, err)
			}
			ng = out.Nodegroup
			if err := nodegroupFailed(r.e.names.Cluster, ng); err != nil {
				return false, err
			}
			return ng.Status == ekstypes.NodegroupStatusActive, nil
		}); err != nil {
			return err
		}
		slog.Info("node group ACTIVE", "nodegroup", g.Name)
		if ng.Resources != nil {
			for _, a := range ng.Resources.AutoScalingGroups {
				if err := r.tagASG(ctx, aws.ToString(a.Name), ng.Tags[CreatedByTagKey] == CreatedByTagValue); err != nil {
					return err
				}
			}
		}
	}
	return r.waitAddons(ctx)
}

// tagASG gives a node group's Auto Scaling group the deployment's tags,
// which EKS does not copy from the node group. The created-by tag goes only
// on the groups of node groups this tool created, not on eksctl's.
//
//	https://docs.aws.amazon.com/autoscaling/ec2/userguide/ec2-auto-scaling-tagging.html
func (r *clusterRun) tagASG(ctx context.Context, name string, ours bool) error {
	out, err := r.c.ASG.DescribeTags(ctx, &autoscaling.DescribeTagsInput{Filters: []asgtypes.Filter{
		{Name: aws.String("auto-scaling-group"), Values: []string{name}}}})
	if err != nil {
		return fmt.Errorf("read the tags of Auto Scaling group %s: %w", name, err)
	}
	have := map[string]string{}
	for _, t := range out.Tags {
		have[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	var add []asgtypes.Tag
	for _, k := range sortedKeys(r.tags) {
		if k == CreatedByTagKey && !ours {
			continue
		}
		if have[k] != r.tags[k] {
			add = append(add, asgtypes.Tag{ResourceId: aws.String(name), ResourceType: aws.String("auto-scaling-group"),
				Key: aws.String(k), Value: aws.String(r.tags[k]), PropagateAtLaunch: aws.Bool(true)})
		}
	}
	if len(add) == 0 {
		return nil
	}
	return r.change("tag Auto Scaling group "+name, func() error {
		_, err := r.c.ASG.CreateOrUpdateTags(ctx, &autoscaling.CreateOrUpdateTagsInput{Tags: add})
		return err
	})
}
