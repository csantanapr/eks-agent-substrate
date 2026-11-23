package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// Self-managed node groups (NODE_GROUP_TYPE=self-managed). Each node group
// is an Auto Scaling group <cluster>-<group> in the private subnets that
// launches from the group's launch template, which names the AMI, the node
// role's instance profile and the cluster security group, and carries the
// NodeConfig that joins the node. The node role gets an EC2_LINUX access
// entry, which EKS makes itself for a managed node group's role.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/launch-workers.html
//	https://docs.aws.amazon.com/eks/latest/userguide/creating-access-entries.html

func (r *clusterRun) selfManaged() bool { return r.cfg.NodeGroupType == nodegroups.SelfManaged }

// asgName is the Auto Scaling group of self-managed node group group.
func (r *clusterRun) asgName(group string) string { return r.e.names.Cluster + "-" + group }

// instanceProfileName is the node role's instance profile, named as the role.
func (r *clusterRun) instanceProfileName() string { return r.nodeRoleName() }

// accessTypeNode is the access entry type of a node role: EKS maps it to
// system:nodes.
const accessTypeNode = "EC2_LINUX"

// instanceProfileReader reads an instance profile and its tags: the IAM
// clients of create cluster and of teardown.
type instanceProfileReader interface {
	GetInstanceProfile(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	ListInstanceProfileTags(context.Context, *iam.ListInstanceProfileTagsInput, ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error)
}

// readInstanceProfile is instance profile name with its tags, or nil when
// it does not exist. Any other failed read is an error.
func readInstanceProfile(ctx context.Context, c instanceProfileReader, name string) (*iamtypes.InstanceProfile, map[string]string, error) {
	out, err := c.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if isNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get instance profile %s: %w", name, err)
	}
	ip := out.InstanceProfile
	tags := kvMap(ip.Tags, iamKV)
	if len(tags) == 0 {
		t, err := c.ListInstanceProfileTags(ctx, &iam.ListInstanceProfileTagsInput{InstanceProfileName: aws.String(name)})
		if isNotFound(err) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read tags of instance profile %s: %w", name, err)
		}
		tags = kvMap(t.Tags, iamKV)
	}
	return ip, tags, nil
}

// ensureInstanceProfile makes the node role's instance profile with the role
// in it, for the self-managed groups' launch templates and the Karpenter
// NodePools' EC2NodeClasses. One that exists is used only with this
// deployment's owner tag, and gets the role when it holds none.
func (r *clusterRun) ensureInstanceProfile(ctx context.Context) error {
	if !r.needsNodeProfile() {
		return nil
	}
	name, role := r.instanceProfileName(), r.nodeRoleName()
	ip, tags, err := readInstanceProfile(ctx, r.c.IAM, name)
	if err != nil {
		return err
	}
	add := func() error {
		return retryRoleNotFound(ctx, role, func() error {
			_, err := r.c.IAM.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: aws.String(role)})
			return err
		})
	}
	what := "add role " + role + " to instance profile " + name
	if ip == nil {
		var it []iamtypes.Tag
		for _, k := range sortedKeys(r.tags) {
			it = append(it, iamtypes.Tag{Key: aws.String(k), Value: aws.String(r.tags[k])})
		}
		if err := r.create("IAM instance profile "+name, func() error {
			_, err := r.c.IAM.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(name), Tags: it})
			return err
		}); err != nil {
			return fmt.Errorf("instance profile %s: %w", name, err)
		}
		// The role is part of making the profile.
		if err := r.e.act(what, add); err != nil {
			return fmt.Errorf("instance profile %s: %w", name, err)
		}
		return nil
	}
	if owner := tags[OwnerTagKey]; owner != r.e.names.OwnerValue {
		return fmt.Errorf("IAM instance profile %s exists but is not owned by %s (%s=%q); use another CLUSTER_NAME",
			name, r.e.names.OwnerValue, OwnerTagKey, owner)
	}
	for _, have := range ip.Roles {
		if aws.ToString(have.RoleName) == role {
			slog.Info("instance profile ok", "profile", name)
			return nil
		}
	}
	if len(ip.Roles) > 0 {
		return fmt.Errorf("IAM instance profile %s holds role %s, not %s, and a profile holds one role; %s",
			name, aws.ToString(ip.Roles[0].RoleName), role, recreate)
	}
	if err := r.change(what, add); err != nil {
		return fmt.Errorf("instance profile %s: %w", name, err)
	}
	return nil
}

