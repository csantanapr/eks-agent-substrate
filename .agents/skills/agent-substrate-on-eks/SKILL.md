---
name: agent-substrate-on-eks
description: Install, verify, troubleshoot and tear down Agent Substrate on Amazon EKS with this repository's native Go installer (substrate-eks and setup-aws, under native/): an EKS cluster with gVisor and microVM node groups, S3 snapshots, RDS PostgreSQL, Secrets Manager, EKS Pod Identity, ECR, and plugins such as the Bedrock LLM gateway, EFS and S3 Files volumes, login rotation, the Secrets Manager credential provider, OpenClaw and mode 3. Use when asked to create the cluster, install or reinstall Agent Substrate or a plugin, check that suspend and resume work, diagnose a failed step, change the configuration, or remove the AWS resources.
---

# Agent Substrate on Amazon EKS

This repository installs Agent Substrate on EKS with a Go installer in
`native/`: the wizard `substrate-eks` (`native/installer`) and the AWS tool
`setup-aws` (`native/tools/setup-aws`). This skill is how to drive them,
check them and fix what goes wrong. Repository rules are in `AGENTS.md`;
follow them.

## Before anything

1. Read `config.env` and `config.local.env`. The cluster name, Region,
   profile, tags and release come from there. Ask the user for values you
   cannot infer (profile, Region); put them in `config.local.env` or pass
   them as flags, never in `config.env`.
2. Check the identity: `aws sts get-caller-identity` (with `--profile` when
   `AWS_PROFILE` is set). Never print the account id back to the user in a
   shared document.
3. Check the tools and the account: `make -C native doctor AWS_PROFILE=...
   AWS_REGION=...`. It needs `go` 1.27+, `git`, `make`, the `aws` CLI v2 and
   `kubectl`. `README.md` lists quotas and Bedrock model access.
4. Every `go` command runs with `GOTOOLCHAIN=local GOPROXY=direct
   GOSUMDB=off`. The Makefile and `native/install.sh` set them; export them
   when you call `go` yourself.
5. Use only the installer's kubeconfig for the cluster:
   `export KUBECONFIG=~/.cache/substrate-eks/kubeconfig/<cluster>.<region>.yaml`
   (`~/Library/Caches/...` on macOS). The control plane step writes it.

## Install

The wizard (`./native/install.sh` or `make -C native run`) needs a
terminal. When the user runs it, tell them the keys: `enter` through the
first screens, `n` on Settings, `space` to pick plugins and `enter`, `y` on
Review (nothing changes in AWS before that), `s` saves the screen.
`--dry-run` walks every screen without touching AWS.

From an agent session, run the same steps the wizard runs, one command each,
from `native/`, in the background with a log file, and read the tail. Set
`C=<cluster> R=<region> P=<profile>` first.

| Step | Command | Time | Done when |
|---|---|---|---|
| Cluster | `CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P go -C tools/setup-aws run . create cluster` | ~20 min | it logs `Cluster ready.` |
| Provision | `CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P go -C tools/setup-aws run . bootstrap` | ~1 min | `Bootstrap completed successfully.` |
| Database | `CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P go -C tools/setup-aws run . create rds` | ~8 min | it ends without an error (skip it with `--database bundled`) |
| Control plane | `go -C installer run . --deploy-control-plane --cluster $C --region $R --profile $P --database rds --snap-bucket $C-snap-<account>-$R --asset-bucket $C-assets-<account>-$R --plugins llm-gateway` | ~2 min | `Substrate control plane is ready` |
| Demo | `go -C installer run . --deploy-demo --verify --cluster $C --region $R --profile $P` | ~11 min | both classes report their counters continued |
| Plugins | `go -C installer run . --plugin-action deploy --plugins llm-gateway --cluster $C --region $R --profile $P` | per plugin | `Plugin ready: <id>` for each |

- Every step is idempotent: after a fix, run the same step again. A rerun of
  a step that already finished reports `created=0 changed=0`.
- Pass every plugin you will install to `--deploy-control-plane --plugins`:
  the control plane is installed once with what they need (preview gates,
  credential providers, a mode 3 build). The installed set is recorded in
  ConfigMap `kube-system/substrate-eks-plugins`, and later control plane
  deploys keep it.
- Plugins: `llm-gateway` (default), `efs`, `s3files` (needs `efs`),
  `credprovider-aws`, `db-rotation` (needs RDS), `openclaw` (needs `efs` and
  its images pinned by digest in `OPENCLAW_GATEWAY_IMAGE`,
  `OPENCLAW_ACTOR_IMAGE`, `OPENCLAW_DASHBOARD_IMAGE`), `mode3`, `mode3-iso`
  (needs `mode3` and Auto Mode). `efs` and `s3files` build on the counter
  demo: deploy the demo first.
- `--plan` on a `setup-aws` command reports what it would do and writes
  nothing.
- A step that outlives the AWS session waits up to 30 minutes for refreshed
  credentials for the same account and role, then goes on.

## Verify

```bash
make -C native verify CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P
go -C native/installer run . --plugin-action verify --plugins efs,s3files --cluster $C --region $R --profile $P
kubectl -n ate-system get pods     # all Running
```

`--verify` suspends one counter actor per sandbox class, resumes it with a
request and fails unless the memory, file and root filesystem counters all
continued and the snapshot is in the S3 bucket. That is the core claim of
Substrate: report the counts, not just "it ran". It waits up to 10 minutes
for the snapshot's S3 copy.

## Troubleshooting

Read the real error before changing anything. The wizard's log is under
`~/.cache/substrate-eks/logs/`. Known failures and their fixes:

