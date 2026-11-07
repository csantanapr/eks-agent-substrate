#!/usr/bin/env bash
# Push a tiny placeholder image for every image a Substrate release
# publishes, under IMAGE_REPO, so the GHCR packages exist and can be made
# public before the first real build.
#
#   IMAGE_REPO=ghcr.io/OWNER/substrate .github/mode3/placeholders.sh
#
# Run by .github/workflows/mode3-placeholders.yaml after docker login.
set -euo pipefail
: "${IMAGE_REPO:?set IMAGE_REPO, e.g. ghcr.io/OWNER/substrate}"

# The release set: images.ReleaseComponents and images.ReleaseDockerfileImages
# in cmd/ate-setup/internal/images/images.go, named by their last path element.
names=(ate-idp-server ateapi atecontroller atelet atenet ateworker-gvisor ateworker-microvm
  kubernetes-secrets objectstore-plugin podcertcontroller envoy-dataplane)

ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
echo "placeholder: the real image is built by .github/workflows/mode3-images.yaml" > "$ctx/README"
cat > "$ctx/Dockerfile" <<'EOF'
FROM scratch
COPY README /README
EOF

denied=()
for n in "${names[@]}"; do
  ref="$IMAGE_REPO/$n:placeholder"
  # No org.opencontainers.image.source label: linking a package to a
  # repository needs more than write:packages, and GHCR denies the push.
  docker build -q -t "$ref" "$ctx" >/dev/null
  if docker push -q "$ref"; then echo "pushed $ref"; else denied+=("$n"); fi
done
# A package that already exists and is linked to a repository refuses a
# token without access to that repository.
if [ "${#denied[@]}" -gt 0 ]; then
  echo "::error::denied: ${denied[*]} (an existing package linked to another repository?)" >&2
  exit 1
fi
