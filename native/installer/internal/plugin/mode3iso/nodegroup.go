package mode3iso

// mode3-iso with NITRO_COMPUTE=node-group: the AWS Nitro sandbox class on
// the nitro node group, which `setup-aws create cluster` makes from a Nitro
// Sandbox AMI (NITRO_AMI_ID) with Nitro Enclaves on, managed or
// self-managed, on a cluster with or without EKS Auto Mode. Its nodes are
// labeled ate.dev/pool=nitro and eks.amazonaws.com/nitro-sandbox=true and
// tainted ate.dev/sandboxClass=nitro; their AMI registers the
// isolated-sandbox handler and advertises eks.amazonaws.com/nitro-sandbox.
//
//   - Deploy: RuntimeClass isolated-sandbox, the one EKS Auto Mode defines,
//     created and labeled as the installer's when the cluster has none, and
//     a refusal when it has another; nodes labeled ate.dev/pool=nitro whose
//     Nitro sandbox capacity covers counter-iso's worker Pods; atelet-iso
//     from atelet, on those nodes, with atelet's AL2023 volumes; the counter
//     image for their architecture; and counter-iso on those nodes. No
//     NodePool, no NodeClass, no credential provider ConfigMap.
//   - Verify: as on Auto Mode, with the workers on nodes labeled
//     ate.dev/pool=nitro.
//   - Remove (mode3iso.go's, with the NITRO_COMPUTE the plugin record holds,
//     else the run's): the pool and its counter image tag, atelet-iso, the
//     ConfigMap an auto-mode deploy may have left, and the RuntimeClass when
//     it carries the installer's labels and no Pod uses it. NodePool and
//     NodeClass ate-isolated it leaves, saying so: only a remove under
//     auto-mode deletes them.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
)

// NitroPool is the ate.dev/pool value (and the ate.dev/sandboxClass taint)
// of the nitro node group's nodes.
const NitroPool = nodegroups.Nitro

// nodeGroupMode reports NITRO_COMPUTE=node-group (Plugin.AutoModeOptional):
// the plugin then needs no Auto Mode.
func nodeGroupMode(o plugin.Options) bool { return o.NitroCompute == plugin.NitroNodeGroup }

// The RuntimeClass the Nitro sandbox runs its Pods under, as EKS Auto Mode
// defines it: the handler the Nitro Sandbox AMI registers, on nodes labeled
// eks.amazonaws.com/nitro-sandbox=true, and each Pod's sandbox counted
// against the node's eks.amazonaws.com/nitro-sandbox with 256Mi of memory
// overhead.
//
//	https://kubernetes.io/docs/concepts/containers/runtime-class/
var (
	wantNodeSelector = map[string]string{nodegroups.NitroSandboxLabel: "true"}
	wantOverhead     = map[string]string{"memory": "256Mi", nodegroups.NitroSandboxResource: "1"}
)

// managedBy is the label of a Kubernetes object the installer made, which
// Remove may delete.
const (
	managedByKey   = "app.kubernetes.io/managed-by"
	managedByValue = "substrate-eks"
)

// RuntimeClassManifest is RuntimeClass isolated-sandbox as the installer
// makes it on a cluster that has none, labeled as the installer's and
// deployment's (owner, <cluster>.<region>).
func RuntimeClassManifest(owner string) ([]byte, error) {
	return json.Marshal(obj{"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass",
		"metadata": obj{"name": RuntimeClass, "labels": obj{managedByKey: managedByValue, awsx.OwnerTagKey: owner}},
		"handler":  RuntimeClass,
		"overhead": obj{"podFixed": wantOverhead}, "scheduling": obj{"nodeSelector": wantNodeSelector}})
}

// runtimeClass is the part of a RuntimeClass (`kubectl get runtimeclass -o
// json`) the plugin reads.
type runtimeClass struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Handler  string `json:"handler"`
	Overhead struct {
		PodFixed map[string]string `json:"podFixed"`
	} `json:"overhead"`
	Scheduling struct {
		NodeSelector map[string]string `json:"nodeSelector"`
	} `json:"scheduling"`
}

// fmtMap is m as k=v pairs in key order, or "none".
func fmtMap(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	var kv []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		kv = append(kv, k+"="+m[k])
	}
	return strings.Join(kv, ",")
}

