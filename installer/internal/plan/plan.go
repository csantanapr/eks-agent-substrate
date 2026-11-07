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

// Sections of the Settings screen's choices.
const (
	SectionDemos   = "Demos"
	SectionPlugins = "Plugins"
)

// Demo is an optional step on the Settings screen: a demo, or a plugin that
// adds an AWS integration to the install.
type Demo struct {
	ID       string
	Section  string // SectionDemos or SectionPlugins
	Title    string
	Note     string
	Default  bool
	Disabled string // why it cannot be chosen here, when set
	Locked   bool   // always on (deploy.sh runs it)
}

// Demos are the choices, in the order the Settings screen shows them.
var Demos = []Demo{
	{ID: "counter", Section: SectionDemos, Title: "Counter on gVisor and microVM", Note: "deploy.sh always deploys it", Default: true, Locked: true},
	{ID: "counter-verify", Section: SectionDemos, Title: "Counter suspend/resume check", Note: "demos/counter/verify.sh --quick, about 1 min", Default: true},
	{ID: "openclaw", Section: SectionDemos, Title: "OpenClaw always-on agent",
		Note: "demos/openclaw: an always-on gateway, and an actor per conversation that thinks with Bedrock. Needs EFS, which turns on with it: the gateway keeps its state there. Clones the always-on-agent repository (branch eks) into .work/, unless OPENCLAW_DIR names a checkout. About 8 min."},
	{ID: "efs", Section: SectionPlugins, Title: "Amazon EFS volumes per actor", Note: "demos/efs, about 6 min"},
	{ID: "s3files", Section: SectionPlugins, Title: "Amazon S3 Files volumes per actor", Note: "demos/s3files, needs EFS"},
	{ID: "db-rotation", Section: SectionPlugins, Title: "Secrets Manager login rotation", Note: "demos/db-rotation"},
	{ID: "credprovider-aws", Section: SectionPlugins, Title: "Secrets Manager credential provider",
		Note: "demos/credprovider-aws: builds the provider from its checkout (CREDPROVIDER_DIR, default ~/credprovider-aws-secret-manager), then checks it injects an actor's secret and refuses the rest. About 3 min."},
}

// OpenClawRepo and OpenClawRef are what the OpenClaw choice clones when
// OPENCLAW_DIR is empty.
const (
	OpenClawRepo = "https://github.com/csantanapr/always-on-agent.git"
	OpenClawRef  = "eks"
)

var createCluster = Command{
	ID: "create-cluster", Title: "Create the EKS cluster", Args: []string{"./create-cluster.sh"},
	Minutes: 18, Summary: "EKS control plane, add-ons, three managed node groups",
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
	Minutes: 12, Summary: "S3, ECR, IAM, RDS, Secrets, Substrate, LLM gateway, counter",
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
		// Older deploy.sh runs deployed OpenClaw here; kept to replay their logs.
		{Title: "OpenClaw demo", Match: []string{"Deploy the OpenClaw demo"}, Optional: true},
		{Title: "Ready", Match: []string{"Done."}},
	},
}

// The installer skips the wait for each snapshot's copy in S3 (about 5
// minutes per actor); demos/counter/verify.sh without --quick checks it.
var counterVerify = Command{
	ID: "counter-verify", Title: "Check the counter demo", Args: []string{"demos/counter/verify.sh", "--quick"},
	Minutes: 1, Summary: "suspend and resume a counter on gVisor and on microVM",
	Rows: []Row{
		{Title: "Suspend and resume on gVisor", Match: []string{"demo-gvisor (ate-demo-counter)"}},
		{Title: "Suspend and resume on microVM", Match: []string{"demo-microvm (ate-demo-counter-microvm)"}},
	},
}

