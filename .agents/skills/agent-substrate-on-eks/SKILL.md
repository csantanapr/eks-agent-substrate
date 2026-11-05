---
name: agent-substrate-on-eks
description: Deploy, verify, troubleshoot and tear down Agent Substrate on Amazon EKS with this repository's scripts (eksctl cluster with gVisor and microVM node groups, S3 snapshots, RDS PostgreSQL, Secrets Manager, EKS Pod Identity, ECR, Bedrock through an in-cluster LLM gateway). Use when asked to create the cluster, deploy or redeploy Agent Substrate or its demos, check that suspend and resume work, diagnose a failed step, change the configuration, or clean up the AWS resources.
---

# Agent Substrate on Amazon EKS

This repository installs Agent Substrate on EKS and runs two demos. Four
scripts do everything; this skill is how to drive them, check them and fix
what goes wrong. Repository rules are in `AGENTS.md`; follow them.

## Before anything

1. Read `config.env` and `config.local.env`. The cluster name, region, profile
   and tags come from there. Ask the user for `config.local.env` values you
   cannot infer (profile, region); never put them in `config.env`.
2. Check the identity: `aws sts get-caller-identity` (with `--profile` when
   `AWS_PROFILE` is set). Never print the account id back to the user in a
   shared document.
3. Check the tools: `aws`, `eksctl` (0.230+), `kubectl`, `docker` with buildx,
   `go` (1.27+), `git`, `jq`, `curl`, `openssl`, `python3 -c 'import yaml'`,
   bash 4+ with GNU `sed`/`sort` (on macOS from Homebrew). The prerequisites
   in `README.md` list quotas and Bedrock model access.
4. Use only `.work/kubeconfig`: `export KUBECONFIG=$PWD/.work/kubeconfig
   PATH=$PWD/.work/bin:$PATH`.

## Deploy

| Step | Command | Time | Done when |
|---|---|---|---|
| Cluster | `./create-cluster.sh` | ~20 min | `kubectl get nodes -L ate.dev/pool` shows system, gvisor and microvm nodes Ready, and the printed IMDS table shows `required 1` for every node |
| Everything else | `./deploy.sh` | ~25 min | it prints `Done.`; the counter golden snapshots are ready; the LLM gateway smoke test answered |

Run long scripts in the background with a log file (`./deploy.sh >
.work/deploy.log 2>&1 &`) and read the tail; do not let a tool timeout kill
them. Both are idempotent: after a fix, run the same script again.
`create-cluster.sh` adds missing node groups and add-ons after a failed first
run; a cluster stuck in a state other than ACTIVE needs `./delete-cluster.sh`.

`deploy.sh` runs, in order: `aws/infra.sh`, `substrate/fetch.sh`,
`aws/db-bootstrap.sh`, `substrate/install.sh`, `llm-gateway/deploy.sh`,
`demos/counter/deploy.sh both`, `demos/openclaw/deploy.sh`. Each can be run on
its own to redo one step.

## Verify

```bash
demos/counter/verify.sh            # must end with two "PASS" lines
demos/openclaw/deploy.sh verify    # when OPENCLAW_DIR or OPENCLAW_REPO is set
demos/efs/verify.sh                # after demos/efs/deploy.sh: actor data on EFS
STORAGE=s3files demos/efs/verify.sh  # after demos/s3files/deploy.sh
kubectl -n ate-system get pods     # all Running
```

`verify.sh` suspends each counter actor, resumes it with a request and fails
unless the in-memory count continued. That is the core claim of Substrate:
report the counts, not just "it ran".

## Troubleshooting

Read the real error before changing anything. Known failures and their fixes:

