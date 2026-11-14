package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"
)

var iamCmd = &cobra.Command{
	Use:   "iam",
	Short: "Create the EKS Pod Identity roles of ate-api-server and atelet",
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		return e.createIAM(cmd.Context())
	},
}

// PodIdentity is one ServiceAccount's role and inline policy.
type PodIdentity struct {
	Namespace, ServiceAccount, Policy string
}

// podIdentities are the least-privilege roles bootstrap makes:
// ate-api-server lists, reads, writes and deletes snapshots (its objectstore
// sidecar copies tagged snapshots and deletes unreferenced ones); atelet reads
// and writes snapshots but never deletes, reads the asset bucket, and pulls
// from this deployment's ECR repositories.
func (e *env) podIdentities() []PodIdentity {
	n := e.names
	api := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":"arn:%[1]s:s3:::%[2]s"},`+
		`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:AbortMultipartUpload"],"Resource":"arn:%[1]s:s3:::%[2]s/*"}]}`,
		n.Partition, n.SnapBucket)
	atelet := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:%[1]s:s3:::%[2]s","arn:%[1]s:s3:::%[3]s"]},`+
		`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:AbortMultipartUpload"],"Resource":"arn:%[1]s:s3:::%[2]s/*"},`+
		`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:%[1]s:s3:::%[3]s/*"},`+
		`{"Effect":"Allow","Action":"ecr:GetAuthorizationToken","Resource":"*"},`+
		`{"Effect":"Allow","Action":["ecr:BatchGetImage","ecr:GetDownloadUrlForLayer","ecr:BatchCheckLayerAvailability"],`+
		`"Resource":"arn:%[1]s:ecr:%[4]s:%[5]s:repository/%[6]s/*"}]}`,
		n.Partition, n.SnapBucket, n.AssetBucket, n.Region, n.Account, n.ECRPrefix)
	return []PodIdentity{
		{"ate-system", "ate-api-server", api},
		{"ate-system", "atelet", atelet},
	}
}

func (e *env) createIAM(ctx context.Context) error {
	for _, p := range e.podIdentities() {
		if err := e.roleForSA(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// trustPolicy lets only the EKS Pod Identity service assume the role, and
// only for this cluster (aws:SourceAccount, aws:SourceArn), which guards
// against the confused deputy problem.
func (e *env) trustPolicy() string {
	n := e.names
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",`+
		`"Principal":{"Service":"pods.eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"],`+
		`"Condition":{"StringEquals":{"aws:SourceAccount":"%[2]s"},"ArnLike":{"aws:SourceArn":"arn:%[1]s:eks:%[3]s:%[2]s:cluster/%[4]s"}}}]}`,
		n.Partition, n.Account, n.Region, n.Cluster)
}

// roleForSA makes a ServiceAccount's role: the role, its trust and inline
// policy, and exactly one Pod Identity association pointing at it. An
// association someone else made stops it before anything is written.
func (e *env) roleForSA(ctx context.Context, p PodIdentity) error {
	role := e.names.IAMPrefix + "-" + p.ServiceAccount
	roleARN := fmt.Sprintf("arn:%s:iam::%s:role/%s", e.names.Partition, e.names.Account, role)
	assoc, err := e.ownedAssociation(ctx, p.Namespace, p.ServiceAccount)
	if err != nil {
		return err
	}
	exists, err := e.ownedRole(ctx, role)
	if err != nil {
		return err
	}
	trust := e.trustPolicy()
	if exists {
		err = e.act("update trust policy of "+role, func() error {
			_, err := e.c.IAM.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(role), PolicyDocument: aws.String(trust)})
			return err
		})
	} else {
		err = e.act("create role "+role, func() error {
			tags := make([]iamtypes.Tag, 0, len(e.tags))
			for _, k := range sortedKeys(e.tags) {
				tags = append(tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
			}
			_, err := e.c.IAM.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(role), AssumeRolePolicyDocument: aws.String(trust), Tags: tags})
			return err
		})
	}
	if err != nil {
		return fmt.Errorf("role %s: %w", role, err)
	}
	if err := e.act("put inline policy on "+role, func() error {
		_, err := e.c.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String("access"), PolicyDocument: aws.String(p.Policy)})
		return err
	}); err != nil {
		return fmt.Errorf("policy of %s: %w", role, err)
	}

	switch {
	case assoc == nil:
		err = e.act(fmt.Sprintf("associate %s/%s with %s", p.Namespace, p.ServiceAccount, role), func() error {
			_, err := e.c.EKS.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
				ClusterName: aws.String(e.names.Cluster), Namespace: aws.String(p.Namespace),
				ServiceAccount: aws.String(p.ServiceAccount), RoleArn: aws.String(roleARN), Tags: e.tags})
			return err
		})
	case aws.ToString(assoc.RoleArn) != roleARN:
		id := assoc.AssociationId
		slog.Warn("association points at another role; repointing it", "sa", p.Namespace+"/"+p.ServiceAccount, "was", aws.ToString(assoc.RoleArn))
		err = e.act("repoint association "+aws.ToString(id)+" at "+role, func() error {
			_, err := e.c.EKS.UpdatePodIdentityAssociation(ctx, &eks.UpdatePodIdentityAssociationInput{
				ClusterName: aws.String(e.names.Cluster), AssociationId: id, RoleArn: aws.String(roleARN)})
			return err
		})
	}
	if err != nil {
		return fmt.Errorf("association of %s/%s: %w", p.Namespace, p.ServiceAccount, err)
	}
	if !e.cfg.Plan {
		slog.Info("pod identity ready", "sa", p.Namespace+"/"+p.ServiceAccount, "role", role)
	}
	return nil
}

