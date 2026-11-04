#!/usr/bin/env bash
# Creates the EKS cluster with eksctl: Amazon Linux 2023 managed node groups,
# one per role, and the add-ons Agent Substrate needs.
#
#   ./create-cluster.sh          about 20 minutes
#
#   system   control plane components (ate-api-server, controllers, atenet,
#            the LLM gateway)
#   gvisor   gVisor worker pods, tainted ate.dev/sandboxClass=gvisor
#   microvm  Kata + Cloud Hypervisor worker pods, tainted
#            ate.dev/sandboxClass=microvm, on a launch template that turns
#            on nested virtualization (/dev/kvm on a regular EC2 instance)
#
# Every node: IMDSv2 only, hop limit 1, so pods in their own network
# namespace cannot read the node's credentials from IMDS. Host-network pods
# (CNI, kube-proxy, CSI node) still can; keep those to trusted system
# DaemonSets. Workloads get AWS access only through EKS Pod Identity.
#
# The Kubernetes API endpoint is public and private. Set
# EKS_PUBLIC_ACCESS_CIDRS in config.local.env to your own address range.
#
# Rerunning it adds node groups or add-ons that are missing (after a failed
# first run) and leaves the existing ones alone.
set -euo pipefail
. "$(dirname "$0")/lib/common.sh"
need eksctl; need aws; need kubectl

phase "Preflight + launch template"
log "Preflight"
nv=$(aws_ ec2 describe-instance-types --instance-types "$MICROVM_INSTANCE_TYPE" \
  --query 'InstanceTypes[0].ProcessorInfo.SupportedFeatures' --output text)
grep -qw nested-virtualization <<<"$nv" \
  || die "$MICROVM_INSTANCE_TYPE does not support nested virtualization; list the ones that do with:
  aws ec2 describe-instance-types --filters Name=processor-info.supported-features,Values=nested-virtualization"
info "$MICROVM_INSTANCE_TYPE supports nested virtualization"
[ "${EKS_PUBLIC_ACCESS_CIDRS:-0.0.0.0/0}" = 0.0.0.0/0 ] \
  && warn "the Kubernetes API accepts connections from any address (authenticated by IAM); set EKS_PUBLIC_ACCESS_CIDRS to narrow it"

log "Launch template for the microvm node group (nested virtualization)"
cat > "$WORK/lt-data.json" <<EOF
{
  "CpuOptions": {"NestedVirtualization": "enabled"},
  "MetadataOptions": {"HttpTokens": "required", "HttpPutResponseHopLimit": 1, "HttpEndpoint": "enabled"},
  "BlockDeviceMappings": [
    {"DeviceName": "/dev/xvda",
     "Ebs": {"VolumeSize": 200, "VolumeType": "gp3", "DeleteOnTermination": true, "Encrypted": true}}
  ],
  "TagSpecifications": [
    {"ResourceType": "instance", "Tags": $(tags_json)},
    {"ResourceType": "volume", "Tags": $(tags_json)},
    {"ResourceType": "network-interface", "Tags": $(tags_json)}
  ]
}
EOF
# The settings that matter for isolation, as one comparable line.
lt_digest() {
  python3 -c '
import sys, json
d = json.load(sys.stdin)
d = d.get("LaunchTemplateData", d)
m, e = d.get("MetadataOptions", {}), d["BlockDeviceMappings"][0]["Ebs"]
print(d.get("CpuOptions", {}).get("NestedVirtualization"), m.get("HttpTokens"),
      m.get("HttpPutResponseHopLimit"), e.get("Encrypted"), e.get("DeleteOnTermination"), e.get("VolumeSize"))'
}
if aws_exists ec2 describe-launch-templates --launch-template-names "$LT_NAME"; then
  lt_id=$(aws_ ec2 describe-launch-templates --launch-template-names "$LT_NAME" \
    --query 'LaunchTemplates[0].LaunchTemplateId' --output text)
  owner=$(aws_ ec2 describe-launch-templates --launch-template-ids "$lt_id" \
    --query "LaunchTemplates[0].Tags[?Key=='$OWNER_TAG_KEY'].Value | [0]" --output text)
  [ "$owner" = "$OWNER_TAG_VALUE" ] || die "launch template $LT_NAME ($lt_id) is not owned by $OWNER_TAG_VALUE"
  lt_version=$(aws_ ec2 describe-launch-templates --launch-template-ids "$lt_id" \
    --query 'LaunchTemplates[0].LatestVersionNumber' --output text)
  have=$(aws_ ec2 describe-launch-template-versions --launch-template-id "$lt_id" --versions "$lt_version" \
    --query 'LaunchTemplateVersions[0]' --output json | lt_digest)
  if [ "$have" != "$(lt_digest < "$WORK/lt-data.json")" ]; then
    lt_version=$(aws_ ec2 create-launch-template-version --launch-template-id "$lt_id" \
      --launch-template-data "file://$WORK/lt-data.json" \
      --query 'LaunchTemplateVersion.VersionNumber' --output text)
    info "$lt_id differed from the desired settings; created version $lt_version"
  else
    info "reusing $lt_id version $lt_version"
  fi