| Symptom | Cause | Fix |
|---|---|---|
| Golden snapshot `GoldenActorCrashed ... 401 Unauthorized` pulling from ECR | atelet pulls actor images with the node's `ecr-credential-provider`, which runs inside the atelet pod and uses atelet's Pod Identity role | The role needs ECR pull on `$ECR_PREFIX/*` (in `aws/infra.sh`); run `aws/infra.sh` again |
| Golden `wakeup probe ... connection refused` | The container crashed at start | Run the image locally (`docker run`) and read its first lines; for example a `FROM scratch` image has no `/` the program can write to |
| Golden `wakeup probe ... HTTP 403` (OpenClaw) | OpenClaw rejects requests from the atenet link-local address as unattributable proxy traffic | The actor image runs OpenClaw on loopback behind the forwarded-header proxy in the always-on-agent repository; do not set `trustedProxies` |
| ate-setup `no matches for kind "PodMonitoring"` | A GKE CRD in the base install | The EKS patch leaves it out; check `.work/substrate-*/.eks-patch-applied` exists |
| ate-setup asks for a transfer service account or `gcloud` | The EKS patch is not applied, or `ATE_INSTALL_STORAGE_BACKEND=s3` is not set | Use `substrate/install.sh`, not a bare `ate-setup` |
| A PVC stays `Pending` | No default StorageClass | `create-cluster.sh` installs the encrypted gp3 default; run it again |
| LLM gateway returns 403 from Bedrock | IAM still settling after create, or the model is not enabled for the account | Wait a minute and retry; check Bedrock model access for `BEDROCK_INFERENCE_PROFILE` |
| A pod signs AWS calls with the node role | IMDS reachable from pods | Every node group must have hop limit 1; the gateway also sets `AWS_EC2_METADATA_DISABLED=true` |
| RDS bootstrap `must be able to SET ROLE "substrate_owner"` | The RDS master user is `rds_superuser`, not a superuser | `aws/db-bootstrap.sh` creates the owner role and grants it to the master user first |
| `delete-cluster.sh` refuses | `aws/inventory.sh` found resources `clean.sh` should have removed | Run `./clean.sh` and read its WARN lines |
| `... exists but is not owned by ...` | A bucket, IAM role or launch template with the same name lacks this deployment's owner tag | Pick another `CLUSTER_NAME`, or remove the other resource yourself after checking whose it is |
| `create-secret ...: scheduled for deletion` | A secret deleted moments ago still holds its name | `aws/infra.sh` retries for two minutes; run `./deploy.sh` again |
| `kubectl-ate ... Text file busy` | A running `kubectl ate proxy` holds the binary | Stop the old port forwards (`pkill -f "kubectl-ate proxy"`) |
| Resume fails with `failed to get volume plugin ... connection refused` | Substrate's EFS CSI controller (`kube-system/efs-csi-controller-ate`) is not serving | Read its `envoy` container logs; `kubectl -n kube-system rollout status deploy/efs-csi-controller-ate` |
| `CSI CreateVolume failed ... Access denied` | The controllers' Pod Identity role lacks an EFS or S3 Files permission | Read the `efs-plugin` logs in `efs-csi-controller-ate`: they name the action; fix the policy in `demos/efs/deploy.sh` or `demos/s3files/deploy.sh` |
| The counter reports `preserved file counter: -1` on EFS | The access point's uid does not match the actor's user, and actors have no `CAP_DAC_OVERRIDE` | Set `EFS_UID`/`EFS_GID` to the actor's user and run the deploy again; new actors get the new uid |
| Demo actors are `ACTOR_STATE_CRASHED` after `demos/efs/deploy.sh` | Turning on the `ExternalVolumes` preview reinstalls Substrate, which rolls the worker pods | `kubectl ate revert actor NAME -a ATESPACE` |

Logs to read: `kubectl -n ate-system logs ds/atelet -c atelet`,
`kubectl -n ate-system logs deploy/ate-api-server`, the worker pod in the
demo's namespace (it carries the actor's stdout), `kubectl ate get
actor-template NAME -a ATESPACE -o json | jq .status`.

## Change the configuration

- Region, instance types, node counts, Kubernetes version, RDS size, Bedrock
  profile, tags: `config.local.env`, then the script that owns it
  (`create-cluster.sh` for nodes, `deploy.sh` for the rest). Node group
  changes on an existing cluster need eksctl directly or a delete and create.
- A new Substrate release (for example rc.3): keep the cluster, run
  `./clean.sh --yes`, then `SUBSTRATE_TAG=v1.0.0-rc.3 ./deploy.sh` (or set it in
  `config.local.env`; `latest` picks the newest tag). `clean.sh` removes the
  release recorded in `.work/installed-substrate-tag`; worker images are
  resolved to the new tag's digest. If `fetch.sh` reports that no patch
  applies, rebase the newest patch as `patches/README.md` shows, and check the
  release notes for changes to the counter templates or the kubectl-ate flags.
  Then run `demos/counter/verify.sh`.
- Production settings (Multi-AZ, backups, deletion protection, a private API
  endpoint, bootstrap owners): see "How it is built" in `README.md`.

## Tear down

```bash
./clean.sh --yes          # everything deploy.sh made; the cluster stays
./delete-cluster.sh --yes # the cluster, its VPC and the launch template
```

Get the user's explicit go-ahead before either: they delete data (RDS without
a final snapshot, both S3 buckets, the ECR images). `clean.sh` must succeed
before `delete-cluster.sh`. Both run `aws/inventory.sh`, which lists anything
left (Pod Identity associations, IAM roles, RDS and its groups, secrets,
buckets, ECR repositories, EBS volumes, namespaces, CRDs) and fails if there
is any; `aws/inventory.sh --all` also checks the cluster and the launch
template.

## Never

- Expose anything publicly (LoadBalancer, Ingress, NodePort, public RDS).
- Create static AWS access keys or put credentials in an actor.
- Write the account id, a password or a token into a tracked file or a log.
- Change `~/.kube/config` or the user's current kube context.
- Delete AWS resources by hand instead of through `clean.sh`.
