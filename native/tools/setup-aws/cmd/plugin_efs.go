package cmd

// `setup-aws plugin efs`: the AWS half of the installer's efs plugin
// (installer/internal/plugin/efs). It makes
//
//   - one encrypted Amazon EFS file system, creation token <cluster>-actors;
//   - the security group <cluster>-efs, which admits NFS (tcp 2049) only from
//     the EKS cluster security group;
//   - a mount target in the Availability Zone of each private subnet, the
//     file system's endpoint for the nodes in that zone;
//   - the Pod Identity role of the EFS CSI controllers, which may manage
//     access points on this file system only;
//   - the aws-efs-csi-driver EKS add-on, whose node plugin also mounts
//     /var/lib/ate so the mounts it makes are visible to atelet.
//
// The installer's half adds Substrate's own CSI controller (ate-api-server
// calls CreateVolume itself, over mTLS), the CSIDriverConfig and the
// StorageClass efs-ate, so each actor gets its own EFS access point. The
// s3files plugin reuses the add-on and the controller role (plugin_s3files.go).
//
//	https://docs.aws.amazon.com/eks/latest/userguide/efs-csi.html
//	https://docs.aws.amazon.com/efs/latest/ug/efs-access-points.html

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

const (
	efsAddon = "aws-efs-csi-driver"
	// efsAddonVersion is config.env's EFS_ADDON_VERSION, which overrides it.
	efsAddonVersion = "v3.5.3-eksbuild.1"
	csiNamespace    = "kube-system"
	efsControllerSA = "efs-csi-controller-sa"
	nfsPort         = 2049
)

// efsAddonConfig is the add-on's configuration: its node plugin mounts each
// volume under /var/lib/ate, where atelet keeps actor directories, and
// Bidirectional propagation makes those mounts visible on the host.
//
//	https://kubernetes.io/docs/concepts/storage/volumes/#mount-propagation
const efsAddonConfig = `{"node":{"volumeMounts":[{"mountPath":"/var/lib/ate","mountPropagation":"Bidirectional","name":"ate-base"}],` +
	`"volumes":[{"hostPath":{"path":"/var/lib/ate","type":"DirectoryOrCreate"},"name":"ate-base"}]}}`

// Names of the efs plugin's resources.
func (n Names) EFSToken() string          { return n.Cluster + "-actors" }
func (n Names) EFSGroup() string          { return n.Cluster + "-efs" }
func (n Names) EFSControllerRole() string { return n.IAMPrefix + "-" + efsControllerSA }

// The APIs the efs and s3files plugins call beyond the shared clients.
type (
	efsAPI interface {
		DescribeFileSystems(context.Context, *efs.DescribeFileSystemsInput, ...func(*efs.Options)) (*efs.DescribeFileSystemsOutput, error)
		CreateFileSystem(context.Context, *efs.CreateFileSystemInput, ...func(*efs.Options)) (*efs.CreateFileSystemOutput, error)
		DeleteFileSystem(context.Context, *efs.DeleteFileSystemInput, ...func(*efs.Options)) (*efs.DeleteFileSystemOutput, error)
		DescribeMountTargets(context.Context, *efs.DescribeMountTargetsInput, ...func(*efs.Options)) (*efs.DescribeMountTargetsOutput, error)
		CreateMountTarget(context.Context, *efs.CreateMountTargetInput, ...func(*efs.Options)) (*efs.CreateMountTargetOutput, error)
		DeleteMountTarget(context.Context, *efs.DeleteMountTargetInput, ...func(*efs.Options)) (*efs.DeleteMountTargetOutput, error)
		DescribeAccessPoints(context.Context, *efs.DescribeAccessPointsInput, ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error)
		DeleteAccessPoint(context.Context, *efs.DeleteAccessPointInput, ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error)
	}
	// clusterAPI reads the cluster's network and manages its add-ons.
	clusterAPI interface {
		DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
		DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
		CreateAddon(context.Context, *eks.CreateAddonInput, ...func(*eks.Options)) (*eks.CreateAddonOutput, error)
		UpdateAddon(context.Context, *eks.UpdateAddonInput, ...func(*eks.Options)) (*eks.UpdateAddonOutput, error)
		DeleteAddon(context.Context, *eks.DeleteAddonInput, ...func(*eks.Options)) (*eks.DeleteAddonOutput, error)
	}
)

type efsClients struct {
	EFS efsAPI
	EC2 EC2API
	EKS clusterAPI
}