else
  lt_id=$(aws_ ec2 create-launch-template --launch-template-name "$LT_NAME" \
    --launch-template-data "file://$WORK/lt-data.json" \
    --tag-specifications "ResourceType=launch-template,Tags=[$(tags_braces)]" \
    --query 'LaunchTemplate.LaunchTemplateId' --output text)
  lt_version=1
  info "created $lt_id"
fi

cidrs=""
for c in ${EKS_PUBLIC_ACCESS_CIDRS:-0.0.0.0/0}; do cidrs+="\"$c\", "; done
cat > "$WORK/cluster.yaml" <<EOF
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: ${CLUSTER_NAME}
  region: ${AWS_REGION}
  version: "${KUBERNETES_VERSION}"
  tags:
$(tags_yaml 4)
vpc:
  clusterEndpoints: { publicAccess: true, privateAccess: true }
  publicAccessCIDRs: [${cidrs%, }]
iam:
  withOIDC: false
addons:
  - name: vpc-cni
    # Substrate ships NetworkPolicies (default deny for ate-system, one per
    # WorkerPool); the VPC CNI enforces them only with this on.
    configurationValues: '{"enableNetworkPolicy": "true"}'
  - name: coredns
  - name: kube-proxy
  - name: eks-pod-identity-agent
  - name: aws-ebs-csi-driver
    useDefaultPodIdentityAssociations: true
managedNodeGroups:
  - name: system
    amiFamily: AmazonLinux2023
    instanceType: ${SYSTEM_INSTANCE_TYPE}
    desiredCapacity: ${SYSTEM_NODES}
    minSize: ${SYSTEM_NODES}
    maxSize: $((SYSTEM_NODES + 1))
    volumeSize: 100
    volumeType: gp3
    privateNetworking: true
    disableIMDSv1: true
    disablePodIMDS: true
    labels: { ate.dev/pool: system }
    tags:
$(tags_yaml 6)
  - name: gvisor
    amiFamily: AmazonLinux2023
    instanceType: ${GVISOR_INSTANCE_TYPE}
    desiredCapacity: ${GVISOR_NODES}
    minSize: 1
    maxSize: $((GVISOR_NODES + 2))
    volumeSize: 200
    volumeType: gp3
    privateNetworking: true
    disableIMDSv1: true
    disablePodIMDS: true
    labels: { ate.dev/pool: gvisor }
    taints:
      - { key: ate.dev/sandboxClass, value: gvisor, effect: NoSchedule }
    tags:
