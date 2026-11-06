# The toolbox: every command-line tool the scripts in this repository use,
# in one image, so a host needs only Docker, git and AWS credentials.
# ../toolbox.sh builds it on first use and runs a script inside it.
#
# Versions are pinned here and every download is checked against a sha256.
# To change a version, edit its ARG, run toolbox/pins.sh, and replace the
# sha256 ARGs below with what it prints.
#
# Learn more:
#   https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html
#   https://eksctl.io/installation/
#   https://kubernetes.io/docs/tasks/tools/install-kubectl-linux/
#   https://helm.sh/docs/intro/install/
#   https://docs.docker.com/engine/install/binaries/

# Go 1.27, Debian bookworm: bash 5, GNU sed and coreutils, git.
FROM public.ecr.aws/docker/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61

ARG AWSCLI_VERSION=2.38.0
ARG KUBECTL_VERSION=1.37.2
ARG HELM_VERSION=4.2.4
ARG EKSCTL_VERSION=0.230.0
ARG DOCKER_VERSION=27.5.1
ARG BUILDX_VERSION=0.21.2
# Not Debian's jq 1.6: its fromjson? drops matches the scripts rely on.
ARG JQ_VERSION=1.8.1

# From toolbox/pins.sh.
ARG AWSCLI_SHA256_AMD64=cb037d35cca5cefc3eb89ebabc471a0b4e4fe44135b72228c7eb0da2d2fae9f6
ARG KUBECTL_SHA256_AMD64=c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4
ARG HELM_SHA256_AMD64=c306b46f719b0a4da32d0f78ee21bf90ce8d602f15b22ab753f0674d1670a7f3
ARG EKSCTL_SHA256_AMD64=a2060956f117c3065abafda5c1f681679b9c3716675d70ce4ffff46033b02c35
ARG DOCKER_SHA256_AMD64=4f798b3ee1e0140eab5bf30b0edc4e84f4cdb53255a429dc3bbae9524845d640
ARG BUILDX_SHA256_AMD64=b13bee81c3db12a4be7d0b9d042b64d0dd9ed116f7674dfac0ffdf2a71acfe3d
ARG AWSCLI_SHA256_ARM64=4987b0d4e12019b8decefcd826393dd3c71ea72c87b88b3aeca570b7039a54c0
ARG KUBECTL_SHA256_ARM64=fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509
ARG HELM_SHA256_ARM64=564de2191b881e9f71b5606b25345821ea1682f06ab90499d3ab22b530176da1
ARG EKSCTL_SHA256_ARM64=21afe8a1e38f0e8153a1f27ff7af6b90e309a0411a1438139463dac2f866674d
ARG DOCKER_SHA256_ARM64=e6b53725a73763ab3f988c73f8772eaed429754c1a579db5ff11f21990fd1817
ARG BUILDX_SHA256_ARM64=7e21e62eae3243e476411c9bbe93b8ee59b5d62ddf075c527d168174c3ab3a04
ARG JQ_SHA256_AMD64=020468de7539ce70ef1bceaf7cde2e8c4f2ca6c3afb84642aabc5c97d9fc2a0d
ARG JQ_SHA256_ARM64=6bc62f25981328edd3cfcfe6fe51b073f2d7e7710d7ef7fcdac28d4e384fc3d4

ARG TARGETARCH

# envsubst (gettext-base), PyYAML, and what the installers need.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl gettext-base openssl python3 python3-yaml unzip \
 && rm -rf /var/lib/apt/lists/*

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN case "$TARGETARCH" in \
      amd64) a=x86_64; S=AMD64 ;; \
      arm64) a=aarch64; S=ARM64 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    sha() { local n="${1}_SHA256_$S"; echo "${!n}"; }; \
    get() { curl -fsSL --retry 3 -o "$1" "$2" && echo "$3  $1" | sha256sum -c --quiet -; }; \
    cd /tmp; \
    get awscli.zip "https://awscli.amazonaws.com/awscli-exe-linux-$a-$AWSCLI_VERSION.zip" "$(sha AWSCLI)"; \
    unzip -q awscli.zip && ./aws/install && rm -rf aws awscli.zip; \
    get /usr/local/bin/kubectl "https://dl.k8s.io/release/v$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl" "$(sha KUBECTL)"; \
    chmod 0755 /usr/local/bin/kubectl; \
    get helm.tgz "https://get.helm.sh/helm-v$HELM_VERSION-linux-$TARGETARCH.tar.gz" "$(sha HELM)"; \
    tar -xzf helm.tgz --strip-components=1 -C /usr/local/bin "linux-$TARGETARCH/helm" && rm helm.tgz; \
    get eksctl.tgz "https://github.com/eksctl-io/eksctl/releases/download/v$EKSCTL_VERSION/eksctl_Linux_$TARGETARCH.tar.gz" "$(sha EKSCTL)"; \
    tar -xzf eksctl.tgz -C /usr/local/bin eksctl && rm eksctl.tgz; \
    get docker.tgz "https://download.docker.com/linux/static/stable/$a/docker-$DOCKER_VERSION.tgz" "$(sha DOCKER)"; \
    tar -xzf docker.tgz --strip-components=1 -C /usr/local/bin docker/docker && rm docker.tgz; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    get /usr/local/lib/docker/cli-plugins/docker-buildx \
      "https://github.com/docker/buildx/releases/download/v$BUILDX_VERSION/buildx-v$BUILDX_VERSION.linux-$TARGETARCH" "$(sha BUILDX)"; \
    chmod 0755 /usr/local/lib/docker/cli-plugins/docker-buildx; \
    get /usr/local/bin/jq "https://github.com/jqlang/jq/releases/download/jq-$JQ_VERSION/jq-linux-$TARGETARCH" "$(sha JQ)"; \
    chmod 0755 /usr/local/bin/jq; \
    jq --version; aws --version; kubectl version --client=true -o yaml | grep gitVersion; helm version --short; \
    eksctl version; docker --version; docker buildx version

# Go fetches modules from their source, not proxy.golang.org, and uses the
# go in this image. toolbox.sh points GOCACHE and GOMODCACHE into .work/.
ENV GOPROXY=direct GOSUMDB=off GOTOOLCHAIN=local
# The checkouts are mounted from the host and may belong to another user.
RUN git config --system --add safe.directory '*'
