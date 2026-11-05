#!/usr/bin/env bash
# Deletes the EKS cluster with eksctl (node groups, add-ons, VPC) and the
# microvm launch template. Run ./clean.sh first.
#
#   ./delete-cluster.sh [--yes]
#
# The last entry point. It refuses to start while aws/inventory.sh still finds
# something deploy.sh made, and it ends with aws/inventory.sh --all.
#
# Learn more:
#   https://docs.aws.amazon.com/eks/latest/eksctl/creating-and-managing-clusters.html
#   https://docs.aws.amazon.com/eks/latest/userguide/delete-cluster.html
set -euo pipefail
[ "${1:-}" = --yes ] && ASSUME_YES=1
. "$(dirname "$0")/lib/common.sh"
need eksctl

# The RDS instance and its groups sit in the cluster's VPC, and the rest
# would be orphaned or keep billing: everything clean.sh removes must be gone.
phase "Preflight inventory"
log "Preflight: nothing from deploy.sh is left"
if cluster_exists; then ensure_kubeconfig; fi
bash "$ROOT/aws/inventory.sh" || die "run ./clean.sh first"
confirm "This deletes the EKS cluster $CLUSTER_NAME and its VPC."

phase "eksctl: node groups, control plane, VPC"
if cluster_exists; then
  log "eksctl delete cluster $CLUSTER_NAME"
  # The control plane delete can fail with 409 "update in progress" while
  # EKS still removes the add-ons eksctl deleted just before; try again.
  # --disable-nodegroup-eviction skips Pod disruption budgets, which can
  # otherwise block node removal (the EBS CSI controller has one).
  for attempt in 1 2 3; do
    eksctl delete cluster --name "$CLUSTER_NAME" --region "$AWS_REGION" --wait --disable-nodegroup-eviction && break
    cluster_exists || break
    [ "$attempt" -lt 3 ] || die "eksctl delete cluster failed three times; see the eksctl-$CLUSTER_NAME-cluster stack events"
    warn "eksctl delete cluster failed (attempt $attempt); retrying in a minute"
    sleep 60
  done
else
  info "cluster $CLUSTER_NAME not found"
fi

phase "Launch template + final inventory"
log "Launch template $LT_NAME"
if aws_exists ec2 describe-launch-templates --launch-template-names "$LT_NAME"; then
  aws_ ec2 delete-launch-template --launch-template-name "$LT_NAME" >/dev/null
  info "deleted"
fi
rm -f "$KUBECONFIG"
bash "$ROOT/aws/inventory.sh" --all || die "resources are left after the cluster delete"
log "Cluster deleted"
