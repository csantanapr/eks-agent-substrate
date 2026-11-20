package cmd

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"gopkg.in/yaml.v3"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// The Kubernetes half of KARPENTER=on: Karpenter's chart, rendered once and
// embedded (karpenter-chart.yaml), then an EC2NodeClass and a NodePool per
// pool (the gvisor one; with KARPENTER_NODE_OVERLAY=on the microvm and nitro
// ones too, each with its NodeOverlay, karpenter_overlay.go), all applied
// through the Kubernetes API (kubeapi.go)
// with the caller's AWS credentials, as the wait for self-managed nodes
// reads it: never helm, kubectl or a kubeconfig.
//
// Every object carries app.kubernetes.io/managed-by=substrate-eks. Before
// applying anything the phase reads each one, and refuses to go on when one
// exists without the label: setup-aws changes only what it made.

//go:embed karpenter-chart.yaml
var karpenterChartYAML []byte

// The ownership label of every Kubernetes object setup-aws applies, the
// one the installer's plugins put on theirs.
const (
	managedByKey   = "app.kubernetes.io/managed-by"
	managedByValue = "substrate-eks"
)

// ownedObject reports an object that carries the ownership label.
func ownedObject(o kubeObject) bool { return o.label(managedByKey) == managedByValue }

// The bounds of the phase's waits; replaced in tests.
var (
	crdWaitTimeout        = 2 * time.Minute
	karpenterReadyTimeout = 10 * time.Minute
	poolReadyTimeout      = 5 * time.Minute
)

// terminationGracePeriod bounds a node's drain: a Pod still on the node
// after this time is deleted, so a drain cannot keep teardown waiting.
const terminationGracePeriod = "15m"

// karpenterChart is the embedded chart's objects for this cluster: the
// controller's settings filled in (CLUSTER_NAME, CLUSTER_ENDPOINT,
// INTERRUPTION_QUEUE, AWS_REGION), replicas controller Pods, with
// nodeOverlay the NodeOverlay feature gate on, and every object labeled
// managedByKey=managedByValue in place of Helm.
func karpenterChart(cluster, endpoint, queue, region string, replicas int, nodeOverlay bool) ([]kubeObject, error) {
	dec := yaml.NewDecoder(bytes.NewReader(karpenterChartYAML))
	var out []kubeObject
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("internal: karpenter-chart.yaml: %w", err)
		}
		if doc == nil {
			continue
		}
		// JSON is what the API takes, and what the objects are compared as.
		b, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("internal: karpenter-chart.yaml: %w", err)
		}
		var o kubeObject
		if err := json.Unmarshal(b, &o); err != nil {
			return nil, err
		}
		if _, err := o.kind(); err != nil {
			return nil, err
		}
		o.setLabel(managedByKey, managedByValue)
		if o.str("kind") == kindDeployment.Kind {
			env := map[string]string{"CLUSTER_NAME": cluster, "CLUSTER_ENDPOINT": endpoint, "INTERRUPTION_QUEUE": queue, "AWS_REGION": region}
			if err := fillController(o, env, replicas); err != nil {
				return nil, err
			}
			if nodeOverlay {
				if err := setNodeOverlayGate(o); err != nil {
					return nil, err
				}
			}
		}
		b, _ = json.Marshal(o)
		if bytes.Contains(b, []byte("${")) {
			return nil, fmt.Errorf("internal: karpenter-chart.yaml: %s %s keeps a ${...} value", o.str("kind"), o.name())
		}
		out = append(out, o)
	}
	return out, nil
}

// fillController sets the Deployment's replicas and the controller
// container's env values, each of which the chart must have.
func fillController(o kubeObject, env map[string]string, replicas int) error {
	spec, _ := o["spec"].(map[string]any)
	if spec == nil {
		return errors.New("internal: karpenter-chart.yaml: the Deployment has no spec")
	}
	spec["replicas"] = float64(replicas)
	containers, _ := o.field("spec", "template", "spec", "containers").([]any)
	set := map[string]bool{}
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		if cm["name"] != "controller" {
			continue
		}
		envs, _ := cm["env"].([]any)
		for _, e := range envs {
			em, _ := e.(map[string]any)
			name, _ := em["name"].(string)
			if v, ok := env[name]; ok {
				em["value"] = v
				set[name] = true
			}
		}
	}
	for name := range env {
		if !set[name] {
			return fmt.Errorf("internal: karpenter-chart.yaml: the controller has no env %s", name)
		}
	}
	return nil
}

