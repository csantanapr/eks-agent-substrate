package cmd

// `setup-aws plugin s3files`: the AWS half of the installer's s3files plugin
// (installer/internal/plugin/s3files). An S3 file system presents an S3 bucket over NFS;
// each actor gets its own S3 Files access point through the EFS CSI driver
// the efs plugin installs, and its files land in the bucket. It makes
//
//   - the bucket <cluster>-files-<account>-<region>: versioned (S3 Files
//     syncs a versioned bucket), encrypted, public access blocked, TLS only;
//   - the role S3 Files assumes to sync the bucket, <prefix>-s3files-sync;
//   - the S3 file system, the security group <cluster>-s3files (tcp 2049
//     from the cluster security group only) and a mount target per private
//     subnet's Availability Zone;
//   - an inline policy "s3files" on the efs plugin's controller role, so
//     the controllers manage S3 Files access points on this file system, and
//     the node plugin's Pod Identity role, which may mount it and read the
//     bucket.
//
//	https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files.html
//	https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files-prereq-policies.html
//	https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files-mounting-eks.html

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/s3files"
	s3ftypes "github.com/aws/aws-sdk-go-v2/service/s3files/types"
)

const efsNodeSA = "efs-csi-node-sa"

// Names of the s3files plugin's resources.
func (n Names) S3FilesBucket() string {
	return fmt.Sprintf("%s-files-%s-%s", n.Cluster, n.Account, n.Region)
}
func (n Names) S3FilesBucketARN() string { return "arn:" + n.Partition + ":s3:::" + n.S3FilesBucket() }
func (n Names) S3FilesGroup() string     { return n.Cluster + "-s3files" }
func (n Names) S3FilesSyncRole() string  { return n.IAMPrefix + "-s3files-sync" }
func (n Names) EFSNodeRole() string      { return n.IAMPrefix + "-" + efsNodeSA }

type (
	s3filesAPI interface {
		ListFileSystems(context.Context, *s3files.ListFileSystemsInput, ...func(*s3files.Options)) (*s3files.ListFileSystemsOutput, error)
		GetFileSystem(context.Context, *s3files.GetFileSystemInput, ...func(*s3files.Options)) (*s3files.GetFileSystemOutput, error)
		CreateFileSystem(context.Context, *s3files.CreateFileSystemInput, ...func(*s3files.Options)) (*s3files.CreateFileSystemOutput, error)
		DeleteFileSystem(context.Context, *s3files.DeleteFileSystemInput, ...func(*s3files.Options)) (*s3files.DeleteFileSystemOutput, error)
		ListMountTargets(context.Context, *s3files.ListMountTargetsInput, ...func(*s3files.Options)) (*s3files.ListMountTargetsOutput, error)
		CreateMountTarget(context.Context, *s3files.CreateMountTargetInput, ...func(*s3files.Options)) (*s3files.CreateMountTargetOutput, error)
		DeleteMountTarget(context.Context, *s3files.DeleteMountTargetInput, ...func(*s3files.Options)) (*s3files.DeleteMountTargetOutput, error)
		ListAccessPoints(context.Context, *s3files.ListAccessPointsInput, ...func(*s3files.Options)) (*s3files.ListAccessPointsOutput, error)
		DeleteAccessPoint(context.Context, *s3files.DeleteAccessPointInput, ...func(*s3files.Options)) (*s3files.DeleteAccessPointOutput, error)
	}
	versioningAPI interface {
		PutBucketVersioning(context.Context, *s3.PutBucketVersioningInput, ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error)
	}
)

type s3filesClients struct {
	S3Files s3filesAPI
	S3      versioningAPI
	EC2     EC2API
	EKS     clusterAPI
}

