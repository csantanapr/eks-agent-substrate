// Package phases is the contract between setup-aws and the installer: the
// phase titles each setup-aws command logs, in order, as "Step i/N: <title>".
// The installer's progress checklists light a row when its phase's line
// appears, so both sides import these lists instead of copying strings.
//
// It has no dependencies and no init side effects, so the installer can
// import it (through a replace directive, as substrate-gke's installer
// imports tools/setup-gcp) without pulling in cobra or the AWS SDK.
package phases

import "fmt"

// Bootstrap is `setup-aws bootstrap`.
var Bootstrap = []string{
	"Checking the EKS cluster...",
	"Creating S3 buckets for snapshots and microVM assets...",
	"Creating EKS Pod Identity roles for ate-api-server and atelet...",
	"Creating ECR repositories...",
}

// Cluster is `setup-aws create cluster`. A phase with nothing to do (Auto
// Mode off, a resource that already exists) still logs its line, so the
// checklist never stalls on a skipped row.
var Cluster = []string{
	"Checking the account, Region and quotas...",
	"Creating the VPC and subnets...",
	"Creating the cluster and node IAM roles...",
	"Creating the EKS cluster...",
	"Granting cluster access...",
	"Installing EKS add-ons...",
	"Creating the microvm launch template...",
	"Creating the managed node groups...",
	"Waiting for the node groups to be ACTIVE...",
}

// RDS is `setup-aws create rds`.
var RDS = []string{
	"Creating the RDS subnet group and security group...",
	"Creating the RDS PostgreSQL instance...",
	"Creating the Substrate database logins in Secrets Manager...",
	"Waiting for the RDS instance to be available...",
}

// Teardown is `setup-aws teardown`. With --keep-cluster the node group,
// cluster and network phases still log their line and delete nothing.
var Teardown = []string{
	"Listing this deployment's resources...",
	"Deleting plugin resources...",
	"Deleting Pod Identity associations and IAM roles...",
	"Deleting the RDS instance and its logins...",
	"Emptying and deleting the S3 buckets...",
	"Deleting the ECR repositories...",
	"Deleting the managed node groups...",
	"Deleting the EKS cluster...",
	"Deleting the launch template, VPC and network...",
	"Checking that nothing is left...",
}

// Line is the log line for phase i (0-based) of list: "Step i+1/N: <title>".
func Line(list []string, i int) string {
	return fmt.Sprintf("Step %d/%d: %s", i+1, len(list), list[i])
}
