#!/usr/bin/env bash
# Optional: Secrets Manager rotation of Agent Substrate's two database logins,
# with ate-api-server following each rotation by a rolling restart.
#
#   demos/db-rotation/deploy.sh          after ./deploy.sh
#
#   Secrets Manager   rotates <cluster>/ate-owner and <cluster>/ate-readwrite
#                     with the AWS-hosted "alternating users" PostgreSQL
#                     function (rotation.cfn.yaml), in the private subnets
#   External Secrets  2.14.0, Pod Identity, read-only on the two logins:
#                     every minute it writes both connection strings into
#                     ate-api-server-secret-envvars, the Secret ate-setup made
#   ESO Reloader      v0.3.0: when that Secret changes it rolls ate-api-server,
#                     which reads the connection strings only at start
#
# ate-api-server keeps both logins for its whole life (the owner one runs the
# outbox maintenance every minute), so both rotate, and alternating users
# keeps the old login valid while the rollout runs.
#
# clean.sh removes it. An ExternalSecret is a custom resource of External
# Secrets Operator: it copies values from Secrets Manager into a Kubernetes
# Secret, and refreshes them on a schedule.
#
# Learn more:
#   https://docs.aws.amazon.com/secretsmanager/latest/userguide/rotation-strategy.html
#   https://external-secrets.io/latest/provider/aws-secrets-manager/
#   https://github.com/external-secrets/reloader
#   https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/transform-aws-secretsmanager.html
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
ensure_kubeconfig
need helm; need envsubst
ESO_CHART_VERSION=2.14.0
ESO_IMAGE="ghcr.io/external-secrets/external-secrets:v2.14.0@sha256:10608eb53e636e744903d5f3657a4d9c44e28d9d9c181b73e07bc7dd2e6884dc"
RELOADER_VERSION=v0.3.0
RELOADER_BUNDLE_SHA256=8d0c536d9e225b1b08b2ea4c437c14dd2f43856284116d7e927d2878c730e975
RELOADER_IMAGE="ghcr.io/external-secrets/reloader:v0.3.0@sha256:94d3628a82996bc4e393aa87c97d96684f4041552ae38d2359a598cd41938a2d"
ESO_NS=external-secrets
RELOADER_NS=external-secrets-reloader
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

vpc=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.vpcId --output text)
read -r db_host master_arn < <(aws_ rds describe-db-instances --db-instance-identifier "$DB_ID" \
  --query 'DBInstances[0].[Endpoint.Address,MasterUserSecret.SecretArn]' --output text)
