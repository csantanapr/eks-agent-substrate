#!/usr/bin/env bash
# Optional, preview: mode 3, a proposal for upstream. The worker Pod itself is
# a microVM (Kata Containers on Cloud Hypervisor) and runs actors with gVisor
# inside, so an escape from gVisor still lands in the guest kernel, not on
# the node. Needs MODE3=on, so deploy.sh installed the proposal's Substrate.
#
#   demos/mode3/deploy.sh        after ./deploy.sh
#
#   Kata        kata-deploy 3.32.0 (Kata's own installer), on the microvm node
#               group only, which already has nested virtualization (/dev/kvm).
#               Only the clh shim: the Go runtime with Cloud Hypervisor, the
#               VMM ateworker-microvm uses too. RuntimeClass kata.
#   counter     a gVisor WorkerPool with spec.networkTransport: its worker Pods
#               run under RuntimeClass kata, and atelet reaches them over
#               mutual TLS on the Pod network instead of the node's files
#
# clean.sh removes it. A RuntimeClass names the container runtime handler a
# Pod runs under; Kata's handler boots each Pod as a lightweight VM.
#
# Learn more:
#   https://github.com/kata-containers/kata-containers/tree/3.32.0/tools/packaging/kata-deploy
#   https://kubernetes.io/docs/concepts/containers/runtime-class/
#   https://github.com/csantanapr/substrate/tree/microvm-mode-3
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
need helm; need jq
[ "${MODE3:-}" = on ] || die "set MODE3=on in config.local.env and run ./deploy.sh first"
SRC="$SUBSTRATE_SRC/demos/counter"
[ -d "$SRC" ] || die "run substrate/fetch.sh first"
ensure_kubeconfig

KATA_VERSION=3.32.0
KATA_CHART_SHA256=901c3895d972d0f87df72f06277504acd81c48042631d3677e989db8c4c70aa6
KATA_DEPLOY_IMAGE="quay.io/kata-containers/kata-deploy@sha256:47c4cf29c9e8cf9bb7debab58761b672e1d4c93ee5e92db7b442ee00e7d561cc"
KATA_KUBECTL_IMAGE="quay.io/kata-containers/kubectl@sha256:623ab96ae28523ae72dea49d0e7a3909afab71d49a552c26f57c62950fe4bbda"
NS=ate-demo-counter-mode3
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

log "Kata Containers $KATA_VERSION on the microvm node group (kata-deploy)"
# The chart is pinned by sha256: a changed file stops the script.
chart="$WORK/kata-deploy-$KATA_VERSION.tgz"
[ -f "$chart" ] || curl -fsSL -o "$chart" \
  "https://github.com/kata-containers/kata-containers/releases/download/$KATA_VERSION/kata-deploy-$KATA_VERSION.tgz"
[ "$(sha256 "$chart")" = "$KATA_CHART_SHA256" ] || { rm -f "$chart"; die "the kata-deploy chart does not match its pinned sha256"; }
cat > "$WORK/kata-deploy-values.yaml" <<EOF
# A reference with @sha256: the chart uses it as is, without a tag.
image:
  reference: $KATA_DEPLOY_IMAGE
kubectlImage:
  reference: $KATA_KUBECTL_IMAGE
imagePullPolicy: IfNotPresent
nodeSelector: { ate.dev/pool: microvm }
tolerations:
  - { key: ate.dev/sandboxClass, operator: Equal, value: microvm, effect: NoSchedule }
snapshotter:
  setup: []
shims:
  disableAll: true
  clh:
    enabled: true
    runtimeClass:
      nodeSelector: { ate.dev/pool: microvm }
defaultShim:
  amd64: clh
runtimeClasses:
  createDefault: true
  defaultName: kata
EOF
helm upgrade --install kata-deploy "$chart" -n kube-system -f "$WORK/kata-deploy-values.yaml" --wait --timeout 10m
kubectl -n kube-system rollout status ds/kata-deploy --timeout=10m
kubectl get runtimeclass kata >/dev/null || die "no RuntimeClass kata after kata-deploy"
info "RuntimeClass kata -> handler $(kubectl get runtimeclass kata -o jsonpath='{.handler}')"