| Symptom | Cause | Fix |
|---|---|---|
| Golden snapshot `GoldenActorCrashed ... 401 Unauthorized` pulling from ECR | atelet pulls actor images with the node's `ecr-credential-provider`, through atelet's Pod Identity role | `setup-aws bootstrap` gives that role ECR pull on the cluster's repositories; run it again |
| Golden `wakeup probe ... connection refused` | The container crashed at start | Read the worker pod's log in the demo's namespace; it carries the actor's stdout |
| ate-setup `no matches for kind "PodMonitoring"`, or it asks for a transfer service account or `gcloud` | Upstream's install ran without `deploy/eks` | Use `--deploy-control-plane`, not a bare `ate-setup` |
| A PVC of your own stays `Pending` on a cluster you created yourself | No default StorageClass: the installer's class is the default only on a cluster it created | Name `substrate-eks-ebs` (`substrate-eks-auto-ebs` on Auto Mode) in the claim |
| LLM gateway returns 403 from Bedrock | IAM still settling after create, or the model is not enabled for the account | Wait a minute and rerun `--plugin-action verify --plugins llm-gateway`; check Bedrock model access for `BEDROCK_INFERENCE_PROFILE` |
| A pod signs AWS calls with the node role | IMDS reachable from pods | Every node group keeps hop limit 1; the gateway also sets `AWS_EC2_METADATA_DISABLED=true` |
| `... belongs to CloudFormation stack ... (made by eksctl)` | The cluster was made by eksctl, so `setup-aws teardown` leaves it alone | `setup-aws teardown --keep-cluster`, then `eksctl delete cluster --name $C --region $R` |
| `... is not owned by ...` | A bucket, IAM role, launch template or security group with the same name lacks this deployment's owner tag | Pick another `CLUSTER_NAME`, or remove the other resource yourself after checking whose it is |
| The wizard refuses to install on a cluster that runs Substrate | The reinstall guard | Tear that install down first (it offers removal), or pick another cluster |
| `--plugin-action deploy` refuses a plugin | A plugin it requires is missing, it needs Auto Mode, or it builds on the counter demo | Deploy what the message names first |
| Resume fails with `failed to get volume plugin ... connection refused` | Substrate's EFS CSI controller (`kube-system/efs-csi-controller-ate`) is not serving | Read its `envoy` container logs; `kubectl -n kube-system rollout status deploy/efs-csi-controller-ate` |
| `CSI CreateVolume failed ... Access denied` | The controllers' Pod Identity role lacks an EFS or S3 Files permission | Read the `efs-plugin` logs in `efs-csi-controller-ate`: they name the action |
| Demo actors are `ACTOR_STATE_CRASHED` with "worker pod went away" | The worker pods rolled while they ran, for example on a control plane reinstall | `kubectl ate revert actor NAME -a ATESPACE`; `--verify` reverts its own |
| `--plugin-action verify --plugins db-rotation` stops at "rotation did not finish" | The rotation function failed a step | `aws logs tail /aws/lambda/$C-db-owner` (or `-db-readwrite`); it logs createSecret, setSecret, testSecret and finishSecret |
| A step stops with `ExpiredToken` after 30 minutes | The AWS session ended and was not refreshed | Refresh the profile's credentials and rerun the step |

Logs to read: `kubectl -n ate-system logs ds/atelet -c atelet`,
`kubectl -n ate-system logs deploy/ate-api-server`, the worker pod in the
demo's namespace, `kubectl ate get actor-template NAME -a ATESPACE -o json |
jq .status`.

## Change the configuration

- Cluster knobs (Kubernetes version, Auto Mode, node types and counts, API
  CIDRs), `TAGS`, the release and its image repository: the wizard's
  Settings screen, which writes `config.local.env`, or edit
  `config.local.env`. Node groups and Auto Mode are fixed once the cluster
  exists; change them with a new cluster.
- A new Substrate release (for example v1.0.1): remove Substrate with
  `--remove-control-plane` (it removes the plugins too), then run the
  Control plane, Demo and Plugins steps again with `--tag v1.0.1` (or set
  `SUBSTRATE_TAG`). `native/deploy/eks` must still apply to the new tag; the
  control plane step stops when its Go patch does not. Check the release
  notes for changes to the counter templates or the kubectl-ate flags, then
  run `make -C native verify`.
- Production settings (Multi-AZ, backups, deletion protection, the API
  CIDRs, bootstrap owners): see "How it is built" in `README.md`.

## Tear down

```bash
go -C native/installer run . --remove-control-plane --cluster $C --region $R --profile $P
CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P go -C native/tools/setup-aws run . teardown   # --keep-cluster keeps the cluster
CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P go -C native/tools/setup-aws run . inventory  # must list nothing
```

Or `make -C native teardown CLUSTER_NAME=$C AWS_REGION=$R AWS_PROFILE=$P`,
which opens the wizard on the teardown.

Get the user's explicit go-ahead first: teardown deletes data (RDS without a
final snapshot, both S3 buckets with every object version, the ECR images).
`--remove-control-plane` removes every recorded plugin first, Kubernetes
objects then AWS resources, then Substrate, so its volumes and claims are
released. `setup-aws teardown` deletes, in reverse order, only what carries
this deployment's owner tag, and ends with the inventory. A rerun resumes.

## Never

- Expose anything publicly (LoadBalancer, Ingress, NodePort, public RDS).
- Create static AWS access keys or put credentials in an actor.
- Write the account id, a password or a token into a tracked file or a log.
- Change `~/.kube/config` or the user's current kube context.
- Delete AWS resources by hand instead of through the teardown.
