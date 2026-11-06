#!/usr/bin/env bash
# Deploys Agent Substrate and the demos on the cluster create-cluster.sh made.
# Idempotent: run it again after a change or a failure.
#
#   ./deploy.sh            about 25 minutes the first time (RDS takes 10)
#
#   1. AWS resources: S3 buckets, ECR, Pod Identity roles, RDS, Secrets Manager
#   2. Substrate source at SUBSTRATE_TAG with the EKS patch; ate-setup, kubectl-ate
#   3. Substrate's database roles and schema on RDS
#   4. Agent Substrate (published images), gVisor and microVM sandbox classes
#   5. The LLM gateway (agentgateway + Amazon Bedrock)
#   6. The counter demo on gVisor and on microVM
#   7. The OpenClaw always-on agent demo, when configured
#
# The second entry point, after create-cluster.sh. It calls the scripts below
# in order, and each of them is idempotent too. Afterwards,
# demos/counter/verify.sh checks the install end to end.
#
# Learn more:
#   https://github.com/agent-substrate/substrate/tree/v1.0.0
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/concepts/architecture.md
set -euo pipefail
. "$(dirname "$0")/lib/common.sh"
for t in aws kubectl git go docker jq python3 curl; do need "$t"; done
python3 -c 'import yaml' 2>/dev/null || die "python3 needs PyYAML (pip install pyyaml)"
ensure_kubeconfig
info "cluster $CLUSTER_NAME, account $ACCOUNT, region $AWS_REGION"

# Order matters: the database bootstrap runs setup.sql from the Substrate
# source, and the install needs the database and its logins.
phase "AWS resources (S3, ECR, IAM, RDS)"; bash "$ROOT/aws/infra.sh"
phase "Substrate source and tools";        bash "$ROOT/substrate/fetch.sh"
phase "RDS ready + database bootstrap";    bash "$ROOT/aws/db-bootstrap.sh"
phase "Agent Substrate install";           bash "$ROOT/substrate/install.sh"
phase "LLM gateway (Bedrock)";             bash "$ROOT/llm-gateway/deploy.sh"
phase "Counter demo (gVisor + microVM)";   bash "$ROOT/demos/counter/deploy.sh" both
phase "OpenClaw demo";                     bash "$ROOT/demos/openclaw/deploy.sh"
phase "";

log "Done. Everything is ClusterIP; reach it with port forwards."
cat <<EOF
   export KUBECONFIG=$KUBECONFIG PATH=$WORK/bin:\$PATH
   kubectl ate get actors -A
   demos/counter/verify.sh          # suspend/resume on gVisor and microVM
   demos/openclaw/deploy.sh verify  # the OpenClaw agent, when deployed
EOF
