#!/usr/bin/env bash
# Removes what demos/credprovider-aws/deploy.sh created. Called by clean.sh;
# safe to run when it was never deployed.
#
#   demos/credprovider-aws/clean.sh [--yes] [--keep-gateway]
#
# Deletes, in this order: the provider's registration, then reinstalls
# Substrate so the egress gateway serves k8s.io only (--keep-gateway skips the
# reinstall: clean.sh removes Substrate next); the probe actors, templates,
# atespaces and worker pool; the provider and its policy; the Pod Identity
# association and IAM role; both demo secrets (no recovery window, so
# deploy.sh can recreate them at once); both ECR repositories.
set -uo pipefail
KEEP_GATEWAY=0
for a in "$@"; do
  case "$a" in
    --yes) export ASSUME_YES=1 ;;
    --keep-gateway) KEEP_GATEWAY=1 ;;
    *) echo "usage: $0 [--yes] [--keep-gateway]" >&2; exit 2 ;;
  esac
done
. "$(dirname "$0")/../../lib/common.sh"
. "$ROOT/demos/credprovider-aws/lib.sh"
confirm "This removes the Secrets Manager credential provider demo and its secrets for $CLUSTER_NAME."
fail=0
step() { "$@" || { warn "failed: $*"; fail=1; }; }

# First, so the next substrate/install.sh (here or in ./deploy.sh) no longer
# points the gateway at the provider.
phase "Egress gateway: unregister the provider"
log "Egress gateway"
rm -f "$CP_REGISTRATION"
info "unregistered (${CP_REGISTRATION#"$ROOT"/})"

if cluster_exists; then
  ensure_kubeconfig
  # Before the provider goes: a gateway that still names it fails every
  # fetch for its URIs.
  if cp_gateway_serves; then
    if [ "$KEEP_GATEWAY" = 1 ]; then
      info "atenet-egress still names $CP_PROVIDER_NAME until Substrate is removed"
    else
      log "Reinstall Substrate: atenet-egress serves k8s.io only"
      step cp_reinstall
      cp_gateway_serves && { warn "atenet-egress still serves $CP_PROVIDER_NAME"; fail=1; }
    fi
  fi

  phase "Probe actors, templates, atespaces"
  log "Probe actors and templates"
  for atespace in "$CP_ATESPACE" "$CP_DENIED_ATESPACE"; do
    kubectl ate get atespace "$atespace" >/dev/null 2>&1 || continue
    for a in $(kubectl ate get actors -a "$atespace" -o json 2>/dev/null | jq -r '.actors[]?.metadata.name'); do
      step kubectl ate delete actor "$a" -a "$atespace"
    done
    if kubectl ate get actor-template "$CP_TEMPLATE" -a "$atespace" >/dev/null 2>&1; then
      step kubectl ate delete actor-template "$CP_TEMPLATE" -a "$atespace"
    fi
    # Refused while any actor remains; deleted actors can take a moment to go.
    for _ in $(seq 1 24); do kubectl ate delete atespace "$atespace" >/dev/null 2>&1 && break; sleep 5; done
    if kubectl ate get atespace "$atespace" >/dev/null 2>&1; then
      warn "could not delete atespace $atespace"; fail=1
    fi
    info "$atespace"
  done
  step kubectl delete namespace "$CP_POOL_NS" --ignore-not-found --timeout=5m

  phase "Provider"
  log "aws-sm-credential-provider"
  step kubectl -n "$CP_NS" delete deployment,service,networkpolicy,poddisruptionbudget,serviceaccount "$CP_SA" --ignore-not-found
  step kubectl -n "$CP_NS" delete configmap "$CP_POLICY_CM" --ignore-not-found

  log "Pod Identity association"
  if ids=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" --namespace "$CP_NS" \
      --service-account "$CP_SA" --query 'associations[].associationId' --output text); then
    for id in $ids; do step aws_ eks delete-pod-identity-association --cluster-name "$CLUSTER_NAME" --association-id "$id" >/dev/null; done
    info "$CP_NS/$CP_SA"
  else
    warn "cannot list the Pod Identity associations of $CP_NS/$CP_SA"; fail=1
  fi
fi

phase "IAM, secrets, ECR"
log "IAM role"
if owned_role "$CP_ROLE"; then
  if policies=$(aws_ iam list-role-policies --role-name "$CP_ROLE" --query 'PolicyNames[]' --output text); then
    for p in $policies; do step aws_ iam delete-role-policy --role-name "$CP_ROLE" --policy-name "$p"; done
    step aws_ iam delete-role --role-name "$CP_ROLE"
    info "$CP_ROLE"
  else
    warn "cannot list the policies of $CP_ROLE"; fail=1
  fi
fi

log "Secrets Manager"
for name in "$CP_SECRET" "$CP_UNTAGGED_SECRET" "${CP_LEGACY_SECRETS[@]}"; do
  # The names carry the atespace but not the cluster, so another cluster in
  # this account and Region can own a secret with the same name. Leave it.
  owner=$(secret_owner "$name"); rc=$?
  [ "$rc" = 1 ] && continue
  if [ "$rc" != 0 ]; then warn "cannot read the tags of secret $name"; fail=1; continue; fi
  if [ "$owner" != "$OWNER_TAG_VALUE" ]; then
    info "$name belongs to $owner, not $OWNER_TAG_VALUE: left in place"
    continue
  fi
  step aws_ secretsmanager delete-secret --secret-id "$name" --force-delete-without-recovery >/dev/null
  # The delete finishes in the background; wait so deploy.sh can reuse the name.
  for _ in $(seq 1 30); do aws_exists secretsmanager describe-secret --secret-id "$name" || break; sleep 2; done
  info "$name"
done

log "ECR repositories"
for repo in "$CP_REPO" "$CP_PROBE_REPO"; do
  aws_exists ecr describe-repositories --repository-names "$repo" || continue
  step aws_ ecr delete-repository --repository-name "$repo" --force >/dev/null
  info "$repo"
done

[ "$fail" = 0 ] || die "some credential provider demo steps failed (see WARN lines); fix and run again"
log "Secrets Manager credential provider demo removed"
