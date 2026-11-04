#!/usr/bin/env bash
# Suspend and resume one counter actor per sandbox class, through
# `kubectl ate proxy actor` (a localhost-only forward through atenet-router).
# Checks that every counter continues after the resume, and lists the
# actor's snapshot in S3. Exits non-zero on any failure.
set -euo pipefail
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
  kubectl ate proxy actor "$actor" -a "$ns" "$port:80" >"$WORK/proxy-$actor.log" 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 60); do curl -sf -m 10 "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 2; done
  a=$(curl -s -m 30 "http://127.0.0.1:$port/"); info "$a"
  b=$(curl -s -m 30 "http://127.0.0.1:$port/"); info "$b"
  kubectl ate suspend actor "$actor" -a "$ns" >/dev/null 2>&1
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_SUSPENDED ] || die "$actor did not suspend"
  info "suspended"
  c=$(curl -s -m 120 "http://127.0.0.1:$port/"); info "$c"
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_RUNNING ] || die "$actor did not resume"
  [ "$(count "$c")" -eq $(( $(count "$b") + 1 )) ] || die "$actor lost its in-memory count across suspend/resume"
  # The resume can use the node's local copy while the S3 upload of the same
  # snapshot finishes in the background; wait for the durable copy.
  local snap="" st=""
  for _ in $(seq 1 60); do
    snap=$(kubectl ate get actor "$actor" -a "$ns" -o json 2>/dev/null \
      | jq -c '[.status.snapshots[]? | select(.owner == "SNAPSHOT_OWNER_ACTOR")] | max_by(.generation) // empty')
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
