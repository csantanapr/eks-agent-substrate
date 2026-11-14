package cmd

// The AWS half of the db-rotation plugin (installer/internal/plugin/dbrotation),
// eks-talk's demos/db-rotation/deploy.sh and clean.sh in Go: Secrets Manager
// rotation of the two RDS logins Substrate uses, with the AWS-hosted
// "alternating users" PostgreSQL function, and the Pod Identity role External
// Secrets Operator reads them with.
//
//   - a security group for the rotation functions, and a rule on the RDS
//     security group that admits it on 5432 (classifyDBIngress accepts it);
//   - the connection fields the function reads (engine, host, port, dbname,
//     masterarn) on both login secrets;
//   - the IAM role and Pod Identity association of external-secrets/
//     external-secrets, read-only on the two logins;
//   - the CloudFormation stack <cluster>-db-rotation (plugin_db_rotation.cfn.yaml):
//     the functions, their log groups and the rotation schedules.
//
// It needs the RDS instance `setup-aws create rds` makes, and refuses without
// one: the bundled PostgreSQL keeps its logins inside the cluster.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"
)

//go:embed plugin_db_rotation.cfn.yaml
var rotationTemplate string

const (
	dbRotationID = "db-rotation"
	// External Secrets Operator's namespace and ServiceAccount (the installer
	// half applies them).
	esoNamespace      = "external-secrets"
	esoServiceAccount = "external-secrets"
	kStack            = "cloudformation stack"
	kSGRule           = "security group rule"
	kRotation         = "secret rotation"
)

func rotationStackName(n Names) string { return n.Cluster + "-db-rotation" }
func esoRoleName(n Names) string       { return n.IAMPrefix + "-" + esoServiceAccount }

// The API surfaces the plugin calls beyond Clients and tdClients.
type (
	rotCFNAPI interface {
		DescribeStacks(context.Context, *cloudformation.DescribeStacksInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error)
		DescribeStackEvents(context.Context, *cloudformation.DescribeStackEventsInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStackEventsOutput, error)
		CreateStack(context.Context, *cloudformation.CreateStackInput, ...func(*cloudformation.Options)) (*cloudformation.CreateStackOutput, error)
		UpdateStack(context.Context, *cloudformation.UpdateStackInput, ...func(*cloudformation.Options)) (*cloudformation.UpdateStackOutput, error)
		DeleteStack(context.Context, *cloudformation.DeleteStackInput, ...func(*cloudformation.Options)) (*cloudformation.DeleteStackOutput, error)
	}
	// rotEC2API is rds.go's EC2API, so findSG and tagSGRules take it, plus
	// the revoke teardown makes.
	rotEC2API interface {
		EC2API
		RevokeSecurityGroupIngress(context.Context, *ec2.RevokeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
	}
	rotRDSAPI interface {
		DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	}
	rotSMAPI interface {
		DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
		GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
		PutSecretValue(context.Context, *secretsmanager.PutSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
		CancelRotateSecret(context.Context, *secretsmanager.CancelRotateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CancelRotateSecretOutput, error)
	}
)

type rotClients struct {
	CFN rotCFNAPI
	EC2 rotEC2API
	RDS rotRDSAPI
	SM  rotSMAPI
}

// rotationClientsFor is replaced in tests.
var rotationClientsFor = func(ctx context.Context, c Config) (*rotClients, error) {
	pc, err := newPluginClients(ctx, c)
	if err != nil {
		return nil, err
	}
	return &rotClients{CFN: pc.CFN, EC2: pc.EC2, RDS: pc.RDS, SM: pc.SM}, nil
}

func init() {
	registerPlugin(pluginDef{
		ID:    dbRotationID,
		Short: "Create Secrets Manager rotation of the RDS logins and the External Secrets role (the db-rotation plugin)",
		Create: func(ctx context.Context, e *env) error {
			rc, err := rotationClientsFor(ctx, e.cfg)
			if err != nil {
				return err
			}
			return e.createDBRotation(ctx, rc)
		},
		Teardown:  dbRotationTeardown,
		Inventory: dbRotationInventory,
	})
}

// rotationTarget is what the rotation needs to know about the database.
type rotationTarget struct {
	vpc, host, masterARN, dbSG string
	port                       int
	subnets                    []string
	secretARN                  map[string]string // login -> ARN
}

func (e *env) createDBRotation(ctx context.Context, rc *rotClients) error {
	if e.cfg.Plan {
		slog.Info("plan mode: reading only; nothing will be created or changed")
	}
	slog.Info("Starting db-rotation setup...", "cluster", e.names.Cluster, "region", e.names.Region)
	t, err := e.rotationTarget(ctx, rc)
	if err != nil {
		return err
	}
	rotSG, err := e.rotationSG(ctx, rc, t)
	if err != nil {
		return err
	}
	for _, who := range dbLogins {
		if err := e.connectionFields(ctx, rc.SM, who, t); err != nil {
			return err
		}
	}
	if err := e.roleForSA(ctx, PodIdentity{Namespace: esoNamespace, ServiceAccount: esoServiceAccount, Policy: esoPolicy(t)}); err != nil {
		return err
	}
	if err := e.rotationStack(ctx, rc, t, rotSG); err != nil {
		return err
	}
	for _, who := range dbLogins {
		s, err := rc.SM.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(e.names.DBSecret(who))})
		if err != nil {
			return fmt.Errorf("describe secret %s: %w", e.names.DBSecret(who), err)
		}
		sched := ""
		if s.RotationRules != nil {
			sched = aws.ToString(s.RotationRules.ScheduleExpression)
		}
		slog.Info("rotation", "secret", e.names.DBSecret(who), "enabled", aws.ToBool(s.RotationEnabled), "schedule", sched)
	}
	slog.Info("db-rotation setup completed successfully.")
	return nil
}

