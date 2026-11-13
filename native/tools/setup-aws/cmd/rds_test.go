package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
)

const (
	fakeVPC       = "vpc-0123"
	fakeClusterSG = "sg-cluster"
	fakePassword  = "Fake0Generated0Password0Never0Logged0xx"
)

// fakeDB is one in-memory account for `create rds`. writes counts every
// mutating call, as fakeAWS does for bootstrap.
type fakeDB struct {
	sgs          map[string]*ec2types.SecurityGroup // name -> group
	subnetGroups map[string]map[string]string       // name -> tags
	instances    map[string]*rdstypes.DBInstance
	created      *rds.CreateDBInstanceInput
	secrets      map[string]*secretsmanager.DescribeSecretOutput
	values       map[string]string
	// pendingDelete makes CreateSecret fail this many times with Secrets
	// Manager's "scheduled for deletion" error first.
	pendingDelete int
	createCalls   int
	deny          string // the read that returns AccessDenied
	writes        []string
}

func newFakeDB() *fakeDB {
	return &fakeDB{sgs: map[string]*ec2types.SecurityGroup{}, subnetGroups: map[string]map[string]string{},
		instances: map[string]*rdstypes.DBInstance{}, secrets: map[string]*secretsmanager.DescribeSecretOutput{},
		values: map[string]string{}}
}

func (f *fakeDB) denied(op string) error {
	if f.deny == op {
		return apiErr("AccessDenied")
	}
	return nil
}

func ownerTags() map[string]string {
	return map[string]string{OwnerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no"}
}

// rdsEKS reports the cluster's VPC and cluster security group.
type rdsEKS struct{ *feks }

func (r rdsEKS) DescribeCluster(ctx context.Context, in *eks.DescribeClusterInput, o ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	out, err := r.feks.DescribeCluster(ctx, in, o...)
	if err == nil {
		out.Cluster.ResourcesVpcConfig = &ekstypes.VpcConfigResponse{VpcId: aws.String(fakeVPC), ClusterSecurityGroupId: aws.String(fakeClusterSG)}
	}
	return out, err
}

type fec2 fakeDB

func filterValue(fs []ec2types.Filter, name string) string {
	for _, f := range fs {
		if aws.ToString(f.Name) == name {
			return f.Values[0]
		}
	}
	return ""
}

func (f *fec2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if filterValue(in.Filters, "vpc-id") != fakeVPC || filterValue(in.Filters, "tag:kubernetes.io/role/internal-elb") != "1" {
		return &ec2.DescribeSubnetsOutput{}, nil
	}
	return &ec2.DescribeSubnetsOutput{Subnets: []ec2types.Subnet{{SubnetId: aws.String("subnet-a")}, {SubnetId: aws.String("subnet-b")}}}, nil
}
func (f *fec2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if err := (*fakeDB)(f).denied("DescribeSecurityGroups"); err != nil {
		return nil, err
	}
	g, ok := f.sgs[filterValue(in.Filters, "group-name")]
	if !ok || filterValue(in.Filters, "vpc-id") != fakeVPC {
		return &ec2.DescribeSecurityGroupsOutput{}, nil
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{*g}}, nil
}
func (f *fec2) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	name := aws.ToString(in.GroupName)
	f.sgs[name] = &ec2types.SecurityGroup{GroupId: aws.String("sg-" + name), GroupName: in.GroupName, Tags: in.TagSpecifications[0].Tags}
	(*fakeDB)(f).writes = append(f.writes, "CreateSecurityGroup "+name)
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-" + name)}, nil
}
func (f *fec2) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	for _, g := range f.sgs {
		if aws.ToString(g.GroupId) == aws.ToString(in.GroupId) {
			g.IpPermissions = append(g.IpPermissions, in.IpPermissions...)
		}
	}
	if len(in.TagSpecifications) == 0 || sgTagMap(in.TagSpecifications[0].Tags)["auto-delete"] != "no" {
		return nil, fmt.Errorf("ingress rule without tags")
	}
	(*fakeDB)(f).writes = append(f.writes, "AuthorizeSecurityGroupIngress "+aws.ToString(in.GroupId))
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