[ -n "$db_host" ] && [[ "$master_arn" == arn:* ]] || die "no RDS instance $DB_ID with an RDS-managed master secret; run ./deploy.sh"
db_sg=$(aws_ ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$DB_ID" \
  --query 'SecurityGroups[0].GroupId' --output text)
owner_arn=$(aws_ secretsmanager describe-secret --secret-id "$SECRET_PREFIX/ate-owner" --query ARN --output text)
rw_arn=$(aws_ secretsmanager describe-secret --secret-id "$SECRET_PREFIX/ate-readwrite" --query ARN --output text)

# The rotation functions run in the VPC with their own network interfaces,
# so the RDS security group has to admit theirs.
#   https://docs.aws.amazon.com/lambda/latest/dg/configuration-vpc.html
log "Security group for the rotation functions; RDS admits it on 5432"
rot_sg=$(aws_ ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$ROTATION_SG_NAME" \
  --query 'SecurityGroups[0].GroupId' --output text)
if [[ "$rot_sg" == None ]]; then
  rot_sg=$(aws_ ec2 create-security-group --group-name "$ROTATION_SG_NAME" --vpc-id "$vpc" \
    --description "$CLUSTER_NAME Secrets Manager rotation functions" \
    --tag-specifications "ResourceType=security-group,Tags=[$(tags_braces)]" --query GroupId --output text)
fi
grep -qw "$rot_sg" <<<"$(aws_ ec2 describe-security-groups --group-ids "$db_sg" \
  --query "SecurityGroups[0].IpPermissions[?FromPort==\`5432\`].UserIdGroupPairs[].GroupId" --output text)" \
  || aws_ ec2 authorize-security-group-ingress --group-id "$db_sg" \
    --ip-permissions "IpProtocol=tcp,FromPort=5432,ToPort=5432,UserIdGroupPairs=[{GroupId=$rot_sg}]" >/dev/null
info "$rot_sg"

# The rotation function reads engine, host, port, dbname and masterarn from
# the secret's JSON.
#   https://docs.aws.amazon.com/secretsmanager/latest/userguide/reference_secret_json_structure.html
log "Connection fields on the login secrets (the rotation function needs them)"
for arn in "$owner_arn" "$rw_arn"; do
  f="$(umask 077; mktemp "$WORK/secret.XXXXXX")"
  aws_ secretsmanager get-secret-value --secret-id "$arn" --query SecretString --output text \
    | python3 -c '
import json, sys
d = json.load(sys.stdin)
want = {"engine": "postgres", "host": sys.argv[1], "port": 5432, "dbname": "atepg", "masterarn": sys.argv[2]}
if all(d.get(k) == v for k, v in want.items()):
    sys.exit(3)
d.update(want)
print(json.dumps(d))' "$db_host" "$master_arn" > "$f" && st=0 || st=$?
  if [ "$st" = 0 ]; then
    aws_ secretsmanager put-secret-value --secret-id "$arn" --secret-string "file://$f" >/dev/null
    info "${arn##*:secret:}: added"
  elif [ "$st" = 3 ]; then
    info "${arn##*:secret:}: already there"
  else
    rm -f "$f"; die "cannot read $arn"
  fi
  rm -f "$f"
done

log "External Secrets Operator $ESO_CHART_VERSION"
cat > "$WORK/policy-external-secrets.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Action":["secretsmanager:GetSecretValue","secretsmanager:DescribeSecret"],
 "Resource":["$owner_arn","$rw_arn"]}]}
EOF
role_for_sa "$ESO_NS" external-secrets "$WORK/policy-external-secrets.json"
# Only the controller: no webhook and no cert controller, nothing listening.
cat > "$WORK/external-secrets-values.yaml" <<EOF
image: { repository: "${ESO_IMAGE%%:*}", tag: "${ESO_IMAGE#*:}" }
webhook: { create: false }
certController: { create: false }
processClusterStore: false
processClusterPushSecret: false
processPushSecret: false
nodeSelector: { ate.dev/pool: system }
# The system group's taint with AUTO_MODE=on (create-cluster.sh).
tolerations: [{ key: ate.dev/workloadType, operator: Equal, value: ate-system-infra, effect: NoSchedule }]
# Pod Identity only; never the node role.
extraEnv: [{ name: AWS_EC2_METADATA_DISABLED, value: "true" }]
EOF
# helm upgrade --install installs the chart, or upgrades the release when it
# exists.
#   https://helm.sh/docs/helm/helm_upgrade/
helm upgrade --install external-secrets external-secrets --repo https://charts.external-secrets.io \
  --version "$ESO_CHART_VERSION" --namespace "$ESO_NS" --create-namespace --wait --timeout 5m \
  --values "$WORK/external-secrets-values.yaml" >/dev/null
ensure_pod_identity "$ESO_NS" deploy/external-secrets

log "SecretStore and ExternalSecret in ate-system"
# Merge into the Secret ate-setup made: only the two connection strings are
# written; ATE_API_POSTGRES_SCHEMA stays as it is. The template in
# external-secret.yaml builds each connection string from the secret fields.
#   https://external-secrets.io/latest/api/externalsecret/
#   https://external-secrets.io/latest/guides/templating/
render_yaml "$ROOT/demos/db-rotation/external-secret.yaml" AWS_REGION SECRET_PREFIX | kubectl apply -f -
kubectl -n ate-system wait externalsecret ate-api-server-db --for=condition=Ready --timeout=3m >/dev/null
info "ExternalSecret ate-api-server-db is Ready"

log "ESO Reloader $RELOADER_VERSION"
# The release bundle is pinned by sha256: a changed file stops the script.
bundle="$WORK/reloader-bundle-$RELOADER_VERSION.yaml"
[ -f "$bundle" ] || curl -fsSL -o "$bundle" \
  "https://github.com/external-secrets/reloader/releases/download/$RELOADER_VERSION/bundle.yaml"
[ "$(sha256 "$bundle")" = "$RELOADER_BUNDLE_SHA256" ] || { rm -f "$bundle"; die "the Reloader bundle does not match its pinned sha256"; }
kubectl create namespace "$RELOADER_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed -e "s|image: \"ghcr.io/external-secrets/reloader:$RELOADER_VERSION\"|image: \"$RELOADER_IMAGE\"|" "$bundle" \
  | kubectl apply --server-side -f - >/dev/null
kubectl -n "$RELOADER_NS" patch deployment reloader --type=strategic -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"ate.dev/pool":"system"},"tolerations":[{"key":"ate.dev/workloadType","operator":"Equal","value":"ate-system-infra","effect":"NoSchedule"}]}}}}' >/dev/null
kubectl -n "$RELOADER_NS" rollout status deploy/reloader --timeout=3m
# A change to ate-api-server-secret-envvars rolls the Deployments that read
# it through envFrom: ate-api-server only (reloader-config.yaml).
render_yaml "$ROOT/demos/db-rotation/reloader-config.yaml" | kubectl apply -f -

log "Both users of each login, before the first rotation"
# The rotation function would make USER_clone itself, with INHERIT, which
# setup.sql then refuses on the next ./deploy.sh.
bash "$ROOT/aws/db-bootstrap.sh" >/dev/null
info "ate-owner and ate-readwrite: user and user_clone ready"

log "Rotation functions and schedules (CloudFormation $ROTATION_STACK)"
# The AWS::SecretsManager transform in the template turns each
# HostedRotationLambda into a rotation function from AWS's templates.
#   https://docs.aws.amazon.com/cli/latest/reference/cloudformation/deploy.html
subnets=$(aws_ ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" Name=tag:kubernetes.io/role/internal-elb,Values=1 \
  --query 'Subnets[].SubnetId' --output text | tr '\t' ',')
# shellcheck disable=SC2046
aws_ cloudformation deploy --stack-name "$ROTATION_STACK" --template-file "$ROOT/demos/db-rotation/rotation.cfn.yaml" \
  --capabilities CAPABILITY_IAM CAPABILITY_AUTO_EXPAND --no-fail-on-empty-changeset \
  --parameter-overrides "OwnerSecretArn=$owner_arn" "ReadWriteSecretArn=$rw_arn" "MasterSecretArn=$master_arn" \
    "SubnetIds=$subnets" "SecurityGroupId=$rot_sg" "NamePrefix=$CLUSTER_NAME-db" \
  --tags $(tags_kv | tr '\n' ' ')
for arn in "$owner_arn" "$rw_arn"; do
  info "${arn##*:secret:}: rotation $(aws_ secretsmanager describe-secret --secret-id "$arn" \
    --query '[RotationEnabled, RotationRules.ScheduleExpression]' --output text | tr '\t' ' ')"
done
info "next: demos/db-rotation/verify.sh   (rotates both logins now)"
