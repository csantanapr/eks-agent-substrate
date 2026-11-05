#!/usr/bin/env bash
# Optional: per-actor Amazon EFS volumes through Substrate's CSI support
# (ExternalVolumes preview). Each actor gets its own EFS access point, created
# when the actor is created and mounted on whichever node it resumes on.
#
#   demos/efs/deploy.sh          after ./deploy.sh
#
#   EFS        one encrypted file system, a mount target per private subnet AZ,
#              NFS (2049) open only to the cluster security group
#   add-on     aws-efs-csi-driver (EKS add-on). Its node plugin also mounts
#              /var/lib/ate with bidirectional propagation, so the mounts it
#              makes are visible to atelet
#   controller a second EFS CSI controller for Substrate: ate-api-server calls
#              CreateVolume over gRPC, so it needs a network endpoint. An Envoy
#              sidecar serves mTLS with a Substrate pod certificate and accepts
#              only ate-api-server's SPIFFE ID
#   IAM        the controllers' Pod Identity role may manage access points on
#              this file system only
#   Substrate  reinstalled with --preview=ExternalVolumes; a CSIDriverConfig, a
#              StorageClass, and counter templates whose data volume is on EFS
#
# demos/s3files/deploy.sh builds on this one, and clean.sh removes both. A CSI
# (Container Storage Interface) driver is a plugin that creates and mounts
# storage for Kubernetes, and here for Substrate too. An EFS access point is
# an entry into the file system with its own root directory and POSIX user.
#
# Learn more:
#   https://docs.aws.amazon.com/eks/latest/userguide/efs-csi.html
#   https://docs.aws.amazon.com/efs/latest/ug/efs-access-points.html
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/guides/csi-volumes.md
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/guides/csi-deployment.md
set -euo pipefail
. "$(dirname "$0")/../../lib/common.sh"
ensure_kubeconfig
NS=kube-system
SVC=efs-csi-controller-ate
SC=efs-ate
ENVOY_IMAGE=envoyproxy/envoy:v1.39.5@sha256:8fc594a5d85eb76dca24155cfb9208882406873932b20cf33649a0a611194c43
# shellcheck disable=SC2207
TAG_ARGS=($(tags_cli))

vpc=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.vpcId --output text)
cluster_sg=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.resourcesVpcConfig.clusterSecurityGroupId --output text)

log "Amazon EFS file system"
fs=$(aws_ efs describe-file-systems --creation-token "$EFS_TOKEN" --query 'FileSystems[0].FileSystemId' --output text)
if [[ "$fs" == None || -z "$fs" ]]; then
  fs=$(aws_ efs create-file-system --creation-token "$EFS_TOKEN" --encrypted \
    --performance-mode generalPurpose --throughput-mode elastic \
    --tags "Key=Name,Value=$CLUSTER_NAME" "${TAG_ARGS[@]}" --query FileSystemId --output text)
fi
for _ in $(seq 1 30); do
  [ "$(aws_ efs describe-file-systems --file-system-id "$fs" --query 'FileSystems[0].LifeCycleState' --output text)" = available ] && break
  sleep 5
done
info "$fs"

# EFS speaks NFS on TCP 2049; this group goes on the mount targets.
#   https://docs.aws.amazon.com/efs/latest/ug/network-access.html
log "Security group: NFS from the cluster security group only"
efs_sg=$(aws_ ec2 describe-security-groups --filters "Name=vpc-id,Values=$vpc" "Name=group-name,Values=$EFS_SG_NAME" \
  --query 'SecurityGroups[0].GroupId' --output text)
if [[ "$efs_sg" == None ]]; then
  efs_sg=$(aws_ ec2 create-security-group --group-name "$EFS_SG_NAME" --vpc-id "$vpc" \
    --description "$CLUSTER_NAME EFS: 2049 from the EKS cluster security group only" \
    --tag-specifications "ResourceType=security-group,Tags=[$(tags_braces)]" --query GroupId --output text)
fi
has_rule=$(aws_ ec2 describe-security-groups --group-ids "$efs_sg" \
  --query "SecurityGroups[0].IpPermissions[?FromPort==\`2049\`].UserIdGroupPairs[].GroupId" --output text)
