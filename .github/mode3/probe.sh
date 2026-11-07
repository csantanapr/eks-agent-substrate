#!/usr/bin/env bash
# One-off: which GHCR names can this repository's workflow token create?
set -uo pipefail
ctx=$(mktemp -d)
echo probe > "$ctx/README"
printf 'FROM scratch\nCOPY README /README\n' > "$ctx/Dockerfile"
src="https://github.com/$GITHUB_REPOSITORY"
for ref in "ghcr.io/$GITHUB_REPOSITORY:probe" "ghcr.io/${GITHUB_REPOSITORY_OWNER}/mode3-probe:probe" "ghcr.io/$GITHUB_REPOSITORY/atelet:probe"; do
  docker build -q --label "org.opencontainers.image.source=$src" -t "$ref" "$ctx" >/dev/null
  if docker push "$ref" > "$ctx/out" 2>&1; then echo "OK     $ref"; else echo "DENIED $ref: $(tail -1 "$ctx/out")"; fi
done