// karpenterReplicas is how many controller Pods run: 2, or 1 when the
// system group has one node, since the Pods need different nodes.
func (r *clusterRun) karpenterReplicas() int { return min(2, max(1, r.cfg.SystemNodes)) }

// kubeArch is the kubernetes.io/arch value of a node of instance type t.
func (r *clusterRun) kubeArch(t string) string {
	if archOf(r.amiTypes[t]) == ec2types.ArchitectureValuesArm64 {
		return "arm64"
	}
	return "amd64"
}

// ec2NodeClass is pool p's EC2NodeClass: the AL2023 AMI family on the AMI
// p.Image names, the node role's instance profile, the cluster's private
// subnets and its security group by ID, IMDSv2 only with a hop limit of 1,
// an encrypted gp3 root volume of the group's size, nested virtualization
// on a microvm pool and Nitro Enclaves on a nitro pool
// (KARPENTER_NODE_OVERLAY=on), and the owner tag and TAGS on what Karpenter
// launches (instances, volumes, network interfaces, launch templates).
func (r *clusterRun) ec2NodeClass(p nodeGroupSpec) (kubeObject, error) {
	if p.Image == nil {
		return nil, fmt.Errorf("internal: the %s has no AMI", p.noun())
	}
	if len(r.private) == 0 {
		return nil, fmt.Errorf("the %s: cluster %s has no private subnets", p.noun(), r.e.names.Cluster)
	}
	sg, err := r.clusterSecurityGroup()
	if err != nil {
		return nil, fmt.Errorf("the %s: %w", p.noun(), err)
	}
	var subnets []any
	for _, s := range r.private {
		subnets = append(subnets, map[string]any{"id": s})
	}
	tags := map[string]any{}
	for k, v := range r.e.tags {
		tags[k] = v
	}
	spec := map[string]any{
		"amiFamily":                  "AL2023",
		"amiSelectorTerms":           []any{map[string]any{"id": p.Image.ID}},
		"instanceProfile":            r.instanceProfileName(),
		"subnetSelectorTerms":        subnets,
		"securityGroupSelectorTerms": []any{map[string]any{"id": sg}},
		"metadataOptions": map[string]any{"httpEndpoint": "enabled", "httpProtocolIPv6": "disabled",
			"httpPutResponseHopLimit": float64(1), "httpTokens": "required"},
		// rootVolume: Karpenter counts this volume as the node's
		// ephemeral storage, whatever the AMI names its root device.
		"blockDeviceMappings": []any{map[string]any{"deviceName": p.Image.RootDevice, "rootVolume": true, "ebs": map[string]any{
			"volumeSize": fmt.Sprintf("%dGi", p.VolumeGB), "volumeType": "gp3", "encrypted": true, "deleteOnTermination": true}}},
		"tags": tags,
	}
	if p.NestedVirt {
		spec["cpuOptions"] = map[string]any{"nestedVirtualization": "enabled"}
	}
	// The Nitro Sandbox AMI needs Nitro Enclaves on, as the nitro node
	// group's launch template has them.
	if p.Enclave {
		spec["enclaveOptions"] = map[string]any{"enabled": true}
	}
	return kubeObject{"apiVersion": kindEC2NodeClass.APIVersion, "kind": kindEC2NodeClass.Kind,
		"metadata": map[string]any{"name": p.Name, "labels": map[string]any{managedByKey: managedByValue}},
		"spec":     spec}, nil
}