// ensureNodeAccess gives the node role an EC2_LINUX access entry, so the
// self-managed nodes join. One that exists is kept when it is EC2_LINUX: EKS
// makes one, without the owner tag, for a managed node group's role, and
// teardown leaves such an entry to the cluster.
func (r *clusterRun) ensureNodeAccess(ctx context.Context) error {
	if !r.selfManaged() || len(r.groups()) == 0 {
		return nil
	}
	return r.nodeAccessEntry(ctx)
}

// needsNodeProfile reports a run whose nodes launch with the node role's
// instance profile: self-managed node groups, or Karpenter's NodePools.
func (r *clusterRun) needsNodeProfile() bool {
	return (r.selfManaged() && len(r.groups()) > 0) || r.karpenter
}

// nodeAccessEntry is ensureNodeAccess's work, which the Karpenter phase does
// too: after the node groups, so a managed group's role already has the
// entry EKS makes for it, which is kept.
func (r *clusterRun) nodeAccessEntry(ctx context.Context) error {
	arn := r.roleARN(r.nodeRoleName())
	who := r.mask(arn)
	cluster := aws.String(r.e.names.Cluster)
	if r.cluster != nil {
		out, err := r.c.EKS.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{ClusterName: cluster, PrincipalArn: aws.String(arn)})
		switch {
		case err == nil:
			if t := aws.ToString(out.AccessEntry.Type); t != accessTypeNode {
				return fmt.Errorf("the node role %s has an access entry of type %s, and %s need %s; %s", who, t, r.nodeKinds(), accessTypeNode, recreate)
			}
			slog.Info("node access ok", "principal", who)
			return nil
		case !isNotFound(err):
			return fmt.Errorf("describe the access entry of %s: %w", who, err)
		}
	}
	if err := r.create("EC2_LINUX access entry for the node role "+who, func() error {
		return retryNewRole(ctx, func() error {
			_, err := r.c.EKS.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{ClusterName: cluster,
				PrincipalArn: aws.String(arn), Type: aws.String(accessTypeNode), Tags: r.tags})
			return err
		})
	}); err != nil {
		return fmt.Errorf("access entry of %s: %w", who, err)
	}
	return nil
}

// readASGs reads the Auto Scaling groups of the derived names of groups,
// by node group; a missing one is not in the map.
func (r *clusterRun) readASGs(ctx context.Context, groups []string) (map[string]*asgtypes.AutoScalingGroup, error) {
	byName := map[string]string{}
	var names []string
	for _, g := range groups {
		byName[r.asgName(g)] = g
		names = append(names, r.asgName(g))
	}
	out, err := r.c.ASG.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: names})
	if err != nil {
		return nil, fmt.Errorf("describe Auto Scaling groups %s: %w", strings.Join(names, ", "), err)
	}
	found := map[string]*asgtypes.AutoScalingGroup{}
	for i := range out.AutoScalingGroups {
		a := &out.AutoScalingGroups[i]
		if g, ok := byName[aws.ToString(a.AutoScalingGroupName)]; ok {
			found[g] = a
		}
	}
	return found, nil
}