// ownedAssociation is ns/sa's Pod Identity association, or nil when it has
// none. Its name is fixed, so one without this deployment's owner tag is
// someone else's (the cluster owner's own EFS CSI driver or External
// Secrets, say): that is an error naming it, and it is never repointed at
// this deployment's role, which teardown deletes from under it.
func (e *env) ownedAssociation(ctx context.Context, ns, sa string) (*ekstypes.PodIdentityAssociation, error) {
	list, err := e.c.EKS.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
		ClusterName: aws.String(e.names.Cluster), Namespace: aws.String(ns), ServiceAccount: aws.String(sa)})
	if err != nil {
		return nil, fmt.Errorf("list Pod Identity associations of %s/%s: %w", ns, sa, err)
	}
	switch len(list.Associations) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, fmt.Errorf("%s/%s has %d Pod Identity associations; expected one", ns, sa, len(list.Associations))
	}
	id := list.Associations[0].AssociationId
	d, err := e.c.EKS.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{ClusterName: aws.String(e.names.Cluster), AssociationId: id})
	if err != nil {
		return nil, fmt.Errorf("describe association %s: %w", aws.ToString(id), err)
	}
	if err := e.owned(fmt.Sprintf("Pod Identity association %s/%s (%s)", ns, sa, aws.ToString(id)), d.Association.Tags); err != nil {
		return nil, err
	}
	return d.Association, nil
}

// ownedRole reports whether role exists; one without the owner tag is an error.
func (e *env) ownedRole(ctx context.Context, role string) (bool, error) {
	if _, err := e.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get role %s: %w", role, err)
	}
	out, err := e.c.IAM.ListRoleTags(ctx, &iam.ListRoleTagsInput{RoleName: aws.String(role)})
	if err != nil {
		return false, fmt.Errorf("read tags of role %s: %w", role, err)
	}
	got := ""
	for _, t := range out.Tags {
		if aws.ToString(t.Key) == OwnerTagKey {
			got = aws.ToString(t.Value)
		}
	}
	if got != e.names.OwnerValue {
		return false, fmt.Errorf("IAM role %s exists but is not owned by %s (%s=%q)", role, e.names.OwnerValue, OwnerTagKey, got)
	}
	return true, nil
}