// nodePool is pool p's NodePool: nodes of p's instance type, On-Demand,
// labeled ate.dev/pool=<pool> (and, for nitro, eks.amazonaws.com/
// nitro-sandbox=true, which RuntimeClass isolated-sandbox selects and the
// Nitro Sandbox AMI's nodeadm sets too: Karpenter must know it to launch a
// node for those Pods) and tainted as the node group would be, from
// the EC2NodeClass of the same name; at most p.Desired nodes (*_NODES) and
// their vCPUs; removed 5 minutes after they are empty, and never replaced
// on a schedule (expireAfter Never); a drain bounded by
// terminationGracePeriod.
func (r *clusterRun) nodePool(p nodeGroupSpec) (kubeObject, error) {
	var taints []any
	for _, t := range p.Taints {
		eff, err := kubeEffect(t.Effect)
		if err != nil {
			return nil, fmt.Errorf("the %s: %w", p.noun(), err)
		}
		taints = append(taints, map[string]any{"key": aws.ToString(t.Key), "value": aws.ToString(t.Value), "effect": eff})
	}
	labels := map[string]any{}
	for k, v := range p.Labels {
		labels[k] = v
	}
	// eks.amazonaws.com keys are allowed in a NodePool's template labels:
	// at runtime Karpenter v1.15.1 refuses only the karpenter.sh domain and
	// kubernetes.io/hostname (pkg/apis/v1/labels.go, IsRestrictedLabel),
	// and the AWS provider adds karpenter.k8s.aws; the NodePool CRD refuses
	// the same.
	for k, v := range p.NodeLabels {
		labels[k] = v
	}
	req := func(key string, values ...string) any {
		var vs []any
		for _, v := range values {
			vs = append(vs, v)
		}
		return map[string]any{"key": key, "operator": "In", "values": vs}
	}
	nodes := int64(p.Desired)
	return kubeObject{"apiVersion": kindNodePool.APIVersion, "kind": kindNodePool.Kind,
		"metadata": map[string]any{"name": p.Name, "labels": map[string]any{managedByKey: managedByValue}},
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": kindEC2NodeClass.Kind, "name": p.Name},
					"requirements": []any{
						req("node.kubernetes.io/instance-type", p.InstanceType),
						req("karpenter.sh/capacity-type", "on-demand"),
						req("kubernetes.io/arch", r.kubeArch(p.InstanceType)),
						req("kubernetes.io/os", "linux"),
					},
					"taints":                 taints,
					"expireAfter":            "Never",
					"terminationGracePeriod": terminationGracePeriod,
				},
			},
			"limits": map[string]any{"nodes": strconv.FormatInt(nodes, 10),
				"cpu": strconv.FormatInt(nodes*int64(r.vcpus[p.InstanceType]), 10)},
			"disruption": map[string]any{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "5m"},
		}}, nil
}

// karpenterChartObjects are the chart's objects for this cluster: its CRDs,
// and the rest, which the phase applies after the CRDs are Established.
func (r *clusterRun) karpenterChartObjects() (crds, chart []kubeObject, err error) {
	endpoint := "https://planned"
	if r.cluster != nil && aws.ToString(r.cluster.Endpoint) != "" {
		endpoint = aws.ToString(r.cluster.Endpoint)
	}
	all, err := karpenterChart(r.e.names.Cluster, endpoint, karpenterQueueName(r.e.names.Cluster), r.e.names.Region, r.karpenterReplicas(), r.nodeOverlay)
	if err != nil {
		return nil, nil, err
	}
	for _, o := range all {
		if o.str("kind") == kindCRD.Kind {
			crds = append(crds, o)
		} else {
			chart = append(chart, o)
		}
	}
	return crds, chart, nil
}

// poolObjects are each pool's EC2NodeClass and NodePool, on the pool's AMI
// (r.poolImages), and its NodeOverlay when it has one (r.overlays), in that
// order.
func (r *clusterRun) poolObjects() ([]kubeObject, error) {
	var out []kubeObject
	overlays := r.overlays()
	for _, p := range r.pools() {
		nc, err := r.ec2NodeClass(p)
		if err != nil {
			return nil, err
		}
		np, err := r.nodePool(p)
		if err != nil {
			return nil, err
		}
		out = append(out, nc, np)
		for _, o := range overlays {
			if o.pool == p.Name {
				out = append(out, o.object())
			}
		}
	}
	return out, nil
}

// clusterReachable reports a cluster this run can read the Kubernetes API
// of: it exists, is ACTIVE, and reports an endpoint.
func (r *clusterRun) clusterReachable() bool {
	return r.cluster != nil && r.cluster.Status == ekstypes.ClusterStatusActive && aws.ToString(r.cluster.Endpoint) != ""
}

