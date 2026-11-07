#!/usr/bin/env bash
# Suspend and resume one counter actor per sandbox class, through
# `kubectl ate proxy actor` (a localhost-only forward through atenet-router).
# Checks that every counter continues after the resume, and lists the
# actor's snapshot in S3. Exits non-zero on any failure.
#
#   demos/counter/verify.sh [--quick]
#
# Run it after deploy.sh. A suspend saves the actor to a snapshot and frees
# its worker; the next request resumes it from that snapshot, possibly on
# another worker. --quick skips the wait for the snapshot's copy in S3
# (about 5 minutes per actor); the installer uses it.
#
# Learn more:
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/concepts/architecture.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/reference/kubectl-ate.md
set -euo pipefail
QUICK=0
case "${1:-}" in
  --quick) QUICK=1 ;;
  "") ;;
  *) echo "usage: $0 [--quick]" >&2; exit 2 ;;
esac
. "$(dirname "$0")/../../lib/common.sh"
PIDS=()
stop_proxies() { local p; for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap stop_proxies EXIT

state() { kubectl ate get actor "$1" -a "$2" -o json 2>/dev/null | jq -r '.status.state'; }
count() { sed -n 's/.*preserved memory count: \([0-9]*\).*/\1/p' <<<"$1"; }

roundtrip() { # actor atespace template localport
  local actor=$1 ns=$2 tpl=$3 port=$4 a b c uri objects
  log "$actor ($ns)"
  kubectl ate get actor "$actor" -a "$ns" >/dev/null 2>&1 \
    || kubectl ate create actor "$actor" -a "$ns" --template "$tpl" >/dev/null 2>&1
  # A demo that reinstalls Substrate or deletes worker pods crashes a running
  # actor. Revert it to its last snapshot; the check below starts from there.
  if [ "$(state "$actor" "$ns")" = ACTOR_STATE_CRASHED ]; then
    kubectl ate revert actor "$actor" -a "$ns" >/dev/null 2>&1 || die "$actor is CRASHED and does not revert"
    info "reverted $actor from CRASHED"
  fi
  kubectl ate proxy actor "$actor" -a "$ns" "$port:80" >"$WORK/proxy-$actor.log" 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 60); do curl -sf -m 10 "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 2; done
  # A failed request leaves its count empty; the checks below report it.
  a=$(curl -s -m 30 "http://127.0.0.1:$port/") || a=""; info "$a"
  b=$(curl -s -m 30 "http://127.0.0.1:$port/") || b=""; info "$b"
  kubectl ate suspend actor "$actor" -a "$ns" >/dev/null 2>&1
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_SUSPENDED ] || die "$actor did not suspend"
  info "suspended"
  c=$(curl -s -m 120 "http://127.0.0.1:$port/") || c=""; info "$c"
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_RUNNING ] || die "$actor did not resume"
  [ "$(count "$c")" -eq $(( $(count "$b") + 1 )) ] || die "$actor lost its in-memory count across suspend/resume"
  if [ "$QUICK" = 1 ]; then
    info "PASS: resumed and every counter continued (S3 copy not checked: --quick)"
    return
  fi
  # The resume can use the node's local copy while the S3 upload of the same
  # snapshot finishes in the background (sometimes minutes later); wait for
  # the durable copy.
  local snap="" st=""
  for _ in $(seq 1 120); do
    snap=$(kubectl ate get actor "$actor" -a "$ns" -o json 2>/dev/null \
      | jq -c '[.status.snapshots[]? | select(.owner == "SNAPSHOT_OWNER_ACTOR")] | max_by(.generation) // empty') || snap=""
    st=$(jq -r '.durableSnapshot.state // empty' <<<"$snap")
    [ "$st" = SNAPSHOT_STORAGE_STATE_COMPLETED ] && break
    sleep 5
  done
  [ "$st" = SNAPSHOT_STORAGE_STATE_COMPLETED ] || die "$actor's snapshot did not reach S3 (state: ${st:-none})"
  uri=$(jq -r '.durableSnapshot.object.snapshotUri' <<<"$snap")
  objects=$(aws_ s3 ls "$uri/" --recursive) || die "no objects under $uri"
  info "snapshot in S3: $(wc -l <<<"$objects") objects under ${uri#s3://"$SNAP_BUCKET"/}"
  info "PASS: resumed and every counter continued"
}

roundtrip demo-gvisor ate-demo-counter counter 8001
roundtrip demo-microvm ate-demo-counter-microvm counter-microvm 8002
# Mode 3 (demos/mode3): the same round trip with gVisor inside a Kata microVM
# Pod, after checking every worker Pod of the pool runs under RuntimeClass kata.
if [ "${MODE3:-}" = on ] && kubectl get namespace ate-demo-counter-mode3 >/dev/null 2>&1; then
  rcs=$(kubectl -n ate-demo-counter-mode3 get pods -l ate.dev/worker-transport=network \
    -o jsonpath='{range .items[*]}{.spec.runtimeClassName}{"\n"}{end}' | sort -u)
  [ "$rcs" = kata ] || die "mode 3 worker Pods run under '${rcs:-nothing}', not RuntimeClass kata"
  roundtrip demo-mode3 ate-demo-counter-mode3 counter-mode3 8003

  # Deleting an actor tears its sandbox down inside the worker's guest: the
  # worker Pod logs a TerminateWorkload call for the actor's UID.
  actor=demo-mode3-delete ns=ate-demo-counter-mode3
  log "$actor ($ns): delete tears down the sandbox in the guest"
  kubectl ate get actor "$actor" -a "$ns" >/dev/null 2>&1 \
    || kubectl ate create actor "$actor" -a "$ns" --template counter-mode3 >/dev/null 2>&1
  kubectl ate proxy actor "$actor" -a "$ns" 8004:80 >"$WORK/proxy-$actor.log" 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 60); do curl -sf -m 10 http://127.0.0.1:8004/ >/dev/null 2>&1 && break; sleep 2; done
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_RUNNING ] || die "$actor did not start"
  json=$(kubectl ate get actor "$actor" -a "$ns" -o json)
  uid=$(jq -r '.metadata.uid' <<<"$json")
  pod=$(jq -r '.status.workerAssignment.workerPod' <<<"$json")
  [ -n "$uid" ] && [ -n "$pod" ] && [ "$pod" != null ] || die "$actor has no worker Pod"
  kubectl ate delete actor "$actor" -a "$ns" >/dev/null
  found=""
  for _ in $(seq 1 30); do
    if kubectl -n "$ns" logs "$pod" --all-containers 2>/dev/null | grep 'Handle RPC' | grep 'TerminateWorkload' | grep -q "$uid"; then
      found=1; break
    fi
    sleep 2
  done
  [ -n "$found" ] || die "worker Pod $pod logged no TerminateWorkload for $actor ($uid)"
  info "PASS: the worker in Pod $pod tore down $actor's sandbox"
fi
