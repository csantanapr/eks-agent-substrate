package cmd

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3files"
	s3ftypes "github.com/aws/aws-sdk-go-v2/service/s3files/types"
)

// fakeNFS is the in-memory EFS, S3 Files, add-on and bucket-versioning side
// of an account, for the efs and s3files plugins. Deletes finish a read
// later, as AWS's do; writes records every mutating call in order.
type fakeNFS struct {
	fss       map[string]*nfsFS // EFS file systems by id
	mts       map[string]*nfsMT // EFS mount targets by id
	aps       map[string]string // EFS access point -> file system
	s3fss     map[string]*nfsFS // S3 file systems by id
	s3mts     map[string]*nfsMT
	s3aps     map[string]string
	addons    map[string]*ekstypes.Addon
	versioned map[string]bool
	created   *efs.CreateFileSystemInput
	writes    []string
	seq       int
	// s3CreateFails makes the first N S3 Files CreateFileSystem calls fail,
	// as they do while a new role cannot be assumed yet.
	s3CreateFails int
}

type nfsFS struct {
	id, token, bucket string
	state             string
	tags              map[string]string
}

type nfsMT struct {
	id, fs, subnet, az string
	state              string
}

func newFakeNFS() *fakeNFS {
	return &fakeNFS{fss: map[string]*nfsFS{}, mts: map[string]*nfsMT{}, aps: map[string]string{},
		s3fss: map[string]*nfsFS{}, s3mts: map[string]*nfsMT{}, s3aps: map[string]string{},
		addons: map[string]*ekstypes.Addon{}, versioned: map[string]bool{}}
}

func (f *fakeNFS) next(p string) string { f.seq++; return fmt.Sprintf("%s-%d", p, f.seq) }
func (f *fakeNFS) write(s string)       { f.writes = append(f.writes, s) }

// settle moves a creating resource to available and a deleting one away.
func settleFS(m map[string]*nfsFS, id string) *nfsFS {
	x := m[id]
	if x == nil {
		return nil
	}
	switch x.state {
	case "creating":
		x.state = "available"
	case "deleting":
		delete(m, id)
		return nil
	}
	return x
}

func settleMT(m map[string]*nfsMT, id string) *nfsMT {
	x := m[id]
	if x == nil {
		return nil
	}
	switch x.state {
	case "creating":
		x.state = "available"
	case "deleting":
		delete(m, id)
		return nil
	}
	return x
}

func mapTags(m map[string]string) []efstypes.Tag { return efsTags(m) }

// installNFS points the efs and s3files plugins' clients at f, with the
// RDS test's EC2 fake (and three private subnets in two zones).
func installNFS(t *testing.T, f *fakeNFS, db *fakeDB) {
	t.Helper()
	prevE, prevS, prevPoll, prevDelay := efsClientsFor, s3filesClientsFor, pollEvery, s3filesRoleDelay
	ec := nfsEC2{(*fec2)(db)}
	efsClientsFor = func(context.Context) (*efsClients, error) {
		return &efsClients{EFS: (*fEFS)(f), EC2: ec, EKS: (*fAddons)(f)}, nil
	}
	s3filesClientsFor = func(context.Context) (*s3filesClients, error) {
		return &s3filesClients{S3Files: (*fS3F)(f), S3: (*fVersioning)(f), EC2: ec, EKS: (*fAddons)(f)}, nil
	}
	pollEvery, s3filesRoleDelay = 0, 0
	t.Cleanup(func() {
		efsClientsFor, s3filesClientsFor, pollEvery, s3filesRoleDelay = prevE, prevS, prevPoll, prevDelay
	})
}

type nfsEC2 struct{ *fec2 }

