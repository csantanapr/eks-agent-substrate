#!/usr/bin/env bash
# Proves egress credential injection from AWS Secrets Manager end to end,
# without printing the secret (values are compared by SHA-256):
#   1. an actor's HTTPS request with a placeholder header reaches a
#      header-echo endpoint with the Secrets Manager value in that header
#   2. the actor holds only the placeholder: the value is in neither its
#      environment, its template nor its EgressPolicy
#   3. a secret without the ate.dev/injectable tag is refused (403): IAM
#      denies the provider's read
#   4. an atespace the provider policy does not list is refused (403)
#   5. the provider logs the actor and the secret name, never the value
#
#   demos/credprovider-aws/verify.sh          after demos/credprovider-aws/deploy.sh
#
# Each run makes fresh probe actors and deletes them when it passes. The echo
# endpoint sends the received headers back in its response body, so in this
# test the actor does see the value in that one response; an ordinary API
# never echoes a credential. Exits non-zero on any failure.
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
. "$ROOT/demos/credprovider-aws/lib.sh"
ensure_kubeconfig
need jq
step() { phase "$1"; log "$1"; }
ACTOR=probe-allowed
DENIED_ACTOR=probe-denied
PLACEHOLDER="Bearer placeholder"
ECHO_HOST=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.urlsplit(sys.argv[1]).hostname)' "$CP_ECHO_URL")
PIDS=()
# The value, for grep -F -f only: a 0600 file under .work, removed on exit.
secret_file="$(umask 077; mktemp "$WORK/secret.XXXXXX")"
cleanup() { local p; for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; rm -f "$secret_file"; }
trap cleanup EXIT
aws_ secretsmanager get-secret-value --secret-id "$CP_SECRET" --query SecretString --output text \
  | tr -d '\n' > "$secret_file"
[ -s "$secret_file" ] || die "cannot read $CP_SECRET; run demos/credprovider-aws/deploy.sh"
# holds_secret: 0 if stdin contains the value.
holds_secret() { grep -qF -f "$secret_file"; }