type frds fakeDB

func (f *frds) DescribeDBSubnetGroups(_ context.Context, in *rds.DescribeDBSubnetGroupsInput, _ ...func(*rds.Options)) (*rds.DescribeDBSubnetGroupsOutput, error) {
	if err := (*fakeDB)(f).denied("DescribeDBSubnetGroups"); err != nil {
		return nil, err
	}
	name := aws.ToString(in.DBSubnetGroupName)
	if _, ok := f.subnetGroups[name]; !ok {
		return nil, &rdstypes.DBSubnetGroupNotFoundFault{Message: aws.String("not found")}
	}
	return &rds.DescribeDBSubnetGroupsOutput{DBSubnetGroups: []rdstypes.DBSubnetGroup{{DBSubnetGroupArn: aws.String("arn:subgrp:" + name)}}}, nil
}
func (f *frds) CreateDBSubnetGroup(_ context.Context, in *rds.CreateDBSubnetGroupInput, _ ...func(*rds.Options)) (*rds.CreateDBSubnetGroupOutput, error) {
	f.subnetGroups[aws.ToString(in.DBSubnetGroupName)] = rdsTagMap(in.Tags)
	(*fakeDB)(f).writes = append(f.writes, "CreateDBSubnetGroup "+aws.ToString(in.DBSubnetGroupName))
	return &rds.CreateDBSubnetGroupOutput{}, nil
}
func (f *frds) ListTagsForResource(_ context.Context, in *rds.ListTagsForResourceInput, _ ...func(*rds.Options)) (*rds.ListTagsForResourceOutput, error) {
	out := &rds.ListTagsForResourceOutput{}
	for k, v := range f.subnetGroups[strings.TrimPrefix(aws.ToString(in.ResourceName), "arn:subgrp:")] {
		out.TagList = append(out.TagList, rdstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}
func (f *frds) DescribeDBInstances(_ context.Context, in *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	db, ok := f.instances[aws.ToString(in.DBInstanceIdentifier)]
	if !ok {
		return nil, &rdstypes.DBInstanceNotFoundFault{Message: aws.String("not found")}
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{*db}}, nil
}
func (f *frds) CreateDBInstance(_ context.Context, in *rds.CreateDBInstanceInput, _ ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error) {
	id := aws.ToString(in.DBInstanceIdentifier)
	f.created = in
	f.instances[id] = &rdstypes.DBInstance{DBInstanceIdentifier: in.DBInstanceIdentifier, DBInstanceStatus: aws.String("available"),
		TagList: in.Tags, Endpoint: &rdstypes.Endpoint{Address: aws.String(id + ".abc.us-west-2.rds.amazonaws.com"), Port: aws.Int32(5432)},
		MasterUserSecret: &rdstypes.MasterUserSecret{SecretArn: aws.String("rds!db-" + id)}}
	f.secrets["rds!db-"+id] = &secretsmanager.DescribeSecretOutput{}
	(*fakeDB)(f).writes = append(f.writes, "CreateDBInstance "+id)
	return &rds.CreateDBInstanceOutput{}, nil
}

type fsm fakeDB

func (f *fsm) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	if err := (*fakeDB)(f).denied("DescribeSecret"); err != nil {
		return nil, err
	}
	s, ok := f.secrets[aws.ToString(in.SecretId)]
	if !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	return s, nil
}
func (f *fsm) GetRandomPassword(context.Context, *secretsmanager.GetRandomPasswordInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetRandomPasswordOutput, error) {
	return &secretsmanager.GetRandomPasswordOutput{RandomPassword: aws.String(fakePassword)}, nil
}
func (f *fsm) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	f.createCalls++
	if f.pendingDelete > 0 {
		f.pendingDelete--
		return nil, &smithy.GenericAPIError{Code: "InvalidRequestException",
			Message: "You can't create this secret because a secret with this name is already scheduled for deletion."}
	}
	name := aws.ToString(in.Name)
	f.secrets[name] = &secretsmanager.DescribeSecretOutput{Tags: in.Tags}
	f.values[name] = aws.ToString(in.SecretString)
	(*fakeDB)(f).writes = append(f.writes, "CreateSecret "+name)
	return &secretsmanager.CreateSecretOutput{}, nil
}
func (f *fsm) TagResource(_ context.Context, in *secretsmanager.TagResourceInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	f.secrets[aws.ToString(in.SecretId)].Tags = in.Tags
	(*fakeDB)(f).writes = append(f.writes, "TagResource "+aws.ToString(in.SecretId))
	return &secretsmanager.TagResourceOutput{}, nil
}