// RuntimeClassDiff is how an existing RuntimeClass isolated-sandbox differs
// from the one the Nitro sandbox needs (its handler, scheduling.nodeSelector
// and overhead.podFixed exactly), one entry per field; none when it is that
// one.
func RuntimeClassDiff(js []byte) ([]string, error) {
	var rc runtimeClass
	if err := json.Unmarshal(js, &rc); err != nil {
		return nil, fmt.Errorf("read RuntimeClass %s: %w", RuntimeClass, err)
	}
	var diff []string
	if rc.Handler != RuntimeClass {
		diff = append(diff, fmt.Sprintf("handler %q, not %q", rc.Handler, RuntimeClass))
	}
	if !maps.Equal(rc.Scheduling.NodeSelector, wantNodeSelector) {
		diff = append(diff, "scheduling.nodeSelector "+fmtMap(rc.Scheduling.NodeSelector)+", not "+fmtMap(wantNodeSelector))
	}
	if !maps.Equal(rc.Overhead.PodFixed, wantOverhead) {
		diff = append(diff, "overhead.podFixed "+fmtMap(rc.Overhead.PodFixed)+", not "+fmtMap(wantOverhead))
	}
	return diff, nil
}

// owned reports a RuntimeClass the installer made for this deployment.
func (rc runtimeClass) owned(o plugin.Options) bool {
	return rc.Metadata.Labels[managedByKey] == managedByValue && rc.Metadata.Labels[awsx.OwnerTagKey] == o.Cluster+"."+o.Region
}

// getRuntimeClass is RuntimeClass isolated-sandbox as JSON, or nil when the
// cluster has none. A failed read is an error, never "none".
func getRuntimeClass(ctx context.Context, w io.Writer, o plugin.Options) ([]byte, error) {
	var out bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &out, "get", "runtimeclass", RuntimeClass, "--ignore-not-found", "-o", "json"); err != nil {
		return nil, fmt.Errorf("read RuntimeClass %s: %w", RuntimeClass, err)
	}
	if strings.TrimSpace(out.String()) == "" {
		return nil, nil
	}
	return out.Bytes(), nil
}

// checkRuntimeClass reads RuntimeClass isolated-sandbox and decides,
// changing nothing: the one the Nitro sandbox needs is used (manifest is
// nil), none is to be made (manifest is its, which makeRuntimeClass
// creates), and any other is refused (useRuntimeClass).
func checkRuntimeClass(ctx context.Context, w io.Writer, o plugin.Options) (manifest []byte, err error) {
	have, err := getRuntimeClass(ctx, w, o)
	if err != nil {
		return nil, err
	}
	if have == nil {
		return RuntimeClassManifest(o.Cluster + "." + o.Region)
	}
	return nil, useRuntimeClass(w, have)
}

// useRuntimeClass takes have, a RuntimeClass isolated-sandbox on the
// cluster, as it is when it is the one the Nitro sandbox needs, and refuses
// any other, naming each difference: it is never changed.
func useRuntimeClass(w io.Writer, have []byte) error {
	diff, err := RuntimeClassDiff(have)
	if err != nil {
		return err
	}
	if len(diff) > 0 {
		return fmt.Errorf("RuntimeClass %s exists with %s; the Nitro sandbox needs handler %s, scheduling.nodeSelector %s and overhead.podFixed %s. The installer leaves it as it is. Other Pods can use it: check them first (kubectl get pods -A -o jsonpath='{range .items[?(@.spec.runtimeClassName==%q)]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}'), then change it to that spec or delete it, and deploy again",
			RuntimeClass, strings.Join(diff, "; "), RuntimeClass, fmtMap(wantNodeSelector), fmtMap(wantOverhead), RuntimeClass)
	}
	fmt.Fprintln(w, "RuntimeClass "+RuntimeClass+" -> handler "+RuntimeClass+", the Nitro sandbox's")
	return nil
}

// makeRuntimeClass creates m, checkRuntimeClass's manifest, when it is one.
// It creates and never applies, so one made since the check is not changed:
// on AlreadyExists (kubectl says so on stderr, which reaches w) it reads
// that one again and uses or refuses it as checkRuntimeClass would.
func makeRuntimeClass(ctx context.Context, w io.Writer, o plugin.Options, m []byte) error {
	if m == nil {
		return nil
	}
	fmt.Fprintln(w, "RuntimeClass "+RuntimeClass+": none on the cluster; making it (handler "+RuntimeClass+", on nodes "+fmtMap(wantNodeSelector)+")")
	var said bytes.Buffer
	err := plugin.Kubectl(ctx, io.MultiWriter(w, &said), o, m, nil, "create", "-f", "-")
	if err == nil || !strings.Contains(said.String(), "(AlreadyExists)") {
		return err
	}
	have, rerr := getRuntimeClass(ctx, w, o)
	if rerr != nil {
		return rerr
	}
	if have == nil {
		return fmt.Errorf("RuntimeClass %s was made by another while this deploy made it, and is gone again: deploy again: %w", RuntimeClass, err)
	}
	fmt.Fprintln(w, "RuntimeClass "+RuntimeClass+" was made since it was read; checking that one")
	return useRuntimeClass(w, have)
}

