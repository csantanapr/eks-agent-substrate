# substrate-eks: native installer

A Go installer for Agent Substrate on Amazon EKS. It follows the architecture
of [ai-on-gke/substrate-gke](https://github.com/ai-on-gke/substrate-gke)
package for package: a Bubble Tea wizard over Go provisioning code. It calls
AWS through the AWS SDK for Go v2 and installs the control plane with
upstream's `ate-setup`. No install step runs a shell script or `bash -c`.

## What it creates

The wizard runs these steps in order. Each one is idempotent: a rerun creates
nothing that already exists, and a run that stopped part way is completed.

| Step | Command | What it creates |
|---|---|---|
| Cluster | `setup-aws create cluster` | A VPC with public and private subnets across two or more Availability Zones and a NAT gateway. The cluster and node IAM roles. An EKS cluster (Auto Mode by default, standard optional) whose public endpoint accepts only `EKS_PUBLIC_ACCESS_CIDRS`, with an access entry for you and the add-ons. A launch template with nested virtualization, IMDSv2 (hop limit 1) and encrypted gp3. The `system`, `gvisor` and `microvm` managed node groups, labeled `ate.dev/pool` and tainted for their sandbox class. |
| Provision | `setup-aws bootstrap` | S3 buckets for snapshots and microVM assets. EKS Pod Identity roles for ate-api-server and atelet. Private ECR repositories. |
| Database | `setup-aws create rds` | A private RDS for PostgreSQL instance in the private subnets. Port 5432 is open only to the cluster security group. RDS manages the admin password, and the owner and read-write logins are in Secrets Manager. The schema is created from inside the cluster. With `--database=bundled`, Substrate's bundled PostgreSQL runs in the cluster instead, on an encrypted gp3 EBS volume from the installer's StorageClass. |
| Control plane | `ate-setup deploy ate-system` | A StorageClass for encrypted gp3 EBS volumes that carry the owner tag and `TAGS` (`substrate-eks-auto-ebs` on Auto Mode, `substrate-eks-ebs` on a standard cluster), the default class on a cluster the installer created. The pinned Substrate release with its published images, the gVisor and microVM sandbox classes, the microVM assets staged in S3, and the EKS changes in `deploy/eks`. |
| Demo (`--demo`) | `ate-setup deploy demo counter`, `counter-microvm` | The counter demo on gVisor and on microVM, each pinned to its node group. |

Every AWS resource carries the owner tag
`agent-substrate-on-eks/deployment=<cluster>.<region>` and `TAGS` (default
`project=agent-substrate auto-delete=no`). A bucket, IAM role, launch
template or security group that already has the name but not this owner tag
is never reused, changed or deleted: the run stops and names it.

EBS volumes for PersistentVolumeClaims get the same tags from the
StorageClass. EKS Auto Mode adds custom tags only when the cluster role
allows it, so `setup-aws create cluster` gives its Auto Mode cluster role one
statement for that (`ec2:CreateVolume` with this cluster's and this
deployment's tags). On an Auto Mode cluster you created yourself, the volume
gets no owner tag; teardown and inventory find it by its
`eks:eks-cluster-name` tag and namespace `ate-system`.

Services are ClusterIP. No Ingress, LoadBalancer or NodePort is created.
Workloads get AWS access from EKS Pod Identity, with no static keys.

## Quick start

You need `git`, `go`, `make`, the `aws` CLI and `kubectl`, and AWS
credentials for the account.

```bash
./native/install.sh                 # from a checkout: build the installer and open the wizard
curl -fsSL https://raw.githubusercontent.com/csantanapr/eks-agent-substrate/main/native/install.sh | bash
```

`install.sh` only fetches, builds and starts the installer. Every AWS and
Kubernetes call is made by the installer.

Or use the Makefile in `native/`:

```bash
cd native
make dry-run                                            # every screen, nothing touches AWS
make doctor AWS_PROFILE=my-profile AWS_REGION=us-west-2 # preflight checks
AWS_PROFILE=my-profile AWS_REGION=us-west-2 make run-demo
make verify AWS_PROFILE=my-profile AWS_REGION=us-west-2 CLUSTER_NAME=my-cluster
```

| Target | What it does |
|---|---|
| `make build` | Builds `bin/substrate-eks`. |
| `make run` | Opens the wizard. `AWS_PROFILE`, `AWS_REGION` and `CLUSTER_NAME` preselect answers. |
| `make run-demo` | The wizard, ending with the counter demo on gVisor and microVM. |
| `make dry-run` | Walks every screen without touching AWS. |
| `make doctor` | Runs the preflight checks only. |
| `make plan` | Reports what Provision would create, changing nothing. |
| `make verify` | Suspends and resumes one counter actor per sandbox class. It passes when the memory, file and rootfs counters all continue and the snapshot is in the S3 bucket. |
| `make teardown` | Removes what the installer created. See [Teardown](#teardown). |

The Makefile and `install.sh` run Go with `GOTOOLCHAIN=local` and
`GOPROXY=direct`, so module downloads go straight to their source.

### What the demo shows

The counter answers each request with three counters: one in memory, one in a
file on a durable volume, and one on the root filesystem. A suspend saves the
actor to a snapshot and frees its worker. The resume restores it from that
snapshot, so all three counters go on from where they were. `make verify`
reads the counters through `kubectl ate proxy`, suspends and resumes the
actor, reads them again, and finds the snapshot under
`atespaces/<atespace>/actors/<uid>/snapshots/` in the snapshot bucket. An
actor left CRASHED by an earlier reinstall is reverted to its last snapshot
first.

Upstream's demo worker pools do not choose a node. On EKS, atelet runs only on
the `gvisor` and `microvm` node groups, so the installer pins each demo pool to
its group (`nodeSelector: ate.dev/pool: <class>`) before `ate-setup` applies
it.

## Teardown

```bash
make teardown CLUSTER_NAME=my-cluster AWS_REGION=us-west-2 AWS_PROFILE=my-profile
make teardown CLUSTER_NAME=my-cluster AWS_REGION=us-west-2 AWS_PROFILE=my-profile KEEP_CLUSTER=true
```

Teardown removes Substrate from the cluster, then deletes in reverse order
everything the installer created: Pod Identity associations and roles, the RDS
instance and its logins, the S3 buckets, the ECR repositories, the node groups,
the cluster, and the launch template, VPC and network. It deletes only
resources that carry this deployment's owner tag. A second run finds nothing
to delete. `KEEP_CLUSTER=true` removes Substrate and its AWS data but leaves
the cluster.

The wizard's Complete screen offers teardown and prints the exact command.
`setup-aws inventory` lists every resource that still carries the owner tag.

## Same layout as substrate-gke

| substrate-gke | here | what it does |
|---|---|---|
| `installer/` (Bubble Tea) | `installer/` | the wizard |
| `internal/state` | `internal/state` | the step machine (code kept identical) and the answers |
| `internal/execx` | `internal/execx` | runs a command argv and streams its output; `--dry-run` replays (copied verbatim) |
| `internal/theme` | `internal/theme` | colours and glyphs (copied verbatim) |
| `internal/steps` | `internal/steps` | live checklists keyed off output lines, with titles from `setup-aws/phases` |
| `internal/doctor` | `internal/doctor` | preflight checks with fixes |
| `internal/gcp` | `internal/awsx` | read-only AWS view (identity, clusters) through the SDK, and the installer's own kubeconfig |
| `internal/gkeoverlay` + `deploy/gke` | `internal/eksoverlay` + `deploy/eks` | adds the EKS kustomize Component to the fetched Substrate tree |
| `internal/snapshot` (deploy step) | `internal/controlplane` | fetches the pinned tree, runs `ate-setup deploy ate-system`, and removes Substrate on teardown |
| `internal/snapshot` (probe steps) | `internal/probe` | probes each cluster for `ate-system` and badges it in the picker (`installed vX`, `partial install`, `ready`) |
| `internal/snapshot` (`DeployDemo`, `make run-demo`) | `internal/demo` | the counter demo on gVisor and microVM, and `make verify` |
| `tools/setup-gcp` (cobra + cloud SDK) | `tools/setup-aws` (cobra + AWS SDK for Go v2) | idempotent `check cluster`, `create bucket\|iam\|ecr`, `bootstrap`, `--plan` |
| `setup-gcp create cluster` | `setup-aws create cluster` | the VPC, roles, cluster, launch template and node groups |
| `setup-gcp create cloudsql` | `setup-aws create rds` | the database and its logins |
| `tools/cleanup-gcp`, `tools/setup-gcp/teardown.sh` | `setup-aws teardown` | deletes what carries the owner tag, in Go |
| (none) | `setup-aws inventory` | lists every resource with the owner tag |
| `install.sh` | `native/install.sh` | the launcher |
| `Makefile` | `native/Makefile` | the same targets, plus `plan`, `verify` and `parity` |
| `.github/workflows/test.yml` | `.github/workflows/native.yaml` | gofmt, vet and tests for both modules, and render parity |

## What deploy/eks changes

`deploy/eks/components/eks` is a kustomize Component, like `deploy/gke`:

- the objectstore-plugin sidecars use S3 (`ATE_STORAGE_BACKEND=s3`), with
  credentials from EKS Pod Identity
- atelet uses the EKS AL2023 `ecr-credential-provider` and runs on the
  `gvisor` and `microvm` node groups
- the GKE Managed Prometheus PodMonitoring is dropped (no such CRD on EKS)

One change can't be expressed in kustomize: staging microVM assets in S3.
Upstream `ate-setup` uses `gs://` for every install outside Kind, so that
change stays a Go patch in `deploy/eks/patches/`.

The overlay renders the same manifests, and the same Go change, as the script
path's `patches/substrate-v1.0.0-eks.patch` on the same release. `make parity`
checks this on a fresh clone of upstream, and CI runs it on every pull request.

## Picking up substrate-gke's improvements

`upstream.lock` records the substrate-gke commit this tree follows, and which
files are copied **verbatim** or **adapted**.

```bash
make upstream-check SUBSTRATE_GKE=/path/to/substrate-gke
```

- A verbatim file edited here fails the check. Change it upstream, or mark it
  adapted.
- For each adapted file, it lists the upstream commits since the lock that
  touch the GKE original. Those are the diffs to read and carry over. Then
  bump `commit` in the lock.

## Upstream asks

These would let this installer drop its overlay and patches:

1. An object-store scheme for microVM assets on installs outside Kind
   (`ate-setup` hard-codes `gs://`). This is the only remaining Go patch.
2. `ate-setup deploy --component`
   ([agent-substrate/substrate#3179](https://github.com/agent-substrate/substrate/pull/3179))
   or `ate-setup render` (#3240), to retire the overlays.
3. A way to create cluster-scoped prerequisites before the bundled PostgreSQL
   is deployed.
4. A node selector for the demo worker pools in `ate-setup deploy demo`, so
   the installer need not edit the demo templates.

## Contributing

`make test` runs gofmt, go vet and the unit tests of both Go modules.
`make parity` runs the render check above, and `make upstream-check` the sync
check. Runtime tools besides Go: the `aws` CLI (kubectl's token plugin, and
the S3 asset upload in the Go patch), `kubectl` and `git`.
