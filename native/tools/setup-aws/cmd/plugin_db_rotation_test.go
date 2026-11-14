package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
)

// fakeCFN is the rotation stack. calls, when set, is the account's write
// log (tdFake.calls), so a teardown test sees one order across services.
type fakeCFN struct {
	stack   *cfntypes.Stack
	created *cloudformation.CreateStackInput
	fail    bool // the create rolls back
	calls   *[]string
}

func (f *fakeCFN) log(s string) {
	if f.calls != nil {
		*f.calls = append(*f.calls, s)
	}
}

func (f *fakeCFN) DescribeStacks(_ context.Context, in *cloudformation.DescribeStacksInput, _ ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error) {
	if f.stack == nil || (f.stack.StackStatus == cfntypes.StackStatusDeleteComplete && !strings.HasPrefix(aws.ToString(in.StackName), "arn:")) {
		return nil, &smithy.GenericAPIError{Code: "ValidationError", Message: "Stack with id " + aws.ToString(in.StackName) + " does not exist"}
	}
	s := *f.stack
	// Background work finishes on the next read.
	switch f.stack.StackStatus {
	case cfntypes.StackStatusCreateInProgress:
		f.stack.StackStatus = cfntypes.StackStatusCreateComplete
		if f.fail {
			f.stack.StackStatus = cfntypes.StackStatusDeleteComplete
		}
	case cfntypes.StackStatusDeleteInProgress:
		f.stack.StackStatus = cfntypes.StackStatusDeleteComplete
	}
	return &cloudformation.DescribeStacksOutput{Stacks: []cfntypes.Stack{s}}, nil
}
func (f *fakeCFN) DescribeStackEvents(context.Context, *cloudformation.DescribeStackEventsInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStackEventsOutput, error) {
	return &cloudformation.DescribeStackEventsOutput{StackEvents: []cfntypes.StackEvent{
		{LogicalResourceId: aws.String("OwnerRotation"), ResourceStatus: cfntypes.ResourceStatusCreateFailed, ResourceStatusReason: aws.String("Resource creation cancelled")},
		{LogicalResourceId: aws.String("OwnerLogs"), ResourceStatus: cfntypes.ResourceStatusCreateFailed, ResourceStatusReason: aws.String("log group already exists")},
	}}, nil
}
func (f *fakeCFN) CreateStack(_ context.Context, in *cloudformation.CreateStackInput, _ ...func(*cloudformation.Options)) (*cloudformation.CreateStackOutput, error) {
	f.created = in
	id := "arn:aws:cloudformation:us-west-2:111122223333:stack/" + aws.ToString(in.StackName) + "/uuid-1"
	f.stack = &cfntypes.Stack{StackName: in.StackName, StackId: aws.String(id), StackStatus: cfntypes.StackStatusCreateInProgress, Tags: in.Tags}
	f.log("CreateStack " + aws.ToString(in.StackName))
	return &cloudformation.CreateStackOutput{StackId: aws.String(id)}, nil
}
func (f *fakeCFN) UpdateStack(context.Context, *cloudformation.UpdateStackInput, ...func(*cloudformation.Options)) (*cloudformation.UpdateStackOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "ValidationError", Message: "No updates are to be performed."}
}
func (f *fakeCFN) DeleteStack(_ context.Context, in *cloudformation.DeleteStackInput, _ ...func(*cloudformation.Options)) (*cloudformation.DeleteStackOutput, error) {
	f.stack.StackStatus = cfntypes.StackStatusDeleteInProgress
	f.log("DeleteStack " + aws.ToString(f.stack.StackName))
	return &cloudformation.DeleteStackOutput{}, nil
}

// rotEC2 is create rds's fake EC2 plus the revoke teardown makes.
type rotEC2 struct{ *fec2 }

func (r rotEC2) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	r.writes = append(r.writes, "RevokeSecurityGroupIngress "+strings.Join(in.SecurityGroupRuleIds, ","))
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

// rotSM is create rds's fake Secrets Manager with values and rotation.
type rotSM struct {
	*fsm
	puts int
}

