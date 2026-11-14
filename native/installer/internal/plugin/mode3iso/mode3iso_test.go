package mode3iso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
)

// testGeneralPurpose is the shape of Auto Mode's built-in NodePool.
const testGeneralPurpose = `{"apiVersion":"karpenter.sh/v1","kind":"NodePool",
 "metadata":{"name":"general-purpose","uid":"u1","resourceVersion":"9","labels":{"app.kubernetes.io/managed-by":"eks"}},
 "spec":{"disruption":{"budgets":[{"nodes":"10%"}],"consolidateAfter":"30s","consolidationPolicy":"WhenEmptyOrUnderutilized"},
  "template":{"metadata":{},"spec":{"expireAfter":"336h",
   "nodeClassRef":{"group":"eks.amazonaws.com","kind":"NodeClass","name":"default"},
   "requirements":[{"key":"karpenter.sh/capacity-type","operator":"In","values":["on-demand"]},
    {"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}},
 "status":{"conditions":[]}}`

// testAtelet is the shape of the atelet DaemonSet deploy/eks makes.
const testAtelet = `{"apiVersion":"apps/v1","kind":"DaemonSet",
 "metadata":{"name":"atelet","namespace":"ate-system","uid":"u2","generation":3,"labels":{"app":"atelet"},
  "annotations":{"deprecated.daemonset.template.generation":"3"}},
 "spec":{"selector":{"matchLabels":{"app":"atelet"}},
  "template":{"metadata":{"labels":{"app":"atelet"}},"spec":{
   "affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"ate.dev/pool","operator":"In","values":["gvisor","microvm"]}]}]}}},
   "containers":[{"name":"atelet","image":"ghcr.io/x/atelet@sha256:1","volumeMounts":[
     {"name":"image-credential-provider-config","mountPath":"/etc/eks/image-credential-provider/config.json"},
     {"name":"image-credential-provider-bin","mountPath":"/etc/eks/image-credential-provider"}]}],
   "volumes":[{"name":"image-credential-provider-bin","hostPath":{"path":"/etc/eks/image-credential-provider","type":"Directory"}},
    {"name":"image-credential-provider-config","hostPath":{"path":"/etc/eks/image-credential-provider/config.json","type":"File"}},
    {"name":"state","emptyDir":{}}]}}},
 "status":{"numberReady":2}}`

func get(t *testing.T, m obj, keys ...string) any {
	t.Helper()
	var v any = m
	for _, k := range keys {
		mm, ok := v.(obj)
		if !ok {
			t.Fatalf("%s: not an object at %q", strings.Join(keys, "."), k)
		}
		v = mm[k]
	}
	return v
}