// efsClientsFor is replaced in tests.
var efsClientsFor = func(ctx context.Context) (*efsClients, error) {
	pc, err := newPluginClients(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &efsClients{EFS: pc.EFS, EC2: pc.EC2, EKS: pc.EKS}, nil
}

func init() {
	registerPlugin(pluginDef{
		ID:           "efs",
		Short:        "Create the Amazon EFS file system, its mount targets and security group, the EFS CSI add-on and its Pod Identity role, for per-actor EFS volumes",
		Create:       createEFS,
		Teardown:     efsTeardown,
		Inventory:    efsInventory,
		TaggingTypes: []string{"elasticfilesystem:file-system", "elasticfilesystem:access-point"},
	})
}

func createEFS(ctx context.Context, e *env) error {
	if e.cfg.Plan {
		slog.Info("plan mode: reading only; nothing will be created or changed")
	}
	c, err := efsClientsFor(ctx)
	if err != nil {
		return err
	}
	net, err := clusterNetwork(ctx, c.EKS, c.EC2, e.names.Cluster)
	if err != nil {
		return err
	}
	fsID, err := e.ensureEFS(ctx, c.EFS)
	if err != nil {
		return err
	}
	sg, err := e.ensureNFSGroup(ctx, c.EC2, net, e.names.EFSGroup(), "EFS")
	if err != nil {
		return err
	}
	if err := e.ensureEFSMountTargets(ctx, c.EFS, fsID, sg, net.subnets); err != nil {
		return err
	}
	if err := e.roleForSA(ctx, PodIdentity{csiNamespace, efsControllerSA, e.efsControllerPolicy(fsID)}); err != nil {
		return err
	}
	if err := e.ensureEFSAddon(ctx, c.EKS); err != nil {
		return err
	}
	if !e.cfg.Plan {
		slog.Info("EFS ready", "fileSystem", fsID, "securityGroup", sg)
	}
	return nil
}

// nfsNet is the cluster's network, as the NFS plugins need it.
type nfsNet struct {
	vpc, clusterSG string
	subnets        []ec2types.Subnet // the private ones
}

// clusterNetwork reads the cluster's VPC and cluster security group (EKS
// attaches it to every node, so a rule admitting it admits the pods) and its
// private subnets, tagged kubernetes.io/role/internal-elb=1 as create cluster
// and eksctl both tag them.
func clusterNetwork(ctx context.Context, k clusterAPI, c EC2API, cluster string) (nfsNet, error) {
	var n nfsNet
	cl, err := k.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(cluster)})
	if err != nil {
		return n, fmt.Errorf("describe cluster %s: %w", cluster, err)
	}
	if vc := cl.Cluster.ResourcesVpcConfig; vc != nil {
		n.vpc, n.clusterSG = aws.ToString(vc.VpcId), aws.ToString(vc.ClusterSecurityGroupId)
	}
	if n.vpc == "" || n.clusterSG == "" {
		return n, fmt.Errorf("cluster %s has no VPC or cluster security group yet", cluster)
	}
	out, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []ec2types.Filter{
		flt("vpc-id", n.vpc), flt("tag:"+dbSubnetTagKey, "1")}})
	if err != nil {
		return n, fmt.Errorf("describe subnets of %s: %w", n.vpc, err)
	}
	n.subnets = out.Subnets
	if len(n.subnets) == 0 {
		return n, fmt.Errorf("no private subnet (tag %s=1) in %s", dbSubnetTagKey, n.vpc)
	}
	sort.Slice(n.subnets, func(i, j int) bool {
		return aws.ToString(n.subnets[i].AvailabilityZoneId)+aws.ToString(n.subnets[i].SubnetId) <
			aws.ToString(n.subnets[j].AvailabilityZoneId)+aws.ToString(n.subnets[j].SubnetId)
	})
	return n, nil
}