func (n nfsEC2) DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, o ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	out, err := n.fec2.DescribeSubnets(ctx, in, o...)
	if err != nil || len(out.Subnets) == 0 {
		return out, err
	}
	return &ec2.DescribeSubnetsOutput{Subnets: []ec2types.Subnet{
		{SubnetId: aws.String("subnet-b"), AvailabilityZone: aws.String("us-west-2b"), AvailabilityZoneId: aws.String("usw2-az2")},
		{SubnetId: aws.String("subnet-a"), AvailabilityZone: aws.String("us-west-2a"), AvailabilityZoneId: aws.String("usw2-az1")},
		// A second private subnet in the first zone gets no mount target.
		{SubnetId: aws.String("subnet-a2"), AvailabilityZone: aws.String("us-west-2a"), AvailabilityZoneId: aws.String("usw2-az1")},
	}}, nil
}

type fAddons fakeNFS

func (f *fAddons) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{Name: in.Name, Status: ekstypes.ClusterStatusActive,
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{VpcId: aws.String(fakeVPC), ClusterSecurityGroupId: aws.String(fakeClusterSG)}}}, nil
}
func (f *fAddons) DescribeAddon(_ context.Context, in *eks.DescribeAddonInput, _ ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	a := f.addons[aws.ToString(in.AddonName)]
	if a == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	if a.Status == ekstypes.AddonStatusDeleting {
		delete(f.addons, aws.ToString(in.AddonName))
	} else {
		a.Status = ekstypes.AddonStatusActive
	}
	c := *a
	return &eks.DescribeAddonOutput{Addon: &c}, nil
}
func (f *fAddons) CreateAddon(_ context.Context, in *eks.CreateAddonInput, _ ...func(*eks.Options)) (*eks.CreateAddonOutput, error) {
	f.addons[aws.ToString(in.AddonName)] = &ekstypes.Addon{AddonName: in.AddonName, AddonVersion: in.AddonVersion,
		ConfigurationValues: in.ConfigurationValues, Status: ekstypes.AddonStatusCreating, Tags: in.Tags}
	(*fakeNFS)(f).write("CreateAddon " + aws.ToString(in.AddonName) + " " + aws.ToString(in.AddonVersion))
	return &eks.CreateAddonOutput{}, nil
}
func (f *fAddons) UpdateAddon(_ context.Context, in *eks.UpdateAddonInput, _ ...func(*eks.Options)) (*eks.UpdateAddonOutput, error) {
	a := f.addons[aws.ToString(in.AddonName)]
	a.AddonVersion, a.ConfigurationValues, a.Status = in.AddonVersion, in.ConfigurationValues, ekstypes.AddonStatusUpdating
	(*fakeNFS)(f).write("UpdateAddon " + aws.ToString(in.AddonName) + " " + aws.ToString(in.AddonVersion))
	return &eks.UpdateAddonOutput{}, nil
}
func (f *fAddons) DeleteAddon(_ context.Context, in *eks.DeleteAddonInput, _ ...func(*eks.Options)) (*eks.DeleteAddonOutput, error) {
	a := f.addons[aws.ToString(in.AddonName)]
	if a == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	a.Status = ekstypes.AddonStatusDeleting
	(*fakeNFS)(f).write("DeleteAddon " + aws.ToString(in.AddonName))
	return &eks.DeleteAddonOutput{}, nil
}

type fEFS fakeNFS