func (s *rotSM) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	out, err := s.fsm.DescribeSecret(ctx, in, o...)
	if err == nil {
		out.ARN = aws.String("arn:aws:secretsmanager:us-west-2:111122223333:secret:" + aws.ToString(in.SecretId) + "-AbCdEf")
	}
	return out, err
}
func (s *rotSM) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(s.values[aws.ToString(in.SecretId)])}, nil
}
func (s *rotSM) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	s.values[aws.ToString(in.SecretId)] = aws.ToString(in.SecretString)
	s.puts++
	s.writes = append(s.writes, "PutSecretValue "+aws.ToString(in.SecretId))
	return &secretsmanager.PutSecretValueOutput{}, nil
}
func (s *rotSM) CancelRotateSecret(context.Context, *secretsmanager.CancelRotateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CancelRotateSecretOutput, error) {
	return &secretsmanager.CancelRotateSecretOutput{}, nil
}

// rotationWorld is an account after create cluster, bootstrap and create
// rds for "demo", with the plugin's clients over it.
func rotationWorld(t *testing.T) (*env, *fakeDB, *rotClients, *fakeCFN) {
	t.Helper()
	e := rdsEnv(t, false)
	captureLogs(t)
	f := newFakeDB()
	if err := e.createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	cf := &fakeCFN{}
	rc := &rotClients{CFN: cf, EC2: rotEC2{(*fec2)(f)}, RDS: (*frds)(f), SM: &rotSM{fsm: (*fsm)(f)}}
	prevPoll := tdPoll
	tdPoll = 0
	t.Cleanup(func() { tdPoll = prevPoll })
	return e, f, rc, cf
}

