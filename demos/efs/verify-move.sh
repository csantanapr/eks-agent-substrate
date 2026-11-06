#!/usr/bin/env bash
# Moves an actor to another node and checks it still reads and writes its
# EFS (or S3 Files) volume there:
#   1. count on node A (the file counter lives on the actor's access point)
#   2. suspend; wait until the snapshot is in S3
#   3. cordon node A and delete the pool's worker pods on it, so the next
#      resume must land on another node
#   4. resume with a request: the actor runs on node B, its access point is
#      mounted on node B, and the file counter continues
# Node A is uncordoned at the end, also on failure.
#
#   demos/efs/verify-move.sh [gvisor|microvm|both]
#   STORAGE=s3files demos/efs/verify-move.sh
#
# Run it after demos/efs/deploy.sh. Cordon marks a node unschedulable, so new
# pods go to other nodes. The script does not drain the node: it deletes only
# this pool's worker pods there.
#
# Learn more:
#   https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/csi-volumes.md
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
ensure_kubeconfig
STORAGE="${STORAGE:-efs}"
PIDS=() CORDONED=""
cleanup() {
  local p
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  [ -z "$CORDONED" ] || kubectl uncordon "$CORDONED" >/dev/null
}
trap cleanup EXIT

actor_json() { kubectl ate get actor "$1" -a "$2" -o json 2>/dev/null; }
field() { sed -n "s/.*preserved $1 counter: \([0-9]*\).*/\1/p" <<<"$2"; }
zone() { kubectl get node "$1" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}'; }
# The access point as the node plugin on NODE sees it: the mount and the file.
on_node() { # node actor-uid
  local p
  p=$(kubectl -n kube-system get pods -l app=efs-csi-node --field-selector "spec.nodeName=$1" -o name)
  kubectl -n kube-system exec "$p" -c efs-plugin -- sh -c \
    "grep ' /var/lib/ate/actors/$2/volumes/data ' /proc/mounts | cut -d' ' -f1-3 | sed 's/^/   mount: /'; \
     printf '   a.txt: '; cat /var/lib/ate/actors/$2/volumes/data/a.txt; echo"
}

move() { # class
  local ns=ate-demo-counter workload=counter port=8021 actor=move-$STORAGE-$1
  [ "$1" = microvm ] && { ns=ate-demo-counter-microvm; workload=counter-microvm; port=8022; }
  local a b c node_a node_b uid st proxy
  log "$actor ($ns)"
  actor_json "$actor" "$ns" >/dev/null || kubectl ate --preview create actor "$actor" -a "$ns" --template "counter-$STORAGE" >/dev/null
  (exec kubectl-ate proxy actor "$actor" -a "$ns" "$port:80" >"$WORK/proxy-$actor.log" 2>&1) &
  proxy=$!; PIDS+=("$proxy")
  for _ in $(seq 1 60); do curl -sf -m 10 "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 2; done
  a=$(curl -s -m 30 "http://127.0.0.1:$port/"); info "$a"
  b=$(curl -s -m 30 "http://127.0.0.1:$port/"); info "$b"
  uid=$(actor_json "$actor" "$ns" | jq -r .metadata.uid)
  node_a=$(actor_json "$actor" "$ns" | jq -r .status.assignedNode)
  info "node A: $node_a ($(zone "$node_a"))"
  on_node "$node_a" "$uid"

  kubectl ate suspend actor "$actor" -a "$ns" >/dev/null
  # atelet uploads in the background, sometimes minutes after the suspend.
  for _ in $(seq 1 120); do
    st=$(actor_json "$actor" "$ns" | jq -r '[.status.snapshots[]? | select(.owner == "SNAPSHOT_OWNER_ACTOR")]
      | max_by(.generation) | .durableSnapshot.state // empty')
    [ "$st" = SNAPSHOT_STORAGE_STATE_COMPLETED ] && break
    sleep 5
  done
  [ "$st" = SNAPSHOT_STORAGE_STATE_COMPLETED ] || die "$actor's snapshot did not reach S3"
  info "suspended; snapshot in S3"

  kubectl cordon "$node_a" >/dev/null; CORDONED=$node_a
  kubectl -n "$ns" delete pod -l "ate.dev/worker-pool=$workload" --field-selector "spec.nodeName=$node_a" --wait=true >/dev/null
  kubectl -n "$ns" wait workerpool "$workload" --for=jsonpath='{.status.readyReplicas}'="$(kubectl -n "$ns" get workerpool "$workload" -o jsonpath='{.spec.replicas}')" --timeout=5m >/dev/null
  info "node A cordoned; its $workload workers moved: $(kubectl -n "$ns" get pods -l "ate.dev/worker-pool=$workload" -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort | uniq -c | tr '\n' ' ')"

  c=$(curl -s -m 180 "http://127.0.0.1:$port/"); info "$c"
  node_b=$(actor_json "$actor" "$ns" | jq -r .status.assignedNode)
  info "node B: $node_b ($(zone "$node_b"))"
  [ "$node_b" != "$node_a" ] || die "$actor resumed on node A again"
  on_node "$node_b" "$uid"
  [ "$(field file "$c")" -eq $(( $(field file "$b") + 1 )) ] || die "$actor lost its $STORAGE file counter on node B"
  info "PASS: resumed on another node; the file counter on $STORAGE continued from node A"

  kubectl uncordon "$node_a" >/dev/null; CORDONED=""
  kill "$proxy" 2>/dev/null || true
  kubectl ate delete actor "$actor" -a "$ns" >/dev/null
}

case "${1:-both}" in
  gvisor|microvm) move "$1" ;;
  both) move gvisor; move microvm ;;
  *) die "usage: $0 [gvisor|microvm|both]" ;;
esac
