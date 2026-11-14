// Package credprovideraws is the credprovider-aws plugin: the AWS Secrets Manager credential provider for actor egress.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go. Its AWS half is setup-aws's
// plugin_credprovider_aws.go.
package credprovideraws