// checkPoolImages picks, in preflight, the AMI of each NodePool: an
// existing EC2NodeClass keeps the AMI it names, as a node group keeps its
// own, so the pools' EC2NodeClasses are read first (one without the
// ownership label stops the run); a pool still to make gets NODE_AMI_ID or
// the recommended EKS optimized AL2023 AMI of its architecture (one SSM
// read per architecture), and the nitro pool NITRO_AMI_ID, its Nitro
// Sandbox AMI, as the nitro node group. Each AMI must fit its pool
// (imageFits).
//
// When the Kubernetes API cannot be read yet (a cluster that is not ACTIVE,
// a caller that has no access entry yet, an address outside the cluster's
// public access ranges), every pool gets its AMI as a new one does, and the
// Karpenter phase, which reads the EC2NodeClasses again, keeps the AMI of
// each one that exists (keepPoolImages).
func (r *clusterRun) checkPoolImages(ctx context.Context) error {
	pools := r.pools()
	if len(pools) == 0 {
		return nil
	}
	have := map[string]kubeObject{}
	if r.clusterReachable() {
		read := func() error {
			kube, err := r.c.Objects(ctx, r.cluster)
			if err != nil {
				return err
			}
			for _, p := range pools {
				nc, err := kube.Get(ctx, kindEC2NodeClass, "", p.Name)
				if err != nil {
					return err
				}
				if nc != nil {
					have[p.Name] = nc
				}
			}
			return nil
		}
		if err := read(); err != nil {
			slog.Warn("cannot read the EC2NodeClasses yet; the Karpenter phase reads them, and each one that exists keeps its AMI",
				"error", r.mask(err.Error()))
			clear(have)
		}
	}
	if err := r.keepPoolImages(ctx, have); err != nil {
		return err
	}
	var custom *nodeImage
	for _, p := range pools {
		if r.poolImages[p.Name] != nil {
			continue
		}
		var im *nodeImage
		var setting string
		var err error
		switch {
		case p.Name == nodegroups.Nitro:
			setting = "NITRO_AMI_ID"
			if im, err = describeImage(ctx, r.c.EC2, r.cfg.NitroAMIID, setting); err != nil {
				return err
			}
		case r.cfg.NodeAMIID != "":
			setting = "NODE_AMI_ID"
			if custom == nil {
				if custom = r.image; custom == nil {
					if custom, err = describeImage(ctx, r.c.EC2, r.cfg.NodeAMIID, setting); err != nil {
						return err
					}
				}
			}
			im = custom
		default:
			var name string
			if im, name, err = r.recommendedImage(ctx, r.kubeVersion(), archOf(r.amiTypes[p.InstanceType])); err != nil {
				return err
			}
			setting = "SSM parameter " + name
		}
		if err := r.imageFits(im, setting, p); err != nil {
			return err
		}
		r.poolImages[p.Name] = im
		slog.Info("NodePool AMI", "pool", p.Name, "ami", im.ID, "kept", false)
	}
	return nil
}

// keepPoolImages gives each pool whose EC2NodeClass exists (classes, by
// pool) the AMI that EC2NodeClass names, which must fit the pool. A pool
// keeps the AMI it was made with: one that runs another AMI than NODE_AMI_ID
// (NITRO_AMI_ID for the nitro pool) gets a warning, and its EC2NodeClass is
// never changed to another AMI.
func (r *clusterRun) keepPoolImages(ctx context.Context, classes map[string]kubeObject) error {
	for _, p := range r.pools() {
		nc := classes[p.Name]
		if nc == nil {
			continue
		}
		if !ownedObject(nc) {
			return foreignObject(kindEC2NodeClass, "", p.Name)
		}
		terms, _ := nc.field("spec", "amiSelectorTerms").([]any)
		id := ""
		if len(terms) == 1 {
			m, _ := terms[0].(map[string]any)
			id, _ = m["id"].(string)
		}
		if id == "" {
			return fmt.Errorf("EC2NodeClass %s names no single AMI ID; %s", p.Name, recreate)
		}
		if r.seeded[p.Name] && r.poolImages[p.Name] != nil && r.poolImages[p.Name].ID == id {
			continue
		}
		setting := "the AMI of EC2NodeClass " + p.Name
		im, err := describeImage(ctx, r.c.EC2, id, setting)
		if err != nil {
			return err
		}
		if err := r.imageFits(im, setting, p); err != nil {
			return err
		}
		// NITRO_AMI_ID for the nitro pool, NODE_AMI_ID for the others.
		want, key := r.cfg.NodeAMIID, imageSetting(p)
		if p.Name == nodegroups.Nitro {
			want = r.cfg.NitroAMIID
		}
		if want != "" && want != im.ID {
			slog.Warn("the NodePool keeps the AMI of its EC2NodeClass, not "+key+"; to change it, tear the deployment down (--remove-control-plane, then setup-aws teardown) and create it again",
				"pool", p.Name, "runs", im.ID, key, want)
		}
		r.poolImages[p.Name], r.seeded[p.Name] = im, true
		slog.Info("NodePool AMI", "pool", p.Name, "ami", im.ID, "kept", true)
	}
	return nil
}

