# Agent Substrate actors on AWS Nitro Sandbox: demo commands

Copy-paste commands for the demos on an install with mode 3 on isolated
sandboxes: each mode 3 worker Pod runs under RuntimeClass `isolated-sandbox`,
an AWS Nitro Sandbox with its own guest kernel, and the worker runs actors
with gVisor inside it.

Install first with the installer (`./native/install.sh`): EKS Auto Mode, the
counter demo, the `mode3` and `mode3-iso` plugins, and the Secrets Manager
credential provider (`credprovider-aws`) for section 3. See the
[README](../README.md#plugins). Run every block from a checkout of this
repository, in one shell.

## 0. Set up the shell

The installer keeps a kubeconfig for each cluster, and builds `kubectl-ate`
when it deploys the demo, both under `~/.cache/substrate-eks/`
(`~/Library/Caches/substrate-eks/` on macOS):

```bash
export AWS_PROFILE=<your profile> AWS_REGION=<the cluster's Region>
export CLUSTER_NAME=<the cluster's name>
export KUBECONFIG=~/.cache/substrate-eks/kubeconfig/$CLUSTER_NAME.$AWS_REGION.yaml
mkdir -p ~/.local/bin && export PATH=~/.local/bin:$PATH
ln -sf "$(ls -t ~/.cache/substrate-eks/bin/kubectl-ate-* | head -1)" ~/.local/bin/kubectl-ate
kubectl get nodes -L ate.dev/pool,node.kubernetes.io/instance-type
```

`kubectl ate` prints a warning about `--token-file` on every call. It acts as
the shared `ate-client` identity, which is fine for a demo.

## 1. The pieces EKS Auto Mode provides

```bash
kubectl get runtimeclass isolated-sandbox -o yaml | grep -E 'handler|nitro|memory'
kubectl get nodepool ate-isolated -o jsonpath='{.spec.template.metadata.labels}{"\n"}{.spec.template.spec.taints}{"\n"}'
kubectl -n ate-demo-counter-iso get workerpool counter-iso -o jsonpath='{.spec.networkTransport}{"\n"}'
```

The worker Pods, the RuntimeClass they run under, and the nodes Auto Mode
launched for them:

```bash
kubectl -n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network \
  -o custom-columns=POD:.metadata.name,RUNTIME:.spec.runtimeClassName,NODE:.spec.nodeName
kubectl get nodes -l ate.dev/pool=isolated -L node.kubernetes.io/instance-type,karpenter.sh/nodepool
```

## 2. Counter actor: suspend and resume on a Nitro sandbox

Create an actor and reach it through `kubectl ate proxy`. The first request
starts it from the template's golden snapshot.

```bash
NS=ate-demo-counter-iso A=demo-nitro
kubectl ate create actor $A -a $NS --template counter-iso
kubectl ate proxy actor $A -a $NS 8041:80 >/tmp/proxy-$A.log 2>&1 &
until curl -sf -m 10 http://127.0.0.1:8041/; do sleep 2; done
curl -s http://127.0.0.1:8041/
```

Which worker Pod it landed on, and that Pod's RuntimeClass:

```bash
POD=$(kubectl ate get actor $A -a $NS -o json | jq -r '.status.workerAssignment.workerPod | split("/") | last')
kubectl -n $NS get pod $POD -o jsonpath='{.metadata.name} {.spec.runtimeClassName} {.spec.nodeName}{"\n"}'
```

Suspend it: its memory, files and root filesystem go to the S3 snapshot
bucket, and the worker is free for another actor.

```bash
kubectl ate suspend actor $A -a $NS
kubectl ate get actor $A -a $NS
```

The next request resumes it, in about a second. Every counter continues where
it stopped:

```bash
time curl -s http://127.0.0.1:8041/
kubectl ate get actor $A -a $NS
```

Delete it. The worker tears the gVisor sandbox down inside its guest:

```bash
kill %1
kubectl ate delete actor $A -a $NS
kubectl -n $NS logs $POD --all-containers | grep TerminateWorkload | tail -1
```

## 3. Secrets Manager injection for an actor on a Nitro sandbox

The probe actor sends an HTTPS request with a placeholder header.
`atenet-egress` swaps in the real value from Secrets Manager on the way out,
so the actor never holds the secret.

The credential provider's probe pool runs gVisor workers. Add a pool of
`isolated-sandbox` workers next to it, and a `probe-iso` template on it in
both probe atespaces:

```bash
WORKER=$(kubectl -n ate-demo-counter-iso get workerpool counter-iso -o jsonpath='{.spec.workerImage}')
kubectl apply -f - <<EOF
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: credprovider-probe-iso
  namespace: ate-demo-credprovider
  labels: { workload: credprovider-probe-iso }
spec:
  replicas: 2
  workerImage: $WORKER
  sandboxClasses: [{ name: gvisor }]
  networkTransport: { runtimeClassName: isolated-sandbox }
  template:
    annotations:
      sandbox.amazonaws.com/ingress-bandwidth: disabled
      sandbox.amazonaws.com/egress-bandwidth: disabled
      sandbox.amazonaws.com/ingress-packets-per-second: disabled
      sandbox.amazonaws.com/egress-packets-per-second: disabled
      sandbox.amazonaws.com/overlay-disk-bandwidth: disabled
      sandbox.amazonaws.com/overlay-disk-iops: disabled
    nodeSelector: { ate.dev/pool: isolated }
    tolerations: [{ key: ate.dev/sandboxClass, operator: Equal, value: isolated, effect: NoSchedule }]
    resources:
      limits: { cpu: "1", memory: 2Gi }
      requests: { cpu: 250m, memory: 2Gi }
EOF
kubectl -n ate-demo-credprovider rollout status deploy/credprovider-probe-iso --timeout=15m
PROBE_IMAGE=$(kubectl ate get actor-template probe -a ate-demo-credprovider -o json | jq -r '(.spec // .).containers[0].image')
for AS in ate-demo-credprovider ate-demo-credprovider-denied; do
  sed -e "s|REPLACE_WITH_ATESPACE|$AS|" -e "s|REPLACE_WITH_PROBE_IMAGE|$PROBE_IMAGE|" \
    -e 's|^  name: probe$|  name: probe-iso|' -e 's|workload: credprovider-probe$|workload: credprovider-probe-iso|' \
    native/installer/internal/plugin/credprovideraws/probe-template.yaml | kubectl ate create actor-template -f -
done
```

Wait until `kubectl ate get actor-template probe-iso -a ate-demo-credprovider`
shows a golden tag. Then create the actor and an EgressPolicy that injects the
demo secret into the `Authorization` header for `postman-echo.com`:

```bash
NS=ate-demo-credprovider A=demo-secret
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
SECRET=ate/$NS/$CLUSTER_NAME-demo-token
URI="ate-secret://secretsmanager.amazonaws.com/accounts/$ACCOUNT/regions/$AWS_REGION/secrets/$SECRET/stages/AWSCURRENT"
kubectl ate create actor $A -a $NS --template probe-iso
kubectl ate create egress-policy $A -a $NS -f - <<EOF
rules:
- https:
    hostnames: ["postman-echo.com"]
    effects:
      replaceHeaders:
      - header: Authorization
        prefix: "Bearer "
        credentialUri: $URI
EOF
kubectl ate proxy actor $A -a $NS 8042:80 >/tmp/proxy-$A.log 2>&1 &
until curl -sf -m 10 http://127.0.0.1:8042/healthz; do sleep 2; done; echo
```

Send `Authorization: Bearer placeholder`. The echo endpoint shows what it
received; print only a hash of it, never the value, next to the hash of the
secret:

```bash
curl -sG http://127.0.0.1:8042/fetch --data-urlencode url=https://postman-echo.com/headers \
  --data-urlencode 'header=Authorization:Bearer placeholder' \
  | jq -r '.body | fromjson | .headers.authorization' | sed 's/^Bearer //' | tr -d '\n' | sha256sum
aws secretsmanager get-secret-value --secret-id $SECRET --query SecretString --output text | tr -d '\n' | sha256sum
```

The two hashes match. The actor's own environment holds no value:

```bash
curl -sG http://127.0.0.1:8042/readfile --data-urlencode path=/proc/self/environ | jq -r .content | tr '\0' '\n' | grep -ci bearer
```

It prints `0`. The worker Pod is a Nitro sandbox:

```bash
POD=$(kubectl ate get actor $A -a $NS -o json | jq -r '.status.workerAssignment.workerPod | split("/") | last')
kubectl -n $NS get pod $POD -o jsonpath='{.metadata.name} {.spec.runtimeClassName} {.spec.nodeName}{"\n"}'
```

Clean up the actor, then the templates and the pool this section added:

```bash
kill %1
kubectl ate delete egress-policy $A -a $NS
kubectl ate delete actor $A -a $NS
for AS in ate-demo-credprovider ate-demo-credprovider-denied; do
  kubectl ate delete actor-template probe-iso -a $AS
done
kubectl -n ate-demo-credprovider delete workerpool credprovider-probe-iso
```

## 4. Not supported yet: per-actor volumes

A Nitro sandbox Pod mounts EBS, EFS and S3 Files volumes through the usual
CSI drivers. Substrate's per-actor volumes (`volumes[].external`, the
`counter-efs` template of the `efs` plugin) are different: atelet mounts them on
the node when an actor starts. A mode 3 worker runs in its own guest, which
cannot see node mounts, so atelet refuses them for mode 3 workers, on Kata
and on Nitro sandboxes alike
([`validateNetworkSpec`](https://github.com/csantanapr/substrate/blob/9f36a0d7f2abca950f25fc7bd023752d3852504c/cmd/atelet/network_staging.go#L270)):

```text
golden actor crashed before its snapshot was taken: resume failed: atelet Run:
rpc error: code = InvalidArgument desc = network workers do not support node-mounted external CSI volumes
```

Actor state still survives suspend and resume through the S3 snapshot, as in
section 2.

## 5. Tear down

The installer's teardown removes the plugins first, then Substrate, then
every AWS resource with this deployment's owner tag:

```bash
make -C native teardown CLUSTER_NAME=$CLUSTER_NAME AWS_REGION=$AWS_REGION AWS_PROFILE=$AWS_PROFILE
```
