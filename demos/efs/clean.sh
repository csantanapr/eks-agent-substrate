#!/usr/bin/env bash
# Removes what demos/efs/deploy.sh and demos/s3files/deploy.sh created. Called
# by clean.sh; safe to run when neither was deployed.
#
#   demos/efs/clean.sh [--yes]
#
# Deletes: the counter-efs and counter-s3files templates and their actors (and
# with them the actors' access points), the StorageClasses, the CSIDriverConfig,
# Substrate's EFS CSI controller, the aws-efs-csi-driver add-on, the Pod Identity
# associations and IAM roles, the EFS and S3 file systems with their mount
# targets and security groups, and the S3 Files bucket with its contents and
# versions.
#
# Order matters: actors first (their access points go with them), then the
# Kubernetes objects and the add-on, then the file systems, and the security
# groups last, once the mount targets' network interfaces are gone.
#
# Learn more:
#   https://docs.aws.amazon.com/efs/latest/ug/accessing-fs.html
#   https://docs.aws.amazon.com/AmazonS3/latest/userguide/Versioning.html
set -uo pipefail
[ "${1:-}" = --yes ] && export ASSUME_YES=1
. "$(dirname "$0")/../../lib/common.sh"
confirm "This deletes the EFS and S3 Files demos and their data (file systems, bucket) for $CLUSTER_NAME."
fail=0
step() { "$@" || { warn "failed: $*"; fail=1; }; }
vpc=""

if cluster_exists; then
  ensure_kubeconfig
  vpc=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.vpcId --output text)
  log "Actors and templates on EFS and S3 Files"
  for atespace in ate-demo-counter ate-demo-counter-microvm; do
    for tpl in counter-efs counter-s3files; do
      kubectl ate get actor-template "$tpl" -a "$atespace" >/dev/null 2>&1 || continue
      # Deleting an actor deletes its volume, which deletes its access point.
      for a in $(kubectl ate get actors -a "$atespace" -o json 2>/dev/null \
          | jq -r --arg t "$tpl" '.actors[]? | select(.actorTemplate.name == $t) | .metadata.name'); do
        step kubectl ate delete actor "$a" -a "$atespace"
      done
      step kubectl ate delete actor-template "$tpl" -a "$atespace"
      info "$atespace/$tpl"
    done
  done
  step kubectl delete storageclass efs-ate s3files-ate --ignore-not-found
  step kubectl delete csidriverconfig efs.csi.aws.com --ignore-not-found
  step kubectl -n kube-system delete deployment efs-csi-controller-ate --ignore-not-found
  step kubectl -n kube-system delete service efs-csi-controller-ate --ignore-not-found
  step kubectl -n kube-system delete configmap efs-csi-controller-ate-envoy --ignore-not-found

  log "aws-efs-csi-driver add-on"
  if aws_exists eks describe-addon --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver; then
    step aws_ eks delete-addon --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver >/dev/null
    step aws_ eks wait addon-deleted --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver
  fi
  for sa in efs-csi-controller-sa efs-csi-node-sa; do
    if ids=$(aws_ eks list-pod-identity-associations --cluster-name "$CLUSTER_NAME" --namespace kube-system \
        --service-account "$sa" --query 'associations[].associationId' --output text); then
      for id in $ids; do step aws_ eks delete-pod-identity-association --cluster-name "$CLUSTER_NAME" --association-id "$id" >/dev/null; done
    else
      fail=1
    fi
  done
fi

log "IAM roles"
for role in "${IAM_PREFIX}-efs-csi-controller-sa" "${IAM_PREFIX}-efs-csi-node-sa" "${IAM_PREFIX}-s3files-sync"; do
  owned_role "$role" || continue
  if policies=$(aws_ iam list-role-policies --role-name "$role" --query 'PolicyNames[]' --output text); then
    for p in $policies; do step aws_ iam delete-role-policy --role-name "$role" --policy-name "$p"; done
    step aws_ iam delete-role --role-name "$role"
    info "$role"
  else
    fail=1
  fi
done

