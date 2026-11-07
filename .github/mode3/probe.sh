#!/usr/bin/env bash
# One-off diagnostic: the GHCR_TOKEN's kind and scopes (never the token), and
# which package names it can create.
set -uo pipefail
case "$GHCR_TOKEN" in
  ghp_*) echo "token kind: classic" ;;
  github_pat_*) echo "token kind: fine-grained (GHCR needs a classic token)" ;;
  *) echo "token kind: other" ;;
esac
curl -sI -H "Authorization: token $GHCR_TOKEN" https://api.github.com/user | grep -iE '^x-oauth-scopes|^HTTP' || true
ctx=$(mktemp -d)
echo probe > "$ctx/README"
printf 'FROM scratch\nCOPY README /README\n' > "$ctx/Dockerfile"
for ref in "ghcr.io/${GITHUB_REPOSITORY_OWNER}/mode3-probe:probe" "ghcr.io/${GITHUB_REPOSITORY_OWNER}/substrate/ate-idp-server:probe"; do
  docker build -q -t "$ref" "$ctx" >/dev/null
  if docker push "$ref" > "$ctx/out" 2>&1; then echo "OK     $ref"; else echo "DENIED $ref: $(tail -1 "$ctx/out")"; fi
done
