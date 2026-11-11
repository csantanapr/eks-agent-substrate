#!/usr/bin/env bash
# Installs shellcheck VERSION, checked against its sha256, for the checks
# workflow. The runner image ships an older one, which reports other warnings.
set -euo pipefail
VERSION=0.11.0
SHA256=8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
url="https://github.com/koalaman/shellcheck/releases/download/v$VERSION/shellcheck-v$VERSION.linux.x86_64.tar.xz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/shellcheck.tar.xz" "$url"
echo "$SHA256  $tmp/shellcheck.tar.xz" | sha256sum -c -
tar -xJf "$tmp/shellcheck.tar.xz" -C "$tmp"
mkdir -p "$HOME/.local/bin"
install -m 0755 "$tmp/shellcheck-v$VERSION/shellcheck" "$HOME/.local/bin/shellcheck"
echo "$HOME/.local/bin" >> "$GITHUB_PATH"
"$HOME/.local/bin/shellcheck" --version