func decode(t *testing.T, b []byte) obj {
	t.Helper()
	var m obj
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Both directions: the copy keeps general-purpose's node class and
// requirements, and adds the label, the taint and the slower consolidation;
// anything but a NodePool is refused.
func TestNodePool(t *testing.T) {
	b, src, err := NodePool([]byte(testGeneralPurpose))
	if err != nil || src != "default" {
		t.Fatalf("NodePool: source class %q, %v", src, err)
	}
	np := decode(t, b)
	if get(t, np, "metadata", "name") != "ate-isolated" || get(t, np, "metadata", "uid") != nil || np["status"] != nil {
		t.Errorf("metadata/status = %v %v", np["metadata"], np["status"])
	}
	if get(t, np, "spec", "template", "metadata", "labels", "ate.dev/pool") != "isolated" {
		t.Error("no ate.dev/pool=isolated label")
	}
	taints, _ := json.Marshal(get(t, np, "spec", "template", "spec", "taints"))
	if string(taints) != `[{"effect":"NoSchedule","key":"ate.dev/sandboxClass","value":"isolated"}]` {
		t.Errorf("taints = %s", taints)
	}
	if get(t, np, "spec", "disruption", "consolidationPolicy") != "WhenEmpty" || get(t, np, "spec", "disruption", "consolidateAfter") != "300s" {
		t.Errorf("disruption = %v", get(t, np, "spec", "disruption"))
	}
	if get(t, np, "spec", "template", "spec", "nodeClassRef", "name") != "ate-isolated" ||
		get(t, np, "spec", "template", "spec", "nodeClassRef", "group") != "eks.amazonaws.com" || !strings.Contains(string(b), `"kubernetes.io/arch"`) {
		t.Errorf("node class or requirements wrong:\n%s", b)
	}
	noClass := strings.Replace(testGeneralPurpose, `"name":"default"`, `"name":""`, 1)
	for _, bad := range []string{`{"kind":"Deployment","spec":{}}`, `{"kind":"NodePool"}`, `not json`, noClass} {
		if _, _, err := NodePool([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// NodeClass statuses as Auto Mode writes them: Ready, and the validation
// failure a cluster role without auto-mode-node-tags gives a tagged one.
const (
	testReady   = `{"status":{"conditions":[{"type":"SubnetsReady","status":"True"},{"type":"Ready","status":"True"}]}}`
	testInvalid = `{"status":{"conditions":[{"type":"SubnetsReady","status":"True"},
  {"type":"ValidationSucceeded","status":"False","reason":"CreateLaunchTemplateAuthCheckFailed","message":"Controller isn't authorized to call ec2:CreateLaunchTemplate"},
  {"type":"Ready","status":"False","reason":"UnhealthyDependents","message":"ValidationSucceeded=False"}]}}`
)

func TestNodeClassReady(t *testing.T) {
	if ok, _, err := NodeClassReady([]byte(testReady)); !ok || err != nil {
		t.Errorf("Ready: %v %v", ok, err)
	}
	ok, why, err := NodeClassReady([]byte(testInvalid))
	if ok || err != nil || why != "ValidationSucceeded: CreateLaunchTemplateAuthCheckFailed: Controller isn't authorized to call ec2:CreateLaunchTemplate" {
		t.Errorf("invalid: %v %q %v", ok, why, err)
	}
	if ok, _, _ := NodeClassReady([]byte(`{}`)); ok {
		t.Error("no status counted as Ready")
	}
}

// testNodeClass is the shape of Auto Mode's default NodeClass.
const testNodeClass = `{"apiVersion":"eks.amazonaws.com/v1","kind":"NodeClass",
 "metadata":{"name":"default","finalizers":["eks.amazonaws.com/termination"],"annotations":{"eks.amazonaws.com/nodeclass-hash":"1"}},
 "spec":{"role":"c1-eks-auto-node","subnetSelectorTerms":[{"id":"subnet-1"}],"securityGroupSelectorTerms":[{"id":"sg-1"}],
  "tags":{"team":"a","auto-delete":"yes"}},
 "status":{"conditions":[]}}`

// Both directions: the copy keeps the role, subnets and security groups and
// adds the owner tags over the source's; anything but a NodeClass is refused.
func TestNodeClass(t *testing.T) {
	b, err := NodeClass([]byte(testNodeClass), map[string]string{"agent-substrate-on-eks/deployment": "c1.us-east-1", "auto-delete": "no"})
	if err != nil {
		t.Fatal(err)
	}
	nc := decode(t, b)
	meta, _ := json.Marshal(nc["metadata"])
	tags, _ := json.Marshal(get(t, nc, "spec", "tags"))
	if string(meta) != `{"name":"ate-isolated"}` || nc["status"] != nil ||
		string(tags) != `{"agent-substrate-on-eks/deployment":"c1.us-east-1","auto-delete":"no","team":"a"}` {
		t.Errorf("metadata %s, tags %s, status %v", meta, tags, nc["status"])
	}
	if get(t, nc, "spec", "role") != "c1-eks-auto-node" || !strings.Contains(string(b), `"subnet-1"`) || !strings.Contains(string(b), `"sg-1"`) {
		t.Errorf("role, subnets or security groups lost:\n%s", b)
	}
	for _, bad := range []string{testGeneralPurpose, `{"kind":"NodeClass"}`, `{"kind":"NodeClass","spec":{"tags":"x"}}`} {
		if _, err := NodeClass([]byte(bad), nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Both directions: the copy is atelet on the isolated nodes with the
// Bottlerocket provider path and the ConfigMap's config; a DaemonSet
// without the provider volumes is refused.
func TestAteletIso(t *testing.T) {
	b, err := AteletIso([]byte(testAtelet))
	if err != nil {
		t.Fatal(err)
	}
	d := decode(t, b)
	meta, _ := json.Marshal(d["metadata"])
	if string(meta) != `{"labels":{"app":"atelet"},"name":"atelet-iso","namespace":"ate-system"}` || d["status"] != nil {
		t.Errorf("metadata = %s, status = %v", meta, d["status"])
	}
	if get(t, d, "spec", "selector", "matchLabels", "ate.dev/atelet-variant") != "iso" ||
		get(t, d, "spec", "template", "metadata", "labels", "ate.dev/atelet-variant") != "iso" ||
		get(t, d, "spec", "template", "metadata", "labels", "app") != "atelet" {
		t.Errorf("labels = %v", get(t, d, "spec", "template", "metadata"))
	}
	aff, _ := json.Marshal(get(t, d, "spec", "template", "spec", "affinity"))
	if !strings.Contains(string(aff), `"values":["isolated"]`) || strings.Contains(string(aff), "gvisor") {
		t.Errorf("affinity = %s", aff)
	}
	vols, _ := json.Marshal(get(t, d, "spec", "template", "spec", "volumes"))
	for _, want := range []string{`{"hostPath":{"path":"/usr/libexec/kubernetes/kubelet/plugins","type":"Directory"},"name":"image-credential-provider-bin"}`,
		`{"configMap":{"name":"atelet-iso-credential-provider"},"name":"image-credential-provider-config"}`, `{"emptyDir":{},"name":"state"}`} {
		if !strings.Contains(string(vols), want) {
			t.Errorf("volumes lack %s:\n%s", want, vols)
		}
	}
	mounts, _ := json.Marshal(get(t, d, "spec", "template", "spec", "containers"))
	if !strings.Contains(string(mounts), `"name":"image-credential-provider-config","subPath":"config.yaml"`) {
		t.Errorf("containers = %s", mounts)
	}
	noVols := strings.Replace(testAtelet, `"image-credential-provider-config","hostPath"`, `"other","hostPath"`, 1)
	if _, err := AteletIso([]byte(noVols)); err == nil || !strings.Contains(err.Error(), "image-credential-provider-config") {
		t.Errorf("no config volume: %v", err)
	}
	if _, err := AteletIso([]byte(testGeneralPurpose)); err == nil {
		t.Error("a NodePool was accepted as atelet")
	}
}

// fake is plugin.Exec and mode3.Demo for the plugin's tests.
type fake struct {
	t     *testing.T
	log   []string
	out   map[string][]string // kubectl argv prefix -> successive answers
	stdin map[string]string
	// applies is the stdin of every `kubectl apply -f -`, in order.
	applies []string
	tmpls   map[string]string
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, out: map[string][]string{}, stdin: map[string]string{}, tmpls: map[string]string{"counter-mode3": "ecr/c1/counter@sha256:c0ffee"}}
	prevExec, prevDemo, prevInterval, prevHere := plugin.Exec, mode3.Demo, mode3.Interval, createdHere
	t.Cleanup(func() {
		plugin.Exec, mode3.Demo, mode3.Interval, createdHere = prevExec, prevDemo, prevInterval, prevHere
	})
	mode3.Interval = 0
	createdHere = func(context.Context, plugin.Options) (bool, error) { return true, nil }
	f.out["get nodeclass ate-isolated"] = []string{testReady}
	plugin.Exec = func(_ context.Context, w io.Writer, c plugin.Cmd) error {
		if len(c.Argv) < 3 || strings.Join(c.Argv[:3], " ") != "kubectl --kubeconfig /kc" {
			return fmt.Errorf("not the installer's kubectl: %q", c.Argv)
		}
		args := strings.Join(c.Argv[3:], " ")
		f.log = append(f.log, "kubectl "+args)
		if c.Stdin != nil {
			f.stdin[args] = string(c.Stdin)
			if args == "apply -f -" {
				f.applies = append(f.applies, string(c.Stdin))
			}
		}
		for k, v := range f.out {
			if strings.HasPrefix(args, k) && len(v) > 0 {
				out := c.Stdout
				if out == nil {
					out = w
				}
				io.WriteString(out, v[0])
				if len(v) > 1 {
					f.out[k] = v[1:]
				}
				break
			}
		}
		return nil
	}
	mode3.Demo = mode3.DemoFuncs{
		KubectlAte: func(context.Context, io.Writer, demo.Options, string) (string, error) {
			f.log = append(f.log, "build kubectl-ate")
			return "/bin/kubectl-ate", nil
		},
		Pin: func(_ context.Context, _ demo.Options, tagged string) (string, error) {
			return tagged + "@sha256:pinned", nil
		},
		Ate: func(_ context.Context, _ demo.Options, args ...string) ([]byte, error) {
			f.log = append(f.log, "ate "+strings.Join(args, " "))
			switch strings.Join(args[:2], " ") {
			case "get actor-template":
				img, ok := f.tmpls[args[2]]
				if !ok {
					return nil, fmt.Errorf("actor template %q not found", args[2])
				}
				return []byte(`{"spec":{"containers":[{"image":"` + img + `"}]}}`), nil
			case "create actor-template":
				b, _ := os.ReadFile(args[3])
				f.tmpls["counter-iso"], f.stdin["template"] = "ecr/c1/counter@sha256:c0ffee", string(b)
			case "delete actor":
				return nil, errors.New("actor not found")
			}
			return nil, nil
		},
		WaitGolden: func(context.Context, io.Writer, demo.Options, demo.Pool) error {
			f.log = append(f.log, "golden")
			return nil
		},
	}
	return f
}

func testOptions(t *testing.T, autoMode bool) plugin.Options {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(controlplane.TreeDir(root, mode3.Tag), "demos", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	return plugin.Options{Cluster: "c1", Region: "us-east-1", TreeRoot: root, Kubeconfig: "/kc", AutoMode: autoMode, Tags: "auto-delete=no"}
}

func (f *fake) has(entries ...string) error {
	i := 0
	for _, l := range f.log {
		if i < len(entries) && strings.HasPrefix(l, entries[i]) {
			i++
		}
	}
	if i < len(entries) {
		return fmt.Errorf("log lacks %q (in this order):\n%s", entries[i], strings.Join(f.log, "\n"))
	}
	return nil
}

func TestRegistered(t *testing.T) {
	p, ok := plugin.Get(ID)
	if !ok || len(p.Requires) != 1 || p.Requires[0] != mode3.ID || p.SetupAWS || p.Needs.Source != nil || p.NeedsAutoMode == "" {
		t.Errorf("plugin = %+v", p)
	}
	if err := plugin.Check(); err != nil {
		t.Error(err)
	}
}

// Both directions: without Auto Mode nothing runs and the error says why;
// with it the NodePool, atelet-iso and the pool go in, the template with
// mode 3's counter image once the worker Pods and atelet-iso are up.
func TestDeploy(t *testing.T) {
	f := newFake(t)
	for _, fn := range []func(context.Context, io.Writer, plugin.Options) error{deploy, verify} {
		err := fn(context.Background(), io.Discard, testOptions(t, false))
		if err == nil || !strings.Contains(err.Error(), "needs EKS Auto Mode") || !strings.Contains(err.Error(), "c1 has EKS Auto Mode off") || len(f.log) != 0 {
			t.Errorf("no Auto Mode: err = %v, ran %q", err, f.log)
		}
	}

	f = newFake(t)
	f.out["get runtimeclass isolated-sandbox"] = []string{"sandbox"}
	f.out["get nodepool general-purpose"] = []string{testGeneralPurpose}
	f.out["get nodeclass default"] = []string{testNodeClass}
	f.out["-n ate-system get ds atelet"] = []string{testAtelet}
	var out bytes.Buffer
	if err := deploy(context.Background(), &out, testOptions(t, true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// The NodeClass, tagged with the owner tag and TAGS, before the NodePool
	// that names it.
	if len(f.applies) < 2 || !strings.Contains(f.applies[0], `"kind":"NodeClass"`) ||
		!strings.Contains(f.applies[0], `"tags":{"agent-substrate-on-eks/deployment":"c1.us-east-1","auto-delete":"no","team":"a"}`) ||
		!strings.Contains(f.applies[1], `"kind":"NodePool"`) || !strings.Contains(f.applies[1], `"name":"ate-isolated"},"requirements"`) {
		t.Errorf("first applies:\n%s", strings.Join(f.applies[:min(2, len(f.applies))], "\n"))
	}
	if err := f.has("kubectl get runtimeclass isolated-sandbox", "kubectl get nodepool general-purpose -o json", "kubectl get nodeclass default -o json",
		"kubectl apply -f -", "kubectl apply -f -", "kubectl get nodeclass ate-isolated -o json",
		"kubectl apply -f -", "kubectl -n ate-system get ds atelet -o json", "kubectl apply -f -", "build kubectl-ate",
		"ate get actor-template counter-mode3 -a ate-demo-counter-mode3", "kubectl apply -f -", "ate create atespace ate-demo-counter-iso",
		"ate get actor-template counter-iso", "kubectl -n ate-demo-counter-iso rollout status deploy/counter-iso --timeout=10m",
		"kubectl -n ate-system rollout status ds/atelet-iso --timeout=5m", "ate create actor-template -f", "golden"); err != nil {
		t.Error(err)
	}
	pool := f.stdin["apply -f -"]
	if !strings.Contains(pool, "workerImage: "+mode3.ImageRepo+"/ateworker-gvisor:"+mode3.Tag+"@sha256:pinned") ||
		!strings.Contains(pool, "runtimeClassName: isolated-sandbox") || !strings.Contains(pool, "sandbox.amazonaws.com/overlay-disk-iops: disabled") {
		t.Errorf("pool:\n%s", pool)
	}
	if !strings.Contains(f.stdin["template"], "image: ecr/c1/counter@sha256:c0ffee") {
		t.Errorf("template:\n%s", f.stdin["template"])
	}

	// A cluster setup-aws did not create: the NodeClass keeps only its
	// source's tags, and the output says so.
	f = newFake(t)
	createdHere = func(context.Context, plugin.Options) (bool, error) { return false, nil }
	f.out["get nodepool general-purpose"] = []string{testGeneralPurpose}
	f.out["get nodeclass default"] = []string{testNodeClass}
	f.out["-n ate-system get ds atelet"] = []string{testAtelet}
	out.Reset()
	if err := deploy(context.Background(), &out, testOptions(t, true)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.applies[0], `"tags":{"auto-delete":"yes","team":"a"}`) || !strings.Contains(out.String(), NoNodeTagsLine) {
		t.Errorf("foreign cluster: NodeClass %s\n%s", f.applies[0], out.String())
	}

	// A NodeClass Auto Mode cannot validate stops the deploy with its reason
	// and the fix, before atelet-iso or the pool.
	f = newFake(t)
	f.out["get nodepool general-purpose"] = []string{testGeneralPurpose}
	f.out["get nodeclass default"] = []string{testNodeClass}
	f.out["get nodeclass ate-isolated"] = []string{testInvalid}
	err := deploy(context.Background(), io.Discard, testOptions(t, true))
	if err == nil || !strings.Contains(err.Error(), "isn't authorized to call ec2:CreateLaunchTemplate") || !strings.Contains(err.Error(), "setup-aws create cluster") || len(f.applies) != 2 {
		t.Errorf("invalid NodeClass: err %v, %d applies", err, len(f.applies))
	}

	// No mode 3 template: mode3-iso says to deploy mode 3 first.
	f = newFake(t)
	delete(f.tmpls, "counter-mode3")
	f.out["get nodepool general-purpose"] = []string{testGeneralPurpose}
	f.out["get nodeclass default"] = []string{testNodeClass}
	f.out["-n ate-system get ds atelet"] = []string{testAtelet}
	if err := deploy(context.Background(), io.Discard, testOptions(t, true)); err == nil || !strings.Contains(err.Error(), "deploy mode3 first") {
		t.Errorf("no mode 3: %v", err)
	}
}

// Remove deletes the pool, atelet-iso and its ConfigMap, then the NodePool,
// and waits until Auto Mode has removed its nodes; nodes that stay fail it.
func TestRemove(t *testing.T) {
	f := newFake(t)
	f.out["get crd nodepools.karpenter.sh"] = []string{"customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh\n"}
	f.out["get nodes -l karpenter.sh/nodepool=ate-isolated"] = []string{"node/i1\n", "node/i1\n", ""}
	var out bytes.Buffer
	if err := remove(context.Background(), &out, testOptions(t, true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := f.has("kubectl get namespace ate-system", "kubectl delete namespace ate-demo-counter-iso",
		"kubectl -n ate-system delete daemonset atelet-iso --ignore-not-found --wait=true", "kubectl delete -f - --ignore-not-found",
		"kubectl get crd nodepools.karpenter.sh", "kubectl delete nodepool ate-isolated --ignore-not-found --wait=true",
		"kubectl get nodes -l karpenter.sh/nodepool=ate-isolated", "kubectl get nodes -l karpenter.sh/nodepool=ate-isolated",
		"kubectl get nodes -l karpenter.sh/nodepool=ate-isolated", "kubectl delete nodeclass ate-isolated --ignore-not-found --wait=true"); err != nil {
		t.Error(err)
	}
	if f.stdin["delete -f - --ignore-not-found"] != string(providerManifest) {
		t.Error("the ConfigMap delete was not the provider manifest")
	}
	if !strings.Contains(out.String(), "No node of NodePool ate-isolated is left") {
		t.Errorf("output:\n%s", out.String())
	}

	f = newFake(t)
	f.out["get crd nodepools.karpenter.sh"] = []string{"crd/nodepools.karpenter.sh\n"}
	f.out["get nodes -l karpenter.sh/nodepool=ate-isolated"] = []string{"node/i1\n"}
	if err := remove(context.Background(), io.Discard, testOptions(t, true)); err == nil || !strings.Contains(err.Error(), "still there after 10 minutes: node/i1") {
		t.Errorf("nodes stay: %v", err)
	}
}