// rotationTarget reads the cluster's network, the RDS instance and its
// security group, and the login secrets, all of which must be this
// deployment's.
func (e *env) rotationTarget(ctx context.Context, rc *rotClients) (rotationTarget, error) {
	t := rotationTarget{secretARN: map[string]string{}}
	id := e.names.DBID()
	out, err := rc.RDS.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(id)})
	if isRDSNotFound(err) || (err == nil && len(out.DBInstances) == 0) {
		return t, fmt.Errorf("db-rotation rotates the logins of the RDS database, and %s has no RDS instance %s: "+
			"it needs an install with --database rds (setup-aws create rds); the bundled PostgreSQL keeps its logins in the cluster", e.names.Cluster, id)
	}
	if err != nil {
		return t, fmt.Errorf("describe RDS instance %s: %w", id, err)
	}
	db := out.DBInstances[0]
	if err := e.owned("RDS instance "+id, rdsTagMap(db.TagList)); err != nil {
		return t, err
	}
	if db.Endpoint == nil || db.MasterUserSecret == nil || aws.ToString(db.MasterUserSecret.SecretArn) == "" {
		return t, fmt.Errorf("RDS instance %s (%s) has no endpoint or RDS-managed master user secret yet; wait for setup-aws create rds to finish",
			id, aws.ToString(db.DBInstanceStatus))
	}
	t.host, t.masterARN = aws.ToString(db.Endpoint.Address), aws.ToString(db.MasterUserSecret.SecretArn)
	if t.port = int(aws.ToInt32(db.Endpoint.Port)); t.port == 0 {
		t.port = dbPort
	}

	cl, err := e.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(e.names.Cluster)})
	if err != nil {
		return t, fmt.Errorf("describe cluster %s: %w", e.names.Cluster, err)
	}
	if vc := cl.Cluster.ResourcesVpcConfig; vc != nil {
		t.vpc = aws.ToString(vc.VpcId)
	}
	if t.vpc == "" {
		return t, fmt.Errorf("cluster %s has no VPC", e.names.Cluster)
	}
	sg, err := findSG(ctx, rc.EC2, t.vpc, id)
	if err != nil {
		return t, err
	}
	if sg == nil {
		return t, fmt.Errorf("no security group %s in %s; run setup-aws create rds", id, t.vpc)
	}
	if err := e.owned("security group "+id, sgTagMap(sg.Tags)); err != nil {
		return t, err
	}
	t.dbSG = aws.ToString(sg.GroupId)
	sn, err := rc.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []ec2types.Filter{
		{Name: aws.String("vpc-id"), Values: []string{t.vpc}},
		{Name: aws.String("tag:" + dbSubnetTagKey), Values: []string{"1"}},
	}})
	if err != nil {
		return t, fmt.Errorf("describe subnets of %s: %w", t.vpc, err)
	}
	for _, s := range sn.Subnets {
		t.subnets = append(t.subnets, aws.ToString(s.SubnetId))
	}
	if len(t.subnets) == 0 {
		return t, fmt.Errorf("no private subnets (tag %s=1) in %s for the rotation functions", dbSubnetTagKey, t.vpc)
	}
	for _, who := range dbLogins {
		name := e.names.DBSecret(who)
		s, err := rc.SM.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
		if isNotFound(err) || (err == nil && s.DeletedDate != nil) {
			return t, fmt.Errorf("no secret %s; run setup-aws create rds", name)
		}
		if err != nil {
			return t, fmt.Errorf("describe secret %s: %w", name, err)
		}
		if err := e.owned("secret "+name, smTagMap(s.Tags)); err != nil {
			return t, err
		}
		t.secretARN[who] = aws.ToString(s.ARN)
	}
	return t, nil
}

