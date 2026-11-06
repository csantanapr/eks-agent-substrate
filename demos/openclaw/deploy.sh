#!/usr/bin/env bash
# Optional: the OpenClaw always-on agent demo. An always-on gateway holds the
# chat channel; each conversation's agent is a Substrate actor that suspends
# when idle and resumes on the next message, and thinks with Amazon Bedrock
# through the LLM gateway without holding any AWS credential.
#
# Needs a checkout of the always-on-agent repository with its demo/eks
# scripts: OPENCLAW_DIR (a local path), or OPENCLAW_REPO + OPENCLAW_REF (cloned
# into .work/). Skipped when neither is set.
#
#   demos/openclaw/deploy.sh [verify]
#
# deploy.sh calls it as its last step. Each actor's egress policy allows only
# the LLM gateway, and swaps a placeholder token for a signed actor JWT on
# the way out.
#
# Learn more:
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/egress-credential-injection.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/actor-jwts.md
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"

dir="${OPENCLAW_DIR:-}"
if [ -z "$dir" ] && [ -n "${OPENCLAW_REPO:-}" ]; then
  dir="$WORK/always-on-agent"
  [ -d "$dir/.git" ] || git clone --quiet "$OPENCLAW_REPO" "$dir"
  # Follow OPENCLAW_REPO and OPENCLAW_REF on every run, not only the first.
  git -C "$dir" remote set-url origin "$OPENCLAW_REPO"
  git -C "$dir" fetch --quiet origin
  git -C "$dir" checkout --quiet --detach "origin/${OPENCLAW_REF:-HEAD}" 2>/dev/null \
    || git -C "$dir" checkout --quiet --detach "${OPENCLAW_REF:?}"
  info "always-on-agent at $(git -C "$dir" rev-parse --short HEAD)"
fi
if [ -z "$dir" ]; then
  info "OpenClaw demo skipped: set OPENCLAW_DIR or OPENCLAW_REPO in config.local.env"
  exit 0
fi
[ -x "$dir/demo/eks/deploy-demo.sh" ] || die "$dir has no demo/eks/deploy-demo.sh"

# The demo images carry the same kubectl-ate the cluster runs.
mkdir -p "$dir/build/bin"
# Copy and rename, so a running kubectl-ate (a port forward) does not block it.
cp "$WORK/bin/kubectl-ate" "$dir/build/bin/.kubectl-ate.new"
mv -f "$dir/build/bin/.kubectl-ate.new" "$dir/build/bin/kubectl-ate"

export KUBE_CONTEXT
KUBE_CONTEXT="$(kubectl config current-context)"
export PROFILE="${AWS_PROFILE:-}" REGION="$AWS_REGION" ECR_PREFIX WORKER_REPLICAS="${OPENCLAW_WORKERS:-3}"
# The release the cluster runs, and its gVisor worker image pinned by digest.
export SUBSTRATE_VERSION="$SUBSTRATE_TAG"
WORKER_IMAGE="$(pinned_image ateworker-gvisor)" || die "cannot resolve ateworker-gvisor:$SUBSTRATE_TAG"
export WORKER_IMAGE
export WHATSAPP_PEER="${WHATSAPP_PEER:-}"
if [ "${1:-}" = verify ]; then
  log "Verify the OpenClaw demo"
  bash "$dir/demo/eks/verify-demo.sh"
else
  log "Deploy the OpenClaw demo from $dir"
  bash "$dir/demo/eks/deploy-demo.sh"
fi
