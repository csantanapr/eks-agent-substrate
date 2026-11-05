#!/usr/bin/env bash
# Rotates both database logins now and checks the chain end to end:
# Secrets Manager switches each login to the other user, External Secrets
# writes the new connection strings, the ESO Reloader rolls ate-api-server,
# and the new pods serve the API (the counter demo) as the new users.
#
#   demos/db-rotation/verify.sh          after demos/db-rotation/deploy.sh
#
# Each run flips both logins (user <-> user_clone). Exits non-zero on any failure.
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
ensure_kubeconfig
need jq
step() { phase "$1"; log "$1"; }
ANNOTATION=reloader.external-secrets.io/last-reloaded

sm_user() { aws_ secretsmanager get-secret-value --secret-id "$SECRET_PREFIX/$1" --query SecretString --output text | jq -r .username; }
# The user in one connection string of ate-api-server-secret-envvars.
k8s_user() {
  kubectl -n ate-system get secret ate-api-server-secret-envvars -o json \
    | jq -r --arg k "$1" '.data[$k] | @base64d' \
    | python3 -c 'import sys, urllib.parse; print(urllib.parse.unquote(urllib.parse.urlsplit(sys.stdin.read().strip()).username))'
}
stages() { aws_ secretsmanager describe-secret --secret-id "$SECRET_PREFIX/$1" --query VersionIdsToStages --output json; }
current_version() { stages "$1" | jq -r 'to_entries[] | select(.value | index("AWSCURRENT")) | .key'; }
pending_only() { stages "$1" | jq '[.[] | select(index("AWSPENDING") and (index("AWSCURRENT") | not))] | length'; }
reloaded() { kubectl -n ate-system get deploy ate-api-server -o json | jq -r --arg a "$ANNOTATION" '.spec.template.metadata.annotations[$a] // ""'; }
# Pods that are not terminating.
pods() {
  kubectl -n ate-system get pods -l app=ate-api-server -o json \
    | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name] | join(" ")'
}

step "Before"
old_owner=$(sm_user ate-owner); old_rw=$(sm_user ate-readwrite)
old_pods=$(pods); old_mark=$(reloaded)
info "owner login     $old_owner"
info "readwrite login $old_rw"
info "pods            $old_pods"

step "Rotate both logins in Secrets Manager"
t0=$SECONDS
declare -A was
for s in ate-owner ate-readwrite; do
  was[$s]=$(current_version "$s")
  aws_ secretsmanager rotate-secret --secret-id "$SECRET_PREFIX/$s" >/dev/null
done
# Done when AWSCURRENT has moved and no version is pending on its own
# (the function leaves AWSPENDING on the new current version).
for s in ate-owner ate-readwrite; do
  done=""
  for _ in $(seq 1 60); do
    [ "$(current_version "$s")" != "${was[$s]}" ] && [ "$(pending_only "$s")" = 0 ] && { done=1; break; }
    sleep 5
  done
  [ -n "$done" ] || die "$s: rotation did not finish; see /aws/lambda/$CLUSTER_NAME-db-* in CloudWatch Logs"
done
new_owner=$(sm_user ate-owner); new_rw=$(sm_user ate-readwrite)
[ "$new_owner" != "$old_owner" ] || die "ate-owner still uses $old_owner"
[ "$new_rw" != "$old_rw" ] || die "ate-readwrite still uses $old_rw"
info "owner login     $old_owner -> $new_owner"
info "readwrite login $old_rw -> $new_rw"
info "rotated in $(fmt_secs $((SECONDS - t0)))"

step "External Secrets writes the new connection strings"
t0=$SECONDS
for _ in $(seq 1 36); do
  [ "$(k8s_user ATE_API_POSTGRES_OWNER_CONNECTION_STRING)" = "$new_owner" ] \
    && [ "$(k8s_user ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING)" = "$new_rw" ] && break
  sleep 5
done
[ "$(k8s_user ATE_API_POSTGRES_OWNER_CONNECTION_STRING)" = "$new_owner" ] || die "the owner connection string was not updated"
[ "$(k8s_user ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING)" = "$new_rw" ] || die "the readwrite connection string was not updated"
keys=$(kubectl -n ate-system get secret ate-api-server-secret-envvars -o json | jq -r '.data | keys | join(" ")')
[ "$keys" = "ATE_API_POSTGRES_OWNER_CONNECTION_STRING ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING ATE_API_POSTGRES_SCHEMA" ] \
  || die "unexpected keys in ate-api-server-secret-envvars: $keys"
info "ate-api-server-secret-envvars updated in $(fmt_secs $((SECONDS - t0)))"

step "ESO Reloader rolls ate-api-server"
t0=$SECONDS
for _ in $(seq 1 36); do [ "$(reloaded)" != "$old_mark" ] && break; sleep 5; done
[ "$(reloaded)" != "$old_mark" ] || die "the Reloader did not restart ate-api-server"
kubectl -n ate-system rollout status deploy/ate-api-server --timeout=5m >/dev/null
stale=x
for _ in $(seq 1 24); do
  new_pods=$(pods); stale=""
  for p in $new_pods; do grep -qw "$p" <<<"$old_pods" && stale="$stale $p"; done
  [ -z "$stale" ] && break
  sleep 5
done
[ -z "$stale" ] || die "pods from before the rotation are still running:$stale"
info "$ANNOTATION=$(reloaded)"
info "pods            $new_pods"
info "rolled out in $(fmt_secs $((SECONDS - t0)))"

step "The new pods serve the API as the new users"
for p in $new_pods; do
  ! kubectl -n ate-system logs "$p" 2>/dev/null | grep -qiE 'password authentication failed|assuming PostgreSQL role' \
    || die "$p cannot log in or assume its role"
done
"$ROOT/demos/counter/verify.sh"
info "PASS: rotation, sync, rollout and API all done"
