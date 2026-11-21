package mode3iso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
)

// testOptionsNodeGroup are testOptions on a cluster without Auto Mode, with
// NITRO_COMPUTE=node-group.
func testOptionsNodeGroup(t *testing.T) plugin.Options {
	o := testOptions(t, false)
	o.NitroCompute = plugin.NitroNodeGroup
	return o
}

// nitroNode is a node of the nitro node group as `kubectl get nodes -o
// json` lists it; alloc "" advertises no Nitro sandbox.
func nitroNode(name, arch, alloc string, ready bool) string {
	al := `{"cpu":"3920m"}`
	if alloc != "" {
		al = `{"cpu":"3920m","eks.amazonaws.com/nitro-sandbox":"` + alloc + `"}`
	}
	st := "False"
	if ready {
		st = "True"
	}
	return `{"metadata":{"name":"` + name + `","labels":{"ate.dev/pool":"nitro","eks.amazonaws.com/nitro-sandbox":"true","kubernetes.io/arch":"` + arch + `"}},` +
		`"status":{"allocatable":` + al + `,"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"` + st + `"}]}}`
}

func nodeList(items ...string) string { return `{"items":[` + strings.Join(items, ",") + `]}` }

// The RuntimeClass Auto Mode makes: the spec the Nitro sandbox needs, and no
// labels (its field manager, eks-auto-mode, is what marks it).
const autoModeRC = `{"apiVersion":"node.k8s.io/v1","kind":"RuntimeClass","metadata":{"name":"isolated-sandbox"},` +
	`"handler":"isolated-sandbox","overhead":{"podFixed":{"eks.amazonaws.com/nitro-sandbox":"1","memory":"256Mi"}},` +
	`"scheduling":{"nodeSelector":{"eks.amazonaws.com/nitro-sandbox":"true"}}}`

// ownedRC is the one the installer makes for deployment c1.us-east-1.
var ownedRC = func() string {
	b, err := RuntimeClassManifest("c1.us-east-1")
	if err != nil {
		panic(err)
	}
	return string(b)
}()

// The auto-mode flows send what they sent before NITRO_COMPUTE, byte for
// byte: every kubectl and demo call, every applied object and the output of
// Deploy, Remove and Verify, as testdata/auto-mode-flows.json recorded them
// on the branch before it (the temporary template file's name aside).
func TestAutoModeFlowsUnchanged(t *testing.T) {
	norm := regexp.MustCompile(`-f \S*substrate-eks-counter-iso-[0-9]+\.yaml`)
	got := map[string]any{}
	p, _ := plugin.Get(ID)
	for _, compute := range []string{"", plugin.NitroAutoMode} {
		f := newFake(t)
		f.out["get runtimeclass isolated-sandbox"] = []string{"sandbox"}
		f.out["get nodepool general-purpose"] = []string{testGeneralPurpose}
		f.out["get nodeclass default"] = []string{testNodeClass}
		f.out["-n ate-system get ds atelet"] = []string{testAtelet}
		o := testOptions(t, true)
		o.NitroCompute = compute
		var out bytes.Buffer
		if err := p.Deploy(context.Background(), &out, o); err != nil {
			t.Fatal(err)
		}
		got["deployLog"], got["deployApplies"], got["deployOut"], got["template"] = f.log, f.applies, out.String(), f.stdin["template"]

		f = newFake(t)
		f.out["get crd nodepools.karpenter.sh"] = []string{"crd/nodepools.karpenter.sh\n"}
		f.out["get nodes -l karpenter.sh/nodepool=ate-isolated"] = []string{"node/i1\n", ""}
		out.Reset()
		if err := p.Remove(context.Background(), &out, o); err != nil {
			t.Fatal(err)
		}
		got["removeLog"], got["removeOut"] = f.log, out.String()

		f = newFake(t)
		f.out["-n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network"] = []string{`{"items":[{"metadata":{"name":"pod-1"},"spec":{"runtimeClassName":"isolated-sandbox","nodeName":"n1"}}]}`}
		f.out["get nodes -o json"] = []string{`{"items":[{"metadata":{"name":"n1","labels":{"ate.dev/pool":"isolated","karpenter.sh/nodepool":"ate-isolated"}}}]}`}
		f.out["-n ate-demo-counter-iso logs pod-1"] = []string{"x Handle RPC method=TerminateWorkload actor=uid-demo-iso-delete\n"}
		out.Reset()
		if err := p.Verify(context.Background(), &out, o); err != nil {
			t.Fatal(err)
		}
		got["verifyLog"], got["verifyOut"] = f.log, out.String()

		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var have map[string]any
		if err := json.Unmarshal([]byte(norm.ReplaceAllString(string(b), "-f <template file>")), &have); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile("testdata/auto-mode-flows.json")
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(have, want) {
			gotJSON, _ := json.MarshalIndent(have, "", " ")
			t.Errorf("NITRO_COMPUTE=%q: the auto-mode flows changed:\n%s", compute, gotJSON)
		}
	}
	if Iso.Pool != poolManifest || Iso.NodeLabels[PoolLabel] != PoolValue || Iso.NodeLabels[nodePoolLabel] != NodePoolName {
		t.Errorf("Iso changed: %+v", Iso.NodeLabels)
	}
}

