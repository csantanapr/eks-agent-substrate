package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// tdObj is one resource of the fake account. Fields are used by the kinds
// that have them.
type tdObj struct {
	kind, id, name             string
	vpc, subnet, alloc, role   string
	autoRole                   string // a cluster's Auto Mode node role
	ns, sa                     string
	state                      string
	main, isDefault, protect   bool
	deleting                   bool
	pending                    int // reads left before a deleting resource is gone
	inline, attached, profiles []string
	objects                    int
	tags                       map[string]string
}

// tdFake is one in-memory account. It enforces the dependencies AWS
// enforces (a cluster with node groups, a subnet with network interfaces, an
// Elastic IP held by a NAT gateway...), deletes in the background like AWS
// (a deleting node group is gone a read later), and records every write in
// order, so a test can check what was deleted and when.
type tdFake struct {
	mu     sync.Mutex
	objs   map[string]*tdObj
	calls  []string
	deny   map[string]bool // write op -> AccessDenied
	vanish map[string]bool // write op -> the resource was deleted behind our back
	tagged []rgttypes.ResourceTagMapping
}

func newTDFake() *tdFake {
	return &tdFake{objs: map[string]*tdObj{}, deny: map[string]bool{}, vanish: map[string]bool{}}
}

func (f *tdFake) clients() *tdClients {
	return &tdClients{STS: fsts{}, Tag: (*tfTag)(f), EC2: (*tfEC2)(f), EKS: (*tfEKS)(f), IAM: (*tfIAM)(f),
		RDS: (*tfRDS)(f), SM: (*tfSM)(f), S3: (*tfS3)(f), ECR: (*tfECR)(f)}
}

func (f *tdFake) put(o *tdObj) { f.objs[o.kind+"|"+o.id] = o }

// settle finishes a background delete whose reads have run out.
func (f *tdFake) settle(o *tdObj) bool {
	if !o.deleting {
		return true
	}
	if o.pending > 0 {
		o.pending--
		return true
	}
	if o.kind == kNAT {
		o.state, o.deleting = "deleted", false
		return true
	}
	f.remove(o)
	return false
}

// remove deletes o and what AWS deletes with it.
func (f *tdFake) remove(o *tdObj) {
	delete(f.objs, o.kind+"|"+o.id)
	switch o.kind {
	case "nodegroup":
		for _, x := range f.objs {
			if x.tags["eks:nodegroup-name"] == o.id {
				delete(f.objs, x.kind+"|"+x.id)
			}
		}
	case "cluster":
		for _, x := range f.objs {
			if x.kind == "assoc" || (x.kind == kSG && x.tags["aws:eks:cluster-name"] == o.id) {
				delete(f.objs, x.kind+"|"+x.id)
			}
		}
	case kVPC:
		for _, x := range f.objs {
			if x.vpc == o.id && (x.main || x.isDefault) {
				delete(f.objs, x.kind+"|"+x.id)
			}
		}
	}
}

func (f *tdFake) get(kind, id string) *tdObj {
	o := f.objs[kind+"|"+id]
	if o == nil || !f.settle(o) {
		return nil
	}
	return o
}

func (f *tdFake) list(kind string) []*tdObj {
	var ids []string
	for _, o := range f.objs {
		if o.kind == kind {
			ids = append(ids, o.id)
		}
	}
	sort.Strings(ids)
	var out []*tdObj
	for _, id := range ids {
		if o := f.get(kind, id); o != nil {
			out = append(out, o)
		}
	}
	return out
}

func (f *tdFake) count(kind string, match func(*tdObj) bool) int {
	n := 0
	for _, o := range f.objs {
		if o.kind == kind && match(o) {
			n++
		}
	}
	return n
}

// write records a write. It fails with AccessDenied when denied, and with
// notFound, after deleting the resource, when it vanished behind our back.
func (f *tdFake) write(op, id, kind, notFound string) error {
	f.calls = append(f.calls, op+" "+id)
	if f.deny[op] {
		return apiErr("AccessDenied")
	}
	if f.vanish[op] {
		if o := f.objs[kind+"|"+id]; o != nil {
			f.remove(o)
		}
		return apiErr(notFound)
	}
	return nil
}