func (f *fEFS) fsDesc(x *nfsFS) efstypes.FileSystemDescription {
	return efstypes.FileSystemDescription{FileSystemId: aws.String(x.id), CreationToken: aws.String(x.token),
		LifeCycleState: efstypes.LifeCycleState(x.state), Tags: mapTags(x.tags)}
}
func (f *fEFS) DescribeFileSystems(_ context.Context, in *efs.DescribeFileSystemsInput, _ ...func(*efs.Options)) (*efs.DescribeFileSystemsOutput, error) {
	out := &efs.DescribeFileSystemsOutput{}
	if id := aws.ToString(in.FileSystemId); id != "" {
		x := settleFS(f.fss, id)
		if x == nil {
			return nil, apiErr("FileSystemNotFound")
		}
		out.FileSystems = append(out.FileSystems, f.fsDesc(x))
		return out, nil
	}
	for _, x := range f.fss {
		if x.token == aws.ToString(in.CreationToken) {
			out.FileSystems = append(out.FileSystems, f.fsDesc(x))
		}
	}
	return out, nil
}
func (f *fEFS) CreateFileSystem(_ context.Context, in *efs.CreateFileSystemInput, _ ...func(*efs.Options)) (*efs.CreateFileSystemOutput, error) {
	id := (*fakeNFS)(f).next("fs")
	f.fss[id] = &nfsFS{id: id, token: aws.ToString(in.CreationToken), state: "creating", tags: efsTagMap(in.Tags)}
	f.created = in
	(*fakeNFS)(f).write("CreateFileSystem " + aws.ToString(in.CreationToken))
	return &efs.CreateFileSystemOutput{FileSystemId: aws.String(id), LifeCycleState: efstypes.LifeCycleStateCreating}, nil
}
func (f *fEFS) DeleteFileSystem(_ context.Context, in *efs.DeleteFileSystemInput, _ ...func(*efs.Options)) (*efs.DeleteFileSystemOutput, error) {
	id := aws.ToString(in.FileSystemId)
	x := f.fss[id]
	if x == nil {
		return nil, apiErr("FileSystemNotFound")
	}
	for _, m := range f.mts {
		if m.fs == id {
			return nil, apiErr("FileSystemInUse")
		}
	}
	x.state = "deleting"
	(*fakeNFS)(f).write("DeleteFileSystem " + id)
	return &efs.DeleteFileSystemOutput{}, nil
}
func (f *fEFS) DescribeMountTargets(_ context.Context, in *efs.DescribeMountTargetsInput, _ ...func(*efs.Options)) (*efs.DescribeMountTargetsOutput, error) {
	out := &efs.DescribeMountTargetsOutput{}
	ids := make([]string, 0, len(f.mts))
	for id := range f.mts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := f.mts[id]
		if (in.MountTargetId != nil && id != aws.ToString(in.MountTargetId)) || (in.FileSystemId != nil && m.fs != aws.ToString(in.FileSystemId)) {
			continue
		}
		if m = settleMT(f.mts, id); m != nil {
			out.MountTargets = append(out.MountTargets, efstypes.MountTargetDescription{MountTargetId: aws.String(m.id), FileSystemId: aws.String(m.fs),
				SubnetId: aws.String(m.subnet), AvailabilityZoneId: aws.String(m.az), LifeCycleState: efstypes.LifeCycleState(m.state)})
		}
	}
	if in.MountTargetId != nil && len(out.MountTargets) == 0 {
		return nil, apiErr("MountTargetNotFound")
	}
	return out, nil
}
func (f *fEFS) CreateMountTarget(_ context.Context, in *efs.CreateMountTargetInput, _ ...func(*efs.Options)) (*efs.CreateMountTargetOutput, error) {
	if len(in.SecurityGroups) != 1 || in.SecurityGroups[0] == "" {
		return nil, fmt.Errorf("mount target without its security group")
	}
	az := map[string]string{"subnet-a": "usw2-az1", "subnet-a2": "usw2-az1", "subnet-b": "usw2-az2"}[aws.ToString(in.SubnetId)]
	for _, m := range f.mts {
		if m.fs == aws.ToString(in.FileSystemId) && m.az == az {
			return nil, apiErr("MountTargetConflict")
		}
	}
	id := (*fakeNFS)(f).next("fsmt")
	f.mts[id] = &nfsMT{id: id, fs: aws.ToString(in.FileSystemId), subnet: aws.ToString(in.SubnetId), az: az, state: "creating"}
	(*fakeNFS)(f).write("CreateMountTarget " + aws.ToString(in.SubnetId))
	return &efs.CreateMountTargetOutput{}, nil
}
func (f *fEFS) DeleteMountTarget(_ context.Context, in *efs.DeleteMountTargetInput, _ ...func(*efs.Options)) (*efs.DeleteMountTargetOutput, error) {
	m := f.mts[aws.ToString(in.MountTargetId)]
	if m == nil {
		return nil, apiErr("MountTargetNotFound")
	}
	m.state = "deleting"
	(*fakeNFS)(f).write("DeleteMountTarget " + m.id)
	return &efs.DeleteMountTargetOutput{}, nil
}
func (f *fEFS) DescribeAccessPoints(_ context.Context, in *efs.DescribeAccessPointsInput, _ ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error) {
	out := &efs.DescribeAccessPointsOutput{}
	for ap, fs := range f.aps {
		if fs == aws.ToString(in.FileSystemId) {
			out.AccessPoints = append(out.AccessPoints, efstypes.AccessPointDescription{AccessPointId: aws.String(ap), FileSystemId: aws.String(fs)})
		}
	}
	return out, nil
}
func (f *fEFS) DeleteAccessPoint(_ context.Context, in *efs.DeleteAccessPointInput, _ ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error) {
	id := aws.ToString(in.AccessPointId)
	if _, ok := f.aps[id]; !ok {
		return nil, apiErr("AccessPointNotFound")
	}
	delete(f.aps, id)
	(*fakeNFS)(f).write("DeleteAccessPoint " + id)
	return &efs.DeleteAccessPointOutput{}, nil
}