// rotationSG makes the functions' security group and the rule on the RDS
// group that admits it, both tagged, and returns its ID ("" under --plan
// before it exists). The functions run in the VPC with network interfaces of
// their own, so RDS has to admit them.
func (e *env) rotationSG(ctx context.Context, rc *rotClients, t rotationTarget) (string, error) {
	name := e.names.RotationSGName()
	sg, err := findSG(ctx, rc.EC2, t.vpc, name)
	if err != nil {
		return "", err
	}
	id := ""
	if sg == nil {
		if err := e.act("create security group "+name+" in "+t.vpc, func() error {
			out, err := rc.EC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
				GroupName:         aws.String(name),
				VpcId:             aws.String(t.vpc),
				Description:       aws.String(e.names.Cluster + " Secrets Manager rotation functions"),
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
		if err := e.owned("security group "+name+" ("+aws.ToString(sg.GroupId)+")", sgTagMap(sg.Tags)); err != nil {
			return "", err
		}
		id = aws.ToString(sg.GroupId)
	}
	// Its allow-all egress rule, which EC2 adds untagged.
	if err := e.tagSGRules(ctx, rc.EC2, name, id); err != nil {
		return "", err
	}
	if id == "" {
		slog.Info("plan: would allow tcp/" + fmt.Sprint(dbPort) + " into " + e.names.DBID() + " from " + name)
		return "", nil
	}
	db, err := findSG(ctx, rc.EC2, t.vpc, e.names.DBID())
	if err != nil {
		return "", err
	}
	if db != nil && admits(db.IpPermissions, id) {
		slog.Info("RDS security group admits the rotation functions", "group", t.dbSG, "from", id)
		return id, nil
	}
	return id, e.act(fmt.Sprintf("allow tcp/%d into %s from %s", dbPort, e.names.DBID(), name), func() error {
		_, err := rc.EC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(t.dbSG),
			IpPermissions: []ec2types.IpPermission{{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int32(dbPort), ToPort: aws.Int32(dbPort),
				UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(id)}},
			}},
			TagSpecifications: e.ec2Tags(ec2types.ResourceTypeSecurityGroupRule),
		})
		return err
	})
}

