// Package llmgateway is the llm-gateway plugin: the LLM gateway (LiteLLM on Amazon Bedrock with EKS Pod Identity).
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. Its AWS half is setup-aws's
// plugin_llm_gateway.go.
package llmgateway
