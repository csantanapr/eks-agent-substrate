# shellcheck shell=bash disable=SC2034
# Shared helpers for the top-level scripts. Sourced, never run. Needs bash 4+
# (macOS: brew install bash) and GNU sed/sort (brew install gnu-sed coreutils).
#
# Loads config.env, then config.local.env when present (gitignored: your own
# profile, region, tags). Exposes:
#   aws_        aws with the configured region (and AWS_PROFILE when set)
#   aws_exists  0 if an AWS read finds the resource, 1 on a not-found error;
#               any other error (denied, throttled) stops the script
#   ACCOUNT, PARTITION, AWS_DNS_SUFFIX   read from STS, never written to a file
#   KUBECONFIG  .work/kubeconfig, written by create-cluster.sh; the shared
#               ~/.kube/config is never read or changed
#   tags_*      the TAGS list, plus the owner tag, in each format the AWS CLI
#               and eksctl want

[ "${BASH_VERSINFO[0]}" -ge 4 ] || { echo "bash 4 or newer is required (macOS: brew install bash)" >&2; exit 1; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$ROOT/.work"
mkdir -p "$WORK"

log()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not on PATH"; }

# SUBSTRATE_TAG from the environment wins over the files, so a new release
# can be tried with `SUBSTRATE_TAG=v1.0.0-rc.3 ./deploy.sh`.
_env_tag="${SUBSTRATE_TAG:-}"
# shellcheck source=../config.env
. "$ROOT/config.env"
# shellcheck source=/dev/null
[ -f "$ROOT/config.local.env" ] && . "$ROOT/config.local.env"
[ -n "$_env_tag" ] && SUBSTRATE_TAG="$_env_tag"
unset _env_tag

# SUBSTRATE_TAG=latest: the newest release tag, release candidates included
# (v1.0.0 sorts after v1.0.0-rc.3).
latest_substrate_tag() {
  local tags
  tags=$(git ls-remote --tags --refs "$SUBSTRATE_REPO" 'v*') || return 1
  sed 's|.*refs/tags/||' <<<"$tags" \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' \
    | sed 's/-rc\./~rc./' | sort -V | tail -1 | sed 's/~rc\./-rc./'
}
if [ "$SUBSTRATE_TAG" = latest ]; then
  SUBSTRATE_TAG="$(latest_substrate_tag)" || die "cannot list the tags of $SUBSTRATE_REPO"
  [ -n "$SUBSTRATE_TAG" ] || die "no release tags in $SUBSTRATE_REPO"
fi

: "${CLUSTER_NAME:?set CLUSTER_NAME in config.env}"
: "${AWS_REGION:?set AWS_REGION in config.env}"
# Every derived name (S3 buckets with account and region, IAM roles, RDS,
# ECR) must stay valid, so the cluster name is short lowercase DNS style.
[[ "$CLUSTER_NAME" =~ ^[a-z][a-z0-9-]{0,26}[a-z0-9]$ ]] \
  || die "CLUSTER_NAME must be 2-28 characters of a-z, 0-9 and '-', starting with a letter"
[[ "$AWS_REGION" =~ ^[a-z]{2}(-[a-z]+)+-[0-9]$ ]] || die "AWS_REGION $AWS_REGION is not a region name"

export AWS_REGION AWS_DEFAULT_REGION="$AWS_REGION"
if [ -n "${AWS_PROFILE:-}" ]; then export AWS_PROFILE; else unset AWS_PROFILE; fi
export KUBECONFIG="$WORK/kubeconfig"
export PATH="$WORK/bin:$PATH"

aws_() { aws --region "$AWS_REGION" "$@"; }

# aws_exists ARGS...: run a describe/get/head call. Not-found -> 1; any other
# failure stops the script, so a denied read is never taken for "absent".
aws_exists() {
  local err
  if err=$(aws_ "$@" 2>&1 >/dev/null); then return 0; fi
  case "$err" in
    *NotFound*|*NoSuchEntity*|*NoSuchBucket*|*"(404)"*|*"Not Found"*|*"does not exist"*) return 1 ;;
  esac
  die "aws $1 $2 failed: $err"
}

_caller_arn="$(aws_ sts get-caller-identity --query Arn --output text 2>/dev/null)" \
  || die "no AWS credentials: configure a profile and set AWS_PROFILE in config.local.env"