// s3filesClientsFor is replaced in tests.
var s3filesClientsFor = func(ctx context.Context) (*s3filesClients, error) {
	pc, err := newPluginClients(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &s3filesClients{S3Files: pc.S3Files, S3: pc.S3, EC2: pc.EC2, EKS: pc.EKS}, nil
}

// Waits on a role S3 Files cannot assume yet, replaced in tests.
var (
	s3filesRoleRetries = 12
	s3filesRoleDelay   = 10 * time.Second
)

func init() {
	registerPlugin(pluginDef{
		ID:           "s3files",
		Short:        "Create the S3 Files bucket, file system, mount targets and security group, and the IAM roles, for per-actor S3 Files volumes (needs the efs plugin)",
		Create:       createS3Files,
		Teardown:     s3filesTeardown,
		Inventory:    s3filesInventory,
		TaggingTypes: []string{"s3files:file-system"},
	})
}

func createS3Files(ctx context.Context, e *env) error {
	if e.cfg.Plan {
		slog.Info("plan mode: reading only; nothing will be created or changed")
	}
	// The controllers' role is the efs plugin's, and S3 Files volumes use
	// its add-on and Substrate's CSI controller.
	ctrl := e.names.EFSControllerRole()
	if ok, err := e.ownedRole(ctx, ctrl); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("IAM role %s does not exist: run `setup-aws plugin efs` first", ctrl)
	}
	c, err := s3filesClientsFor(ctx)
	if err != nil {
		return err
	}
	net, err := clusterNetwork(ctx, c.EKS, c.EC2, e.names.Cluster)
	if err != nil {
		return err
	}
	b := e.names.S3FilesBucket()
	if err := e.ensureBucket(ctx, b); err != nil {
		return err
	}
	if err := e.act("turn on versioning of "+b, func() error {
		_, err := c.S3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(b), ExpectedBucketOwner: aws.String(e.names.Account),
			VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled}})
		return err
	}); err != nil {
		return fmt.Errorf("versioning of %s: %w", b, err)
	}
	if err := e.ensureSyncRole(ctx); err != nil {
		return err
	}
	fs, err := e.ensureS3FileSystem(ctx, c.S3Files)
	if err != nil {
		return err
	}
	sg, err := e.ensureNFSGroup(ctx, c.EC2, net, e.names.S3FilesGroup(), "S3 Files")
	if err != nil {
		return err
	}
	fsID, fsARN := "", ""
	if fs != nil {
		fsID, fsARN = aws.ToString(fs.FileSystemId), aws.ToString(fs.FileSystemArn)
	}
	if err := e.ensureS3FilesMountTargets(ctx, c.S3Files, fsID, sg, net); err != nil {
		return err
	}
	if fsARN == "" {
		fsARN = "arn:" + e.names.Partition + ":s3files:" + e.names.Region + ":" + e.names.Account + ":file-system/fs-tbd" // --plan only
	}
	if err := e.act("put inline policy s3files on "+ctrl, func() error {
		_, err := e.c.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(ctrl), PolicyName: aws.String("s3files"),
			PolicyDocument: aws.String(e.s3filesControllerPolicy(fsARN))})
		return err
	}); err != nil {
		return fmt.Errorf("policy s3files of %s: %w", ctrl, err)
	}
	if err := e.roleForSA(ctx, PodIdentity{csiNamespace, efsNodeSA, e.s3filesNodePolicy(fsARN)}); err != nil {
		return err
	}
	if !e.cfg.Plan {
		slog.Info("S3 Files ready", "fileSystem", fsID, "securityGroup", sg)
	}
	return nil
}

// syncTrust lets S3 Files assume the role, as the elasticfilesystem service
// principal, for file systems of this account and Region only.
func (e *env) syncTrust() string {
	n := e.names
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"elasticfilesystem.amazonaws.com"},`+
		`"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"%[2]s"},`+
		`"ArnLike":{"aws:SourceArn":"arn:%[1]s:s3files:%[3]s:%[2]s:file-system/*"}}}]}`, n.Partition, n.Account, n.Region)
}