log "Amazon EFS"
fs=$(aws_ efs describe-file-systems --creation-token "$EFS_TOKEN" --query 'FileSystems[0].FileSystemId' --output text)
if [[ "$fs" == fs-* ]]; then
  for ap in $(aws_ efs describe-access-points --file-system-id "$fs" --query 'AccessPoints[].AccessPointId' --output text); do
    step aws_ efs delete-access-point --access-point-id "$ap"
  done
  for mt in $(aws_ efs describe-mount-targets --file-system-id "$fs" --query 'MountTargets[].MountTargetId' --output text); do
    step aws_ efs delete-mount-target --mount-target-id "$mt"
  done
  for _ in $(seq 1 60); do
    [ "$(aws_ efs describe-mount-targets --file-system-id "$fs" --query 'length(MountTargets)' --output text)" = 0 ] && break
    sleep 5
  done
  step aws_ efs delete-file-system --file-system-id "$fs"
  info "$fs"
fi

log "Amazon S3 Files"
bucket_arn="arn:$PARTITION:s3:::$S3FILES_BUCKET"
s3fs=$(aws_ s3files list-file-systems --output json 2>/dev/null \
  | jq -r --arg b "$bucket_arn" '.fileSystems[]? | select(.bucket == $b and .status != "deleted" and .status != "deleting") | .fileSystemId')
for f in $s3fs; do
  for ap in $(aws_ s3files list-access-points --file-system-id "$f" --output json | jq -r '.accessPoints[].accessPointId'); do
    step aws_ s3files delete-access-point --access-point-id "$ap"
  done
  for mt in $(aws_ s3files list-mount-targets --file-system-id "$f" --output json | jq -r '.mountTargets[].mountTargetId'); do
    step aws_ s3files delete-mount-target --mount-target-id "$mt"
  done
  for _ in $(seq 1 60); do
    [ "$(aws_ s3files list-mount-targets --file-system-id "$f" --output json | jq '.mountTargets | length')" = 0 ] && break
    sleep 5
  done
  step aws_ s3files delete-file-system --file-system-id "$f"
  for _ in $(seq 1 60); do
    st=$(aws_ s3files list-file-systems --output json | jq -r --arg f "$f" '[.fileSystems[] | select(.fileSystemId == $f)][0].status // "deleted"')
    [ "$st" = deleted ] && break
    sleep 5
  done
  info "$f"
done
if owned_bucket "$S3FILES_BUCKET"; then
  # A versioned bucket: delete every version and delete marker, then the bucket.
  python3 - "$S3FILES_BUCKET" "$AWS_REGION" <<'EOF' || fail=1
import json, subprocess, sys
bucket, region = sys.argv[1:]
def aws(*a):
    return subprocess.run(["aws", "--region", region, *a], check=True, capture_output=True, text=True).stdout
while True:
    out = json.loads(aws("s3api", "list-object-versions", "--bucket", bucket, "--max-items", "1000", "--output", "json") or "{}")
    objs = [{"Key": v["Key"], "VersionId": v["VersionId"]} for v in out.get("Versions", []) + out.get("DeleteMarkers", [])]
    if not objs:
        break
    aws("s3api", "delete-objects", "--bucket", bucket, "--delete", json.dumps({"Objects": objs, "Quiet": True}))
EOF
  step aws_ s3api delete-bucket --bucket "$S3FILES_BUCKET"
  info "$S3FILES_BUCKET"
fi

log "Security groups"
filters=("Name=group-name,Values=$EFS_SG_NAME,$S3FILES_SG_NAME")
[ -n "$vpc" ] && filters+=("Name=vpc-id,Values=$vpc")
for sg in $(aws_ ec2 describe-security-groups --filters "${filters[@]}" --query 'SecurityGroups[].GroupId' --output text); do
  # Mount target network interfaces can take a minute to go away.
  for _ in $(seq 1 24); do aws_ ec2 delete-security-group --group-id "$sg" 2>/dev/null && break; sleep 5; done
  aws_exists ec2 describe-security-groups --group-ids "$sg" && { warn "could not delete $sg"; fail=1; }
  info "$sg"
done

[ "$fail" = 0 ] || die "some EFS and S3 Files steps failed (see WARN lines); fix and run again"
log "EFS and S3 Files demos removed"
