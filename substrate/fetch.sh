#!/usr/bin/env bash
# Clones Agent Substrate at SUBSTRATE_TAG into .work/, applies the EKS patch
# from patches/, and builds ate-setup and kubectl-ate into .work/bin.
#
# The patch lets ate-setup install on EKS with S3 (see patches/README.md). A
# tag with no patch of its own gets the newest patch that applies cleanly.
# Idempotent: an existing clone with the patch applied is reused; each tag
# has its own clone, so switching releases leaves the old one in place.
#
# deploy.sh calls it, and so does clean.sh, to get the installer of the
# release the cluster runs. ate-setup installs and removes Substrate.
# kubectl-ate is the kubectl plugin for actors (kubectl ate get, proxy,
# suspend).
#
# Learn more:
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/get-started/quickstart.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/reference/kubectl-ate.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/upgrade.md
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"
need git; need go
info "Agent Substrate $SUBSTRATE_TAG"

clone() {
  rm -rf "$SUBSTRATE_SRC"
  log "Clone $SUBSTRATE_REPO at $SUBSTRATE_TAG"
  git clone --quiet --depth 1 --branch "$SUBSTRATE_TAG" "$SUBSTRATE_REPO" "$SUBSTRATE_SRC" \
    || die "no tag $SUBSTRATE_TAG in $SUBSTRATE_REPO"
}
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
[ -d "$SUBSTRATE_SRC/.git" ] || clone

# The patch for this tag, else the newest patch that applies cleanly: a new
# release candidate usually takes the previous one's patch unchanged. A clean
# apply only shows the text still fits, so a borrowed patch gets a warning and
# the install deserves a check; add patches/substrate-<tag>-eks.patch once it
# is reviewed.
pick_patch() {
  local exact="$ROOT/patches/substrate-${SUBSTRATE_TAG}-eks.patch" p
  if [ -f "$exact" ]; then printf '%s' "$exact"; return; fi
  while read -r p; do
    if git -C "$SUBSTRATE_SRC" apply --check "$p" 2>/dev/null; then printf '%s' "$p"; return; fi
  done < <(find "$ROOT/patches" -name 'substrate-*-eks.patch' | sed 's/-rc\./~rc./' | sort -rV | sed 's/~rc\./-rc./')
}
# The marker records what the clone was built from; any change (a new or
# fixed patch, another region) rebuilds the clone from scratch.
marker="$SUBSTRATE_SRC/.eks-patch-applied"
want_marker() { # $1 patch
  printf 'tag=%s commit=%s patch=%s sha256=%s region=%s\n' "$SUBSTRATE_TAG" \
    "$(git -C "$SUBSTRATE_SRC" rev-parse HEAD)" "$(basename "$1")" "$(sha256 "$1")" "$AWS_REGION"
}
if [ -f "$marker" ]; then
  exact="$ROOT/patches/substrate-${SUBSTRATE_TAG}-eks.patch"
  applied=$(sed -n 's/.* patch=\([^ ]*\) .*/\1/p' "$marker")
  p="$exact"; [ -f "$exact" ] || p="$ROOT/patches/$applied"
  if [ -f "$p" ] && [ "$(cat "$marker")" = "$(want_marker "$p")" ]; then
    info "patch already applied: $(basename "$p")"
  else
    info "the clone does not match patches/ or AWS_REGION; starting over"
    clone
  fi
fi
if [ ! -f "$marker" ]; then
  PATCH="$(pick_patch)"
  [ -n "$PATCH" ] || die "no patch in patches/ applies to $SUBSTRATE_TAG: rebase the newest one (see patches/README.md)"
  [ "$(basename "$PATCH")" = "substrate-${SUBSTRATE_TAG}-eks.patch" ] \
    || warn "no patch for $SUBSTRATE_TAG; $(basename "$PATCH") applies cleanly, using it. Check the install, then save it as patches/substrate-${SUBSTRATE_TAG}-eks.patch"
  log "Apply $(basename "$PATCH")"
  git -C "$SUBSTRATE_SRC" apply "$PATCH"
  # The patch leaves a REPLACE_WITH_AWS_REGION placeholder in the install
  # kustomization; write AWS_REGION in its place.
  kust="$SUBSTRATE_SRC/manifests/ate-install/base/kustomization.yaml"
  python3 - "$kust" "$AWS_REGION" <<'EOF'
import sys
p, region = sys.argv[1], sys.argv[2]
s = open(p).read()
assert "value: REPLACE_WITH_AWS_REGION" in s, "no REPLACE_WITH_AWS_REGION in " + p
open(p, "w").write(s.replace("value: REPLACE_WITH_AWS_REGION", "value: " + region))
EOF
  want_marker "$PATCH" > "$marker"
fi

log "Build ate-setup and kubectl-ate ($(go version | awk '{print $3}'))"
mkdir -p "$WORK/bin"
# -mod=vendor builds from the modules vendored in the clone, so the build
# downloads no Go modules.
(
  cd "$SUBSTRATE_SRC"
  export GOFLAGS=-mod=vendor CGO_ENABLED=0
  go build -o "$WORK/bin/ate-setup" ./cmd/ate-setup
  go build -o "$WORK/bin/kubectl-ate" ./cmd/kubectl-ate
)
"$WORK/bin/kubectl-ate" --version | sed 's/^/   /'
