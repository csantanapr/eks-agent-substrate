#!/usr/bin/env bash
# Installs Agent Substrate (published release images) with ate-setup:
#   - snapshots in S3 through the objectstore-plugin sidecars, credentials
#     from EKS Pod Identity
#   - ate-api-server on the private RDS instance: logins read from Secrets
#     Manager here, passed through the environment (never a flag), TLS
#     verify-full against the RDS CA bundle
#   - gVisor and microVM (Kata + Cloud Hypervisor) sandbox classes; the
#     microVM guest assets are staged in the S3 asset bucket
# Extra arguments go to `ate-setup deploy ate-system`.
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
[ -x "$WORK/bin/ate-setup" ] || die "run substrate/fetch.sh first"
secret_field() {
  aws_ secretsmanager get-secret-value --secret-id "$1" --query SecretString --output text \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)[sys.argv[1]])' "$2"
}
DB_HOST=$(aws_ rds describe-db-instances --db-instance-identifier "$DB_ID" \
  --query 'DBInstances[0].Endpoint.Address' --output text)

export ATE_INSTALL_STORAGE_BACKEND=s3    # the EKS patch: S3 asset staging, no GCP checks
export ATE_ASSET_BUCKET="$ASSET_BUCKET"
export NO_DEV_ENV=1
ca=/run/postgres-server-ca/server-ca.pem
dsn() {
  printf 'postgres://%s:%s@%s:5432/atepg?sslmode=verify-full&sslrootcert=%s' \
    "$(secret_field "$SECRET_PREFIX/ate-$1" username)" "$(secret_field "$SECRET_PREFIX/ate-$1" password)" "$DB_HOST" "$ca"
}
ATE_API_POSTGRES_OWNER_CONNECTION_STRING="$(dsn owner)"
ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING="$(dsn readwrite)"
export ATE_API_POSTGRES_OWNER_CONNECTION_STRING ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING

# Clients atenet-router lets reach actors: `kubectl ate proxy`, and the
# OpenClaw demo's gateway and dashboard.
INGRESS_IDS="spiffe://cluster.local/ns/ate-system/sa/atenet-client"
INGRESS_IDS+=",spiffe://cluster.local/ns/openclaw/sa/openclaw-gateway"
INGRESS_IDS+=",spiffe://cluster.local/ns/openclaw/sa/openclaw-dashboard"

log "ate-setup deploy ate-system ($SUBSTRATE_TAG)"
cd "$SUBSTRATE_SRC"
"$WORK/bin/ate-setup" deploy ate-system \
  --context "$(kubectl config current-context)" \
  --image-repo "$SUBSTRATE_IMAGE_REPO" --image-tag "$SUBSTRATE_TAG" \
  --storage-bucket-name "$SNAP_BUCKET" \
  --snapshot-location "s3://${SNAP_BUCKET}/" \
  --ateapi-postgres-server-ca-file "$WORK/rds-global-bundle.pem" \
  --credential-provider '{"name":"k8s.io"}' \
  --ingress-allowed-spiffe-ids "$INGRESS_IDS" \
  --sandbox-classes gvisor,microvm \
  --rollout-timeout 5m \
  "$@"
printf '%s' "$SUBSTRATE_TAG" > "$INSTALLED_TAG_FILE"
kubectl -n ate-system get pods
