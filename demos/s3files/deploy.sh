#!/usr/bin/env bash
# Optional: per-actor Amazon S3 Files volumes. An S3 file system presents an S3
# bucket as NFS; each actor gets its own S3 Files access point through the same
# EFS CSI driver (provisioningMode s3files-ap), and its files land in the
# bucket. Run demos/efs/deploy.sh first: it installs the driver, Substrate's
# CSI controller and the CSIDriverConfig this reuses.
#
#   demos/s3files/deploy.sh
#
#   S3         a versioned, private bucket the file system syncs with
#   IAM        the role S3 Files assumes to sync the bucket; S3 Files access
#              point management for the controllers; mount and write on this
#              file system only for the node plugin
#   S3 Files   the file system, NFS (2049) open only to the cluster security
#              group, a mount target per private subnet AZ
#   Substrate  StorageClass s3files-ate and counter-s3files templates
#
# clean.sh removes it, through demos/efs/clean.sh. S3 Files syncs changes
# between the file system and the bucket in both directions, so an actor's
# files show up as objects about a minute after they are written.
#
# Learn more:
#   https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files.html
#   https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files-prereq-policies.html
#   https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files-mounting-eks.html
#   https://docs.aws.amazon.com/AmazonS3/latest/userguide/Versioning.html
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
need envsubst
ensure_kubeconfig
kubectl get csidriverconfig efs.csi.aws.com >/dev/null 2>&1 || die "run demos/efs/deploy.sh first"
NS=kube-system
SC=s3files-ate
# shellcheck disable=SC2207
TAG_ARGS=($(tags_cli))
# S3 Files takes tags as [{"key":..,"value":..}].
s3f_tags=$(tags_kv | python3 -c 'import sys,json; print(json.dumps([{"key":l.split("=",1)[0],"value":l.rstrip("\n").split("=",1)[1]} for l in sys.stdin if l.strip()]))')

vpc=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.vpcId --output text)
cluster_sg=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.clusterSecurityGroupId --output text)

log "S3 bucket $S3FILES_BUCKET (versioned, private)"
if ! owned_bucket "$S3FILES_BUCKET"; then
  if [ "$AWS_REGION" = us-east-1 ]; then
    aws_ s3api create-bucket --bucket "$S3FILES_BUCKET" --object-ownership BucketOwnerEnforced >/dev/null
  else
    aws_ s3api create-bucket --bucket "$S3FILES_BUCKET" --object-ownership BucketOwnerEnforced \
      --create-bucket-configuration "LocationConstraint=$AWS_REGION" >/dev/null
  fi
  aws_ s3api wait bucket-exists --bucket "$S3FILES_BUCKET" --expected-bucket-owner "$ACCOUNT"
  aws_ s3api put-bucket-tagging --bucket "$S3FILES_BUCKET" --tagging "TagSet=[$(tags_braces)]"
fi
aws_ s3api put-public-access-block --bucket "$S3FILES_BUCKET" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws_ s3api put-bucket-encryption --bucket "$S3FILES_BUCKET" --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
# S3 Files needs versioning on the bucket it syncs.
aws_ s3api put-bucket-versioning --bucket "$S3FILES_BUCKET" --versioning-configuration Status=Enabled
bucket_arn="arn:$PARTITION:s3:::$S3FILES_BUCKET"
cat > "$WORK/bucket-policy-$S3FILES_BUCKET.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Sid":"DenyInsecureTransport","Effect":"Deny","Principal":"*",
 "Action":"s3:*","Resource":["$bucket_arn","$bucket_arn/*"],
 "Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}
EOF
aws_ s3api put-bucket-policy --bucket "$S3FILES_BUCKET" --policy "file://$WORK/bucket-policy-$S3FILES_BUCKET.json"
info "$S3FILES_BUCKET"

log "IAM role S3 Files assumes to sync the bucket"
sync_role="${IAM_PREFIX}-s3files-sync"
# S3 Files assumes the role as the elasticfilesystem.amazonaws.com service
# principal; the conditions limit that to file systems in this account and
# Region.
cat > "$WORK/s3files-sync-trust.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"elasticfilesystem.amazonaws.com"},
 "Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"$ACCOUNT"},
 "ArnLike":{"aws:SourceArn":"arn:$PARTITION:s3files:$AWS_REGION:$ACCOUNT:file-system/*"}}}]}
