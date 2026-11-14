package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// The shapes Auto Mode gives its built-in objects.
const (
	testDefaultClass = `{"apiVersion":"eks.amazonaws.com/v1","kind":"NodeClass",
 "metadata":{"name":"default","labels":{"app.kubernetes.io/managed-by":"eks"},"finalizers":["eks.amazonaws.com/termination"]},
 "spec":{"role":"c1-eks-auto-node","subnetSelectorTerms":[{"id":"subnet-1"}],"securityGroupSelectorTerms":[{"id":"sg-1"}],"snatPolicy":"Random"},
 "status":{"conditions":[]}}`
	testGeneralPurpose = `{"apiVersion":"karpenter.sh/v1","kind":"NodePool","metadata":{"name":"general-purpose","uid":"u1"},
 "spec":{"disruption":{"consolidationPolicy":"Balanced","consolidateAfter":"30s","budgets":[{"nodes":"10%"}]},
  "template":{"metadata":{},"spec":{"expireAfter":"336h","nodeClassRef":{"group":"eks.amazonaws.com","kind":"NodeClass","name":"default"},
   "requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64"]}]}}},"status":{}}`
	testSystemPool = `{"apiVersion":"karpenter.sh/v1","kind":"NodePool","metadata":{"name":"system"},
 "spec":{"template":{"metadata":{},"spec":{"nodeClassRef":{"group":"eks.amazonaws.com","kind":"NodeClass","name":"default"},
  "taints":[{"effect":"NoSchedule","key":"CriticalAddonsOnly"}],
  "requirements":[{"key":"kubernetes.io/arch","operator":"In","values":["amd64","arm64"]}]}}}}`
	testClassReady   = `{"status":{"conditions":[{"type":"SubnetsReady","status":"True"},{"type":"Ready","status":"True"}]}}`
	testClassInvalid = `{"status":{"conditions":[{"type":"SubnetsReady","status":"True"},
  {"type":"ValidationSucceeded","status":"False","reason":"CreateLaunchTemplateAuthCheckFailed","message":"Controller isn't authorized to call ec2:CreateLaunchTemplate"},
  {"type":"Ready","status":"False","reason":"UnhealthyDependents","message":"ValidationSucceeded=False"}]}}`
)

// autoKube is runCommand for the step: kubectl answers by argv prefix (one
// answer per call, the last repeated), and every argv and stdin is kept.
type autoKube struct {
	answers map[string][]string
	argv    []string
	applied []string
}

func (k *autoKube) run(_ context.Context, _ io.Writer, c command) error {
	if c.Argv[0] != "kubectl" || !slices.Contains(c.Env, "KUBECONFIG=/kc") {
		return fmt.Errorf("not the installer's kubectl: %v %v", c.Argv, c.Env)
	}
	line := strings.Join(c.Argv[1:], " ")
	k.argv = append(k.argv, line)
	if line == "apply -f -" {
		k.applied = append(k.applied, string(c.Stdin))
	}
	for p, a := range k.answers {
		if strings.HasPrefix(line, p) && len(a) > 0 {
			io.WriteString(c.Stdout, a[0])
			if len(a) > 1 {
				k.answers[p] = a[1:]
			}
		}
	}
	return nil
}

func autoFixture(t *testing.T, claims string) (*autoKube, Options) {
	t.Helper()
	k := &autoKube{answers: map[string][]string{
		"get nodeclass default":          {testDefaultClass},
		"get nodeclass " + AutoNodeClass: {`{"status":{}}`, testClassReady},
		"get nodepool general-purpose":   {testGeneralPurpose},
		"get nodepool system":            {testSystemPool},
		"get nodeclaims -l karpenter.sh/nodepool in (general-purpose,system) -o name": {claims},
	}}
	prevRun, prevPoll := runCommand, autoNodesPoll
	t.Cleanup(func() { runCommand, autoNodesPoll = prevRun, prevPoll })
	runCommand, autoNodesPoll = k.run, 0
	o := opts()
	o.AutoMode, o.clusterOwned = true, true
	o.volumeTags = map[string]string{ownerTagKey: "demo.us-west-2", "auto-delete": "no", "eks:team": "x", "karpenter.sh/x": "y"}
	return k, o
}

