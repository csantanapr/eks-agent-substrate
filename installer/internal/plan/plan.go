// Package plan lists the eks-talk scripts the installer runs, in order, and
// the checklist rows each one is expected to light up. A row lights up when
// the script prints its "== <step>" line (log() in lib/common.sh).
package plan

import "strings"

// Row is one expected checklist line: a short title, and the prefixes of the
// "== <step>" texts that belong to it.
type Row struct {
	Title string
	Match []string
	// Optional rows may not print at all (a step a rerun skips); matching
	// may pass over them.
	Optional bool
}

// Command is one script run in the target checkout.
type Command struct {
	ID    string
	Title string
	Env   []string // extra KEY=VALUE for this command only
	Args  []string // run as: bash Args...
	Rows  []Row
	// Minutes is a rough duration for the Review screen; Summary says what
	// the command makes.
	Minutes int
	Summary string
}

// Display is the command as a person would type it.
func (c Command) Display() string {
	s := strings.Join(c.Args, " ")
	if len(c.Env) > 0 {
		s = strings.Join(c.Env, " ") + " " + s
	}
	return s
}

// Demo is an optional step on the Settings screen.
type Demo struct {
	ID       string
	Title    string
	Note     string
	Default  bool
	Disabled string // why it cannot be chosen here, when set
	Locked   bool   // always on (deploy.sh runs it)
}

// Demos are the demo choices, in the order they run.
var Demos = []Demo{
	{ID: "counter", Title: "Counter on gVisor and microVM", Note: "deploy.sh always deploys it", Default: true, Locked: true},
	{ID: "counter-verify", Title: "Counter suspend/resume check", Note: "demos/counter/verify.sh, about 10 min", Default: true},
	{ID: "efs", Title: "Amazon EFS volumes per actor", Note: "demos/efs, about 10 min"},
	{ID: "s3files", Title: "Amazon S3 Files volumes per actor", Note: "demos/s3files, needs EFS"},
	{ID: "db-rotation", Title: "Secrets Manager login rotation", Note: "demos/db-rotation"},
	{ID: "credprovider-aws", Title: "Secrets Manager credential provider", Disabled: "its secret names are account-wide; run it from one checkout only"},
	{ID: "openclaw", Title: "OpenClaw always-on agent", Disabled: "needs OPENCLAW_DIR in config.local.env"},
}

var createCluster = Command{
	ID: "create-cluster", Title: "Create the EKS cluster", Args: []string{"./create-cluster.sh"},
	Minutes: 20, Summary: "EKS control plane, add-ons, three managed node groups",
	Rows: []Row{
		{Title: "Preflight: nested virtualization", Match: []string{"Preflight"}},
		{Title: "Launch template for microVM nodes", Match: []string{"Launch template"}},
		{Title: "EKS control plane, add-ons, node groups", Match: []string{"eksctl create cluster", "Add the missing"}, Optional: true},
		{Title: "Tag the Auto Scaling groups", Match: []string{"Tag the node group"}},
		{Title: "Default StorageClass (encrypted gp3)", Match: []string{"Default StorageClass"}},
		{Title: "Node IMDS settings", Match: []string{"Node IMDS settings"}},
	},
}

var deploy = Command{
	ID: "deploy", Title: "Deploy Agent Substrate", Args: []string{"./deploy.sh"},
	Minutes: 15, Summary: "S3, ECR, IAM, RDS, Secrets, Substrate, LLM gateway, counter",
	Rows: []Row{
		{Title: "S3 snapshot and asset buckets", Match: []string{"S3 buckets"}},
		{Title: "ECR repositories", Match: []string{"ECR repositories"}},
		{Title: "IAM roles for EKS Pod Identity", Match: []string{"IAM roles for EKS Pod Identity"}},
		{Title: "Amazon RDS for PostgreSQL", Match: []string{"Amazon RDS for PostgreSQL"}},
		{Title: "Secrets Manager database logins", Match: []string{"Secrets Manager"}},
		{Title: "Substrate source with the EKS patch", Match: []string{"Clone ", "Apply "}, Optional: true},
		{Title: "Build ate-setup and kubectl-ate", Match: []string{"Build ate-setup"}},
		{Title: "Wait for RDS", Match: []string{"Wait for "}},
		{Title: "Database roles and schema", Match: []string{"Bootstrap roles"}},
		{Title: "Agent Substrate (ate-setup deploy)", Match: []string{"ate-setup deploy ate-system"}},
		{Title: "LLM gateway for Amazon Bedrock", Match: []string{"IAM role for the LLM gateway", "Deploy the gateway", "Smoke test"}},
		{Title: "Counter image to Amazon ECR", Match: []string{"Build and push the counter image"}},
		{Title: "Counter on gVisor", Match: []string{"Counter on gvisor"}},
		{Title: "Counter on microVM", Match: []string{"Counter on microvm"}},
		{Title: "OpenClaw demo", Match: []string{"Deploy the OpenClaw demo"}, Optional: true},
		{Title: "Ready", Match: []string{"Done."}},
	},
}

var counterVerify = Command{
	ID: "counter-verify", Title: "Check the counter demo", Args: []string{"demos/counter/verify.sh"},
	Minutes: 10, Summary: "suspend and resume a counter on gVisor and on microVM",
	Rows: []Row{
		{Title: "Suspend and resume on gVisor", Match: []string{"demo-gvisor"}},
		{Title: "Suspend and resume on microVM", Match: []string{"demo-microvm"}},
	},
}

