#!/usr/bin/env bash
# For each sandbox class: create a counter actor whose data volume is its own
# Amazon EFS (or S3 Files) access point, count, suspend, resume, and check the
# file counter continued. Then delete the actor and check its access point is
# gone.
#
#   demos/efs/verify.sh [gvisor|microvm|both]
#   STORAGE=s3files demos/efs/verify.sh        after demos/s3files/deploy.sh
#
# Run it after demos/efs/deploy.sh. The file counter is written to the data
# volume, so it lives on EFS (or S3 Files), not in the snapshot.
#
# Learn more:
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/guides/csi-volumes.md
#   https://docs.aws.amazon.com/efs/latest/ug/efs-access-points.html
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
ensure_kubeconfig
STORAGE="${STORAGE:-efs}"
case "$STORAGE" in
  efs)
    fs=$(aws_ efs describe-file-systems --creation-token "$EFS_TOKEN" --query 'FileSystems[0].FileSystemId' --output text)
    aps() { aws_ efs describe-access-points --file-system-id "$fs" --query 'AccessPoints[].AccessPointId' --output text; } ;;
  s3files)
    fs=$(aws_ s3files list-file-systems --output json | jq -r --arg b "arn:$PARTITION:s3:::$S3FILES_BUCKET" \
      '[.fileSystems[] | select(.bucket == $b and .status != "deleted")][0].fileSystemId // empty')
    aps() { aws_ s3files list-access-points --file-system-id "$fs" --output json | jq -r '[.accessPoints[].accessPointId] | join(" ")'; } ;;
  *) die "STORAGE must be efs or s3files" ;;
esac
[[ "$fs" == fs-* ]] || die "no $STORAGE file system; run demos/$STORAGE/deploy.sh"
PIDS=()
stop_proxies() { local p; for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap stop_proxies EXIT

state() { kubectl ate get actor "$1" -a "$2" -o json 2>/dev/null | jq -r '.status.state'; }
field() { sed -n "s/.*preserved $1 counter: \([0-9]*\).*/\1/p" <<<"$2"; }

check() { # class
  local ns=ate-demo-counter port=8011 actor=$STORAGE-$1 before a b c volid ap
  [ "$1" = microvm ] && { ns=ate-demo-counter-microvm; port=8012; }
  log "$actor ($ns)"
  before=$(aps)
  kubectl ate get actor "$actor" -a "$ns" >/dev/null 2>&1 \
    || kubectl ate --preview create actor "$actor" -a "$ns" --template "counter-$STORAGE" >/dev/null
  (exec kubectl-ate proxy actor "$actor" -a "$ns" "$port:80" >"$WORK/proxy-$actor.log" 2>&1) &
  PIDS+=("$!")
  # The first request resumes the actor, which creates and mounts its volume.
  for _ in $(seq 1 60); do curl -sf -m 10 "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 2; done
  a=$(curl -s -m 30 "http://127.0.0.1:$port/"); info "$a"
  volid=$(kubectl ate get actor "$actor" -a "$ns" -o json | jq -r '.status.externalVolumes.volumes[0].storageVolumeId // empty')
  [ -n "$volid" ] || die "$actor has no external volume"
  # The volume ID ends with the access point ID, after "::".
  ap="${volid##*::}"
  info "volume $volid"
  grep -qw "$ap" <<<"$(aps)" || die "access point $ap is not on $fs"
  b=$(curl -s -m 30 "http://127.0.0.1:$port/"); info "$b"
  kubectl ate suspend actor "$actor" -a "$ns" >/dev/null
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_SUSPENDED ] || die "$actor did not suspend"
  info "suspended"
  c=$(curl -s -m 120 "http://127.0.0.1:$port/"); info "$c"
  [ "$(state "$actor" "$ns")" = ACTOR_STATE_RUNNING ] || die "$actor did not resume"
  [ "$(field file "$c")" -eq $(( $(field file "$b") + 1 )) ] || die "$actor lost its file counter on $STORAGE"
  info "PASS: the file counter on $STORAGE continued across suspend and resume"
  kill "${PIDS[-1]}" 2>/dev/null || true
  kubectl ate delete actor "$actor" -a "$ns" >/dev/null
  for _ in $(seq 1 30); do grep -qw "$ap" <<<"$(aps)" || break; sleep 2; done
  grep -qw "$ap" <<<"$(aps)" && die "access point $ap is still there after the actor was deleted"
  info "PASS: deleting the actor deleted access point $ap"
  [ "$(aps)" = "$before" ] || info "access points before: ${before:-none}; now: $(aps)"
}

case "${1:-both}" in
  gvisor|microvm) check "$1" ;;
  both) check gvisor; check microvm ;;
  *) die "usage: $0 [gvisor|microvm|both]" ;;
esac
