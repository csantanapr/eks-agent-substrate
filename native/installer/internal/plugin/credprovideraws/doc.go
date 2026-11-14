// Package credprovideraws is the credprovider-aws plugin: egress credential
// injection from AWS Secrets Manager, the native form of
// demos/credprovider-aws/{deploy,verify,clean}.sh.
//
// An actor sends a placeholder header; the egress gateway asks the
// credprovider-aws-secret-manager provider for the secret its EgressPolicy
// names, and the request leaves the cluster with the real value. The actor
// never holds the secret.
//
//   - Needs: the control plane install registers the provider with the
//     egress gateway ({"name":"secretsmanager.amazonaws.com","address":
//     "aws-sm-credential-provider.ate-system.svc:50051"}), so run
//     --deploy-control-plane --plugins credprovider-aws first.
//   - AWS half (setup-aws's plugin_credprovider_aws.go): the provider's Pod
//     Identity role, which may read only this deployment's secrets under ate/
//     tagged ate.dev/injectable=true; one injectable probe secret and one
//     without the tag; the two ECR repositories.
//   - Deploy: builds the provider (from its repository at a pinned commit)
//     and Substrate's e2e probe in Go, pushes both to ECR with an in-memory
//     token, applies the default-deny secret policy and the provider (mTLS,
//     only the egress gateway may call it), and makes two probe atespaces:
//     one the policy grants, one it does not.
//   - Verify: the allowed atespace gets the tagged secret injected, the
//     untagged secret and the unlisted atespace are refused, and no value
//     reaches an actor's environment or any log.
//
// It registers itself with plugin.Register from an init function; the
// installer imports it from plugins.go.
package credprovideraws