// admits reports a tcp/5432 rule from group in perms.
func admits(perms []ec2types.IpPermission, group string) bool {
	for _, p := range perms {
		if aws.ToString(p.IpProtocol) != "tcp" || aws.ToInt32(p.FromPort) != dbPort || aws.ToInt32(p.ToPort) != dbPort {
			continue
		}
		for _, g := range p.UserIdGroupPairs {
			if aws.ToString(g.GroupId) == group {
				return true
			}
		}
	}
	return false
}

// connectionFields adds the fields the rotation function reads to a login
// secret when they are missing or stale. The secret value stays in memory:
// it is never logged, and an error names the secret, never its value.
func (e *env) connectionFields(ctx context.Context, sm rotSMAPI, who string, t rotationTarget) error {
	name := e.names.DBSecret(who)
	v, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
	if err != nil {
		return fmt.Errorf("read secret %s: %w", name, err)
	}
	merged, changed, err := withConnectionFields(aws.ToString(v.SecretString), t)
	if err != nil {
		return fmt.Errorf("secret %s: %w", name, err)
	}
	if !changed {
		slog.Info("secret has the rotation function's connection fields", "secret", name)
		return nil
	}
	return e.act("add the connection fields the rotation function reads to secret "+name, func() error {
		_, err := sm.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: aws.String(name), SecretString: aws.String(merged)})
		if err != nil {
			return fmt.Errorf("update secret %s: %w", name, err)
		}
		return nil
	})
}

// withConnectionFields returns the login JSON with engine, host, port, dbname
// and masterarn set, and whether that changed it. A value that is not a
// {username, password} login is an error that does not quote it.
func withConnectionFields(value string, t rotationTarget) (string, bool, error) {
	var d map[string]any
	if err := json.Unmarshal([]byte(value), &d); err != nil {
		return "", false, errors.New("its value is not a JSON login")
	}
	if u, _ := d["username"].(string); u == "" {
		return "", false, errors.New("its value has no username")
	}
	if p, _ := d["password"].(string); p == "" {
		return "", false, errors.New("its value has no password")
	}
	want := map[string]any{"engine": "postgres", "host": t.host, "port": float64(t.port), "dbname": dbName, "masterarn": t.masterARN}
	changed := false
	for k, v := range want {
		if d[k] != v {
			d[k], changed = v, true
		}
	}
	if !changed {
		return value, false, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// esoPolicy lets External Secrets read the two logins and nothing else.
func esoPolicy(t rotationTarget) string {
	b, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{{
		"Effect": "Allow", "Action": []string{"secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"},
		"Resource": []string{t.secretARN["owner"], t.secretARN["readwrite"]},
	}}})
	return string(b)
}

