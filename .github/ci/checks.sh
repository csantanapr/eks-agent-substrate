#!/usr/bin/env bash
# The checks in AGENTS.md, for CI and for a local run before a pull request:
# bash syntax and shellcheck on every tracked script, no AWS account id in
# tracked text files, and gofmt, go vet and go test for the installer.
#
#   .github/ci/checks.sh          from anywhere in the repository
#
# Needs bash, git, shellcheck and go. Runs no deploy script and calls no AWS
# or Kubernetes API. Exits non-zero if any check fails, after running all.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
fail=0
step() { printf '\n== %s\n' "$*"; }

mapfile -t scripts < <(git ls-files '*.sh')

step "bash -n (${#scripts[@]} scripts)"
for f in "${scripts[@]}"; do bash -n "$f" || fail=1; done

step "shellcheck $(shellcheck --version | sed -n 's/^version: //p')"
shellcheck -S warning -x "${scripts[@]}" || fail=1

# -w skips digit runs inside hex digests and other words.
step "no AWS account ids (111122223333 is the placeholder)"
if git grep -nwE '[0-9]{12}' -- '*.sh' '*.yaml' '*.yml' '*.md' '*.html' '*.json' | grep -v 111122223333; then
  fail=1
fi

step "installer: gofmt, go vet, go test"
(
  cd installer
  unformatted=$(gofmt -l .)
  if [ -n "$unformatted" ]; then printf 'not gofmt-formatted: %s\n' "$unformatted"; exit 1; fi
  go vet ./...
  go test -count=1 ./...
) || fail=1

[ "$fail" = 0 ] && step "all checks passed"
exit "$fail"