// foreignObject is the refusal of an object setup-aws did not make.
func foreignObject(k kubeKind, ns, name string) error {
	id := name
	if ns != "" {
		id = ns + "/" + name
	}
	return fmt.Errorf("Kubernetes %s %s exists without the label %s=%s: setup-aws did not make it and leaves it alone; use another CLUSTER_NAME",
		k.Kind, id, managedByKey, managedByValue)
}

// describe names an object in messages: its kind and name.
func describe(o kubeObject) string {
	if ns := o.namespace(); ns != "" {
		return o.str("kind") + " " + ns + "/" + o.name()
	}
	return o.str("kind") + " " + o.name()
}

// --- phase 11: Karpenter and its NodePools ----------------------------------------

// ensureKarpenterKube applies Karpenter and its NodePools. It reads every
// object first and refuses one without the ownership label before it
// applies anything; then it applies the CRDs and waits until they are
// Established, the rest of the chart and waits until the controller is
// available, then the EC2NodeClasses and NodePools and waits until each is
// Ready. It does not wait for nodes: a pool starts with none. An existing
// EC2NodeClass keeps its AMI (keepPoolImages). An object that exists is
// applied only when a server-side dry run shows it would change, so a rerun
// writes nothing and --plan counts what would change; --plan therefore
// needs the right to patch those objects, though it stores nothing. The
// Kubernetes API's messages can name the caller's ARN, so the errors are
// masked.
func (r *clusterRun) ensureKarpenterKube(ctx context.Context) error {
	if err := r.applyKarpenter(ctx); err != nil {
		return errors.New(r.mask(err.Error()))
	}
	return nil
}

