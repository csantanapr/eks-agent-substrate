#!/usr/bin/env bash
# Read-only: lists what is left of a deployment and exits 1 if anything is.
#
#   aws/inventory.sh          what clean.sh removes (the cluster may stay)
#   aws/inventory.sh --all    that, plus the cluster and the launch template
#
# A failed AWS read stops the script instead of passing as "not found".
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
left=0
found() { printf '   LEFT  %s\n' "$*"; left=1; }

if cluster_exists; then
  [ "${1:-}" = --all ] && found "EKS cluster $CLUSTER_NAME"
  for pair in $POD_IDENTITY_SAS; do
    ids=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" \
      --namespace "${pair%/*}" --service-account "${pair#*/}" --query 'associations[].associationId' --output text)
    [ -n "$ids" ] && found "Pod Identity association $pair ($ids)"
  done
  if [ -f "$KUBECONFIG" ]; then
    ns=$(kubectl get namespace -o name) || die "kubectl cannot reach $CLUSTER_NAME"
    for n in openclaw ate-demo-counter ate-demo-counter-microvm llm-gateway ate-db-bootstrap \
             ate-system podcertificate-controller-system; do
      grep -qx "namespace/$n" <<<"$ns" && found "namespace $n"
    done
    crds=$(kubectl get crd -o name | grep -E '\.ate\.dev$' || true)
    [ -n "$crds" ] && found "Substrate CRDs: $(tr '\n' ' ' <<<"$crds")"
  fi
fi

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
# EBS volumes from PVCs (the gp3 StorageClass tags them with the owner tag).
vols=$(aws_ ec2 describe-volumes --filters "Name=tag:$OWNER_TAG_KEY,Values=$OWNER_TAG_VALUE" Name=status,Values=available \
  --query 'Volumes[].VolumeId' --output text)
[ -n "$vols" ] && found "EBS volumes $vols"
if [ "${1:-}" = --all ]; then
  aws_exists ec2 describe-launch-templates --launch-template-names "$LT_NAME" && found "launch template $LT_NAME"
fi

if [ "$left" = 0 ]; then info "nothing left for $CLUSTER_NAME in $ACCOUNT/$AWS_REGION"; fi
exit "$left"
