#!/usr/bin/env bash
# The AWS resources Agent Substrate uses, all private. Idempotent.
#
#   S3       snapshot bucket + microVM asset bucket: Block Public Access,
#            BucketOwnerEnforced, SSE-S3, TLS-only bucket policy, a lifecycle
#            rule for incomplete multipart uploads
#   IAM      one role per ServiceAccount, assumed through EKS Pod Identity
#            (no static keys, no IRSA annotations)
#              ate-system/ate-api-server  snapshot bucket: list, read, write,
#                                         delete (its objectstore sidecar
#                                         copies tag snapshots and deletes
#                                         unreferenced ones)
#              ate-system/atelet          snapshot bucket: read and write, no
#                                         delete (fetch and upload); asset
#                                         bucket read; ECR pull (atelet runs
#                                         the node's ecr-credential-provider)
#   ECR      private repositories for the demo images, scan on push
#   RDS      PostgreSQL in the cluster's private subnets, port 5432 open only
#            to the cluster security group, master password managed by RDS in
#            Secrets Manager
#   Secrets  the owner and readwrite logins Substrate uses
set -euo pipefail
. "$(dirname "$0")/../lib/common.sh"

vpc=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.vpcId --output text)
cluster_sg=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.clusterSecurityGroupId --output text)
subnets=$(aws_ ec2 describe-subnets \
  --filters "Name=vpc-id,Values=$vpc" Name=tag:kubernetes.io/role/internal-elb,Values=1 \
  --query 'Subnets[].SubnetId' --output text)
read -ra private_subnets <<<"$subnets"
[ "${#private_subnets[@]}" -ge 2 ] || die "found ${#private_subnets[@]} private subnets in $vpc; RDS needs two"
# shellcheck disable=SC2207
TAG_ARGS=($(tags_cli))

log "S3 buckets"
for b in "$SNAP_BUCKET" "$ASSET_BUCKET"; do
  if ! owned_bucket "$b"; then
    if [ "$AWS_REGION" = us-east-1 ]; then
      aws_ s3api create-bucket --bucket "$b" --object-ownership BucketOwnerEnforced >/dev/null
    else
      aws_ s3api create-bucket --bucket "$b" --object-ownership BucketOwnerEnforced \
        --create-bucket-configuration "LocationConstraint=$AWS_REGION" >/dev/null
    fi
    aws_ s3api wait bucket-exists --bucket "$b" --expected-bucket-owner "$ACCOUNT"
    # Tag it first: owned_bucket only reuses a bucket with the owner tag.
    aws_ s3api put-bucket-tagging --bucket "$b" --tagging "TagSet=[$(tags_braces)]"
  fi
  aws_ s3api put-public-access-block --bucket "$b" --expected-bucket-owner "$ACCOUNT" --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  aws_ s3api put-bucket-encryption --bucket "$b" --server-side-encryption-configuration \
    '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
  aws_ s3api put-bucket-tagging --bucket "$b" --tagging "TagSet=[$(tags_braces)]"
  cat > "$WORK/bucket-policy-$b.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Sid":"DenyInsecureTransport","Effect":"Deny","Principal":"*",
 "Action":"s3:*","Resource":["arn:$PARTITION:s3:::$b","arn:$PARTITION:s3:::$b/*"],
 "Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}
EOF
  aws_ s3api put-bucket-policy --bucket "$b" --policy "file://$WORK/bucket-policy-$b.json"
  aws_ s3api put-bucket-lifecycle-configuration --bucket "$b" --lifecycle-configuration \
    '{"Rules":[{"ID":"abort-incomplete-mpu","Status":"Enabled","Filter":{},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}' >/dev/null
  info "$b"
done

log "ECR repositories"
for r in counter openclaw-gateway openclaw-actor openclaw-dashboard; do
  repo="$ECR_PREFIX/$r"
  if ! aws_exists ecr describe-repositories --repository-names "$repo"; then
    aws_ ecr create-repository --repository-name "$repo" \
      --image-scanning-configuration scanOnPush=true --encryption-configuration encryptionType=AES256 \
      --tags "${TAG_ARGS[@]}" >/dev/null
  fi
  info "$repo"
done