var efs = []Command{
	{ID: "efs", Title: "Amazon EFS volumes", Args: []string{"demos/efs/deploy.sh"}, Minutes: 10, Summary: "EFS, the EFS CSI add-on, per-actor access points", Rows: []Row{
		{Title: "EFS file system", Match: []string{"Amazon EFS file system"}},
		{Title: "Security group (NFS)", Match: []string{"Security group"}},
		{Title: "Mount targets", Match: []string{"Mount targets"}},
		{Title: "IAM role for the EFS CSI controllers", Match: []string{"IAM role for the EFS"}},
		{Title: "EKS add-on aws-efs-csi-driver", Match: []string{"EKS add-on"}},
		{Title: "Substrate's EFS CSI controller", Match: []string{"Substrate's EFS CSI"}},
		{Title: "Substrate with ExternalVolumes", Match: []string{"Agent Substrate with"}},
		{Title: "CSIDriverConfig and StorageClass", Match: []string{"CSIDriverConfig"}},
		{Title: "Counter templates on EFS", Match: []string{"Counter templates"}},
	}},
	{ID: "efs-verify", Title: "Check the EFS volumes", Args: []string{"demos/efs/verify.sh"}, Minutes: 5, Summary: "a file counter survives suspend/resume"},
}

var s3files = []Command{
	{ID: "s3files", Title: "Amazon S3 Files volumes", Args: []string{"demos/s3files/deploy.sh"}, Minutes: 8, Summary: "an S3 file system and per-actor access points", Rows: []Row{
		{Title: "S3 bucket (versioned, private)", Match: []string{"S3 bucket"}},
		{Title: "IAM role for S3 Files sync", Match: []string{"IAM role S3 Files"}},
		{Title: "S3 file system", Match: []string{"S3 file system"}},
		{Title: "Security group and mount targets", Match: []string{"Security group"}},
		{Title: "IAM for the CSI driver", Match: []string{"IAM: "}},
		{Title: "StorageClass", Match: []string{"StorageClass"}},
		{Title: "Counter templates on S3 Files", Match: []string{"Counter templates"}},
	}},
	{ID: "s3files-verify", Title: "Check the S3 Files volumes", Env: []string{"STORAGE=s3files"}, Args: []string{"demos/efs/verify.sh"}, Minutes: 5, Summary: "the same check on S3 Files"},
}

var dbRotation = []Command{
	{ID: "db-rotation", Title: "Database login rotation", Args: []string{"demos/db-rotation/deploy.sh"}, Minutes: 8, Summary: "rotation functions, External Secrets, Reloader", Rows: []Row{
		{Title: "Security group for rotation", Match: []string{"Security group"}},
		{Title: "Connection fields on the logins", Match: []string{"Connection fields"}},
		{Title: "External Secrets Operator", Match: []string{"External Secrets Operator"}},
		{Title: "SecretStore and ExternalSecret", Match: []string{"SecretStore"}},
		{Title: "ESO Reloader", Match: []string{"ESO Reloader"}},
		{Title: "Both users of each login", Match: []string{"Both users"}},
		{Title: "Rotation functions and schedules", Match: []string{"Rotation functions"}},
	}},
	{ID: "db-rotation-verify", Title: "Check the login rotation", Args: []string{"demos/db-rotation/verify.sh"}, Minutes: 5, Summary: "rotate a login and watch the rollout"},
}

// Install returns the commands for an install with the chosen demos.
func Install(demos map[string]bool) []Command {
	cmds := []Command{createCluster, deploy}
	if demos["counter-verify"] {
		cmds = append(cmds, counterVerify)
	}
	if demos["efs"] || demos["s3files"] {
		cmds = append(cmds, efs...)
	}
	if demos["s3files"] {
		cmds = append(cmds, s3files...)
	}
	if demos["db-rotation"] {
		cmds = append(cmds, dbRotation...)
	}
	return cmds
}

// Teardown returns clean.sh and delete-cluster.sh, both with --yes: the
// installer asks for the cluster name itself before it runs them.
func Teardown() []Command {
	return []Command{
		{ID: "clean", Title: "Remove Agent Substrate and its AWS data", Args: []string{"./clean.sh", "--yes"}, Minutes: 12, Summary: "demos, Substrate, RDS, S3, Secrets Manager, ECR, IAM; then the inventory", Rows: []Row{
			{Title: "Demos and the LLM gateway", Match: []string{"Demos and the LLM gateway"}},
			{Title: "Agent Substrate uninstall", Match: []string{"Agent Substrate"}},
			{Title: "Pod Identity associations", Match: []string{"Pod Identity associations"}},
			{Title: "IAM roles", Match: []string{"IAM roles"}},
			{Title: "Amazon RDS delete", Match: []string{"Amazon RDS"}},
			{Title: "Secrets Manager", Match: []string{"Secrets Manager"}},
			{Title: "S3 buckets", Match: []string{"S3 buckets"}},
			{Title: "ECR repositories", Match: []string{"ECR repositories"}},
			{Title: "Inventory: nothing left", Match: []string{"What is left"}},
		}},
		{ID: "delete-cluster", Title: "Delete the EKS cluster", Args: []string{"./delete-cluster.sh", "--yes"}, Minutes: 15, Summary: "node groups, control plane, VPC, launch template; then the inventory", Rows: []Row{
			{Title: "Preflight inventory", Match: []string{"Preflight"}},
			{Title: "eksctl delete cluster", Match: []string{"eksctl delete cluster"}, Optional: true},
			{Title: "Launch template", Match: []string{"Launch template"}},
			{Title: "Cluster deleted", Match: []string{"Cluster deleted"}},
		}},
	}
}

// ByID returns the command with id from the full catalog (install and
// teardown), for replaying a recorded log.
func ByID(id string) (Command, bool) {
	all := append(Install(map[string]bool{"counter-verify": true, "s3files": true, "db-rotation": true}), Teardown()...)
	for _, c := range all {
		if c.ID == id {
			return c, true
		}
	}
	return Command{}, false
}