// removeRuntimeClass deletes RuntimeClass isolated-sandbox when the
// installer made it for this deployment and no Pod in the cluster runs
// under it; it says why it leaves one.
func removeRuntimeClass(ctx context.Context, w io.Writer, o plugin.Options) error {
	have, err := getRuntimeClass(ctx, w, o)
	if err != nil || have == nil {
		return err
	}
	var rc runtimeClass
	if err := json.Unmarshal(have, &rc); err != nil {
		return fmt.Errorf("read RuntimeClass %s: %w", RuntimeClass, err)
	}
	if !rc.owned(o) {
		fmt.Fprintln(w, "RuntimeClass "+RuntimeClass+" is not the installer's: left as it is")
		return nil
	}
	var pods bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &pods, "get", "pods", "--all-namespaces", "-o",
		`jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name} {.spec.runtimeClassName}{"\n"}{end}`); err != nil {
		return fmt.Errorf("list the Pods that use RuntimeClass %s: %w", RuntimeClass, err)
	}
	if users := podsUsing(pods.String(), RuntimeClass); len(users) > 0 {
		fmt.Fprintf(w, "RuntimeClass %s is left: Pods still run under it (%s)\n", RuntimeClass, strings.Join(users, ", "))
		return nil
	}
	fmt.Fprintln(w, "Deleting RuntimeClass "+RuntimeClass)
	return plugin.Kubectl(ctx, w, o, nil, nil, "delete", "runtimeclass", RuntimeClass, "--ignore-not-found")
}

// podsUsing are the namespace/name of the Pods in list (one
// "<namespace>/<name> <runtimeClassName>" line each) that run under rc.
func podsUsing(list, rc string) []string {
	var out []string
	for _, l := range strings.Split(list, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == rc {
			out = append(out, f[0])
		}
	}
	return out
}

// replicasRE finds the WorkerPool's worker count in counter-iso.yaml.
var replicasRE = regexp.MustCompile(`(?m)^  replicas: ([0-9]+)$`)

// poolReplicas is counter-iso's worker Pods: each takes one Nitro sandbox.
func poolReplicas() (int, error) {
	m := replicasRE.FindStringSubmatch(poolManifest)
	if m == nil {
		return 0, errors.New("counter-iso.yaml has no spec.replicas")
	}
	return strconv.Atoi(m[1])
}

// NitroNodes reads `kubectl get nodes -l ate.dev/pool=nitro -o json`: the
// Ready nodes labeled eks.amazonaws.com/nitro-sandbox=true (RuntimeClass
// isolated-sandbox's nodeSelector, so its Pods run nowhere else) that
// advertise eks.amazonaws.com/nitro-sandbox must have replicas of it
// between them, one per worker Pod, and one CPU architecture, which it
// returns, for the counter image. Too little capacity names each node's,
// the nodes without that label, and what NITRO_AMI_ID must be.
func NitroNodes(nodesJSON []byte, replicas int) (arch string, err error) {
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return "", fmt.Errorf("read the nodes labeled %s=%s: %w", PoolLabel, NitroPool, err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no node is labeled %s=%s: with NITRO_COMPUTE=node-group the Nitro sandbox runs on the nitro node group, which setup-aws create cluster makes (NODE_GROUPS with nitro, and NITRO_AMI_ID a Nitro Sandbox AMI)", PoolLabel, NitroPool)
	}
	total := int64(0)
	var archs, seen, unlabeled []string
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status == "True"
			}
		}
		q, has := n.Status.Allocatable[nodegroups.NitroSandboxResource]
		c, _ := nodegroups.SandboxCount(q)
		switch {
		case n.Metadata.Labels[nodegroups.NitroSandboxLabel] != "true":
			seen, unlabeled = append(seen, n.Metadata.Name+" not labeled "+nodegroups.NitroSandboxLabel+"=true"), append(unlabeled, n.Metadata.Name)
			continue
		case !ready:
			seen = append(seen, n.Metadata.Name+" not Ready")
			continue
		case !has:
			seen = append(seen, n.Metadata.Name+" none")
			continue
		}
		seen = append(seen, n.Metadata.Name+" "+q)
		if c < 1 {
			continue
		}
		total += c
		if a := n.Metadata.Labels[archKey]; !slices.Contains(archs, a) {
			archs = append(archs, a)
		}
	}
	if total < int64(replicas) {
		label := ""
		if len(unlabeled) > 0 {
			label = fmt.Sprintf(". RuntimeClass %s runs its Pods only on nodes labeled %s=true, and %s lack that label: a Nitro Sandbox AMI's nodeadm sets it, and nodes you label yourself need it",
				RuntimeClass, nodegroups.NitroSandboxLabel, strings.Join(unlabeled, ", "))
		}
		return "", fmt.Errorf("the nodes labeled %s=%s have %d allocatable %s in all (%s), and %s's %d worker Pods take one each: their AMI must be a Nitro Sandbox AMI (NITRO_AMI_ID; build one %s, see %s), with enough nodes (NITRO_NODES)%s",
			PoolLabel, NitroPool, total, nodegroups.NitroSandboxResource, strings.Join(seen, ", "), Iso.Name, replicas,
			nodegroups.NitroAMIBuild("<the cluster's version>"), nodegroups.NitroAMIDoc, label)
	}
	slices.Sort(archs)
	if len(archs) != 1 || archs[0] == "" {
		return "", fmt.Errorf("the nodes labeled %s=%s that run the Nitro sandbox have the %s values %q: mode3-iso builds its counter image for one architecture", PoolLabel, NitroPool, archKey, archs)
	}
	return archs[0], nil
}