log "Counter on mode 3 (gVisor in a Kata microVM Pod)"
digest=$(aws_ ecr describe-images --repository-name "$ECR_PREFIX/counter" \
  --image-ids "imageTag=$SUBSTRATE_TAG" --query 'imageDetails[0].imageDigest' --output text)
COUNTER_IMAGE="$ECR_REGISTRY/$ECR_PREFIX/counter@$digest"
GVISOR_WORKER="$(pinned_image ateworker-gvisor)" || die "cannot resolve ateworker-gvisor:$SUBSTRATE_TAG"
info "worker ${GVISOR_WORKER##*/}"
# The gVisor counter's manifests under new names, on the microvm nodes, with
# the network transport and the kata RuntimeClass.
mode3() { # stdin: a counter manifest; prints it for mode 3
  sed -e "s|ko://go.ate.dev/substrate/demos/counter|$COUNTER_IMAGE|" \
      -e "s|ko://go.ate.dev/substrate/cmd/ateworker-gvisor|$GVISOR_WORKER|" \
      -e '/^\${[A-Z_]*}$/d' | python3 -c '
import sys, yaml
ns = sys.argv[1]
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
for d in docs:
    m = d.setdefault("metadata", {})
    if d.get("kind") == "Namespace":
        m["name"] = ns
    elif d.get("kind") == "WorkerPool":
        m.update(name="counter-mode3", namespace=ns)
        m.setdefault("labels", {})["workload"] = "counter-mode3"
        d["spec"]["replicas"] = 2
        d["spec"]["networkTransport"] = {"runtimeClassName": "kata"}
        t = d["spec"].setdefault("template", {})
        t["nodeSelector"] = {"ate.dev/pool": "microvm"}
        t["tolerations"] = [{"key": "ate.dev/sandboxClass", "operator": "Equal",
                             "value": "microvm", "effect": "NoSchedule"}]
    elif "workerSelector" in d:
        m.update(atespace=ns, name="counter-mode3")
        d["workerSelector"]["matchLabels"]["workload"] = "counter-mode3"
print(yaml.safe_dump_all(docs, sort_keys=False))' "$NS"
}
mode3 < "$SRC/counter.yaml.tmpl" | kubectl apply -f -
if ! err=$(kubectl ate create atespace "$NS" 2>&1); then
  grep -qi 'already exists' <<<"$err" || die "create atespace $NS: $err"
fi
if have=$(kubectl ate get actor-template counter-mode3 -a "$NS" -o json 2>/dev/null); then
  have=$(jq -r '(.spec // .).containers[0].image' <<<"$have")
  [ "$have" = "$COUNTER_IMAGE" ] || die "$NS/counter-mode3 runs $have, not $COUNTER_IMAGE; run ./clean.sh to replace it"
else
  mode3 < "$SRC/counter-template.yaml.tmpl" | kubectl ate create actor-template -f -
fi
for _ in $(seq 1 90); do
  out=$(kubectl ate get actor-template counter-mode3 -a "$NS" -o json 2>/dev/null || true)
  jq -e '.status.goldenSnapshotStatus.goldenTag.name // empty' <<<"$out" >/dev/null 2>&1 && break
  err=$(jq -r '.status.goldenSnapshotStatus.errorMessage // empty' <<<"$out" 2>/dev/null || true)
  [ -n "$err" ] && info "golden: $err"
  sleep 10
done
jq -e '.status.goldenSnapshotStatus.goldenTag.name // empty' <<<"$out" >/dev/null 2>&1 \
  || die "golden snapshot not ready for $NS/counter-mode3"
info "golden snapshot ready for $NS/counter-mode3"
kubectl -n "$NS" get pods -o custom-columns='POD:.metadata.name,RUNTIME:.spec.runtimeClassName,NODE:.spec.nodeName'
info "next: demos/counter/verify.sh (it checks mode 3 too when MODE3=on)"