EOF
cat > "$WORK/s3files-sync-policy.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket","s3:ListBucketVersions"],"Resource":"$bucket_arn",
  "Condition":{"StringEquals":{"aws:ResourceAccount":"$ACCOUNT"}}},
 {"Effect":"Allow","Action":["s3:AbortMultipartUpload","s3:DeleteObject*","s3:GetObject*","s3:List*","s3:PutObject*"],
  "Resource":"$bucket_arn/*","Condition":{"StringEquals":{"aws:ResourceAccount":"$ACCOUNT"}}},
 {"Effect":"Allow","Action":["events:DeleteRule","events:DisableRule","events:EnableRule","events:PutRule",
   "events:PutTargets","events:RemoveTargets"],
  "Condition":{"StringEquals":{"events:ManagedBy":"elasticfilesystem.amazonaws.com"}},
  "Resource":"arn:$PARTITION:events:*:*:rule/DO-NOT-DELETE-S3-Files*"},
 {"Effect":"Allow","Action":["events:DescribeRule","events:ListRuleNamesByTarget","events:ListRules","events:ListTargetsByRule"],
  "Resource":"arn:$PARTITION:events:*:*:rule/*"}]}
EOF
if owned_role "$sync_role"; then
  aws_ iam update-assume-role-policy --role-name "$sync_role" --policy-document "file://$WORK/s3files-sync-trust.json"
else
  aws_ iam create-role --role-name "$sync_role" --assume-role-policy-document "file://$WORK/s3files-sync-trust.json" \
    --tags "${TAG_ARGS[@]}" >/dev/null
fi
aws_ iam put-role-policy --role-name "$sync_role" --policy-name sync --policy-document "file://$WORK/s3files-sync-policy.json"
info "$sync_role"

log "S3 file system"
fs=$(aws_ s3files list-file-systems --output json \
  | jq -r --arg b "$bucket_arn" '[.fileSystems[] | select(.bucket == $b and .status != "deleted")][0].fileSystemId // empty')
if [ -z "$fs" ]; then
  # A new role can take a few seconds before S3 Files may assume it.
  for i in $(seq 1 12); do
    if fs=$(aws_ s3files create-file-system --bucket "$bucket_arn" --role-arn "arn:$PARTITION:iam::$ACCOUNT:role/$sync_role" \
        --client-token "$CLUSTER_NAME-$AWS_REGION" --tags "$s3f_tags" --query fileSystemId --output text 2>"$WORK/s3files.err"); then break; fi
    [ "$i" -lt 12 ] || die "create-file-system: $(cat "$WORK/s3files.err")"
    sleep 10
  done
fi
for _ in $(seq 1 60); do
  st=$(aws_ s3files get-file-system --file-system-id "$fs" --query status --output text)
  [ "$st" = available ] && break
  sleep 10
done
[ "$st" = available ] || die "S3 file system $fs is $st"
fs_arn=$(aws_ s3files get-file-system --file-system-id "$fs" --query fileSystemArn --output text)
info "$fs"

