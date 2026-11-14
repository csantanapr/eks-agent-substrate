# Agent Substrate on Amazon EKS

Instructions for coding agents (Claude Code, Kiro, Codex and others) working
in this repository. Read `README.md` for what the repository deploys, and
`native/README.md` for how the installer is built.

The task-level procedure (install, verify, troubleshoot, tear down) is the
skill in `.agents/skills/agent-substrate-on-eks/SKILL.md`. Read it before you
run the installer.

Skills live in `.agents/skills/`, where Codex finds them. `.claude/skills` and
`.kiro/skills` are symlinks to that folder, so Claude Code and Kiro find the
same skills. Add a new skill under `.agents/skills/` only.

## Layout

- `native/` is the installer, in Go, with the same architecture as
  [substrate-gke](https://github.com/ai-on-gke/substrate-gke)'s:
  - `native/installer/` is the wizard (`substrate-eks`, Bubble Tea) and the
    subcommands it runs as steps (`--deploy-control-plane`,
    `--plugin-action`, `--deploy-demo`, `--verify`, `--remove-control-plane`).
  - `native/tools/setup-aws/` creates and deletes the AWS resources with the
    AWS SDK for Go v2 (`create cluster`, `bootstrap`, `create rds`,
    `plugin <id>`, `teardown`, `inventory`).
  - `native/installer/internal/plugin/<id>/` holds each plugin's Kubernetes
    half, `native/tools/setup-aws/cmd/plugin_<id>.go` its AWS half.
  - `native/deploy/eks/` is the kustomize Component and Go patch that make
    upstream's install run on EKS with S3.
  - `native/install.sh` is the launcher: it builds the installer and starts
    it. It runs no install step.
  - `native/upstream.lock` records the substrate-gke commit the shared code
    follows.
- Settings come from `config.env`, then `config.local.env` (gitignored), under
  the environment. Never hardcode a cluster name, region, profile, account id
  or tag.
- `patches/substrate-v1.0.0-eks.patch` is the reference `make -C native
  parity` compares `native/deploy/eks` with.
- The installer keeps its state under the user cache dir
  (`~/.cache/substrate-eks/` on Linux): a kubeconfig per cluster, the fetched
  Substrate trees, `kubectl-ate`, logs and snapshots. Never commit any of it.

## Rules

- **No shell in an install step.** Every step is Go code that starts each
  child process from an argv: no shell script, no `bash -c`. Shell scripts are
  only `native/install.sh` (the launcher), the CI scripts under `.github/`,
  and `native/installer/docs/splash-gif.sh` (rebuilds a doc image).
- **No public application endpoints.** Services are ClusterIP. Never add a
  LoadBalancer, an Ingress, a NodePort, a public RDS instance or a public
  bucket. Operators and demos use `kubectl port-forward` or `kubectl ate
  proxy`. The EKS API endpoint is public and IAM authenticated, limited by
  `EKS_PUBLIC_ACCESS_CIDRS`.
- **No static AWS keys.** Workloads get AWS access only through EKS Pod
  Identity, with least-privilege policies. Actors get none. Keep IMDSv2 with
  hop limit 1 on every node group. That blocks pods in their own network
  namespace; never run workloads with `hostNetwork`.
- **Never print or commit secrets or account ids.** Passwords and tokens go
  from Secrets Manager into a child's environment or stdin. Never put them in
  a command-line argument (other users can read it in `ps`), a log or a
  tracked file. Example values use placeholders like `111122223333`.
- **Use only the installer's kubeconfig.** It writes one per cluster under
  the user cache dir; never change `~/.kube/config` or switch the user's
  current context.
- **Every step is idempotent.** Each one checks before it creates and
  reconciles what it finds. A denied or throttled read stops the step instead
  of passing as "not found". Keep it so.
- **Tag every AWS resource** at create time with `TAGS` and the owner tag
  `agent-substrate-on-eks/deployment=<cluster>.<region>`. Reuse or delete a
  resource with a global name only when it carries this deployment's owner
  tag.
- **Destructive changes go through the installer's teardown**
  (`--remove-control-plane`, then `setup-aws teardown`). Do not delete AWS
  resources by hand. A new resource needs its teardown step and its
  `setup-aws inventory` row in the same change.
- **Pin images by digest** and pin tool versions.
- **Agent Substrate changes for EKS go into `native/deploy/eks`**, with
  `patches/substrate-v1.0.0-eks.patch` kept in step (`make -C native
  parity`). Do not fork the release images.

## Checks before you finish

```bash
make -C native test          # gofmt, go vet and the unit tests of both Go modules
make -C native parity        # deploy/eks renders what the reference patch does
bash .github/ci/checks.sh    # bash -n and shellcheck on every tracked script, no account ids
```

The `native` and `checks` workflows run the same on every pull request.
Every `go` command needs `GOTOOLCHAIN=local GOPROXY=direct GOSUMDB=off`; the
Makefile and `native/install.sh` set them.

On a live cluster, a change is done when `make -C native verify` passes, and
`--plugin-action verify --plugins <id>` for each plugin it touches.

## Writing

Short, plain sentences. Use the real names from the code. Write `microVM` in
prose and `microvm` for API values and flags.