PARTITION="$(cut -d: -f2 <<<"$_caller_arn")"
ACCOUNT="$(cut -d: -f5 <<<"$_caller_arn")"
unset _caller_arn
case "$PARTITION" in
  aws-cn) AWS_DNS_SUFFIX=amazonaws.com.cn ;;
  *)      AWS_DNS_SUFFIX=amazonaws.com ;;
esac

# Names derived from the cluster name. S3 bucket and IAM role names are
# global, so they also carry the region (and buckets the account id).
SNAP_BUCKET="${CLUSTER_NAME}-snap-${ACCOUNT}-${AWS_REGION}"
ASSET_BUCKET="${CLUSTER_NAME}-assets-${ACCOUNT}-${AWS_REGION}"
IAM_PREFIX="${CLUSTER_NAME}-${AWS_REGION}"
DB_ID="${CLUSTER_NAME}-pg"
LT_NAME="${CLUSTER_NAME}-microvm-nested-virt"
SECRET_PREFIX="${CLUSTER_NAME}"
ECR_PREFIX="${CLUSTER_NAME}"
ECR_REGISTRY="${ACCOUNT}.dkr.ecr.${AWS_REGION}.${AWS_DNS_SUFFIX}"
# demos/efs: the file system's creation token and its security group.
EFS_TOKEN="${CLUSTER_NAME}-actors"
EFS_SG_NAME="${CLUSTER_NAME}-efs"
# demos/db-rotation: the rotation Lambdas' stack and security group.
ROTATION_STACK="${CLUSTER_NAME}-db-rotation"
ROTATION_SG_NAME="${CLUSTER_NAME}-db-rotation"
# demos/s3files: the bucket the S3 file system syncs with, and its security group.
S3FILES_BUCKET="${CLUSTER_NAME}-files-${ACCOUNT}-${AWS_REGION}"
S3FILES_SG_NAME="${CLUSTER_NAME}-s3files"
# The ServiceAccounts with an IAM role through EKS Pod Identity.
POD_IDENTITY_SAS="ate-system/ate-api-server ate-system/atelet llm-gateway/llm-gateway"
# Upstream source at SUBSTRATE_TAG with patches/ applied (substrate/fetch.sh).
SUBSTRATE_SRC="$WORK/substrate-$SUBSTRATE_TAG"
# The release the cluster runs, written by substrate/install.sh. clean.sh
# removes that one, even when config.env already names the next release.
INSTALLED_TAG_FILE="$WORK/installed-substrate-tag"

# image_digest IMAGE TAG: the manifest digest of a public ghcr.io image tag,
# so release images are pinned by digest without hardcoding it.
image_digest() {
  local repo="${1#ghcr.io/}" tag="$2" token
  token=$(curl -fsS "https://ghcr.io/token?scope=repository:${repo}:pull" | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])') || return 1
  curl -fsSI -H "Authorization: Bearer $token" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json' \
    "https://ghcr.io/v2/${repo}/manifests/${tag}" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:" {print $2}'
}
# pinned_image NAME: SUBSTRATE_IMAGE_REPO/NAME:SUBSTRATE_TAG@sha256:...
pinned_image() {
  local d
  d=$(image_digest "$SUBSTRATE_IMAGE_REPO/$1" "$SUBSTRATE_TAG")
  [ -n "$d" ] || { echo "no $1 image for $SUBSTRATE_TAG in $SUBSTRATE_IMAGE_REPO" >&2; return 1; }
  printf '%s/%s:%s@%s' "$SUBSTRATE_IMAGE_REPO" "$1" "$SUBSTRATE_TAG" "$d"
}
export GOTOOLCHAIN

# --- tags -------------------------------------------------------------------
# TAGS is a space-separated list of key=value pairs (config.env). Every
# resource also gets OWNER_TAG, which the scripts check before they reuse or
# delete a resource with a global name.
OWNER_TAG_KEY=agent-substrate-on-eks/deployment
OWNER_TAG_VALUE="${CLUSTER_NAME}.${AWS_REGION}"
for _kv in $TAGS; do
  [[ "$_kv" =~ ^[A-Za-z0-9_.:/+@-]+=[A-Za-z0-9_.:/+@-]*$ ]] \
    || die "TAGS entry '$_kv' must be key=value with letters, digits and _ . : / + @ - only"