func tagsOf(m map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for _, k := range sortedKeys(m) {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// ---- EKS --------------------------------------------------------------------

type tfEKS tdFake

func (t *tfEKS) f() *tdFake { return (*tdFake)(t) }

func (t *tfEKS) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.get("cluster", aws.ToString(in.Name))
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	st := ekstypes.ClusterStatusActive
	if o.deleting {
		st = ekstypes.ClusterStatusDeleting
	}
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{Name: aws.String(o.id), Status: st, Tags: o.tags,
		RoleArn: aws.String(o.role), ResourcesVpcConfig: &ekstypes.VpcConfigResponse{VpcId: aws.String(o.vpc)},
		ComputeConfig: &ekstypes.ComputeConfigResponse{Enabled: aws.Bool(o.autoRole != ""), NodeRoleArn: aws.String(o.autoRole)}}}, nil
}
func (t *tfEKS) ListNodegroups(_ context.Context, in *eks.ListNodegroupsInput, _ ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objs["cluster|"+aws.ToString(in.ClusterName)] == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &eks.ListNodegroupsOutput{}
	for _, o := range f.list("nodegroup") {
		out.Nodegroups = append(out.Nodegroups, o.id)
	}
	return out, nil
}
func (t *tfEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.get("nodegroup", aws.ToString(in.NodegroupName))
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	st := ekstypes.NodegroupStatusActive
	if o.deleting {
		st = ekstypes.NodegroupStatusDeleting
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: &ekstypes.Nodegroup{NodegroupName: aws.String(o.id), NodeRole: aws.String(o.role), Status: st, Tags: o.tags}}, nil
}
func (t *tfEKS) ListPodIdentityAssociations(_ context.Context, in *eks.ListPodIdentityAssociationsInput, _ ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objs["cluster|"+aws.ToString(in.ClusterName)] == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &eks.ListPodIdentityAssociationsOutput{}
	for _, o := range f.list("assoc") {
		out.Associations = append(out.Associations, ekstypes.PodIdentityAssociationSummary{AssociationId: aws.String(o.id), Namespace: aws.String(o.ns), ServiceAccount: aws.String(o.sa)})
	}
	return out, nil
}
func (t *tfEKS) DescribePodIdentityAssociation(_ context.Context, in *eks.DescribePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.get("assoc", aws.ToString(in.AssociationId))
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	return &eks.DescribePodIdentityAssociationOutput{Association: &ekstypes.PodIdentityAssociation{AssociationId: aws.String(o.id),
		Namespace: aws.String(o.ns), ServiceAccount: aws.String(o.sa), RoleArn: aws.String(o.role), Tags: o.tags}}, nil
}
func (t *tfEKS) DeletePodIdentityAssociation(_ context.Context, in *eks.DeletePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.AssociationId)
	if err := f.write("DeletePodIdentityAssociation", id, "assoc", "ResourceNotFoundException"); err != nil {
		return nil, err
	}
	o := f.objs["assoc|"+id]
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	f.remove(o)
	return &eks.DeletePodIdentityAssociationOutput{}, nil
}
func (t *tfEKS) DeleteNodegroup(_ context.Context, in *eks.DeleteNodegroupInput, _ ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.NodegroupName)
	if err := f.write("DeleteNodegroup", id, "nodegroup", "ResourceNotFoundException"); err != nil {
		return nil, err
	}
	o := f.objs["nodegroup|"+id]
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	o.deleting, o.pending = true, 1
	return &eks.DeleteNodegroupOutput{}, nil
}
func (t *tfEKS) DeleteCluster(_ context.Context, in *eks.DeleteClusterInput, _ ...func(*eks.Options)) (*eks.DeleteClusterOutput, error) {
	f := t.f()
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.ToString(in.Name)
	if err := f.write("DeleteCluster", id, "cluster", "ResourceNotFoundException"); err != nil {
		return nil, err
	}
	if f.count("nodegroup", func(*tdObj) bool { return true }) > 0 {
		return nil, apiErr("ResourceInUseException")
	}
	o := f.objs["cluster|"+id]
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	o.deleting, o.pending = true, 1
	return &eks.DeleteClusterOutput{}, nil
}

// ---- IAM --------------------------------------------------------------------

type tfIAM tdFake

func (t *tfIAM) f() *tdFake { return (*tdFake)(t) }

