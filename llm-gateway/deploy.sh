#!/usr/bin/env bash
# The in-cluster LLM gateway: agentgateway with an Amazon Bedrock backend.
#
# Models actors can ask for, by name:
#   claude          BEDROCK_INFERENCE_PROFILE (config.env), the default
#   claude-haiku    the newest Claude Haiku inference profile in the Region
#   claude-sonnet   the newest Claude Sonnet one
#   claude-opus     the newest Claude Opus one
# "Newest" is read from Bedrock's list of active inference profiles at deploy
# time, with the same prefix as BEDROCK_INFERENCE_PROFILE (us., eu., global.),
# so a new model version is picked up by the next deploy.
#
# IAM: one role, assumed through EKS Pod Identity by llm-gateway/llm-gateway,
# allowed to invoke only these inference profiles. Each foundation-model grant
# is conditioned on its own profile, so the role cannot call a model directly
# or any other model. Actors hold no AWS credentials: their egress policy lets
# them reach this gateway, nothing else.
#
# deploy.sh calls it after the Substrate install. It applies
# llm-gateway/llm-gateway.yaml and ends with a smoke test of every model name.
#
# Learn more:
#   https://docs.aws.amazon.com/bedrock/latest/userguide/inference-profiles.html
#   https://docs.aws.amazon.com/bedrock/latest/userguide/inference-profiles-prereq.html
#   https://agentgateway.dev/docs/standalone/latest/integrations/llm/providers/bedrock/
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
DEFAULT_ID="$BEDROCK_INFERENCE_PROFILE"
PREFIX="${DEFAULT_ID%%.*}"

log "Inference profiles (newest per family, prefix $PREFIX.)"
profiles=$(aws_ bedrock list-inference-profiles --output json) || die "cannot list the inference profiles in $AWS_REGION"
# latest FAMILY: the ACTIVE PREFIX.anthropic.claude-FAMILY-* profile with the
# highest version. The minor version has at most two digits, so a date such as
# claude-sonnet-4-20250514 reads as 4.0.
latest() {
  jq -r --arg p "$PREFIX.anthropic.claude-$1-" '[.inferenceProfileSummaries[]
      | select(.status == "ACTIVE" and (.inferenceProfileId | startswith($p)))
      | .inferenceProfileId as $id
      | ($id | ltrimstr($p) | capture("^(?<maj>[0-9]+)(-(?<min>[0-9]{1,2}))?(-|$)")) as $v
      | {id: $id, maj: ($v.maj | tonumber), min: (($v.min // "0") | tonumber)}]
    | sort_by(.maj, .min) | last | .id // empty' <<<"$profiles"
}
HAIKU_ID=$(latest haiku); SONNET_ID=$(latest sonnet); OPUS_ID=$(latest opus)
for v in HAIKU_ID SONNET_ID OPUS_ID; do
  [ -n "${!v}" ] || die "no active $PREFIX. Claude ${v%_ID} inference profile in $AWS_REGION"
done
info "claude         $DEFAULT_ID"
info "claude-haiku   $HAIKU_ID"
info "claude-sonnet  $SONNET_ID"
info "claude-opus    $OPUS_ID"

log "IAM role for the LLM gateway"
# Two statements per profile: invoke the profile, and invoke its foundation
# models only when the request comes through that profile.
statements=()
for id in $(printf '%s\n' "$DEFAULT_ID" "$HAIKU_ID" "$SONNET_ID" "$OPUS_ID" | sort -u); do
  profile=$(aws_ bedrock get-inference-profile --inference-profile-identifier "$id" --output json) \
    || die "no inference profile $id in $AWS_REGION"
  statements+=("$(jq -c '.inferenceProfileArn as $p | [.models[].modelArn] as $fm
    | if ($fm | length) == 0 then error("lists no models") else . end
    | [{Effect: "Allow", Action: ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"], Resource: $p},
       {Effect: "Allow", Action: ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"], Resource: $fm,
        Condition: {StringEquals: {"bedrock:InferenceProfileArn": $p}}}][]' <<<"$profile")") \
    || die "inference profile $id lists no models"
done
printf '%s\n' "${statements[@]}" | jq -s '{Version: "2012-10-17", Statement: .}' > "$WORK/policy-llm-gateway.json"
role_for_sa llm-gateway llm-gateway "$WORK/policy-llm-gateway.json"

log "Deploy the gateway (ClusterIP only)"
sed -e "s|REPLACE_WITH_AWS_REGION|$AWS_REGION|" \
    -e "s|REPLACE_WITH_DEFAULT_PROFILE|$DEFAULT_ID|" \
    -e "s|REPLACE_WITH_HAIKU_PROFILE|$HAIKU_ID|" \
    -e "s|REPLACE_WITH_SONNET_PROFILE|$SONNET_ID|" \
    -e "s|REPLACE_WITH_OPUS_PROFILE|$OPUS_ID|" \
  "$ROOT/llm-gateway/llm-gateway.yaml" | kubectl apply -f -
# The config is a ConfigMap; restart so a changed model list takes effect.
kubectl -n llm-gateway rollout restart deploy/llm-gateway >/dev/null
kubectl -n llm-gateway rollout status deploy/llm-gateway --timeout=180s
ensure_pod_identity llm-gateway deploy/llm-gateway

# The NetworkPolicy admits any pod in the llm-gateway namespace, so a
# throwaway busybox pod there can call the gateway.
log "Smoke test of every model name (IAM can take a minute to settle)"
smoke() { # MODEL
  local body out
  body=$(printf '{"model":"%s","max_tokens":40,"messages":[{"role":"user","content":"Reply with: ready"}]}' "$1")
  for _ in $(seq 1 12); do
    kubectl -n llm-gateway delete pod smoke --ignore-not-found >/dev/null
    if out=$(kubectl -n llm-gateway run smoke --rm -i --restart=Never --quiet \
        --image=public.ecr.aws/docker/library/busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec \
        -- wget -qO- --header 'content-type: application/json' --post-data "$body" http://llm-gateway/v1/messages 2>/dev/null) \
        && grep -q '"type":"message"' <<<"$out"; then
      info "$1: Bedrock answered through the gateway"
      return 0
    fi
    sleep 10
  done
  die "$1: the LLM gateway did not get an answer from Bedrock; see kubectl -n llm-gateway logs deploy/llm-gateway"
}
for m in claude claude-haiku claude-sonnet claude-opus; do smoke "$m"; done