type fVersioning fakeNFS

func (f *fVersioning) PutBucketVersioning(_ context.Context, in *s3.PutBucketVersioningInput, _ ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error) {
	f.versioned[aws.ToString(in.Bucket)] = in.VersioningConfiguration.Status == "Enabled"
	(*fakeNFS)(f).write("PutBucketVersioning " + aws.ToString(in.Bucket))
	return &s3.PutBucketVersioningOutput{}, nil
}

type fS3F fakeNFS

func (f *fS3F) ListFileSystems(_ context.Context, in *s3files.ListFileSystemsInput, _ ...func(*s3files.Options)) (*s3files.ListFileSystemsOutput, error) {
	out := &s3files.ListFileSystemsOutput{}
	for id := range f.s3fss {
		if x := settleFS(f.s3fss, id); x != nil && (in.Bucket == nil || x.bucket == aws.ToString(in.Bucket)) {
			out.FileSystems = append(out.FileSystems, s3ftypes.ListFileSystemsDescription{FileSystemId: aws.String(x.id), Bucket: aws.String(x.bucket),
				Status: s3ftypes.LifeCycleState(x.state)})
		}
	}
	return out, nil
}
func (f *fS3F) GetFileSystem(_ context.Context, in *s3files.GetFileSystemInput, _ ...func(*s3files.Options)) (*s3files.GetFileSystemOutput, error) {
	x := settleFS(f.s3fss, aws.ToString(in.FileSystemId))
	if x == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	return &s3files.GetFileSystemOutput{FileSystemId: aws.String(x.id), FileSystemArn: aws.String("arn:aws:s3files:us-west-2:111122223333:file-system/" + x.id),
		Bucket: aws.String(x.bucket), Status: s3ftypes.LifeCycleState(x.state), Tags: s3fTags(x.tags)}, nil
}
func (f *fS3F) CreateFileSystem(_ context.Context, in *s3files.CreateFileSystemInput, _ ...func(*s3files.Options)) (*s3files.CreateFileSystemOutput, error) {
	if f.s3CreateFails > 0 {
		f.s3CreateFails--
		return nil, apiErr("ValidationException")
	}
	if !strings.HasSuffix(aws.ToString(in.RoleArn), ":role/demo-us-west-2-s3files-sync") {
		return nil, fmt.Errorf("file system with role %s", aws.ToString(in.RoleArn))
	}
	id := (*fakeNFS)(f).next("fs")
	f.s3fss[id] = &nfsFS{id: id, bucket: aws.ToString(in.Bucket), state: "creating", tags: s3fTagMap(in.Tags)}
	(*fakeNFS)(f).write("S3Files.CreateFileSystem " + aws.ToString(in.Bucket))
	return &s3files.CreateFileSystemOutput{FileSystemId: aws.String(id)}, nil
}
func (f *fS3F) DeleteFileSystem(_ context.Context, in *s3files.DeleteFileSystemInput, _ ...func(*s3files.Options)) (*s3files.DeleteFileSystemOutput, error) {
	id := aws.ToString(in.FileSystemId)
	for _, m := range f.s3mts {
		if m.fs == id {
			return nil, apiErr("ConflictException")
		}
	}
	x := f.s3fss[id]
	if x == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	x.state = "deleting"
	(*fakeNFS)(f).write("S3Files.DeleteFileSystem " + id)
	return &s3files.DeleteFileSystemOutput{}, nil
}
func (f *fS3F) ListMountTargets(_ context.Context, in *s3files.ListMountTargetsInput, _ ...func(*s3files.Options)) (*s3files.ListMountTargetsOutput, error) {
	out := &s3files.ListMountTargetsOutput{}
	for id, m := range f.s3mts {
		if m.fs != aws.ToString(in.FileSystemId) {
			continue
		}
		if m = settleMT(f.s3mts, id); m != nil {
			out.MountTargets = append(out.MountTargets, s3ftypes.ListMountTargetsDescription{MountTargetId: aws.String(m.id), FileSystemId: aws.String(m.fs),
				SubnetId: aws.String(m.subnet), AvailabilityZoneId: aws.String(m.az), Status: s3ftypes.LifeCycleState(m.state)})
		}
	}
	return out, nil
}
func (f *fS3F) CreateMountTarget(_ context.Context, in *s3files.CreateMountTargetInput, _ ...func(*s3files.Options)) (*s3files.CreateMountTargetOutput, error) {
	if len(in.SecurityGroups) != 1 || in.SecurityGroups[0] == "" {
		return nil, fmt.Errorf("mount target without its security group")
	}
	id := (*fakeNFS)(f).next("s3mt")
	az := map[string]string{"subnet-a": "usw2-az1", "subnet-a2": "usw2-az1", "subnet-b": "usw2-az2"}[aws.ToString(in.SubnetId)]
	f.s3mts[id] = &nfsMT{id: id, fs: aws.ToString(in.FileSystemId), subnet: aws.ToString(in.SubnetId), az: az, state: "creating"}
	(*fakeNFS)(f).write("S3Files.CreateMountTarget " + aws.ToString(in.SubnetId))
	return &s3files.CreateMountTargetOutput{}, nil
}
func (f *fS3F) DeleteMountTarget(_ context.Context, in *s3files.DeleteMountTargetInput, _ ...func(*s3files.Options)) (*s3files.DeleteMountTargetOutput, error) {
	m := f.s3mts[aws.ToString(in.MountTargetId)]
	if m == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	m.state = "deleting"
	(*fakeNFS)(f).write("S3Files.DeleteMountTarget " + m.id)
	return &s3files.DeleteMountTargetOutput{}, nil
}
func (f *fS3F) ListAccessPoints(_ context.Context, in *s3files.ListAccessPointsInput, _ ...func(*s3files.Options)) (*s3files.ListAccessPointsOutput, error) {
	out := &s3files.ListAccessPointsOutput{}
	for ap, fs := range f.s3aps {
		if fs == aws.ToString(in.FileSystemId) {
			out.AccessPoints = append(out.AccessPoints, s3ftypes.ListAccessPointsDescription{AccessPointId: aws.String(ap), FileSystemId: aws.String(fs),
				Status: s3ftypes.LifeCycleStateAvailable})
		}
	}
	return out, nil
}
func (f *fS3F) DeleteAccessPoint(_ context.Context, in *s3files.DeleteAccessPointInput, _ ...func(*s3files.Options)) (*s3files.DeleteAccessPointOutput, error) {
	id := aws.ToString(in.AccessPointId)
	if _, ok := f.s3aps[id]; !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	delete(f.s3aps, id)
	(*fakeNFS)(f).write("S3Files.DeleteAccessPoint " + id)
	return &s3files.DeleteAccessPointOutput{}, nil
}