// syncPolicy lets S3 Files read and write the bucket and manage the
// EventBridge rules it keeps the sync with.
func (e *env) syncPolicy() string {
	n := e.names
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["s3:ListBucket","s3:ListBucketVersions"],"Resource":"%[1]s","Condition":{"StringEquals":{"aws:ResourceAccount":"%[2]s"}}},`+
		`{"Effect":"Allow","Action":["s3:AbortMultipartUpload","s3:DeleteObject*","s3:GetObject*","s3:List*","s3:PutObject*"],"Resource":"%[1]s/*",`+
		`"Condition":{"StringEquals":{"aws:ResourceAccount":"%[2]s"}}},`+
		`{"Effect":"Allow","Action":["events:DeleteRule","events:DisableRule","events:EnableRule","events:PutRule","events:PutTargets","events:RemoveTargets"],`+
		`"Condition":{"StringEquals":{"events:ManagedBy":"elasticfilesystem.amazonaws.com"}},"Resource":"arn:%[3]s:events:*:*:rule/DO-NOT-DELETE-S3-Files*"},`+
		`{"Effect":"Allow","Action":["events:DescribeRule","events:ListRuleNamesByTarget","events:ListRules","events:ListTargetsByRule"],"Resource":"arn:%[3]s:events:*:*:rule/*"}]}`,
		n.S3FilesBucketARN(), n.Account, n.Partition)
}

func (e *env) ensureSyncRole(ctx context.Context) error {
	role := e.names.S3FilesSyncRole()
	exists, err := e.ownedRole(ctx, role)
	if err != nil {
		return err
	}
	if exists {
		err = e.act("update trust policy of "+role, func() error {
			_, err := e.c.IAM.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(role), PolicyDocument: aws.String(e.syncTrust())})
			return err
		})
	} else {
		err = e.act("create role "+role, func() error {
			tags := make([]iamtypes.Tag, 0, len(e.tags))
			for _, k := range sortedKeys(e.tags) {
				tags = append(tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
			}
			_, err := e.c.IAM.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(role), AssumeRolePolicyDocument: aws.String(e.syncTrust()), Tags: tags})
			return err
		})
	}
	if err != nil {
		return fmt.Errorf("role %s: %w", role, err)
	}
	return e.act("put inline policy sync on "+role, func() error {
		_, err := e.c.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String("sync"), PolicyDocument: aws.String(e.syncPolicy())})
		return err
	})
}

// s3FileSystems are the file systems on the bucket that are not deleted.
func s3FileSystems(ctx context.Context, c s3filesAPI, bucketARN string) ([]s3ftypes.ListFileSystemsDescription, error) {
	var out []s3ftypes.ListFileSystemsDescription
	p := s3files.NewListFileSystemsPaginator(c, &s3files.ListFileSystemsInput{Bucket: aws.String(bucketARN)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list S3 file systems: %w", err)
		}
		for _, f := range page.FileSystems {
			if aws.ToString(f.Bucket) == bucketARN && f.Status != s3ftypes.LifeCycleStateDeleted {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

func s3fTags(m map[string]string) []s3ftypes.Tag {
	out := make([]s3ftypes.Tag, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, s3ftypes.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

func s3fTagMap(ts []s3ftypes.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// ensureS3FileSystem returns the bucket's file system, made with the sync
// role if there is none, once it is available; nil under --plan when it
// would be made.
func (e *env) ensureS3FileSystem(ctx context.Context, c s3filesAPI) (*s3files.GetFileSystemOutput, error) {
	bucketARN := e.names.S3FilesBucketARN()
	fss, err := s3FileSystems(ctx, c, bucketARN)
	if err != nil {
		return nil, err
	}
	id := ""
	switch len(fss) {
	case 0:
		roleARN := fmt.Sprintf("arn:%s:iam::%s:role/%s", e.names.Partition, e.names.Account, e.names.S3FilesSyncRole())
		if err := e.act("create S3 file system on "+e.names.S3FilesBucket(), func() error {
			// A new role can take a few seconds before S3 Files may assume it.
			var err error
			for i := 0; i < s3filesRoleRetries; i++ {
				var out *s3files.CreateFileSystemOutput
				out, err = c.CreateFileSystem(ctx, &s3files.CreateFileSystemInput{Bucket: aws.String(bucketARN), RoleArn: aws.String(roleARN),
					ClientToken: aws.String(e.names.IAMPrefix), Tags: s3fTags(e.tags)})
				if err == nil {
					id = aws.ToString(out.FileSystemId)
					return nil
				}
				slog.Info("S3 Files cannot create the file system yet; retrying", "error", err.Error())
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(s3filesRoleDelay):
				}
			}
			return err
		}); err != nil {
			return nil, fmt.Errorf("create S3 file system: %w", err)
		}
		if id == "" {
			return nil, nil // --plan
		}
	case 1:
		id = aws.ToString(fss[0].FileSystemId)
	default:
		return nil, fmt.Errorf("bucket %s has %d S3 file systems; expected one", e.names.S3FilesBucket(), len(fss))
	}
	var fs *s3files.GetFileSystemOutput
	err = waitFor(ctx, "S3 file system "+id+" to be available", 10*time.Minute, func() (bool, error) {
		out, err := c.GetFileSystem(ctx, &s3files.GetFileSystemInput{FileSystemId: aws.String(id)})
		if err != nil {
			return false, fmt.Errorf("get S3 file system %s: %w", id, err)
		}
		if err := e.owned("S3 file system "+id, s3fTagMap(out.Tags)); err != nil {
			return false, err
		}
		fs = out
		switch out.Status {
		case s3ftypes.LifeCycleStateAvailable:
			return true, nil
		case s3ftypes.LifeCycleStateError, s3ftypes.LifeCycleStateDeleting, s3ftypes.LifeCycleStateDeleted, s3ftypes.LifeCycleStateMisconfigured:
			return false, fmt.Errorf("S3 file system %s is %s (%s)", id, out.Status, aws.ToString(out.StatusMessage))
		}
		return false, nil
	})
	return fs, err
}

func s3MountTargets(ctx context.Context, c s3filesAPI, fsID string) ([]s3ftypes.ListMountTargetsDescription, error) {
	var out []s3ftypes.ListMountTargetsDescription
	p := s3files.NewListMountTargetsPaginator(c, &s3files.ListMountTargetsInput{FileSystemId: aws.String(fsID)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if tdGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list mount targets of %s: %w", fsID, err)
		}
		out = append(out, page.MountTargets...)
	}
	return out, nil
}

// ensureS3FilesMountTargets makes a mount target in each private subnet's
// Availability Zone that has none, then waits until all are available.
func (e *env) ensureS3FilesMountTargets(ctx context.Context, c s3filesAPI, fsID, sg string, net nfsNet) error {
	if fsID == "" {
		slog.Info("plan: would create a mount target in each private subnet's Availability Zone")
		return nil
	}
	mts, err := s3MountTargets(ctx, c, fsID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, m := range mts {
		have[aws.ToString(m.AvailabilityZoneId)] = true
	}
	for _, s := range net.subnets {
		az, id := aws.ToString(s.AvailabilityZoneId), aws.ToString(s.SubnetId)
		if have[az] {
			continue
		}
		have[az] = true
		if err := e.act("create S3 Files mount target in "+aws.ToString(s.AvailabilityZone)+" ("+id+")", func() error {
			_, err := c.CreateMountTarget(ctx, &s3files.CreateMountTargetInput{FileSystemId: aws.String(fsID), SubnetId: aws.String(id), SecurityGroups: []string{sg}})
			return err
		}); err != nil {
			return fmt.Errorf("create S3 Files mount target in %s: %w", id, err)
		}
	}
	if e.cfg.Plan {
		return nil
	}
	return waitFor(ctx, "the S3 Files mount targets to be available", 10*time.Minute, func() (bool, error) {
		mts, err := s3MountTargets(ctx, c, fsID)
		if err != nil {
			return false, err
		}
		for _, m := range mts {
			switch m.Status {
			case s3ftypes.LifeCycleStateAvailable:
			case s3ftypes.LifeCycleStateError, s3ftypes.LifeCycleStateDeleting, s3ftypes.LifeCycleStateDeleted, s3ftypes.LifeCycleStateMisconfigured:
				return false, fmt.Errorf("S3 Files mount target %s is %s (%s)", aws.ToString(m.MountTargetId), m.Status, aws.ToString(m.StatusMessage))
			default:
				return false, nil
			}
		}
		return len(mts) > 0, nil
	})
}

// s3filesControllerPolicy lets the CSI controllers create S3 Files access
// points on this file system only, with the efs.csi.aws.com/cluster tag and
// otherwise only this deployment's tags (accessPointTagKeys), and delete
// only those that carry the driver's tag.
func (e *env) s3filesControllerPolicy(fsARN string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["s3files:ListAccessPoints","s3files:ListFileSystems"],"Resource":"*"},`+
		`{"Effect":"Allow","Action":"s3files:CreateAccessPoint","Resource":"%[1]s",`+
		`"Condition":{"Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},"ForAllValues:StringEquals":{"aws:TagKeys":%[2]s}}},`+
		`{"Effect":"Allow","Action":"s3files:TagResource","Resource":"*",`+
		`"Condition":{"StringEquals":{"s3files:CreateAction":"CreateAccessPoint"},"Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},`+
		`"ForAllValues:StringEquals":{"aws:TagKeys":%[2]s}}},`+
		`{"Effect":"Allow","Action":"s3files:DeleteAccessPoint","Resource":"%[1]s/access-point/*",`+
		`"Condition":{"StringEquals":{"aws:ResourceTag/efs.csi.aws.com/cluster":"true"}}}]}`, fsARN, e.accessPointTagKeys())
}

