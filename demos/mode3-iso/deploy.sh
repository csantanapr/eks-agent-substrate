#!/usr/bin/env bash
# Optional, preview: mode 3 on EKS Auto Mode isolated sandboxes. Like
# demos/mode3, the worker Pod is a microVM that runs actors with gVisor
# inside; here the microVM is the RuntimeClass isolated-sandbox that EKS Auto
# Mode provides, on nodes Auto Mode launches for those Pods. No node group
# and no Kata install is involved. Needs MODE3=on and AUTO_MODE=on, so
# deploy.sh installed the proposal's Substrate on an Auto Mode cluster.
#
#   demos/mode3-iso/deploy.sh        after ./deploy.sh
#
#   NodePool ate-isolated   a copy of the built-in general-purpose NodePool
#                           that labels its nodes ate.dev/pool=isolated and
#                           taints them ate.dev/sandboxClass=isolated, so only
#                           the worker Pods and atelet-iso run there
#   atelet-iso              the atelet DaemonSet, for those nodes: Auto Mode
#                           nodes run Bottlerocket, whose ECR credential
#                           provider lives elsewhere than on Amazon Linux
#   counter-iso             a gVisor WorkerPool with spec.networkTransport and
#                           RuntimeClass isolated-sandbox
#
# clean.sh removes it. Every activation pushes the actor's image layers,
# runtime and state into the worker's guest, so the pool turns off the
# guest's default network and disk throttles (sandbox.amazonaws.com/*): with
# them on, a resume can take longer than the request that wakes the actor.
#
# Learn more:
#   https://docs.aws.amazon.com/eks/latest/userguide/automode.html
#   https://kubernetes.io/docs/concepts/containers/runtime-class/
#   https://github.com/csantanapr/substrate/tree/microvm-mode-3
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
need jq
[ "${MODE3:-}" = on ] || die "set MODE3=on in config.local.env and run ./deploy.sh first"
[ "$AUTO_MODE" = on ] || die "mode 3 on isolated sandboxes needs an EKS Auto Mode cluster (AUTO_MODE=on)"
SRC="$SUBSTRATE_SRC/demos/counter"
[ -d "$SRC" ] || die "run substrate/fetch.sh first"
ensure_kubeconfig
NS=ate-demo-counter-iso

log "RuntimeClass isolated-sandbox"
kubectl get runtimeclass isolated-sandbox >/dev/null 2>&1 \
  || die "this cluster has no RuntimeClass isolated-sandbox (EKS Auto Mode adds it where isolated sandboxes are available)"
info "handler $(kubectl get runtimeclass isolated-sandbox -o jsonpath='{.handler}')"

log "NodePool ate-isolated (a copy of general-purpose, labeled and tainted)"
kubectl get nodepool general-purpose -o json | python3 -c '
import sys, json
gp = json.load(sys.stdin)
spec = gp["spec"]
spec["template"].setdefault("metadata", {}).setdefault("labels", {})["ate.dev/pool"] = "isolated"
spec["template"]["spec"]["taints"] = [{"key": "ate.dev/sandboxClass", "value": "isolated", "effect": "NoSchedule"}]
# Keep an empty node a few minutes, so a resume rarely waits for a new one.
spec["disruption"] = {"consolidationPolicy": "WhenEmpty", "consolidateAfter": "300s", "budgets": [{"nodes": "10%"}]}
print(json.dumps({"apiVersion": gp["apiVersion"], "kind": "NodePool",
                  "metadata": {"name": "ate-isolated"}, "spec": spec}))' | kubectl apply -f -

log "atelet-iso on the isolated nodes"
# The kubelet image credential provider of a Bottlerocket node, with a config
# of its own: the node's config sets AWS_PROFILE=default, which fails inside
# a Pod with no ~/.aws. Without it the provider uses atelet's Pod Identity
# credentials, as on the other nodes.
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata: { name: atelet-iso-credential-provider, namespace: ate-system }
data:
  config.yaml: |
    apiVersion: kubelet.config.k8s.io/v1
    kind: CredentialProviderConfig
    providers:
      - name: ecr-credential-provider
        matchImages: ["*.dkr.ecr.*.amazonaws.com", "*.dkr.ecr.*.amazonaws.com.cn", "public.ecr.aws"]
        defaultCacheDuration: "12h"
        apiVersion: credentialprovider.kubelet.k8s.io/v1
EOF
# A copy of the atelet DaemonSet, so a Substrate upgrade that changes atelet
# needs this script run again. Its pods keep app=atelet, so the ate-system
# NetworkPolicies cover them.
kubectl -n ate-system get ds atelet -o json | python3 -c '
import sys, json
d = json.load(sys.stdin)
m = d["metadata"]
d["metadata"] = {"name": "atelet-iso", "namespace": m["namespace"], "labels": m.get("labels", {})}
d.pop("status", None)
s = d["spec"]
s["selector"]["matchLabels"]["ate.dev/atelet-variant"] = "iso"
t = s["template"]
t["metadata"].setdefault("labels", {})["ate.dev/atelet-variant"] = "iso"
p = t["spec"]
p["affinity"] = {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
    {"matchExpressions": [{"key": "ate.dev/pool", "operator": "In", "values": ["isolated"]}]}]}}}
