package cmd

// `setup-aws create rds`: the database, the counterpart of substrate-gke's
// `setup-gcp create cloudsql`. It makes a
// private Amazon RDS for PostgreSQL instance in the cluster's private
// subnets, a security group that admits only 5432 from the cluster security
// group, and the two Substrate logins in Secrets Manager. The schema
// bootstrap runs later, from inside the cluster (the installer's
// controlplane package), because the instance has no public endpoint.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

// rdsConfig holds the `create rds` settings, config.env's DB_* values.
type rdsConfig struct {
	EngineVersion, InstanceClass string
	StorageGB, BackupDays        int
	MultiAZ, DeletionProtection  bool
}

var rdsCfg rdsConfig

// Fixed names; the installer reads the same ones.
const (
	dbName         = "atepg"
	dbMasterUser   = "ateadmin"
	dbPort         = 5432
	dbCACert       = "rds-ca-rsa2048-g1"
	dbSubnetTagKey = "kubernetes.io/role/internal-elb"
)

// dbLogins are the Secrets Manager logins Substrate uses: owner runs DDL and
// migrations, readwrite runs DML.
var dbLogins = []string{"owner", "readwrite"}

// DBID is the RDS instance, DB subnet group and security group name.
func (n Names) DBID() string { return n.Cluster + "-pg" }

// RotationSGName is the security group the db-rotation plugin gives its
// rotation Lambdas. It may also reach 5432.
func (n Names) RotationSGName() string { return n.Cluster + "-db-rotation" }

// DBSecret is a login's secret name, <cluster>/ate-<who>.
func (n Names) DBSecret(who string) string { return n.Cluster + "/ate-" + who }