func (e *env) cfnTags() []cfntypes.Tag {
	out := make([]cfntypes.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		out = append(out, cfntypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	return out
}

// rotationStack creates the stack, or updates an existing one of this
// deployment (a no-op when nothing changed), and waits for it.
func (e *env) rotationStack(ctx context.Context, rc *rotClients, t rotationTarget, rotSG string) error {
	name := rotationStackName(e.names)
	s, err := describeStack(ctx, rc.CFN, name)
	if err != nil {
		return err
	}
	params := []cfntypes.Parameter{
		{ParameterKey: aws.String("OwnerSecretArn"), ParameterValue: aws.String(t.secretARN["owner"])},
		{ParameterKey: aws.String("ReadWriteSecretArn"), ParameterValue: aws.String(t.secretARN["readwrite"])},
		{ParameterKey: aws.String("MasterSecretArn"), ParameterValue: aws.String(t.masterARN)},
		{ParameterKey: aws.String("SubnetIds"), ParameterValue: aws.String(strings.Join(t.subnets, ","))},
		{ParameterKey: aws.String("SecurityGroupId"), ParameterValue: aws.String(rotSG)},
		{ParameterKey: aws.String("NamePrefix"), ParameterValue: aws.String(e.names.Cluster + "-db")},
	}
	// CAPABILITY_AUTO_EXPAND lets CreateStack run the transform itself;
	// CAPABILITY_IAM, the functions' roles it adds.
	caps := []cfntypes.Capability{cfntypes.CapabilityCapabilityIam, cfntypes.CapabilityCapabilityAutoExpand}
	if s == nil {
		return e.act("create CloudFormation stack "+name+" (rotation functions and schedules, a few minutes)", func() error {
			out, err := rc.CFN.CreateStack(ctx, &cloudformation.CreateStackInput{
				StackName: aws.String(name), TemplateBody: aws.String(rotationTemplate), Parameters: params,
				Capabilities: caps, Tags: e.cfnTags(),
				// A failed create deletes itself, so a rerun starts clean.
				OnFailure: cfntypes.OnFailureDelete,
			})
			if err != nil {
				return fmt.Errorf("create stack %s: %w", name, err)
			}
			return waitStack(ctx, rc.CFN, name, aws.ToString(out.StackId), cfntypes.StackStatusCreateComplete)
		})
	}
	if err := e.owned("CloudFormation stack "+name, cfnTagMap(s.Tags)); err != nil {
		return err
	}
	switch s.StackStatus {
	case cfntypes.StackStatusCreateComplete, cfntypes.StackStatusUpdateComplete, cfntypes.StackStatusUpdateRollbackComplete:
	default:
		return fmt.Errorf("CloudFormation stack %s is %s; delete it with `setup-aws plugin %s --delete` and run this again", name, s.StackStatus, dbRotationID)
	}
	return e.act("update CloudFormation stack "+name+" if it changed", func() error {
		_, err := rc.CFN.UpdateStack(ctx, &cloudformation.UpdateStackInput{
			StackName: aws.String(name), TemplateBody: aws.String(rotationTemplate), Parameters: params,
			Capabilities: caps, Tags: e.cfnTags(),
		})
		if noUpdates(err) {
			slog.Info("CloudFormation stack is up to date", "stack", name)
			return nil
		}
		if err != nil {
			return fmt.Errorf("update stack %s: %w", name, err)
		}
		return waitStack(ctx, rc.CFN, name, aws.ToString(s.StackId), cfntypes.StackStatusUpdateComplete)
	})
}

// describeStack returns the stack, or nil when it does not exist or is deleted.
func describeStack(ctx context.Context, c rotCFNAPI, name string) (*cfntypes.Stack, error) {
	out, err := c.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if stackGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe CloudFormation stack %s: %w", name, err)
	}
	for i := range out.Stacks {
		if out.Stacks[i].StackStatus != cfntypes.StackStatusDeleteComplete {
			return &out.Stacks[i], nil
		}
	}
	return nil, nil
}

// stackGone: CloudFormation answers a missing stack with a ValidationError
// that says so; any other error (a denied read among them) is not "gone".
func stackGone(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "ValidationError" && strings.Contains(ae.ErrorMessage(), "does not exist")
}

func noUpdates(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "ValidationError" && strings.Contains(ae.ErrorMessage(), "No updates are to be performed")
}

// waitStack waits until the stack (by ID, which also finds it once deleted)
// leaves its *_IN_PROGRESS state, and fails with the stack's own failure
// reasons unless it reached want.
func waitStack(ctx context.Context, c rotCFNAPI, name, id string, want cfntypes.StackStatus) error {
	slog.Info("waiting for CloudFormation stack", "stack", name, "want", want)
	var got cfntypes.StackStatus
	err := poll(ctx, "CloudFormation stack "+name, func(ctx context.Context) (bool, error) {
		out, err := c.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(id)})
		if err != nil {
			return false, fmt.Errorf("describe CloudFormation stack %s: %w", name, err)
		}
		if len(out.Stacks) == 0 {
			return false, fmt.Errorf("CloudFormation stack %s is gone", name)
		}
		got = out.Stacks[0].StackStatus
		return !strings.HasSuffix(string(got), "_IN_PROGRESS"), nil
	})
	if err != nil {
		return err
	}
	if got == want {
		slog.Info("CloudFormation stack ready", "stack", name, "status", got)
		return nil
	}
	return fmt.Errorf("CloudFormation stack %s ended %s, not %s: %s", name, got, want, stackFailures(ctx, c, id))
}