// The node-group pool is counter-iso.yaml with its nodeSelector and its
// toleration on the nitro nodes, and nothing else changed; a manifest
// without one of the lines, or with one twice, is refused.
func TestNodeGroupPool(t *testing.T) {
	if nodeGroupPoolErr != nil {
		t.Fatal(nodeGroupPoolErr)
	}
	a, b := strings.Split(poolManifest, "\n"), strings.Split(nodeGroupPool, "\n")
	if len(a) != len(b) {
		t.Fatalf("%d lines, want %d", len(b), len(a))
	}
	var diff []string
	for i := range a {
		if a[i] != b[i] {
			diff = append(diff, a[i]+" -> "+b[i])
		}
	}
	if want := []string{"      ate.dev/pool: isolated ->       ate.dev/pool: nitro", "      value: isolated ->       value: nitro"}; strings.Join(diff, "|") != strings.Join(want, "|") {
		t.Errorf("changed lines %q, want %q", diff, want)
	}
	if IsoNodeGroup.Pool != nodeGroupPool || len(IsoNodeGroup.NodeLabels) != 1 || IsoNodeGroup.NodeLabels[PoolLabel] != NitroPool ||
		IsoNodeGroup.Name != Iso.Name || IsoNodeGroup.Namespace != Iso.Namespace || IsoNodeGroup.RuntimeClass != RuntimeClass {
		t.Errorf("IsoNodeGroup %+v", IsoNodeGroup)
	}
	for _, bad := range []string{
		strings.Replace(poolManifest, "      value: isolated\n", "      value: other\n", 1),
		strings.Replace(poolManifest, "    nodeSelector:\n      ate.dev/pool: isolated\n", "    nodeSelector:\n      ate.dev/pool: isolated\n      ate.dev/pool: isolated\n", 1),
	} {
		if _, err := renderNodeGroupPool(bad); err == nil || !strings.Contains(err.Error(), "not one") {
			t.Errorf("a changed manifest: %v", err)
		}
	}
	if n, err := poolReplicas(); err != nil || n != 2 {
		t.Errorf("replicas %d, %v", n, err)
	}
}

// The RuntimeClass decisions: none is made, with the spec and the
// installer's labels, but only by makeRuntimeClass, with create, never
// apply (the check changes nothing); Auto Mode's (the same spec) is used as
// it is; one that differs in handler, node selector or overhead is refused,
// naming the difference, before anything else; a failed read is never
// "none".
func TestRuntimeClassDecisions(t *testing.T) {
	f := newFake(t)
	o := testOptionsNodeGroup(t)
	m, err := checkRuntimeClass(context.Background(), io.Discard, o)
	if err != nil || m == nil || len(f.applies) != 0 || len(f.creates) != 0 {
		t.Fatalf("none: manifest %s, %v, applies %q, creates %q", m, err, f.applies, f.creates)
	}
	if err := makeRuntimeClass(context.Background(), io.Discard, o, m); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 || len(f.applies) != 0 || f.has("kubectl create -f -") != nil {
		t.Fatalf("creates %q, applies %q", f.creates, f.applies)
	}
	rc := decode(t, []byte(f.creates[0]))
	meta, _ := json.Marshal(rc["metadata"])
	over, _ := json.Marshal(rc["overhead"])
	sched, _ := json.Marshal(rc["scheduling"])
	if rc["kind"] != "RuntimeClass" || rc["apiVersion"] != "node.k8s.io/v1" || rc["handler"] != "isolated-sandbox" ||
		string(meta) != `{"labels":{"agent-substrate-on-eks/deployment":"c1.us-east-1","app.kubernetes.io/managed-by":"substrate-eks"},"name":"isolated-sandbox"}` ||
		string(over) != `{"podFixed":{"eks.amazonaws.com/nitro-sandbox":"1","memory":"256Mi"}}` ||
		string(sched) != `{"nodeSelector":{"eks.amazonaws.com/nitro-sandbox":"true"}}` {
		t.Errorf("made RuntimeClass %s", f.creates[0])
	}
	if diff, err := RuntimeClassDiff([]byte(f.creates[0])); err != nil || len(diff) != 0 {
		t.Errorf("the made one differs from the spec: %v %v", diff, err)
	}

	f = newFake(t)
	f.out["get runtimeclass isolated-sandbox"] = []string{autoModeRC}
	m, err = checkRuntimeClass(context.Background(), io.Discard, testOptionsNodeGroup(t))
	if err == nil {
		err = makeRuntimeClass(context.Background(), io.Discard, testOptionsNodeGroup(t), m)
	}
	if err != nil || m != nil || len(f.applies) != 0 || len(f.creates) != 0 {
		t.Errorf("Auto Mode's: %v, applies %q, creates %q", err, f.applies, f.creates)
	}

	for want, rc := range map[string]string{
		`handler "kata", not "isolated-sandbox"`: strings.Replace(autoModeRC, `"handler":"isolated-sandbox"`, `"handler":"kata"`, 1),
		"scheduling.nodeSelector eks.amazonaws.com/nitro-sandbox=true,zone=a, not eks.amazonaws.com/nitro-sandbox=true": strings.Replace(autoModeRC,
			`{"eks.amazonaws.com/nitro-sandbox":"true"}`, `{"eks.amazonaws.com/nitro-sandbox":"true","zone":"a"}`, 1),
		"scheduling.nodeSelector none":                                     strings.Replace(autoModeRC, `,"scheduling":{"nodeSelector":{"eks.amazonaws.com/nitro-sandbox":"true"}}`, ``, 1),
		"overhead.podFixed eks.amazonaws.com/nitro-sandbox=1,memory=512Mi": strings.Replace(autoModeRC, `"256Mi"`, `"512Mi"`, 1),
		"overhead.podFixed memory=256Mi, not":                              strings.Replace(autoModeRC, `"eks.amazonaws.com/nitro-sandbox":"1",`, ``, 1),
	} {
		f := newFake(t)
		f.out["get runtimeclass isolated-sandbox"] = []string{rc}
		f.out["get nodes -l ate.dev/pool=nitro"] = []string{nodeList(nitroNode("n1", "amd64", "2", true))}
		err := deploy(context.Background(), io.Discard, testOptionsNodeGroup(t))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "The installer leaves it as it is") ||
			!strings.Contains(err.Error(), "Other Pods can use it: check them first") {
			t.Errorf("want %q, got %v", want, err)
		}
		if len(f.applies) != 0 || len(f.creates) != 0 || f.has("kubectl get nodes") == nil {
			t.Errorf("%s: went on: applies %q, creates %q, log %q", want, f.applies, f.creates, f.log)
		}
	}

	f = newFake(t)
	f.fail = map[string]error{"get runtimeclass": errors.New("forbidden")}
	if m, err := checkRuntimeClass(context.Background(), io.Discard, testOptionsNodeGroup(t)); err == nil || !strings.Contains(err.Error(), "forbidden") || m != nil || len(f.applies) != 0 || len(f.creates) != 0 {
		t.Errorf("a denied read: %v, applies %q, creates %q", err, f.applies, f.creates)
	}
}