func (t *tfIAM) role(name string) (*tdObj, error) {
	o := t.f().get("role", name)
	if o == nil {
		return nil, apiErr("NoSuchEntity")
	}
	return o, nil
}
func (t *tfIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	r := &iamtypes.Role{RoleName: aws.String(o.id)}
	for _, k := range sortedKeys(o.tags) {
		r.Tags = append(r.Tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(o.tags[k])})
	}
	return &iam.GetRoleOutput{Role: r}, nil
}
func (t *tfIAM) ListRoles(context.Context, *iam.ListRolesInput, ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	out := &iam.ListRolesOutput{}
	for _, o := range t.f().list("role") {
		out.Roles = append(out.Roles, iamtypes.Role{RoleName: aws.String(o.id)}) // ListRoles returns no tags
	}
	return out, nil
}
func (t *tfIAM) ListRolePolicies(_ context.Context, in *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	return &iam.ListRolePoliciesOutput{PolicyNames: append([]string(nil), o.inline...)}, nil
}
func (t *tfIAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	if err := t.f().write("DeleteRolePolicy", aws.ToString(in.RoleName), "role", "NoSuchEntity"); err != nil {
		return nil, err
	}
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	o.inline = without(o.inline, aws.ToString(in.PolicyName))
	return &iam.DeleteRolePolicyOutput{}, nil
}
func (t *tfIAM) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, p := range o.attached {
		out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{PolicyArn: aws.String(p)})
	}
	return out, nil
}
func (t *tfIAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	if err := t.f().write("DetachRolePolicy", aws.ToString(in.RoleName), "role", "NoSuchEntity"); err != nil {
		return nil, err
	}
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	o.attached = without(o.attached, aws.ToString(in.PolicyArn))
	return &iam.DetachRolePolicyOutput{}, nil
}
func (t *tfIAM) ListInstanceProfilesForRole(_ context.Context, in *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	out := &iam.ListInstanceProfilesForRoleOutput{}
	for _, p := range o.profiles {
		out.InstanceProfiles = append(out.InstanceProfiles, iamtypes.InstanceProfile{InstanceProfileName: aws.String(p)})
	}
	return out, nil
}
func (t *tfIAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	if err := t.f().write("RemoveRoleFromInstanceProfile", aws.ToString(in.RoleName), "role", "NoSuchEntity"); err != nil {
		return nil, err
	}
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	o.profiles = without(o.profiles, aws.ToString(in.InstanceProfileName))
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}
func (t *tfIAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	if err := t.f().write("DeleteRole", aws.ToString(in.RoleName), "role", "NoSuchEntity"); err != nil {
		return nil, err
	}
	o, err := t.role(aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	if len(o.inline)+len(o.attached)+len(o.profiles) > 0 {
		return nil, apiErr("DeleteConflict")
	}
	t.f().remove(o)
	return &iam.DeleteRoleOutput{}, nil
}

func without(s []string, x string) []string {
	var out []string
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// ---- RDS and Secrets Manager -------------------------------------------------

type tfRDS tdFake

func (t *tfRDS) f() *tdFake { return (*tdFake)(t) }

func rdsTagList(m map[string]string) []rdstypes.Tag {
	var out []rdstypes.Tag
	for _, k := range sortedKeys(m) {
		out = append(out, rdstypes.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

func (t *tfRDS) DescribeDBInstances(_ context.Context, in *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o := t.f().get("db", aws.ToString(in.DBInstanceIdentifier))
	if o == nil {
		return nil, apiErr("DBInstanceNotFound")
	}
	st := "available"
	if o.deleting {
		st = "deleting"
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{DBInstanceIdentifier: aws.String(o.id),
		DBInstanceStatus: aws.String(st), TagList: rdsTagList(o.tags), DeletionProtection: aws.Bool(o.protect)}}}, nil
}
func (t *tfRDS) DeleteDBInstance(_ context.Context, in *rds.DeleteDBInstanceInput, _ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	id := aws.ToString(in.DBInstanceIdentifier)
	if err := t.f().write("DeleteDBInstance", id, "db", "DBInstanceNotFound"); err != nil {
		return nil, err
	}
	o := t.f().objs["db|"+id]
	if o == nil {
		return nil, apiErr("DBInstanceNotFound")
	}
	if !aws.ToBool(in.SkipFinalSnapshot) || !aws.ToBool(in.DeleteAutomatedBackups) {
		return nil, fmt.Errorf("RDS delete without SkipFinalSnapshot and DeleteAutomatedBackups")
	}
	if o.protect {
		return nil, apiErr("InvalidParameterCombination")
	}
	o.deleting, o.pending = true, 1
	return &rds.DeleteDBInstanceOutput{}, nil
}
func (t *tfRDS) DescribeDBSubnetGroups(_ context.Context, in *rds.DescribeDBSubnetGroupsInput, _ ...func(*rds.Options)) (*rds.DescribeDBSubnetGroupsOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o := t.f().get("subgrp", aws.ToString(in.DBSubnetGroupName))
	if o == nil {
		return nil, apiErr("DBSubnetGroupNotFoundFault")
	}
	return &rds.DescribeDBSubnetGroupsOutput{DBSubnetGroups: []rdstypes.DBSubnetGroup{{DBSubnetGroupName: aws.String(o.id),
		DBSubnetGroupArn: aws.String("arn:aws:rds:us-west-2:111122223333:subgrp:" + o.id)}}}, nil
}
func (t *tfRDS) ListTagsForResource(_ context.Context, in *rds.ListTagsForResourceInput, _ ...func(*rds.Options)) (*rds.ListTagsForResourceOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	_, _, name := arnType(aws.ToString(in.ResourceName))
	o := t.f().get("subgrp", name)
	if o == nil {
		return nil, apiErr("DBSubnetGroupNotFoundFault")
	}
	return &rds.ListTagsForResourceOutput{TagList: rdsTagList(o.tags)}, nil
}
func (t *tfRDS) DeleteDBSubnetGroup(_ context.Context, in *rds.DeleteDBSubnetGroupInput, _ ...func(*rds.Options)) (*rds.DeleteDBSubnetGroupOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	id := aws.ToString(in.DBSubnetGroupName)
	if err := t.f().write("DeleteDBSubnetGroup", id, "subgrp", "DBSubnetGroupNotFoundFault"); err != nil {
		return nil, err
	}
	if t.f().objs["db|"+id] != nil {
		return nil, apiErr("InvalidDBSubnetGroupStateFault")
	}
	o := t.f().objs["subgrp|"+id]
	if o == nil {
		return nil, apiErr("DBSubnetGroupNotFoundFault")
	}
	t.f().remove(o)
	return &rds.DeleteDBSubnetGroupOutput{}, nil
}

type tfSM tdFake

func (t *tfSM) f() *tdFake { return (*tdFake)(t) }

func (t *tfSM) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o := t.f().get("secret", aws.ToString(in.SecretId))
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &secretsmanager.DescribeSecretOutput{Name: aws.String(o.id)}
	for _, k := range sortedKeys(o.tags) {
		out.Tags = append(out.Tags, smtypes.Tag{Key: aws.String(k), Value: aws.String(o.tags[k])})
	}
	if o.deleting {
		out.DeletedDate = aws.Time(time.Unix(0, 0))
	}
	return out, nil
}
func (t *tfSM) DeleteSecret(_ context.Context, in *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	id := aws.ToString(in.SecretId)
	if err := t.f().write("DeleteSecret", id, "secret", "ResourceNotFoundException"); err != nil {
		return nil, err
	}
	o := t.f().objs["secret|"+id]
	if o == nil {
		return nil, apiErr("ResourceNotFoundException")
	}
	if !aws.ToBool(in.ForceDeleteWithoutRecovery) {
		return nil, fmt.Errorf("secret %s deleted with a recovery window", id)
	}
	if o.deleting {
		return nil, apiErr("InvalidRequestException")
	}
	o.deleting, o.pending = true, 1
	return &secretsmanager.DeleteSecretOutput{}, nil
}

// ---- S3 and ECR ---------------------------------------------------------------

type tfS3 tdFake

func (t *tfS3) f() *tdFake { return (*tdFake)(t) }

func (t *tfS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	if t.f().get("bucket", aws.ToString(in.Bucket)) == nil {
		return nil, apiErr("NotFound")
	}
	return &s3.HeadBucketOutput{}, nil
}
func (t *tfS3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o := t.f().get("bucket", aws.ToString(in.Bucket))
	if o == nil {
		return nil, apiErr("NoSuchBucket")
	}
	if len(o.tags) == 0 {
		return nil, apiErr("NoSuchTagSet")
	}
	out := &s3.GetBucketTaggingOutput{}
	for _, k := range sortedKeys(o.tags) {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(o.tags[k])})
	}
	return out, nil
}

// ListObjectVersions returns two versions (and a delete marker) per page.
func (t *tfS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o := t.f().get("bucket", aws.ToString(in.Bucket))
	if o == nil {
		return nil, apiErr("NoSuchBucket")
	}
	out := &s3.ListObjectVersionsOutput{IsTruncated: aws.Bool(o.objects > 3), NextKeyMarker: aws.String("k"), NextVersionIdMarker: aws.String("v")}
	for i := 0; i < min(2, o.objects); i++ {
		out.Versions = append(out.Versions, s3types.ObjectVersion{Key: aws.String(fmt.Sprintf("k%d", i)), VersionId: aws.String(fmt.Sprintf("v%d", i))})
	}
	if o.objects > 2 {
		out.DeleteMarkers = append(out.DeleteMarkers, s3types.DeleteMarkerEntry{Key: aws.String("k2"), VersionId: aws.String("d2")})
	}
	return out, nil
}
func (t *tfS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	id := aws.ToString(in.Bucket)
	if err := t.f().write("DeleteObjects", id, "bucket", "NoSuchBucket"); err != nil {
		return nil, err
	}
	o := t.f().objs["bucket|"+id]
	if o == nil {
		return nil, apiErr("NoSuchBucket")
	}
	o.objects -= len(in.Delete.Objects)
	return &s3.DeleteObjectsOutput{}, nil
}
func (t *tfS3) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	id := aws.ToString(in.Bucket)
	if err := t.f().write("DeleteBucket", id, "bucket", "NoSuchBucket"); err != nil {
		return nil, err
	}
	o := t.f().objs["bucket|"+id]
	if o == nil {
		return nil, apiErr("NoSuchBucket")
	}
	if o.objects > 0 {
		return nil, apiErr("BucketNotEmpty")
	}
	t.f().remove(o)
	return &s3.DeleteBucketOutput{}, nil
}

