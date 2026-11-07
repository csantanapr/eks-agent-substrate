#!/usr/bin/env bash
# Log docker in to GHCR as the repository owner with GHCR_TOKEN, a personal
# access token with write:packages (the workflow's GITHUB_TOKEN cannot create
# packages other than the one named after the repository).
set -euo pipefail
if [ -z "${GHCR_TOKEN:-}" ]; then
  echo "::error::secret GHCR_TOKEN is not set: create a token with write:packages and run gh secret set GHCR_TOKEN -R $GITHUB_REPOSITORY" >&2
  exit 1
fi
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_REPOSITORY_OWNER" --password-stdin
