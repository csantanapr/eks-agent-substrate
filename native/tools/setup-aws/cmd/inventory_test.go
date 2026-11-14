package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Live, a --keep-cluster teardown failed its final check: the tagging API
// still listed the RDS instance's automated snapshot, which RDS had deleted
// with the instance. Inventory now asks RDS. A gone automated snapshot is
// not listed; a manual snapshot that exists is (it is data); a denied read
// stops the inventory instead of passing as gone.
func TestInventoryReadsRDSSnapshotsFromRDS(t *testing.T) {
	tag := func(arn string) rgttypes.ResourceTagMapping {
		return rgttypes.ResourceTagMapping{ResourceARN: aws.String(arn),
			Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}}
	}
	const auto = "arn:aws:rds:us-west-2:111122223333:snapshot:rds:demo-pg-2026-11-13-23-14"
	const manual = "arn:aws:rds:us-west-2:111122223333:snapshot:demo-pg-before-upgrade"

	f := newTDFake()
	f.tagged = append(f.tagged, tag(auto), tag(manual))
	f.put(&tdObj{kind: "rds snapshot", id: "demo-pg-before-upgrade", state: "available"})
	e, _, _ := tdTestEnv(t, f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Kind+" "+r.ID] = true
	}
	if got["rds snapshot rds:demo-pg-2026-11-13-23-14"] {
		t.Error("a gone automated snapshot was listed (stale tagging entry)")
	}
	if !got["rds snapshot demo-pg-before-upgrade"] {
		t.Error("an existing manual snapshot was not listed")
	}

	// The automated snapshot still exists (deleting does not count).
	f.put(&tdObj{kind: "rds snapshot", id: "rds:demo-pg-2026-11-13-23-14", state: "available"})
	if rows, _ = e.inventory(context.Background()); !hasRow(rows, "rds snapshot rds:demo-pg-2026-11-13-23-14") {
		t.Error("an existing automated snapshot was not listed")
	}
	f.get("rds snapshot", "rds:demo-pg-2026-11-13-23-14").state = "deleting"
	if rows, _ = e.inventory(context.Background()); hasRow(rows, "rds snapshot rds:demo-pg-2026-11-13-23-14") {
		t.Error("a deleting automated snapshot was listed")
	}

	f.deny["DescribeDBSnapshots"] = true
	if _, err := e.inventory(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("a denied snapshot read did not stop the inventory: %v", err)
	}
}

func hasRow(rows []tdRow, kindID string) bool {
	for _, r := range rows {
		if r.Kind+" "+r.ID == kindID {
			return true
		}
	}
	return false
}

func TestInventoryListsTheDeployment(t *testing.T) {
	f := fullDeployment()
	// A plugin script's file system: a type setup-aws does not manage.
	f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String("arn:aws:elasticfilesystem:us-west-2:111122223333:file-system/fs-1"),
		Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
	e, out, _ := tdTestEnv(t, f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Kind+" "+r.ID] = true
	}
	for _, want := range []string{
		"EKS cluster demo", "EKS node group system", "EKS node group gvisor", "EKS node group microvm",
		"Pod Identity association ate-system/ate-api-server (a-api)", "Pod Identity association ate-system/atelet (a-atelet)",
		"Pod Identity association kube-system/ebs-csi-controller-sa (a-ebs)",
		"IAM role demo-us-west-2-ate-api-server", "IAM role demo-us-west-2-atelet", "IAM role demo-us-west-2-eks-cluster",
		"IAM role demo-us-west-2-eks-auto-node", "IAM role demo-us-west-2-eks-node", "IAM role demo-us-west-2-ebs-csi-driver",
		"RDS instance demo-pg", "RDS subnet group demo-pg", "security group demo-pg (sg-pg)",
		"secret demo/ate-owner", "secret demo/ate-readwrite", "secret rds!db-0001",
		"S3 bucket demo-snap-111122223333-us-west-2", "S3 bucket demo-assets-111122223333-us-west-2", "ECR repository demo/counter",
		"launch template demo-microvm-nested-virt (lt-1)", "launch template demo-gvisor-nodes (lt-3)", "VPC vpc-1", "subnet subnet-pub-a", "subnet subnet-priv-b",
		"internet gateway igw-1", "NAT gateway nat-1", "Elastic IP eipalloc-1", "route table rtb-pub",
		"network interface eni-node", "EBS volume vol-pvc", "EBS volume vol-root", "EC2 instance i-node",
		"security group default (sg-default)", "security group eks-cluster-sg-demo-1 (sg-eks)", "route table rtb-main",
		"elasticfilesystem file-system fs-1",
	} {
		if !got[want] {
			t.Errorf("inventory lacks %q", want)
		}
	}
	for row := range got {
		for _, never := range []string{"someone-elses", "demo-2-", "other/app", "a-app", "vpc-default", "eni-left"} {
			if strings.Contains(row, never) {
				t.Errorf("listed what is not this deployment's: %s", row)
			}
		}
		// The tagging API's stale entries of known types are not listed again.
		if strings.HasPrefix(row, "eks ") || strings.HasPrefix(row, "ec2 ") || strings.HasPrefix(row, "rds ") || strings.HasPrefix(row, "s3 ") {
			t.Errorf("listed a tagging-API entry of a type read per service: %s", row)
		}
	}
	// What --keep-cluster keeps is marked, and only that.
	for _, r := range rows {
		cluster := strings.HasPrefix(r.Kind, "EKS") || r.ID == "kube-system/ebs-csi-controller-sa (a-ebs)" ||
			(r.Kind == "IAM role" && !strings.HasSuffix(r.ID, "ate-api-server") && !strings.HasSuffix(r.ID, "atelet")) ||
			(r.Kind == kSG && r.ID != "demo-pg (sg-pg)") || r.Kind == kVPC || r.Kind == kSubnet || r.Kind == kIGW || r.Kind == kNAT ||
			r.Kind == kEIP || r.Kind == kRTB || r.Kind == kLT || r.Kind == kENI || r.Kind == kVolume || r.Kind == kInstance
		if r.Cluster != cluster {
			t.Errorf("%s %s: Cluster = %v", r.Kind, r.ID, r.Cluster)
		}
	}
	e.printRows(rows)
	if s := out.String(); strings.Contains(s, "111122223333") || !strings.Contains(s, "demo-snap-********3333-us-west-2") ||
		!strings.Contains(s, "TYPE") || !strings.Contains(s, "setup-aws") {
		t.Errorf("table not masked or incomplete:\n%s", s)
	}
}

