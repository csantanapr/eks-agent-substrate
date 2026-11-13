package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
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

// podIdentities are the least-privilege roles aws/infra.sh makes:
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

// roleForSA ports role_for_sa (lib/common.sh): the role, its trust and inline
// policy, and exactly one Pod Identity association pointing at it.
func (e *env) roleForSA(ctx context.Context, p PodIdentity) error {
	role := e.names.IAMPrefix + "-" + p.ServiceAccount
	roleARN := fmt.Sprintf("arn:%s:iam::%s:role/%s", e.names.Partition, e.names.Account, role)
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

	list, err := e.c.EKS.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
		ClusterName: aws.String(e.names.Cluster), Namespace: aws.String(p.Namespace), ServiceAccount: aws.String(p.ServiceAccount)})
	if err != nil {
		return fmt.Errorf("list Pod Identity associations of %s/%s: %w", p.Namespace, p.ServiceAccount, err)
	}
	switch len(list.Associations) {
	case 0:
		err = e.act(fmt.Sprintf("associate %s/%s with %s", p.Namespace, p.ServiceAccount, role), func() error {
			_, err := e.c.EKS.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
				ClusterName: aws.String(e.names.Cluster), Namespace: aws.String(p.Namespace),
				ServiceAccount: aws.String(p.ServiceAccount), RoleArn: aws.String(roleARN), Tags: e.tags})
			return err
		})
	case 1:
		id := list.Associations[0].AssociationId
		d, derr := e.c.EKS.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{ClusterName: aws.String(e.names.Cluster), AssociationId: id})
		if derr != nil {
			return fmt.Errorf("describe association %s: %w", aws.ToString(id), derr)
		}
		if cur := aws.ToString(d.Association.RoleArn); cur != roleARN {
			slog.Warn("association points at another role; repointing it", "sa", p.Namespace+"/"+p.ServiceAccount, "was", cur)
			err = e.act("repoint association "+aws.ToString(id)+" at "+role, func() error {
				_, err := e.c.EKS.UpdatePodIdentityAssociation(ctx, &eks.UpdatePodIdentityAssociationInput{
					ClusterName: aws.String(e.names.Cluster), AssociationId: id, RoleArn: aws.String(roleARN)})
				return err
			})
		}
	default:
		return fmt.Errorf("%s/%s has %d Pod Identity associations; expected one", p.Namespace, p.ServiceAccount, len(list.Associations))
	}
	if err != nil {
		return fmt.Errorf("association of %s/%s: %w", p.Namespace, p.ServiceAccount, err)
	}
	slog.Info("pod identity ready", "sa", p.Namespace+"/"+p.ServiceAccount, "role", role)
	return nil
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