func TestDBRotationCreatesOnceAndReruns(t *testing.T) {
	e, f, rc, cf := rotationWorld(t)
	f.writes = nil
	if err := e.createDBRotation(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"CreateSecurityGroup demo-db-rotation", "AuthorizeSecurityGroupIngress sg-demo-pg",
		"PutSecretValue demo/ate-owner", "PutSecretValue demo/ate-readwrite"} {
		if count(f.writes, w) != 1 {
			t.Errorf("%q made %d times; writes %v", w, count(f.writes, w), f.writes)
		}
	}
	// The rule into RDS names the rotation group, and is tagged (the fake
	// refuses an untagged one).
	if !admits(f.sgs["demo-pg"].IpPermissions, "sg-demo-db-rotation") {
		t.Error("RDS security group does not admit the rotation group")
	}
	if st, err := classifyDBIngress(f.sgs["demo-pg"].IpPermissions, fakeClusterSG, "sg-demo-db-rotation"); err != nil || st != ingressOK {
		t.Errorf("create rds would refuse the rule: %v %v", st, err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(f.values["demo/ate-owner"]), &v); err != nil {
		t.Fatal(err)
	}
	if v["engine"] != "postgres" || v["dbname"] != "atepg" || v["port"] != float64(5432) ||
		v["host"] != "demo-pg.abc.us-west-2.rds.amazonaws.com" || v["masterarn"] != "rds!db-demo-pg" || v["password"] != fakePassword {
		t.Errorf("owner login fields = %v", v)
	}
	in := cf.created
	if in == nil || aws.ToString(in.StackName) != "demo-db-rotation" || aws.ToString(in.TemplateBody) != rotationTemplate {
		t.Fatalf("stack not created from the embedded template: %+v", in)
	}
	if tags := cfnTagMap(in.Tags); tags[OwnerTagKey] != "demo.us-west-2" || tags["auto-delete"] != "no" {
		t.Errorf("stack tags %v", tags)
	}
	caps := map[cfntypes.Capability]bool{}
	for _, c := range in.Capabilities {
		caps[c] = true
	}
	if !caps[cfntypes.CapabilityCapabilityIam] || !caps[cfntypes.CapabilityCapabilityAutoExpand] {
		t.Errorf("capabilities %v", in.Capabilities)
	}
	params := map[string]string{}
	for _, p := range in.Parameters {
		params[aws.ToString(p.ParameterKey)] = aws.ToString(p.ParameterValue)
	}
	if params["SecurityGroupId"] != "sg-demo-db-rotation" || params["SubnetIds"] != "subnet-a,subnet-b" ||
		params["MasterSecretArn"] != "rds!db-demo-pg" || params["NamePrefix"] != "demo-db" ||
		!strings.Contains(params["OwnerSecretArn"], ":secret:demo/ate-owner-") {
		t.Errorf("parameters %v", params)
	}
	// The External Secrets role reads only the two logins.
	pol := e.c.IAM.(*fiam).policies["demo-us-west-2-external-secrets"]
	if !strings.Contains(pol, "demo/ate-owner-AbCdEf") || !strings.Contains(pol, "demo/ate-readwrite-AbCdEf") || strings.Contains(pol, `"*"`) {
		t.Errorf("policy %s", pol)
	}
	if e.c.IAM.(*fiam).assocs["external-secrets/external-secrets"] == "" {
		t.Error("no Pod Identity association for external-secrets/external-secrets")
	}

	// A rerun changes nothing.
	f.writes, cf.created = nil, nil
	roles := len(e.c.IAM.(*fiam).roles)
	if err := e.createDBRotation(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	for _, w := range f.writes {
		if !strings.HasPrefix(w, "CreateTags") {
			t.Errorf("rerun wrote %s", w)
		}
	}
	if cf.created != nil || len(e.c.IAM.(*fiam).roles) != roles {
		t.Error("rerun created the stack or a role again")
	}
}

func TestDBRotationRefusesWithoutRDS(t *testing.T) {
	e := rdsEnv(t, false)
	captureLogs(t)
	f := newFakeDB() // no create rds: the bundled-database install
	cf := &fakeCFN{}
	rc := &rotClients{CFN: cf, EC2: rotEC2{(*fec2)(f)}, RDS: (*frds)(f), SM: &rotSM{fsm: (*fsm)(f)}}
	err := e.createDBRotation(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "--database rds") || !strings.Contains(err.Error(), "no RDS instance demo-pg") {
		t.Fatalf("want the RDS refusal, got %v", err)
	}
	if len(f.writes) != 0 || cf.created != nil {
		t.Errorf("wrote %v", f.writes)
	}
}

func TestDBRotationRefusesForeignResources(t *testing.T) {
	e, f, rc, cf := rotationWorld(t)
	f.sgs["demo-db-rotation"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-x"), GroupName: aws.String("demo-db-rotation"),
		Tags: ec2Tags(map[string]string{OwnerTagKey: "someone.else"})}
	f.writes = nil
	err := e.createDBRotation(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "not owned by demo.us-west-2") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(f.writes) != 0 || cf.created != nil {
		t.Errorf("wrote %v after the refusal", f.writes)
	}

	delete(f.sgs, "demo-db-rotation")
	cf.stack = &cfntypes.Stack{StackName: aws.String("demo-db-rotation"), StackId: aws.String("arn:aws:cloudformation:us-west-2:111122223333:stack/demo-db-rotation/u"),
		StackStatus: cfntypes.StackStatusCreateComplete, Tags: []cfntypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String("someone.else")}}}
	if err := e.createDBRotation(context.Background(), rc); err == nil || !strings.Contains(err.Error(), "CloudFormation stack demo-db-rotation exists but is not owned") {
		t.Fatalf("want the stack refusal, got %v", err)
	}
}

func TestDBRotationFailedStackNamesTheReason(t *testing.T) {
	e, _, rc, cf := rotationWorld(t)
	cf.fail = true
	err := e.createDBRotation(context.Background(), rc)
	if err == nil || !strings.Contains(err.Error(), "OwnerLogs: log group already exists") || strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("want the failure reason, got %v", err)
	}
}