func TestInventoryCommandExitCodes(t *testing.T) {
	run := func(f *tdFake) (code int, out string) {
		tdTestEnv(t, f, false, false)
		code = -1
		prev := tdExit
		tdExit = func(c int) { code = c }
		defer func() { tdExit = prev }()
		var b bytes.Buffer
		inventoryCmd.SetContext(context.Background())
		inventoryCmd.SetOut(&b)
		if err := inventoryCmd.RunE(inventoryCmd, nil); err != nil {
			t.Fatal(err)
		}
		return code, b.String()
	}
	if code, out := run(fullDeployment()); code != 3 || !strings.Contains(out, "are left") {
		t.Errorf("deployment present: exit %d, output %q; want exit 3", code, out)
	}
	if code, out := run(newTDFake()); code != -1 || !strings.Contains(out, "nothing left for demo.us-west-2") {
		t.Errorf("empty account: exit %d, output %q; want no exit (0)", code, out)
	}
}

// The core inventory lists every owner-tagged role that is not a Substrate
// role as a cluster row, and a plugin lists its own role again as a
// non-cluster row. Under --keep-cluster, a plugin role left behind must fail
// the final check, while the cluster's own roles (only the core lists them)
// are kept.
func TestKeepClusterSeesALeftoverPluginRole(t *testing.T) {
	const leftover = "demo-us-west-2-efs-csi-controller-sa"
	for _, left := range []bool{false, true} {
		f := fullDeployment()
		e, out, logs := tdTestEnv(t, f, false, true)
		if left {
			f.put(&tdObj{kind: "role", id: leftover, tags: own()})
			// A plugin whose teardown left its role behind.
			pluginDefs["leaky"] = pluginDef{ID: "leaky", Inventory: func(ctx context.Context, e *tdEnv) ([]tdRow, error) {
				return e.ownedRoles(ctx, leftover)
			}}
		}
		err := e.teardown(context.Background())
		switch {
		case !left && err != nil:
			t.Errorf("only the cluster's own roles are left: teardown --keep-cluster failed: %v\n%s", err, logs)
		case left && (err == nil || !strings.Contains(err.Error(), "1 resources of demo.us-west-2 are left") ||
			!strings.Contains(out.String()[strings.LastIndex(out.String(), "TYPE"):], leftover)):
			t.Errorf("plugin role %s left: teardown --keep-cluster returned %v, want the final check to fail naming it", leftover, err)
		}
		if f.get("role", "demo-us-west-2-eks-cluster") == nil {
			t.Error("--keep-cluster deleted the cluster's role")
		}
	}
}

// AC2.3: a denied read stops the inventory; it never reads as "nothing left".
func TestInventoryDeniedReadIsAnError(t *testing.T) {
	e, _, _ := tdTestEnv(t, newTDFake(), false, false)
	e.c.S3 = denyS3{e.c.S3}
	if _, err := e.inventory(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want AccessDenied, got %v", err)
	}
}

type denyS3 struct{ tdS3API }

func (denyS3) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return nil, apiErr("AccessDenied")
}
