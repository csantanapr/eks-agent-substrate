#!/usr/bin/env bash
# Removes what demos/db-rotation/deploy.sh created. Called by clean.sh; safe
# to run when it was never deployed.
#
#   demos/db-rotation/clean.sh [--yes]
#
# Deletes: the ESO Reloader and its Config, the ExternalSecret and SecretStore,
# External Secrets Operator, its Pod Identity association and IAM role, the
# rotation CloudFormation stack (functions and schedules), and the rotation
# security group with its rule on the RDS security group. The login secrets,
# the Kubernetes Secret ate-api-server-secret-envvars and the database users
# stay: ate-api-server keeps working with the current logins.
set -uo pipefail
[ "${1:-}" = --yes ] && export ASSUME_YES=1
. "$(dirname "$0")/../../lib/common.sh"
confirm "This removes Secrets Manager rotation and External Secrets for $CLUSTER_NAME."
fail=0
step() { "$@" || { warn "failed: $*"; fail=1; }; }
ESO_NS=external-secrets
RELOADER_NS=external-secrets-reloader
vpc=""

if cluster_exists; then
  ensure_kubeconfig
  vpc=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.vpcId --output text)
  log "ESO Reloader"
  if kubectl get crd configs.reloader.external-secrets.io >/dev/null 2>&1; then
    step kubectl delete configs.reloader.external-secrets.io ate-api-server-db --ignore-not-found
    step kubectl delete crd configs.reloader.external-secrets.io --ignore-not-found
  fi
  step kubectl delete clusterrolebinding,clusterrole reloader-controller --ignore-not-found
  step kubectl delete namespace "$RELOADER_NS" --ignore-not-found --timeout=5m

  log "External Secrets Operator"
  # deletionPolicy Retain: the Secret ate-api-server-secret-envvars stays.
  if kubectl get crd externalsecrets.external-secrets.io >/dev/null 2>&1; then
    step kubectl -n ate-system delete externalsecret ate-api-server-db --ignore-not-found
    step kubectl -n ate-system delete secretstore aws-secrets-manager --ignore-not-found
  fi
  if helm status external-secrets -n "$ESO_NS" >/dev/null 2>&1; then
    step helm uninstall external-secrets -n "$ESO_NS" --wait >/dev/null
  fi
  step kubectl delete namespace "$ESO_NS" --ignore-not-found --timeout=5m
  if ids=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" --namespace "$ESO_NS" \
      --service-account external-secrets --query 'associations[].associationId' --output text); then
    for id in $ids; do step aws_ eks delete-pod-identity-association --cluster-name "$CLUSTER_NAME" --association-id "$id" >/dev/null; done
  else
    fail=1
  fi
fi

log "IAM role"
role="${IAM_PREFIX}-external-secrets"
if owned_role "$role"; then
  if policies=$(aws_ iam list-role-policies --role-name "$role" --query 'PolicyNames[]' --output text); then
    for p in $policies; do step aws_ iam delete-role-policy --role-name "$role" --policy-name "$p"; done
    step aws_ iam delete-role --role-name "$role"
    info "$role"
  else
    fail=1
  fi
fi

log "Rotation functions and schedules (CloudFormation $ROTATION_STACK)"
if aws_exists cloudformation describe-stacks --stack-name "$ROTATION_STACK"; then
  step aws_ cloudformation delete-stack --stack-name "$ROTATION_STACK"
  step aws_ cloudformation wait stack-delete-complete --stack-name "$ROTATION_STACK"
  info "$ROTATION_STACK"
fi
for who in owner readwrite; do
  name="$SECRET_PREFIX/ate-$who"
  aws_exists secretsmanager describe-secret --secret-id "$name" || continue
  if [ "$(aws_ secretsmanager describe-secret --secret-id "$name" --query RotationEnabled --output text)" = True ]; then
    step aws_ secretsmanager cancel-rotate-secret --secret-id "$name" >/dev/null
  fi
  info "$name: rotation off"
done

log "Rotation security group"
filters=("Name=group-name,Values=$ROTATION_SG_NAME")
[ -n "$vpc" ] && filters+=("Name=vpc-id,Values=$vpc")
for sg in $(aws_ ec2 describe-security-groups --filters "${filters[@]}" --query 'SecurityGroups[].GroupId' --output text); do
  sg_vpc=$(aws_ ec2 describe-security-groups --group-ids "$sg" --query 'SecurityGroups[0].VpcId' --output text)
  db_sg=$(aws_ ec2 describe-security-groups --filters "Name=vpc-id,Values=$sg_vpc" "Name=group-name,Values=$DB_ID" \
    --query 'SecurityGroups[0].GroupId' --output text)
  if [[ "$db_sg" == sg-* ]]; then
    aws_ ec2 revoke-security-group-ingress --group-id "$db_sg" \
      --ip-permissions "IpProtocol=tcp,FromPort=5432,ToPort=5432,UserIdGroupPairs=[{GroupId=$sg}]" >/dev/null 2>&1 || true
  fi
  # Lambda releases its VPC network interfaces some minutes after the
  # functions are deleted.
  for _ in $(seq 1 90); do aws_ ec2 delete-security-group --group-id "$sg" >/dev/null 2>&1 && break; sleep 10; done
  aws_exists ec2 describe-security-groups --group-ids "$sg" && { warn "could not delete $sg"; fail=1; }
  info "$sg"
done

[ "$fail" = 0 ] || die "some rotation steps failed (see WARN lines); fix and run again"
log "Rotation and External Secrets removed"
