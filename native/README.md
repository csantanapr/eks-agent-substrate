# substrate-eks: native installer (POC)

A Go installer for Agent Substrate on Amazon EKS that follows the architecture
of [ai-on-gke/substrate-gke](https://github.com/ai-on-gke/substrate-gke)
package for package. It provisions AWS resources with a Go program
(`tools/setup-aws`, the counterpart of `tools/setup-gcp`) and installs the
control plane with upstream's `ate-setup`. No step runs a shell script.

This folder is a proof of concept next to the existing scripts. It changes
none of them, and it creates the same resource names with the same owner tag,
so `clean.sh` still tears down what it makes.

## Quick start

```bash
cd native
make dry-run                                 # every screen, nothing touches AWS
make doctor AWS_PROFILE=my-profile AWS_REGION=us-west-2
CLUSTER_NAME=my-cluster AWS_REGION=us-west-2 make plan   # what Provision would create, read-only
AWS_PROFILE=my-profile AWS_REGION=us-west-2 make run     # the wizard
```

The wizard connects to an existing EKS 1.37+ cluster (Auto Mode or standard
with the `eks-pod-identity-agent` add-on). Creating the cluster is not native
yet; see [Gaps](#gaps).

## Same layout as substrate-gke

| substrate-gke | here | what it does |
|---|---|---|
| `installer/` (Bubble Tea) | `installer/` | the wizard |
| `internal/state` | `internal/state` | step machine (code kept identical) and the answers |
| `internal/execx` | `internal/execx` | runs a command argv and streams output; `--dry-run` replays (copied verbatim) |
| `internal/theme` | `internal/theme` | colours and glyphs (copied verbatim) |
| `internal/steps` | `internal/steps` | live checklists keyed off output lines |
| `internal/doctor` | `internal/doctor` | preflight checks with fixes |
| `internal/gcp` | `internal/awsx` | read-only cloud view (identity, clusters), via the AWS SDK |
| `internal/gkeoverlay` + `deploy/gke` | `internal/eksoverlay` + `deploy/eks` | adds the cloud's kustomize Component to the fetched Substrate tree |
| `internal/snapshot` (deploy step) | `internal/controlplane` | fetch the pinned tree, write the installer's own kubeconfig, run `ate-setup deploy ate-system` |
| `tools/setup-gcp` (cobra + cloud SDK) | `tools/setup-aws` (cobra + AWS SDK for Go v2) | idempotent `check cluster`, `create bucket\|iam\|ecr`, `bootstrap`, `--plan` |

## Picking up substrate-gke's improvements

`upstream.lock` records the substrate-gke commit this tree was synced at and
which files are copied **verbatim** or **adapted**.

```bash
make upstream-check SUBSTRATE_GKE=/path/to/substrate-gke
```

- A verbatim file edited here fails the check: change it upstream, or mark it adapted.
- For each adapted file, it lists the upstream commits since the lock that
  touch the GKE original. Those are the diffs to read and carry over. Then bump
  `commit` in the lock.

## What deploy/eks changes

`deploy/eks/components/eks` is a kustomize Component, like `deploy/gke`:

- the objectstore-plugin sidecars use S3 (`ATE_STORAGE_BACKEND=s3`), with
  credentials from EKS Pod Identity
- atelet uses the EKS AL2023 `ecr-credential-provider` and runs on the
  `gvisor` and `microvm` node groups
- the GKE Managed Prometheus PodMonitoring is dropped (no such CRD on EKS)

One change can't be expressed in kustomize: staging microVM assets in S3.
Upstream `ate-setup` uses `gs://` for every non-Kind install, so that change
stays a Go patch in `deploy/eks/patches/`.

On a pristine v1.0.0 tree, the overlay renders the same manifests, and the same
Go diff, as eks-talk's `patches/substrate-v1.0.0-eks.patch`.

## Gaps

- **Cluster creation.** `create-cluster.sh` (eksctl) still makes the cluster.
  The native equivalent is a `setup-aws create cluster` step: VPC, cluster
  and node roles, the Auto Mode cluster, the nested-virtualization launch
  template, and node groups.
- **Database.** The POC uses Substrate's bundled PostgreSQL. eks-talk's RDS +
  Secrets Manager path belongs in `setup-aws create rds`, the counterpart of
  `setup-gcp create cloudsql`.
- **Plugins and demos.** EFS, S3 Files, Secrets Manager rotation, the
  credential provider, OpenClaw and the counter demo are still scripts.
- **Teardown.** Not native yet; `clean.sh` and `delete-cluster.sh` remove
  what the native installer makes.
- Runtime tools: `aws` CLI (kubectl's token plugin, and the S3 asset upload in
  the Go patch), `kubectl`, `git` and `go`. The installer and `setup-aws`
  themselves call AWS only through the SDK.
