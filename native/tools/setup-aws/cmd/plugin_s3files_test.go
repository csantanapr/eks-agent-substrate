package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const (
	s3fBucket = "demo-files-111122223333-us-west-2"
	s3fCtrl   = "demo-us-west-2-efs-csi-controller-sa"
)

func TestCreateS3FilesFreshThenRerunWritesNothing(t *testing.T) {
	f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
	aws_.roles[s3fCtrl] = map[string]string{OwnerTagKey: "demo.us-west-2"} // the efs plugin's
	f.s3CreateFails = 2                                                    // the new sync role is not assumable yet
	installNFS(t, f, db)
	e := testEnv(t, aws_, false)
	if err := createS3Files(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"PutBucketVersioning " + s3fBucket, "S3Files.CreateFileSystem arn:aws:s3:::" + s3fBucket,
		"S3Files.CreateMountTarget subnet-a", "S3Files.CreateMountTarget subnet-b"} {
		if !slices.Contains(f.writes, w) {
			t.Errorf("missing %q in %v", w, f.writes)
		}
	}
	if slices.Contains(f.writes, "S3Files.CreateMountTarget subnet-a2") {
		t.Error("a second mount target in one Availability Zone")
	}
	if !f.versioned[s3fBucket] || aws_.buckets[s3fBucket][OwnerTagKey] != "demo.us-west-2" {
		t.Errorf("bucket versioned=%v tags=%v", f.versioned[s3fBucket], aws_.buckets[s3fBucket])
	}
	for _, w := range []string{"CreateBucket " + s3fBucket, "PutPublicAccessBlock " + s3fBucket, "PutBucketEncryption " + s3fBucket,
		"PutBucketPolicy " + s3fBucket, "CreateRole demo-us-west-2-s3files-sync", "CreateRole demo-us-west-2-efs-csi-node-sa",
		"CreatePodIdentityAssociation kube-system/efs-csi-node-sa"} {
		if !slices.Contains(aws_.writes, w) {
			t.Errorf("missing %q in %v", w, aws_.writes)
		}
	}
	for id, x := range f.s3fss {
		if x.tags[OwnerTagKey] != "demo.us-west-2" || x.tags["auto-delete"] != "no" {
			t.Errorf("file system %s tags %v", id, x.tags)
		}
	}
	if p := aws_.policies[s3fCtrl]; !strings.Contains(p, `"s3files:CreateAccessPoint","Resource":"arn:aws:s3files:us-west-2:111122223333:file-system/fs-`) {
		t.Errorf("controller s3files policy = %s", p)
	}
	if db.sgs["demo-s3files"] == nil {
		t.Error("no security group demo-s3files")
	}

	f.writes, aws_.writes, db.writes = nil, nil, nil
	if err := createS3Files(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if count(f.writes, "S3Files.Create")+len(db.writes)+count(aws_.writes, "Create") > 0 {
		t.Errorf("rerun created %v %v %v", f.writes, db.writes, aws_.writes)
	}
}

// Without the efs plugin there is no controller role to extend: stop
// before anything is made. With it, the run goes on (above).
func TestCreateS3FilesNeedsEFS(t *testing.T) {
	f, db, aws_ := newFakeNFS(), newFakeDB(), newFake()
	installNFS(t, f, db)
	err := createS3Files(context.Background(), testEnv(t, aws_, false))
	if err == nil || !strings.Contains(err.Error(), "run `setup-aws plugin efs` first") {
		t.Fatalf("want the efs prerequisite, got %v", err)
	}
	if len(f.writes)+len(aws_.writes) > 0 {
		t.Errorf("wrote %v %v", f.writes, aws_.writes)
	}
}

func s3filesDeployed(bucketOwner string) (*tdFake, *fakeNFS) {
	td := newTDFake()
	td.put(&tdObj{kind: "cluster", id: "demo", vpc: "vpc-1", tags: own()})
	td.put(&tdObj{kind: "assoc", id: "a-node", ns: "kube-system", sa: "efs-csi-node-sa", tags: own()})
	td.put(&tdObj{kind: "role", id: "demo-us-west-2-efs-csi-node-sa", inline: []string{"access"}, tags: own()})
	td.put(&tdObj{kind: "role", id: "demo-us-west-2-s3files-sync", inline: []string{"sync"}, tags: own()})
	td.put(&tdObj{kind: "role", id: s3fCtrl, inline: []string{"access", "s3files"}, tags: own()})
	td.put(&tdObj{kind: "bucket", id: s3fBucket, objects: 5, tags: map[string]string{OwnerTagKey: bucketOwner}})
	td.put(&tdObj{kind: kSG, id: "sg-s3f", name: "demo-s3files", vpc: "vpc-1", tags: own()})
	f := newFakeNFS()
	f.s3fss["fs-9"] = &nfsFS{id: "fs-9", bucket: "arn:aws:s3:::" + s3fBucket, state: "available", tags: map[string]string{OwnerTagKey: tdOwner}}
	f.s3aps["fsap-9"] = "fs-9"
	f.s3mts["s3mt-1"] = &nfsMT{id: "s3mt-1", fs: "fs-9", az: "usw2-az1", state: "available"}
	return td, f
}

func TestDeletePluginS3Files(t *testing.T) {
	d := pluginDefs["s3files"]
	td, f := s3filesDeployed(tdOwner)
	e, _, logs := tdTestEnv(t, td, false, false)
	installNFS(t, f, newFakeDB())
	if err := e.deletePlugin(context.Background(), d); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	order := strings.Join(f.writes, ",")
	if !strings.Contains(order, "S3Files.DeleteAccessPoint fsap-9") || !strings.Contains(order, "S3Files.DeleteFileSystem fs-9") ||
		strings.Index(order, "S3Files.DeleteMountTarget") > strings.Index(order, "S3Files.DeleteFileSystem") {
		t.Errorf("S3 Files deletes: %s", order)
	}
	for _, k := range []string{"assoc|a-node", "role|demo-us-west-2-efs-csi-node-sa", "role|demo-us-west-2-s3files-sync", "bucket|" + s3fBucket, kSG + "|sg-s3f"} {
		if td.objs[k] != nil {
			t.Errorf("%s is left", k)
		}
	}
	// The efs plugin's role stays, without the s3files policy.
	if r := td.objs["role|"+s3fCtrl]; r == nil || !slices.Equal(r.inline, []string{"access"}) {
		t.Errorf("controller role = %+v", r)
	}
	if len(f.s3fss)+len(f.s3mts)+len(f.s3aps) > 0 || !strings.Contains(logs.String(), "Plugin s3files deleted") {
		t.Errorf("left %v %v %v\n%s", f.s3fss, f.s3mts, f.s3aps, logs)
	}
	if !slices.Contains(td.calls, "DeleteBucket "+s3fBucket) || !slices.Contains(td.calls, "DeleteObjects "+s3fBucket) {
		t.Errorf("bucket not emptied then deleted: %v", td.calls)
	}
}

func TestDeletePluginS3FilesRefusesForeignBucket(t *testing.T) {
	d := pluginDefs["s3files"]
	td, f := s3filesDeployed("other.us-west-2")
	e, _, _ := tdTestEnv(t, td, false, false)
	installNFS(t, f, newFakeDB())
	err := e.deletePlugin(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "S3 bucket "+s3fBucket+" is not owned") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if len(f.writes)+len(td.calls) > 0 {
		t.Errorf("deleted after a refusal: %v %v", f.writes, td.calls)
	}
}

// A file system ListFileSystems still returns but GetFileSystem reports gone
// (its delete just finished) is dropped: not refused as another owner's, not
// listed. One GetFileSystem returns with another owner's tag still refuses.
func TestS3FileSystemsDropsOneThatIsGone(t *testing.T) {
	for _, tc := range []struct {
		name, refusal string
		vanished      bool
	}{
		{"gone", "", true},
		{"another owner's", "S3 file system fs-x is not owned by demo.us-west-2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := pluginDefs["s3files"]
			td, f := s3filesDeployed(tdOwner)
			e, _, logs := tdTestEnv(t, td, false, false)
			installNFS(t, f, newFakeDB())
			f.s3fss["fs-x"] = &nfsFS{id: "fs-x", bucket: "arn:aws:s3:::" + s3fBucket, state: "available", tags: map[string]string{OwnerTagKey: "other.us-west-2"}}
			f.s3Vanished = map[string]bool{"fs-x": tc.vanished}
			rows, err := s3filesInventory(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			if hasRow(rows, "S3 file system fs-x") == tc.vanished {
				t.Errorf("inventory lists fs-x: %v; want %v", hasRow(rows, "S3 file system fs-x"), !tc.vanished)
			}
			err = e.deletePlugin(context.Background(), d)
			switch {
			case tc.refusal == "" && err != nil:
				t.Fatalf("delete: %v\n%s", err, logs)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
				t.Fatalf("want %q, got %v", tc.refusal, err)
			}
			if strings.Contains(strings.Join(f.writes, ","), "DeleteFileSystem fs-x") {
				t.Errorf("deleted fs-x: %v", f.writes)
			}
		})
	}
}

func TestS3FilesInventory(t *testing.T) {
	td, f := s3filesDeployed(tdOwner)
	e, _, _ := tdTestEnv(t, td, false, false)
	installNFS(t, f, newFakeDB())
	f.addons[efsAddon] = &ekstypes.Addon{Tags: own()} // the efs plugin's, not listed here
	rows, err := s3filesInventory(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Pod Identity association kube-system/efs-csi-node-sa (a-node)", "S3 bucket " + s3fBucket,
		"S3 file system fs-9", "S3 Files access point fsap-9", "S3 Files mount target s3mt-1",
		"IAM role demo-us-west-2-efs-csi-node-sa", "IAM role demo-us-west-2-s3files-sync", "IAM role policy " + s3fCtrl + "/s3files",
		"security group demo-s3files (sg-s3f)"} {
		if !hasRow(rows, want) {
			t.Errorf("inventory lacks %q", want)
		}
	}
	if hasRow(rows, "EKS add-on aws-efs-csi-driver") || hasRow(rows, "IAM role "+s3fCtrl) {
		t.Error("listed the efs plugin's resources")
	}
}
