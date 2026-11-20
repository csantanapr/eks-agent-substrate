package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// nodeGroupSpec is one node group of the cluster, managed or self-managed.
type nodeGroupSpec struct {
	Name              string
	InstanceType      string
	Min, Desired, Max int32
	VolumeGB          int32
	NestedVirt        bool
	// Enclave turns Nitro Enclaves on in the launch template (the nitro
	// group: the Nitro Sandbox AMI needs them).
	Enclave bool
	Labels  map[string]string
	// NodeLabels are labels the nodes register with through the NodeConfig
	// only, never the EKS API's node group labels: eks.amazonaws.com/ keys,
	// which are EKS's own (the nitro group's NitroSandboxLabel, which its AMI
	// sets too).
	NodeLabels map[string]string
	Taints     []ekstypes.Taint
	// Image is the AMI the group's launch template names (groupImage), or
	// nil when EKS picks it (a managed group without NODE_AMI_ID). A
	// Karpenter NodePool's is its EC2NodeClass's AMI.
	Image *nodeImage
	// Pool is a Karpenter NodePool (KARPENTER=on), not a node group: its
	// nodes come and go, up to Desired (*_NODES) of them.
	Pool bool
}

// noun names g in messages: "<name> node group", or "<name> NodePool".
func (g nodeGroupSpec) noun() string {
	if g.Pool {
		return g.Name + " NodePool"
	}
	return g.Name + " node group"
}

// groupExists reports whether node group name exists as a group of the
// run's type: an EKS managed node group, or a self-managed group's Auto
// Scaling group.
func (r *clusterRun) groupExists(name string) bool {
	if r.selfManaged() {
		return r.asgs[name] != nil
	}
	return r.nodegroups[name] != nil
}

// groups are the node groups NODE_GROUPS names, of the system, gvisor,
// microvm and nitro ones (allGroups), in that order: on an Auto Mode cluster
// any of them, or none (nodegroups.Check is the rule).
//
// With KARPENTER=on the gvisor group is a Karpenter NodePool (pools), not a
// node group, so it is not among them.
func (r *clusterRun) groups() []nodeGroupSpec {
	var out []nodeGroupSpec
	for _, g := range r.allGroups() {
		if slices.Contains(r.wanted, g.Name) && !r.isPool(g.Name) {
			out = append(out, g)
		}
	}
	return out
}

// isPool reports a group this run makes as a Karpenter NodePool
// (nodegroups.KarpenterPools): gvisor with KARPENTER=on, and microvm and
// nitro too with nodeOverlay.
func (r *clusterRun) isPool(name string) bool {
	return r.karpenter && nodegroups.IsKarpenterPool(name, r.nodeOverlay)
}

// pools are, with KARPENTER=on, the groups NODE_GROUPS names that are made
// as Karpenter NodePools (isPool): each with the group's instance type,
// root volume, labels and taints, its EC2NodeClass's AMI (Image), and up to
// Desired nodes. Without KARPENTER=on there are none.
func (r *clusterRun) pools() []nodeGroupSpec {
	var out []nodeGroupSpec
	for _, g := range r.allGroups() {
		if slices.Contains(r.wanted, g.Name) && r.isPool(g.Name) {
			g.Pool, g.Image = true, r.poolImages[g.Name]
			out = append(out, g)
		}
	}
	return out
}

// computeSpecs are the node groups, then the NodePools: every instance type
// the run launches.
func (r *clusterRun) computeSpecs() []nodeGroupSpec {
	return append(r.groups(), r.pools()...)
}