// ensureNFSGroup makes the named security group in the cluster's VPC and
// admits tcp 2049 from the cluster security group. Any other ingress rule
// stops the run, which never removes a rule. It returns the group's id, ""
// under --plan when the group is not there yet.
func (e *env) ensureNFSGroup(ctx context.Context, c EC2API, net nfsNet, name, what string) (string, error) {
	g, err := findSG(ctx, c, net.vpc, name)
	if err != nil {
		return "", err
	}
	id := ""
	if g == nil {
		if err := e.act("create security group "+name+" in "+net.vpc, func() error {
			out, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
				GroupName:         aws.String(name),
				VpcId:             aws.String(net.vpc),
				Description:       aws.String(e.names.Cluster + " " + what + ": 2049 from the EKS cluster security group only"),
				TagSpecifications: e.ec2Tags(ec2types.ResourceTypeSecurityGroup),
			})
			if err == nil {
				id = aws.ToString(out.GroupId)
			}
			return err
		}); err != nil {
			return "", fmt.Errorf("create security group %s: %w", name, err)
		}
	} else {
		id = aws.ToString(g.GroupId)
		if err := e.owned("security group "+name+" ("+id+")", sgTagMap(g.Tags)); err != nil {
			return "", err
		}
	}
	var perms []ec2types.IpPermission
	if g != nil {
		perms = g.IpPermissions
	}
	ok, err := nfsIngressOK(perms, net.clusterSG)
	if err != nil {
		return "", fmt.Errorf("security group %s (%s): %w", name, id, err)
	}
	if !ok {
		if err := e.act(fmt.Sprintf("allow tcp/%d into %s from the cluster security group %s", nfsPort, name, net.clusterSG), func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
				GroupId: aws.String(id),
				IpPermissions: []ec2types.IpPermission{{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(nfsPort), ToPort: aws.Int32(nfsPort),
					UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(net.clusterSG)}}}},
				TagSpecifications: e.ec2Tags(ec2types.ResourceTypeSecurityGroupRule),
			})
			return err
		}); err != nil {
			return "", err
		}
	}
	return id, e.tagSGRules(ctx, c, name, id)
}

// nfsIngressOK checks an NFS group's ingress rules: each must be tcp 2049
// from the cluster security group only, with no address range or prefix
// list. It reports whether that rule is there, or an error naming the first
// other rule.
func nfsIngressOK(perms []ec2types.IpPermission, clusterSG string) (bool, error) {
	found := false
	for _, p := range perms {
		bad := aws.ToString(p.IpProtocol) != "tcp" || aws.ToInt32(p.FromPort) != nfsPort || aws.ToInt32(p.ToPort) != nfsPort ||
			len(p.IpRanges) > 0 || len(p.Ipv6Ranges) > 0 || len(p.PrefixListIds) > 0 || len(p.UserIdGroupPairs) == 0
		for _, g := range p.UserIdGroupPairs {
			bad = bad || aws.ToString(g.GroupId) != clusterSG
		}
		if bad {
			return false, fmt.Errorf("has an ingress rule other than tcp/%d from %s: %s; remove it yourself, this tool never removes a rule",
				nfsPort, clusterSG, describePerm(p))
		}
		found = true
	}
	return found, nil
}

func efsTags(m map[string]string) []efstypes.Tag {
	out := make([]efstypes.Tag, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, efstypes.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

func efsTagMap(ts []efstypes.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// efsGone adds EFS's own not-found codes to tdGone.
func efsGone(err error) bool {
	return tdGone(err) || hasCode(err, "FileSystemNotFound", "MountTargetNotFound", "AccessPointNotFound")
}

// findEFS is the file system with token, or nil.
func findEFS(ctx context.Context, c efsAPI, token string) (*efstypes.FileSystemDescription, error) {
	out, err := c.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{CreationToken: aws.String(token)})
	if efsGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe EFS file system %s: %w", token, err)
	}
	for i := range out.FileSystems {
		if out.FileSystems[i].LifeCycleState != efstypes.LifeCycleStateDeleted {
			return &out.FileSystems[i], nil
		}
	}
	return nil, nil
}

// ensureEFS returns the file system's id, made encrypted with elastic
// throughput if it is not there; "" under --plan when it would be made.
func (e *env) ensureEFS(ctx context.Context, c efsAPI) (string, error) {
	token := e.names.EFSToken()
	fs, err := findEFS(ctx, c, token)
	if err != nil {
		return "", err
	}
	if fs == nil {
		tags := map[string]string{"Name": e.names.Cluster}
		for k, v := range e.tags {
			tags[k] = v
		}
		if err := e.act("create EFS file system "+token+" (encrypted, elastic throughput)", func() error {
			out, err := c.CreateFileSystem(ctx, &efs.CreateFileSystemInput{CreationToken: aws.String(token), Encrypted: aws.Bool(true),
				PerformanceMode: efstypes.PerformanceModeGeneralPurpose, ThroughputMode: efstypes.ThroughputModeElastic, Tags: efsTags(tags)})
			if err == nil {
				fs = &efstypes.FileSystemDescription{FileSystemId: out.FileSystemId, LifeCycleState: out.LifeCycleState}
			}
			return err
		}); err != nil {
			return "", fmt.Errorf("create EFS file system %s: %w", token, err)
		}
		if fs == nil {
			return "", nil // --plan
		}
	} else if err := e.owned("EFS file system "+aws.ToString(fs.FileSystemId)+" ("+token+")", efsTagMap(fs.Tags)); err != nil {
		return "", err
	}
	id := aws.ToString(fs.FileSystemId)
	if fs.LifeCycleState == efstypes.LifeCycleStateDeleting {
		return "", fmt.Errorf("EFS file system %s (%s) is being deleted; run again once it is gone", id, token)
	}
	if fs.LifeCycleState == efstypes.LifeCycleStateAvailable {
		return id, nil
	}
	return id, waitFor(ctx, "EFS file system "+id+" to be available", 10*time.Minute, func() (bool, error) {
		out, err := c.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{FileSystemId: aws.String(id)})
		if err != nil {
			return false, fmt.Errorf("describe EFS file system %s: %w", id, err)
		}
		if len(out.FileSystems) == 0 {
			return false, fmt.Errorf("EFS file system %s is gone", id)
		}
		switch st := out.FileSystems[0].LifeCycleState; st {
		case efstypes.LifeCycleStateAvailable:
			return true, nil
		case efstypes.LifeCycleStateError, efstypes.LifeCycleStateDeleting, efstypes.LifeCycleStateDeleted:
			return false, fmt.Errorf("EFS file system %s is %s", id, st)
		}
		return false, nil
	})
}