// kubectl's answer when the RuntimeClass was made between the check and the
// create, as it writes it to stderr; its error is only the exit status.
const (
	rcAlreadyExists = `Error from server (AlreadyExists): error when creating "STDIN": runtimeclasses.node.k8s.io "isolated-sandbox" already exists` + "\n"
	rcForbidden     = `Error from server (Forbidden): error when creating "STDIN": runtimeclasses.node.k8s.io is forbidden` + "\n"
)

// A RuntimeClass made by another between the check and the create is never
// changed: the create fails with AlreadyExists, and the one there is read
// again and used when it has the Nitro sandbox's spec, or refused, naming
// the difference, before anything else is applied. Another create failure
// stops Deploy as it is, with no second read, and one gone again by then is
// an error too.
func TestRuntimeClassMadeMeanwhile(t *testing.T) {
	exit1 := errors.New("kubectl --kubeconfig /kc: exit status 1")
	for _, c := range []struct {
		name, stderr, racer, want string
	}{
		{"the Nitro sandbox's", rcAlreadyExists, autoModeRC, ""},
		{"another handler", rcAlreadyExists, strings.Replace(autoModeRC, `"handler":"isolated-sandbox"`, `"handler":"kata"`, 1), `handler "kata", not "isolated-sandbox"`},
		{"another overhead", rcAlreadyExists, strings.Replace(autoModeRC, `"256Mi"`, `"512Mi"`, 1), "overhead.podFixed eks.amazonaws.com/nitro-sandbox=1,memory=512Mi"},
		{"gone again", rcAlreadyExists, "", "is gone again"},
		{"denied, not AlreadyExists", rcForbidden, autoModeRC, "exit status 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			// None at the check, the racer's from then on.
			f.out["get runtimeclass isolated-sandbox"] = []string{"", c.racer}
			f.out["get nodes -l ate.dev/pool=nitro"] = []string{nodeList(nitroNode("n1", "amd64", "2", true))}
			f.out["-n ate-system get ds atelet"] = []string{testAtelet}
			f.fail = map[string]error{"create -f -": exit1}
			f.stderr = map[string]string{"create -f -": c.stderr}
			var out bytes.Buffer
			err := deploy(context.Background(), &out, testOptionsNodeGroup(t))
			if c.want == "" {
				if err != nil {
					t.Fatalf("%v\n%s", err, out.String())
				}
				if err := f.has("kubectl get runtimeclass isolated-sandbox --ignore-not-found -o json", "kubectl create -f -",
					"kubectl get runtimeclass isolated-sandbox --ignore-not-found -o json", "kubectl -n ate-system get ds atelet"); err != nil {
					t.Error(err)
				}
				if !strings.Contains(out.String(), "was made since it was read; checking that one\nRuntimeClass isolated-sandbox -> handler isolated-sandbox, the Nitro sandbox's") {
					t.Errorf("output:\n%s", out.String())
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("want %q, got %v", c.want, err)
				}
				if len(f.applies) != 0 || f.has("kubectl -n ate-system get ds atelet") == nil {
					t.Errorf("went on: applies %q, log %q", f.applies, f.log)
				}
			}
			for _, a := range f.applies {
				if strings.Contains(a, `"kind":"RuntimeClass"`) {
					t.Errorf("applied the RuntimeClass: %s", a)
				}
			}
			reads := 0
			for _, l := range f.log {
				if strings.HasPrefix(l, "kubectl get runtimeclass") {
					reads++
				}
				if strings.Contains(l, "runtimeclass") && !strings.HasPrefix(l, "kubectl get runtimeclass") {
					t.Errorf("wrote the RuntimeClass: %q", l)
				}
			}
			// Read again only on AlreadyExists.
			want := 2
			if c.stderr == rcForbidden {
				want = 1
			}
			if reads != want {
				t.Errorf("read the RuntimeClass %d times, want %d:\n%s", reads, want, strings.Join(f.log, "\n"))
			}
		})
	}
}

