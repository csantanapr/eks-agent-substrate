#!/usr/bin/env bash
# Read-only: lists what is left of a deployment and exits 1 if anything is.
#
#   aws/inventory.sh          what clean.sh removes (the cluster may stay)
#   aws/inventory.sh --all    that, plus the cluster and the launch template
#
# A failed AWS read stops the script instead of passing as "not found".
#
# clean.sh ends with it and delete-cluster.sh starts with it. When a script
# gains a resource, add a check for it here too.
#
# Learn more:
#   https://docs.aws.amazon.com/eks/latest/userguide/pod-id-association.html
#   https://docs.aws.amazon.com/vpc/latest/userguide/vpc-security-groups.html
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
left=0
found() { printf '   LEFT  %s\n' "$*"; left=1; }

if cluster_exists; then
  [ "${1:-}" = --all ] && found "EKS cluster $CLUSTER_NAME"
  for pair in $POD_IDENTITY_SAS kube-system/efs-csi-controller-sa kube-system/efs-csi-node-sa external-secrets/external-secrets \
              ate-system/aws-sm-credential-provider; do
    ids=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" \
      --namespace "${pair%/*}" --service-account "${pair#*/}" --query 'associations[].associationId' --output text)
    [ -n "$ids" ] && found "Pod Identity association $pair ($ids)"
  done
  if [ -f "$KUBECONFIG" ]; then
    ns=$(kubectl get namespace -o name) || die "kubectl cannot reach $CLUSTER_NAME"
    for n in openclaw ate-demo-counter ate-demo-counter-microvm llm-gateway ate-db-bootstrap \
             ate-system podcertificate-controller-system external-secrets external-secrets-reloader \
             ate-demo-credprovider; do
      grep -qx "namespace/$n" <<<"$ns" && found "namespace $n"
    done
    crds=$(kubectl get crd -o name | grep -E '\.ate\.dev$' || true)
    [ -n "$crds" ] && found "Substrate CRDs: $(tr '\n' ' ' <<<"$crds")"
  fi
fi

# AWS resources outside the cluster: checked even when the cluster is gone.
for sa in $POD_IDENTITY_SAS; do
  role="${IAM_PREFIX}-${sa#*/}"
  aws_exists iam get-role --role-name "$role" && found "IAM role $role"
done
aws_exists rds describe-db-instances --db-instance-identifier "$DB_ID" && found "RDS instance $DB_ID"
aws_exists rds describe-db-subnet-groups --db-subnet-group-name "$DB_ID" && found "RDS subnet group $DB_ID"
sgs=$(aws_ ec2 describe-security-groups --filters "Name=group-name,Values=$DB_ID" --query 'SecurityGroups[].GroupId' --output text)
[ -n "$sgs" ] && found "security group $DB_ID ($sgs)"
for who in owner readwrite; do
  aws_exists secretsmanager describe-secret --secret-id "$SECRET_PREFIX/ate-$who" && found "secret $SECRET_PREFIX/ate-$who"
done
for b in "$SNAP_BUCKET" "$ASSET_BUCKET"; do
  aws_exists s3api head-bucket --bucket "$b" --expected-bucket-owner "$ACCOUNT" && found "S3 bucket $b"
done
repos=$(aws_ ecr describe-repositories --query "repositories[?starts_with(repositoryName, \`$ECR_PREFIX/\`)].repositoryName" --output text)
[ -n "$repos" ] && found "ECR repositories $repos"
# demos/efs and demos/s3files: file systems hold mount targets in the VPC.
for role in "${IAM_PREFIX}-efs-csi-controller-sa" "${IAM_PREFIX}-efs-csi-node-sa" "${IAM_PREFIX}-s3files-sync"; do
  aws_exists iam get-role --role-name "$role" && found "IAM role $role"
done
efs=$(aws_ efs describe-file-systems --creation-token "$EFS_TOKEN" --query 'FileSystems[].FileSystemId' --output text)
[ -n "$efs" ] && found "EFS file system $efs"
s3fs=$(aws_ s3files list-file-systems --output json | jq -r --arg b "arn:$PARTITION:s3:::$S3FILES_BUCKET" \
  '[.fileSystems[] | select(.bucket == $b and .status != "deleted") | .fileSystemId] | join(" ")')
[ -n "$s3fs" ] && found "S3 file system $s3fs"
aws_exists s3api head-bucket --bucket "$S3FILES_BUCKET" --expected-bucket-owner "$ACCOUNT" && found "S3 bucket $S3FILES_BUCKET"
sgs=$(aws_ ec2 describe-security-groups --filters "Name=group-name,Values=$EFS_SG_NAME,$S3FILES_SG_NAME" \
  --query 'SecurityGroups[].GroupId' --output text)
[ -n "$sgs" ] && found "security groups $EFS_SG_NAME/$S3FILES_SG_NAME ($sgs)"
if cluster_exists && aws_exists eks describe-addon --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver; then
  found "EKS add-on aws-efs-csi-driver"
fi
# demos/db-rotation: the rotation security group is in the VPC.
aws_exists iam get-role --role-name "${IAM_PREFIX}-external-secrets" && found "IAM role ${IAM_PREFIX}-external-secrets"
aws_exists cloudformation describe-stacks --stack-name "$ROTATION_STACK" && found "CloudFormation stack $ROTATION_STACK"
sgs=$(aws_ ec2 describe-security-groups --filters "Name=group-name,Values=$ROTATION_SG_NAME" --query 'SecurityGroups[].GroupId' --output text)
[ -n "$sgs" ] && found "security group $ROTATION_SG_NAME ($sgs)"
# demos/credprovider-aws: the provider's role and the demo secrets (its ECR
# repositories are under ECR_PREFIX, listed above).
aws_exists iam get-role --role-name "${IAM_PREFIX}-aws-sm-credential-provider" \
  && found "IAM role ${IAM_PREFIX}-aws-sm-credential-provider"
for s in demo-token untagged-token; do
  # The name has no cluster in it: count it only when it is ours.
  name="ate/ate-demo-credprovider/$s"
  aws_exists secretsmanager describe-secret --secret-id "$name" || continue
  owner=$(aws_ secretsmanager describe-secret --secret-id "$name" \
    --query "Tags[?Key=='$OWNER_TAG_KEY'].Value | [0]" --output text)
  [ "$owner" = "$OWNER_TAG_VALUE" ] && found "secret $name"
done
# EBS volumes from PVCs (the gp3 StorageClass tags them with the owner tag).
vols=$(aws_ ec2 describe-volumes --filters "Name=tag:$OWNER_TAG_KEY,Values=$OWNER_TAG_VALUE" Name=status,Values=available \
  --query 'Volumes[].VolumeId' --output text)
[ -n "$vols" ] && found "EBS volumes $vols"
if [ "${1:-}" = --all ]; then
  aws_exists ec2 describe-launch-templates --launch-template-names "$LT_NAME" && found "launch template $LT_NAME"
fi

if [ "$left" = 0 ]; then info "nothing left for $CLUSTER_NAME in $ACCOUNT/$AWS_REGION"; fi
exit "$left"