// stackFailures is the stack's failure reasons, oldest first.
func stackFailures(ctx context.Context, c rotCFNAPI, id string) string {
	out, err := c.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: aws.String(id)})
	if err != nil {
		return "its events could not be read: " + err.Error()
	}
	var rs []string
	for i := len(out.StackEvents) - 1; i >= 0; i-- {
		ev := out.StackEvents[i]
		r := aws.ToString(ev.ResourceStatusReason)
		if strings.HasSuffix(string(ev.ResourceStatus), "_FAILED") && r != "" && !strings.Contains(r, "Resource creation cancelled") {
			rs = append(rs, aws.ToString(ev.LogicalResourceId)+": "+r)
		}
	}
	if len(rs) == 0 {
		return "no failure reason recorded"
	}
	return strings.Join(rs[:min(len(rs), 3)], "; ")
}

func cfnTagMap(tags []cfntypes.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// ---- teardown and inventory -----------------------------------------------

// rotTD is the plugin's teardown and inventory context: tdEnv plus the
// plugin's clients, made on first use.
type rotTD struct {
	e  *tdEnv
	rc *rotClients
}

func (r *rotTD) clients(ctx context.Context) (*rotClients, error) {
	if r.rc != nil {
		return r.rc, nil
	}
	rc, err := rotationClientsFor(ctx, cfg)
	if err != nil {
		return nil, err
	}
	r.rc = rc
	return rc, nil
}

// dbRotationTeardown deletes, in order: the stack (functions, log groups,
// schedules; waiting for it), rotation left on by a stack that is already
// gone, the External Secrets association and role, then the RDS rule that
// names the rotation group and the group itself, which Lambda's network
// interfaces hold for some minutes after the functions are gone. It runs in
// teardown's plugin phase, before the RDS instance and its group go.
func dbRotationTeardown(e *tdEnv) []pluginStep {
	r := &rotTD{e: e}
	return []pluginStep{{Step: r.stepStack}, {Step: r.stepRotationOff}, {Step: r.stepAssociation},
		{Step: r.stepRole}, {Step: r.stepIngress}, {Step: r.stepSG}}
}

func (r *rotTD) stepStack(ctx context.Context) ([]tdTarget, []string, error) {
	rc, err := r.clients(ctx)
	if err != nil {
		return nil, nil, err
	}
	name := rotationStackName(r.e.n)
	s, err := describeStack(ctx, rc.CFN, name)
	if err != nil || s == nil {
		return nil, nil, err
	}
	if rf := r.e.refuse(kStack, name, cfnTagMap(s.Tags)); rf != "" {
		return nil, []string{rf}, nil
	}
	id := aws.ToString(s.StackId)
	t := tdTarget{kind: kStack, id: name, wait: func(ctx context.Context) error {
		err := waitStack(ctx, rc.CFN, name, id, cfntypes.StackStatusDeleteComplete)
		if stackGone(err) {
			return nil
		}
		return err
	}}
	if s.StackStatus != cfntypes.StackStatusDeleteInProgress {
		t.del = func(ctx context.Context) error {
			_, err := rc.CFN.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: aws.String(id)})
			return err
		}
	}
	return []tdTarget{t}, nil, nil
}

