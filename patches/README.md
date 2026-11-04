# Patches to Agent Substrate

`substrate/fetch.sh` clones Agent Substrate at `SUBSTRATE_TAG` and applies
`substrate-<tag>-eks.patch` before it builds `ate-setup` and `kubectl-ate`.
The release images are not changed: the patch touches only the installer and
the install manifests it renders.

## substrate-v1.0.0-rc.2-eks.patch

In v1.0.0-rc.2, `ate-setup` treats every cluster that is not kind as GKE with
Cloud Storage. The patch makes it install on EKS with Amazon S3:

| File | Change |
|---|---|
| `cmd/ate-setup/internal/config/config.go` | With `ATE_INSTALL_STORAGE_BACKEND=s3`, no Google service account is required for snapshot transfers. |
| `cmd/ate-setup/internal/steps/microvm.go` | With `ATE_INSTALL_STORAGE_BACKEND=s3`, the microVM guest assets are uploaded with the AWS CLI to `ATE_ASSET_BUCKET`, and the `microvm` SandboxConfig points at `s3://` URLs. |
| `manifests/ate-install/base/kustomization.yaml` | The `objectstore-plugin` sidecars of ate-api-server and atelet use S3 with the AWS SDK default credential chain (EKS Pod Identity). atelet uses the EKS AL2023 image credential provider (`/etc/eks/image-credential-provider`) for ECR pulls. The GKE `PodMonitoring` is left out. `REPLACE_WITH_AWS_REGION` is filled in by `fetch.sh`. |

These are the gaps an EKS profile in `ate-setup` would close upstream.

## A new release

`fetch.sh` uses `substrate-<tag>-eks.patch` when it exists, else the newest
patch here that applies cleanly to the new tag (with a warning). A clean
apply only means the text still fits, so check the install and then save the
patch as `substrate-NEW_TAG-eks.patch`. The clone records the tag, commit,
patch name, patch hash and region in `.eks-patch-applied`; when any of them
changes, `fetch.sh` clones and patches again. When no patch applies, rebase
the newest one:

```bash
git clone --depth 1 --branch NEW_TAG https://github.com/agent-substrate/substrate.git /tmp/s
cd /tmp/s && git apply --3way /path/to/patches/substrate-OLD_TAG-eks.patch
# resolve, keep REPLACE_WITH_AWS_REGION as is, then:
git diff > /path/to/patches/substrate-NEW_TAG-eks.patch
```

Once `ate-setup` installs on EKS with S3 upstream, drop the patch.