type tfECR tdFake

func (t *tfECR) f() *tdFake { return (*tdFake)(t) }

const repoARN = "arn:aws:ecr:us-west-2:111122223333:repository/"

func (t *tfECR) DescribeRepositories(context.Context, *ecr.DescribeRepositoriesInput, ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	out := &ecr.DescribeRepositoriesOutput{}
	for _, o := range t.f().list("repo") {
		out.Repositories = append(out.Repositories, ecrtypes.Repository{RepositoryName: aws.String(o.id), RepositoryArn: aws.String(repoARN + o.id)})
	}
	return out, nil
}
func (t *tfECR) ListTagsForResource(_ context.Context, in *ecr.ListTagsForResourceInput, _ ...func(*ecr.Options)) (*ecr.ListTagsForResourceOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	o := t.f().get("repo", strings.TrimPrefix(aws.ToString(in.ResourceArn), repoARN))
	if o == nil {
		return nil, apiErr("RepositoryNotFoundException")
	}
	out := &ecr.ListTagsForResourceOutput{}
	for _, k := range sortedKeys(o.tags) {
		out.Tags = append(out.Tags, ecrtypes.Tag{Key: aws.String(k), Value: aws.String(o.tags[k])})
	}
	return out, nil
}
func (t *tfECR) DeleteRepository(_ context.Context, in *ecr.DeleteRepositoryInput, _ ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error) {
	t.f().mu.Lock()
	defer t.f().mu.Unlock()
	id := aws.ToString(in.RepositoryName)
	if err := t.f().write("DeleteRepository", id, "repo", "RepositoryNotFoundException"); err != nil {
		return nil, err
	}
	if !in.Force {
		return nil, fmt.Errorf("repository %s deleted without Force", id)
	}
	o := t.f().objs["repo|"+id]
	if o == nil {
		return nil, apiErr("RepositoryNotFoundException")
	}
	t.f().remove(o)
	return &ecr.DeleteRepositoryOutput{}, nil
}