// Both directions: the step runs only on an Auto Mode cluster this
// installer created, and says why it does not elsewhere.
func TestAutoNodesRunOnlyOnOwnedAutoMode(t *testing.T) {
	for _, tc := range []struct {
		auto, owned bool
		why         string
	}{{false, true, "the cluster does not run EKS Auto Mode"}, {true, false, "setup-aws did not create the cluster"}} {
		k, o := autoFixture(t, "")
		o.AutoMode, o.clusterOwned = tc.auto, tc.owned
		var out bytes.Buffer
		if err := applyAutoNodes(context.Background(), &out, o, "/kc"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), AutoNodesSkipLine+tc.why) || len(k.argv) != 0 {
			t.Errorf("auto=%v owned=%v: ran %q, said %q", tc.auto, tc.owned, k.argv, out.String())
		}
	}
	k, o := autoFixture(t, "")
	if err := applyAutoNodes(context.Background(), io.Discard, o, "/kc"); err != nil || len(k.applied) != 3 {
		t.Errorf("owned Auto Mode: err %v, %d applies", err, len(k.applied))
	}
}

// The tagged NodeClass, its Ready wait, then the two weighted NodePools, in
// that order; built-in nodes are replaced once: a rerun with none left
// deletes nothing.
func TestAutoNodesOrderAndOneReplacement(t *testing.T) {
	k, o := autoFixture(t, "nodeclaim.karpenter.sh/general-purpose-a\nnodeclaim.karpenter.sh/general-purpose-b\n")
	var out bytes.Buffer
	if err := applyAutoNodes(context.Background(), &out, o, "/kc"); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	want := []string{
		"get nodeclass default -o json", "apply -f -", "get nodeclass substrate-eks -o json", "get nodeclass substrate-eks -o json",
		"get nodepool general-purpose -o json", "apply -f -", "get nodepool system -o json", "apply -f -",
		"get nodeclaims -l karpenter.sh/nodepool in (general-purpose,system) -o name",
		"delete nodeclaim.karpenter.sh/general-purpose-a nodeclaim.karpenter.sh/general-purpose-b --wait=false",
		"wait --for=delete nodeclaim.karpenter.sh/general-purpose-a nodeclaim.karpenter.sh/general-purpose-b --timeout=15m",
	}
	if !slices.Equal(k.argv, want) {
		t.Errorf("argv:\n%s\nwant\n%s", strings.Join(k.argv, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "Replacing 2 node(s)") || !strings.Contains(out.String(), PrereqLine+"NodeClass substrate-eks") {
		t.Errorf("output:\n%s", out.String())
	}

	k, o = autoFixture(t, "")
	out.Reset()
	if err := applyAutoNodes(context.Background(), &out, o, "/kc"); err != nil {
		t.Fatal(err)
	}
	for _, a := range k.argv {
		if strings.HasPrefix(a, "delete") || strings.HasPrefix(a, "wait") {
			t.Errorf("rerun with no built-in node ran %q", a)
		}
	}
	if !strings.Contains(out.String(), "No node of the built-in NodePools is left to replace") {
		t.Errorf("output:\n%s", out.String())
	}
}

// A NodeClass that never validates stops the step with its reason and the
// fix, before any NodePool or replacement.
func TestAutoNodesStopOnAnInvalidNodeClass(t *testing.T) {
	k, o := autoFixture(t, "nodeclaim.karpenter.sh/general-purpose-a\n")
	k.answers["get nodeclass "+AutoNodeClass] = []string{testClassInvalid}
	err := applyAutoNodes(context.Background(), io.Discard, o, "/kc")
	if err == nil || !strings.Contains(err.Error(), "isn't authorized to call ec2:CreateLaunchTemplate") || !strings.Contains(err.Error(), "setup-aws create cluster") {
		t.Fatalf("err = %v", err)
	}
	if len(k.applied) != 1 || slices.ContainsFunc(k.argv, func(a string) bool { return strings.HasPrefix(a, "delete") }) {
		t.Errorf("after an invalid NodeClass: %d applies, argv %q", len(k.applied), k.argv)
	}
}

func TestNodeClassReady(t *testing.T) {
	if ok, _, err := NodeClassReady([]byte(testClassReady)); !ok || err != nil {
		t.Errorf("Ready: %v %v", ok, err)
	}
	ok, why, err := NodeClassReady([]byte(testClassInvalid))
	if ok || err != nil || why != "ValidationSucceeded: CreateLaunchTemplateAuthCheckFailed: Controller isn't authorized to call ec2:CreateLaunchTemplate" {
		t.Errorf("invalid: %v %q %v", ok, why, err)
	}
	if ok, _, _ := NodeClassReady([]byte(`{}`)); ok {
		t.Error("no status counted as Ready")
	}
}

func decodeObj(t *testing.T, b []byte) kobj {
	t.Helper()
	var m kobj
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Both directions: the copy has default's role, subnets and security groups
// and the owner tags without reserved keys; anything but a NodeClass, or
// non-map tags, is refused.
func TestTaggedNodeClass(t *testing.T) {
	src := strings.Replace(testDefaultClass, `"snatPolicy":"Random"`, `"snatPolicy":"Random","tags":{"team":"a","kubernetes.io/cluster/c1":"owned"}`, 1)
	b, err := TaggedNodeClass([]byte(src), map[string]string{ownerTagKey: "demo.us-west-2", "auto-delete": "no", "eks:x": "1", "karpenter.sh/y": "2"})
	if err != nil {
		t.Fatal(err)
	}
	nc := decodeObj(t, b)
	tags, _ := json.Marshal(nc["spec"].(kobj)["tags"])
	if want := `{"agent-substrate-on-eks/deployment":"demo.us-west-2","auto-delete":"no","team":"a"}`; string(tags) != want {
		t.Errorf("tags = %s, want %s", tags, want)
	}
	meta, _ := json.Marshal(nc["metadata"])
	if string(meta) != `{"labels":{"app.kubernetes.io/managed-by":"substrate-eks"},"name":"substrate-eks"}` || nc["status"] != nil ||
		!strings.Contains(string(b), `"role":"c1-eks-auto-node"`) || !strings.Contains(string(b), `"subnet-1"`) || !strings.Contains(string(b), `"sg-1"`) {
		t.Errorf("copy:\n%s", b)
	}
	for _, bad := range []string{testGeneralPurpose, `{"kind":"NodeClass"}`, `{"kind":"NodeClass","spec":{"tags":"x"}}`, "not json"} {
		if _, err := TaggedNodeClass([]byte(bad), nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Both directions: each copy keeps the built-in's requirements, taints and
// disruption, names substrate-eks with weight 10 and adds no
// ate.dev/workloadType; a NodePool without a NodeClass reference is refused.
func TestWeightedNodePool(t *testing.T) {
	for _, tc := range []struct{ src, name, keep string }{
		{testGeneralPurpose, "substrate-general-purpose", `"consolidationPolicy":"Balanced"`},
		{testSystemPool, "substrate-system", `"taints":[{"effect":"NoSchedule","key":"CriticalAddonsOnly"}]`},
	} {
		b, err := WeightedNodePool([]byte(tc.src), tc.name)
		if err != nil {
			t.Fatal(err)
		}
		np := decodeObj(t, b)
		spec := np["spec"].(kobj)
		ref := spec["template"].(kobj)["spec"].(kobj)["nodeClassRef"].(kobj)
		if np["metadata"].(kobj)["name"] != tc.name || spec["weight"] != float64(10) || ref["name"] != AutoNodeClass || ref["group"] != "eks.amazonaws.com" ||
			!strings.Contains(string(b), tc.keep) || !strings.Contains(string(b), `"kubernetes.io/arch"`) || np["status"] != nil {
			t.Errorf("%s:\n%s", tc.name, b)
		}
		if strings.Contains(string(b), "ate.dev/workloadType") {
			t.Errorf("%s draws the cordoned control plane:\n%s", tc.name, b)
		}
	}
	noRef := strings.Replace(testSystemPool, `"nodeClassRef":{"group":"eks.amazonaws.com","kind":"NodeClass","name":"default"},`, "", 1)
	for _, bad := range []string{testDefaultClass, noRef, `{"kind":"NodePool","spec":{}}`} {
		if _, err := WeightedNodePool([]byte(bad), "x"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// install runs the step before the StorageClass and ate-setup.
func TestInstallRunsAutoNodesFirst(t *testing.T) {
	k := &fakeKube{}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	o.Database, o.clusterOwned, o.volumeTags = DatabaseBundled, true, testTags
	ran := -1
	autoNodes = func(_ context.Context, _ io.Writer, got Options, kc string) error {
		if !got.AutoMode || !got.clusterOwned || kc != "/k" {
			t.Errorf("step options auto=%v owned=%v kc=%q", got.AutoMode, got.clusterOwned, kc)
		}
		ran = len(k.cmds)
		return nil
	}
	if err := install(context.Background(), io.Discard, o, "demo", "/k", tree); err != nil {
		t.Fatal(err)
	}
	if _, at := k.appliedClass(t); ran < 0 || at < ran {
		t.Errorf("step after %d commands, StorageClass applied at %d", ran, at)
	}
	autoNodes = func(context.Context, io.Writer, Options, string) error { return fmt.Errorf("boom") }
	k.cmds = nil
	if err := install(context.Background(), io.Discard, o, "demo", "/k", tree); err == nil || len(k.cmds) != 0 {
		t.Errorf("a failed step: err %v, then ran %d commands", err, len(k.cmds))
	}
}
