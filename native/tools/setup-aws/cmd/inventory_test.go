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