# fresh_actor ATESPACE NAME POLICY_YAML PORT: a new actor from the probe
# template with this EgressPolicy, reached through `kubectl ate proxy`.
fresh_actor() {
  local ns=$1 name=$2 policy=$3 port=$4
  if kubectl ate get actor "$name" -a "$ns" >/dev/null 2>&1; then
    kubectl ate delete actor "$name" -a "$ns" >/dev/null
  fi
  kubectl ate create actor "$name" -a "$ns" --template "$CP_TEMPLATE" >/dev/null
  # Created before the first request, so the actor starts with it. `get
  # egress-policy` exits 0 when there is none and prints nothing on stdout.
  if [ -n "$(kubectl ate get egress-policy "$name" -a "$ns" -o json 2>/dev/null)" ]; then
    kubectl ate delete egress-policy "$name" -a "$ns" >/dev/null
  fi
  kubectl ate create egress-policy "$name" -a "$ns" -f - <<<"$policy" >/dev/null
  (exec kubectl-ate proxy actor "$name" -a "$ns" "$port:80" >"$WORK/proxy-$name.log" 2>&1) &
  PIDS+=("$!")
  # The first request resumes the actor.
  for _ in $(seq 1 60); do curl -sf -m 10 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && return 0; sleep 2; done
  die "$ns/$name did not answer through the proxy (see $WORK/proxy-$name.log)"
}
# fetch PORT HEADER: the probe's GET of the echo endpoint with one header,
# as the probe's JSON ({status, body, error}). Retries a 503 or a certificate
# error for up to two minutes: the gateway reconnecting to a new provider, or
# its CA reaching the actor.
fetch() {
  local port=$1 header=$2 out st err
  for _ in $(seq 1 24); do
    out=$(curl -sG -m 30 "http://127.0.0.1:$port/fetch" --data-urlencode "url=$CP_ECHO_URL" \
      --data-urlencode "header=$header")
    st=$(jq -r '.status // ""' <<<"$out"); err=$(jq -r '.error // ""' <<<"$out")
    if [ "$st" != 503 ] && ! grep -qiE 'certificate|x509' <<<"$err"; then printf '%s' "$out"; return 0; fi
    info "status ${st:-none} ${err:+($err)}; retrying"
    sleep 5
  done
  printf '%s' "$out"
}
# echoed HEADER: from the probe's JSON on stdin, the value the echo endpoint
# received in HEADER (header names compared case-insensitively).
echoed() {
  jq -r '.body' | jq -r --arg h "${1,,}" '.headers | to_entries[] | select(.key | ascii_downcase == $h) | .value'
}
https_rule() { # header=uri ... : one https rule for the echo host replacing each header
  local h u
  printf 'rules:\n- https:\n    hostnames: ["%s"]\n    effects:\n      replaceHeaders:\n' "$ECHO_HOST"
  for kv in "$@"; do
    h=${kv%%=*}; u=${kv#*=}
    printf '      - header: %s\n' "$h"
    [ "$h" = Authorization ] && printf '        prefix: "Bearer "\n'
    printf '        credentialUri: %s\n' "$u"
  done
}

step "Provider and gateway"
kubectl -n "$CP_NS" rollout status "deploy/$CP_SA" --timeout=2m >/dev/null
cp_gateway_serves || die "atenet-egress does not serve $CP_PROVIDER_NAME; run demos/credprovider-aws/deploy.sh"
info "atenet-egress serves k8s.io and $CP_PROVIDER_NAME ($CP_PROVIDER_ADDRESS)"
info "echo endpoint $CP_ECHO_URL"

step "1. The upstream receives the Secrets Manager value"
fresh_actor "$CP_ATESPACE" "$ACTOR" \
  "$(https_rule "Authorization=$(cp_uri "$CP_SECRET")" "X-Untagged-Token=$(cp_uri "$CP_UNTAGGED_SECRET")")" 8021
t0=$SECONDS
out=$(fetch 8021 "Authorization:$PLACEHOLDER")
st=$(jq -r '.status // ""' <<<"$out")
[ "$st" = 200 ] || die "the injected request got status ${st:-none} ($(jq -r '.error // ""' <<<"$out"))"
got=$(echoed authorization <<<"$out") || die "the echo endpoint did not answer with JSON headers"
[ -n "$got" ] || die "the echo endpoint received no Authorization header"
[ "$got" != "$PLACEHOLDER" ] || die "the echo endpoint received the placeholder: nothing was injected"
[ "${got#Bearer }" != "$got" ] || die "the echoed Authorization header lacks the Bearer prefix"
want_sha=$(sha256 < "$secret_file")
got_sha=$(printf '%s' "${got#Bearer }" | sha256)
unset got
[ "$got_sha" = "$want_sha" ] || die "the echoed value (sha256 ${got_sha:0:12}) is not $CP_SECRET (sha256 ${want_sha:0:12})"
info "sent:     Authorization: $PLACEHOLDER"
info "received: Authorization: Bearer <sha256 ${got_sha:0:12}...>, the value of $CP_SECRET (sha256 ${want_sha:0:12}...)"
info "PASS in $(fmt_secs $((SECONDS - t0)))"

step "2. The actor holds only the placeholder"
env_out=$(curl -sG -m 30 "http://127.0.0.1:8021/readfile" --data-urlencode "path=/proc/self/environ")
[ -z "$(jq -r '.error // ""' <<<"$env_out")" ] || die "cannot read the actor's environment: $(jq -r .error <<<"$env_out")"
# Captured first: a pipe into grep -q could end early and hide a match.
actor_env=$(jq -r '.content' <<<"$env_out" | tr '\0' '\n')
holds_secret <<<"$actor_env" && die "the actor's environment holds the value"
template=$(kubectl ate get actor-template "$CP_TEMPLATE" -a "$CP_ATESPACE" -o json)
holds_secret <<<"$template" && die "the template holds the value"
policy=$(kubectl ate get egress-policy "$ACTOR" -a "$CP_ATESPACE" -o json)
holds_secret <<<"$policy" && die "the EgressPolicy holds the value"
grep -qF "$(cp_uri "$CP_SECRET")" <<<"$policy" || die "the EgressPolicy does not name $CP_SECRET"
info "environment, template and EgressPolicy: no value; the policy names only the URI"
info "PASS"

step "3. A secret without the $CP_INJECTABLE_TAG tag is refused"
out=$(fetch 8021 "X-Untagged-Token:placeholder")
st=$(jq -r '.status // ""' <<<"$out")
[ "$st" = 403 ] || die "the untagged secret got status ${st:-none}, want 403 ($(jq -r '.error // ""' <<<"$out"))"
info "$CP_UNTAGGED_SECRET: 403 (the provider policy allows it; its IAM role does not)"
info "PASS"

step "4. An atespace the provider policy does not list is refused"
fresh_actor "$CP_DENIED_ATESPACE" "$DENIED_ACTOR" "$(https_rule "Authorization=$(cp_uri "$CP_SECRET")")" 8022
out=$(fetch 8022 "Authorization:$PLACEHOLDER")
st=$(jq -r '.status // ""' <<<"$out")
[ "$st" = 403 ] || die "$CP_DENIED_ATESPACE got status ${st:-none} for $CP_SECRET, want 403"
info "$CP_DENIED_ATESPACE/$DENIED_ACTOR -> $CP_SECRET: 403"
info "PASS"

step "5. The provider logs the actor and the secret, never the value"
logs=$(kubectl -n "$CP_NS" logs -l "app=$CP_SA" --tail=-1 --max-log-requests=10)
actor_id="spiffe://substrate-actor.local/actor/$CP_ATESPACE/$ACTOR"
denied_id="spiffe://substrate-actor.local/actor/$CP_DENIED_ATESPACE/$DENIED_ACTOR"
line() { # msg actor secret: the provider logged msg for this actor and secret
  jq -Rce --arg m "$1" --arg a "$2" --arg s ":secret:$3 " \
    'fromjson? | select(.msg == $m and .actor == $a and ((.secret // "") | contains($s)))' <<<"$logs" >/dev/null
}
line "resolving credential" "$actor_id" "$CP_SECRET" || die "no 'resolving credential' log for $actor_id and $CP_SECRET"
line "credential request failed" "$actor_id" "$CP_UNTAGGED_SECRET" || die "no 'credential request failed' log for $CP_UNTAGGED_SECRET"
line "credential request denied: atespace not granted the secret" "$denied_id" "$CP_SECRET" \
  || die "no denial log for $denied_id"
holds_secret <<<"$logs" && die "the provider's logs hold the value"
if gw=$(kubectl -n ate-system logs -l app=atenet-egress -c ext-proc --tail=-1 --max-log-requests=10 2>/dev/null); then
  holds_secret <<<"$gw" && die "the egress gateway's logs hold the value"
  info "egress gateway (ext-proc) logs: no value"
fi
info "served, IAM-refused and policy-refused requests are logged with actor and secret; no value"
info "PASS"

step "Clean up the probe actors"
for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
kubectl ate delete actor "$ACTOR" -a "$CP_ATESPACE" >/dev/null
kubectl ate delete actor "$DENIED_ACTOR" -a "$CP_DENIED_ATESPACE" >/dev/null
log "PASS: injected from Secrets Manager; untagged secret, unlisted atespace refused; no value in logs"
