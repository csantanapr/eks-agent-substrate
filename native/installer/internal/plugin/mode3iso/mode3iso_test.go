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
	"slices"
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
	// creates is the stdin of every `kubectl create -f -`, in order.
	creates []string
	// replaces is the stdin of every `kubectl replace --save-config -f -`
	// (the plugin record's pending entry), in order.
	replaces []string
	tmpls    map[string]string
	// actors are the actors the ate API holds, each running on pod-1.
	actors map[string]bool
	// deleted are the counter image tags Remove deleted.
	deleted []string
	// fail fails each kubectl call whose arguments start with a key, after
	// writing stderr's text for that key, as kubectl writes its error.
	fail   map[string]error
	stderr map[string]string
}

// testAppliedPool is NodePool ate-isolated as the API server returns it,
// with arch as its one kubernetes.io/arch value. newFake answers arm64, not
// testGeneralPurpose's amd64, so a build that read the source NodePool (or
// assumed amd64) would show.
func testAppliedPool(arch string) string {
	return `{"apiVersion":"karpenter.sh/v1","kind":"NodePool","metadata":{"name":"ate-isolated"},
 "spec":{"template":{"spec":{"requirements":[{"key":"karpenter.sh/capacity-type","operator":"In","values":["on-demand"]},
  {"key":"kubernetes.io/arch","operator":"In","values":["` + arch + `"]}]}}}}`
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, out: map[string][]string{}, stdin: map[string]string{}, tmpls: map[string]string{}, actors: map[string]bool{}}
	prevExec, prevDemo, prevInterval, prevHere, prevDelete := plugin.Exec, mode3.Demo, mode3.Interval, createdHere, deleteImage
	t.Cleanup(func() {
		plugin.Exec, mode3.Demo, mode3.Interval, createdHere, deleteImage = prevExec, prevDemo, prevInterval, prevHere, prevDelete
	})
	mode3.Interval = 0
	createdHere = func(context.Context, plugin.Options) (bool, error) { return true, nil }
	deleteImage = func(_ context.Context, _ io.Writer, o plugin.Options, tag string) error {
		if o.Cluster != "c1" {
			return fmt.Errorf("deleted from cluster %q", o.Cluster)
		}
		f.log = append(f.log, "delete image "+tag)
		f.deleted = append(f.deleted, tag)
		return nil
	}
	f.out["get nodeclass ate-isolated"] = []string{testReady}
	f.out["get nodepool ate-isolated"] = []string{testAppliedPool("arm64")}
	plugin.Exec = func(_ context.Context, w io.Writer, c plugin.Cmd) error {
		if len(c.Argv) < 3 || strings.Join(c.Argv[:3], " ") != "kubectl --kubeconfig /kc" {
			return fmt.Errorf("not the installer's kubectl: %q", c.Argv)
		}
		args := strings.Join(c.Argv[3:], " ")
		f.log = append(f.log, "kubectl "+args)
		for k, err := range f.fail {
			if strings.HasPrefix(args, k) {
				io.WriteString(w, f.stderr[k])
				return err
			}
		}
		if c.Stdin != nil {
			f.stdin[args] = string(c.Stdin)
			switch args {
			case "apply -f -":
				f.applies = append(f.applies, string(c.Stdin))
			case "create -f -":
				f.creates = append(f.creates, string(c.Stdin))
			case "replace --save-config -f -":
				f.replaces = append(f.replaces, string(c.Stdin))
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
		// No CounterImage: reading a node pool's nodes would panic here.
		CounterImageFor: func(_ context.Context, _ io.Writer, o demo.Options, tree, arch, tag string) (string, error) {
			if o.Tag != mode3.Tag || o.ImageRepo != mode3.ImageRepo || filepath.Base(tree) != "substrate-"+mode3.Tag {
				return "", fmt.Errorf("built from %s with %+v, not the fork's", tree, o)
			}
			f.log = append(f.log, "counter image "+arch+" "+tag)
			return "ecr/c1/counter@sha256:c0ffee", nil
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
			case "get actor":
				if _, ok := f.actors[args[2]]; !ok {
					return nil, errors.New("actor not found")
				}
				return []byte(`{"metadata":{"uid":"uid-` + args[2] + `"},"status":{"state":"ACTOR_STATE_RUNNING","workerAssignment":{"workerPod":"pod-1"}}}`), nil
			case "create actor":
				f.actors[args[2]] = true
			case "delete actor":
				if !f.actors[args[2]] {
					return nil, errors.New("actor not found")
				}
				delete(f.actors, args[2])
			}
			return nil, nil
		},
		RoundTrip: func(_ context.Context, _ io.Writer, _ demo.Options, p demo.Pool) (string, error) {
			f.log = append(f.log, "roundtrip "+p.Atespace+"/"+p.Template+"/"+p.Actor)
			return "resumed", nil
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
	tree := filepath.Join(root, "substrate-"+mode3.Tag)
	if err := os.MkdirAll(filepath.Join(tree, "demos", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	return plugin.Options{Cluster: "c1", Region: "us-east-1", Release: plugin.Release{Tag: mode3.Tag, ImageRepo: mode3.ImageRepo, Tree: tree}, TreeRoot: root, Kubeconfig: "/kc", AutoMode: autoMode, Tags: "auto-delete=no"}
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

// mode3-iso stands alone: it requires no plugin, needs Auto Mode, and
// declares mode 3's own Needs (the same Source, not a copy), so resolving it
// alone yields only mode3-iso, with the fork's release and NetworkWorkers.
func TestRegistered(t *testing.T) {
	p, ok := plugin.Get(ID)
	if !ok || len(p.Requires) != 0 || p.SetupAWS || p.NeedsAutoMode == "" {
		t.Errorf("plugin = %+v", p)
	}
	if p.Needs.Source != mode3.Needs.Source || !slices.Equal(p.Needs.Preview, mode3.Needs.Preview) {
		t.Errorf("needs = %+v, want mode3.Needs %+v", p.Needs, mode3.Needs)
	}
	if err := plugin.Check(); err != nil {
		t.Error(err)
	}
	ps, err := plugin.Resolve([]string{ID})
	if err != nil || len(ps) != 1 || ps[0].ID != ID {
		t.Fatalf("Resolve(%s) = %v, %v", ID, ps, err)
	}
	fork := plugin.Release{Tag: mode3.Tag, ImageRepo: mode3.ImageRepo, Repo: mode3.Repo, Ref: mode3.Commit}
	if r, err := plugin.ReleaseOf(ps, "v1.0.0", "ghcr.io/agent-substrate/substrate"); err != nil || r != fork {
		t.Errorf("release of %s alone = %+v, %v; want the fork %+v", ID, r, err, fork)
	}
	// With mode3 too: one release, both plugins.
	both, err := plugin.Resolve([]string{mode3.ID, ID})
	if err != nil || len(both) != 2 {
		t.Fatalf("Resolve both = %v, %v", both, err)
	}
	if r, err := plugin.ReleaseOf(both, "v1.0.0", "ghcr.io/agent-substrate/substrate"); err != nil || r != fork {
		t.Errorf("release of both = %+v, %v", r, err)
	}
}

// The control plane deploy for mode3-iso alone (its Needs merged, its
// release set, as resolvePlugins does) runs ate-setup with the fork's images
// and the NetworkWorkers gate. The same deploy without it carries neither.
func TestControlPlaneForMode3IsoAloneCarriesTheFork(t *testing.T) {
	for _, ids := range [][]string{{ID}, nil} {
		ps, err := plugin.Resolve(ids)
		if err != nil {
			t.Fatal(err)
		}
		o := controlplane.Options{Cluster: "c1", Region: "us-west-2", Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate",
			SnapBucket: "b", SandboxClasses: "gvisor,microvm", Database: controlplane.DatabaseBundled}
		if o.Needs, err = plugin.Merge(ps); err != nil {
			t.Fatal(err)
		}
		rel := o.Needs.Release(o.Tag, o.ImageRepo)
		o.Tag, o.ImageRepo = rel.Tag, rel.ImageRepo
		args := strings.Join(controlplane.AteSetupArgs(o, "c1"), " ")
		fork := strings.Contains(args, "--image-repo ghcr.io/csantanapr/substrate --image-tag "+mode3.Tag+" ") &&
			strings.Contains(args, " --preview=NetworkWorkers") && rel.Repo == mode3.Repo && rel.Ref == mode3.Commit
		if fork != (len(ids) == 1) {
			t.Errorf("plugins %v: fork %v: release %+v, ate-setup %s", ids, fork, rel, args)
		}
	}
}

// Both directions: without Auto Mode nothing runs and the error says why;
// with it the NodePool, atelet-iso and the pool go in, the template with
// mode 3's counter image once the worker Pods and atelet-iso are up.
func TestDeploy(t *testing.T) {
	// Without Auto Mode, Run refuses deploy and verify through the
	// registration's NeedsAutoMode after one read of the record, which
	// holds the value its deploy ran with: verify checks with it, and a
	// deploy with another one is refused before the Auto Mode check.
	var err error
	for _, action := range []string{plugin.ActionDeploy, plugin.ActionVerify} {
		f := newFake(t)
		err = plugin.Run(context.Background(), io.Discard, action, []string{ID}, testOptions(t, false))
		reads := 1
		if err == nil || !strings.Contains(err.Error(), needsAutoMode) || !strings.Contains(err.Error(), "c1 has EKS Auto Mode off") || len(f.log) != reads ||
			(reads == 1 && !strings.HasPrefix(f.log[0], "kubectl -n kube-system get configmap substrate-eks-plugins")) {
			t.Errorf("Run %s without Auto Mode: err = %v, ran %q", action, err, f.log)
		}
	}

	f := newFake(t)
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
	// Its own counter image, built for the applied NodePool's arm64 under
	// its own tag; nothing of mode 3's is read.
	if err := f.has("kubectl get runtimeclass isolated-sandbox", "kubectl get nodepool general-purpose -o json", "kubectl get nodeclass default -o json",
		"kubectl apply -f -", "kubectl apply -f -", "kubectl get nodepool ate-isolated -o json", "kubectl get nodeclass ate-isolated -o json",
		"kubectl apply -f -", "kubectl -n ate-system get ds atelet -o json", "kubectl apply -f -", "build kubectl-ate",
		"counter image arm64 mode3-iso-"+mode3.Tag, "kubectl apply -f -", "ate create atespace ate-demo-counter-iso",
		"ate get actor-template counter-iso", "kubectl -n ate-demo-counter-iso rollout status deploy/counter-iso --timeout=10m",
		"kubectl -n ate-system rollout status ds/atelet-iso --timeout=5m", "ate create actor-template -f", "golden"); err != nil {
		t.Error(err)
	}
	for _, l := range f.log {
		if strings.Contains(l, mode3.Kata.Name) || strings.Contains(l, mode3.Kata.Namespace) || strings.Contains(l, "kata") {
			t.Errorf("read mode 3's objects: %q", l)
		}
	}
	if !strings.Contains(out.String(), "NodePool ate-isolated launches arm64 nodes") {
		t.Errorf("output:\n%s", out.String())
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
	err = deploy(context.Background(), io.Discard, testOptions(t, true))
	if err == nil || !strings.Contains(err.Error(), "isn't authorized to call ec2:CreateLaunchTemplate") || !strings.Contains(err.Error(), "setup-aws create cluster") || len(f.applies) != 2 {
		t.Errorf("invalid NodeClass: err %v, %d applies", err, len(f.applies))
	}

	// An applied NodePool that allows two architectures stops the deploy
	// with the reason, after the two applies and before any build,
	// atelet-iso or pool.
	f = newFake(t)
	f.out["get nodepool general-purpose"] = []string{testGeneralPurpose}
	f.out["get nodeclass default"] = []string{testNodeClass}
	f.out["get nodepool ate-isolated"] = []string{strings.Replace(testAppliedPool("amd64"), `["amd64"]`, `["amd64","arm64"]`, 1)}
	f.out["-n ate-system get ds atelet"] = []string{testAtelet}
	err = deploy(context.Background(), io.Discard, testOptions(t, true))
	if err == nil || !strings.Contains(err.Error(), "NodePool ate-isolated allows amd64 and arm64") || len(f.applies) != 2 || f.has("counter image") == nil {
		t.Errorf("two architectures: err %v, %d applies, log %q", err, len(f.applies), f.log)
	}

	// A missing fork tree names mode3-iso, not mode3, as the plugin to
	// deploy the control plane with.
	f = newFake(t)
	o := testOptions(t, true)
	o.Tree = filepath.Join(t.TempDir(), "substrate-"+mode3.Tag)
	if err := deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "--deploy-control-plane --plugins mode3-iso first") || len(f.log) != 0 {
		t.Errorf("no tree: err = %v, ran %q", err, f.log)
	}
}

// Both ways: the applied NodePool's one kubernetes.io/arch value is the
// counter image's architecture, through several In requirements too; more
// than one, none, every one (no requirement or another operator) are
// refused with the reason.
func TestPoolArch(t *testing.T) {
	pool := func(reqs string) []byte {
		return []byte(`{"kind":"NodePool","spec":{"template":{"spec":{"requirements":[` + reqs + `]}}}}`)
	}
	other := `{"key":"karpenter.sh/capacity-type","operator":"In","values":["on-demand"]}`
	for want, reqs := range map[string]string{
		"amd64": other + `,{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}`,
		"arm64": `{"key":"kubernetes.io/arch","operator":"In","values":["arm64"]}`,
		// Two requirements: the value both allow.
		"amd64 ": `{"key":"kubernetes.io/arch","operator":"In","values":["amd64","arm64"]},{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}`,
	} {
		if got, err := PoolArch(pool(reqs)); err != nil || got != strings.TrimSpace(want) {
			t.Errorf("%s: PoolArch = %q, %v", reqs, got, err)
		}
	}
	for want, reqs := range map[string]string{
		"allows amd64 and arm64":                    `{"key":"kubernetes.io/arch","operator":"In","values":["arm64","amd64"]}`,
		"allows no architecture":                    `{"key":"kubernetes.io/arch","operator":"In","values":[]}`,
		"allows no architecture ":                   `{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]},{"key":"kubernetes.io/arch","operator":"In","values":["arm64"]}`,
		"has no kubernetes.io/arch requirement":     other,
		"requires kubernetes.io/arch NotIn [arm64]": `{"key":"kubernetes.io/arch","operator":"NotIn","values":["arm64"]}`,
		"requires kubernetes.io/arch Exists []":     `{"key":"kubernetes.io/arch","operator":"Exists"}`,
		"read NodePool ate-isolated":                `not json`,
	} {
		_, err := PoolArch(pool(reqs))
		if want == "read NodePool ate-isolated" {
			_, err = PoolArch([]byte(reqs))
		}
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%s: err = %v, want %q", reqs, err, want)
		}
	}
}

// Both ways: Verify passes a worker under isolated-sandbox on an
// ate-isolated node, runs the round trip and the delete check on its own
// pool, and reads nothing of mode 3's; a worker on another node fails it.
func TestVerify(t *testing.T) {
	const nodes = `{"items":[{"metadata":{"name":"n1","labels":{"ate.dev/pool":"isolated","karpenter.sh/nodepool":"ate-isolated"}}},
 {"metadata":{"name":"n2","labels":{"ate.dev/pool":"microvm"}}}]}`
	pods := func(node string) string {
		return `{"items":[{"metadata":{"name":"pod-1"},"spec":{"runtimeClassName":"isolated-sandbox","nodeName":"` + node + `"}}]}`
	}
	f := newFake(t)
	f.out["-n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network"] = []string{pods("n1")}
	f.out["get nodes -o json"] = []string{nodes}
	f.out["-n ate-demo-counter-iso logs pod-1"] = []string{"x Handle RPC method=TerminateWorkload actor=uid-demo-iso-delete\n"}
	var out bytes.Buffer
	if err := Iso.Verify(context.Background(), &out, testOptions(t, true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := f.has("build kubectl-ate", "kubectl -n ate-demo-counter-iso get pods", "kubectl get nodes -o json",
		"roundtrip ate-demo-counter-iso/counter-iso/demo-iso", "ate create actor demo-iso-delete -a ate-demo-counter-iso --template counter-iso",
		"ate delete actor demo-iso-delete", "kubectl -n ate-demo-counter-iso logs pod-1 --all-containers"); err != nil {
		t.Error(err)
	}
	for _, l := range f.log {
		if strings.Contains(l, mode3.Kata.Name) || strings.Contains(l, mode3.Kata.Namespace) || strings.Contains(l, "kata") {
			t.Errorf("Verify read mode 3's objects: %q", l)
		}
	}
	if !strings.Contains(out.String(), "PASS: the worker in Pod pod-1 tore down demo-iso-delete's sandbox") {
		t.Errorf("output:\n%s", out.String())
	}

	f = newFake(t)
	f.out["-n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network"] = []string{pods("n2")}
	f.out["get nodes -o json"] = []string{nodes}
	if err := Iso.Verify(context.Background(), io.Discard, testOptions(t, true)); err == nil || !strings.Contains(err.Error(), `whose ate.dev/pool is "microvm", not "isolated"`) {
		t.Errorf("worker on a microvm node: %v", err)
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
	// Its own counter image only, once its pool's Namespace is gone: never
	// mode 3's tag or the release tag the demo's counter carries.
	if !slices.Equal(f.deleted, []string{"mode3-iso-" + mode3.Tag}) || slices.Contains(f.deleted, mode3.CounterTag) {
		t.Errorf("deleted images %q", f.deleted)
	}
	for _, l := range f.log {
		if strings.Contains(l, mode3.Kata.Name) || strings.Contains(l, mode3.Kata.Namespace) || strings.Contains(l, "kata") {
			t.Errorf("Remove touched mode 3's objects: %q", l)
		}
	}
	if err := f.has("kubectl get namespace ate-system", "kubectl delete namespace ate-demo-counter-iso", "delete image mode3-iso-"+mode3.Tag,
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