func TestDBRotationPlanWritesNothing(t *testing.T) {
	e, f, rc, cf := rotationWorld(t)
	e.cfg.Plan = true
	f.writes = nil
	if err := e.createDBRotation(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || cf.created != nil || len(e.c.IAM.(*fiam).roles) != 0 {
		t.Errorf("plan wrote %v", f.writes)
	}
}

// Both directions for the connection fields: missing ones are added and a
// complete login is left alone; a value that is not a login is refused
// without being quoted.
func TestWithConnectionFields(t *testing.T) {
	tg := rotationTarget{host: "h.example", port: 5432, masterARN: "arn:m"}
	out, changed, err := withConnectionFields(`{"username":"u","password":"p"}`, tg)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	again, changed, err := withConnectionFields(out, tg)
	if err != nil || changed || again != out {
		t.Errorf("second pass changed=%v err=%v", changed, err)
	}
	tg.host = "moved.example"
	if _, changed, _ := withConnectionFields(out, tg); !changed {
		t.Error("a stale host was kept")
	}
	_, _, err = withConnectionFields(`not-json-s3cr3t`, tg)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := withConnectionFields(`{"username":"u"}`, tg); err == nil {
		t.Error("a login with no password was accepted")
	}
}

// rotationTeardownWorld is fullDeployment plus what the plugin made, with
// the plugin's clients writing to the same call log.
func rotationTeardownWorld(t *testing.T, stackOwner string) (*tdEnv, *tdFake, *fakeCFN, *fakeRotTD) {
	t.Helper()
	f := fullDeployment()
	e, _, _ := tdTestEnv(t, f, false, false)
	f.put(&tdObj{kind: kSG, id: "sg-rot", name: "demo-db-rotation", vpc: "vpc-1", tags: own()})
	f.put(&tdObj{kind: "assoc", id: "a-eso", ns: "external-secrets", sa: "external-secrets", role: roleARN("demo-us-west-2-external-secrets"), tags: own()})
	f.put(&tdObj{kind: "role", id: "demo-us-west-2-external-secrets", inline: []string{"access"}, tags: own()})
	cf := &fakeCFN{calls: &f.calls, stack: &cfntypes.Stack{StackName: aws.String("demo-db-rotation"),
		StackId:     aws.String("arn:aws:cloudformation:us-west-2:111122223333:stack/demo-db-rotation/uuid-1"),
		StackStatus: cfntypes.StackStatusCreateComplete, Tags: []cfntypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(stackOwner)}}}}
	r := &fakeRotTD{calls: &f.calls, rotation: map[string]bool{"demo/ate-owner": true, "demo/ate-readwrite": false},
		rules: []ec2types.SecurityGroupRule{
			{SecurityGroupRuleId: aws.String("sgr-rot"), GroupId: aws.String("sg-pg"), IsEgress: aws.Bool(false),
				ReferencedGroupInfo: &ec2types.ReferencedSecurityGroup{GroupId: aws.String("sg-rot")}, Tags: tagsOf(own())},
			{SecurityGroupRuleId: aws.String("sgr-cluster"), GroupId: aws.String("sg-pg"), IsEgress: aws.Bool(false),
				ReferencedGroupInfo: &ec2types.ReferencedSecurityGroup{GroupId: aws.String("sg-eks")}, Tags: tagsOf(own())}}}
	prev := rotationClientsFor
	rotationClientsFor = func(context.Context, Config) (*rotClients, error) {
		return &rotClients{CFN: cf, EC2: r, SM: r}, nil
	}
	t.Cleanup(func() { rotationClientsFor = prev })
	return e, f, cf, r
}

// fakeRotTD is the EC2 rules and Secrets Manager rotation teardown reads.
type fakeRotTD struct {
	rotEC2API
	calls    *[]string
	rotation map[string]bool
	rules    []ec2types.SecurityGroupRule
}