// The API surfaces `create rds` uses, beside the shared EKS client.
type (
	EC2API interface {
		DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
		DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
		CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
		AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
		DescribeSecurityGroupRules(context.Context, *ec2.DescribeSecurityGroupRulesInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error)
		// CreateTags and the reads of tagVPCInterfaces.
		eniAPI
	}
	RDSAPI interface {
		DescribeDBSubnetGroups(context.Context, *rds.DescribeDBSubnetGroupsInput, ...func(*rds.Options)) (*rds.DescribeDBSubnetGroupsOutput, error)
		CreateDBSubnetGroup(context.Context, *rds.CreateDBSubnetGroupInput, ...func(*rds.Options)) (*rds.CreateDBSubnetGroupOutput, error)
		ListTagsForResource(context.Context, *rds.ListTagsForResourceInput, ...func(*rds.Options)) (*rds.ListTagsForResourceOutput, error)
		DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
		CreateDBInstance(context.Context, *rds.CreateDBInstanceInput, ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error)
	}
	SecretsAPI interface {
		DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
		GetRandomPassword(context.Context, *secretsmanager.GetRandomPasswordInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetRandomPasswordOutput, error)
		CreateSecret(context.Context, *secretsmanager.CreateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	}
)

type rdsClients struct {
	EC2 EC2API
	RDS RDSAPI
	SM  SecretsAPI
}

// newRDSClients is replaced in tests.
var newRDSClients = func(ctx context.Context, c Config) (*rdsClients, error) {
	ac, err := loadAWSConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	return &rdsClients{EC2: ec2.NewFromConfig(ac), RDS: rds.NewFromConfig(ac), SM: secretsmanager.NewFromConfig(ac)}, nil
}

// Waits, replaced in tests.
var (
	secretRetryDelay   = 10 * time.Second
	secretRetries      = 12
	dbAvailableTimeout = 40 * time.Minute
)

var rdsCmd = &cobra.Command{
	Use:   "rds",
	Short: "Create the private RDS PostgreSQL instance and Substrate's logins in Secrets Manager",
	Long: `Creates a DB subnet group on the cluster's private subnets, a security group
that admits only 5432 from the cluster security group, an encrypted, private
RDS for PostgreSQL instance whose master password RDS manages, and the owner
and readwrite logins in Secrets Manager, then waits for the instance. A
security group rule other than 5432 from the cluster security group stops the
run; no rule is ever removed. Idempotent; --plan changes nothing.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		rc, err := newRDSClients(cmd.Context(), e.cfg)
		if err != nil {
			return err
		}
		return e.createRDS(cmd.Context(), rc)
	},
}

func init() {
	f := rdsCmd.Flags()
	f.StringVar(&rdsCfg.EngineVersion, "db-engine-version", getEnv("DB_ENGINE_VERSION", "18.3"), "PostgreSQL engine version (DB_ENGINE_VERSION)")
	f.StringVar(&rdsCfg.InstanceClass, "db-instance-class", getEnv("DB_INSTANCE_CLASS", "db.t4g.medium"), "RDS instance class (DB_INSTANCE_CLASS)")
	f.IntVar(&rdsCfg.StorageGB, "db-storage-gb", getEnv("DB_STORAGE_GB", 20), "allocated gp3 storage in GiB (DB_STORAGE_GB)")
	f.IntVar(&rdsCfg.BackupDays, "db-backup-days", getEnv("DB_BACKUP_DAYS", 1), "automated backup retention in days (DB_BACKUP_DAYS)")
	f.BoolVar(&rdsCfg.MultiAZ, "db-multi-az", getEnv("DB_MULTI_AZ", false), "Multi-AZ deployment (DB_MULTI_AZ); on for production")
	f.BoolVar(&rdsCfg.DeletionProtection, "db-deletion-protection", getEnv("DB_DELETION_PROTECTION", false), "deletion protection (DB_DELETION_PROTECTION); on for production, and teardown then cannot delete the instance")
	createCmd.AddCommand(rdsCmd)
}

// rdsNet is what the network phase finds for the later ones.
type rdsNet struct {
	vpc, clusterSG, dbSG string
	subnets              []string
}

func (e *env) createRDS(ctx context.Context, rc *rdsClients) error {
	if e.cfg.Plan {
		slog.Info("plan mode: reading only; nothing will be created or changed")
	}
	slog.Info("Starting RDS setup...", "cluster", e.names.Cluster, "region", e.names.Region, "instance", e.names.DBID())
	var net rdsNet
	steps := []func(context.Context) error{
		func(ctx context.Context) error { return e.rdsNetwork(ctx, rc, &net) },
		func(ctx context.Context) error { return e.rdsInstance(ctx, rc, &net) },
		func(ctx context.Context) error { return e.rdsSecrets(ctx, rc) },
		func(ctx context.Context) error { return e.rdsWait(ctx, rc) },
	}
	if len(steps) != len(phases.RDS) {
		return fmt.Errorf("internal: %d rds steps for %d phases", len(steps), len(phases.RDS))
	}
	for i, run := range steps {
		slog.Info(phases.Line(phases.RDS, i))
		if err := run(ctx); err != nil {
			return err
		}
	}
	// The instance's network interface, which RDS makes (eni_tags.go).
	if err := e.tagVPCInterfaces(ctx, rc.EC2, net.vpc); err != nil {
		return err
	}
	slog.Info("RDS setup completed successfully.")
	return nil
}

func (e *env) rdsNetwork(ctx context.Context, rc *rdsClients, net *rdsNet) error {
	// EKS attaches the cluster security group to every node, so a rule that
	// admits it admits the cluster's pods. The private subnets carry the
	// kubernetes.io/role/internal-elb tag.
	cl, err := e.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(e.names.Cluster)})
	if err != nil {
		return fmt.Errorf("describe cluster %s: %w", e.names.Cluster, err)
	}
	if vc := cl.Cluster.ResourcesVpcConfig; vc != nil {
		net.vpc, net.clusterSG = aws.ToString(vc.VpcId), aws.ToString(vc.ClusterSecurityGroupId)
	}
	if net.vpc == "" || net.clusterSG == "" {
		return fmt.Errorf("cluster %s has no VPC or cluster security group yet", e.names.Cluster)
	}
	sn, err := rc.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []ec2types.Filter{
		{Name: aws.String("vpc-id"), Values: []string{net.vpc}},
		{Name: aws.String("tag:" + dbSubnetTagKey), Values: []string{"1"}},
	}})
	if err != nil {
		return fmt.Errorf("describe subnets of %s: %w", net.vpc, err)
	}
	for _, s := range sn.Subnets {
		net.subnets = append(net.subnets, aws.ToString(s.SubnetId))
	}
	if len(net.subnets) < 2 {
		return fmt.Errorf("found %d private subnets (tag %s=1) in %s; RDS needs two", len(net.subnets), dbSubnetTagKey, net.vpc)
	}

	id := e.names.DBID()
	sg, err := rc.RDS.DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{DBSubnetGroupName: aws.String(id)})
	switch {
	case err == nil && len(sg.DBSubnetGroups) == 1:
		tags, err := rc.RDS.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: sg.DBSubnetGroups[0].DBSubnetGroupArn})
		if err != nil {
			return fmt.Errorf("read the tags of DB subnet group %s: %w", id, err)
		}
		if err := e.owned("DB subnet group "+id, rdsTagMap(tags.TagList)); err != nil {
			return err
		}
		slog.Info("DB subnet group exists", "name", id)
	case err != nil && !isRDSNotFound(err):
		return fmt.Errorf("describe DB subnet group %s: %w", id, err)
	default:
		if err := e.act("create DB subnet group "+id, func() error {
			_, err := rc.RDS.CreateDBSubnetGroup(ctx, &rds.CreateDBSubnetGroupInput{
				DBSubnetGroupName:        aws.String(id),
				DBSubnetGroupDescription: aws.String(e.names.Cluster + " private subnets"),
				SubnetIds:                net.subnets,
				Tags:                     e.rdsTags(),
			})
			return err
		}); err != nil {
			return fmt.Errorf("create DB subnet group %s: %w", id, err)
		}
	}

	// The security group: a stateful firewall on the instance's interface.
	group, err := findSG(ctx, rc.EC2, net.vpc, id)
	if err != nil {
		return err
	}
	if group == nil {
		if err := e.act("create security group "+id+" in "+net.vpc, func() error {
			out, err := rc.EC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
				GroupName:         aws.String(id),
				VpcId:             aws.String(net.vpc),
				Description:       aws.String(e.names.Cluster + " RDS: 5432 from the EKS cluster security group only"),
				TagSpecifications: e.ec2Tags(ec2types.ResourceTypeSecurityGroup),
			})
			if err == nil {
				net.dbSG = aws.ToString(out.GroupId)
			}
			return err
		}); err != nil {
			return fmt.Errorf("create security group %s: %w", id, err)
		}
	} else {
		if err := e.owned("security group "+id+" ("+aws.ToString(group.GroupId)+")", sgTagMap(group.Tags)); err != nil {
			return err
		}
		net.dbSG = aws.ToString(group.GroupId)
	}

	// Ingress: 5432 from the cluster security group, and from the rotation
	// Lambdas' group when the db-rotation plugin added one. Nothing else: any
	// other rule stops the run, which never removes a rule.
	rotation, err := findSG(ctx, rc.EC2, net.vpc, e.names.RotationSGName())
	if err != nil {
		return err
	}
	rotationID := ""
	if rotation != nil {
		rotationID = aws.ToString(rotation.GroupId)
	}
	var perms []ec2types.IpPermission
	if group != nil {
		perms = group.IpPermissions
	}
	state, err := classifyDBIngress(perms, net.clusterSG, rotationID)
	if err != nil {
		return fmt.Errorf("security group %s (%s): %w", id, net.dbSG, err)
	}
	if state == ingressOK {
		slog.Info("security group admits only 5432 from the cluster", "group", net.dbSG)
	} else if err := e.act(fmt.Sprintf("allow tcp/%d into %s from the cluster security group %s", dbPort, id, net.clusterSG), func() error {
		_, err := rc.EC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(net.dbSG),
			IpPermissions: []ec2types.IpPermission{{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int32(dbPort), ToPort: aws.Int32(dbPort),
				UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String(net.clusterSG)}},
			}},
			TagSpecifications: e.ec2Tags(ec2types.ResourceTypeSecurityGroupRule),
		})
		return err
	}); err != nil {
		return err
	}
	return e.tagSGRules(ctx, rc.EC2, id, net.dbSG)
}

// tagSGRules tags the group's rules that lack this deployment's tags: the
// allow-all egress rule EC2 adds to every new group by itself, and a rule an
// older install added untagged.
func (e *env) tagSGRules(ctx context.Context, c EC2API, name, group string) error {
	if group == "" {
		return nil // --plan, and the group is not made yet
	}
	out, err := c.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{group}}},
	})
	if err != nil {
		return fmt.Errorf("describe the rules of security group %s: %w", name, err)
	}
	var ids []string
	for _, r := range out.SecurityGroupRules {
		have := sgTagMap(r.Tags)
		for k, v := range e.tags {
			if have[k] != v {
				ids = append(ids, aws.ToString(r.SecurityGroupRuleId))
				break
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return e.act(fmt.Sprintf("tag %d rule(s) of security group %s", len(ids), name), func() error {
		_, err := c.CreateTags(ctx, &ec2.CreateTagsInput{Resources: ids, Tags: e.ec2Tags(ec2types.ResourceTypeSecurityGroupRule)[0].Tags})
		if err != nil {
			return fmt.Errorf("tag the rules of security group %s: %w", name, err)
		}
		return nil
	})
}

type ingressState int

const (
	ingressNone ingressState = iota // no rule from the cluster yet
	ingressOK                       // exactly what we want
)

// classifyDBIngress checks the database security group's ingress rules:
// every rule must be tcp 5432 from the cluster security group
// or the rotation group, with no address range or prefix list. It returns
// ingressOK when the cluster rule is there, ingressNone when it is missing,
// and an error naming the first other rule.
func classifyDBIngress(perms []ec2types.IpPermission, clusterSG, rotationSG string) (ingressState, error) {
	allowed := map[string]bool{clusterSG: true}
	if strings.HasPrefix(rotationSG, "sg-") {
		allowed[rotationSG] = true
	}
	fromCluster := false
	for _, p := range perms {
		bad := aws.ToString(p.IpProtocol) != "tcp" || aws.ToInt32(p.FromPort) != dbPort || aws.ToInt32(p.ToPort) != dbPort ||
			len(p.IpRanges) > 0 || len(p.Ipv6Ranges) > 0 || len(p.PrefixListIds) > 0 || len(p.UserIdGroupPairs) == 0
		for _, g := range p.UserIdGroupPairs {
			id := aws.ToString(g.GroupId)
			bad = bad || !allowed[id]
			fromCluster = fromCluster || id == clusterSG
		}
		if bad {
			return ingressNone, fmt.Errorf("has an ingress rule other than tcp/%d from %s (and the rotation group): %s; remove it yourself, this tool never removes a rule",
				dbPort, clusterSG, describePerm(p))
		}
	}
	if fromCluster {
		return ingressOK, nil
	}
	return ingressNone, nil
}

func describePerm(p ec2types.IpPermission) string {
	var src []string
	for _, g := range p.UserIdGroupPairs {
		src = append(src, aws.ToString(g.GroupId))
	}
	for _, r := range p.IpRanges {
		src = append(src, aws.ToString(r.CidrIp))
	}
	for _, r := range p.Ipv6Ranges {
		src = append(src, aws.ToString(r.CidrIpv6))
	}
	for _, r := range p.PrefixListIds {
		src = append(src, aws.ToString(r.PrefixListId))
	}
	return fmt.Sprintf("%s %d-%d from %s", aws.ToString(p.IpProtocol), aws.ToInt32(p.FromPort), aws.ToInt32(p.ToPort), strings.Join(src, ","))
}

// findSG returns the security group named name in vpc, or nil.
func findSG(ctx context.Context, c EC2API, vpc, name string) (*ec2types.SecurityGroup, error) {
	out, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []ec2types.Filter{
		{Name: aws.String("vpc-id"), Values: []string{vpc}},
		{Name: aws.String("group-name"), Values: []string{name}},
	}})
	if err != nil {
		return nil, fmt.Errorf("describe security group %s: %w", name, err)
	}
	if len(out.SecurityGroups) == 0 {
		return nil, nil
	}
	return &out.SecurityGroups[0], nil
}

func (e *env) rdsInstance(ctx context.Context, rc *rdsClients, net *rdsNet) error {
	id := e.names.DBID()
	db, err := describeDB(ctx, rc.RDS, id)
	if err != nil {
		return err
	}
	if db != nil {
		if err := e.owned("RDS instance "+id, rdsTagMap(db.TagList)); err != nil {
			return err
		}
		slog.Info("RDS instance exists", "instance", id, "status", aws.ToString(db.DBInstanceStatus))
		return nil
	}
	// ManageMasterUserPassword: RDS makes the admin password and keeps it in
	// Secrets Manager. CACertificateIdentifier picks the CA that signs the
	// server certificate clients verify with sslmode=verify-full.
	return e.act("create RDS instance "+id+" (about 10 minutes)", func() error {
		_, err := rc.RDS.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
			DBInstanceIdentifier:     aws.String(id),
			Engine:                   aws.String("postgres"),
			EngineVersion:            aws.String(rdsCfg.EngineVersion),
			DBInstanceClass:          aws.String(rdsCfg.InstanceClass),
			AllocatedStorage:         aws.Int32(int32(rdsCfg.StorageGB)),
			StorageType:              aws.String("gp3"),
			StorageEncrypted:         aws.Bool(true),
			DBName:                   aws.String(dbName),
			MasterUsername:           aws.String(dbMasterUser),
			ManageMasterUserPassword: aws.Bool(true),
			DBSubnetGroupName:        aws.String(id),
			VpcSecurityGroupIds:      []string{net.dbSG},
			MultiAZ:                  aws.Bool(rdsCfg.MultiAZ),
			DeletionProtection:       aws.Bool(rdsCfg.DeletionProtection),
			PubliclyAccessible:       aws.Bool(false),
			BackupRetentionPeriod:    aws.Int32(int32(rdsCfg.BackupDays)),
			CopyTagsToSnapshot:       aws.Bool(true),
			CACertificateIdentifier:  aws.String(dbCACert),
			Tags:                     e.rdsTags(),
		})
		if err != nil {
			return fmt.Errorf("create RDS instance %s: %w", id, err)
		}
		return nil
	})
}

func describeDB(ctx context.Context, c RDSAPI, id string) (*rdstypes.DBInstance, error) {
	out, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(id)})
	if isRDSNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe RDS instance %s: %w", id, err)
	}
	if len(out.DBInstances) == 0 {
		return nil, nil
	}
	return &out.DBInstances[0], nil
}

func (e *env) rdsSecrets(ctx context.Context, rc *rdsClients) error {
	for _, who := range dbLogins {
		name := e.names.DBSecret(who)
		s, err := rc.SM.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
		switch {
		case err == nil && s.DeletedDate == nil:
			if err := e.owned("secret "+name, smTagMap(s.Tags)); err != nil {
				return err
			}
			slog.Info("secret exists", "secret", name)
			continue
		case err != nil && !isNotFound(err):
			return fmt.Errorf("describe secret %s: %w", name, err)
		}
		// Absent, or scheduled for deletion: create it. A deleted secret can
		// hold its name for a while, which createSecret waits out.
		if err := e.act("create secret "+name+" with a generated password", func() error {
			return e.createSecret(ctx, rc.SM, name, who)
		}); err != nil {
			return fmt.Errorf("create secret %s: %w", name, err)
		}
	}
	return nil
}

// createSecret stores a generated login. The password goes from one
// Secrets Manager call to the next in memory: never a file, an argv or a log
// line.
func (e *env) createSecret(ctx context.Context, c SecretsAPI, name, who string) error {
	pw, err := c.GetRandomPassword(ctx, &secretsmanager.GetRandomPasswordInput{
		PasswordLength: aws.Int64(40), ExcludePunctuation: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("generate a password: %w", err)
	}
	value, err := json.Marshal(map[string]string{"username": "substrate_" + who + "_user", "password": aws.ToString(pw.RandomPassword)})
	if err != nil {
		return err
	}
	for i := 1; ; i++ {
		_, err = c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
			Name:         aws.String(name),
			Description:  aws.String("Agent Substrate " + who + " login for " + e.names.DBID()),
			SecretString: aws.String(string(value)),
			Tags:         e.smTags(),
		})
		if err == nil || !scheduledForDeletion(err) || i >= secretRetries {
			return err
		}
		slog.Info("secret is still being deleted; retrying", "secret", name, "attempt", i)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(secretRetryDelay):
		}
	}
}

func scheduledForDeletion(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "InvalidRequestException" && strings.Contains(ae.ErrorMessage(), "scheduled for deletion")
}

func (e *env) rdsWait(ctx context.Context, rc *rdsClients) error {
	id := e.names.DBID()
	if e.cfg.Plan {
		db, err := describeDB(ctx, rc.RDS, id)
		if err != nil {
			return err
		}
		status := "not created"
		if db != nil {
			status = aws.ToString(db.DBInstanceStatus)
		}
		slog.Info("plan: would wait for "+id+" to be available", "status", status)
		return nil
	}
	slog.Info("waiting for the RDS instance", "instance", id, "timeout", dbAvailableTimeout)
	out, err := rds.NewDBInstanceAvailableWaiter(rc.RDS).WaitForOutput(ctx,
		&rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(id)}, dbAvailableTimeout)
	if err != nil {
		return fmt.Errorf("wait for RDS instance %s: %w", id, err)
	}
	// RDS gives the master user secret it makes the instance's tags (seen
	// on a live instance), so it needs no tagging here.
	if db := out.DBInstances[0]; db.Endpoint != nil {
		slog.Info("RDS instance available", "instance", id, "endpoint", aws.ToString(db.Endpoint.Address))
	}
	return nil
}

// owned stops the run unless tags carry this deployment's owner tag, so a
// resource someone else made under the same name is never reused or changed.
func (e *env) owned(what string, tags map[string]string) error {
	if v := tags[OwnerTagKey]; v != e.names.OwnerValue {
		return fmt.Errorf("%s exists but is not owned by %s (%s=%q); it is left as it is", what, e.names.OwnerValue, OwnerTagKey, v)
	}
	return nil
}

// isRDSNotFound adds RDS's own not-found codes to isNotFound.
func isRDSNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "DBInstanceNotFound", "DBInstanceNotFoundFault", "DBSubnetGroupNotFoundFault":
			return true
		}
	}
	return isNotFound(err)
}

func (e *env) rdsTags() []rdstypes.Tag {
	out := make([]rdstypes.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		out = append(out, rdstypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	return out
}

func (e *env) smTags() []smtypes.Tag {
	out := make([]smtypes.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		out = append(out, smtypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	return out
}

func (e *env) ec2Tags(rt ec2types.ResourceType) []ec2types.TagSpecification {
	tags := make([]ec2types.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	return []ec2types.TagSpecification{{ResourceType: rt, Tags: tags}}
}

func rdsTagMap(tags []rdstypes.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func smTagMap(tags []smtypes.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func sgTagMap(tags []ec2types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
