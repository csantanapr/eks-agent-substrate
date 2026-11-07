#!/usr/bin/env bash
# Optional: egress credential injection from AWS Secrets Manager. An actor
# sends a placeholder header; the egress gateway asks the
# credprovider-aws-secret-manager plugin for the secret its EgressPolicy names,
# and the request leaves the cluster with the real value. The actor never
# holds the secret.
#
#   demos/credprovider-aws/deploy.sh          after ./deploy.sh
#   CREDPROVIDER_DIR=~/src/credprovider-aws-secret-manager demos/credprovider-aws/deploy.sh
#
#   provider   aws-sm-credential-provider in ate-system, 2 replicas, built with
#              docker from CREDPROVIDER_DIR (default ~/credprovider-aws-secret-manager)
#              and pushed to ECR; mTLS, only the egress gateway may call it
#              (NetworkPolicy and SPIFFE ID); default-deny atespace policy
#   IAM        its Pod Identity role may read only secrets under ate/ that are
#              tagged ate.dev/injectable=true and owned by this deployment
#   secrets    ate/ate-demo-credprovider/<cluster>-demo-token (random, injectable) and
#              .../untagged-token (not injectable), in Secrets Manager
#   gateway    substrate/install.sh with the provider registered in
#              .work/credential-providers/, so atenet-egress serves both k8s.io
#              and secretsmanager.amazonaws.com, now and on every ./deploy.sh
#   actors     Substrate's e2e probe as the actor, in two atespaces: one the
#              provider policy grants, one it does not
#
# https://github.com/csantanapr/credprovider-aws-secret-manager
# https://github.com/agent-substrate/substrate/blob/main/docs/guides/egress-credential-injection.md
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
. "$ROOT/demos/credprovider-aws/lib.sh"
ensure_kubeconfig
for t in docker jq go git python3 envsubst; do need "$t"; done
CREDPROVIDER_DIR="${CREDPROVIDER_DIR:-$HOME/credprovider-aws-secret-manager}"
[ -f "$CREDPROVIDER_DIR/Dockerfile" ] && [ -d "$CREDPROVIDER_DIR/config" ] \
  || die "no credprovider-aws-secret-manager checkout at $CREDPROVIDER_DIR; set CREDPROVIDER_DIR"
[ -x "$WORK/bin/ate-setup" ] && [ -d "$SUBSTRATE_SRC/internal/e2e/fixtures/probe" ] || die "run ./deploy.sh first"
if [ -f "$INSTALLED_TAG_FILE" ] && [ "$(cat "$INSTALLED_TAG_FILE")" != "$SUBSTRATE_TAG" ]; then
  die "the cluster runs $(cat "$INSTALLED_TAG_FILE") but config names $SUBSTRATE_TAG; run ./deploy.sh first"
fi
# shellcheck disable=SC2207
TAG_ARGS=($(tags_cli))
BUILD="$WORK/credprovider-aws"
mkdir -p "$BUILD"

# First, so the association has applied by the time the provider's pods start.
phase "IAM role and Pod Identity association"
log "IAM role for the provider (injectable secrets of this deployment only)"
cat > "$WORK/policy-$CP_SA.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Sid":"ReadInjectableSecrets","Effect":"Allow",
 "Action":"secretsmanager:GetSecretValue",
 "Resource":"arn:$PARTITION:secretsmanager:$AWS_REGION:$ACCOUNT:secret:ate/*",
 "Condition":{"StringEquals":{"aws:ResourceTag/$CP_INJECTABLE_TAG":"true",
   "aws:ResourceTag/$OWNER_TAG_KEY":"$OWNER_TAG_VALUE"}}}]}
EOF
role_for_sa "$CP_NS" "$CP_SA" "$WORK/policy-$CP_SA.json"

phase "Secrets Manager secrets"
log "Secrets Manager: the demo secrets"
# ensure_secret NAME INJECTABLE DESCRIPTION: a random 40-character value the
# first time (through a 0600 file, never a command-line argument); the
# injectable tag is set or removed every time.
ensure_secret() {
  local name="$1" injectable="$2" f err i
  if owned_secret "$name"; then
    info "$name exists"
  else
    f="$(umask 077; mktemp "$WORK/secret.XXXXXX")"
    aws_ secretsmanager get-random-password --password-length 40 --exclude-punctuation \
      --query RandomPassword --output text | tr -d '\n' > "$f"
    # A secret deleted by clean.sh can hold its name for a short while.
    for i in $(seq 1 12); do
      if err=$(aws_ secretsmanager create-secret --name "$name" --description "$3" \
          --secret-string "file://$f" --tags "${TAG_ARGS[@]}" 2>&1 >/dev/null); then break; fi
      grep -q 'scheduled for deletion' <<<"$err" && [ "$i" -lt 12 ] || { rm -f "$f"; die "create-secret $name: $err"; }
      info "$name is still being deleted; retrying"
      sleep 10
    done
    rm -f "$f"
    info "$name created"
  fi
  if [ "$injectable" = true ]; then
    aws_ secretsmanager tag-resource --secret-id "$name" --tags "Key=$CP_INJECTABLE_TAG,Value=true"
  else
    aws_ secretsmanager untag-resource --secret-id "$name" --tag-keys "$CP_INJECTABLE_TAG"
  fi
}
ensure_secret "$CP_SECRET" true "Agent Substrate credential injection demo token ($CLUSTER_NAME)"
ensure_secret "$CP_UNTAGGED_SECRET" false "Agent Substrate demo: not injectable, the provider must be refused ($CLUSTER_NAME)"