func efsMountTargets(ctx context.Context, c efsAPI, fsID string) ([]efstypes.MountTargetDescription, error) {
	var out []efstypes.MountTargetDescription
	in := &efs.DescribeMountTargetsInput{FileSystemId: aws.String(fsID)}
	for {
		page, err := c.DescribeMountTargets(ctx, in)
		if efsGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("describe mount targets of %s: %w", fsID, err)
		}
		out = append(out, page.MountTargets...)
		if aws.ToString(page.NextMarker) == "" {
			return out, nil
		}
		in.Marker = page.NextMarker
	}
}

// ensureEFSMountTargets makes a mount target in each private subnet's
// Availability Zone that has none, then waits until all are available. Nodes
// mount through the one in their own zone.
//
//	https://docs.aws.amazon.com/efs/latest/ug/accessing-fs.html
func (e *env) ensureEFSMountTargets(ctx context.Context, c efsAPI, fsID, sg string, subnets []ec2types.Subnet) error {
	if fsID == "" {
		slog.Info("plan: would create a mount target in each private subnet's Availability Zone")
		return nil
	}
	mts, err := efsMountTargets(ctx, c, fsID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, m := range mts {
		have[aws.ToString(m.AvailabilityZoneId)] = true
	}
	for _, s := range subnets {
		az, id := aws.ToString(s.AvailabilityZoneId), aws.ToString(s.SubnetId)
		if have[az] {
			continue
		}
		have[az] = true
		if err := e.act("create EFS mount target in "+aws.ToString(s.AvailabilityZone)+" ("+id+")", func() error {
			_, err := c.CreateMountTarget(ctx, &efs.CreateMountTargetInput{FileSystemId: aws.String(fsID), SubnetId: aws.String(id), SecurityGroups: []string{sg}})
			return err
		}); err != nil {
			return fmt.Errorf("create EFS mount target in %s: %w", id, err)
		}
	}
	if e.cfg.Plan {
		return nil
	}
	return waitFor(ctx, "the EFS mount targets to be available", 10*time.Minute, func() (bool, error) {
		mts, err := efsMountTargets(ctx, c, fsID)
		if err != nil {
			return false, err
		}
		for _, m := range mts {
			switch m.LifeCycleState {
			case efstypes.LifeCycleStateAvailable:
			case efstypes.LifeCycleStateError, efstypes.LifeCycleStateDeleting, efstypes.LifeCycleStateDeleted:
				return false, fmt.Errorf("EFS mount target %s is %s", aws.ToString(m.MountTargetId), m.LifeCycleState)
			default:
				return false, nil
			}
		}
		return len(mts) > 0, nil
	})
}

// csiClusterTag is the tag the EFS CSI driver puts on every access point it
// makes; the controllers' policies require it.
const csiClusterTag = "efs.csi.aws.com/cluster"

// accessPointTagKeys are the tag keys an access point may be created with:
// the driver's own, plus the owner tag and TAGS, which Substrate's
// controller adds (--tags, installer/internal/plugin/efs).
func (e *env) accessPointTagKeys() string {
	keys := append([]string{csiClusterTag}, sortedKeys(e.tags)...)
	b, _ := json.Marshal(keys)
	return string(b)
}

// efsControllerPolicy lets the EFS CSI controllers (the add-on's and
// Substrate's, which share efs-csi-controller-sa) create access points on
// this file system only, carrying the efs.csi.aws.com/cluster tag and
// otherwise only this deployment's tags, and delete only access points that
// carry the driver's tag.
func (e *env) efsControllerPolicy(fsID string) string {
	n := e.names
	if fsID == "" {
		fsID = "fs-tbd" // --plan before the file system exists; never written
	}
	fsARN := fmt.Sprintf("arn:%s:elasticfilesystem:%s:%s:file-system/%s", n.Partition, n.Region, n.Account, fsID)
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["elasticfilesystem:DescribeAccessPoints","elasticfilesystem:DescribeFileSystems",`+
		`"elasticfilesystem:DescribeMountTargets","ec2:DescribeAvailabilityZones"],"Resource":"*"},`+
		`{"Effect":"Allow","Action":"elasticfilesystem:CreateAccessPoint","Resource":"%[1]s",`+
		`"Condition":{"Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},"ForAllValues:StringEquals":{"aws:TagKeys":%[5]s}}},`+
		`{"Effect":"Allow","Action":"elasticfilesystem:TagResource","Resource":"%[1]s",`+
		`"Condition":{"StringEquals":{"elasticfilesystem:CreateAction":"CreateAccessPoint"},"Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},`+
		`"ForAllValues:StringEquals":{"aws:TagKeys":%[5]s}}},`+
		`{"Effect":"Allow","Action":"elasticfilesystem:DeleteAccessPoint","Resource":"arn:%[2]s:elasticfilesystem:%[3]s:%[4]s:access-point/*",`+
		`"Condition":{"StringEquals":{"aws:ResourceTag/efs.csi.aws.com/cluster":"true"}}}]}`,
		fsARN, n.Partition, n.Region, n.Account, e.accessPointTagKeys())
}

// describeAddon is the add-on, or nil when it (or the cluster) is not there.
func describeAddon(ctx context.Context, c clusterAPI, cluster, name string) (*ekstypes.Addon, error) {
	out, err := c.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(cluster), AddonName: aws.String(name)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe add-on %s: %w", name, err)
	}
	return out.Addon, nil
}