// stepRotationOff turns rotation off on a login secret that still has it,
// as clean.sh did: deleting the stack's schedules normally does it already.
func (r *rotTD) stepRotationOff(ctx context.Context) ([]tdTarget, []string, error) {
	rc, err := r.clients(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, name := range r.e.secretNames() {
		s, err := r.rotating(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if s == nil {
			continue
		}
		if rf := r.e.refuse("secret", name, kvMap(s.Tags, smKV)); rf != "" {
			rs = append(rs, rf)
			continue
		}
		n := name
		ts = append(ts, tdTarget{kind: kRotation, id: n, del: func(ctx context.Context) error {
			_, err := rc.SM.CancelRotateSecret(ctx, &secretsmanager.CancelRotateSecretInput{SecretId: aws.String(n)})
			return err
		}})
	}
	return ts, rs, nil
}

// rotating returns the login secret when rotation is on for it, else nil.
func (r *rotTD) rotating(ctx context.Context, name string) (*secretsmanager.DescribeSecretOutput, error) {
	rc, err := r.clients(ctx)
	if err != nil {
		return nil, err
	}
	s, err := rc.SM.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if tdGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe secret %s: %w", name, err)
	}
	if s.DeletedDate != nil || !aws.ToBool(s.RotationEnabled) {
		return nil, nil
	}
	return s, nil
}

func (r *rotTD) esoAssociations(ctx context.Context) ([]esoAssoc, error) {
	cl, err := r.e.cluster(ctx)
	if err != nil || cl == nil {
		return nil, err
	}
	as, err := r.e.associations(ctx)
	if err != nil {
		return nil, err
	}
	var out []esoAssoc
	for _, a := range as {
		if aws.ToString(a.Namespace) == esoNamespace && aws.ToString(a.ServiceAccount) == esoServiceAccount {
			out = append(out, esoAssoc{id: a.AssociationId, label: assocLabel(a), tags: a.Tags})
		}
	}
	return out, nil
}

type esoAssoc struct {
	id    *string
	label string
	tags  map[string]string
}

func (r *rotTD) stepAssociation(ctx context.Context) ([]tdTarget, []string, error) {
	as, err := r.esoAssociations(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, a := range as {
		if rf := r.e.refuse("Pod Identity association", a.label, a.tags); rf != "" {
			rs = append(rs, rf)
			continue
		}
		id := a.id
		ts = append(ts, tdTarget{kind: "Pod Identity association", id: a.label, del: func(ctx context.Context) error {
			_, err := r.e.c.EKS.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(r.e.n.Cluster), AssociationId: id})
			return err
		}})
	}
	return ts, rs, nil
}

func (r *rotTD) stepRole(ctx context.Context) ([]tdTarget, []string, error) {
	return r.e.roleTargets(ctx, []string{esoRoleName(r.e.n)}, true)
}

// rotationGroups are the security groups named <cluster>-db-rotation, in any
// VPC: the cluster, and so its VPC ID, may already be gone.
func (r *rotTD) rotationGroups(ctx context.Context) ([]ec2Item, error) {
	return r.e.ec2List(ctx, kSG, flt("group-name", r.e.n.RotationSGName()))
}