// Both directions: Ready nodes whose eks.amazonaws.com/nitro-sandbox covers
// the workers pass and give their architecture; none, 0, not Ready, too
// little in all, two architectures and no node at all are refused.
func TestNitroNodes(t *testing.T) {
	for want, nodes := range map[string]string{
		"amd64": nodeList(nitroNode("n1", "amd64", "2", true)),
		"arm64": nodeList(nitroNode("n1", "arm64", "1", true), nitroNode("n2", "arm64", "1", true)),
		// A NotReady node and one without capacity are not counted.
		"amd64 ":  nodeList(nitroNode("n1", "amd64", "4", true), nitroNode("n2", "arm64", "4", false), nitroNode("n3", "arm64", "", true)),
		"amd64  ": nodeList(nitroNode("n1", "amd64", "2", true), nitroNode("n2", "arm64", "0", true)),
	} {
		if arch, err := NitroNodes([]byte(nodes), 2); err != nil || arch != strings.TrimSpace(want) {
			t.Errorf("%s: %q, %v", nodes, arch, err)
		}
	}
	for want, nodes := range map[string]string{
		"0 allocatable eks.amazonaws.com/nitro-sandbox in all (n1 0)":         nodeList(nitroNode("n1", "amd64", "0", true)),
		"0 allocatable eks.amazonaws.com/nitro-sandbox in all (n1 none)":      nodeList(nitroNode("n1", "amd64", "", true)),
		"0 allocatable eks.amazonaws.com/nitro-sandbox in all (n1 not Ready)": nodeList(nitroNode("n1", "amd64", "2", false)),
		"1 allocatable eks.amazonaws.com/nitro-sandbox in all (n1 1)":         nodeList(nitroNode("n1", "amd64", "1", true)),
		`the kubernetes.io/arch values ["amd64" "arm64"]`:                     nodeList(nitroNode("n1", "amd64", "1", true), nitroNode("n2", "arm64", "1", true)),
		"no node is labeled ate.dev/pool=nitro":                               nodeList(),
		"read the nodes labeled ate.dev/pool=nitro":                           "not json",
		// Capacity on nodes RuntimeClass isolated-sandbox does not select
		// does not count, and those nodes are named.
		"1 allocatable eks.amazonaws.com/nitro-sandbox in all (n1 not labeled eks.amazonaws.com/nitro-sandbox=true, n2 1, n3 not labeled eks.amazonaws.com/nitro-sandbox=true)": nodeList(
			unlabeled(nitroNode("n1", "amd64", "4", true), ""), nitroNode("n2", "amd64", "1", true), unlabeled(nitroNode("n3", "amd64", "4", true), "false")),
		"RuntimeClass isolated-sandbox runs its Pods only on nodes labeled eks.amazonaws.com/nitro-sandbox=true, and n1, n3 lack that label": nodeList(
			unlabeled(nitroNode("n1", "amd64", "4", true), ""), nitroNode("n2", "amd64", "1", true), unlabeled(nitroNode("n3", "amd64", "4", true), "false")),
	} {
		_, err := NitroNodes([]byte(nodes), 2)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
		if err != nil && strings.Contains(want, "in all") && !strings.Contains(err.Error(), "Nitro Sandbox AMI (NITRO_AMI_ID; build one in a clone of https://github.com/awslabs/amazon-eks-ami: make k8s=") {
			t.Errorf("%s: no AMI advice: %v", want, err)
		}
		if err != nil && strings.Contains(err.Error(), "lack that label") != (strings.Contains(want, "not labeled") || strings.Contains(want, "lack that label")) {
			t.Errorf("%s: the label hint is wrong: %v", want, err)
		}
	}
	// Enough capacity on the labeled nodes: the others are left out, and
	// their architecture does not count.
	if arch, err := NitroNodes([]byte(nodeList(nitroNode("n1", "amd64", "2", true), unlabeled(nitroNode("n2", "arm64", "4", true), ""))), 2); err != nil || arch != "amd64" {
		t.Errorf("an unlabeled arm64 node next to an amd64 one: %q, %v", arch, err)
	}
}

// unlabeled is node without eks.amazonaws.com/nitro-sandbox=true: the
// label removed, or set to value.
func unlabeled(node, value string) string {
	if value == "" {
		return strings.Replace(node, `,"eks.amazonaws.com/nitro-sandbox":"true"`, "", 1)
	}
	return strings.Replace(node, `"eks.amazonaws.com/nitro-sandbox":"true"`, `"eks.amazonaws.com/nitro-sandbox":"`+value+`"`, 1)
}

// atelet-iso on the nitro nodes: atelet renamed and labeled, on those nodes
// only, with atelet's AL2023 volumes and mounts as they are (no ConfigMap);
// an atelet without them is refused.
func TestAteletIsoNodeGroup(t *testing.T) {
	b, err := AteletIsoNodeGroup([]byte(testAtelet))
	if err != nil {
		t.Fatal(err)
	}
	d := decode(t, b)
	src := decode(t, []byte(testAtelet))
	meta, _ := json.Marshal(d["metadata"])
	if string(meta) != `{"labels":{"app":"atelet"},"name":"atelet-iso","namespace":"ate-system"}` || d["status"] != nil ||
		get(t, d, "spec", "selector", "matchLabels", "ate.dev/atelet-variant") != "iso" ||
		get(t, d, "spec", "template", "metadata", "labels", "ate.dev/atelet-variant") != "iso" {
		t.Errorf("metadata %s, labels %v", meta, get(t, d, "spec", "template", "metadata"))
	}
	aff, _ := json.Marshal(get(t, d, "spec", "template", "spec", "affinity"))
	if string(aff) != `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"ate.dev/pool","operator":"In","values":["nitro"]}]}]}}}` {
		t.Errorf("affinity %s", aff)
	}
	for _, k := range []string{"volumes", "containers"} {
		have, _ := json.Marshal(get(t, d, "spec", "template", "spec", k))
		want, _ := json.Marshal(get(t, src, "spec", "template", "spec", k))
		if string(have) != string(want) {
			t.Errorf("%s changed:\n got %s\nwant %s", k, have, want)
		}
	}
	if strings.Contains(string(b), CredentialProviderConfigMap) || strings.Contains(string(b), "subPath") || strings.Contains(string(b), "/usr/libexec") {
		t.Errorf("Bottlerocket changes in %s", b)
	}
	noVols := strings.Replace(testAtelet, `"image-credential-provider-bin","hostPath"`, `"other","hostPath"`, 1)
	if _, err := AteletIsoNodeGroup([]byte(noVols)); err == nil || !strings.Contains(err.Error(), "image-credential-provider-bin") {
		t.Errorf("no provider bin volume: %v", err)
	}
	if _, err := AteletIsoNodeGroup([]byte(testGeneralPurpose)); err == nil {
		t.Error("a NodePool was accepted as atelet")
	}
}