// ensureEFSAddon creates the aws-efs-csi-driver add-on at the pinned
// version with efsAddonConfig, or updates an owned one that differs, and
// waits until it is ACTIVE at that version.
func (e *env) ensureEFSAddon(ctx context.Context, c clusterAPI) error {
	version := getEnv("EFS_ADDON_VERSION", efsAddonVersion)
	cluster := aws.String(e.names.Cluster)
	cur, err := describeAddon(ctx, c, e.names.Cluster, efsAddon)
	if err != nil {
		return err
	}
	switch {
	case cur == nil:
		if err := e.act("create EKS add-on "+efsAddon+" "+version, func() error {
			_, err := c.CreateAddon(ctx, &eks.CreateAddonInput{ClusterName: cluster, AddonName: aws.String(efsAddon),
				AddonVersion: aws.String(version), ConfigurationValues: aws.String(efsAddonConfig),
				ResolveConflicts: ekstypes.ResolveConflictsOverwrite, Tags: e.tags})
			return err
		}); err != nil {
			return fmt.Errorf("create add-on %s: %w", efsAddon, err)
		}
	default:
		if err := e.owned("EKS add-on "+efsAddon, cur.Tags); err != nil {
			return err
		}
		if cur.Status == ekstypes.AddonStatusCreateFailed {
			return fmt.Errorf("add-on %s is CREATE_FAILED (%s); delete it (setup-aws plugin efs --delete) and run again", efsAddon, addonIssues(cur))
		}
		if aws.ToString(cur.AddonVersion) == version && sameJSON(aws.ToString(cur.ConfigurationValues), efsAddonConfig) {
			slog.Info("add-on exists", "addon", efsAddon, "version", version)
			break
		}
		if err := e.act("update EKS add-on "+efsAddon+" to "+version+" with the /var/lib/ate mount", func() error {
			_, err := c.UpdateAddon(ctx, &eks.UpdateAddonInput{ClusterName: cluster, AddonName: aws.String(efsAddon),
				AddonVersion: aws.String(version), ConfigurationValues: aws.String(efsAddonConfig), ResolveConflicts: ekstypes.ResolveConflictsOverwrite})
			return err
		}); err != nil {
			return fmt.Errorf("update add-on %s: %w", efsAddon, err)
		}
	}
	if e.cfg.Plan {
		return nil
	}
	var last *ekstypes.Addon
	err = waitFor(ctx, "add-on "+efsAddon+" to be ACTIVE", 20*time.Minute, func() (bool, error) {
		a, err := describeAddon(ctx, c, e.names.Cluster, efsAddon)
		if err != nil {
			return false, err
		}
		if a == nil {
			return false, errors.New("add-on " + efsAddon + " is gone")
		}
		last = a
		switch a.Status {
		case ekstypes.AddonStatusActive:
			return aws.ToString(a.AddonVersion) == version, nil
		case ekstypes.AddonStatusCreateFailed, ekstypes.AddonStatusUpdateFailed, ekstypes.AddonStatusDeleting, ekstypes.AddonStatusDeleteFailed:
			return false, fmt.Errorf("add-on %s is %s (%s)", efsAddon, a.Status, addonIssues(a))
		}
		return false, nil
	})
	if err != nil && last != nil {
		err = fmt.Errorf("%w; last status %s (%s)", err, last.Status, addonIssues(last))
	}
	return err
}