func asgTagMap(ts []asgtypes.TagDescription) map[string]string {
	m := map[string]string{}
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// recreate is the way on from what create cluster does not change in place.
// It never says to delete a resource by hand: the installer's teardown
// checks the owner of each resource it deletes.
const recreate = "tear the deployment down (--remove-control-plane, then setup-aws teardown) and create it again, or use another CLUSTER_NAME"

// managedNodegroups names every EKS managed node group of the cluster,
// whatever its name, with those readCluster read.
func (r *clusterRun) managedNodegroups(ctx context.Context) ([]string, error) {
	names := map[string]bool{}
	for g := range r.nodegroups {
		names[g] = true
	}
	p := eks.NewListNodegroupsPaginator(r.c.EKS, &eks.ListNodegroupsInput{ClusterName: aws.String(r.e.names.Cluster)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the node groups of cluster %s: %w", r.e.names.Cluster, err)
		}
		for _, g := range page.Nodegroups {
			names[g] = true
		}
	}
	return tdKeys(names), nil
}

// checkGroupType finds the self-managed groups that exist, and refuses a
// cluster that has node groups of the other type: a cluster has one node
// group type. A self-managed run lists every managed node group of the
// cluster, of any name. A managed run reads the managed node groups of the
// wanted names only (readCluster), and the Auto Scaling groups of every name
// in nodegroups.All only on an existing cluster with a node group still to
// make, the one case where it would add a group, so a new or complete
// cluster reads nothing more than before.
func (r *clusterRun) checkGroupType(ctx context.Context) error {
	var wanted []string
	missing := false
	for _, g := range r.groups() {
		wanted = append(wanted, g.Name)
		missing = missing || r.nodegroups[g.Name] == nil
	}
	if len(wanted) == 0 {
		return nil
	}
	cluster := r.e.names.Cluster
	if r.selfManaged() && r.cluster != nil {
		managed, err := r.managedNodegroups(ctx)
		if err != nil {
			return err
		}
		if len(managed) > 0 {
			return fmt.Errorf("cluster %s has the managed node groups %s, and NODE_GROUP_TYPE=self-managed: a cluster has one node group type. Set NODE_GROUP_TYPE=managed to match the cluster, or %s",
				cluster, strings.Join(managed, ", "), recreate)
		}
	}
	if !r.selfManaged() && (r.cluster == nil || !missing) {
		return nil
	}
	found, err := r.readASGs(ctx, nodegroups.All)
	if err != nil {
		return err
	}
	// owned are this deployment's groups: a managed run must find none, and
	// a run without the cluster none either.
	var owned []string
	for _, g := range nodegroups.All {
		a := found[g]
		if a == nil {
			continue
		}
		name, owner := aws.ToString(a.AutoScalingGroupName), asgTagMap(a.Tags)[OwnerTagKey]
		mine := owner == r.e.names.OwnerValue
		if !r.selfManaged() || !slices.Contains(wanted, g) {
			// This run makes no Auto Scaling group of this name, so
			// another owner's is not this cluster's and is left alone.
			if mine {
				owned = append(owned, name)
			}
			continue
		}
		switch {
		case !mine:
			return fmt.Errorf("Auto Scaling group %s exists but is not owned by %s (%s=%q); use another CLUSTER_NAME",
				name, r.e.names.OwnerValue, OwnerTagKey, owner)
		case aws.ToString(a.Status) != "":
			return fmt.Errorf("Auto Scaling group %s is %s; wait for it to be gone and run again", name, strings.ToLower(aws.ToString(a.Status)))
		}
		r.asgs[g] = a
		owned = append(owned, name)
	}
	if !r.selfManaged() && len(owned) > 0 {
		return fmt.Errorf("cluster %s has the self-managed node groups %s (Auto Scaling groups), and NODE_GROUP_TYPE=managed: a cluster has one node group type. Set NODE_GROUP_TYPE=self-managed to match the cluster, or %s",
			cluster, strings.Join(owned, ", "), recreate)
	}
	if r.cluster == nil && len(owned) > 0 {
		return fmt.Errorf("the Auto Scaling groups %s exist, but cluster %s does not, and their nodes cannot join a new one; delete them with setup-aws teardown and run again",
			strings.Join(owned, ", "), cluster)
	}
	return nil
}

// clusterSecurityGroup is the security group EKS made for the cluster,
// which self-managed nodes use as a managed group's nodes do. Under --plan
// before the cluster exists it is a placeholder.
func (r *clusterRun) clusterSecurityGroup() (string, error) {
	if r.cluster != nil && r.cluster.ResourcesVpcConfig != nil {
		if id := aws.ToString(r.cluster.ResourcesVpcConfig.ClusterSecurityGroupId); id != "" {
			return id, nil
		}
	}
	if r.plan() {
		return plannedPrefix + "cluster-security-group", nil
	}
	return "", fmt.Errorf("cluster %s reports no cluster security group", r.e.names.Cluster)
}

// createASGInput is group g's Auto Scaling group: its launch template's
// version, the group's size, the private subnets, and the deployment's
// tags, which every instance gets at launch.
func (r *clusterRun) createASGInput(g nodeGroupSpec) *autoscaling.CreateAutoScalingGroupInput {
	lt := r.templates[g.Name]
	name := r.asgName(g.Name)
	tags := map[string]string{}
	for k, v := range r.tags {
		tags[k] = v
	}
	tags["Name"] = name
	tags["kubernetes.io/cluster/"+r.e.names.Cluster] = "owned"
	var at []asgtypes.Tag
	for _, k := range sortedKeys(tags) {
		at = append(at, asgtypes.Tag{Key: aws.String(k), Value: aws.String(tags[k]), PropagateAtLaunch: aws.Bool(true)})
	}
	return &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(name),
		LaunchTemplate: &asgtypes.LaunchTemplateSpecification{LaunchTemplateId: aws.String(lt.id),
			Version: aws.String(strconv.FormatInt(lt.version, 10))},
		MinSize:           aws.Int32(g.Min),
		DesiredCapacity:   aws.Int32(g.Desired),
		MaxSize:           aws.Int32(g.Max),
		VPCZoneIdentifier: aws.String(strings.Join(r.private, ",")),
		HealthCheckType:   aws.String("EC2"),
		Tags:              at,
	}
}

