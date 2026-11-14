# The reference patch for Agent Substrate on EKS

`substrate-v1.0.0-eks.patch` is the set of changes Agent Substrate v1.0.0
needs to install on EKS with Amazon S3, written as one patch against the
release. The installer does not apply it: it makes the same changes with a
kustomize Component and one Go patch in
[`native/deploy/eks`](../native/deploy/eks). `make -C native parity` (and the
`native` workflow on every pull request) renders both on a fresh clone of
v1.0.0 and fails if their manifests or their Go changes differ.

| File | Change |
|---|---|
| `cmd/ate-setup/internal/config/config.go` | With `ATE_INSTALL_STORAGE_BACKEND=s3`, no Google service account is required for snapshot transfers. |
| `cmd/ate-setup/internal/steps/microvm.go` | With `ATE_INSTALL_STORAGE_BACKEND=s3`, the microVM guest assets are uploaded with the AWS CLI to `ATE_ASSET_BUCKET`, and the `microvm` SandboxConfig points at `s3://` URLs. |
| `manifests/ate-install/base/kustomization.yaml` | The `objectstore-plugin` sidecars of ate-api-server and atelet use S3 with the AWS SDK default credential chain (EKS Pod Identity). atelet uses the EKS AL2023 image credential provider (`/etc/eks/image-credential-provider`) for ECR pulls, and runs only on the gvisor and microvm node groups (`ate.dev/pool`), where the worker pods are. The GKE `PodMonitoring` is left out. The parity test writes the Region over `REPLACE_WITH_AWS_REGION`. |

These are the gaps an EKS profile in `ate-setup` would close upstream. Once
`ate-setup` installs on EKS with S3 upstream, `native/deploy/eks` and this
patch go away.