// ---- teardown and inventory --------------------------------------------------

func efsTeardown(e *tdEnv) []pluginStep {
	return []pluginStep{
		// The add-on first: with its controllers gone nothing makes another
		// access point while the file system is deleted.
		{Step: e.stepAddon(efsAddon)},
		{Step: e.stepCSIAssociation(efsControllerSA)},
		{Step: func(ctx context.Context) ([]tdTarget, []string, error) {
			return e.roleTargets(ctx, []string{e.n.EFSControllerRole()}, true)
		}},
		{Step: e.stepEFSAccessPoints},
		{Step: e.stepEFSMountTargets},
		{Step: e.stepEFSFileSystem},
		// Last: the mount targets' network interfaces hold the group until
		// they are gone (sgTarget retries).
		{Step: e.stepNamedSG(e.n.EFSGroup())},
	}
}

// stepAddon deletes an owned add-on of the cluster and waits until it is gone.
func (e *tdEnv) stepAddon(name string) tdStep {
	return func(ctx context.Context) ([]tdTarget, []string, error) {
		cl, err := e.cluster(ctx)
		if err != nil || cl == nil {
			return nil, nil, err
		}
		c, err := efsClientsFor(ctx)
		if err != nil {
			return nil, nil, err
		}
		a, err := describeAddon(ctx, c.EKS, e.n.Cluster, name)
		if err != nil || a == nil {
			return nil, nil, err
		}
		if r := e.refuse("EKS add-on", name, a.Tags); r != "" {
			return nil, []string{r}, nil
		}
		t := tdTarget{kind: "EKS add-on", id: name, wait: gone(func(ctx context.Context) (bool, error) {
			a, err := describeAddon(ctx, c.EKS, e.n.Cluster, name)
			return a != nil, err
		})}
		if a.Status != ekstypes.AddonStatusDeleting {
			t.del = func(ctx context.Context) error {
				_, err := c.EKS.DeleteAddon(ctx, &eks.DeleteAddonInput{ClusterName: aws.String(e.n.Cluster), AddonName: aws.String(name)})
				return err
			}
		}
		return []tdTarget{t}, nil, nil
	}
}

// csiAssociations are the Pod Identity associations of kube-system/sa.
func (e *tdEnv) csiAssociations(ctx context.Context, sa string) ([]*ekstypes.PodIdentityAssociation, error) {
	as, err := e.associations(ctx)
	if err != nil {
		return nil, err
	}
	var out []*ekstypes.PodIdentityAssociation
	for _, a := range as {
		if aws.ToString(a.Namespace) == csiNamespace && aws.ToString(a.ServiceAccount) == sa {
			out = append(out, a)
		}
	}
	return out, nil
}

// stepCSIAssociation deletes the Pod Identity associations of kube-system/sa
// (core teardown leaves kube-system's to the cluster).
func (e *tdEnv) stepCSIAssociation(sa string) tdStep {
	return func(ctx context.Context) ([]tdTarget, []string, error) {
		as, err := e.csiAssociations(ctx, sa)
		if err != nil {
			return nil, nil, err
		}
		var ts []tdTarget
		var rs []string
		for _, a := range as {
			label := assocLabel(a)
			if r := e.refuse("Pod Identity association", label, a.Tags); r != "" {
				rs = append(rs, r)
				continue
			}
			id := a.AssociationId
			ts = append(ts, tdTarget{kind: "Pod Identity association", id: label, del: func(ctx context.Context) error {
				_, err := e.c.EKS.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(e.n.Cluster), AssociationId: id})
				return err
			}})
		}
		return ts, rs, nil
	}
}

// stepNamedSG deletes the security groups named name (in any VPC: the
// cluster may be gone), refusing one this deployment does not own.
func (e *tdEnv) stepNamedSG(name string) tdStep {
	return func(ctx context.Context) ([]tdTarget, []string, error) {
		sgs, err := e.ec2List(ctx, kSG, flt("group-name", name))
		if err != nil {
			return nil, nil, err
		}
		var ts []tdTarget
		var rs []string
		for _, sg := range sgs {
			label := sg.name + " (" + sg.id + ")"
			if r := e.refuse(kSG, label, sg.tags); r != "" {
				rs = append(rs, r)
				continue
			}
			ts = append(ts, e.sgTarget(sg.id, label))
		}
		return ts, rs, nil
	}
}