// ---- create ------------------------------------------------------------------

func TestCreateEFSFreshThenRerunWritesNothing(t *testing.T) {
	f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
	installNFS(t, f, db)
	e := testEnv(t, aws_, false)
	if err := createEFS(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"CreateFileSystem demo-actors", "CreateMountTarget subnet-a", "CreateMountTarget subnet-b",
		"CreateAddon aws-efs-csi-driver v3.5.3-eksbuild.1"} {
		if !slices.Contains(f.writes, w) {
			t.Errorf("missing %q in %v", w, f.writes)
		}
	}
	if slices.Contains(f.writes, "CreateMountTarget subnet-a2") {
		t.Error("a second mount target in one Availability Zone")
	}
	in := f.created
	if !aws.ToBool(in.Encrypted) || in.ThroughputMode != efstypes.ThroughputModeElastic {
		t.Errorf("file system not encrypted with elastic throughput: %+v", in)
	}
	if tags := efsTagMap(in.Tags); tags[OwnerTagKey] != "demo.us-west-2" || tags["auto-delete"] != "no" || tags["Name"] != "demo" {
		t.Errorf("file system tags = %v", tags)
	}
	sg := db.sgs["demo-efs"]
	if sg == nil || sgTagMap(sg.Tags)[OwnerTagKey] != "demo.us-west-2" {
		t.Fatalf("security group demo-efs not made with the owner tag: %+v", sg)
	}
	if ok, err := nfsIngressOK(sg.IpPermissions, fakeClusterSG); !ok || err != nil {
		t.Errorf("security group admits %v (%v)", sg.IpPermissions, err)
	}
	a := f.addons[efsAddon]
	if a.Tags[OwnerTagKey] != "demo.us-west-2" || !sameJSON(aws.ToString(a.ConfigurationValues), efsAddonConfig) {
		t.Errorf("add-on tags %v config %s", a.Tags, aws.ToString(a.ConfigurationValues))
	}
	role := "demo-us-west-2-efs-csi-controller-sa"
	if aws_.roles[role][OwnerTagKey] != "demo.us-west-2" || !strings.Contains(aws_.policies[role], "file-system/fs-1") ||
		aws_.assocs["kube-system/efs-csi-controller-sa"] != "arn:aws:iam::111122223333:role/"+role {
		t.Errorf("controller role %v, policy %s, association %q", aws_.roles[role], aws_.policies[role], aws_.assocs["kube-system/efs-csi-controller-sa"])
	}
	// Access points may carry the driver's tag and this deployment's, which
	// Substrate's controller adds; the policy must be valid JSON.
	if p := aws_.policies[role]; !sameJSON(p, p) ||
		!strings.Contains(p, `"aws:TagKeys":["efs.csi.aws.com/cluster","agent-substrate-on-eks/deployment","auto-delete","project"]`) {
		t.Errorf("controller policy tag keys: %s", p)
	}

	// A rerun creates nothing.
	f.writes, aws_.writes, db.writes = nil, nil, nil
	if err := createEFS(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(f.writes)+len(db.writes)+count(aws_.writes, "Create") > 0 {
		t.Errorf("rerun wrote %v %v %v", f.writes, db.writes, aws_.writes)
	}
}

func TestCreateEFSPlanWritesNothing(t *testing.T) {
	f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
	installNFS(t, f, db)
	if err := createEFS(context.Background(), testEnv(t, aws_, true)); err != nil {
		t.Fatal(err)
	}
	if len(f.writes)+len(db.writes)+len(aws_.writes) > 0 {
		t.Errorf("--plan wrote %v %v %v", f.writes, db.writes, aws_.writes)
	}
}

// Both directions of reuse: an owned file system, group and add-on are kept
// (the rerun above); one with the same name and another owner stops the run
// before anything is made.
func TestCreateEFSRefusesForeign(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakeNFS, db *fakeDB)
		want  string
	}{
		{"file system", func(f *fakeNFS, _ *fakeDB) {
			f.fss["fs-x"] = &nfsFS{id: "fs-x", token: "demo-actors", state: "available", tags: map[string]string{OwnerTagKey: "other.us-west-2"}}
		}, "EFS file system fs-x (demo-actors) exists but is not owned"},
		{"security group", func(_ *fakeNFS, db *fakeDB) {
			db.sgs["demo-efs"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-x"), GroupName: aws.String("demo-efs")}
		}, "security group demo-efs (sg-x) exists but is not owned"},
		{"add-on", func(f *fakeNFS, _ *fakeDB) {
			f.addons[efsAddon] = &ekstypes.Addon{AddonName: aws.String(efsAddon), Status: ekstypes.AddonStatusActive}
		}, "EKS add-on aws-efs-csi-driver exists but is not owned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
			tc.setup(f, db)
			installNFS(t, f, db)
			err := createEFS(context.Background(), testEnv(t, aws_, false))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if count(f.writes, "CreateAddon")+count(f.writes, "UpdateAddon") > 0 {
				t.Errorf("changed the add-on after a refusal: %v", f.writes)
			}
		})
	}
}

