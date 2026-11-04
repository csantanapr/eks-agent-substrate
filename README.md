# Agent Substrate on Amazon EKS

Run [Agent Substrate](https://github.com/agent-substrate/substrate) on Amazon
EKS with the AWS services it needs: Amazon S3 for actor snapshots, Amazon RDS
for PostgreSQL for state, AWS Secrets Manager for database logins, EKS Pod
Identity for every AWS call, Amazon ECR for images, and Amazon Bedrock for
agent inference. Actors run in gVisor sandboxes and in microVMs (Kata
Containers on Cloud Hypervisor) on EC2 instances with nested virtualization.

No application is exposed: every Service is ClusterIP, the database and
buckets are private, and you reach the demos with port forwards. The
Kubernetes API endpoint is public by default (IAM authenticated); set
`EKS_PUBLIC_ACCESS_CIDRS` to your own address range.

## What you get

```
EKS (Amazon Linux 2023, private subnets)
 ├─ system   node group   Substrate control plane, atenet, LLM gateway
 ├─ gvisor   node group   gVisor worker pods
 └─ microvm  node group   Kata + Cloud Hypervisor worker pods (nested virtualization)
       │
       ├─ Amazon S3          actor snapshots, microVM guest assets
       ├─ Amazon RDS         PostgreSQL, TLS verify-full
       ├─ Secrets Manager    database logins
       ├─ Amazon ECR         demo images
       └─ Amazon Bedrock     agent inference, through the in-cluster LLM gateway
```

| Demo | What it shows |
|---|---|
| counter, gVisor and microVM | An actor counts in memory, in a file and in its root filesystem. Suspend it: the worker is freed and the snapshot goes to S3. The next request resumes it and every count continues. |
| OpenClaw always-on agent (optional) | An always-on gateway holds the chat channel (for example WhatsApp); each conversation's agent is an actor that suspends when idle and resumes on the next message. It thinks with Bedrock and holds no AWS credential. |

## Prerequisites

- An AWS account in the commercial partition and credentials for the AWS CLI
  with permission to create EKS, EC2, VPC, CloudFormation, IAM, S3, RDS,
  Secrets Manager, ECR and Bedrock resources.
- Quota for the nodes: 6 instances, 40 On-Demand standard vCPUs by default
  (`m8i`), plus a NAT gateway and an Elastic IP for the new VPC.
- A region with EKS 1.37, RDS for PostgreSQL `DB_ENGINE_VERSION`, and the
  Bedrock inference profile in `BEDROCK_INFERENCE_PROFILE`. For Anthropic
  models, submit the Bedrock first-time-use form once per account.
- On your machine: `aws` (v2), `eksctl` 0.230 or newer, `kubectl`, `docker`
  with buildx (builds `linux/amd64` images; on arm64 it uses emulation),
  `go` 1.27+, `git`, `jq`, `curl`, `openssl`, `python3` with PyYAML.
- bash 4+ and GNU `sed` and `sort`. On macOS: `brew install bash gnu-sed
  coreutils` and put the GNU tools first on `PATH`.

## Configure

Defaults are in `config.env`. Put your own values in `config.local.env`
(gitignored), for example:

```bash
CLUSTER_NAME=my-substrate
AWS_PROFILE=my-profile
AWS_REGION=us-west-2
TAGS="project=my-substrate team=platform"
```

## Deploy

```bash
./create-cluster.sh   # EKS cluster with eksctl, about 20 minutes
./deploy.sh           # AWS resources, Agent Substrate and the demos, about 25 minutes
```

Both are idempotent: run them again after a change or a failure.
`create-cluster.sh` adds node groups or add-ons a failed first run left out;
it does not change ones that exist (use eksctl for that).

## Try it

```bash
export KUBECONFIG=$PWD/.work/kubeconfig PATH=$PWD/.work/bin:$PATH
kubectl ate get actors -A
demos/counter/verify.sh          # suspend and resume on gVisor and on microVM
```

By hand:

```bash
kubectl ate create actor my-counter -a ate-demo-counter --template counter
kubectl ate proxy actor my-counter -a ate-demo-counter 8001:80 &
curl -s localhost:8001
kubectl ate suspend actor my-counter -a ate-demo-counter
curl -s localhost:8001          # resumes the actor; the counts continue
```

The OpenClaw demo needs a checkout of the always-on-agent repository with its
EKS scripts: set `OPENCLAW_DIR`, or `OPENCLAW_REPO` and `OPENCLAW_REF`, and
optionally `WHATSAPP_PEER`, then run `demos/openclaw/deploy.sh` (and
`demos/openclaw/deploy.sh verify`).

## Move to a new release

Set `SUBSTRATE_TAG` in `config.local.env` (for example `v1.0.0-rc.3`, or
`latest` for the newest tag), or pass it for one run, then reinstall on the
same cluster:

```bash
./clean.sh                                # removes the installed release
SUBSTRATE_TAG=v1.0.0-rc.3 ./deploy.sh     # installs the new one
```

`clean.sh` removes the release the cluster runs (it records the tag at install
time), whatever the config says now. The worker images are pinned to the
digest the new tag names. If `patches/` has no patch for the new tag, the
newest patch that applies cleanly is used with a warning, and `deploy.sh`
stops if none does (see `patches/README.md`). A clean apply only means the
text still fits: check the install, then save the patch under the new tag. Keep the cluster: `delete-cluster.sh` is only needed
to change the node groups or the Kubernetes version.

## Tear down

```bash
./clean.sh            # everything deploy.sh created; the cluster stays
./delete-cluster.sh   # the EKS cluster, its VPC and the launch template
```

Both ask you to type the cluster name; pass `--yes` to skip that.
`clean.sh` deletes the RDS instance without a final snapshot and empties the
S3 buckets. It ends by running `aws/inventory.sh`, which lists anything left
and fails if there is; `delete-cluster.sh` runs it first and refuses to start
until `clean.sh` has finished. Run `aws/inventory.sh --all` any time.

## How it is built, and why

| Area | Choice |
|---|---|
| Nodes | Amazon Linux 2023 EKS-optimized AMI, IMDSv2 only with a hop limit of 1, so pods in their own network namespace cannot read node credentials from IMDS. Host-network pods (CNI, kube-proxy, CSI) still can, so keep host networking to trusted system DaemonSets. AWS access is only through Pod Identity. |
| microVM nodes | Nested virtualization through the launch template's `CpuOptions`, on Intel 7th and 8th generation families (m8i here; `create-cluster.sh` checks the type). No bare metal needed. |
| Network | VPC CNI with NetworkPolicy enforcement on: Substrate ships default-deny policies. Actors have no external egress except what their EgressPolicy allows. |
| Identity | One IAM role per ServiceAccount through EKS Pod Identity, scoped to its buckets, repositories or model. Actors get no AWS identity. |
| S3 | Block Public Access, BucketOwnerEnforced, default encryption, a TLS-only bucket policy, a lifecycle rule for incomplete uploads. A separate read-only bucket for microVM guest assets. |
| RDS | Private subnets, port 5432 open only to the cluster security group, TLS verify-full, separate owner (migrations) and readwrite (runtime) logins in Secrets Manager, master password managed by RDS. |
| Bedrock | An in-cluster agentgateway signs requests with its Pod Identity role, which may invoke one inference profile only. Each actor's EgressPolicy allows only the gateway and swaps a placeholder token for a signed actor JWT. |

For production, also: Multi-AZ RDS with longer backup retention and deletion
protection (`DB_MULTI_AZ`, `DB_BACKUP_DAYS`, `DB_DELETION_PROTECTION`), a
CIDR-restricted EKS API endpoint (`EKS_PUBLIC_ACCESS_CIDRS`), and
`--ateapi-authz-bootstrap-owners` set to your operators instead of the shared
client identity (pass it through `substrate/install.sh`).

## Known limits

- Resource names derive from `CLUSTER_NAME` and the region. Global names (S3
  buckets, IAM roles) also carry the region, and the scripts only reuse or
  delete them when they have the owner tag
  `agent-substrate-on-eks/deployment=<cluster>.<region>`.
- S3 bucket names are deterministic. Once deleted, a name can in principle be
  taken by another account before the next deploy; `deploy.sh` then stops.
- The OpenClaw demo puts one gateway token in its ActorTemplate, so every
  actor and its snapshots hold it. It is a demo credential with no AWS access;
  a production integration would mint a per-actor credential at resume.
- Tested on the commercial AWS partition (`aws`) in us-west-2.

## Layout

| Path | What |
|---|---|
| `create-cluster.sh`, `deploy.sh`, `clean.sh`, `delete-cluster.sh` | The four entry points |
| `config.env` | Settings; overrides go in `config.local.env` |
| `lib/common.sh` | Shared helpers: config, naming, tags, kubeconfig |
| `aws/` | S3, ECR, IAM, RDS, Secrets Manager; the database bootstrap; `inventory.sh` |
| `substrate/` | Fetch, patch and build the installer; install Agent Substrate |
| `patches/` | The installer patch for EKS and S3, and why |
| `llm-gateway/` | agentgateway with the Bedrock backend |
| `demos/` | The counter and OpenClaw demos |
| `slides/` | The talk deck (open `slides/index.html`) |
| `.agents/skills/` | Agent skill for deploying and troubleshooting this repo; `.claude/skills` and `.kiro/skills` link to it |
| `.work/` | Generated: kubeconfig, Substrate clone, tools (gitignored) |