// ownedEFS is this deployment's file system, or nil when there is none or it
// is someone else's (stepEFSFileSystem refuses that one).
func (e *tdEnv) ownedEFS(ctx context.Context) (*efsClients, *efstypes.FileSystemDescription, error) {
	c, err := efsClientsFor(ctx)
	if err != nil {
		return nil, nil, err
	}
	fs, err := findEFS(ctx, c.EFS, e.n.EFSToken())
	if err != nil || fs == nil || !e.owned(efsTagMap(fs.Tags)) {
		return c, nil, err
	}
	return c, fs, nil
}

func efsAccessPoints(ctx context.Context, c efsAPI, fsID string) ([]efstypes.AccessPointDescription, error) {
	var out []efstypes.AccessPointDescription
	in := &efs.DescribeAccessPointsInput{FileSystemId: aws.String(fsID)}
	for {
		page, err := c.DescribeAccessPoints(ctx, in)
		if efsGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("describe access points of %s: %w", fsID, err)
		}
		out = append(out, page.AccessPoints...)
		if aws.ToString(page.NextToken) == "" {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}

// stepEFSAccessPoints deletes the file system's access points: the CSI
// driver makes one per actor volume, tagged only efs.csi.aws.com/cluster
// (its role may tag nothing else), so they belong to the owned file system.
func (e *tdEnv) stepEFSAccessPoints(ctx context.Context) ([]tdTarget, []string, error) {
	c, fs, err := e.ownedEFS(ctx)
	if err != nil || fs == nil {
		return nil, nil, err
	}
	aps, err := efsAccessPoints(ctx, c.EFS, aws.ToString(fs.FileSystemId))
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	for _, ap := range aps {
		id := aws.ToString(ap.AccessPointId)
		ts = append(ts, tdTarget{kind: "EFS access point", id: id, del: func(ctx context.Context) error {
			_, err := c.EFS.DeleteAccessPoint(ctx, &efs.DeleteAccessPointInput{AccessPointId: aws.String(id)})
			if efsGone(err) {
				return nil
			}
			return err
		}})
	}
	return ts, nil, nil
}

// stepEFSMountTargets deletes the mount targets and waits until each is gone:
// the file system cannot be deleted before.
func (e *tdEnv) stepEFSMountTargets(ctx context.Context) ([]tdTarget, []string, error) {
	c, fs, err := e.ownedEFS(ctx)
	if err != nil || fs == nil {
		return nil, nil, err
	}
	mts, err := efsMountTargets(ctx, c.EFS, aws.ToString(fs.FileSystemId))
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	for _, m := range mts {
		if m.LifeCycleState == efstypes.LifeCycleStateDeleted {
			continue
		}
		id := aws.ToString(m.MountTargetId)
		t := tdTarget{kind: "EFS mount target", id: id + " (" + aws.ToString(m.AvailabilityZoneName) + ")", wait: gone(func(ctx context.Context) (bool, error) {
			out, err := c.EFS.DescribeMountTargets(ctx, &efs.DescribeMountTargetsInput{MountTargetId: aws.String(id)})
			if efsGone(err) {
				return false, nil
			}
			return err == nil && len(out.MountTargets) > 0 && out.MountTargets[0].LifeCycleState != efstypes.LifeCycleStateDeleted, err
		})}
		if m.LifeCycleState != efstypes.LifeCycleStateDeleting {
			t.del = func(ctx context.Context) error {
				_, err := c.EFS.DeleteMountTarget(ctx, &efs.DeleteMountTargetInput{MountTargetId: aws.String(id)})
				if efsGone(err) {
					return nil
				}
				return err
			}
		}
		ts = append(ts, t)
	}
	return ts, nil, nil
}

// stepEFSFileSystem deletes the file system, refusing one with the creation
// token that this deployment does not own.
func (e *tdEnv) stepEFSFileSystem(ctx context.Context) ([]tdTarget, []string, error) {
	c, err := efsClientsFor(ctx)
	if err != nil {
		return nil, nil, err
	}
	fs, err := findEFS(ctx, c.EFS, e.n.EFSToken())
	if err != nil || fs == nil {
		return nil, nil, err
	}
	id := aws.ToString(fs.FileSystemId)
	label := id + " (" + e.n.EFSToken() + ")"
	if r := e.refuse("EFS file system", label, efsTagMap(fs.Tags)); r != "" {
		return nil, []string{r}, nil
	}
	t := tdTarget{kind: "EFS file system", id: label + " and its data", wait: gone(func(ctx context.Context) (bool, error) {
		out, err := c.EFS.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{FileSystemId: aws.String(id)})
		if efsGone(err) {
			return false, nil
		}
		return err == nil && len(out.FileSystems) > 0 && out.FileSystems[0].LifeCycleState != efstypes.LifeCycleStateDeleted, err
	})}
	if fs.LifeCycleState != efstypes.LifeCycleStateDeleting {
		t.del = func(ctx context.Context) error {
			// EFS refuses while a deleted mount target is still going away.
			err := retryOn(ctx, "EFS file system "+id+" to be free", func() error {
				_, err := c.EFS.DeleteFileSystem(ctx, &efs.DeleteFileSystemInput{FileSystemId: aws.String(id)})
				return err
			}, "FileSystemInUse")
			if efsGone(err) {
				return nil
			}
			return err
		}
	}
	return []tdTarget{t}, nil, nil
}