var efs = []Command{
	{ID: "efs", Title: "Amazon EFS volumes", Args: []string{"demos/efs/deploy.sh"}, Minutes: 4, Summary: "EFS, the EFS CSI add-on, per-actor access points", Rows: []Row{
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
	{ID: "efs-verify", Title: "Check the EFS volumes", Args: []string{"demos/efs/verify.sh"}, Minutes: 2, Summary: "a file counter survives suspend/resume"},
}

var s3files = []Command{
	{ID: "s3files", Title: "Amazon S3 Files volumes", Args: []string{"demos/s3files/deploy.sh"}, Minutes: 7, Summary: "an S3 file system and per-actor access points", Rows: []Row{
		{Title: "S3 bucket (versioned, private)", Match: []string{"S3 bucket"}},
		{Title: "IAM role for S3 Files sync", Match: []string{"IAM role S3 Files"}},
		{Title: "S3 file system", Match: []string{"S3 file system"}},
		{Title: "Security group and mount targets", Match: []string{"Security group"}},
		{Title: "IAM for the CSI driver", Match: []string{"IAM: "}},
		{Title: "StorageClass", Match: []string{"StorageClass"}},
		{Title: "Counter templates on S3 Files", Match: []string{"Counter templates"}},
	}},
	{ID: "s3files-verify", Title: "Check the S3 Files volumes", Env: []string{"STORAGE=s3files"}, Args: []string{"demos/efs/verify.sh"}, Minutes: 1, Summary: "the same check on S3 Files"},
}

var dbRotation = []Command{
	{ID: "db-rotation", Title: "Database login rotation", Args: []string{"demos/db-rotation/deploy.sh"}, Minutes: 7, Summary: "rotation functions, External Secrets, Reloader", Rows: []Row{
		{Title: "Security group for rotation", Match: []string{"Security group"}},
		{Title: "Connection fields on the logins", Match: []string{"Connection fields"}},
		{Title: "External Secrets Operator", Match: []string{"External Secrets Operator"}},
		{Title: "SecretStore and ExternalSecret", Match: []string{"SecretStore"}},
		{Title: "ESO Reloader", Match: []string{"ESO Reloader"}},
		{Title: "Both users of each login", Match: []string{"Both users"}},
		{Title: "Rotation functions and schedules", Match: []string{"Rotation functions"}},
	}},
}

// The installer no longer runs these; ByID keeps them to replay older logs.
var retired = []Command{
	{ID: "db-rotation-verify", Title: "Check the login rotation", Args: []string{"demos/db-rotation/verify.sh"}, Minutes: 12, Summary: "rotate a login and watch the rollout"},
}

var credprovider = []Command{
	{ID: "credprovider-aws", Title: "Secrets Manager credential provider", Args: []string{"demos/credprovider-aws/deploy.sh"}, Minutes: 2, Summary: "the provider, its IAM role, demo secrets, probe actors", Rows: []Row{
		{Title: "IAM role for the provider", Match: []string{"IAM role for the provider"}},
		{Title: "Demo secrets", Match: []string{"Secrets Manager: the demo secrets"}},
		{Title: "ECR repositories", Match: []string{"ECR repositories"}},
		{Title: "Provider image", Match: []string{"Provider image from"}},
		{Title: "Probe actor image", Match: []string{"Probe actor image"}},
		{Title: "Secret policy", Match: []string{"Secret policy:"}},
		{Title: "Provider in ate-system", Match: []string{"aws-sm-credential-provider (config/"}},
		{Title: "Register with the egress gateway", Match: []string{"atenet-egress serves"}},
		{Title: "Probe worker pool and templates", Match: []string{"Worker pool credprovider-probe"}},
	}},
	{ID: "credprovider-aws-verify", Title: "Check the credential provider", Args: []string{"demos/credprovider-aws/verify.sh"}, Minutes: 1, Summary: "inject a secret; refuse an untagged one and an unlisted atespace"},
}

var openclaw = []Command{
	{ID: "openclaw", Title: "OpenClaw demo", Args: []string{"demos/openclaw/deploy.sh"}, Minutes: 7, Summary: "the gateway (state on EFS), worker pool, agent template", Rows: []Row{
		{Title: "StorageClass on EFS", Match: []string{"StorageClass efs-openclaw"}},
		{Title: "Gateway state to EFS", Match: []string{"Move the gateway's state"}, Optional: true},
		{Title: "Gateway, workers and agent", Match: []string{"Deploy the OpenClaw demo"}},
	}},
	{ID: "openclaw-verify", Title: "Check the OpenClaw demo", Args: []string{"demos/openclaw/deploy.sh", "verify"},
		Minutes: 1, Summary: "the actor, the gateway, and the gateway-to-actor path"},
}

// Install returns the commands for an install with the chosen demos.
func Install(demos map[string]bool) []Command {
	cmds := []Command{createCluster, deploy}
	if demos["counter-verify"] {
		cmds = append(cmds, counterVerify)
	}
	if demos["efs"] || demos["s3files"] || demos["openclaw"] {
		cmds = append(cmds, efs...)
	}
	if demos["s3files"] {
		cmds = append(cmds, s3files...)
	}
	if demos["db-rotation"] {
		cmds = append(cmds, dbRotation...)
	}
	if demos["credprovider-aws"] {
		cmds = append(cmds, credprovider...)
	}
	// Last: the plugins above reinstall Substrate, which crashes running actors.
	if demos["openclaw"] {
		cmds = append(cmds, openclaw...)
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
	all := append(Install(map[string]bool{"counter-verify": true, "s3files": true, "db-rotation": true,
		"credprovider-aws": true, "openclaw": true}), Teardown()...)
	all = append(all, retired...)
	for _, c := range all {
		if c.ID == id {
			return c, true
		}
	}
	return Command{}, false
}