if ! grep -qw "$cluster_sg" <<<"$has_rule"; then
  aws_ ec2 authorize-security-group-ingress --group-id "$efs_sg" \
    --ip-permissions "IpProtocol=tcp,FromPort=2049,ToPort=2049,UserIdGroupPairs=[{GroupId=$cluster_sg}]" >/dev/null
fi
info "$efs_sg"

# A mount target is the file system's network endpoint in one Availability
# Zone; nodes mount through the one in their own zone.
#   https://docs.aws.amazon.com/efs/latest/ug/accessing-fs.html
log "Mount targets, one per Availability Zone"
have_azs=$(aws_ efs describe-mount-targets --file-system-id "$fs" --query 'MountTargets[].AvailabilityZoneName' --output text)
subnets=$(aws_ ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" Name=tag:kubernetes.io/role/internal-elb,Values=1 \
  --query 'Subnets[].[SubnetId,AvailabilityZone]' --output text)
while read -r subnet az; do
  grep -qw "$az" <<<"$have_azs" && { info "$az exists"; continue; }
  aws_ efs create-mount-target --file-system-id "$fs" --subnet-id "$subnet" --security-groups "$efs_sg" >/dev/null
  have_azs+=" $az"
  info "$az created"
done <<<"$subnets"
for _ in $(seq 1 60); do
  pending=$(aws_ efs describe-mount-targets --file-system-id "$fs" --query "MountTargets[?LifeCycleState!='available'] | length(@)" --output text)
  [ "$pending" = 0 ] && break
  sleep 10
done
[ "$pending" = 0 ] || die "EFS mount targets are not available"

log "IAM role for the EFS CSI controllers ($fs only)"
fs_arn="arn:$PARTITION:elasticfilesystem:$AWS_REGION:$ACCOUNT:file-system/$fs"
# Access points may be created only with the efs.csi.aws.com/cluster tag, and
# deleted only when they carry it.
cat > "$WORK/policy-efs-csi-controller.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["elasticfilesystem:DescribeAccessPoints","elasticfilesystem:DescribeFileSystems",
   "elasticfilesystem:DescribeMountTargets","ec2:DescribeAvailabilityZones"],"Resource":"*"},
 {"Effect":"Allow","Action":"elasticfilesystem:CreateAccessPoint","Resource":"$fs_arn",
  "Condition":{"Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},
               "ForAllValues:StringEquals":{"aws:TagKeys":"efs.csi.aws.com/cluster"}}},
 {"Effect":"Allow","Action":"elasticfilesystem:TagResource","Resource":"$fs_arn",
  "Condition":{"StringEquals":{"elasticfilesystem:CreateAction":"CreateAccessPoint"},
               "Null":{"aws:RequestTag/efs.csi.aws.com/cluster":"false"},
               "ForAllValues:StringEquals":{"aws:TagKeys":"efs.csi.aws.com/cluster"}}},
 {"Effect":"Allow","Action":"elasticfilesystem:DeleteAccessPoint",
  "Resource":"arn:$PARTITION:elasticfilesystem:$AWS_REGION:$ACCOUNT:access-point/*",
  "Condition":{"StringEquals":{"aws:ResourceTag/efs.csi.aws.com/cluster":"true"}}}]}
EOF
role_for_sa "$NS" efs-csi-controller-sa "$WORK/policy-efs-csi-controller.json"

log "EKS add-on aws-efs-csi-driver"
# The node plugin mounts each volume under /var/lib/ate, where atelet keeps
# actor directories; Bidirectional makes the mount visible on the host.
#   https://kubernetes.io/docs/concepts/storage/volumes/#mount-propagation
addon_config=$(jq -cn '{node: {
  volumes: [{name: "ate-base", hostPath: {path: "/var/lib/ate", type: "DirectoryOrCreate"}}],
  volumeMounts: [{name: "ate-base", mountPath: "/var/lib/ate", mountPropagation: "Bidirectional"}]}}')
if aws_exists eks describe-addon --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver; then
  aws_ eks update-addon --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver \
    --addon-version "$EFS_ADDON_VERSION" --configuration-values "$addon_config" --resolve-conflicts OVERWRITE >/dev/null
