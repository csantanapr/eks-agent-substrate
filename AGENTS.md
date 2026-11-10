# Agent Substrate on Amazon EKS

Instructions for coding agents (Claude Code, Kiro, Codex and others) working
in this repository. Read `README.md` for what the repository deploys.

The task-level procedure (deploy, verify, troubleshoot, tear down) is the
skill in `.agents/skills/agent-substrate-on-eks/SKILL.md`. Read it before you
run any script.

Skills live in `.agents/skills/`, where Codex finds them. `.claude/skills` and
`.kiro/skills` are symlinks to that folder, so Claude Code and Kiro find the
same skills. Add a new skill under `.agents/skills/` only.

## Layout

- Four entry points: `create-cluster.sh`, `deploy.sh`, `clean.sh`,
  `delete-cluster.sh`. Everything else is called by them.
- All settings come from `config.env`, then `config.local.env` (gitignored).
  Never hardcode a cluster name, region, profile, account id or tag in a script.
- `lib/common.sh` holds the shared helpers: `aws_`, `aws_exists` (not found
  vs. a real error), the derived names (`SNAP_BUCKET`, `IAM_PREFIX`, `DB_ID`,
  `ECR_PREFIX`, ...), `PARTITION`, the `tags_*` formatters, `owned_role` and
  `owned_bucket`, `role_for_sa`, `confirm`. Reuse them.
- `aws/inventory.sh` lists every resource the scripts create. `clean.sh` ends
  with it and `delete-cluster.sh` starts with it.
- `.work/` is generated state: the kubeconfig, the patched Substrate clone,
  `ate-setup` and `kubectl-ate`. Never commit it.
- One checkout drives one cluster (its `config.local.env` and `.work/`). For
  a second cluster use a second checkout, a `git worktree` (README, "One
  checkout per cluster"); never point two clusters at one checkout.
- `toolbox.sh` runs any script inside `toolbox/Dockerfile`, which pins every
  tool the scripts use; a host then needs only Docker, git and AWS
  credentials. When a script needs a new tool, add it there, pinned by
  sha256 (`toolbox/pins.sh`), and check the script both ways.
- `installer/` is an optional terminal UI over the same scripts (Go, its own
  `go.mod`); `installer/run.sh` runs it in the toolbox. It adds no install
  logic of its own.

## Rules

- **No public application endpoints.** Services are ClusterIP. Never add a
  LoadBalancer, an Ingress, a NodePort, a public RDS instance or a public
  bucket. Operators and demos use `kubectl port-forward` or `kubectl ate
  proxy`. The EKS API endpoint is public and IAM authenticated, limited by
  `EKS_PUBLIC_ACCESS_CIDRS`.
- **No static AWS keys.** Workloads get AWS access only through EKS Pod
  Identity (`role_for_sa`), with least-privilege policies. Actors get none.
  Keep IMDSv2 with hop limit 1 on every node group. That blocks pods in their
  own network namespace; never run workloads with `hostNetwork`.
- **Never print or commit secrets or account ids.** Passwords and tokens go
  from Secrets Manager into environment variables, stdin or 0600 files under
  `.work/` that are deleted after use. Never put them in a command-line
  argument (other users can read it in `ps`), a log or a tracked file.
  Example values use placeholders like `111122223333`.
- **Use only the kubeconfig in `.work/`.** Scripts export `KUBECONFIG`; never
  change `~/.kube/config` or switch the user's current context.
- **Scripts are idempotent.** Each step checks before it creates, and
  reconciles what it finds (rules, associations, template versions). Use
  `aws_exists` for the check, so a denied or throttled read stops the script
  instead of passing as "not found". Keep it so.
- **Tag every AWS resource** with `tags_*`, at create time. They include the
  owner tag; check it with `owned_*` before you reuse or delete a resource
  with a global name.
- **Destructive changes go through `clean.sh` and `delete-cluster.sh`.** Do not
  delete AWS resources by hand; when `deploy.sh` gains a resource, extend
  `clean.sh` (in reverse order of creation) and `aws/inventory.sh`.
- **Pin images by digest** and pin tool versions.
- **Agent Substrate changes go into `patches/`** as a patch against
  `SUBSTRATE_TAG`, documented in `patches/README.md`. Do not fork the release
  images.

## Checks before you finish

`.github/ci/checks.sh` runs these checks on every tracked script. The
`checks` workflow runs it on every pull request and every push to main.

```bash
for f in *.sh lib/*.sh aws/*.sh substrate/*.sh llm-gateway/*.sh demos/*/*.sh toolbox/*.sh installer/*.sh; do bash -n "$f"; done
shellcheck -S warning -x *.sh lib/*.sh aws/*.sh substrate/*.sh llm-gateway/*.sh demos/*/*.sh toolbox/*.sh installer/*.sh
grep -rnE '[0-9]{12}' --include='*.sh' --include='*.yaml' --include='*.md' . | grep -v 111122223333
(cd installer && go test ./...)   # when installer/ changed
```

On a live cluster, a change is done when `demos/counter/verify.sh` passes (and
`demos/openclaw/deploy.sh verify` when that demo is configured).

## Writing

Short, plain sentences. Use the real names from the code. Write `microVM` in
prose and `microvm` for API values and flags.