found = set()
for v in p["volumes"]:
    if v["name"] == "image-credential-provider-bin":
        v["hostPath"] = {"path": "/usr/libexec/kubernetes/kubelet/plugins", "type": "Directory"}
        found.add(v["name"])
    if v["name"] == "image-credential-provider-config":
        v.pop("hostPath", None)
        v["configMap"] = {"name": "atelet-iso-credential-provider"}
        found.add(v["name"])
for c in p["containers"]:
    for vm in c.get("volumeMounts", []):
        if vm["name"] == "image-credential-provider-config":
            vm["subPath"] = "config.yaml"
assert len(found) == 2, "atelet has no image credential provider volumes: " + str(found)
print(json.dumps(d))' | kubectl apply -f -

log "Counter on mode 3, isolated sandboxes"
digest=$(aws_ ecr describe-images --repository-name "$ECR_PREFIX/counter" \
  --image-ids "imageTag=$SUBSTRATE_TAG" --query 'imageDetails[0].imageDigest' --output text)
COUNTER_IMAGE="$ECR_REGISTRY/$ECR_PREFIX/counter@$digest"
GVISOR_WORKER="$(pinned_image ateworker-gvisor)" || die "cannot resolve ateworker-gvisor:$SUBSTRATE_TAG"
info "worker ${GVISOR_WORKER##*/}"
# The gVisor counter's manifests under new names, with the network transport
# and RuntimeClass isolated-sandbox, on the ate-isolated NodePool.
iso() { # stdin: a counter manifest; prints it for the isolated pool
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
        m.update(name="counter-iso", namespace=ns)
        m.setdefault("labels", {})["workload"] = "counter-iso"
        d["spec"]["replicas"] = 2
        d["spec"]["networkTransport"] = {"runtimeClassName": "isolated-sandbox"}
        t = d["spec"].setdefault("template", {})
        # The guest is sized from the memory request; the worker keeps runtime
        # assets, image layers and actor state on a tmpfs in it.
        t["resources"] = {"limits": {"cpu": "1", "memory": "2Gi"},
                          "requests": {"cpu": "250m", "memory": "2Gi"}}
        t["nodeSelector"] = {"ate.dev/pool": "isolated"}
        t["tolerations"] = [{"key": "ate.dev/sandboxClass", "operator": "Equal",
                             "value": "isolated", "effect": "NoSchedule"}]
        t["annotations"] = {"sandbox.amazonaws.com/" + k: "disabled" for k in (
            "ingress-bandwidth", "egress-bandwidth", "ingress-packets-per-second",
            "egress-packets-per-second", "overlay-disk-bandwidth", "overlay-disk-iops")}
    elif "workerSelector" in d:
        m.update(atespace=ns, name="counter-iso")
        d["workerSelector"]["matchLabels"]["workload"] = "counter-iso"
print(yaml.safe_dump_all(docs, sort_keys=False))' "$NS"
}
iso < "$SRC/counter.yaml.tmpl" | kubectl apply -f -
if ! err=$(kubectl ate create atespace "$NS" 2>&1); then
  grep -qi 'already exists' <<<"$err" || die "create atespace $NS: $err"
fi
if have=$(kubectl ate get actor-template counter-iso -a "$NS" -o json 2>/dev/null); then
  have=$(jq -r '(.spec // .).containers[0].image' <<<"$have")
  [ "$have" = "$COUNTER_IMAGE" ] || die "$NS/counter-iso runs $have, not $COUNTER_IMAGE; run ./clean.sh to replace it"
else
  # The worker Pods wait for Auto Mode to launch their nodes; the golden
  # snapshot needs a worker and atelet-iso there first.
  kubectl -n "$NS" rollout status deploy/counter-iso --timeout=10m
  kubectl -n ate-system rollout status ds/atelet-iso --timeout=5m
  iso < "$SRC/counter-template.yaml.tmpl" | kubectl ate create actor-template -f -
fi
for _ in $(seq 1 90); do
  out=$(kubectl ate get actor-template counter-iso -a "$NS" -o json 2>/dev/null || true)
  jq -e '.status.goldenSnapshotStatus.goldenTag.name // empty' <<<"$out" >/dev/null 2>&1 && break
  err=$(jq -r '.status.goldenSnapshotStatus.errorMessage // empty' <<<"$out" 2>/dev/null || true)
  [ -n "$err" ] && info "golden: $err"
  sleep 10
done
jq -e '.status.goldenSnapshotStatus.goldenTag.name // empty' <<<"$out" >/dev/null 2>&1 \
  || die "golden snapshot not ready for $NS/counter-iso"
info "golden snapshot ready for $NS/counter-iso"
kubectl -n "$NS" get pods -o custom-columns='POD:.metadata.name,RUNTIME:.spec.runtimeClassName,NODE:.spec.nodeName'
info "next: demos/counter/verify.sh (it checks this pool too when MODE3_ISO=on)"