// allGroups are the system, gvisor, microvm and nitro node groups, each with
// its image (groupImage). The sandbox groups are tainted
// ate.dev/sandboxClass, so only the worker pods that tolerate it land there.
// ate.dev/workloadType=ate-system-infra is the label ate-setup pins the
// control plane to under --cordon-system-infra-components; with Auto Mode
// the system group is also tainted with it, so any other pod runs on a node
// Auto Mode launches. The nitro group's nodes also carry
// eks.amazonaws.com/nitro-sandbox=true, which RuntimeClass isolated-sandbox
// selects (its AMI sets it too).
//
//	https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
func (r *clusterRun) allGroups() []nodeGroupSpec {
	sandbox := func(class string) []ekstypes.Taint {
		return []ekstypes.Taint{{Key: aws.String("ate.dev/sandboxClass"), Value: aws.String(class), Effect: ekstypes.TaintEffectNoSchedule}}
	}
	system := nodeGroupSpec{Name: "system", InstanceType: r.cfg.SystemInstanceType, VolumeGB: 100,
		Min: int32(r.cfg.SystemNodes), Desired: int32(r.cfg.SystemNodes), Max: int32(r.cfg.SystemNodes) + 1,
		Labels: map[string]string{"ate.dev/pool": "system", "ate.dev/workloadType": "ate-system-infra"}}
	if r.auto {
		system.Taints = []ekstypes.Taint{{Key: aws.String("ate.dev/workloadType"), Value: aws.String("ate-system-infra"), Effect: ekstypes.TaintEffectNoSchedule}}
	}
	gs := []nodeGroupSpec{
		system,
		{Name: "gvisor", InstanceType: r.cfg.GvisorInstanceType, VolumeGB: 200,
			Min: 1, Desired: int32(r.cfg.GvisorNodes), Max: int32(r.cfg.GvisorNodes) + 2,
			Labels: map[string]string{"ate.dev/pool": "gvisor"}, Taints: sandbox("gvisor")},
		{Name: "microvm", InstanceType: r.cfg.MicrovmInstanceType, VolumeGB: 200, NestedVirt: true,
			Min: 1, Desired: int32(r.cfg.MicrovmNodes), Max: int32(r.cfg.MicrovmNodes) + 2,
			Labels: map[string]string{"ate.dev/pool": "microvm"}, Taints: sandbox("microvm")},
		{Name: nodegroups.Nitro, InstanceType: r.cfg.NitroInstanceType, VolumeGB: 100, Enclave: true,
			Min: 1, Desired: int32(r.cfg.NitroNodes), Max: int32(r.cfg.NitroNodes) + 2,
			Labels:     map[string]string{"ate.dev/pool": nodegroups.Nitro},
			NodeLabels: map[string]string{nodegroups.NitroSandboxLabel: "true"}, Taints: sandbox(nodegroups.Nitro)},
	}
	for i := range gs {
		gs[i].Image = r.groupImage(gs[i])
	}
	return gs
}

func (r *clusterRun) createNodegroupInput(g nodeGroupSpec) *eks.CreateNodegroupInput {
	lt := r.templates[g.Name]
	in := &eks.CreateNodegroupInput{
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
	// A group whose launch template names an AMI takes no AMI type.
	if g.Image != nil {
		in.AmiType = ""
	}
	return in
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
			if err := r.checkGroupImage(ctx, g, ng); err != nil {
				return err
			}
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

// checkGroupImage warns when an existing node group runs another AMI than
// its setting (NODE_AMI_ID, or NITRO_AMI_ID for the nitro group) names: it
// keeps its nodes, since moving it to another template version replaces
// them, which is a decision for a person. It reads the group's template only
// when a custom AMI is involved, on either side.
func (r *clusterRun) checkGroupImage(ctx context.Context, g nodeGroupSpec, ng *ekstypes.Nodegroup) error {
	if g.Image == nil && ng.AmiType != ekstypes.AMITypesCustom {
		return nil
	}
	want, have := "", ""
	if g.Image != nil {
		want = g.Image.ID
	}
	if lt := ng.LaunchTemplate; lt != nil && aws.ToString(lt.Id) != "" {
		out, err := r.c.EC2.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
			LaunchTemplateId: lt.Id, Versions: []string{aws.ToString(lt.Version)}})
		switch {
		case hasCode(err, "InvalidLaunchTemplateId.NotFound"), hasCode(err, "InvalidLaunchTemplateId.VersionNotFound"):
		case err != nil:
			return fmt.Errorf("read the launch template of node group %s: %w", aws.ToString(ng.NodegroupName), err)
		case len(out.LaunchTemplateVersions) == 1 && out.LaunchTemplateVersions[0].LaunchTemplateData != nil:
			have = aws.ToString(out.LaunchTemplateVersions[0].LaunchTemplateData.ImageId)
		}
	}
	if have != want {
		setting := imageSetting(g)
		slog.Warn("the node group runs another AMI than "+setting+" and keeps its nodes; to change it, tear the deployment down (--remove-control-plane, then setup-aws teardown) and create it again",
			"nodegroup", aws.ToString(ng.NodegroupName), "runs", orDefault(have), setting, orDefault(want))
	}
	return nil
}

// orDefault names an empty AMI ID: EKS's own AMI.
func orDefault(ami string) string {
	if ami == "" {
		return "the EKS optimized AMI"
	}
	return ami
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
		// A nitro NodePool (KARPENTER_NODE_OVERLAY=on) is not among
		// r.groups(): it starts with no node, so nothing waits for its
		// capacity here; Karpenter launches a node for the counter-iso
		// worker Pods, and mode3-iso checks them.
		if g.Name == nodegroups.Nitro {
			desired := g.Desired
			if sc := ng.ScalingConfig; sc != nil && sc.DesiredSize != nil {
				desired = aws.ToInt32(sc.DesiredSize)
			}
			kube, err := r.c.Kube(ctx, r.cluster)
			if err != nil {
				return err
			}
			if err := r.waitNitroCapacity(ctx, kube, desired); err != nil {
				return err
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