phase "ECR repositories and images"
log "ECR repositories"
for repo in "$CP_REPO" "$CP_PROBE_REPO"; do
  if ! aws_exists ecr describe-repositories --repository-names "$repo"; then
    aws_ ecr create-repository --repository-name "$repo" \
      --image-scanning-configuration scanOnPush=true --encryption-configuration encryptionType=AES256 \
      --tags "${TAG_ARGS[@]}" >/dev/null
  fi
  info "$repo"
done
aws_ ecr get-login-password | docker login --username AWS --password-stdin "$ECR_REGISTRY" >/dev/null 2>&1
# ecr_digest REPO TAG: the digest ECR holds for the tag just pushed.
ecr_digest() {
  aws_ ecr describe-images --repository-name "$1" --image-ids "imageTag=$2" \
    --query 'imageDetails[0].imageDigest' --output text
}

log "Provider image from $CREDPROVIDER_DIR"
# A copy of the checkout's files (committed or not) with the modules vendored
# by the local go, so the docker build downloads nothing.
src="$BUILD/src"
rm -rf "$src"; mkdir -p "$src"
git -C "$CREDPROVIDER_DIR" ls-files -z --cached --others --exclude-standard \
  | tar -C "$CREDPROVIDER_DIR" --null -T - -cf - | tar -C "$src" -xf -
(cd "$src" && go mod vendor)
version=$(git -C "$CREDPROVIDER_DIR" describe --tags --always --dirty)
docker build -q --platform linux/amd64 --build-arg "VERSION=$version" -t "$ECR_REGISTRY/$CP_REPO:$version" "$src" >/dev/null
docker push -q "$ECR_REGISTRY/$CP_REPO:$version" >/dev/null
PROVIDER_IMAGE="$ECR_REGISTRY/$CP_REPO@$(ecr_digest "$CP_REPO" "$version")"
info "${PROVIDER_IMAGE#"$ECR_REGISTRY"/} ($version)"

log "Probe actor image from the Substrate $SUBSTRATE_TAG source"
mkdir -p "$BUILD/probe-image"
(cd "$SUBSTRATE_SRC" && GOFLAGS=-mod=vendor CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o "$BUILD/probe-image/probe" ./internal/e2e/fixtures/probe)
cp "$ROOT/demos/credprovider-aws/probe.Dockerfile" "$BUILD/probe-image/Dockerfile"
docker build -q --platform linux/amd64 -t "$ECR_REGISTRY/$CP_PROBE_REPO:$SUBSTRATE_TAG" "$BUILD/probe-image" >/dev/null
docker push -q "$ECR_REGISTRY/$CP_PROBE_REPO:$SUBSTRATE_TAG" >/dev/null
PROBE_IMAGE="$ECR_REGISTRY/$CP_PROBE_REPO@$(ecr_digest "$CP_PROBE_REPO" "$SUBSTRATE_TAG")"
info "${PROBE_IMAGE#"$ECR_REGISTRY"/}"

phase "Provider in ate-system"
log "Secret policy: $CP_ATESPACE may resolve ate/$CP_ATESPACE/*"
# The policy is secret-policy.yaml. The live account goes only into the
# cluster, never into a tracked file.
policy_yaml=$(render_yaml "$ROOT/demos/credprovider-aws/secret-policy.yaml" \
  CP_POLICY_CM CP_NS CP_DENIED_ATESPACE CP_ATESPACE ACCOUNT AWS_REGION)
kubectl apply -f - <<<"$policy_yaml" >/dev/null
policy_sha=$(sha256 <<<"$policy_yaml")

log "aws-sm-credential-provider (config/ from the checkout)"
# The plugin's kustomize base, with the ECR image, this Region, the system
# node group, and the policy's hash, so a policy change rolls the pods (the
# provider reads it only at start).
# --expected-role-arn pins the provider to this deployment's role: it exits
# at startup if its Pod Identity credentials resolve to any other role.
kubectl kustomize "$CREDPROVIDER_DIR/config" | python3 -c '
import sys, yaml
image, region, sha, role = sys.argv[1:]
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
found = 0
for d in docs:
    if d["kind"] != "Deployment":
        continue
    tpl = d["spec"]["template"]
    tpl["spec"]["nodeSelector"] = {"ate.dev/pool": "system"}
    # The taint of the system group with AUTO_MODE=on (create-cluster.sh).
    tpl["spec"]["tolerations"] = [{"key": "ate.dev/workloadType", "operator": "Equal",
                                   "value": "ate-system-infra", "effect": "NoSchedule"}]
    tpl["metadata"].setdefault("annotations", {})["agent-substrate-on-eks/secret-policy-sha256"] = sha
    for c in tpl["spec"]["containers"]:
        assert c["image"].startswith("ko://"), c["image"]
        c["image"] = image
        args = ["--region=" + region if a.startswith("--region=") else a for a in c["args"]]
        assert "--region=" + region in args, "no --region in the provider args"
        args = [a for a in args if not a.startswith("--expected-role-arn=")] + ["--expected-role-arn=" + role]
        c["args"] = args
        found += 1