// s3filesNodePolicy lets the node plugin mount and write this file system
// and read the bucket behind it. Root access because the access points are
// owned by uid 0: the counter runs as root.
//
// shortcut: the installer's StorageClass is fixed at uid 0 (config.env's
// EFS_UID default); drop ClientRootAccess if it ever takes a non-root uid.
func (e *env) s3filesNodePolicy(fsARN string) string {
	b := e.names.S3FilesBucketARN()
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["s3files:ClientMount","s3files:ClientWrite","s3files:ClientRootAccess"],"Resource":"%[1]s"},`+
		`{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":"%[2]s/*"},`+
		`{"Effect":"Allow","Action":"s3:ListBucket","Resource":"%[2]s"}]}`, fsARN, b)
}

// ---- teardown and inventory --------------------------------------------------

func s3filesTeardown(e *tdEnv) []pluginStep {
	return []pluginStep{
		{Step: e.stepCSIAssociation(efsNodeSA)},
		{Step: e.stepS3FilesAccessPoints},
		{Step: e.stepS3FilesMountTargets},
		{Step: e.stepS3FileSystems},
		{Step: e.stepS3FilesBucket},
		{Step: func(ctx context.Context) ([]tdTarget, []string, error) {
			return e.roleTargets(ctx, []string{e.n.EFSNodeRole(), e.n.S3FilesSyncRole()}, true)
		}},
		{Step: e.stepS3FilesControllerPolicy},
		{Step: e.stepNamedSG(e.n.S3FilesGroup())},
	}
}

// ownedS3FileSystems are this deployment's file systems on the bucket, each
// with its tags; one of another owner is left to stepS3FileSystems, which
// refuses it.
func (e *tdEnv) s3FileSystems(ctx context.Context) (*s3filesClients, []s3ftypes.ListFileSystemsDescription, []map[string]string, error) {
	c, err := s3filesClientsFor(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	fss, err := s3FileSystems(ctx, c.S3Files, e.n.S3FilesBucketARN())
	if err != nil {
		return nil, nil, nil, err
	}
	var live []s3ftypes.ListFileSystemsDescription
	var tags []map[string]string
	for _, f := range fss {
		out, err := c.S3Files.GetFileSystem(ctx, &s3files.GetFileSystemInput{FileSystemId: f.FileSystemId})
		if tdGone(err) {
			// Listed but gone: a delete that just finished. Untagged, it
			// would read as another owner's and be refused.
			continue
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("get S3 file system %s: %w", aws.ToString(f.FileSystemId), err)
		}
		live, tags = append(live, f), append(tags, s3fTagMap(out.Tags))
	}
	return c, live, tags, nil
}

func s3AccessPoints(ctx context.Context, c s3filesAPI, fsID string) ([]s3ftypes.ListAccessPointsDescription, error) {
	var out []s3ftypes.ListAccessPointsDescription
	p := s3files.NewListAccessPointsPaginator(c, &s3files.ListAccessPointsInput{FileSystemId: aws.String(fsID)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if tdGone(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list access points of %s: %w", fsID, err)
		}
		out = append(out, page.AccessPoints...)
	}
	return out, nil
}

func (e *tdEnv) stepS3FilesAccessPoints(ctx context.Context) ([]tdTarget, []string, error) {
	c, fss, tags, err := e.s3FileSystems(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	for i, f := range fss {
		if !e.owned(tags[i]) {
			continue
		}
		aps, err := s3AccessPoints(ctx, c.S3Files, aws.ToString(f.FileSystemId))
		if err != nil {
			return nil, nil, err
		}
		for _, ap := range aps {
			if ap.Status == s3ftypes.LifeCycleStateDeleted || ap.Status == s3ftypes.LifeCycleStateDeleting {
				continue
			}
			id := aws.ToString(ap.AccessPointId)
			ts = append(ts, tdTarget{kind: "S3 Files access point", id: id, del: func(ctx context.Context) error {
				_, err := c.S3Files.DeleteAccessPoint(ctx, &s3files.DeleteAccessPointInput{AccessPointId: aws.String(id)})
				return err
			}})
		}
	}
	return ts, nil, nil
}

func (e *tdEnv) stepS3FilesMountTargets(ctx context.Context) ([]tdTarget, []string, error) {
	c, fss, tags, err := e.s3FileSystems(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	for i, f := range fss {
		if !e.owned(tags[i]) {
			continue
		}
		fsID := aws.ToString(f.FileSystemId)
		mts, err := s3MountTargets(ctx, c.S3Files, fsID)
		if err != nil {
			return nil, nil, err
		}
		for _, m := range mts {
			if m.Status == s3ftypes.LifeCycleStateDeleted {
				continue
			}
			id := aws.ToString(m.MountTargetId)
			t := tdTarget{kind: "S3 Files mount target", id: id, wait: gone(func(ctx context.Context) (bool, error) {
				left, err := s3MountTargets(ctx, c.S3Files, fsID)
				for _, x := range left {
					if aws.ToString(x.MountTargetId) == id && x.Status != s3ftypes.LifeCycleStateDeleted {
						return true, err
					}
				}
				return false, err
			})}
			if m.Status != s3ftypes.LifeCycleStateDeleting {
				t.del = func(ctx context.Context) error {
					_, err := c.S3Files.DeleteMountTarget(ctx, &s3files.DeleteMountTargetInput{MountTargetId: aws.String(id)})
					return err
				}
			}
			ts = append(ts, t)
		}
	}
	return ts, nil, nil
}

// stepS3FileSystems deletes the bucket's file systems, refusing one of
// another owner, and waits until each is deleted.
func (e *tdEnv) stepS3FileSystems(ctx context.Context) ([]tdTarget, []string, error) {
	c, fss, tags, err := e.s3FileSystems(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for i, f := range fss {
		id := aws.ToString(f.FileSystemId)
		if r := e.refuse("S3 file system", id, tags[i]); r != "" {
			rs = append(rs, r)
			continue
		}
		t := tdTarget{kind: "S3 file system", id: id, wait: gone(func(ctx context.Context) (bool, error) {
			out, err := c.S3Files.GetFileSystem(ctx, &s3files.GetFileSystemInput{FileSystemId: aws.String(id)})
			if tdGone(err) {
				return false, nil
			}
			return err == nil && out.Status != s3ftypes.LifeCycleStateDeleted, err
		})}
		if f.Status != s3ftypes.LifeCycleStateDeleting {
			t.del = func(ctx context.Context) error {
				// S3 Files refuses while a deleted mount target is still going away.
				return retryOn(ctx, "S3 file system "+id+" to be free", func() error {
					_, err := c.S3Files.DeleteFileSystem(ctx, &s3files.DeleteFileSystemInput{FileSystemId: aws.String(id)})
					return err
				}, "ConflictException")
			}
		}
		ts = append(ts, t)
	}
	return ts, rs, nil
}

// stepS3FilesBucket deletes every object version and delete marker of the
// bucket, then the bucket, as stepBuckets does Substrate's.
func (e *tdEnv) stepS3FilesBucket(ctx context.Context) ([]tdTarget, []string, error) {
	b := e.n.S3FilesBucket()
	ok, tags, err := e.bucketTags(ctx, b)
	if err != nil || !ok {
		return nil, nil, err
	}
	if r := e.refuse("S3 bucket", b, tags); r != "" {
		return nil, []string{r}, nil
	}
	return []tdTarget{{kind: "S3 bucket", id: b + " (every object version)", del: func(ctx context.Context) error { return e.deleteBucket(ctx, b) }}}, nil, nil
}

// stepS3FilesControllerPolicy removes the s3files inline policy from the efs
// plugin's controller role, which stays while efs does. A role of another
// owner is left alone: the efs plugin's teardown refuses it.
func (e *tdEnv) stepS3FilesControllerPolicy(ctx context.Context) ([]tdTarget, []string, error) {
	role := e.n.EFSControllerRole()
	g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	if tdGone(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get role %s: %w", role, err)
	}
	if !e.owned(kvMap(g.Role.Tags, iamKV)) {
		return nil, nil, nil
	}
	has, err := e.hasInlinePolicy(ctx, role, "s3files")
	if err != nil || !has {
		return nil, nil, err
	}
	return []tdTarget{{kind: "IAM role policy", id: role + "/s3files", del: func(ctx context.Context) error {
		_, err := e.c.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String("s3files")})
		return err
	}}}, nil, nil
}

func (e *tdEnv) hasInlinePolicy(ctx context.Context, role, name string) (bool, error) {
	p := iam.NewListRolePoliciesPaginator(e.c.IAM, &iam.ListRolePoliciesInput{RoleName: aws.String(role)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if tdGone(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("list the policies of role %s: %w", role, err)
		}
		for _, n := range page.PolicyNames {
			if n == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// s3filesInventory lists what is left of the s3files plugin: the node
// association when it carries the owner tag (stepAddon), and whatever its
// tags the bucket, the file systems with their access points and mount
// targets, the roles, the controller role's s3files policy and the security
// group.
func s3filesInventory(ctx context.Context, e *tdEnv) ([]tdRow, error) {
	rows, err := e.addonRows(ctx, "", efsNodeSA)
	if err != nil {
		return nil, err
	}
	b := e.n.S3FilesBucket()
	if ok, tags, err := e.bucketTags(ctx, b); err != nil {
		return nil, err
	} else if ok {
		rows = append(rows, tdRow{Kind: "S3 bucket", ID: b, Tags: tags})
	}
	c, fss, tags, err := e.s3FileSystems(ctx)
	if err != nil {
		return nil, err
	}
	for i, f := range fss {
		id := aws.ToString(f.FileSystemId)
		rows = append(rows, tdRow{Kind: "S3 file system", ID: id, Tags: tags[i]})
		aps, err := s3AccessPoints(ctx, c.S3Files, id)
		if err != nil {
			return nil, err
		}
		for _, ap := range aps {
			if ap.Status != s3ftypes.LifeCycleStateDeleted {
				rows = append(rows, tdRow{Kind: "S3 Files access point", ID: aws.ToString(ap.AccessPointId), Tags: tags[i]})
			}
		}
		mts, err := s3MountTargets(ctx, c.S3Files, id)
		if err != nil {
			return nil, err
		}
		for _, m := range mts {
			if m.Status != s3ftypes.LifeCycleStateDeleted {
				rows = append(rows, tdRow{Kind: "S3 Files mount target", ID: aws.ToString(m.MountTargetId), Tags: tags[i]})
			}
		}
	}
	for _, role := range []string{e.n.EFSNodeRole(), e.n.S3FilesSyncRole()} {
		r, err := e.roleRow(ctx, role)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	if pol, _, err := e.stepS3FilesControllerPolicy(ctx); err != nil {
		return nil, err
	} else if len(pol) > 0 {
		rows = append(rows, tdRow{Kind: "IAM role policy", ID: pol[0].id, Tags: map[string]string{OwnerTagKey: e.n.OwnerValue}})
	}
	sg, err := e.sgRows(ctx, e.n.S3FilesGroup())
	if err != nil {
		return nil, err
	}
	return append(rows, sg...), nil
}
