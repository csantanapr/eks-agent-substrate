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
#
# The first of the four entry points: create-cluster.sh, deploy.sh, clean.sh,
# delete-cluster.sh. It needs only config.env and AWS credentials, and it
# writes .work/kubeconfig for the others. gVisor runs a pod's containers on a
# user-space kernel. A microVM gives them their own guest kernel under a
# hypervisor (Kata Containers with Cloud Hypervisor), which needs /dev/kvm.
#
# Learn more:
#   https://docs.aws.amazon.com/eks/latest/eksctl/creating-and-managing-clusters.html
#   https://docs.aws.amazon.com/eks/latest/userguide/managed-node-groups.html
#   https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
#   https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-options.html
set -euo pipefail
. "$(dirname "$0")/lib/common.sh"
need eksctl; need aws; need kubectl; need envsubst

# Nested virtualization passes Intel VT-x through to a regular (not bare
# metal) instance, so Kata can start microVMs there. Only some families have it.
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
# The launch template holds the EC2 settings of the microvm nodes: CpuOptions
# for nested virtualization, the IMDS options and the encrypted root disk.
# HttpPutResponseHopLimit 1 keeps the IMDSv2 token reply from crossing the
# extra network hop into a pod's own network namespace.
#   https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html
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

# The eksctl config, cluster.yaml: the API endpoint (public, limited to
# EKS_PUBLIC_ACCESS_CIDRS, and private inside the VPC), the add-ons, and one
# managed node group per role. The vpc-cni add-on enforces NetworkPolicy only
# with enableNetworkPolicy on. The gvisor and microvm groups carry a
# NoSchedule taint, so only pods that tolerate it (the worker pods) land there.
#   https://docs.aws.amazon.com/eks/latest/userguide/cluster-endpoint.html
#   https://docs.aws.amazon.com/eks/latest/userguide/cni-network-policy.html
#   https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
cidrs=""
for c in ${EKS_PUBLIC_ACCESS_CIDRS:-0.0.0.0/0}; do cidrs+="\"$c\", "; done
# The values cluster.yaml needs beyond config.env. A tag placeholder sits
# where the first tag line goes, so that line loses tags_yaml's indent.
# shellcheck disable=SC2034  # render_yaml reads these by name
{
  PUBLIC_ACCESS_CIDRS="${cidrs%, }"
  CLUSTER_TAGS="$(tags_yaml 4 | sed '1s/^ *//')"
  NODE_TAGS="$(tags_yaml 6 | sed '1s/^ *//')"
  SYSTEM_MAX_NODES=$((SYSTEM_NODES + 1))
  GVISOR_MAX_NODES=$((GVISOR_NODES + 2))
  MICROVM_MAX_NODES=$((MICROVM_NODES + 2))
}
render_yaml "$ROOT/cluster.yaml" CLUSTER_NAME AWS_REGION KUBERNETES_VERSION CLUSTER_TAGS PUBLIC_ACCESS_CIDRS \
  SYSTEM_INSTANCE_TYPE SYSTEM_NODES SYSTEM_MAX_NODES GVISOR_INSTANCE_TYPE GVISOR_NODES GVISOR_MAX_NODES \
  MICROVM_INSTANCE_TYPE MICROVM_NODES MICROVM_MAX_NODES NODE_TAGS lt_id lt_version > "$WORK/cluster.yaml"