else
  aws_ eks create-addon --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver \
    --addon-version "$EFS_ADDON_VERSION" --configuration-values "$addon_config" --tags "$(tags_csv)" >/dev/null
fi
aws_ eks wait addon-active --cluster-name "$CLUSTER_NAME" --addon-name aws-efs-csi-driver
kubectl -n "$NS" rollout status ds/efs-csi-node --timeout=5m
kubectl -n "$NS" rollout status deploy/efs-csi-controller --timeout=5m
ensure_pod_identity "$NS" deploy/efs-csi-controller
EFS_IMAGE=$(kubectl -n "$NS" get deploy efs-csi-controller -o json \
  | jq -r '.spec.template.spec.containers[] | select(.name=="efs-plugin") | .image')
info "driver image: ${EFS_IMAGE##*/}"

log "Substrate's EFS CSI controller (ClusterIP, mTLS)"
# The Service comes first: the servicedns signer issues the Envoy serving
# certificate only for pods a Service selects.
cfg_sha=$(sha256sum "$ROOT/demos/efs/controller.yaml" 2>/dev/null || shasum -a 256 "$ROOT/demos/efs/controller.yaml")
sed -e "s|REPLACE_WITH_EFS_IMAGE|$EFS_IMAGE|" -e "s|REPLACE_WITH_ENVOY_IMAGE|$ENVOY_IMAGE|" \
  -e "s|REPLACE_WITH_CONFIG_SHA|${cfg_sha%% *}|" \
  "$ROOT/demos/efs/controller.yaml" | kubectl apply -f -
kubectl -n "$NS" rollout status "deploy/$SVC" --timeout=5m
ensure_pod_identity "$NS" "deploy/$SVC"

log "Agent Substrate with the ExternalVolumes preview"
# install.sh suspends running actors first, and later installs keep the gate.
# A preview gate turns on a feature that is off by default.
#   https://github.com/agent-substrate/substrate/blob/v1.0.0-rc.3/docs/dev/preview-gates.md
bash "$ROOT/substrate/install.sh" --preview=ExternalVolumes

log "CSIDriverConfig and StorageClass $SC"
# StorageClass parameters are immutable: replace the class when they change.
have_uid=$(kubectl get storageclass "$SC" -o jsonpath='{.parameters.uid}' 2>/dev/null || true)
if kubectl get storageclass "$SC" >/dev/null 2>&1 && [ "$have_uid" != "$EFS_UID" ]; then
  kubectl delete storageclass "$SC"
fi
# The CSIDriverConfig tells ate-api-server where the driver's controller
# listens and to use TLS. provisioningMode efs-ap makes one access point per
# volume, under basePath.
cat <<EOF | kubectl apply -f -
apiVersion: ate.dev/v1
kind: CSIDriverConfig
metadata:
  name: efs.csi.aws.com
spec:
  driverName: efs.csi.aws.com
  controllerEndpoint: tcp://$SVC.$NS.svc.cluster.local:50052
  tls:
    enabled: true
    usePodIdentity: true
    serverName: $SVC.$NS.svc
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $SC
provisioner: efs.csi.aws.com
parameters:
  provisioningMode: efs-ap
  fileSystemId: $fs
  directoryPerms: "700"
  uid: "$EFS_UID"
  gid: "$EFS_GID"
  basePath: /ate
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF

log "Counter templates with the data volume on EFS"
digest=$(aws_ ecr describe-images --repository-name "$ECR_PREFIX/counter" \
  --image-ids "imageTag=$SUBSTRATE_TAG" --query 'imageDetails[0].imageDigest' --output text)
COUNTER_IMAGE="$ECR_REGISTRY/$ECR_PREFIX/counter@$digest"
# The external volume template asks StorageClass $SC for a new volume per
# actor, mounted at /home/counter.
for class in gvisor microvm; do
  atespace=ate-demo-counter workload=counter
  [ "$class" = microvm ] && { atespace=ate-demo-counter-microvm; workload=counter-microvm; }
  if kubectl ate get actor-template counter-efs -a "$atespace" >/dev/null 2>&1; then
    info "$atespace/counter-efs exists"; continue
  fi
  cat <<EOF | kubectl ate --preview create actor-template -f -
metadata: { atespace: $atespace, name: counter-efs }
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
info "next: demos/efs/verify.sh"