func (f *fakeDB) clients() *rdsClients {
	return &rdsClients{EC2: (*fec2)(f), RDS: (*frds)(f), SM: (*fsm)(f)}
}

// rdsEnv is testEnv with the cluster's VPC filled in, the config.env DB
// defaults, and no waiting between secret retries.
func rdsEnv(t *testing.T, plan bool) *env {
	t.Helper()
	e := testEnv(t, newFake(), plan)
	e.c.EKS = rdsEKS{e.c.EKS.(*feks)}
	rdsCfg = rdsConfig{EngineVersion: "18.3", InstanceClass: "db.t4g.medium", StorageGB: 20, BackupDays: 1}
	secretRetryDelay = 0
	return e
}

// captureLogs sends slog to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &b
}

func TestCreateRDSFreshAccountCreatesOnce(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeDB()
	if err := rdsEnv(t, false).createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]int{
		"CreateDBSubnetGroup demo-pg": 1, "CreateSecurityGroup demo-pg": 1, "AuthorizeSecurityGroupIngress sg-demo-pg": 1,
		"CreateDBInstance demo-pg": 1, "CreateSecret demo/ate-owner": 1, "CreateSecret demo/ate-readwrite": 1,
		"TagResource rds!db-demo-pg": 1,
	} {
		if got := count(f.writes, prefix); got != want {
			t.Errorf("%s: got %d, want %d (writes %v)", prefix, got, want, f.writes)
		}
	}
	// The instance is private, encrypted, verify-full capable, and RDS keeps the master password.
	in := f.created
	if aws.ToBool(in.PubliclyAccessible) || !aws.ToBool(in.StorageEncrypted) || !aws.ToBool(in.ManageMasterUserPassword) ||
		!aws.ToBool(in.CopyTagsToSnapshot) || aws.ToString(in.CACertificateIdentifier) != "rds-ca-rsa2048-g1" ||
		aws.ToString(in.EngineVersion) != "18.3" || aws.ToString(in.DBInstanceClass) != "db.t4g.medium" ||
		in.MasterUserPassword != nil || len(in.VpcSecurityGroupIds) != 1 || in.VpcSecurityGroupIds[0] != "sg-demo-pg" {
		t.Errorf("CreateDBInstance input is not the private, managed-password instance: %+v", in)
	}
	// Owned and tagged at create time (AC2.1).
	for what, tags := range map[string]map[string]string{
		"subnet group": f.subnetGroups["demo-pg"], "security group": sgTagMap(f.sgs["demo-pg"].Tags),
		"instance": rdsTagMap(in.Tags), "owner secret": smTagMap(f.secrets["demo/ate-owner"].Tags),
		"master secret": smTagMap(f.secrets["rds!db-demo-pg"].Tags),
	} {
		if tags[OwnerTagKey] != "demo.us-west-2" || tags["auto-delete"] != "no" {
			t.Errorf("%s tags = %v", what, tags)
		}
	}
	// Only 5432 from the cluster security group.
	perms := f.sgs["demo-pg"].IpPermissions
	if st, err := classifyDBIngress(perms, fakeClusterSG, ""); err != nil || st != ingressOK || len(perms) != 1 {
		t.Errorf("ingress after create: %v %v %v", perms, st, err)
	}
	if !strings.Contains(f.values["demo/ate-owner"], `"username":"substrate_owner_user"`) || !strings.Contains(f.values["demo/ate-owner"], fakePassword) {
		t.Errorf("owner secret value = %q", f.values["demo/ate-owner"])
	}
	// Every phase logs its line, and the generated password is never logged (AC3.2).
	for i := range 4 {
		if want := fmt.Sprintf("Step %d/4: ", i+1); !strings.Contains(logs.String(), want) {
			t.Errorf("no %q line", want)
		}
	}
	if strings.Contains(logs.String(), fakePassword) {
		t.Error("the generated password reached a log line")
	}
}