func (r *clusterRun) applyKarpenter(ctx context.Context) error {
	crds, chart, err := r.karpenterChartObjects()
	if err != nil {
		return err
	}
	if !r.clusterReachable() {
		if !r.plan() {
			return fmt.Errorf("cluster %s is not ACTIVE with an endpoint", r.e.names.Cluster)
		}
		if r.cluster != nil {
			// A cluster that exists may have them all already: say so,
			// rather than count them.
			slog.Info("plan: would read Karpenter's objects once the cluster is ACTIVE, and apply what is missing or differs",
				"cluster", r.e.names.Cluster, "status", string(r.cluster.Status))
			return nil
		}
		pools, err := r.poolObjects()
		if err != nil {
			return err
		}
		for _, o := range slices.Concat(crds, chart, pools) {
			if err := r.create("Kubernetes "+describe(o), nil); err != nil {
				return err
			}
		}
		slog.Info("plan: would wait for the CRDs, the Karpenter controller and the NodePools")
		return nil
	}
	kube, err := r.c.Objects(ctx, r.cluster)
	if err != nil {
		return err
	}
	// Every fixed-name object, the pools' too, is read before anything is
	// written.
	type ref struct {
		k        kubeKind
		ns, name string
	}
	var refs []ref
	for _, o := range slices.Concat(crds, chart) {
		k, _ := o.kind()
		refs = append(refs, ref{k, o.namespace(), o.name()})
	}
	for _, p := range r.pools() {
		refs = append(refs, ref{kindEC2NodeClass, "", p.Name}, ref{kindNodePool, "", p.Name})
	}
	for _, o := range r.overlays() {
		refs = append(refs, ref{kindNodeOverlay, "", o.pool})
	}
	live := map[string]kubeObject{}
	classes := map[string]kubeObject{}
	var refusals []string
	for _, x := range refs {
		got, err := kube.Get(ctx, x.k, x.ns, x.name)
		if err != nil {
			return err
		}
		if got == nil {
			continue
		}
		if !ownedObject(got) {
			refusals = append(refusals, foreignObject(x.k, x.ns, x.name).Error())
			continue
		}
		live[x.k.Kind+" "+x.ns+"/"+x.name] = got
		if x.k == kindEC2NodeClass {
			classes[x.name] = got
		}
	}
	if len(refusals) > 0 {
		return fmt.Errorf("Karpenter is not installed:\n  %s", strings.Join(refusals, "\n  "))
	}
	if err := r.keepPoolImages(ctx, classes); err != nil {
		return err
	}
	pools, err := r.poolObjects()
	if err != nil {
		return err
	}
	apply := func(objs []kubeObject) error {
		for _, o := range objs {
			k, _ := o.kind()
			what := "Kubernetes " + describe(o)
			do := func() error {
				_, err := kube.Apply(ctx, o, false)
				return err
			}
			if live[k.Kind+" "+o.namespace()+"/"+o.name()] == nil {
				if err := r.create(what, do); err != nil {
					return err
				}
				continue
			}
			// Read again right before the dry run: a controller may have
			// changed the object since the phase began.
			have, err := kube.Get(ctx, k, o.namespace(), o.name())
			if err != nil {
				return err
			}
			if have != nil && !ownedObject(have) {
				return foreignObject(k, o.namespace(), o.name())
			}
			if have == nil {
				if err := r.create(what, do); err != nil {
					return err
				}
				continue
			}
			dry, err := kube.Apply(ctx, o, true)
			if err != nil {
				return err
			}
			if sameObject(have, dry) {
				continue
			}
			if err := r.change("apply "+what, do); err != nil {
				return err
			}
		}
		return nil
	}
	if err := apply(crds); err != nil {
		return err
	}
	if !r.plan() {
		for _, o := range crds {
			if err := waitKube(ctx, kube, kindCRD, "", o.name(), crdWaitTimeout, crdEstablished); err != nil {
				return err
			}
		}
	}
	if err := apply(chart); err != nil {
		return err
	}
	if !r.plan() {
		if err := waitKube(ctx, kube, kindDeployment, karpenterNamespace, "karpenter", karpenterReadyTimeout, deploymentAvailable); err != nil {
			if why := controllerWithoutCredentials(ctx, kube); why != "" {
				return fmt.Errorf("%w; %s", err, why)
			}
			return fmt.Errorf("%w; the controller logs say why: kubectl -n %s logs deployment/karpenter", err, karpenterNamespace)
		}
	}
	if err := apply(pools); err != nil {
		return err
	}
	if r.plan() {
		slog.Info("plan: would wait for the CRDs, the Karpenter controller and the NodePools")
		return nil
	}
	for _, o := range pools {
		k, _ := o.kind()
		if err := waitKube(ctx, kube, k, "", o.name(), poolReadyTimeout, ready); err != nil {
			return err
		}
	}
	if n := len(r.overlays()); n > 0 {
		slog.Info("Karpenter is ready", "pools", len(r.pools()), "nodeOverlays", n)
		return nil
	}
	slog.Info("Karpenter is ready", "pools", len(r.pools()))
	return nil
}

// The controller Pods' selector (the chart's), and the variable EKS Pod
// Identity's webhook puts in a Pod it gives credentials to.
const (
	karpenterPodSelector   = "app.kubernetes.io/name=karpenter,app.kubernetes.io/instance=karpenter"
	podIdentityCredentials = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
)