// ingressRules are the rules of the RDS security groups that name one of
// this deployment's rotation groups: rule ID -> the RDS group holding it.
func (r *rotTD) ingressRules(ctx context.Context) (map[string]string, map[string]map[string]string, error) {
	rc, err := r.clients(ctx)
	if err != nil {
		return nil, nil, err
	}
	rot, err := r.rotationGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	mine := map[string]bool{}
	for _, g := range rot {
		if r.e.owned(g.tags) {
			mine[g.id] = true
		}
	}
	if len(mine) == 0 {
		return nil, nil, nil
	}
	dbs, err := r.e.ec2List(ctx, kSG, flt("group-name", r.e.dbID()))
	if err != nil {
		return nil, nil, err
	}
	rules, tags := map[string]string{}, map[string]map[string]string{}
	for _, db := range dbs {
		out, err := rc.EC2.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
			Filters: []ec2types.Filter{flt("group-id", db.id)}})
		if err != nil {
			return nil, nil, fmt.Errorf("describe the rules of security group %s: %w", db.id, err)
		}
		for _, rule := range out.SecurityGroupRules {
			if aws.ToBool(rule.IsEgress) || rule.ReferencedGroupInfo == nil || !mine[aws.ToString(rule.ReferencedGroupInfo.GroupId)] {
				continue
			}
			id := aws.ToString(rule.SecurityGroupRuleId)
			rules[id], tags[id] = db.id, kvMap(rule.Tags, ec2KV)
		}
	}
	return rules, tags, nil
}

func (r *rotTD) stepIngress(ctx context.Context) ([]tdTarget, []string, error) {
	rc, err := r.clients(ctx)
	if err != nil {
		return nil, nil, err
	}
	rules, _, err := r.ingressRules(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	for _, id := range tdKeys(rules) {
		rule, group := id, rules[id]
		ts = append(ts, tdTarget{kind: kSGRule, id: rule + " (5432 into " + group + " from the rotation functions)", del: func(ctx context.Context) error {
			_, err := rc.EC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
				GroupId: aws.String(group), SecurityGroupRuleIds: []string{rule}})
			return err
		}})
	}
	return ts, nil, nil
}

func (r *rotTD) stepSG(ctx context.Context) ([]tdTarget, []string, error) {
	gs, err := r.rotationGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, g := range gs {
		label := g.name + " (" + g.id + ")"
		if rf := r.e.refuse(kSG, label, g.tags); rf != "" {
			rs = append(rs, rf)
			continue
		}
		// sgTarget retries while Lambda's network interfaces still hold it.
		ts = append(ts, r.e.sgTarget(g.id, label))
	}
	return ts, rs, nil
}

// dbRotationInventory lists what is left of the plugin, in the TYPE and ID
// forms the core inventory uses, so a resource both list is shown once.
func dbRotationInventory(ctx context.Context, e *tdEnv) ([]tdRow, error) {
	r := &rotTD{e: e}
	rc, err := r.clients(ctx)
	if err != nil {
		return nil, err
	}
	var rows []tdRow
	s, err := describeStack(ctx, rc.CFN, rotationStackName(e.n))
	if err != nil {
		return nil, err
	}
	if s != nil {
		// The tagging API's ID for a stack: name/uuid.
		_, _, id := arnType(aws.ToString(s.StackId))
		rows = append(rows, tdRow{Kind: kStack, ID: id, Tags: cfnTagMap(s.Tags)})
	}
	for _, name := range e.secretNames() {
		sec, err := r.rotating(ctx, name)
		if err != nil {
			return nil, err
		}
		if sec != nil {
			rows = append(rows, tdRow{Kind: kRotation, ID: name, Tags: kvMap(sec.Tags, smKV)})
		}
	}
	as, err := r.esoAssociations(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		rows = append(rows, tdRow{Kind: "Pod Identity association", ID: a.label, Tags: a.tags})
	}
	role := esoRoleName(e.n)
	g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	switch {
	case tdGone(err):
	case err != nil:
		return nil, fmt.Errorf("get role %s: %w", role, err)
	default:
		rows = append(rows, tdRow{Kind: "IAM role", ID: role, Tags: kvMap(g.Role.Tags, iamKV)})
	}
	rules, ruleTags, err := r.ingressRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range tdKeys(rules) {
		rows = append(rows, tdRow{Kind: kSGRule, ID: id, Tags: ruleTags[id]})
	}
	gs, err := r.rotationGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, sg := range gs {
		rows = append(rows, tdRow{Kind: kSG, ID: sg.name + " (" + sg.id + ")", Tags: sg.tags})
	}
	return rows, nil
}
