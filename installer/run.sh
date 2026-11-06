#!/usr/bin/env bash
# Runs the installer inside the toolbox (../toolbox.sh), so the host needs
# only Docker, git and AWS credentials: the toolbox builds the installer with
# its own Go, and the scripts the installer runs use the toolbox's tools.
#
#   installer/run.sh --repo ../../eks-talk-tui --cluster substrate-tui --region us-west-2 --profile eks-sandbox
#
# Takes the installer's own flags. The --repo checkout is mounted into the
# toolbox, and --profile is the profile the toolbox passes credentials for.
# Commands the Done screen prints (kubectl, kubectl ate) then run the same
# way: ./toolbox.sh kubectl get nodes.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"

args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    --repo|-repo|--profile|-profile)
      [ $((i + 1)) -lt ${#args[@]} ] || { echo "run.sh: ${args[i]} needs a value" >&2; exit 2; }
      v="${args[i + 1]}"
      case "${args[i]}" in
        *repo) v="$(cd "$v" && pwd)"; args[i + 1]="$v"; export TOOLBOX_MOUNTS="${TOOLBOX_MOUNTS:-} $v" ;;
        *) export AWS_PROFILE="$v" ;;
      esac ;;
    --repo=*|-repo=*)
      v="$(cd "${args[i]#*=}" && pwd)"; args[i]="--repo=$v"; export TOOLBOX_MOUNTS="${TOOLBOX_MOUNTS:-} $v" ;;
    --profile=*|-profile=*)
      export AWS_PROFILE="${args[i]#*=}" ;;
  esac
done

# The binary goes in .work/toolbox/, next to the toolbox's caches. The
# script in single quotes runs in the toolbox's bash.
# shellcheck disable=SC2016
exec "$ROOT/toolbox.sh" bash -c 'cd "$1" && go build -o "$2" . && shift 2 && exec "$0" "$@"' \
  "$ROOT/.work/toolbox/installer" "$HERE" "$ROOT/.work/toolbox/installer" ${args[@]+"${args[@]}"}