func TestCreateRDSRerunCreatesNothing(t *testing.T) {
	f := newFakeDB()
	if err := rdsEnv(t, false).createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	if err := rdsEnv(t, false).createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("second run wrote: %v", f.writes)
	}
}

func TestCreateRDSPlanWritesNothing(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeDB()
	if err := rdsEnv(t, true).createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("--plan wrote: %v", f.writes)
	}
	for _, want := range []string{"would create DB subnet group demo-pg", "would create security group demo-pg",
		"would allow tcp/5432 into demo-pg", "would create RDS instance demo-pg", "would create secret demo/ate-owner",
		"would create secret demo/ate-readwrite", "would wait for demo-pg"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("plan does not say %q", want)
		}
	}
}

func TestForeignIngressRuleStopsTheRun(t *testing.T) {
	owned := ownerTags()
	perm := func(proto string, from, to int32, mod func(*ec2types.IpPermission)) ec2types.IpPermission {
		p := ec2types.IpPermission{IpProtocol: aws.String(proto), FromPort: aws.Int32(from), ToPort: aws.Int32(to),
			UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(fakeClusterSG)}}}
		if mod != nil {
			mod(&p)
		}
		return p
	}
	for name, p := range map[string]ec2types.IpPermission{
		"cidr": perm("tcp", 5432, 5432, func(p *ec2types.IpPermission) { p.IpRanges = []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}} }),
		"ipv6": perm("tcp", 5432, 5432, func(p *ec2types.IpPermission) { p.Ipv6Ranges = []ec2types.Ipv6Range{{CidrIpv6: aws.String("::/0")}} }),
		"prefix list": perm("tcp", 5432, 5432, func(p *ec2types.IpPermission) {
			p.PrefixListIds = []ec2types.PrefixListId{{PrefixListId: aws.String("pl-1")}}
		}),
		"other port":  perm("tcp", 22, 22, nil),
		"all traffic": perm("-1", 0, 0, nil),
		"other group": perm("tcp", 5432, 5432, func(p *ec2types.IpPermission) { p.UserIdGroupPairs[0].GroupId = aws.String("sg-other") }),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDB()
			f.subnetGroups["demo-pg"] = owned
			f.sgs["demo-pg"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-demo-pg"), Tags: ec2Tags(owned), IpPermissions: []ec2types.IpPermission{p}}
			err := rdsEnv(t, false).createRDS(context.Background(), f.clients())
			if err == nil || !strings.Contains(err.Error(), "demo-pg") || !strings.Contains(err.Error(), "never removes") {
				t.Fatalf("want a stop naming the group, got %v", err)
			}
			if len(f.writes) != 0 {
				t.Errorf("wrote after a foreign rule: %v", f.writes)
			}
		})
	}
	// And the rules it accepts: the cluster group, plus the rotation group when it exists.
	f := newFakeDB()
	f.sgs["demo-db-rotation"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-rotation")}
	f.sgs["demo-pg"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-demo-pg"), Tags: ec2Tags(owned), IpPermissions: []ec2types.IpPermission{
		perm("tcp", 5432, 5432, func(p *ec2types.IpPermission) {
			p.UserIdGroupPairs = append(p.UserIdGroupPairs, ec2types.UserIdGroupPair{GroupId: aws.String("sg-rotation")})
		})}}
	if err := rdsEnv(t, false).createRDS(context.Background(), f.clients()); err != nil {
		t.Fatalf("the cluster and rotation rules were refused: %v", err)
	}
	if count(f.writes, "AuthorizeSecurityGroupIngress") != 0 {
		t.Errorf("re-added a rule that was there: %v", f.writes)
	}
}

