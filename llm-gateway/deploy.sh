#!/usr/bin/env bash
# The in-cluster LLM gateway: agentgateway with an Amazon Bedrock backend.
#
# IAM: one role, assumed through EKS Pod Identity by llm-gateway/llm-gateway,
# allowed to invoke ONE model through ONE cross-region inference profile. The
# foundation-model grant is conditioned on that profile, so the role cannot call
# the model directly or any other model. Actors hold no AWS credentials: their
# egress policy lets them reach this gateway, nothing else.
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
IP_ID="$BEDROCK_INFERENCE_PROFILE"

log "IAM role for the LLM gateway ($IP_ID)"
profile=$(aws_ bedrock get-inference-profile --inference-profile-identifier "$IP_ID" --output json) \
  || die "no inference profile $IP_ID in $AWS_REGION (BEDROCK_INFERENCE_PROFILE)"
IP_ARN=$(jq -r .inferenceProfileArn <<<"$profile")
mapfile -t FM_ARNS < <(jq -r '.models[].modelArn' <<<"$profile")
[ "${#FM_ARNS[@]}" -gt 0 ] && [ -n "$IP_ARN" ] || die "inference profile $IP_ID lists no models"
fm_json=$(printf '"%s",' "${FM_ARNS[@]}"); fm_json="[${fm_json%,}]"
cat > "$WORK/policy-llm-gateway.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Sid":"InvokeThroughProfile","Effect":"Allow",
  "Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":"$IP_ARN"},
 {"Sid":"FoundationModelOnlyViaProfile","Effect":"Allow",
  "Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
  "Resource":$fm_json,
  "Condition":{"StringEquals":{"bedrock:InferenceProfileArn":"$IP_ARN"}}}]}
EOF
role_for_sa llm-gateway llm-gateway "$WORK/policy-llm-gateway.json"

log "Deploy the gateway (ClusterIP only)"
sed -e "s|REPLACE_WITH_AWS_REGION|$AWS_REGION|" -e "s|REPLACE_WITH_INFERENCE_PROFILE|$IP_ID|" \
  "$ROOT/llm-gateway/llm-gateway.yaml" | kubectl apply -f -
kubectl -n llm-gateway rollout status deploy/llm-gateway --timeout=180s
ensure_pod_identity llm-gateway deploy/llm-gateway

log "Smoke test from inside the namespace (IAM can take a minute to settle)"
body='{"model":"claude","max_tokens":40,"messages":[{"role":"user","content":"Reply with: ready"}]}'
for _ in $(seq 1 12); do
  kubectl -n llm-gateway delete pod smoke --ignore-not-found >/dev/null
  if out=$(kubectl -n llm-gateway run smoke --rm -i --restart=Never --quiet \
      --image=public.ecr.aws/docker/library/busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec \
      -- wget -qO- --header 'content-type: application/json' --post-data "$body" http://llm-gateway/v1/messages 2>/dev/null) \
      && grep -q '"type":"message"' <<<"$out"; then
    info "Bedrock answered through the gateway"
    exit 0
  fi
  sleep 10
done
die "the LLM gateway did not get an answer from Bedrock; see kubectl -n llm-gateway logs deploy/llm-gateway"