log "IAM roles for EKS Pod Identity"
cat > "$WORK/policy-ate-api-server.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":"arn:$PARTITION:s3:::$SNAP_BUCKET"},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:AbortMultipartUpload"],"Resource":"arn:$PARTITION:s3:::$SNAP_BUCKET/*"}]}
EOF
cat > "$WORK/policy-atelet.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:$PARTITION:s3:::$SNAP_BUCKET","arn:$PARTITION:s3:::$ASSET_BUCKET"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:AbortMultipartUpload"],"Resource":"arn:$PARTITION:s3:::$SNAP_BUCKET/*"},
 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:$PARTITION:s3:::$ASSET_BUCKET/*"},
 {"Effect":"Allow","Action":"ecr:GetAuthorizationToken","Resource":"*"},
 {"Effect":"Allow","Action":["ecr:BatchGetImage","ecr:GetDownloadUrlForLayer","ecr:BatchCheckLayerAvailability"],
  "Resource":"arn:$PARTITION:ecr:$AWS_REGION:$ACCOUNT:repository/$ECR_PREFIX/*"}]}
EOF
role_for_sa ate-system ate-api-server "$WORK/policy-ate-api-server.json"
role_for_sa ate-system atelet "$WORK/policy-atelet.json"

log "Amazon RDS for PostgreSQL (private)"
if ! aws_exists rds describe-db-subnet-groups --db-subnet-group-name "$DB_ID"; then
  aws_ rds create-db-subnet-group --db-subnet-group-name "$DB_ID" \
    --db-subnet-group-description "$CLUSTER_NAME private subnets" \
    --subnet-ids "${private_subnets[@]}" --tags "${TAG_ARGS[@]}" >/dev/null
fi
db_sg=$(aws_ ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$DB_ID" \
  --query 'SecurityGroups[0].GroupId' --output text)
if [[ "$db_sg" == "None" ]]; then
  db_sg=$(aws_ ec2 create-security-group --group-name "$DB_ID" --vpc-id "$vpc" \
    --description "$CLUSTER_NAME RDS: 5432 from the EKS cluster security group only" \
    --tag-specifications "ResourceType=security-group,Tags=[$(tags_braces)]" \
    --query GroupId --output text)
fi
# Exactly one ingress rule: 5432 from the cluster security group.
rules=$(aws_ ec2 describe-security-groups --group-ids "$db_sg" --output json \
  --query 'SecurityGroups[0].IpPermissions[].{p:IpProtocol,f:FromPort,t:ToPort,g:UserIdGroupPairs[].GroupId,c:IpRanges[].CidrIp,c6:Ipv6Ranges[].CidrIpv6}')
case "$(python3 -c '
import sys, json
r = json.load(sys.stdin); want = sys.argv[1]
ok = [x for x in r if x["p"] == "tcp" and x["f"] == 5432 and x["t"] == 5432 and x["g"] == [want] and not x["c"] and not x["c6"]]
print("ok" if len(r) == 1 and ok else "none" if not r else "other")' "$cluster_sg" <<<"$rules")" in
  ok) ;;
  none) aws_ ec2 authorize-security-group-ingress --group-id "$db_sg" \
          --ip-permissions "IpProtocol=tcp,FromPort=5432,ToPort=5432,UserIdGroupPairs=[{GroupId=$cluster_sg}]" >/dev/null ;;
  *) die "security group $DB_ID ($db_sg) has ingress rules other than 5432 from $cluster_sg: $rules" ;;
esac
if ! aws_exists rds describe-db-instances --db-instance-identifier "$DB_ID"; then
  multi_az=--no-multi-az; [ "$DB_MULTI_AZ" = true ] && multi_az=--multi-az
  protect=--no-deletion-protection; [ "${DB_DELETION_PROTECTION:-false}" = true ] && protect=--deletion-protection
  aws_ rds create-db-instance --db-instance-identifier "$DB_ID" \
    --engine postgres --engine-version "$DB_ENGINE_VERSION" --db-instance-class "$DB_INSTANCE_CLASS" \
    --allocated-storage "$DB_STORAGE_GB" --storage-type gp3 --storage-encrypted \
    --db-name atepg --master-username ateadmin --manage-master-user-password \
    --db-subnet-group-name "$DB_ID" --vpc-security-group-ids "$db_sg" "$multi_az" "$protect" \
    --no-publicly-accessible --backup-retention-period "$DB_BACKUP_DAYS" --copy-tags-to-snapshot \
    --ca-certificate-identifier rds-ca-rsa2048-g1 --tags "${TAG_ARGS[@]}" >/dev/null
  info "creating $DB_ID (about 10 minutes)"
else
  info "$DB_ID exists"
fi

log "Secrets Manager: Substrate's database logins"
for who in owner readwrite; do
  name="$SECRET_PREFIX/ate-$who"
  if ! aws_exists secretsmanager describe-secret --secret-id "$name"; then
    # The password goes through a 0600 file, never a command-line argument.
    secret_file="$(umask 077; mktemp "$WORK/secret.XXXXXX")"
    pw=$(aws_ secretsmanager get-random-password --password-length 40 --exclude-punctuation \
      --query RandomPassword --output text)
    printf '{"username":"substrate_%s_user","password":"%s"}' "$who" "$pw" > "$secret_file"
    unset pw
    # A secret deleted by clean.sh can hold its name for a short while.
    for i in $(seq 1 12); do
      if err=$(aws_ secretsmanager create-secret --name "$name" \
          --description "Agent Substrate $who login for $DB_ID" \
          --secret-string "file://$secret_file" --tags "${TAG_ARGS[@]}" 2>&1 >/dev/null); then break; fi
      grep -q 'scheduled for deletion' <<<"$err" && [ "$i" -lt 12 ] || { rm -f "$secret_file"; die "create-secret $name: $err"; }
      info "$name is still being deleted; retrying"
      sleep 10
    done
    rm -f "$secret_file"
  fi
  info "$name"
done

curl -fsS -o "$WORK/rds-global-bundle.pem" https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
info "RDS CA bundle: $WORK/rds-global-bundle.pem"