func ec2Tags(m map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for k, v := range m {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func TestForeignResourcesAreNotReused(t *testing.T) {
	foreign := map[string]string{OwnerTagKey: "someone-else"}
	for name, setup := range map[string]func(*fakeDB){
		"security group": func(f *fakeDB) {
			f.sgs["demo-pg"] = &ec2types.SecurityGroup{GroupId: aws.String("sg-x"), Tags: ec2Tags(foreign)}
		},
		"subnet group": func(f *fakeDB) { f.subnetGroups["demo-pg"] = foreign },
		"secret": func(f *fakeDB) {
			f.secrets["demo/ate-owner"] = &secretsmanager.DescribeSecretOutput{Tags: []smtypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String("x")}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDB()
			setup(f)
			err := rdsEnv(t, false).createRDS(context.Background(), f.clients())
			if err == nil || !strings.Contains(err.Error(), "not owned by demo.us-west-2") {
				t.Fatalf("want a not-owned stop, got %v", err)
			}
			for _, w := range f.writes {
				if strings.Contains(w, "sg-x") || strings.HasSuffix(w, "demo/ate-owner") {
					t.Errorf("changed a resource it does not own: %s", w)
				}
			}
		})
	}
}

func TestSecretScheduledForDeletionIsRetried(t *testing.T) {
	f := newFakeDB()
	f.pendingDelete = 2
	if err := rdsEnv(t, false).createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	if f.createCalls != 4 || count(f.writes, "CreateSecret ") != 2 {
		t.Errorf("CreateSecret calls = %d, created %v; want 2 refusals then 2 creates", f.createCalls, f.writes)
	}
	// It gives up after secretRetries, with Secrets Manager's own message.
	f = newFakeDB()
	f.pendingDelete = 100
	err := rdsEnv(t, false).createRDS(context.Background(), f.clients())
	if err == nil || !strings.Contains(err.Error(), "scheduled for deletion") || f.createCalls != secretRetries {
		t.Errorf("want a stop after %d tries, got %d tries and %v", secretRetries, f.createCalls, err)
	}
}

func TestRDSAccessDeniedStops(t *testing.T) {
	for _, op := range []string{"DescribeDBSubnetGroups", "DescribeSecurityGroups", "DescribeSecret"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeDB()
			f.deny = op
			err := rdsEnv(t, false).createRDS(context.Background(), f.clients())
			if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
				t.Fatalf("want AccessDenied, got %v", err)
			}
			for _, w := range f.writes {
				if (op == "DescribeDBSubnetGroups" && strings.HasPrefix(w, "CreateDBSubnetGroup")) ||
					(op == "DescribeSecurityGroups" && strings.HasPrefix(w, "CreateSecurityGroup")) ||
					(op == "DescribeSecret" && strings.HasPrefix(w, "CreateSecret")) {
					t.Errorf("a denied read was taken for not found: %s", w)
				}
			}
		})
	}
}

// The waiter must not hide a failed instance as a timeout.
func TestWaitFailsOnAFailedInstance(t *testing.T) {
	f := newFakeDB()
	e := rdsEnv(t, false)
	if err := e.createRDS(context.Background(), f.clients()); err != nil {
		t.Fatal(err)
	}
	f.instances["demo-pg"].DBInstanceStatus = aws.String("failed")
	dbAvailableTimeout = time.Second
	t.Cleanup(func() { dbAvailableTimeout = 40 * time.Minute })
	if err := e.rdsWait(context.Background(), f.clients()); err == nil {
		t.Error("waited past a failed instance")
	}
}
