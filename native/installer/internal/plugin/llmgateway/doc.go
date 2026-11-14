// Package llmgateway is the llm-gateway plugin: the in-cluster LLM gateway,
// agentgateway with an Amazon Bedrock backend and EKS Pod Identity, the
// native form of llm-gateway/deploy.sh (which eks-talk's deploy.sh always
// ran, so the wizard preselects it).
//
// Actors ask for "claude" (BEDROCK_INFERENCE_PROFILE, default
// us.anthropic.claude-haiku-5-5) or the newest "claude-haiku",
// "claude-sonnet" or "claude-opus" profile in the Region, read from Bedrock
// at deploy time. Its AWS half is setup-aws's plugin_llm_gateway.go: one IAM
// role for llm-gateway/llm-gateway that may invoke only those profiles.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go.
package llmgateway