func TestNFSIngressOK(t *testing.T) {
	nfs := ec2types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(2049), ToPort: aws.Int32(2049),
		UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(fakeClusterSG)}}}
	if ok, err := nfsIngressOK([]ec2types.IpPermission{nfs}, fakeClusterSG); !ok || err != nil {
		t.Errorf("the cluster rule: ok=%v err=%v", ok, err)
	}
	if ok, err := nfsIngressOK(nil, fakeClusterSG); ok || err != nil {
		t.Errorf("no rule: ok=%v err=%v", ok, err)
	}
	open := nfs
	open.IpRanges = []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}
	other := nfs
	other.UserIdGroupPairs = []ec2types.UserIdGroupPair{{GroupId: aws.String("sg-other")}}
	ssh := nfs
	ssh.FromPort, ssh.ToPort = aws.Int32(22), aws.Int32(22)
	for name, p := range map[string]ec2types.IpPermission{"an address range": open, "another group": other, "another port": ssh} {
		if _, err := nfsIngressOK([]ec2types.IpPermission{nfs, p}, fakeClusterSG); err == nil || !strings.Contains(err.Error(), "never removes a rule") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestCreateEFSUpdatesAnOwnedAddonThatDiffers(t *testing.T) {
	f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
	f.addons[efsAddon] = &ekstypes.Addon{AddonName: aws.String(efsAddon), AddonVersion: aws.String("v2.0.0-eksbuild.1"),
		Status: ekstypes.AddonStatusActive, Tags: map[string]string{OwnerTagKey: "demo.us-west-2"}}
	installNFS(t, f, db)
	if err := createEFS(context.Background(), testEnv(t, aws_, false)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.writes, "UpdateAddon aws-efs-csi-driver v3.5.3-eksbuild.1") || !sameJSON(aws.ToString(f.addons[efsAddon].ConfigurationValues), efsAddonConfig) {
		t.Errorf("add-on not updated: %v", f.writes)
	}
}

// ---- teardown ----------------------------------------------------------------

// efsDeployed is an account where the efs plugin ran: in tdFake the cluster,
// the association, the role and the security group; in f the add-on, the
// file system with two access points and two mount targets.
func efsDeployed(fsOwner string) (*tdFake, *fakeNFS) {
	td := newTDFake()
	td.put(&tdObj{kind: "cluster", id: "demo", vpc: "vpc-1", tags: own()})
	td.put(&tdObj{kind: "assoc", id: "a-efs", ns: "kube-system", sa: "efs-csi-controller-sa", role: roleARN("demo-us-west-2-efs-csi-controller-sa"), tags: own()})
	td.put(&tdObj{kind: "assoc", id: "a-ebs", ns: "kube-system", sa: "ebs-csi-controller-sa", tags: own()})
	td.put(&tdObj{kind: "role", id: "demo-us-west-2-efs-csi-controller-sa", inline: []string{"access", "s3files"}, tags: own()})
	td.put(&tdObj{kind: kSG, id: "sg-efs", name: "demo-efs", vpc: "vpc-1", tags: own()})
	f := newFakeNFS()
	f.addons[efsAddon] = &ekstypes.Addon{AddonName: aws.String(efsAddon), Status: ekstypes.AddonStatusActive, Tags: own()}
	f.fss["fs-1"] = &nfsFS{id: "fs-1", token: "demo-actors", state: "available", tags: map[string]string{OwnerTagKey: fsOwner}}
	f.aps["fsap-1"], f.aps["fsap-2"] = "fs-1", "fs-1"
	f.mts["fsmt-1"] = &nfsMT{id: "fsmt-1", fs: "fs-1", az: "usw2-az1", state: "available"}
	f.mts["fsmt-2"] = &nfsMT{id: "fsmt-2", fs: "fs-1", az: "usw2-az2", state: "available"}
	return td, f
}

func TestDeletePluginEFS(t *testing.T) {
	d := pluginDefs["efs"]
	td, f := efsDeployed(tdOwner)
	e, _, logs := tdTestEnv(t, td, false, false)
	installNFS(t, f, newFakeDB())
	if err := e.deletePlugin(context.Background(), d); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	order := strings.Join(f.writes, ",")
	for _, w := range []string{"DeleteAddon aws-efs-csi-driver", "DeleteAccessPoint fsap-1", "DeleteMountTarget fsmt-1", "DeleteFileSystem fs-1"} {
		if !strings.Contains(order, w) {
			t.Errorf("missing %q in %s", w, order)
		}
	}
	if strings.Index(order, "DeleteAddon") > strings.Index(order, "DeleteFileSystem") ||
		strings.Index(order, "DeleteMountTarget fsmt-2") > strings.Index(order, "DeleteFileSystem") {
		t.Errorf("deleted out of order: %s", order)
	}
	for _, k := range []string{"assoc|a-efs", "role|demo-us-west-2-efs-csi-controller-sa", kSG + "|sg-efs"} {
		if td.objs[k] != nil {
			t.Errorf("%s is left", k)
		}
	}
	if td.objs["assoc|a-ebs"] == nil {
		t.Error("deleted the EBS CSI driver's association, which is not the plugin's")
	}
	if len(f.fss)+len(f.mts)+len(f.aps)+len(f.addons) > 0 || !strings.Contains(logs.String(), "Plugin efs deleted") {
		t.Errorf("left %v %v %v %v\n%s", f.fss, f.mts, f.aps, f.addons, logs)
	}
	// Run again: nothing to delete, still clean.
	f.writes = nil
	if err := e.deletePlugin(context.Background(), d); err != nil || len(f.writes) > 0 {
		t.Errorf("rerun: %v, writes %v", err, f.writes)
	}
}

// A file system with the plugin's creation token and another owner stops
// the delete before anything is deleted, the add-on included.
func TestDeletePluginEFSRefusesForeign(t *testing.T) {
	d := pluginDefs["efs"]
	td, f := efsDeployed("other.us-west-2")
	e, _, _ := tdTestEnv(t, td, false, false)
	installNFS(t, f, newFakeDB())
	err := e.deletePlugin(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "EFS file system fs-1 (demo-actors) is not owned by demo.us-west-2") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if len(f.writes) > 0 || len(td.calls) > 0 {
		t.Errorf("deleted after a refusal: %v %v", f.writes, td.calls)
	}
}

func TestEFSInventory(t *testing.T) {
	d := pluginDefs["efs"]
	td, f := efsDeployed(tdOwner)
	e, _, _ := tdTestEnv(t, td, false, false)
	installNFS(t, f, newFakeDB())
	rows, err := d.Inventory(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"EKS add-on aws-efs-csi-driver", "Pod Identity association kube-system/efs-csi-controller-sa (a-efs)",
		"IAM role demo-us-west-2-efs-csi-controller-sa", "EFS file system fs-1 (demo-actors)", "EFS access point fsap-1",
		"EFS mount target fsmt-2", "security group demo-efs (sg-efs)"} {
		if !hasRow(rows, want) {
			t.Errorf("inventory lacks %q", want)
		}
	}
	if hasRow(rows, "Pod Identity association kube-system/ebs-csi-controller-sa (a-ebs)") {
		t.Error("listed the EBS CSI driver's association")
	}
}