log "Security group and mount targets"
sg=$(aws_ ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$S3FILES_SG_NAME" \
  --query 'SecurityGroups[0].GroupId' --output text)
if [[ "$sg" == None ]]; then
  sg=$(aws_ ec2 create-security-group --group-name "$S3FILES_SG_NAME" --vpc-id "$vpc" \
    --description "$CLUSTER_NAME S3 Files: 2049 from the EKS cluster security group only" \
    --tag-specifications "ResourceType=security-group,Tags=[$(tags_braces)]" --query GroupId --output text)
fi
grep -qw "$cluster_sg" <<<"$(aws_ ec2 describe-security-groups --group-ids "$sg" \
  --query "SecurityGroups[0].IpPermissions[?FromPort==\`2049\`].UserIdGroupPairs[].GroupId" --output text)" \
  || aws_ ec2 authorize-security-group-ingress --group-id "$sg" \
    --ip-permissions "IpProtocol=tcp,FromPort=2049,ToPort=2049,UserIdGroupPairs=[{GroupId=$cluster_sg}]" >/dev/null
have=$(aws_ s3files list-mount-targets --file-system-id "$fs" --output json | jq -r '.mountTargets[].subnetId')
while read -r subnet az; do
  grep -qw "$subnet" <<<"$have" && { info "$az exists"; continue; }
  aws_ s3files create-mount-target --file-system-id "$fs" --subnet-id "$subnet" --security-groups "$sg" >/dev/null
  info "$az created"
done < <(aws_ ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" Name=tag:kubernetes.io/role/internal-elb,Values=1 \
  --query 'Subnets[].[SubnetId,AvailabilityZone]' --output text)
for _ in $(seq 1 60); do
  pending=$(aws_ s3files list-mount-targets --file-system-id "$fs" --output json \
    | jq '[.mountTargets[] | select(.status != "available")] | length')
  [ "$pending" = 0 ] && break
  sleep 10
done
[ "$pending" = 0 ] || die "S3 Files mount targets are not available"

log "IAM: controllers manage S3 Files access points; the node plugin mounts this file system"
cat > "$WORK/policy-s3files-controller.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3files:ListAccessPoints","s3files:ListFileSystems"],"Resource":"*"},
 {"Effect":"Allow","Action":"s3files:CreateAccessPoint","Resource":"$fs_arn",
  "Condition":{"Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},
               "ForAllValues:StringEquals":{"aws:TagKeys":"efs.csi.aws.com/cluster"}}},
 {"Effect":"Allow","Action":"s3files:TagResource","Resource":"*",
  "Condition":{"StringEquals":{"s3files:CreateAction":"CreateAccessPoint"},
               "Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},
               "ForAllValues:StringEquals":{"aws:TagKeys":"efs.csi.aws.com/cluster"}}},
 {"Effect":"Allow","Action":"s3files:DeleteAccessPoint","Resource":"$fs_arn/access-point/*",
  "Condition":{"StringEquals":{"aws:ResourceTag/efs.csi.aws.com/cluster":"true"}}}]}
EOF
aws_ iam put-role-policy --role-name "${IAM_PREFIX}-efs-csi-controller-sa" --policy-name s3files \
  --policy-document "file://$WORK/policy-s3files-controller.json"
# Root access only because the counter runs as root; an actor image with a
# non-root USER needs ClientMount and ClientWrite only.
root_access=""
[ "$EFS_UID" = 0 ] && root_access=',"s3files:ClientRootAccess"'
cat > "$WORK/policy-efs-csi-node.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3files:ClientMount","s3files:ClientWrite"$root_access],"Resource":"$fs_arn"},
 {"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":"$bucket_arn/*"},
 {"Effect":"Allow","Action":"s3:ListBucket","Resource":"$bucket_arn"}]}
EOF
role_for_sa "$NS" efs-csi-node-sa "$WORK/policy-efs-csi-node.json"
ensure_pod_identity "$NS" ds/efs-csi-node

log "StorageClass $SC"
have_fs=$(kubectl get storageclass "$SC" -o jsonpath='{.parameters.fileSystemId}' 2>/dev/null || true)
if kubectl get storageclass "$SC" >/dev/null 2>&1 && [ "$have_fs" != "$fs" ]; then kubectl delete storageclass "$SC"; fi
# provisioningMode s3files-ap makes one S3 Files access point per volume
# (storageclass.yaml).
render_yaml "$ROOT/demos/s3files/storageclass.yaml" SC fs EFS_UID EFS_GID | kubectl apply -f -

log "Counter templates with the data volume on S3 Files"
digest=$(aws_ ecr describe-images --repository-name "$ECR_PREFIX/counter" \
  --image-ids "imageTag=$SUBSTRATE_TAG" --query 'imageDetails[0].imageDigest' --output text)
COUNTER_IMAGE="$ECR_REGISTRY/$ECR_PREFIX/counter@$digest"
for class in gvisor microvm; do
  atespace=ate-demo-counter workload=counter
  [ "$class" = microvm ] && { atespace=ate-demo-counter-microvm; workload=counter-microvm; }
  if kubectl ate get actor-template counter-s3files -a "$atespace" >/dev/null 2>&1; then
    info "$atespace/counter-s3files exists"; continue
  fi
  cat <<EOF | kubectl ate --preview create actor-template -f -
metadata: { atespace: $atespace, name: counter-s3files }
workerSelector: { matchLabels: { workload: $workload } }
containers:
- name: counter
  image: $COUNTER_IMAGE
  command: [/ko-app/counter]
  wakeupProbe: { httpGet: { path: /readyz, port: 80 } }
  volumeMounts: [{ name: data, mountPath: /home/counter }]
resources: { limits: { cpuMillicores: 1000, memoryBytes: 536870912 } }
snapshotConfig: { preferredFidelity: SNAPSHOT_FIDELITY_MEMORY }
volumes:
- name: data
  external: { template: { capacityBytes: 1073741824, storageClassName: $SC } }
EOF
done
info "next: STORAGE=s3files demos/efs/verify.sh"
