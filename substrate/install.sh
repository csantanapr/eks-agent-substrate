#!/usr/bin/env bash
# Installs Agent Substrate (published release images) with ate-setup:
#   - snapshots in S3 through the objectstore-plugin sidecars, credentials
#     from EKS Pod Identity
#   - ate-api-server on the private RDS instance: logins read from Secrets
#     Manager here, passed through the environment (never a flag), TLS
#     verify-full against the RDS CA bundle
#   - gVisor and microVM (Kata + Cloud Hypervisor) sandbox classes; the
#     microVM guest assets are staged in the S3 asset bucket
#   - egress credential injection from the bundled Kubernetes Secrets
#     provider, plus any provider a demo registered in
#     .work/credential-providers/ (demos/credprovider-aws)
# Extra arguments go to `ate-setup deploy ate-system`.
#
# deploy.sh calls it after aws/infra.sh, substrate/fetch.sh and
# aws/db-bootstrap.sh; demos/efs/deploy.sh calls it again with
# --preview=ExternalVolumes. A sandbox class is how a worker isolates actors:
# gVisor runs them on a user-space kernel, a microVM on its own guest kernel.
#
# Learn more:
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/reference/sandbox-classes.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/cmd/ate-setup/commands.md
#   https://gvisor.dev/docs/
#   https://katacontainers.io/
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
[ -x "$WORK/bin/ate-setup" ] || die "run substrate/fetch.sh first"
need jq
secret_field() {
  aws_ secretsmanager get-secret-value --secret-id "$1" --query SecretString --output text \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$2"
}
DB_HOST=$(aws_ rds describe-db-instances --db-instance-identifier "$DB_ID" \
  --query 'DBInstances[0].Endpoint.Address' --output text)

export ATE_INSTALL_STORAGE_BACKEND=s3    # the EKS patch: S3 asset staging, no GCP checks
export ATE_ASSET_BUCKET="$ASSET_BUCKET"
export NO_DEV_ENV=1
# ate-setup stores the --ateapi-postgres-server-ca-file bundle in the
# postgres-server-ca Secret, and ate-api-server mounts it at this path.
ca=/run/postgres-server-ca/server-ca.pem
dsn() {
  printf 'postgres://%s:%s@%s:5432/atepg?sslmode=verify-full&sslrootcert=%s' \
    "$(secret_field "$SECRET_PREFIX/ate-$1" username)" "$(secret_field "$SECRET_PREFIX/ate-$1" password)" "$DB_HOST" "$ca"
}
ATE_API_POSTGRES_OWNER_CONNECTION_STRING="$(dsn owner)"
ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING="$(dsn readwrite)"
export ATE_API_POSTGRES_OWNER_CONNECTION_STRING ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING

# Clients atenet-router lets reach actors: `kubectl ate proxy`, and the
# OpenClaw demo's gateway and dashboard. A SPIFFE ID names a pod's
# ServiceAccount: spiffe://cluster.local/ns/<namespace>/sa/<name>.
INGRESS_IDS="spiffe://cluster.local/ns/ate-system/sa/atenet-client"
INGRESS_IDS+=",spiffe://cluster.local/ns/openclaw/sa/openclaw-gateway"
INGRESS_IDS+=",spiffe://cluster.local/ns/openclaw/sa/openclaw-dashboard"

# The egress gateway's credential providers. k8s.io is the bundled provider,
# which injects credentials from Kubernetes Secrets into actor egress. Each
# JSON object a demo registered in .work/credential-providers/
# (demos/credprovider-aws) adds one more. They are read on every install, so a
# later ./deploy.sh keeps them. With none registered, only k8s.io is passed.
CREDENTIAL_PROVIDERS='{"name":"k8s.io"}'
for f in "$WORK"/credential-providers/*.json; do
  [ -f "$f" ] || continue
  CREDENTIAL_PROVIDERS+=",$(jq -ce 'select(type == "object")' "$f")" || die "$f is not a JSON object"
  info "credential provider: $(jq -r .name "$f")"
done
[ "$CREDENTIAL_PROVIDERS" = '{"name":"k8s.io"}' ] || CREDENTIAL_PROVIDERS="[$CREDENTIAL_PROVIDERS]"

# Preview gates: the ones the cluster runs now, plus any --preview= given
# here. ate-setup deploys exactly the gates it is passed, so a plain
# ./deploy.sh would otherwise turn off one a demo turned on (ExternalVolumes
# for demos/efs) and roll the worker pods. ./clean.sh uninstalls Substrate,
# which clears them.
args=() gates=""
for a in "$@"; do
  case "$a" in
    --preview=*) gates+=",${a#--preview=}" ;;
    *) args+=("$a") ;;
  esac
done
if kubectl -n ate-system get deploy ate-api-server >/dev/null 2>&1; then
  gates+=",$(kubectl -n ate-system get deploy ate-api-server -o json \
    | jq -r '[.spec.template.spec.containers[].args[]? | select(startswith("--preview=")) | ltrimstr("--preview=")] | first // ""')"
fi
gates=$(tr ',' '\n' <<<"$gates" | sed '/^$/d' | sort -u | paste -sd, -)
[ -z "$gates" ] || { args+=("--preview=$gates"); info "preview gates: $gates"; }

# A reinstall can roll the worker pods, which crashes the actors running on
# them. Suspend those first; the next request resumes them from their snapshot.
# https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/reference/kubectl-ate.md
if kubectl get crd workerpools.ate.dev >/dev/null 2>&1; then
  running=$(kubectl ate get actors -A -o json 2>/dev/null \
    | jq -r '.actors[]? | select(.status.state == "ACTOR_STATE_RUNNING") | "\(.metadata.atespace) \(.metadata.name)"') \
    || { warn "cannot list actors; running ones may crash when the workers roll"; running=""; }
  while read -r atespace actor; do
    [ -n "$actor" ] || continue
    kubectl ate suspend actor "$actor" -a "$atespace" >/dev/null && info "suspended $atespace/$actor"
  done <<<"$running"
fi

log "ate-setup deploy ate-system ($SUBSTRATE_TAG)"
cd "$SUBSTRATE_SRC"
"$WORK/bin/ate-setup" deploy ate-system \
  --context "$(kubectl config current-context)" \
  --image-repo "$SUBSTRATE_IMAGE_REPO" --image-tag "$SUBSTRATE_TAG" \
  --storage-bucket-name "$SNAP_BUCKET" \
  --snapshot-location "s3://${SNAP_BUCKET}/" \
  --ateapi-postgres-server-ca-file "$WORK/rds-global-bundle.pem" \
  --credential-provider "$CREDENTIAL_PROVIDERS" \
  --ingress-allowed-spiffe-ids "$INGRESS_IDS" \
  --sandbox-classes gvisor,microvm \
  --rollout-timeout 5m \
  "${args[@]}"
printf '%s' "$SUBSTRATE_TAG" > "$INSTALLED_TAG_FILE"
# The objectstore sidecars and atelet's image pulls sign with Pod Identity.
ensure_pod_identity ate-system deploy/ate-api-server
ensure_pod_identity ate-system ds/atelet
kubectl -n ate-system get pods