$(tags_yaml 6)
  - name: microvm
    amiFamily: AmazonLinux2023
    instanceTypes: [${MICROVM_INSTANCE_TYPE}]
    desiredCapacity: ${MICROVM_NODES}
    minSize: 1
    maxSize: $((MICROVM_NODES + 2))
    privateNetworking: true
    # IMDS settings, disk and nested virtualization come from the template.
    launchTemplate: { id: ${lt_id}, version: "${lt_version}" }
    labels: { ate.dev/pool: microvm }
    taints:
      - { key: ate.dev/sandboxClass, value: microvm, effect: NoSchedule }
    tags:
$(tags_yaml 6)
EOF

phase "eksctl: control plane, add-ons, node groups"
if cluster_exists; then
  status=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.status --output text)
  [ "$status" = ACTIVE ] || die "cluster $CLUSTER_NAME is $status; wait for ACTIVE, or ./delete-cluster.sh and start again"
  info "cluster $CLUSTER_NAME already exists in $AWS_REGION"
  addons=$(aws_ eks list-addons --cluster-name "$CLUSTER_NAME" --query 'addons[]' --output text)
  for a in vpc-cni coredns kube-proxy eks-pod-identity-agent aws-ebs-csi-driver; do
    if ! grep -qw "$a" <<<"$addons"; then
      log "Add the missing add-ons"
      eksctl create addon -f "$WORK/cluster.yaml"
      break
    fi
  done
  ngs=$(aws_ eks list-nodegroups --cluster-name "$CLUSTER_NAME" --query 'nodegroups[]' --output text)
  for ng in system gvisor microvm; do
    if ! grep -qw "$ng" <<<"$ngs"; then
      log "Add the missing node groups (eksctl skips the ones that exist)"
      eksctl create nodegroup -f "$WORK/cluster.yaml"
      break
    fi
  done
else
  log "eksctl create cluster $CLUSTER_NAME ($KUBERNETES_VERSION, $AWS_REGION)"
  eksctl create cluster -f "$WORK/cluster.yaml" --kubeconfig "$KUBECONFIG"
fi

ensure_kubeconfig

phase "Tags, StorageClass, checks"
log "Tag the node group Auto Scaling groups (EKS does not copy node group tags to them)"
for ng in system gvisor microvm; do
  asg=$(aws_ eks describe-nodegroup --cluster-name "$CLUSTER_NAME" --nodegroup-name "$ng" \
    --query 'nodegroup.resources.autoScalingGroups[0].name' --output text)
  args=()
  while IFS= read -r kv; do
    args+=("ResourceId=$asg,ResourceType=auto-scaling-group,Key=${kv%%=*},Value=${kv#*=},PropagateAtLaunch=true")
  done < <(tags_kv)
  aws_ autoscaling create-or-update-tags --tags "${args[@]}"
  info "$asg"
done

log "Default StorageClass: encrypted gp3"
# EKS ships only a non-default gp2 class, so a PVC without storageClassName
# (the OpenClaw gateway's) would stay Pending.
{
  cat <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: gp3
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: ebs.csi.aws.com
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
reclaimPolicy: Delete
parameters:
  type: gp3
  encrypted: "true"
EOF
  i=0; while IFS= read -r kv; do i=$((i + 1)); printf '  tagSpecification_%d: "%s"\n' "$i" "$kv"; done < <(tags_kv)
} > "$WORK/storageclass.yaml"
# StorageClass parameters are immutable; replacing the class leaves existing
# volumes alone.
kubectl apply -f "$WORK/storageclass.yaml" 2>/dev/null || kubectl replace --force -f "$WORK/storageclass.yaml"

log "Node IMDS settings"
aws_ ec2 describe-instances \
  --filters "Name=tag:eks:cluster-name,Values=$CLUSTER_NAME" Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].[Tags[?Key==`eks:nodegroup-name`]|[0].Value,MetadataOptions.HttpTokens,MetadataOptions.HttpPutResponseHopLimit]' \
  --output text | sort | sed 's/^/   /'
kubectl get nodes -L ate.dev/pool
info "kubeconfig: $KUBECONFIG"
info "next: ./deploy.sh"