// controllerWithoutCredentials says why the controller is not available
// when one of its Pods has no EKS Pod Identity credentials: EKS gives them
// only to a Pod made after the association, and a restart keeps the Pod.
// It is "" when every Pod has them, or the Pods cannot be read.
func controllerWithoutCredentials(ctx context.Context, kube kubeObjects) string {
	pods, err := kube.List(ctx, kindPod, karpenterNamespace, karpenterPodSelector)
	if err != nil {
		return ""
	}
	for _, p := range pods {
		containers, _ := p.field("spec", "containers").([]any)
		for _, c := range containers {
			cm, _ := c.(map[string]any)
			if cm["name"] != "controller" {
				continue
			}
			envs, _ := cm["env"].([]any)
			found := false
			for _, e := range envs {
				em, _ := e.(map[string]any)
				found = found || em["name"] == podIdentityCredentials
			}
			if !found {
				return fmt.Sprintf("controller Pod %s has no EKS Pod Identity credentials (no %s): it was made before the association of %s/%s took effect. "+
					"Delete the controller Pods so the Deployment makes new ones (kubectl -n %s delete pod -l %s), then run create cluster again",
					p.name(), podIdentityCredentials, karpenterNamespace, karpenterServiceAccount, karpenterNamespace, karpenterPodSelector)
			}
		}
	}
	return ""
}

// sameObject reports that applying gave what is there: the two objects are
// equal but for the fields every write changes, and the status, which the
// controllers write (a NodePool's node count, an EC2NodeClass's subnets).
func sameObject(a, b kubeObject) bool {
	strip := func(o kubeObject) any {
		raw, _ := json.Marshal(o)
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		delete(c, "status")
		if md, ok := c["metadata"].(map[string]any); ok {
			delete(md, "managedFields")
			delete(md, "resourceVersion")
			delete(md, "generation")
		}
		return c
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// A wait's test of an object: done, or why not.
func crdEstablished(o kubeObject) (bool, string) {
	c, ok := o.condition("Established")
	return ok && c.Status == "True", conditionWhy(o, "Established")
}

func deploymentAvailable(o kubeObject) (bool, string) {
	num := func(path ...string) float64 { f, _ := o.field(path...).(float64); return f }
	want := num("spec", "replicas")
	done := num("status", "observedGeneration") >= num("metadata", "generation") && num("status", "availableReplicas") >= want && want > 0
	return done, fmt.Sprintf("%v of %v Pods available; %s", num("status", "availableReplicas"), want, conditionWhy(o, "Available"))
}

func ready(o kubeObject) (bool, string) {
	c, ok := o.condition("Ready")
	return ok && c.Status == "True", conditionWhy(o, "Ready")
}

// conditionWhy says why condition t is not True: its own reason and
// message, else those of a condition that is False.
func conditionWhy(o kubeObject, t string) string {
	c, ok := o.condition(t)
	if !ok {
		return "no " + t + " condition yet"
	}
	if c.Status != "True" && c.Message == "" {
		for _, x := range o.conditions() {
			if x.Status == "False" && x.Message != "" {
				return fmt.Sprintf("%s is %s; %s: %s", t, c.Status, x.Type, x.Message)
			}
		}
	}
	return fmt.Sprintf("%s is %s (%s: %s)", t, c.Status, c.Reason, c.Message)
}

// waitKube reads object name of kind k until done says so, or timeout
// passes; then the error says why it is not done. A blip of the Kubernetes
// API (no answer, 429, a 5xx) is "not yet"; any other failed read stops it.
func waitKube(ctx context.Context, kube kubeObjects, k kubeKind, ns, name string, timeout time.Duration, done func(kubeObject) (bool, string)) error {
	why := "it does not exist yet"
	var lastErr error
	id := k.Kind + " " + name
	err := waitFor(ctx, id+" to be ready", timeout, func() (bool, error) {
		o, err := kube.Get(ctx, k, ns, name)
		if kubeTransient(err) {
			lastErr = err
			return false, nil
		}
		if err != nil {
			return false, err
		}
		lastErr = nil
		if o == nil {
			why = "it does not exist"
			return false, nil
		}
		ok, w := done(o)
		why = w
		return ok, nil
	})
	switch {
	case err == nil:
		return nil
	case lastErr != nil:
		return fmt.Errorf("%w; the last read failed: %v", err, lastErr)
	case strings.HasPrefix(err.Error(), "timed out"):
		return fmt.Errorf("%w: %s", err, why)
	}
	return err
}