done
unset _kv
_tag_pairs() {
  for kv in $TAGS; do printf '%s %s\n' "${kv%%=*}" "${kv#*=}"; done
  printf '%s %s\n' "$OWNER_TAG_KEY" "$OWNER_TAG_VALUE"
}
# Key=a,Value=b Key=c,Value=d     (iam, rds, secretsmanager, ecr --tags)
tags_cli() { _tag_pairs | while read -r k v; do printf 'Key=%s,Value=%s ' "$k" "$v"; done; }
# {Key=a,Value=b},{Key=c,Value=d} (inside ResourceType=...,Tags=[...] and TagSet=[...])
tags_braces() { _tag_pairs | while read -r k v; do printf '{Key=%s,Value=%s},' "$k" "$v"; done | sed 's/,$//'; }
# [{"Key":"a","Value":"b"}]        (JSON)
tags_json() { _tag_pairs | while read -r k v; do printf '{"Key":"%s","Value":"%s"},' "$k" "$v"; done | sed 's/^/[/; s/,$/]/'; }
# a=b,c=d                          (eks --tags)
tags_csv() { _tag_pairs | while read -r k v; do printf '%s=%s,' "$k" "$v"; done | sed 's/,$//'; }
# YAML map lines, indented by $1 spaces (eksctl)
tags_yaml() { local pad; pad="$(printf '%*s' "$1" '')"; _tag_pairs | while read -r k v; do printf '%s"%s": "%s"\n' "$pad" "$k" "$v"; done; }
# key=value lines (Auto Scaling group and StorageClass tags)
tags_kv() { _tag_pairs | while read -r k v; do printf '%s=%s\n' "$k" "$v"; done; }

# owned_role ROLE / owned_bucket BUCKET: 0 if it carries our owner tag, 1 if
# it does not exist; stops the script when it exists without the tag.
owned_role() {
  aws_exists iam get-role --role-name "$1" || return 1
  local v
  v=$(aws_ iam list-role-tags --role-name "$1" --query "Tags[?Key=='$OWNER_TAG_KEY'].Value | [0]" --output text) \
    || die "cannot read the tags of IAM role $1"
  [ "$v" = "$OWNER_TAG_VALUE" ] || die "IAM role $1 exists but is not owned by $OWNER_TAG_VALUE ($OWNER_TAG_KEY=$v)"
}
owned_bucket() {
  aws_exists s3api head-bucket --bucket "$1" --expected-bucket-owner "$ACCOUNT" || return 1
  local v
  v=$(aws_ s3api get-bucket-tagging --bucket "$1" --expected-bucket-owner "$ACCOUNT" \
        --query "TagSet[?Key=='$OWNER_TAG_KEY'].Value | [0]" --output text 2>/dev/null) || v=None
  [ "$v" = "$OWNER_TAG_VALUE" ] || die "bucket $1 exists but is not owned by $OWNER_TAG_VALUE ($OWNER_TAG_KEY=$v)"
}

# --- timing -------------------------------------------------------------------
# phase NAME starts timing a major piece and ends the previous one. At exit
# the script prints how long each took, and appends them to .work/timings.tsv
# (UTC start, script, phase, seconds, exit status).
TIMINGS_FILE="$WORK/timings.tsv"
_phases=()
_phase_name=""
_phase_t0=0
_phase_end() { # $1 exit status
  [ -n "$_phase_name" ] || return 0
  local secs=$((SECONDS - _phase_t0))
  _phases+=("$_phase_name"$'\t'"$secs"$'\t'"$1")
  printf '%s\t%s\t%s\t%s\t%s\n' "$(date -u -d "-$secs seconds" +%FT%TZ)" "$(basename "$0")" \
    "$_phase_name" "$secs" "$1" >> "$TIMINGS_FILE"
  _phase_name=""
}
phase() { _phase_end 0; _phase_name="$1"; _phase_t0=$SECONDS; }
fmt_secs() { printf '%d:%02d' $(($1 / 60)) $(($1 % 60)); }
_timings_exit() {
  local rc=$? total=0 name secs st
  _phase_end "$rc"
  [ "${#_phases[@]}" -gt 0 ] || return "$rc"
  printf '\n\033[1;36m== Timings: %s\033[0m\n' "$(basename "$0")"
  for row in "${_phases[@]}"; do
    IFS=$'\t' read -r name secs st <<<"$row"
    total=$((total + secs))
    printf '   %-34s %7s%s\n' "$name" "$(fmt_secs "$secs")" "$([ "$st" = 0 ] || echo "  (failed)")"
  done
  printf '   %-34s %7s\n' "total" "$(fmt_secs "$total")"
  return "$rc"
}
trap _timings_exit EXIT

# Ask before a destructive step unless --yes was given.
confirm() { # $1 what
  [ "${ASSUME_YES:-}" = 1 ] && return 0
  printf '%s\nType the cluster name (%s) to continue: ' "$1" "$CLUSTER_NAME"
  read -r answer
  [ "$answer" = "$CLUSTER_NAME" ] || die "not confirmed"
}