// ensureAutoScalingGroups creates the Auto Scaling group of every
// self-managed node group that does not exist. One that exists is left as
// it is, as a managed node group is. It does not wait for them, so the
// groups come up together (waitSelfManaged).
func (r *clusterRun) ensureAutoScalingGroups(ctx context.Context) error {
	for _, g := range r.groups() {
		name := r.asgName(g.Name)
		if a := r.asgs[g.Name]; a != nil {
			slog.Info("Auto Scaling group exists", "nodegroup", g.Name, "asg", name, "desired", aws.ToInt32(a.DesiredCapacity))
			continue
		}
		in := r.createASGInput(g)
		if err := r.create(fmt.Sprintf("Auto Scaling group %s (%d x %s)", name, g.Desired, g.InstanceType), func() error {
			return retryNewProfile(ctx, func() error {
				_, err := r.c.ASG.CreateAutoScalingGroup(ctx, in)
				return err
			})
		}); err != nil {
			return fmt.Errorf("create Auto Scaling group %s: %w", name, err)
		}
	}
	return nil
}

// retryNewProfile retries do while EC2 cannot use an instance profile yet:
// CreateAutoScalingGroup checks the launch template's profile, and one made
// seconds ago is not visible to EC2 everywhere at once.
func retryNewProfile(ctx context.Context, do func() error) error {
	return retryWhile(ctx, "the new instance profile is not usable by EC2 yet; retrying", func(ae smithy.APIError) bool {
		msg := strings.ToLower(ae.ErrorMessage())
		return ae.ErrorCode() == "ValidationError" && (strings.Contains(msg, "instance profile") || strings.Contains(msg, "iaminstanceprofile"))
	}, do)
}

// retryRoleNotFound retries do while IAM does not find role yet:
// AddRoleToInstanceProfile seconds after CreateRole can answer NoSuchEntity
// ("The role with name ... cannot be found"). The profile has the role's
// name, so the message must name it as a role: a missing profile is not
// retried.
func retryRoleNotFound(ctx context.Context, role string, do func() error) error {
	return retryWhile(ctx, "IAM does not find the new role yet; retrying", func(ae smithy.APIError) bool {
		msg := ae.ErrorMessage()
		return ae.ErrorCode() == "NoSuchEntity" && strings.Contains(msg, role) &&
			strings.Contains(strings.ToLower(strings.ReplaceAll(msg, role, "")), "role")
	}, do)
}

// retryWhile calls do until it does not fail with an AWS error that retry
// accepts, at most 21 times, pollEvery apart.
func retryWhile(ctx context.Context, why string, retry func(smithy.APIError) bool, do func() error) error {
	for i := 0; ; i++ {
		err := do()
		var ae smithy.APIError
		if err == nil || i >= 20 || !errors.As(err, &ae) || !retry(ae) {
			return err
		}
		slog.Info(why, "error", ae.ErrorMessage())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// describeASG reads Auto Scaling group name, or nil when it does not exist.
func (r *clusterRun) describeASG(ctx context.Context, name string) (*asgtypes.AutoScalingGroup, error) {
	out, err := r.c.ASG.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{name}})
	if err != nil {
		return nil, fmt.Errorf("describe Auto Scaling group %s: %w", name, err)
	}
	for i := range out.AutoScalingGroups {
		if aws.ToString(out.AutoScalingGroups[i].AutoScalingGroupName) == name {
			return &out.AutoScalingGroups[i], nil
		}
	}
	return nil, nil
}

func inService(a *asgtypes.AutoScalingGroup) int {
	n := 0
	for _, i := range a.Instances {
		if i.LifecycleState == asgtypes.LifecycleStateInService {
			n++
		}
	}
	return n
}

