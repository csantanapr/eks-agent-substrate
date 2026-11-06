#!/usr/bin/env bash
# Runs a script of this repository inside the toolbox image (toolbox/), so
# the host needs only Docker, git and AWS credentials: no aws, eksctl,
# kubectl, helm, go, jq, envsubst or PyYAML.
#
#   ./toolbox.sh ./create-cluster.sh
#   ./toolbox.sh ./deploy.sh
#   ./toolbox.sh demos/counter/verify.sh
#   ./toolbox.sh                  # a bash shell with every tool
#
# The first run builds the image (a few minutes); a changed toolbox/Dockerfile
# builds a new one. The checkout is mounted at the same path, and scripts run
# as you, so .work/ and the logs look as they do on the host. Image builds
# use the host's Docker through its socket, which gives the container the
# same control of Docker that you have.
#
# AWS credentials, for AWS_PROFILE (the environment, else config.local.env):
# - a profile with credential_process (a helper that exists only on the
#   host): the host's aws CLI exports its credentials to a 0600 file in
#   .work/toolbox/creds/ every 2 minutes while the script runs, and the
#   container's profile reads that file. The file is deleted on exit.
# - any other profile (SSO, static keys, assume-role): ~/.aws is mounted.
# - AWS_ACCESS_KEY_ID and the other standard variables are passed through
#   when set.
#
# Also mounted, when they exist: CREDPROVIDER_DIR (default
# ~/credprovider-aws-secret-manager), OPENCLAW_DIR from the config, and the
# space-separated paths in TOOLBOX_MOUNTS. TOOLBOX_ENV names more variables
# to pass through.
#
# Learn more:
#   https://docs.docker.com/engine/security/#docker-daemon-attack-surface
#   https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sourcing-external.html
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
TB="$ROOT/.work/toolbox"
die() { echo "toolbox.sh: $*" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || die "docker is required"

sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -c1-12; }
IMAGE="eks-talk-toolbox:$(sha256 < "$ROOT/toolbox/Dockerfile")"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "toolbox.sh: building $IMAGE (once per toolbox/Dockerfile change)" >&2
  docker build -q -t "$IMAGE" "$ROOT/toolbox" >/dev/null
fi

# The values the scripts would use: config.env, then config.local.env, then
# the environment.
cfg() {
  # shellcheck disable=SC1091
  ( set +u; . "$ROOT/config.env"; [ ! -f "$ROOT/config.local.env" ] || . "$ROOT/config.local.env"; printf '%s' "${!1:-}" )
}
PROFILE="${AWS_PROFILE:-$(cfg AWS_PROFILE)}"
REGION="${AWS_REGION:-$(cfg AWS_REGION)}"
CREDPROVIDER_DIR="${CREDPROVIDER_DIR:-$HOME/credprovider-aws-secret-manager}"
OPENCLAW_DIR="${OPENCLAW_DIR:-$(cfg OPENCLAW_DIR)}"

mkdir -p "$TB/home" "$TB/go-build" "$TB/gomod"
args=(--rm -i --init
  -v "$ROOT:$ROOT" -w "$ROOT"
  -e HOME="$TB/home" -e GOCACHE="$TB/go-build" -e GOMODCACHE="$TB/gomod"
  -e TERM="${TERM:-xterm}" -e CREDPROVIDER_DIR="$CREDPROVIDER_DIR")
[ -t 0 ] && [ -t 1 ] && args+=(-t)
case "$PWD/" in "$ROOT"/*) args+=(-w "$PWD") ;; esac
[ -z "$PROFILE" ] || args+=(-e AWS_PROFILE="$PROFILE")

for d in "$CREDPROVIDER_DIR" "$OPENCLAW_DIR" ${TOOLBOX_MOUNTS:-}; do
  [ -n "$d" ] && [ -d "$d" ] && args+=(-v "$d:$d")
done
for n in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN TZ ${TOOLBOX_ENV:-}; do
  [ -z "${!n:-}" ] || args+=(-e "$n")
done

# The host's Docker: its socket, as yourself on Linux with the socket's
# group, as root on macOS, where Docker Desktop owns the socket. The group is
# read inside a container: on the host it can show as nobody (a user
# namespace), but the daemon checks the real one.
sock=/var/run/docker.sock
case "${DOCKER_HOST:-}" in unix://*) sock="${DOCKER_HOST#unix://}" ;; ?*) die "DOCKER_HOST=$DOCKER_HOST: only a unix socket is supported" ;; esac
[ -S "$sock" ] || die "no Docker socket at $sock"
args+=(-v "$sock:/var/run/docker.sock")
if [ "$(uname -s)" = Darwin ]; then
  args+=(--user 0:0)
else
  gid=$(docker run --rm -v "$sock:/s" "$IMAGE" stat -c %g /s) || die "cannot read the group of $sock"
  args+=(--user "$(id -u):$(id -g)" --group-add "$gid")
fi

# Credentials. Each run gets its own folder under .work/toolbox/creds/, so
# runs side by side (two checkouts, or the installer and a script) do not
# delete each other's credentials when one ends.
refresher=
run_creds=
cleanup() {
  [ -z "$refresher" ] || kill "$refresher" 2>/dev/null || true
  [ -z "$run_creds" ] || rm -rf "$run_creds"
}
trap cleanup EXIT
helper=
if [ -n "$PROFILE" ] && command -v aws >/dev/null 2>&1; then
  helper=$(aws configure get credential_process --profile "$PROFILE" 2>/dev/null || true)
fi
if [ -n "$helper" ]; then
  mkdir -p "$TB/creds"; chmod 0700 "$TB/creds"
  run_creds=$(mktemp -d "$TB/creds/run.XXXXXX")
  export_creds() {
    local f="$run_creds/$PROFILE.json"
    (umask 077; aws configure export-credentials --profile "$PROFILE" --format process > "$f.new") && mv -f "$f.new" "$f"
  }
  export_creds || die "cannot get credentials for profile $PROFILE on the host"
  ( while sleep 120; do export_creds 2>/dev/null || true; done ) &
  refresher=$!
  (umask 077; printf '[profile %s]\nregion = %s\ncredential_process = cat %s\n' \
    "$PROFILE" "$REGION" "$run_creds/$PROFILE.json" > "$run_creds/config")
  args+=(-e AWS_CONFIG_FILE="$run_creds/config" -e AWS_SHARED_CREDENTIALS_FILE="$run_creds/none")
elif [ -d "$HOME/.aws" ]; then
  args+=(-v "$HOME/.aws:$TB/home/.aws")
fi

[ "$#" -gt 0 ] || set -- bash
docker run "${args[@]}" "$IMAGE" "$@"