cluster_exists() { aws_exists eks describe-cluster --name "$CLUSTER_NAME"; }

# Write .work/kubeconfig for the cluster (a no-op when the context is there).
ensure_kubeconfig() {
  cluster_exists || die "cluster $CLUSTER_NAME not found in $AWS_REGION; run ./create-cluster.sh"
  aws_ eks update-kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG" >/dev/null
  chmod 600 "$KUBECONFIG"
}

# ensure_pod_identity NAMESPACE KIND/NAME: EKS adds the Pod Identity
# credentials (AWS_CONTAINER_CREDENTIALS_FULL_URI) to a pod only when it is
# created, and a new association takes a few seconds to apply. Restart the
# workload until every one of its pods has them.
ensure_pod_identity() {
  local ns="$1" obj="$2" sel missing
  sel=$(kubectl -n "$ns" get "$obj" -o json | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
  for attempt in 1 2 3 4; do
    missing=$(kubectl -n "$ns" get pods -l "$sel" -o json | jq -r '.items[]
      | select(.metadata.deletionTimestamp == null)
      | select([.spec.containers[].env[]?.name] | index("AWS_CONTAINER_CREDENTIALS_FULL_URI") | not)
      | .metadata.name')
    [ -n "$missing" ] || { info "$ns/$obj has Pod Identity credentials"; return 0; }
    [ "$attempt" -lt 4 ] || die "$ns/$obj pods still lack Pod Identity credentials: $missing"
    info "$ns/$obj started before its Pod Identity association applied; restarting"
    sleep 10
    kubectl -n "$ns" rollout restart "$obj" >/dev/null
    kubectl -n "$ns" rollout status "$obj" --timeout=5m >/dev/null
  done
}

# role_for_sa NAMESPACE SERVICEACCOUNT POLICY_FILE: an IAM role that only EKS
# Pod Identity for this cluster can assume, its inline policy, and the Pod
# Identity association for the ServiceAccount, pointed at that role.
role_for_sa() {
  # shellcheck disable=SC2207
  local TAG_ARGS=($(tags_cli))
  local ns="$1" sa="$2" policy="$3" role="${IAM_PREFIX}-$2" role_arn ids id current
  role_arn="arn:$PARTITION:iam::$ACCOUNT:role/$role"
  cat > "$WORK/pod-identity-trust.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Principal":{"Service":"pods.eks.amazonaws.com"},
 "Action":["sts:AssumeRole","sts:TagSession"],
 "Condition":{"StringEquals":{"aws:SourceAccount":"$ACCOUNT"},
   "ArnLike":{"aws:SourceArn":"arn:$PARTITION:eks:$AWS_REGION:$ACCOUNT:cluster/$CLUSTER_NAME"}}}]}
EOF
  if owned_role "$role"; then
    aws_ iam update-assume-role-policy --role-name "$role" --policy-document "file://$WORK/pod-identity-trust.json"
  else
    aws_ iam create-role --role-name "$role" \
      --assume-role-policy-document "file://$WORK/pod-identity-trust.json" --tags "${TAG_ARGS[@]}" >/dev/null
  fi
  aws_ iam put-role-policy --role-name "$role" --policy-name access --policy-document "file://$policy"
  ids=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" \
    --namespace "$ns" --service-account "$sa" --query 'associations[].associationId' --output text)
  read -ra ids <<<"$ids"
  if [ "${#ids[@]}" -eq 0 ]; then
    aws_ eks create-pod-identity-association --cluster-name "$CLUSTER_NAME" --namespace "$ns" \
      --service-account "$sa" --role-arn "$role_arn" --tags "$(tags_csv)" >/dev/null
  else
    [ "${#ids[@]}" -eq 1 ] || die "$ns/$sa has ${#ids[@]} Pod Identity associations; expected one"
    id="${ids[0]}"
    current=$(aws_ eks describe-pod-identity-association --cluster-name "$CLUSTER_NAME" \
      --association-id "$id" --query association.roleArn --output text)
    if [ "$current" != "$role_arn" ]; then
      warn "$ns/$sa was associated with $current; pointing it at $role"
      aws_ eks update-pod-identity-association --cluster-name "$CLUSTER_NAME" \
        --association-id "$id" --role-arn "$role_arn" >/dev/null
    fi
  fi
  info "$ns/$sa -> $role"
}