// Deploy on the nitro node group: the RuntimeClass read, the nodes'
// capacity and architecture, then the RuntimeClass made, atelet-iso and the
// pool on those nodes, in that order; nothing of the NodePool path, no AWS
// call. Too little capacity stops it before anything is applied, the
// RuntimeClass included, whether the cluster has one or not.
func TestDeployNodeGroup(t *testing.T) {
	f := newFake(t)
	createdHere = func(context.Context, plugin.Options) (bool, error) {
		t.Error("read the cluster from AWS")
		return false, nil
	}
	f.out["get nodes -l ate.dev/pool=nitro"] = []string{nodeList(nitroNode("n1", "arm64", "2", true))}
	f.out["-n ate-system get ds atelet"] = []string{testAtelet}
	var out bytes.Buffer
	if err := deploy(context.Background(), &out, testOptionsNodeGroup(t)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := f.has("kubectl get runtimeclass isolated-sandbox --ignore-not-found -o json",
		"kubectl get nodes -l ate.dev/pool=nitro -o json", "kubectl create -f -", "kubectl -n ate-system get ds atelet -o json", "kubectl apply -f -",
		"build kubectl-ate", "counter image arm64 mode3-iso-"+mode3.Tag, "kubectl apply -f -", "ate create atespace ate-demo-counter-iso",
		"ate get actor-template counter-iso", "kubectl -n ate-demo-counter-iso rollout status deploy/counter-iso --timeout=10m",
		"kubectl -n ate-system rollout status ds/atelet-iso --timeout=5m", "ate create actor-template -f", "golden"); err != nil {
		t.Error(err)
	}
	for _, l := range f.log {
		for _, never := range []string{"nodepool", "nodeclass", "crd", CredentialProviderConfigMap, "kata"} {
			if strings.Contains(l, never) {
				t.Errorf("node-group deploy ran %q", l)
			}
		}
	}
	if len(f.creates) != 1 || !strings.Contains(f.creates[0], `"kind":"RuntimeClass"`) || len(f.applies) != 2 || !strings.Contains(f.applies[0], `"name":"atelet-iso"`) {
		t.Fatalf("creates:\n%s\napplies:\n%s", strings.Join(f.creates, "\n"), strings.Join(f.applies, "\n"))
	}
	if !strings.Contains(f.applies[0], `"values":["nitro"]`) || strings.Contains(f.applies[0], CredentialProviderConfigMap) {
		t.Errorf("atelet-iso %s", f.applies[0])
	}
	pool := f.applies[1]
	if !strings.Contains(pool, "      ate.dev/pool: nitro\n") || !strings.Contains(pool, "      value: nitro\n") ||
		strings.Contains(pool, "ate.dev/pool: isolated") || strings.Contains(pool, "value: isolated") ||
		!strings.Contains(pool, "runtimeClassName: isolated-sandbox") ||
		!strings.Contains(pool, "workerImage: "+mode3.ImageRepo+"/ateworker-gvisor:"+mode3.Tag+"@sha256:pinned") {
		t.Errorf("pool:\n%s", pool)
	}
	if !strings.Contains(out.String(), "The nodes labeled ate.dev/pool=nitro run 2 or more Nitro sandboxes, on arm64") {
		t.Errorf("output:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "RuntimeClass isolated-sandbox: none on the cluster; making it") {
		t.Errorf("output:\n%s", out.String())
	}

	for name, rc := range map[string]string{"Auto Mode's RuntimeClass": autoModeRC, "no RuntimeClass": ""} {
		f = newFake(t)
		f.out["get runtimeclass isolated-sandbox"] = []string{rc}
		f.out["get nodes -l ate.dev/pool=nitro"] = []string{nodeList(nitroNode("n1", "amd64", "1", true))}
		out.Reset()
		err := deploy(context.Background(), &out, testOptionsNodeGroup(t))
		if err == nil || !strings.Contains(err.Error(), "1 allocatable eks.amazonaws.com/nitro-sandbox in all") || len(f.applies) != 0 || len(f.creates) != 0 ||
			f.has("kubectl apply") == nil || f.has("kubectl create") == nil || f.has("kubectl -n ate-system get ds atelet") == nil || strings.Contains(out.String(), "making it") {
			t.Errorf("%s, too little capacity: %v, applies %d, log %q", name, err, len(f.applies), f.log)
		}
	}
}

// The Auto Mode refusal is skipped in node-group mode only: without Auto
// Mode, Run deploys and checks mode3-iso with NITRO_COMPUTE=node-group, and
// refuses it with auto-mode or unset, before anything runs.
func TestRunNeedsAutoModeOnlyOnAutoMode(t *testing.T) {
	for _, c := range []struct {
		compute string
		auto    bool
		runs    bool
	}{
		{"", false, false}, {plugin.NitroAutoMode, false, false}, {plugin.NitroNodeGroup, false, true},
		{plugin.NitroAutoMode, true, true}, {plugin.NitroNodeGroup, true, true},
	} {
		f := newFake(t)
		label := PoolValue
		if c.compute == plugin.NitroNodeGroup {
			label = NitroPool
		}
		f.out["-n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network"] = []string{`{"items":[{"metadata":{"name":"pod-1"},"spec":{"runtimeClassName":"isolated-sandbox","nodeName":"n1"}}]}`}
		f.out["get nodes -o json"] = []string{`{"items":[{"metadata":{"name":"n1","labels":{"ate.dev/pool":"` + label + `","karpenter.sh/nodepool":"ate-isolated"}}}]}`}
		f.out["-n ate-demo-counter-iso logs pod-1"] = []string{"x Handle RPC method=TerminateWorkload actor=uid-demo-iso-delete\n"}
		o := testOptions(t, c.auto)
		o.NitroCompute = c.compute
		err := plugin.Run(context.Background(), io.Discard, plugin.ActionVerify, []string{ID}, o)
		if c.runs != (err == nil) {
			t.Errorf("%+v: %v", c, err)
		}
		if !c.runs && (!strings.Contains(err.Error(), "c1 has EKS Auto Mode off") || len(f.log) != 0) {
			t.Errorf("%+v: %v, ran %q", c, err, f.log)
		}
	}
}

// Verify on the nitro node group wants the workers on nodes labeled
// ate.dev/pool=nitro: one on an Auto Mode isolated node fails it.
func TestVerifyNodeGroup(t *testing.T) {
	nodes := `{"items":[{"metadata":{"name":"n1","labels":{"ate.dev/pool":"nitro"}}},{"metadata":{"name":"n2","labels":{"ate.dev/pool":"isolated","karpenter.sh/nodepool":"ate-isolated"}}}]}`
	pods := func(node string) string {
		return `{"items":[{"metadata":{"name":"pod-1"},"spec":{"runtimeClassName":"isolated-sandbox","nodeName":"` + node + `"}}]}`
	}
	p, _ := plugin.Get(ID)
	f := newFake(t)
	f.out["-n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network"] = []string{pods("n1")}
	f.out["get nodes -o json"] = []string{nodes}
	f.out["-n ate-demo-counter-iso logs pod-1"] = []string{"x Handle RPC method=TerminateWorkload actor=uid-demo-iso-delete\n"}
	var out bytes.Buffer
	if err := p.Verify(context.Background(), &out, testOptionsNodeGroup(t)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "node n1 (ate.dev/pool=nitro)") {
		t.Errorf("output:\n%s", out.String())
	}
	f = newFake(t)
	f.out["-n ate-demo-counter-iso get pods -l ate.dev/worker-transport=network"] = []string{pods("n2")}
	f.out["get nodes -o json"] = []string{nodes}
	if err := p.Verify(context.Background(), io.Discard, testOptionsNodeGroup(t)); err == nil || !strings.Contains(err.Error(), `whose ate.dev/pool is "isolated", not "nitro"`) {
		t.Errorf("a worker on an Auto Mode node: %v", err)
	}
}

// Remove on the nitro node group: the pool, its image tag and atelet-iso,
// what an auto-mode deploy may have left (here none: no Auto Mode kinds),
// then the RuntimeClass only when the installer made it for this
// deployment and no Pod runs under it.
func TestRemoveNodeGroup(t *testing.T) {
	otherOwner := strings.Replace(ownedRC, "c1.us-east-1", "c2.us-east-1", 1)
	otherTool := strings.Replace(ownedRC, `"app.kubernetes.io/managed-by":"substrate-eks"`, `"app.kubernetes.io/managed-by":"helm"`, 1)
	for _, c := range []struct {
		name, rc, pods string
		deleted        bool
		say            string
	}{
		{"owned and unused", ownedRC, "kube-system/coredns-1 \nate-system/atelet-x \n", true, "Deleting RuntimeClass isolated-sandbox"},
		{"owned, in use", ownedRC, "kube-system/coredns-1 \ndefault/app isolated-sandbox\n", false, "Pods still run under it (default/app)"},
		{"Auto Mode's", autoModeRC, "", false, "is not the installer's"},
		{"another deployment's", otherOwner, "", false, "is not the installer's"},
		{"another tool's, with this deployment's label", otherTool, "", false, "is not the installer's"},
		{"none", "", "", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			f.out["get runtimeclass isolated-sandbox"] = []string{c.rc}
			f.out["get pods --all-namespaces"] = []string{c.pods}
			var out bytes.Buffer
			if err := remove(context.Background(), &out, testOptionsNodeGroup(t)); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if err := f.has("kubectl get namespace ate-system", "kubectl delete namespace ate-demo-counter-iso", "delete image mode3-iso-"+mode3.Tag,
				"kubectl -n ate-system delete daemonset atelet-iso --ignore-not-found --wait=true", "kubectl delete -f - --ignore-not-found",
				"kubectl get crd nodepools.karpenter.sh nodeclasses.eks.amazonaws.com --ignore-not-found -o name",
				"kubectl get runtimeclass isolated-sandbox --ignore-not-found -o json"); err != nil {
				t.Error(err)
			}
			if deleted := f.has("kubectl delete runtimeclass isolated-sandbox --ignore-not-found") == nil; deleted != c.deleted {
				t.Errorf("RuntimeClass deleted %v, want %v:\n%s", deleted, c.deleted, strings.Join(f.log, "\n"))
			}
			if listed := f.has("kubectl get pods --all-namespaces") == nil; listed != (c.rc == ownedRC) {
				t.Errorf("listed the Pods %v", listed)
			}
			if !strings.Contains(out.String(), c.say) {
				t.Errorf("output lacks %q:\n%s", c.say, out.String())
			}
			for _, l := range f.log {
				for _, never := range []string{"delete nodepool", "delete nodeclass", "get nodepool", "get nodes -l karpenter.sh"} {
					if strings.Contains(l, never) {
						t.Errorf("node-group remove ran %q", l)
					}
				}
			}
		})
	}
	f := newFake(t)
	f.out["get runtimeclass isolated-sandbox"] = []string{ownedRC}
	f.fail = map[string]error{"get pods --all-namespaces": errors.New("forbidden")}
	if err := remove(context.Background(), io.Discard, testOptionsNodeGroup(t)); err == nil || !strings.Contains(err.Error(), "forbidden") ||
		f.has("kubectl delete runtimeclass") == nil {
		t.Errorf("a denied Pod list: %v", err)
	}
}

// Remove does not trust NITRO_COMPUTE, which is not recorded: a node-group
// install removed under auto-mode (on a cluster without Auto Mode) loses its
// RuntimeClass, and an auto-mode install removed under node-group (on an
// Auto Mode cluster) loses its ConfigMap. Under node-group, NodePool and
// NodeClass ate-isolated are never deleted, whoever made them: those there
// are named, with the auto-mode remove that deletes them, and a cluster that
// runs its own Karpenter (the NodePool kind, no NodeClass kind) or has
// neither object gets no such line. Under auto-mode on an Auto Mode cluster
// the NodePool and NodeClass are deleted and no RuntimeClass is read, as
// before.
func TestRemoveIsModeAgnostic(t *testing.T) {
	const bothCRDs = "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh\ncustomresourcedefinition.apiextensions.k8s.io/nodeclasses.eks.amazonaws.com\n"
	for _, c := range []struct {
		name          string
		auto          bool
		compute       string
		crds, objs    string
		rcRead, rcDel bool
		nodePool      bool
		// left is what the "Left as they are" line names; "" is no line.
		left string
	}{
		{"node-group install, removed under auto-mode", false, plugin.NitroAutoMode, "", "", true, true, false, ""},
		{"node-group install, removed with NITRO_COMPUTE unset", false, "", "", "", true, true, false, ""},
		{"auto-mode install, removed under node-group", true, plugin.NitroNodeGroup, bothCRDs, "nodepool.karpenter.sh/ate-isolated\nnodeclass.eks.amazonaws.com/ate-isolated\n", true, false, false,
			"nodepool.karpenter.sh/ate-isolated, nodeclass.eks.amazonaws.com/ate-isolated"},
		{"node-group, only a NodeClass there", true, plugin.NitroNodeGroup, bothCRDs, "nodeclass.eks.amazonaws.com/ate-isolated\n", true, false, false, "nodeclass.eks.amazonaws.com/ate-isolated"},
		{"node-group, Auto Mode, neither there", true, plugin.NitroNodeGroup, bothCRDs, "", true, false, false, ""},
		{"node-group, its own Karpenter", false, plugin.NitroNodeGroup, "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh\n", "", true, true, false, ""},
		{"auto-mode on Auto Mode", true, plugin.NitroAutoMode, "crd/nodepools.karpenter.sh\n", "", false, false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			f.out["get crd nodepools.karpenter.sh"] = []string{c.crds}
			f.out["get nodepool,nodeclass ate-isolated"] = []string{c.objs}
			f.out["get nodes -l karpenter.sh/nodepool=ate-isolated"] = []string{""}
			rc := ownedRC
			if c.auto {
				rc = autoModeRC
			}
			f.out["get runtimeclass isolated-sandbox"] = []string{rc}
			o := testOptions(t, c.auto)
			o.NitroCompute = c.compute
			p, _ := plugin.Get(ID)
			var out bytes.Buffer
			if err := p.Remove(context.Background(), &out, o); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if err := f.has("kubectl delete namespace ate-demo-counter-iso", "kubectl -n ate-system delete daemonset atelet-iso", "kubectl delete -f - --ignore-not-found"); err != nil {
				t.Error(err)
			}
			if !strings.Contains(f.stdin["delete -f - --ignore-not-found"], CredentialProviderConfigMap) {
				t.Errorf("the ConfigMap was not deleted: %q", f.stdin)
			}
			if read := f.has("kubectl get runtimeclass") == nil; read != c.rcRead {
				t.Errorf("RuntimeClass read %v, want %v", read, c.rcRead)
			}
			if del := f.has("kubectl delete runtimeclass isolated-sandbox") == nil; del != c.rcDel {
				t.Errorf("RuntimeClass deleted %v, want %v", del, c.rcDel)
			}
			pool := f.has("kubectl delete nodepool ate-isolated --ignore-not-found --wait=true --timeout=10m",
				"kubectl get nodes -l karpenter.sh/nodepool=ate-isolated", "kubectl delete nodeclass ate-isolated --ignore-not-found --wait=true --timeout=5m") == nil
			if pool != c.nodePool || (!pool && (f.has("kubectl delete nodepool") == nil || f.has("kubectl delete nodeclass") == nil)) {
				t.Errorf("NodePool path %v, want %v:\n%s", pool, c.nodePool, strings.Join(f.log, "\n"))
			}
			// Without both of Auto Mode's kinds the objects are never read:
			// on a cluster with its own Karpenter, nodeclass names no kind.
			if read := f.has("kubectl get nodepool,nodeclass ate-isolated --ignore-not-found -o name") == nil; read != (c.compute == plugin.NitroNodeGroup && c.crds == bothCRDs) {
				t.Errorf("read NodePool and NodeClass ate-isolated %v:\n%s", read, strings.Join(f.log, "\n"))
			}
			if line := "Left as they are: " + c.left + ". " + NodePoolLeftLine + "\n"; c.left != "" && !strings.Contains(out.String(), line) ||
				c.left == "" && strings.Contains(out.String(), "Left as they are") {
				t.Errorf("want the line for %q:\n%s", c.left, out.String())
			}
		})
	}

	// Under node-group, the NodePool and NodeClass left do not stop the
	// rest: the installer's unused RuntimeClass is deleted after the line,
	// and no node is waited for.
	f := newFake(t)
	f.out["get crd nodepools.karpenter.sh"] = []string{bothCRDs}
	f.out["get nodepool,nodeclass ate-isolated"] = []string{"nodepool.karpenter.sh/ate-isolated\nnodeclass.eks.amazonaws.com/ate-isolated\n"}
	f.out["get runtimeclass isolated-sandbox"] = []string{ownedRC}
	f.out["get pods --all-namespaces"] = []string{""}
	o := testOptions(t, true)
	o.NitroCompute = plugin.NitroNodeGroup
	var out bytes.Buffer
	if err := remove(context.Background(), &out, o); err != nil {
		t.Fatal(err)
	}
	if err := f.has("kubectl get crd nodepools.karpenter.sh nodeclasses.eks.amazonaws.com --ignore-not-found -o name",
		"kubectl get nodepool,nodeclass ate-isolated --ignore-not-found -o name", "kubectl get runtimeclass isolated-sandbox",
		"kubectl delete runtimeclass isolated-sandbox --ignore-not-found"); err != nil {
		t.Error(err)
	}
	if !strings.Contains(out.String(), NodePoolLeftLine+"\nDeleting RuntimeClass isolated-sandbox\n") {
		t.Errorf("output:\n%s", out.String())
	}
	for _, l := range f.log {
		if strings.Contains(l, "delete nodepool") || strings.Contains(l, "delete nodeclass") || strings.Contains(l, "get nodes") {
			t.Errorf("node-group remove ran %q", l)
		}
	}
}