// ---- tagging API --------------------------------------------------------------

type tfTag tdFake

func (t *tfTag) GetResources(context.Context, *rgt.GetResourcesInput, ...func(*rgt.Options)) (*rgt.GetResourcesOutput, error) {
	return &rgt.GetResourcesOutput{ResourceTagMappingList: (*tdFake)(t).tagged}, nil
}

// ---- EC2 ----------------------------------------------------------------------

type tfEC2 tdFake

func (t *tfEC2) f() *tdFake { return (*tdFake)(t) }

// find lists kind matching the EC2 filters teardown uses.
func (t *tfEC2) find(kind string, filters []ec2types.Filter) []*tdObj {
	var out []*tdObj
	for _, o := range t.f().list(kind) {
		ok := true
		for _, fl := range filters {
			name, v := aws.ToString(fl.Name), ""
			switch {
			case name == "vpc-id", name == "attachment.vpc-id":
				v = o.vpc
			case name == "group-name", name == "launch-template-name":
				v = o.name
			case name == "status":
				v = o.state
			case name == "nat-gateway-id":
				v = o.id
			case strings.HasPrefix(name, "tag:"):
				v = o.tags[strings.TrimPrefix(name, "tag:")]
			default:
				panic("fake EC2: unsupported filter " + name)
			}
			ok = ok && v != "" && contains(fl.Values, v)
		}
		if ok {
			out = append(out, o)
		}
	}
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func (t *tfEC2) lock() func() { t.f().mu.Lock(); return t.f().mu.Unlock }

func (t *tfEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeVpcsOutput{}
	for _, o := range t.find(kVPC, in.Filters) {
		out.Vpcs = append(out.Vpcs, ec2types.Vpc{VpcId: aws.String(o.id), IsDefault: aws.Bool(o.isDefault), Tags: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeSubnetsOutput{}
	for _, o := range t.find(kSubnet, in.Filters) {
		out.Subnets = append(out.Subnets, ec2types.Subnet{SubnetId: aws.String(o.id), VpcId: aws.String(o.vpc), Tags: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, o := range t.find(kSG, in.Filters) {
		out.SecurityGroups = append(out.SecurityGroups, ec2types.SecurityGroup{GroupId: aws.String(o.id), GroupName: aws.String(o.name), VpcId: aws.String(o.vpc), Tags: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeInternetGateways(_ context.Context, in *ec2.DescribeInternetGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeInternetGatewaysOutput{}
	for _, o := range t.find(kIGW, in.Filters) {
		g := ec2types.InternetGateway{InternetGatewayId: aws.String(o.id), Tags: tagsOf(o.tags)}
		if o.vpc != "" {
			g.Attachments = []ec2types.InternetGatewayAttachment{{VpcId: aws.String(o.vpc)}}
		}
		out.InternetGateways = append(out.InternetGateways, g)
	}
	return out, nil
}
func (t *tfEC2) DescribeNatGateways(_ context.Context, in *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeNatGatewaysOutput{}
	for _, o := range t.find(kNAT, in.Filter) {
		out.NatGateways = append(out.NatGateways, ec2types.NatGateway{NatGatewayId: aws.String(o.id), VpcId: aws.String(o.vpc),
			State: ec2types.NatGatewayState(o.state), Tags: tagsOf(o.tags), NatGatewayAddresses: []ec2types.NatGatewayAddress{{AllocationId: aws.String(o.alloc)}}})
	}
	return out, nil
}
func (t *tfEC2) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeAddressesOutput{}
	for _, o := range t.find(kEIP, in.Filters) {
		out.Addresses = append(out.Addresses, ec2types.Address{AllocationId: aws.String(o.id), Tags: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeRouteTables(_ context.Context, in *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeRouteTablesOutput{}
	for _, o := range t.find(kRTB, in.Filters) {
		out.RouteTables = append(out.RouteTables, ec2types.RouteTable{RouteTableId: aws.String(o.id), VpcId: aws.String(o.vpc), Tags: tagsOf(o.tags),
			Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(o.main)}}})
	}
	return out, nil
}
func (t *tfEC2) DescribeLaunchTemplates(_ context.Context, in *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeLaunchTemplatesOutput{}
	for _, o := range t.find(kLT, in.Filters) {
		out.LaunchTemplates = append(out.LaunchTemplates, ec2types.LaunchTemplate{LaunchTemplateId: aws.String(o.id), LaunchTemplateName: aws.String(o.name), Tags: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	defer t.lock()()
	if !aws.ToBool(in.IncludeManagedResources) {
		return nil, fmt.Errorf("network interfaces described without IncludeManagedResources: Auto Mode's are hidden")
	}
	out := &ec2.DescribeNetworkInterfacesOutput{}
	for _, o := range t.find(kENI, in.Filters) {
		out.NetworkInterfaces = append(out.NetworkInterfaces, ec2types.NetworkInterface{NetworkInterfaceId: aws.String(o.id), VpcId: aws.String(o.vpc),
			Status: ec2types.NetworkInterfaceStatus(o.state), TagSet: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	defer t.lock()()
	out := &ec2.DescribeVolumesOutput{}
	for _, o := range t.find(kVolume, in.Filters) {
		out.Volumes = append(out.Volumes, ec2types.Volume{VolumeId: aws.String(o.id), State: ec2types.VolumeState(o.state), Tags: tagsOf(o.tags)})
	}
	return out, nil
}
func (t *tfEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	defer t.lock()()
	if !aws.ToBool(in.IncludeManagedResources) {
		return nil, fmt.Errorf("instances described without IncludeManagedResources: Auto Mode's are hidden")
	}
	r := ec2types.Reservation{}
	for _, o := range t.find(kInstance, in.Filters) {
		r.Instances = append(r.Instances, ec2types.Instance{InstanceId: aws.String(o.id), VpcId: aws.String(o.vpc),
			State: &ec2types.InstanceState{Name: ec2types.InstanceStateName(o.state)}, Tags: tagsOf(o.tags)})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{r}}, nil
}

// del is the shared body of the EC2 deletes: record, then refuse with code
// when blocked() names a dependency, then delete.
func (t *tfEC2) del(op, kind, id, notFound string, blocked func(o *tdObj) string) error {
	defer t.lock()()
	f := t.f()
	if err := f.write(op, id, kind, notFound); err != nil {
		return err
	}
	o := f.objs[kind+"|"+id]
	if o == nil {
		return apiErr(notFound)
	}
	if blocked != nil {
		if code := blocked(o); code != "" {
			return apiErr(code)
		}
	}
	f.remove(o)
	return nil
}

func (t *tfEC2) any(kind string, match func(*tdObj) bool) bool { return t.f().count(kind, match) > 0 }

func (t *tfEC2) DeleteVpc(_ context.Context, in *ec2.DeleteVpcInput, _ ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error) {
	id := aws.ToString(in.VpcId)
	return &ec2.DeleteVpcOutput{}, t.del("DeleteVpc", kVPC, id, "InvalidVpcID.NotFound", func(*tdObj) string {
		inVPC := func(o *tdObj) bool { return o.vpc == id }
		if t.any(kSubnet, inVPC) || t.any(kIGW, inVPC) || t.any(kENI, inVPC) || t.any(kInstance, inVPC) ||
			t.any(kSG, func(o *tdObj) bool { return o.vpc == id && !o.isDefault }) ||
			t.any(kRTB, func(o *tdObj) bool { return o.vpc == id && !o.main }) ||
			t.any(kNAT, func(o *tdObj) bool { return o.vpc == id && o.state != "deleted" }) {
			return "DependencyViolation"
		}
		return ""
	})
}
func (t *tfEC2) DeleteSubnet(_ context.Context, in *ec2.DeleteSubnetInput, _ ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error) {
	id := aws.ToString(in.SubnetId)
	return &ec2.DeleteSubnetOutput{}, t.del("DeleteSubnet", kSubnet, id, "InvalidSubnetID.NotFound", func(*tdObj) string {
		// The cluster's own network interfaces sit in its subnets until it is gone.
		if t.any("cluster", func(*tdObj) bool { return true }) || t.any(kENI, func(o *tdObj) bool { return o.subnet == id }) ||
			t.any(kNAT, func(o *tdObj) bool { return o.subnet == id && o.state != "deleted" }) {
			return "DependencyViolation"
		}
		return ""
	})
}
func (t *tfEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	id := aws.ToString(in.GroupId)
	return &ec2.DeleteSecurityGroupOutput{}, t.del("DeleteSecurityGroup", kSG, id, "InvalidGroup.NotFound", func(o *tdObj) string {
		// The RDS instance's network interface uses its security group.
		if o.name == "demo-pg" && t.any("db", func(*tdObj) bool { return true }) {
			return "DependencyViolation"
		}
		return ""
	})
}
func (t *tfEC2) DetachInternetGateway(_ context.Context, in *ec2.DetachInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error) {
	defer t.lock()()
	id := aws.ToString(in.InternetGatewayId)
	if err := t.f().write("DetachInternetGateway", id, kIGW, "InvalidInternetGatewayID.NotFound"); err != nil {
		return nil, err
	}
	o := t.f().objs[kIGW+"|"+id]
	if o == nil {
		return nil, apiErr("InvalidInternetGatewayID.NotFound")
	}
	if o.vpc == "" {
		return nil, apiErr("Gateway.NotAttached")
	}
	o.vpc = ""
	return &ec2.DetachInternetGatewayOutput{}, nil
}
func (t *tfEC2) DeleteInternetGateway(_ context.Context, in *ec2.DeleteInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error) {
	return &ec2.DeleteInternetGatewayOutput{}, t.del("DeleteInternetGateway", kIGW, aws.ToString(in.InternetGatewayId), "InvalidInternetGatewayID.NotFound", func(o *tdObj) string {
		if o.vpc != "" {
			return "DependencyViolation"
		}
		return ""
	})
}
func (t *tfEC2) DeleteNatGateway(_ context.Context, in *ec2.DeleteNatGatewayInput, _ ...func(*ec2.Options)) (*ec2.DeleteNatGatewayOutput, error) {
	defer t.lock()()
	id := aws.ToString(in.NatGatewayId)
	if err := t.f().write("DeleteNatGateway", id, kNAT, "NatGatewayNotFound"); err != nil {
		return nil, err
	}
	o := t.f().objs[kNAT+"|"+id]
	if o == nil {
		return nil, apiErr("NatGatewayNotFound")
	}
	o.state, o.deleting, o.pending = "deleting", true, 1
	return &ec2.DeleteNatGatewayOutput{}, nil
}
func (t *tfEC2) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, _ ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	id := aws.ToString(in.AllocationId)
	return &ec2.ReleaseAddressOutput{}, t.del("ReleaseAddress", kEIP, id, "InvalidAllocationID.NotFound", func(*tdObj) string {
		if t.any(kNAT, func(o *tdObj) bool { return o.alloc == id && o.state != "deleted" }) {
			return "InvalidIPAddress.InUse"
		}
		return ""
	})
}
func (t *tfEC2) DeleteRouteTable(_ context.Context, in *ec2.DeleteRouteTableInput, _ ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error) {
	return &ec2.DeleteRouteTableOutput{}, t.del("DeleteRouteTable", kRTB, aws.ToString(in.RouteTableId), "InvalidRouteTableID.NotFound", func(o *tdObj) string {
		if o.main {
			return "DependencyViolation"
		}
		return ""
	})
}
func (t *tfEC2) DeleteLaunchTemplate(_ context.Context, in *ec2.DeleteLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error) {
	return &ec2.DeleteLaunchTemplateOutput{}, t.del("DeleteLaunchTemplate", kLT, aws.ToString(in.LaunchTemplateId), "InvalidLaunchTemplateId.NotFound", func(*tdObj) string {
		// Test-only rule: a node group still launches from it.
		if t.any("nodegroup", func(*tdObj) bool { return true }) {
			return "Fake.LaunchTemplateInUse"
		}
		return ""
	})
}
func (t *tfEC2) DeleteNetworkInterface(_ context.Context, in *ec2.DeleteNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error) {
	return &ec2.DeleteNetworkInterfaceOutput{}, t.del("DeleteNetworkInterface", kENI, aws.ToString(in.NetworkInterfaceId), "InvalidNetworkInterfaceID.NotFound", func(o *tdObj) string {
		if o.state != "available" {
			return "InvalidNetworkInterface.InUse"
		}
		return ""
	})
}
func (t *tfEC2) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	return &ec2.DeleteVolumeOutput{}, t.del("DeleteVolume", kVolume, aws.ToString(in.VolumeId), "InvalidVolume.NotFound", func(o *tdObj) string {
		if o.state != "available" {
			return "VolumeInUse"
		}
		return ""
	})
}