assert found == 1, "want one provider container, found %d" % found
print(yaml.safe_dump_all(docs))' "$PROVIDER_IMAGE" "$AWS_REGION" "$policy_sha" \
  "arn:$PARTITION:iam::$ACCOUNT:role/$CP_ROLE" | kubectl apply -f -
# A pod that started before the association applied exits at startup (no
# Pod Identity credentials); ensure_pod_identity restarts it.
kubectl -n "$CP_NS" rollout status "deploy/$CP_SA" --timeout=2m >/dev/null 2>&1 \
  || info "not ready yet; checking its Pod Identity credentials"
ensure_pod_identity "$CP_NS" "deploy/$CP_SA"
kubectl -n "$CP_NS" rollout status "deploy/$CP_SA" --timeout=5m
kubectl -n "$CP_NS" logs "deploy/$CP_SA" --tail=50 | grep -m1 '"msg":"AWS identity"' \
  | jq -r '"   provider runs as \(.arn | sub(":[0-9]{12}:"; ":<account>:"))"' || true

phase "Egress gateway: register the provider"
log "atenet-egress serves k8s.io and $CP_PROVIDER_NAME"
mkdir -p "$(dirname "$CP_REGISTRATION")"
jq -cn --arg n "$CP_PROVIDER_NAME" --arg a "$CP_PROVIDER_ADDRESS" '{name: $n, address: $a}' > "$CP_REGISTRATION"
info "registered in ${CP_REGISTRATION#"$ROOT"/}; every substrate/install.sh passes it on"
if cp_gateway_serves; then
  info "atenet-egress already serves $CP_PROVIDER_NAME"
else
  cp_reinstall
  cp_gateway_serves || die "atenet-egress does not serve $CP_PROVIDER_NAME after the install"
fi
kubectl -n ate-system rollout status deploy/atenet-egress --timeout=5m

phase "Probe actors: worker pool and templates"
log "Worker pool credprovider-probe (gVisor)"
GVISOR_WORKER="$(pinned_image ateworker-gvisor)" || die "cannot resolve ateworker-gvisor:$SUBSTRATE_TAG"
sed -e "s|ko://go.ate.dev/substrate/cmd/ateworker-gvisor|$GVISOR_WORKER|" "$ROOT/demos/credprovider-aws/probe.yaml.tmpl" \
  | python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
for d in docs:
    if d.get("kind") == "WorkerPool":
        d["spec"].setdefault("template", {})["nodeSelector"] = {"ate.dev/pool": "gvisor"}
print(yaml.safe_dump_all(docs))' | kubectl apply -f -

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
for atespace in "$CP_ATESPACE" "$CP_DENIED_ATESPACE"; do
  log "Atespace $atespace and template $CP_TEMPLATE"
  if ! err=$(kubectl ate create atespace "$atespace" 2>&1); then
    grep -qi 'already exists' <<<"$err" || die "create atespace $atespace: $err"
  fi
  # Templates are immutable: one with another probe image is replaced, with
  # its actors (verify.sh makes new ones).
  if have=$(kubectl ate get actor-template "$CP_TEMPLATE" -a "$atespace" -o json 2>/dev/null); then
    have=$(jq -r '(.spec // .).containers[0].image' <<<"$have")
    if [ "$have" != "$PROBE_IMAGE" ]; then
      info "replacing $atespace/$CP_TEMPLATE (was ${have##*/})"
      for a in $(kubectl ate get actors -a "$atespace" -o json | jq -r '.actors[]?.metadata.name'); do
        kubectl ate delete actor "$a" -a "$atespace" >/dev/null
      done
      kubectl ate delete actor-template "$CP_TEMPLATE" -a "$atespace" >/dev/null
      have=""
    fi
  fi
  if [ -z "${have:-}" ]; then
    sed -e "s|REPLACE_WITH_ATESPACE|$atespace|" -e "s|REPLACE_WITH_PROBE_IMAGE|$PROBE_IMAGE|" \
      "$ROOT/demos/credprovider-aws/probe-template.yaml.tmpl" | kubectl ate create actor-template -f - >/dev/null
  fi
  wait_golden "$atespace" "$CP_TEMPLATE"
done
info "next: demos/credprovider-aws/verify.sh"