// IsoNodeGroup is the counter pool on the nitro node group: counter-iso's
// Namespace, WorkerPool and template, its workers selecting and tolerating
// the nitro nodes (nodeGroupPool).
var IsoNodeGroup = func() mode3.Counter {
	c := Iso
	c.NodeLabels = map[string]string{PoolLabel: NitroPool}
	c.Pool = nodeGroupPool
	return c
}()

// nodeGroupPool is counter-iso.yaml with the nodeSelector and the
// toleration of the nitro node group's nodes; nodeGroupPoolErr says the
// manifest's lines were not found once each.
var nodeGroupPool, nodeGroupPoolErr = renderNodeGroupPool(poolManifest)

// renderNodeGroupPool replaces counter-iso's two isolated values, its
// nodeSelector and its toleration, with nitro. Each line must be there once,
// so a change to the manifest fails here instead of applying a pool that
// still selects the Auto Mode nodes.
func renderNodeGroupPool(pool string) (string, error) {
	for _, r := range [][2]string{
		{"      " + PoolLabel + ": " + PoolValue + "\n", "      " + PoolLabel + ": " + NitroPool + "\n"},
		{"    - key: " + TaintKey + "\n      operator: Equal\n      value: " + PoolValue + "\n",
			"    - key: " + TaintKey + "\n      operator: Equal\n      value: " + NitroPool + "\n"},
	} {
		if n := strings.Count(pool, r[0]); n != 1 {
			return "", fmt.Errorf("counter-iso.yaml has %d of %q, not one", n, r[0])
		}
		pool = strings.Replace(pool, r[0], r[1], 1)
	}
	return pool, nil
}

// iso is the counter pool of o's NITRO_COMPUTE.
func iso(o plugin.Options) (mode3.Counter, error) {
	if !nodeGroupMode(o) {
		return Iso, nil
	}
	return IsoNodeGroup, nodeGroupPoolErr
}

// deployNodeGroup is deploy on the nitro node group. It makes no AWS call:
// the nodes are the deployment's node group's, tagged by its launch
// template. It reads the RuntimeClass and the nodes before it creates or
// applies anything, so a refusal of either leaves the cluster as it was,
// and the plugin record too: plugin.Run records the plugin as pending only
// when Deploy calls o.BeforeWrite, after those checks. A failure after that
// leaves what it made, recorded as pending, so a removal of every plugin
// removes it, with NITRO_COMPUTE=node-group.
func deployNodeGroup(ctx context.Context, w io.Writer, o plugin.Options, tree string) error {
	pool, err := iso(o)
	if err != nil {
		return err
	}
	rc, err := checkRuntimeClass(ctx, w, o)
	if err != nil {
		return err
	}
	replicas, err := poolReplicas()
	if err != nil {
		return err
	}
	var nodes bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &nodes, "get", "nodes", "-l", PoolLabel+"="+NitroPool, "-o", "json"); err != nil {
		return err
	}
	arch, err := NitroNodes(nodes.Bytes(), replicas)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "The nodes labeled %s=%s run %d or more Nitro sandboxes, on %s\n", PoolLabel, NitroPool, replicas, arch)
	// The checks passed: plugin.Run records the plugin as pending now,
	// before the first write, so a refusal above leaves the record as it
	// was.
	if err := o.BeforeWrite(); err != nil {
		return err
	}
	if err := makeRuntimeClass(ctx, w, o, rc); err != nil {
		return err
	}

	fmt.Fprintln(w, AteletName+" on the nitro nodes")
	var atelet bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &atelet, "-n", "ate-system", "get", "ds", "atelet", "-o", "json"); err != nil {
		return err
	}
	ds, err := AteletIsoNodeGroup(atelet.Bytes())
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, ds, nil, "apply", "-f", "-"); err != nil {
		return err
	}
	return deployPool(ctx, w, o, tree, pool, arch)
}
