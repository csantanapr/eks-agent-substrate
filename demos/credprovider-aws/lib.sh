# shellcheck shell=bash disable=SC2034
# Names and helpers shared by demos/credprovider-aws/{deploy,verify,clean}.sh.
# Sourced after lib/common.sh, never run.

# The provider: credprovider-aws-secret-manager, in ate-system.
CP_NS=ate-system
CP_SA=aws-sm-credential-provider
CP_ROLE="${IAM_PREFIX}-${CP_SA}"
CP_POLICY_CM=aws-sm-credential-provider-secret-policy
CP_PROVIDER_NAME=secretsmanager.amazonaws.com
CP_PROVIDER_ADDRESS="$CP_SA.$CP_NS.svc:50051"
# substrate/install.sh adds each JSON object in .work/credential-providers/ to
# the egress gateway's --credential-provider list, on every install.
CP_REGISTRATION="$WORK/credential-providers/$CP_PROVIDER_NAME.json"
# ECR repositories: the provider image, and the probe actor's image.
CP_REPO="$ECR_PREFIX/aws-sm-credential-provider"
CP_PROBE_REPO="$ECR_PREFIX/credprovider-probe"

# Actors in CP_ATESPACE may resolve its secrets; CP_DENIED_ATESPACE is in no
# grant of the provider's policy. Both use one gVisor WorkerPool, in the
# Kubernetes namespace CP_POOL_NS.
CP_ATESPACE=ate-demo-credprovider
CP_DENIED_ATESPACE=ate-demo-credprovider-denied
CP_POOL_NS=ate-demo-credprovider
CP_TEMPLATE=probe
# The demo secret, tagged ate.dev/injectable=true, and one without the tag,
# which the provider's IAM policy does not let it read.
CP_SECRET="ate/$CP_ATESPACE/demo-token"
CP_UNTAGGED_SECRET="ate/$CP_ATESPACE/untagged-token"
CP_INJECTABLE_TAG=ate.dev/injectable

# A public HTTPS endpoint that echoes the request headers it received.
# CREDPROVIDER_ECHO_URL=https://httpbin.org/headers works too.
CP_ECHO_URL="${CREDPROVIDER_ECHO_URL:-https://postman-echo.com/headers}"

# cp_uri SECRET: the credential URI an EgressPolicy names it by.
cp_uri() {
  printf 'ate-secret://%s/accounts/%s/regions/%s/secrets/%s/stages/AWSCURRENT' \
    "$CP_PROVIDER_NAME" "$ACCOUNT" "$AWS_REGION" "$1"
}

# owned_secret NAME: 0 if it carries our owner tag, 1 if it does not exist;
# stops the script when it exists without the tag (another deployment's).
owned_secret() {
  aws_exists secretsmanager describe-secret --secret-id "$1" || return 1
  local v
  v=$(aws_ secretsmanager describe-secret --secret-id "$1" \
        --query "Tags[?Key=='$OWNER_TAG_KEY'].Value | [0]" --output text) \
    || die "cannot read the tags of secret $1"
  [ "$v" = "$OWNER_TAG_VALUE" ] || die "secret $1 exists but is not owned by $OWNER_TAG_VALUE ($OWNER_TAG_KEY=$v)"
}

# cp_preview_arg: the --preview the installed Substrate runs with (for
# example ExternalVolumes from demos/efs), so a reinstall keeps it.
cp_preview_arg() {
  local p
  p=$(kubectl -n ate-system get deploy ate-api-server -o json 2>/dev/null \
    | jq -r '[.spec.template.spec.containers[].args[]? | select(startswith("--preview="))][0] // empty')
  [ "$p" = "--preview=" ] || printf '%s' "$p"
}

# cp_gateway_serves: 0 if the egress gateway is configured with the provider.
cp_gateway_serves() {
  kubectl -n ate-system get deploy atenet-egress -o json 2>/dev/null \
    | jq -e --arg a "--credential-provider-name=$CP_PROVIDER_NAME" \
        '[.spec.template.spec.containers[].args[]?] | index($a) != null' >/dev/null
}

# cp_reinstall: substrate/install.sh with the installed release's previews,
# which rolls the egress gateway onto the current provider list.
cp_reinstall() {
  local preview
  preview=$(cp_preview_arg)
  [ -z "$preview" ] || info "keeping $preview"
  # shellcheck disable=SC2086
  bash "$ROOT/substrate/install.sh" $preview
}

sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