// roleRow is the inventory row of an IAM role, when it exists.
func (e *tdEnv) roleRow(ctx context.Context, name string) ([]tdRow, error) {
	r, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get role %s: %w", name, err)
	}
	return []tdRow{{Kind: "IAM role", ID: name, Tags: kvMap(r.Role.Tags, iamKV)}}, nil
}

// addonRows are the add-on's and kube-system/sa's association rows, while the
// cluster exists.
func (e *tdEnv) addonRows(ctx context.Context, addon string, sas ...string) ([]tdRow, error) {
	cl, err := e.cluster(ctx)
	if err != nil || cl == nil {
		return nil, err
	}
	var rows []tdRow
	if addon != "" {
		c, err := efsClientsFor(ctx)
		if err != nil {
			return nil, err
		}
		a, err := describeAddon(ctx, c.EKS, e.n.Cluster, addon)
		if err != nil {
			return nil, err
		}
		if a != nil {
			rows = append(rows, tdRow{Kind: "EKS add-on", ID: addon, Tags: a.Tags})
		}
	}
	for _, sa := range sas {
		as, err := e.csiAssociations(ctx, sa)
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			rows = append(rows, tdRow{Kind: "Pod Identity association", ID: assocLabel(a), Tags: a.Tags})
		}
	}
	return rows, nil
}

// sgRows are the rows of the security groups named name.
func (e *tdEnv) sgRows(ctx context.Context, name string) ([]tdRow, error) {
	sgs, err := e.ec2List(ctx, kSG, flt("group-name", name))
	if err != nil {
		return nil, err
	}
	var rows []tdRow
	for _, sg := range sgs {
		rows = append(rows, tdRow{Kind: kSG, ID: sg.name + " (" + sg.id + ")", Tags: sg.tags})
	}
	return rows, nil
}

// efsInventory lists what is left of the efs plugin, whatever its tags: the
// add-on, the association and role, the file system with its access points
// and mount targets, and the security group.
func efsInventory(ctx context.Context, e *tdEnv) ([]tdRow, error) {
	rows, err := e.addonRows(ctx, efsAddon, efsControllerSA)
	if err != nil {
		return nil, err
	}
	r, err := e.roleRow(ctx, e.n.EFSControllerRole())
	if err != nil {
		return nil, err
	}
	rows = append(rows, r...)
	c, err := efsClientsFor(ctx)
	if err != nil {
		return nil, err
	}
	fs, err := findEFS(ctx, c.EFS, e.n.EFSToken())
	if err != nil {
		return nil, err
	}
	if fs != nil {
		id, tags := aws.ToString(fs.FileSystemId), efsTagMap(fs.Tags)
		rows = append(rows, tdRow{Kind: "EFS file system", ID: id + " (" + e.n.EFSToken() + ")", Tags: tags})
		aps, err := efsAccessPoints(ctx, c.EFS, id)
		if err != nil {
			return nil, err
		}
		for _, ap := range aps {
			rows = append(rows, tdRow{Kind: "EFS access point", ID: aws.ToString(ap.AccessPointId), Tags: tags})
		}
		mts, err := efsMountTargets(ctx, c.EFS, id)
		if err != nil {
			return nil, err
		}
		for _, m := range mts {
			if m.LifeCycleState != efstypes.LifeCycleStateDeleted {
				rows = append(rows, tdRow{Kind: "EFS mount target", ID: aws.ToString(m.MountTargetId), Tags: tags})
			}
		}
	}
	sg, err := e.sgRows(ctx, e.n.EFSGroup())
	if err != nil {
		return nil, err
	}
	return append(rows, sg...), nil
}