// A read that fails is never "none": a denied read of Auto Mode's kinds, of
// NodePool and NodeClass ate-isolated or of the RuntimeClass stops Remove,
// and a denied list of the nitro nodes stops Deploy, each with its error and
// nothing deleted, applied or made after it. Under node-group, Remove reads
// the NodePool and NodeClass only to name the ones it leaves, and a failed
// read of them still stops it, before the RuntimeClass.
func TestFailedReadsStop(t *testing.T) {
	const bothCRDs = "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh\ncustomresourcedefinition.apiextensions.k8s.io/nodeclasses.eks.amazonaws.com\n"
	denied := errors.New("the API server answered 403 Forbidden")
	changes := regexp.MustCompile(`\b(delete|apply|create)\b|^(counter image|build kubectl-ate|golden)`)
	for _, c := range []struct {
		name    string
		auto    bool
		compute string
		fail    string
		deploy  bool
	}{
		{"remove under auto-mode: the NodePool kind", true, plugin.NitroAutoMode, "get crd nodepools.karpenter.sh --ignore-not-found", false},
		{"remove under node-group: Auto Mode's kinds", true, plugin.NitroNodeGroup, "get crd nodepools.karpenter.sh nodeclasses.eks.amazonaws.com", false},
		{"remove under node-group: NodePool and NodeClass ate-isolated", true, plugin.NitroNodeGroup, "get nodepool,nodeclass ate-isolated", false},
		{"remove under node-group: the RuntimeClass", false, plugin.NitroNodeGroup, "get runtimeclass isolated-sandbox", false},
		{"remove under auto-mode without Auto Mode: the RuntimeClass", false, plugin.NitroAutoMode, "get runtimeclass isolated-sandbox", false},
		{"deploy under node-group: the nitro nodes", false, plugin.NitroNodeGroup, "get nodes -l ate.dev/pool=nitro", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			// What each read would answer had it not failed: the kinds, both
			// objects, an owned RuntimeClass no Pod uses (for Deploy none, so
			// it has one to make), and nodes that run the pool.
			rc := ownedRC
			if c.deploy {
				rc = ""
			}
			f.out["get crd nodepools.karpenter.sh"] = []string{bothCRDs}
			f.out["get nodepool,nodeclass ate-isolated"] = []string{"nodepool.karpenter.sh/ate-isolated\nnodeclass.eks.amazonaws.com/ate-isolated\n"}
			f.out["get nodes -l karpenter.sh/nodepool=ate-isolated"] = []string{""}
			f.out["get runtimeclass isolated-sandbox"] = []string{rc}
			f.out["get pods --all-namespaces"] = []string{""}
			f.out["get nodes -l ate.dev/pool=nitro"] = []string{nodeList(nitroNode("n1", "amd64", "2", true))}
			f.out["-n ate-system get ds atelet"] = []string{testAtelet}
			f.fail = map[string]error{c.fail: denied}
			o := testOptions(t, c.auto)
			o.NitroCompute = c.compute
			step := remove
			if c.deploy {
				step = deploy
			}
			if err := step(context.Background(), io.Discard, o); !errors.Is(err, denied) {
				t.Fatalf("want the denied read, got %v:\n%s", err, strings.Join(f.log, "\n"))
			}
			at := -1
			for i, l := range f.log {
				if strings.HasPrefix(l, "kubectl "+c.fail) {
					at = i
					break
				}
			}
			if at < 0 {
				t.Fatalf("never read %q:\n%s", c.fail, strings.Join(f.log, "\n"))
			}
			for _, l := range f.log[at+1:] {
				if changes.MatchString(l) {
					t.Errorf("ran %q after the denied read", l)
				}
			}
			if c.deploy && (len(f.applies) != 0 || len(f.creates) != 0) {
				t.Errorf("applied %q, created %q", f.applies, f.creates)
			}
		})
	}
}
