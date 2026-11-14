package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/spf13/cobra"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

var teardownKeepCluster bool

var teardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Delete everything this deployment created, in reverse order",
	Long: `Deletes, in reverse dependency order, what bootstrap, create rds and create
cluster made: the Pod Identity associations and roles, the RDS instance, its
subnet and security groups and the database logins, both S3 buckets with every
object version, the ECR repositories, then (unless --keep-cluster) the node
groups, the cluster and its IAM roles, the launch template and the VPC.

Only resources with this deployment's owner tag are deleted. A resource with
one of its names but another owner, or one that belongs to a CloudFormation
stack (eksctl, create-cluster.sh), stops the run before anything is deleted.
Run the installer's control-plane removal first, so PVC-backed volumes are
released. It ends with the inventory and fails if anything is left. Rerunning
it resumes; --plan deletes nothing.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newTDEnv(cmd.Context(), cmd.OutOrStdout())
		if err != nil {
			return err
		}
		e.keepCluster = teardownKeepCluster
		if err := e.teardown(cmd.Context()); err != nil {
			return errors.New(e.mask(err.Error()))
		}
		return nil
	},
}

func init() {
	teardownCmd.Flags().BoolVar(&teardownKeepCluster, "keep-cluster", getEnv("KEEP_CLUSTER", false),
		"remove Substrate and its AWS data, and leave the cluster, its node groups, roles and network (KEEP_CLUSTER)")
	rootCmd.AddCommand(teardownCmd, inventoryCmd)
}

// Polling of the deletes that finish in the background (node groups, the
// cluster, RDS, NAT gateways, secrets) and of the retries on a dependency
// AWS releases late. Replaced in tests.
var (
	tdPoll    = 15 * time.Second
	tdTimeout = 40 * time.Minute
)

// tdTarget is one delete. A nil del means AWS is already deleting it; wait,
// when set, returns once it is gone.
type tdTarget struct {
	kind, id string
	del      func(context.Context) error
	wait     func(context.Context) error
}

// A tdStep reads one kind of resource right before it is deleted and
// returns what to delete, and what it found but may not delete.
type tdStep func(context.Context) (targets []tdTarget, refusals []string, err error)

type tdPhase struct {
	cluster bool // kept by --keep-cluster
	steps   []tdStep
}

// teardownPhases are phases.Teardown[1:9], in order. Within a phase the
// steps run in order, each after the one before is gone. Plugins go first:
// their resources (an EFS file system's mount targets, a rotation stack's
// security group) hold the VPC and the RDS security group.
func (e *tdEnv) teardownPhases() []tdPhase {
	return []tdPhase{
		e.pluginPhase(),
		{steps: []tdStep{e.stepAssociations, e.stepSubstrateRoles}},
		{steps: []tdStep{e.stepDBInstance, e.stepDBNetwork, e.stepSecrets}},
		{steps: []tdStep{e.stepBuckets}},
		{steps: []tdStep{e.stepRepositories}},
		{cluster: true, steps: []tdStep{e.stepNodegroups}},
		{cluster: true, steps: []tdStep{e.stepCluster, e.stepClusterRoles}},
		// An available network interface holds its subnet and security
		// groups, so it goes before both; the VPC goes last.
		{cluster: true, steps: []tdStep{e.stepLaunchTemplates, e.stepVolumes, e.stepNATs, e.stepEIPs,
			e.stepIGWs, e.stepENIs, e.stepVPCSecurityGroups, e.stepSubnets, e.stepRouteTables, e.stepVPCs}},
	}
}

func (e *tdEnv) say(format string, a ...any) { slog.Info(e.mask(fmt.Sprintf(format, a...))) }

func refusalError(r []string) error {
	return fmt.Errorf("teardown stopped before deleting anything it does not own:\n  %s", strings.Join(r, "\n  "))
}

func (e *tdEnv) teardown(ctx context.Context) error {
	ps := e.teardownPhases()
	if len(ps)+2 != len(phases.Teardown) {
		return fmt.Errorf("internal: %d teardown phases for %d phase lines", len(ps)+2, len(phases.Teardown))
	}
	if e.plan {
		slog.Info("plan mode: reading only; nothing will be deleted")
	}
	e.say("Starting teardown of %s...", e.n.OwnerValue)

	slog.Info(phases.Line(phases.Teardown, 0))
	rows, err := e.inventory(ctx)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		e.printRows(rows)
	}
	// Every refusal is found before the first delete, so a resource this
	// deployment does not own never leaves a half-deleted deployment behind.
	var refusals []string
	for _, p := range ps {
		if p.cluster && e.keepCluster {
			continue
		}
		for _, s := range p.steps {
			_, r, err := s(ctx)
			if err != nil {
				return err
			}
			refusals = append(refusals, r...)
		}
	}
	if len(refusals) > 0 {
		return refusalError(refusals)
	}

	for i, p := range ps {
		slog.Info(phases.Line(phases.Teardown, i+1))
		if p.cluster && e.keepCluster {
			e.say("kept (--keep-cluster)")
			continue
		}
		n := 0
		for _, s := range p.steps {
			ts, r, err := s(ctx)
			if err != nil {
				return err
			}
			if len(r) > 0 {
				return refusalError(r)
			}
			if err := e.run(ctx, ts); err != nil {
				return err
			}
			n += len(ts)
		}
		if n == 0 {
			e.say("nothing to delete")
		}
	}

	slog.Info(phases.Line(phases.Teardown, len(phases.Teardown)-1))
	if e.plan {
		e.say("plan: would check that nothing of %s is left", e.n.OwnerValue)
		return nil
	}
	rows, err = e.inventory(ctx)
	if err != nil {
		return err
	}
	var left []tdRow
	for _, r := range rows {
		if !e.keepCluster || !r.Cluster {
			left = append(left, r)
		}
	}
	if len(left) > 0 {
		e.printRows(left)
		return fmt.Errorf("%d resources of %s are left (listed above); teardown does not delete these kinds, or AWS refused: remove them and run it again", len(left), e.n.OwnerValue)
	}
	if e.keepCluster {
		e.say("Teardown complete: nothing of Substrate's AWS data is left; the cluster %s is kept", e.n.Cluster)
	} else {
		e.say("Teardown complete: nothing left for %s", e.n.OwnerValue)
	}
	return nil
}

// run deletes the targets of one step, then waits for each to be gone. A
// target already gone when its delete runs is not an error.
func (e *tdEnv) run(ctx context.Context, ts []tdTarget) error {
	for _, t := range ts {
		what := t.kind + " " + t.id
		if t.del == nil {
			e.say("%s is already being deleted", what)
			continue
		}
		if e.plan {
			e.say("plan: would delete %s", what)
			continue
		}
		e.say("delete %s", what)
		if err := t.del(ctx); tdGone(err) {
			e.say("%s is already gone", what)
		} else if err != nil {
			return fmt.Errorf("delete %s: %w", what, err)
		}
	}
	if e.plan {
		return nil
	}
	for _, t := range ts {
		if t.wait == nil {
			continue
		}
		e.say("waiting for %s %s to be deleted", t.kind, t.id)
		if err := t.wait(ctx); err != nil {
			return fmt.Errorf("wait for %s %s: %w", t.kind, t.id, err)
		}
	}
	return nil
}

// poll calls done every tdPoll until it reports true, fails, or tdTimeout passes.
func poll(ctx context.Context, what string, done func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(tdTimeout)
	for {
		ok, err := done(ctx)
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s waiting for %s", tdTimeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tdPoll):
		}
	}
}

// retryOn retries fn while it fails with one of codes: a dependency AWS
// releases in the background (an ENI of a deleted cluster holds its subnet).
func retryOn(ctx context.Context, what string, fn func() error, codes ...string) error {
	var last error
	err := poll(ctx, what, func(context.Context) (bool, error) {
		last = fn()
		if last != nil && hasCode(last, codes...) {
			return false, nil
		}
		return true, last
	})
	if err != nil && last != nil && err != last {
		return fmt.Errorf("%w: %v", err, last)
	}
	return err
}

// gone waits until exists reports false.
func gone(exists func(context.Context) (bool, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		return poll(ctx, "the delete", func(ctx context.Context) (bool, error) {
			ok, err := exists(ctx)
			return !ok, err
		})
	}
}

// ---- phase 1: Pod Identity associations and roles --------------------------

// stepAssociations deletes the ate-system associations. Those of other
// namespaces (the EBS CSI driver's) belong to the cluster and go with it.
func (e *tdEnv) stepAssociations(ctx context.Context) ([]tdTarget, []string, error) {
	cl, err := e.cluster(ctx)
	if err != nil || cl == nil {
		return nil, nil, err
	}
	as, err := e.associations(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, a := range as {
		if aws.ToString(a.Namespace) != "ate-system" {
			continue
		}
		label := assocLabel(a)
		if r := e.refuse("Pod Identity association", label, a.Tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := a.AssociationId
		ts = append(ts, tdTarget{kind: "Pod Identity association", id: label, del: func(ctx context.Context) error {
			_, err := e.c.EKS.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(e.n.Cluster), AssociationId: id})
			return err
		}})
	}
	return ts, rs, nil
}

func (e *tdEnv) stepSubstrateRoles(ctx context.Context) ([]tdTarget, []string, error) {
	return e.roleTargets(ctx, e.substrateRoles(), true)
}

// roleTargets turns role names into deletes. strict: a role found without
// this deployment's owner tag is a refusal; otherwise it is left alone (a
// role the cluster points at but someone else made).
func (e *tdEnv) roleTargets(ctx context.Context, names []string, strict bool) ([]tdTarget, []string, error) {
	var ts []tdTarget
	var rs []string
	for _, n := range names {
		g, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(n)})
		if tdGone(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("get role %s: %w", n, err)
		}
		tags := kvMap(g.Role.Tags, iamKV)
		if !strict && !e.owned(tags) {
			continue
		}
		if r := e.refuse("IAM role", n, tags); r != "" {
			rs = append(rs, r)
			continue
		}
		name := n
		ts = append(ts, tdTarget{kind: "IAM role", id: name, del: func(ctx context.Context) error { return e.deleteRole(ctx, name) }})
	}
	return ts, rs, nil
}

// deleteRole removes what IAM requires gone first (inline policies,
// attached policies, instance profile membership), then the role.
func (e *tdEnv) deleteRole(ctx context.Context, role string) error {
	r := aws.String(role)
	pp := iam.NewListRolePoliciesPaginator(e.c.IAM, &iam.ListRolePoliciesInput{RoleName: r})
	for pp.HasMorePages() {
		page, err := pp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, p := range page.PolicyNames {
			if _, err := e.c.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: r, PolicyName: aws.String(p)}); err != nil && !tdGone(err) {
				return err
			}
		}
	}
	ap := iam.NewListAttachedRolePoliciesPaginator(e.c.IAM, &iam.ListAttachedRolePoliciesInput{RoleName: r})
	for ap.HasMorePages() {
		page, err := ap.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, p := range page.AttachedPolicies {
			if _, err := e.c.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: r, PolicyArn: p.PolicyArn}); err != nil && !tdGone(err) {
				return err
			}
		}
	}
	ip := iam.NewListInstanceProfilesForRolePaginator(e.c.IAM, &iam.ListInstanceProfilesForRoleInput{RoleName: r})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, p := range page.InstanceProfiles {
			if _, err := e.c.IAM.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{RoleName: r, InstanceProfileName: p.InstanceProfileName}); err != nil && !tdGone(err) {
				return err
			}
		}
	}
	_, err := e.c.IAM.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: r})
	return err
}

// ---- phase 2: RDS and its logins ---------------------------------------------

func (e *tdEnv) stepDBInstance(ctx context.Context) ([]tdTarget, []string, error) {
	db, err := e.dbInstance(ctx)
	if err != nil || db == nil {
		return nil, nil, err
	}
	id := e.dbID()
	if r := e.refuse("RDS instance", id, kvMap(db.TagList, rdsKV)); r != "" {
		return nil, []string{r}, nil
	}
	if aws.ToBool(db.DeletionProtection) {
		return nil, []string{fmt.Sprintf("RDS instance %s has deletion protection on; teardown never turns it off: turn it off yourself if you mean to delete the database", id)}, nil
	}
	t := tdTarget{kind: "RDS instance", id: id + " (no final snapshot)", wait: gone(func(ctx context.Context) (bool, error) {
		db, err := e.dbInstance(ctx)
		return db != nil, err
	})}
	if aws.ToString(db.DBInstanceStatus) != "deleting" {
		t.del = func(ctx context.Context) error {
			// A busy instance (backing up, modifying) refuses the delete for a while.
			return retryOn(ctx, "RDS instance "+id+" to accept the delete", func() error {
				_, err := e.c.RDS.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{DBInstanceIdentifier: aws.String(id),
					SkipFinalSnapshot: aws.Bool(true), DeleteAutomatedBackups: aws.Bool(true)})
				return err
			}, "InvalidDBInstanceState", "InvalidDBInstanceStateFault")
		}
	}
	return []tdTarget{t}, nil, nil
}

// stepDBNetwork deletes the subnet group and the security group, which stay
// in use until the instance is gone (the step before waited for that).
func (e *tdEnv) stepDBNetwork(ctx context.Context) ([]tdTarget, []string, error) {
	var ts []tdTarget
	var rs []string
	id := e.dbID()
	ok, tags, err := e.dbSubnetGroup(ctx)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		if r := e.refuse("RDS subnet group", id, tags); r != "" {
			rs = append(rs, r)
		} else {
			ts = append(ts, tdTarget{kind: "RDS subnet group", id: id, del: func(ctx context.Context) error {
				_, err := e.c.RDS.DeleteDBSubnetGroup(ctx, &rds.DeleteDBSubnetGroupInput{DBSubnetGroupName: aws.String(id)})
				return err
			}})
		}
	}
	sgs, err := e.ec2List(ctx, kSG, flt("group-name", id))
	if err != nil {
		return nil, nil, err
	}
	for _, sg := range sgs {
		label := sg.name + " (" + sg.id + ")"
		if r := e.refuse(kSG, label, sg.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		ts = append(ts, e.sgTarget(sg.id, label))
	}
	return ts, rs, nil
}

func (e *tdEnv) sgTarget(id, label string) tdTarget {
	return tdTarget{kind: kSG, id: label, del: func(ctx context.Context) error {
		return retryOn(ctx, "security group "+id+" to be free", func() error {
			_, err := e.c.EC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
			return err
		}, "DependencyViolation")
	}}
}

// stepSecrets deletes the logins with no recovery window, so a new install
// can create them again at once, and waits for the names to be free.
func (e *tdEnv) stepSecrets(ctx context.Context) ([]tdTarget, []string, error) {
	var ts []tdTarget
	var rs []string
	for _, name := range e.secretNames() {
		s, err := e.secret(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if s == nil {
			continue
		}
		if r := e.refuse("secret", name, kvMap(s.Tags, smKV)); r != "" {
			rs = append(rs, r)
			continue
		}
		n := name
		t := tdTarget{kind: "secret", id: n, wait: gone(func(ctx context.Context) (bool, error) {
			s, err := e.secret(ctx, n)
			return s != nil, err
		})}
		if s.DeletedDate == nil {
			t.del = func(ctx context.Context) error {
				_, err := e.c.SM.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: aws.String(n), ForceDeleteWithoutRecovery: aws.Bool(true)})
				return err
			}
		}
		ts = append(ts, t)
	}
	return ts, rs, nil
}

// ---- phase 3 and 4: S3 and ECR ------------------------------------------------

func (e *tdEnv) stepBuckets(ctx context.Context) ([]tdTarget, []string, error) {
	var ts []tdTarget
	var rs []string
	for _, b := range []string{e.n.SnapBucket, e.n.AssetBucket} {
		ok, tags, err := e.bucketTags(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		if r := e.refuse("S3 bucket", b, tags); r != "" {
			rs = append(rs, r)
			continue
		}
		name := b
		ts = append(ts, tdTarget{kind: "S3 bucket", id: name + " (every object version)", del: func(ctx context.Context) error { return e.deleteBucket(ctx, name) }})
	}
	return ts, rs, nil
}

// deleteBucket deletes every object version and delete marker, then the bucket.
func (e *tdEnv) deleteBucket(ctx context.Context, b string) error {
	owner := aws.String(e.n.Account)
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(b), ExpectedBucketOwner: owner}
	for {
		out, err := e.c.S3.ListObjectVersions(ctx, in)
		if err != nil {
			return err
		}
		var ids []s3types.ObjectIdentifier
		for _, v := range out.Versions {
			ids = append(ids, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, d := range out.DeleteMarkers {
			ids = append(ids, s3types.ObjectIdentifier{Key: d.Key, VersionId: d.VersionId})
		}
		if len(ids) > 0 {
			res, err := e.c.S3.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(b), ExpectedBucketOwner: owner,
				Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
			if err != nil {
				return err
			}
			if len(res.Errors) > 0 {
				x := res.Errors[0]
				return fmt.Errorf("%d objects not deleted, first %s: %s %s", len(res.Errors), aws.ToString(x.Key), aws.ToString(x.Code), aws.ToString(x.Message))
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	_, err := e.c.S3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(b), ExpectedBucketOwner: owner})
	return err
}

func (e *tdEnv) stepRepositories(ctx context.Context) ([]tdTarget, []string, error) {
	repos, err := e.repositories(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, name := range tdKeys(repos) {
		if r := e.refuse("ECR repository", name, repos[name]); r != "" {
			rs = append(rs, r)
			continue
		}
		n := name
		ts = append(ts, tdTarget{kind: "ECR repository", id: n + " (and its images)", del: func(ctx context.Context) error {
			_, err := e.c.ECR.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{RepositoryName: aws.String(n), Force: true})
			return err
		}})
	}
	return ts, rs, nil
}

// ---- phase 5 and 6: node groups, cluster, cluster roles -----------------------

func (e *tdEnv) stepNodegroups(ctx context.Context) ([]tdTarget, []string, error) {
	cl, err := e.cluster(ctx)
	if err != nil || cl == nil {
		return nil, nil, err
	}
	ngs, err := e.nodegroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	e.noteRefs(nil, ngs, nil)
	var ts []tdTarget
	var rs []string
	for _, ng := range ngs {
		name := aws.ToString(ng.NodegroupName)
		if r := e.refuse("EKS node group", name, ng.Tags); r != "" {
			rs = append(rs, r)
			continue
		}
		t := tdTarget{kind: "EKS node group", id: name, wait: gone(func(ctx context.Context) (bool, error) {
			_, err := e.c.EKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(e.n.Cluster), NodegroupName: aws.String(name)})
			if tdGone(err) {
				return false, nil
			}
			return err == nil, err
		})}
		if ng.Status != ekstypes.NodegroupStatusDeleting {
			t.del = func(ctx context.Context) error {
				_, err := e.c.EKS.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{ClusterName: aws.String(e.n.Cluster), NodegroupName: aws.String(name)})
				return err
			}
		}
		ts = append(ts, t)
	}
	return ts, rs, nil
}

func (e *tdEnv) stepCluster(ctx context.Context) ([]tdTarget, []string, error) {
	cl, err := e.cluster(ctx)
	if err != nil || cl == nil {
		return nil, nil, err
	}
	e.noteRefs(cl, nil, nil)
	if r := e.refuse("EKS cluster", e.n.Cluster, cl.Tags); r != "" {
		return nil, []string{r}, nil
	}
	t := tdTarget{kind: "EKS cluster", id: e.n.Cluster, wait: gone(func(ctx context.Context) (bool, error) {
		cl, err := e.cluster(ctx)
		return cl != nil, err
	})}
	if cl.Status != ekstypes.ClusterStatusDeleting {
		t.del = func(ctx context.Context) error {
			// EKS refuses while it still removes node groups or add-ons.
			return retryOn(ctx, "cluster "+e.n.Cluster+" to accept the delete", func() error {
				_, err := e.c.EKS.DeleteCluster(ctx, &eks.DeleteClusterInput{Name: aws.String(e.n.Cluster)})
				return err
			}, "ResourceInUseException")
		}
	}
	return []tdTarget{t}, nil, nil
}

// stepClusterRoles deletes the owned roles the cluster, node groups and
// associations pointed at (the cluster, node and EBS CSI roles), and any
// other owned role named "<cluster>-...", once the cluster is gone.
func (e *tdEnv) stepClusterRoles(ctx context.Context) ([]tdTarget, []string, error) {
	roles, err := e.roles(ctx)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, n := range tdKeys(roles) {
		if !e.isSubstrateRole(n) {
			names = append(names, n)
		}
	}
	return e.roleTargets(ctx, names, false)
}

// ---- phase 7: launch template, volumes, network --------------------------------

// stepLaunchTemplates deletes the node groups' launch templates: those with
// the derived names (one of another owner is a refusal) and every other one
// with the owner tag.
func (e *tdEnv) stepLaunchTemplates(ctx context.Context) ([]tdTarget, []string, error) {
	named, err := e.ec2List(ctx, kLT, flt("launch-template-name", e.ltNames()...))
	if err != nil {
		return nil, nil, err
	}
	owned, err := e.ec2List(ctx, kLT, e.ownerFilter())
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	seen := map[string]bool{}
	for _, lt := range append(named, owned...) {
		if seen[lt.id] {
			continue
		}
		seen[lt.id] = true
		label := lt.name + " (" + lt.id + ")"
		if r := e.refuse(kLT, label, lt.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := lt.id
		ts = append(ts, tdTarget{kind: kLT, id: label, del: func(ctx context.Context) error {
			_, err := e.c.EC2.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: aws.String(id)})
			return err
		}})
	}
	return ts, rs, nil
}

// stepVolumes deletes the detached PVC volumes a deleted cluster could no
// longer release: those with the owner tag, and the ate-system ones EKS Auto
// Mode made for this cluster without it (clusterPVC). Any other detached
// volume, a user's Retain volume among them, is left alone.
func (e *tdEnv) stepVolumes(ctx context.Context) ([]tdTarget, []string, error) {
	vs, err := e.volumes(ctx, flt("status", "available"))
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, v := range vs {
		if !e.clusterPVC(v.tags) {
			if r := e.refuse(kVolume, v.id, v.tags); r != "" {
				rs = append(rs, r)
				continue
			}
		}
		id := v.id
		ts = append(ts, tdTarget{kind: kVolume, id: id, del: func(ctx context.Context) error {
			_, err := e.c.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(id)})
			return err
		}})
	}
	return ts, rs, nil
}

// inVPCs lists one kind in each owned VPC.
func (e *tdEnv) inVPCs(ctx context.Context, kind, vpcFilter string, f ...ec2types.Filter) ([]ec2Item, error) {
	vpcs, err := e.ownedVPCs(ctx)
	if err != nil {
		return nil, err
	}
	var out []ec2Item
	for _, v := range vpcs {
		items, err := e.ec2List(ctx, kind, append([]ec2types.Filter{flt(vpcFilter, v.id)}, f...)...)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// stepNATs deletes the NAT gateways and waits until they are deleted: a NAT
// gateway holds its Elastic IP and its subnet until then.
func (e *tdEnv) stepNATs(ctx context.Context) ([]tdTarget, []string, error) {
	nats, err := e.inVPCs(ctx, kNAT, "vpc-id")
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, n := range nats {
		if n.state == "deleted" || n.state == "failed" {
			continue
		}
		if r := e.refuse(kNAT, n.id, n.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := n.id
		t := tdTarget{kind: kNAT, id: id, wait: gone(func(ctx context.Context) (bool, error) {
			out, err := e.ec2List(ctx, kNAT, flt("nat-gateway-id", id))
			if err != nil {
				return false, err
			}
			return len(out) > 0 && out[0].state != "deleted", nil
		})}
		if n.state != "deleting" {
			t.del = func(ctx context.Context) error {
				_, err := e.c.EC2.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(id)})
				return err
			}
		}
		ts = append(ts, t)
	}
	return ts, rs, nil
}

func (e *tdEnv) stepEIPs(ctx context.Context) ([]tdTarget, []string, error) {
	eips, err := e.ec2List(ctx, kEIP, e.ownerFilter())
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, a := range eips {
		if r := e.refuse(kEIP, a.id, a.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := a.id
		ts = append(ts, tdTarget{kind: kEIP, id: id, del: func(ctx context.Context) error {
			_, err := e.c.EC2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(id)})
			return err
		}})
	}
	return ts, rs, nil
}

// stepIGWs detaches and deletes the internet gateways: the owned ones (also
// one a previous run detached), and refuses one attached that is not owned.
func (e *tdEnv) stepIGWs(ctx context.Context) ([]tdTarget, []string, error) {
	attached, err := e.inVPCs(ctx, kIGW, "attachment.vpc-id")
	if err != nil {
		return nil, nil, err
	}
	owned, err := e.ec2List(ctx, kIGW, e.ownerFilter())
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	seen := map[string]bool{}
	for _, g := range append(attached, owned...) {
		if seen[g.id] {
			continue
		}
		seen[g.id] = true
		if r := e.refuse(kIGW, g.id, g.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id, vpc := g.id, g.vpc
		ts = append(ts, tdTarget{kind: kIGW, id: id, del: func(ctx context.Context) error {
			if vpc != "" {
				// Public addresses still mapped in the VPC hold the gateway for a while.
				if err := retryOn(ctx, "internet gateway "+id+" to detach", func() error {
					_, err := e.c.EC2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(id), VpcId: aws.String(vpc)})
					if hasCode(err, "Gateway.NotAttached") {
						return nil
					}
					return err
				}, "DependencyViolation"); err != nil {
					return err
				}
			}
			_, err := e.c.EC2.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(id)})
			return err
		}})
	}
	return ts, rs, nil
}

// stepENIs deletes the detached network interfaces in owned VPCs that carry
// the owner tag or this cluster's tag (the VPC CNI's, Auto Mode's). Others
// (EKS releases its own) are left to their owner.
func (e *tdEnv) stepENIs(ctx context.Context) ([]tdTarget, []string, error) {
	enis, err := e.inVPCs(ctx, kENI, "vpc-id", flt("status", "available"))
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	for _, n := range enis {
		if !e.owned(n.tags) && !e.clusterMade(n.tags) {
			continue
		}
		id := n.id
		ts = append(ts, tdTarget{kind: kENI, id: id, del: func(ctx context.Context) error {
			_, err := e.c.EC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)})
			return err
		}})
	}
	return ts, nil, nil
}

// stepVPCSecurityGroups deletes every security group of an owned VPC but its
// default one: owned, or made by EKS for this cluster.
func (e *tdEnv) stepVPCSecurityGroups(ctx context.Context) ([]tdTarget, []string, error) {
	sgs, err := e.inVPCs(ctx, kSG, "vpc-id")
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, sg := range sgs {
		if sg.isDefault {
			continue
		}
		label := sg.name + " (" + sg.id + ")"
		if !e.clusterMade(sg.tags) || sg.tags[tdStackKey] != "" {
			if r := e.refuse(kSG, label, sg.tags); r != "" {
				rs = append(rs, r)
				continue
			}
		}
		ts = append(ts, e.sgTarget(sg.id, label))
	}
	return ts, rs, nil
}

func (e *tdEnv) stepSubnets(ctx context.Context) ([]tdTarget, []string, error) {
	subnets, err := e.inVPCs(ctx, kSubnet, "vpc-id")
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, s := range subnets {
		if r := e.refuse(kSubnet, s.id, s.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := s.id
		ts = append(ts, tdTarget{kind: kSubnet, id: id, del: func(ctx context.Context) error {
			// The ENIs of a deleted cluster leave its subnets a few minutes later.
			return retryOn(ctx, "subnet "+id+" to be free", func() error {
				_, err := e.c.EC2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(id)})
				return err
			}, "DependencyViolation")
		}})
	}
	return ts, rs, nil
}

// stepRouteTables deletes the route tables but the main one, which goes with the VPC.
func (e *tdEnv) stepRouteTables(ctx context.Context) ([]tdTarget, []string, error) {
	rtbs, err := e.inVPCs(ctx, kRTB, "vpc-id")
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, t := range rtbs {
		if t.main {
			continue
		}
		if r := e.refuse(kRTB, t.id, t.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := t.id
		ts = append(ts, tdTarget{kind: kRTB, id: id, del: func(ctx context.Context) error {
			return retryOn(ctx, "route table "+id+" to be free", func() error {
				_, err := e.c.EC2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: aws.String(id)})
				return err
			}, "DependencyViolation")
		}})
	}
	return ts, rs, nil
}

// stepVPCs deletes the owned VPCs, with their main route table, default
// security group and default network ACL.
//
// shortcut: a VPC that still holds a kind teardown does not delete (a VPC
// endpoint, a non-default network ACL, a peering) fails only once tdTimeout
// has passed; add that kind as a step if create cluster starts making one.
func (e *tdEnv) stepVPCs(ctx context.Context) ([]tdTarget, []string, error) {
	vpcs, err := e.ownedVPCs(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ts []tdTarget
	var rs []string
	for _, v := range vpcs {
		if r := e.refuse(kVPC, v.id, v.tags); r != "" {
			rs = append(rs, r)
			continue
		}
		id := v.id
		ts = append(ts, tdTarget{kind: kVPC, id: id, del: func(ctx context.Context) error {
			return retryOn(ctx, "VPC "+id+" to be empty", func() error {
				_, err := e.c.EC2.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(id)})
				return err
			}, "DependencyViolation")
		}})
	}
	return ts, rs, nil
}