// lastScalingFailure is the message of the newest failed scaling activity of
// Auto Scaling group name (why it could not launch an instance), or "".
func (r *clusterRun) lastScalingFailure(ctx context.Context, name string) string {
	out, err := r.c.ASG.DescribeScalingActivities(ctx, &autoscaling.DescribeScalingActivitiesInput{
		AutoScalingGroupName: aws.String(name), MaxRecords: aws.Int32(20)})
	if err != nil {
		return "the scaling activities could not be read: " + err.Error()
	}
	for _, a := range out.Activities {
		if a.StatusCode == asgtypes.ScalingActivityStatusCodeFailed {
			if m := aws.ToString(a.StatusMessage); m != "" {
				return m
			}
			return aws.ToString(a.Description)
		}
	}
	return ""
}

// nodeWaitTimeout bounds each of a self-managed group's two waits.
// Replaced in tests.
var nodeWaitTimeout = 30 * time.Minute

// waitSelfManaged waits, group by group, until the Auto Scaling group has
// its desired instances InService and as many nodes of the group
// (ate.dev/pool=<group>) are Ready in the Kubernetes API (and for the nitro
// group until they advertise Nitro sandbox capacity, waitNitroCapacity),
// then for the add-ons, which need the nodes.
func (r *clusterRun) waitSelfManaged(ctx context.Context) error {
	if r.plan() {
		slog.Info("plan: would wait for the self-managed nodes to be Ready and the add-ons to be ACTIVE")
		return nil
	}
	if len(r.groups()) == 0 {
		return r.waitAddons(ctx)
	}
	kube, err := r.c.Kube(ctx, r.cluster)
	if err != nil {
		return err
	}
	for _, g := range r.groups() {
		name := r.asgName(g.Name)
		var desired int32
		var readErr error
		// DescribeAutoScalingGroups can miss a group for a while after
		// CreateAutoScalingGroup: that is "not yet", and absent keeps it for
		// the timeout.
		absent := false
		err := waitFor(ctx, "the instances of Auto Scaling group "+name+" to be InService", nodeWaitTimeout, func() (bool, error) {
			a, err := r.describeASG(ctx, name)
			absent = err == nil && a == nil
			switch {
			case err != nil:
				readErr = err
			case a == nil:
				slog.Info("DescribeAutoScalingGroups does not return the group yet; retrying", "asg", name)
				return false, nil
			case aws.ToString(a.Status) != "":
				readErr = fmt.Errorf("Auto Scaling group %s is %s", name, strings.ToLower(aws.ToString(a.Status)))
			default:
				desired = aws.ToInt32(a.DesiredCapacity)
				return inService(a) >= int(desired), nil
			}
			return false, readErr
		})
		if err != nil {
			switch {
			case readErr != nil || ctx.Err() != nil:
			case absent:
				err = fmt.Errorf("%w; DescribeAutoScalingGroups does not return Auto Scaling group %s", err, name)
			default:
				if why := r.lastScalingFailure(ctx, name); why != "" {
					err = fmt.Errorf("%w; the last failed scaling activity: %s", err, why)
				}
			}
			return err
		}
		slog.Info("instances InService", "asg", name, "instances", desired)
		sel := "ate.dev/pool=" + g.Name
		ready := 0
		readErr = nil
		// A blip (no answer, 429, a 5xx) is "not yet", and lastErr keeps it
		// for the timeout until a list answers. 401, 403 and other answers
		// stop the wait.
		var lastErr error
		err = waitFor(ctx, fmt.Sprintf("%d nodes %s to be Ready", desired, sel), nodeWaitTimeout, func() (bool, error) {
			nodes, err := kube.Nodes(ctx, sel)
			if kubeTransient(err) {
				lastErr = err
				slog.Info("the Kubernetes API did not answer the node list; retrying", "error", err)
				return false, nil
			}
			if err != nil {
				readErr = err
				return false, err
			}
			lastErr = nil
			ready = 0
			for _, n := range nodes {
				if n.Ready {
					ready++
				}
			}
			return ready >= int(desired), nil
		})
		if err != nil {
			switch {
			case readErr != nil || ctx.Err() != nil:
			case lastErr != nil:
				err = fmt.Errorf("%w; the last list of the nodes failed: %v", err, lastErr)
			default:
				err = fmt.Errorf("%w; %d of %d nodes %s are Ready: a node that never joins has the wrong NodeConfig, AMI or access entry, see its console output (aws ec2 get-console-output)",
					err, ready, desired, sel)
			}
			return err
		}
		slog.Info("nodes Ready", "nodegroup", g.Name, "ready", ready)
		// Not a nitro NodePool: those are not among r.groups().
		if g.Name == nodegroups.Nitro {
			if err := r.waitNitroCapacity(ctx, kube, desired); err != nil {
				return err
			}
		}
	}
	return r.waitAddons(ctx)
}
