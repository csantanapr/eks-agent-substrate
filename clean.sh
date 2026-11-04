#!/usr/bin/env bash
# Removes everything deploy.sh created, and leaves the EKS cluster running.
# Run it before delete-cluster.sh: the RDS security group and subnet group
# live in the cluster's VPC and would block its deletion.
#
#   ./clean.sh [--yes]
#
# Deletes: the demos and their volumes, the LLM gateway, Agent Substrate, the
# Pod Identity associations and IAM roles, the RDS instance (no final
# snapshot), its subnet and security groups, the Secrets Manager secrets
# (no recovery window, so deploy.sh can recreate them at once), both S3
# buckets with their contents, and the ECR repositories with their images.
# Ends with aws/inventory.sh, and reports success only when nothing is left.
set -uo pipefail
[ "${1:-}" = --yes ] && ASSUME_YES=1
. "$(dirname "$0")/lib/common.sh"
confirm "This deletes the Agent Substrate install and its AWS data (RDS, S3) for $CLUSTER_NAME."
fail=0
step() { "$@" || { warn "failed: $*"; fail=1; }; }

if cluster_exists; then
  ensure_kubeconfig
  phase "Demos and LLM gateway"
  log "Demos and the LLM gateway"
  # Namespaces first: deleting the openclaw namespace deletes its PVC, and
  # the gp3 StorageClass deletes the EBS volume behind it.
  step kubectl delete namespace openclaw ate-demo-counter ate-demo-counter-microvm llm-gateway ate-db-bootstrap \
    --ignore-not-found --timeout=10m
  step kubectl delete clusterrole,clusterrolebinding openclaw-gateway-eks openclaw-dashboard --ignore-not-found

  phase "Agent Substrate uninstall"
  log "Agent Substrate"
  # Remove the release that is installed, with that release's installer.
  if [ -f "$INSTALLED_TAG_FILE" ]; then
    SUBSTRATE_TAG="$(cat "$INSTALLED_TAG_FILE")"
    SUBSTRATE_SRC="$WORK/substrate-$SUBSTRATE_TAG"
    info "installed release: $SUBSTRATE_TAG"
    step env SUBSTRATE_TAG="$SUBSTRATE_TAG" bash "$ROOT/substrate/fetch.sh" >/dev/null
  fi
  if kubectl get namespace ate-system >/dev/null 2>&1 || kubectl get crd -o name | grep -q '\.ate\.dev$'; then
    if [ -x "$WORK/bin/ate-setup" ] && [ -d "$SUBSTRATE_SRC" ]; then
      (cd "$SUBSTRATE_SRC" && NO_DEV_ENV=1 ATE_INSTALL_STORAGE_BACKEND=s3 \
        "$WORK/bin/ate-setup" delete ate-system --context "$(kubectl config current-context)") \
        || { warn "ate-setup delete ate-system failed"; fail=1; }
    else
      # Without the installer the cluster-scoped objects (CRDs, roles,
      # webhooks) cannot all be found, so this does not count as clean.
      warn "no ate-setup for $SUBSTRATE_TAG in .work; run substrate/fetch.sh with that tag and ./clean.sh again"
      fail=1
    fi
  else
    info "not installed"
  fi

  phase "Pod Identity associations"
  log "Pod Identity associations"
  for pair in $POD_IDENTITY_SAS; do
    if assoc=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" \
        --namespace "${pair%/*}" --service-account "${pair#*/}" --query 'associations[].associationId' --output text); then
      for id in $assoc; do step aws_ eks delete-pod-identity-association --cluster-name "$CLUSTER_NAME" --association-id "$id" >/dev/null; done
      info "$pair"
    else
      warn "cannot list the Pod Identity associations of $pair"; fail=1
    fi
  done
else
  warn "cluster $CLUSTER_NAME not found; cleaning the AWS resources only"
fi

phase "IAM roles"
log "IAM roles"
for pair in $POD_IDENTITY_SAS; do
  role="${IAM_PREFIX}-${pair#*/}"
  owned_role "$role" || continue
  if policies=$(aws_ iam list-role-policies --role-name "$role" --query 'PolicyNames[]' --output text); then
    for p in $policies; do step aws_ iam delete-role-policy --role-name "$role" --policy-name "$p"; done
    step aws_ iam delete-role --role-name "$role"
    info "$role"
  else
    warn "cannot list the policies of $role"; fail=1
  fi
done

phase "Amazon RDS delete"
log "Amazon RDS"
if aws_exists rds describe-db-instances --db-instance-identifier "$DB_ID"; then
  step aws_ rds delete-db-instance --db-instance-identifier "$DB_ID" \
    --skip-final-snapshot --delete-automated-backups >/dev/null
  info "waiting for $DB_ID to be deleted (about 5 minutes)"
  step aws_ rds wait db-instance-deleted --db-instance-identifier "$DB_ID"
fi
if aws_exists rds describe-db-subnet-groups --db-subnet-group-name "$DB_ID"; then
  step aws_ rds delete-db-subnet-group --db-subnet-group-name "$DB_ID"
fi
if sgs=$(aws_ ec2 describe-security-groups --filters "Name=group-name,Values=$DB_ID" \
    --query 'SecurityGroups[].GroupId' --output text); then
  for sg in $sgs; do step aws_ ec2 delete-security-group --group-id "$sg"; info "$sg"; done
else
  warn "cannot list the security groups named $DB_ID"; fail=1
fi

phase "Secrets, S3, ECR"
log "Secrets Manager"
for who in owner readwrite; do
  name="$SECRET_PREFIX/ate-$who"
  aws_exists secretsmanager describe-secret --secret-id "$name" || continue
  step aws_ secretsmanager delete-secret --secret-id "$name" --force-delete-without-recovery >/dev/null
  # The delete finishes in the background; wait so deploy.sh can reuse the name.
  for _ in $(seq 1 30); do aws_exists secretsmanager describe-secret --secret-id "$name" || break; sleep 2; done
  info "$name"
done

log "S3 buckets"
for b in "$SNAP_BUCKET" "$ASSET_BUCKET"; do
  owned_bucket "$b" || continue
  step aws_ s3 rb "s3://$b" --force >/dev/null
  info "$b"
done

log "ECR repositories"
if repos=$(aws_ ecr describe-repositories --query "repositories[?starts_with(repositoryName, \`$ECR_PREFIX/\`)].repositoryName" --output text); then
  for repo in $repos; do step aws_ ecr delete-repository --repository-name "$repo" --force >/dev/null; info "$repo"; done
else
  warn "cannot list the ECR repositories"; fail=1
fi

phase "Inventory"
log "What is left"
bash "$ROOT/aws/inventory.sh" || fail=1
[ "$fail" = 0 ] || die "some steps failed or resources are left (see above); fix and run ./clean.sh again"
rm -f "$INSTALLED_TAG_FILE"
log "Clean. The cluster is still running; ./delete-cluster.sh removes it."
