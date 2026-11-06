#!/usr/bin/env bash
# Deploys Substrate's counter demo on both sandbox classes:
#   gvisor  -> atespace ate-demo-counter,         gvisor node group
#   microvm -> atespace ate-demo-counter-microvm, microvm (nested virt) node group
# The counter image is built from the Substrate source and pushed to private
# ECR; the worker images are the published release images, pinned by digest.
#
#   demos/counter/deploy.sh [gvisor|microvm|both]
#
# deploy.sh calls it. An atespace is the isolation boundary an actor belongs
# to (not a Kubernetes namespace). A WorkerPool keeps worker pods warm for
# actors. An ActorTemplate names an actor's images; creating one captures a
# golden snapshot, which new actors restore from instead of a cold boot.
#
# Learn more:
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/reference/glossary.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/reference/sandbox-classes.md
#   https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
need docker
SRC="$SUBSTRATE_SRC/demos/counter"
[ -d "$SRC" ] || die "run substrate/fetch.sh first"
# Release worker images for SUBSTRATE_TAG, pinned to the digest the tag names now.
GVISOR_WORKER="$(pinned_image ateworker-gvisor)" || die "cannot resolve ateworker-gvisor:$SUBSTRATE_TAG"
MICROVM_WORKER="$(pinned_image ateworker-microvm)" || die "cannot resolve ateworker-microvm:$SUBSTRATE_TAG"
info "workers: ${GVISOR_WORKER##*/}, ${MICROVM_WORKER##*/}"
REPO="$ECR_REGISTRY/$ECR_PREFIX/counter"

# docker logs in to ECR with a 12-hour token from get-login-password.
log "Build and push the counter image"
build="$WORK/counter-image"
mkdir -p "$build"
(cd "$SUBSTRATE_SRC" && GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o "$build/counter" ./demos/counter)
cp "$ROOT/demos/counter/Dockerfile" "$build/Dockerfile"
aws_ ecr get-login-password | docker login --username AWS --password-stdin "$ECR_REGISTRY" >/dev/null 2>&1
docker build -q --platform linux/amd64 -t "$REPO:$SUBSTRATE_TAG" "$build" >/dev/null
docker push -q "$REPO:$SUBSTRATE_TAG" >/dev/null
digest=$(aws_ ecr describe-images --repository-name "$ECR_PREFIX/counter" \
  --image-ids "imageTag=$SUBSTRATE_TAG" --query 'imageDetails[0].imageDigest' --output text)
COUNTER_IMAGE="$REPO@$digest"
info "$ECR_PREFIX/counter@$digest"

render() {
  sed -e "s|ko://go.ate.dev/substrate/demos/counter|$COUNTER_IMAGE|" \
      -e "s|ko://go.ate.dev/substrate/cmd/ateworker-gvisor|$GVISOR_WORKER|" \
      -e "s|ko://go.ate.dev/substrate/cmd/ateworker-microvm|$MICROVM_WORKER|" \
      -e '/^\${[A-Z_]*}$/d' "$1"
}
# Pin each WorkerPool to its node group.
pin_pool() { # stdin: manifests; $1: node pool label
  python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
for d in docs:
    if d.get("kind") == "WorkerPool":
        d["spec"].setdefault("template", {})["nodeSelector"] = {"ate.dev/pool": sys.argv[1]}
print(yaml.safe_dump_all(docs))' "$1"
}
wait_golden() { # atespace template
  local out err
  for _ in $(seq 1 90); do
    out=$(kubectl ate get actor-template "$2" -a "$1" -o json 2>/dev/null || true)
    if jq -e '.status.goldenSnapshotStatus.goldenTag.name // empty' <<<"$out" >/dev/null 2>&1; then
      info "golden snapshot ready for $1/$2"; return 0
    fi
    err=$(jq -r '.status.goldenSnapshotStatus.errorMessage // empty' <<<"$out" 2>/dev/null || true)
    [ -n "$err" ] && info "golden: $err"
    sleep 10
  done
  die "golden snapshot not ready for $1/$2"
}
deploy() { # class
  local suffix="" atespace=ate-demo-counter template=counter
  if [ "$1" = microvm ]; then suffix=-microvm; atespace=ate-demo-counter-microvm; template=counter-microvm; fi
  log "Counter on $1"
  render "$SRC/counter${suffix}.yaml.tmpl" | pin_pool "$1" | kubectl apply -f -
  local err have
  if ! err=$(kubectl ate create atespace "$atespace" 2>&1); then
    grep -qi 'already exists' <<<"$err" || die "create atespace $atespace: $err"
  fi
  # Templates are immutable: create it once, and stop if an existing one runs
  # another counter image (clean.sh, then deploy.sh, replaces it).
  if have=$(kubectl ate get actor-template "$template" -a "$atespace" -o json 2>/dev/null); then
    have=$(jq -r '(.spec // .).containers[0].image' <<<"$have")
    [ "$have" = "$COUNTER_IMAGE" ] \
      || die "$atespace/$template runs $have, not $COUNTER_IMAGE; run ./clean.sh and ./deploy.sh to replace it"
  else
    render "$SRC/counter${suffix}-template.yaml.tmpl" | kubectl ate create actor-template -f -
  fi
  wait_golden "$atespace" "$template"
}

case "${1:-both}" in
  gvisor|microvm) deploy "$1" ;;
  both) deploy gvisor; deploy microvm ;;
  *) die "usage: $0 [gvisor|microvm|both]" ;;
esac
