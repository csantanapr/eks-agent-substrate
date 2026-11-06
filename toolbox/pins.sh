#!/usr/bin/env bash
# Prints the sha256 of every download in toolbox/Dockerfile, for amd64 and
# arm64, as ARG lines to paste into it. Run it after changing a version.
#
#   toolbox/pins.sh
#
# Uses the checksum files the projects publish where there is one (kubectl,
# helm, eksctl, buildx, jq), and hashes the download itself for the AWS CLI and
# the Docker CLI, which publish none.
set -euo pipefail
cd "$(dirname "$0")"
v() { sed -n "s/^ARG $1=//p" Dockerfile; }
AWSCLI=$(v AWSCLI_VERSION) KUBECTL=$(v KUBECTL_VERSION) HELM=$(v HELM_VERSION)
EKSCTL=$(v EKSCTL_VERSION) DOCKER=$(v DOCKER_VERSION) BUILDX=$(v BUILDX_VERSION)
JQ=$(v JQ_VERSION)
fetch() { curl -fsSL --retry 3 "$1"; }
hash_of() { fetch "$1" | sha256sum | cut -d' ' -f1; }

for arch in amd64 arm64; do
  case $arch in amd64) a=x86_64 ;; arm64) a=aarch64 ;; esac
  U=$(tr '[:lower:]' '[:upper:]' <<<"$arch")
  echo "ARG AWSCLI_SHA256_$U=$(hash_of "https://awscli.amazonaws.com/awscli-exe-linux-$a-$AWSCLI.zip")"
  echo "ARG KUBECTL_SHA256_$U=$(fetch "https://dl.k8s.io/release/v$KUBECTL/bin/linux/$arch/kubectl.sha256")"
  echo "ARG HELM_SHA256_$U=$(fetch "https://get.helm.sh/helm-v$HELM-linux-$arch.tar.gz.sha256sum" | cut -d' ' -f1)"
  echo "ARG EKSCTL_SHA256_$U=$(fetch "https://github.com/eksctl-io/eksctl/releases/download/v$EKSCTL/eksctl_checksums.txt" \
    | awk -v f="eksctl_Linux_$arch.tar.gz" '$2 == f {print $1}')"
  echo "ARG DOCKER_SHA256_$U=$(hash_of "https://download.docker.com/linux/static/stable/$a/docker-$DOCKER.tgz")"
  echo "ARG BUILDX_SHA256_$U=$(fetch "https://github.com/docker/buildx/releases/download/v$BUILDX/checksums.txt" \
    | awk -v f="*buildx-v$BUILDX.linux-$arch" '$2 == f {print $1}')"
  echo "ARG JQ_SHA256_$U=$(fetch "https://github.com/jqlang/jq/releases/download/jq-$JQ/sha256sum.txt" \
    | awk -v f="jq-linux-$arch" '$2 == f {print $1}')"
done