# eksctl turns the config into CloudFormation stacks: one for the cluster and
# its VPC, one per node group.
phase "eksctl: control plane, add-ons, node groups"
if cluster_exists; then
  status=$(aws_ eks describe-cluster --name "$CLUSTER_NAME" --query cluster.status --output text)
  [ "$status" = ACTIVE ] || die "cluster $CLUSTER_NAME is $status; wait for ACTIVE, or ./delete-cluster.sh and start again"
  info "cluster $CLUSTER_NAME already exists in $AWS_REGION"
  addons=$(aws_ eks list-addons --cluster-name "$CLUSTER_NAME" --query 'addons[]' --output text)
  for a in vpc-cni coredns kube-proxy eks-pod-identity-agent aws-ebs-csi-driver; do
    if ! grep -qw "$a" <<<"$addons"; then
      # Nodes need vpc-cni and kube-proxy to become Ready. coredns and the EBS
      # CSI driver stay DEGRADED until nodes exist, so a failure here is only
      # fatal after the node groups are up (checked below).
      log "Add the missing add-ons"
      eksctl create addon -f "$WORK/cluster.yaml" || warn "some add-ons are not healthy yet; checking again after the node groups"
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
  addons=$(aws_ eks list-addons --cluster-name "$CLUSTER_NAME" --query 'addons[]' --output text)
  for a in vpc-cni coredns kube-proxy eks-pod-identity-agent aws-ebs-csi-driver; do
    if ! grep -qw "$a" <<<"$addons"; then
      log "Add the add-ons still missing, now that nodes exist"
      eksctl create addon -f "$WORK/cluster.yaml"
      break
    fi
  done
  log "Wait for every add-on to be ACTIVE"
  for a in vpc-cni coredns kube-proxy eks-pod-identity-agent aws-ebs-csi-driver; do
    aws_ eks wait addon-active --cluster-name "$CLUSTER_NAME" --addon-name "$a" \
      || die "add-on $a is not ACTIVE; see: aws eks describe-addon --cluster-name $CLUSTER_NAME --addon-name $a"
    info "$a"
  done
else
  # A failed create leaves its stack in ROLLBACK_COMPLETE, empty but holding
  # the name, so eksctl cannot create the cluster again until it is deleted.
  stack="eksctl-$CLUSTER_NAME-cluster"
  if [ "$(aws_ cloudformation describe-stacks --stack-name "$stack" --query 'Stacks[0].StackStatus' --output text 2>/dev/null)" = ROLLBACK_COMPLETE ]; then
    log "Delete the stack a failed create left ($stack, ROLLBACK_COMPLETE)"
    aws_ cloudformation delete-stack --stack-name "$stack"
    aws_ cloudformation wait stack-delete-complete --stack-name "$stack"
  fi
  log "eksctl create cluster $CLUSTER_NAME ($KUBERNETES_VERSION, $AWS_REGION)"
  eksctl create cluster -f "$WORK/cluster.yaml" --kubeconfig "$KUBECONFIG"
fi

ensure_kubeconfig

phase "Tags, StorageClass, checks"
log "Tag the node group Auto Scaling groups (EKS does not copy node group tags to them)"
#   https://docs.aws.amazon.com/autoscaling/ec2/userguide/ec2-auto-scaling-tagging.html
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
# (the OpenClaw gateway's) would stay Pending. A StorageClass tells a CSI
# driver, here the EBS CSI driver, how to make the volume behind a PVC. The
# class is storageclass-gp3.yaml, plus one tag parameter per tag.
#   https://kubernetes.io/docs/concepts/storage/storage-classes/
#   https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html
{
  render_yaml "$ROOT/storageclass-gp3.yaml"
  i=0; while IFS= read -r kv; do i=$((i + 1)); printf '  tagSpecification_%d: "%s"\n' "$i" "$kv"; done < <(tags_kv)
} > "$WORK/storageclass.yaml"
# StorageClass parameters are immutable; replacing the class leaves existing
# volumes alone.
kubectl apply -f "$WORK/storageclass.yaml" 2>/dev/null || kubectl replace --force -f "$WORK/storageclass.yaml"

# Expect "required" (IMDSv2 only) and a hop limit of 1 on every node.
log "Node IMDS settings"
aws_ ec2 describe-instances \
  --filters "Name=tag:eks:cluster-name,Values=$CLUSTER_NAME" Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].[Tags[?Key==`eks:nodegroup-name`]|[0].Value,MetadataOptions.HttpTokens,MetadataOptions.HttpPutResponseHopLimit]' \
  --output text | sort | sed 's/^/   /'
kubectl get nodes -L ate.dev/pool
info "kubeconfig: $KUBECONFIG"
info "next: ./deploy.sh"