func (r *fakeRotTD) DescribeSecurityGroupRules(_ context.Context, in *ec2.DescribeSecurityGroupRulesInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	out := &ec2.DescribeSecurityGroupRulesOutput{}
	for _, x := range r.rules {
		if aws.ToString(x.GroupId) == filterValue(in.Filters, "group-id") {
			out.SecurityGroupRules = append(out.SecurityGroupRules, x)
		}
	}
	return out, nil
}
func (r *fakeRotTD) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	*r.calls = append(*r.calls, "RevokeSecurityGroupIngress "+strings.Join(in.SecurityGroupRuleIds, ","))
	var keep []ec2types.SecurityGroupRule
	for _, x := range r.rules {
		if !strings.Contains(strings.Join(in.SecurityGroupRuleIds, ","), aws.ToString(x.SecurityGroupRuleId)) {
			keep = append(keep, x)
		}
	}
	r.rules = keep
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}
func (r *fakeRotTD) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	name := aws.ToString(in.SecretId)
	on, ok := r.rotation[name]
	if !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	return &secretsmanager.DescribeSecretOutput{Name: in.SecretId, RotationEnabled: aws.Bool(on),
		Tags: []smtypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}}, nil
}
func (r *fakeRotTD) CancelRotateSecret(_ context.Context, in *secretsmanager.CancelRotateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CancelRotateSecretOutput, error) {
	*r.calls = append(*r.calls, "CancelRotateSecret "+aws.ToString(in.SecretId))
	r.rotation[aws.ToString(in.SecretId)] = false
	return &secretsmanager.CancelRotateSecretOutput{}, nil
}
func (r *fakeRotTD) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	panic("teardown reads no secret value")
}
func (r *fakeRotTD) PutSecretValue(context.Context, *secretsmanager.PutSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	panic("teardown writes no secret value")
}

func indexOf(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// `setup-aws plugin db-rotation --delete` deletes everything the plugin
// made, in order, touches nothing of the core deployment, and its inventory
// is empty after.
func TestDBRotationDelete(t *testing.T) {
	e, f, _, _ := rotationTeardownWorld(t, tdOwner)
	rows, err := dbRotationInventory(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, r.Kind+" "+r.ID)
	}
	want := []string{"cloudformation stack demo-db-rotation/uuid-1", "secret rotation demo/ate-owner",
		"Pod Identity association external-secrets/external-secrets (a-eso)", "IAM role demo-us-west-2-external-secrets",
		"security group rule sgr-rot", "security group demo-db-rotation (sg-rot)"}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Errorf("inventory\n got %v\nwant %v", kinds, want)
	}

	d := pluginDef{ID: dbRotationID, Teardown: dbRotationTeardown, Inventory: dbRotationInventory}
	if err := e.deletePlugin(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	order := []string{"DeleteStack demo-db-rotation", "CancelRotateSecret demo/ate-owner", "DeletePodIdentityAssociation a-eso",
		"DeleteRole demo-us-west-2-external-secrets", "RevokeSecurityGroupIngress sgr-rot", "DeleteSecurityGroup sg-rot"}
	last := -1
	for _, o := range order {
		i := indexOf(f.calls, o)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in %v", o, f.calls)
		}
		last = i
	}
	for _, c := range f.calls {
		for _, core := range []string{"sg-pg", "demo-pg", "demo/ate-readwrite", "a-api", "DeleteSecret"} {
			if strings.Contains(c, core) {
				t.Errorf("deleting the plugin touched the core deployment: %s", c)
			}
		}
	}
	if f.get(kSG, "sg-pg") == nil || f.get("assoc", "a-api") == nil {
		t.Error("core resources are gone")
	}
}

// A same-named stack of another owner stops the delete before anything goes.
func TestDBRotationDeleteRefusesForeignStack(t *testing.T) {
	e, f, _, _ := rotationTeardownWorld(t, "someone.else")
	err := e.deletePlugin(context.Background(), pluginDef{ID: dbRotationID, Teardown: dbRotationTeardown, Inventory: dbRotationInventory})
	if err == nil || !strings.Contains(err.Error(), "cloudformation stack demo-db-rotation is not owned") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("deleted after a refusal: %v", f.calls)
	}
}

// In a full teardown the plugin's steps run first: the rotation group is
// gone before the RDS instance and its group, which its rule names.
func TestDBRotationTeardownRunsBeforeRDS(t *testing.T) {
	e, f, _, _ := rotationTeardownWorld(t, tdOwner)
	pluginDefs[dbRotationID] = pluginDef{ID: dbRotationID, Teardown: dbRotationTeardown, Inventory: dbRotationInventory}
	if err := e.teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	sg, db := indexOf(f.calls, "DeleteSecurityGroup sg-rot"), indexOf(f.calls, "DeleteDBInstance demo-pg")
	if sg < 0 || db < 0 || sg > db || indexOf(f.calls, "RevokeSecurityGroupIngress sgr-rot") > sg {
		t.Errorf("order: rotation group %d, RDS %d in %v", sg, db, f.calls)
	}
}
