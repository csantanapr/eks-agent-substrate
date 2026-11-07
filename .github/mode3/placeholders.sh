#!/usr/bin/env bash
# Push a tiny placeholder image for every image a Substrate release
# publishes, under IMAGE_REPO, so the GHCR packages exist (linked to this
# repository) and can be made public before the first real build.
#
#   IMAGE_REPO=ghcr.io/OWNER/REPO .github/mode3/placeholders.sh
#
# Run by .github/workflows/mode3-placeholders.yaml after docker login.
set -euo pipefail
: "${IMAGE_REPO:?set IMAGE_REPO, e.g. ghcr.io/OWNER/REPO}"
SOURCE="${SOURCE:-https://github.com/${GITHUB_REPOSITORY:-csantanapr/eks-agent-substrate}}"

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

for n in "${names[@]}"; do
  ref="$IMAGE_REPO/$n:placeholder"
  docker build -q --label "org.opencontainers.image.source=$SOURCE" -t "$ref" "$ctx" >/dev/null
  docker push -q "$ref"
done
