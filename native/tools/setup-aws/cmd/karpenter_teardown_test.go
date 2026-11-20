package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// karpenterDeployment is fullDeployment made with KARPENTER=on: the cluster
// marked, the system and microvm node groups (no gvisor one), the
// controller role and its association, the node role's instance profile,
// the queue and its rules, a Karpenter instance and launch template, and in
// the cluster the CRDs, the gvisor NodePool and EC2NodeClass of setup-aws,
// a NodePool labeled as setup-aws's that is not a Karpenter pool (as Auto
// Mode's are), one NodePool of someone else, and the gvisor pool's
// NodeClaim. When the gvisor NodePool is deleted, its NodeClaim and
// instance go, as Karpenter does (unless stuck).
type karpenterTD struct {
	f     *tdFake
	kube  *kubeFake
	stuck bool // Karpenter removes no node
}

func karpenterDeployment(t *testing.T) *karpenterTD {
	shortKarpenterTeardown(t)
	f := fullDeployment()
	f.objs["cluster|demo"].tags = own(nodegroups.KarpenterTagKey, "on")
	for _, k := range []string{"nodegroup|gvisor", kLT + "|lt-3"} {
		delete(f.objs, k)
	}
	p := "demo-us-west-2-"
	f.objs["role|"+p+"eks-node"].profiles = []string{p + "eks-node"}
	f.put(&tdObj{kind: "profile", id: p + "eks-node", tags: own()})
	f.put(&tdObj{kind: "role", id: p + "karpenter", inline: []string{"access"}, tags: own()})
	f.put(&tdObj{kind: "assoc", id: "a-karpenter", ns: "kube-system", sa: "karpenter", role: roleARN(p + "karpenter"), tags: own()})
	f.put(&tdObj{kind: "queue", id: "demo-karpenter", tags: own()})
	for _, n := range karpenterRuleNames("demo") {
		f.put(&tdObj{kind: "rule", id: n, attached: []string{karpenterQueueTarget}, tags: own()})
	}
	f.put(&tdObj{kind: kInstance, id: "i-gvisor", vpc: "vpc-1", state: "running",
		tags: own(karpenterPoolTag, "gvisor", karpenterClassTag, "gvisor", "kubernetes.io/cluster/demo", "owned")})
	f.put(&tdObj{kind: kLT, id: "lt-k", name: "karpenter.k8s.aws/1234", tags: own(karpenterClassTag, "gvisor", "karpenter.k8s.aws/cluster", "demo")})
	for _, arn := range append([]string{"arn:aws:sqs:us-west-2:111122223333:demo-karpenter"}, func() []string {
		var out []string
		for _, n := range karpenterRuleNames("demo") {
			out = append(out, ruleARN(n))
		}
		return out
	}()...) {
		f.tagged = append(f.tagged, rgttypes.ResourceTagMapping{ResourceARN: aws.String(arn),
			Tags: []rgttypes.Tag{{Key: aws.String(OwnerTagKey), Value: aws.String(tdOwner)}}})
	}

	kd := &karpenterTD{f: f, kube: newKubeFake(t)}
	k := kd.kube
	k.installCRDs()
	owned := map[string]any{managedByKey: managedByValue}
	k.put(kubeObject{"apiVersion": kindNodePool.APIVersion, "kind": kindNodePool.Kind, "metadata": map[string]any{"name": "gvisor", "labels": owned}})
	k.put(kubeObject{"apiVersion": kindEC2NodeClass.APIVersion, "kind": kindEC2NodeClass.Kind, "metadata": map[string]any{"name": "gvisor", "labels": owned}})
	k.put(kubeObject{"apiVersion": kindNodeClaim.APIVersion, "kind": kindNodeClaim.Kind,
		"metadata": map[string]any{"name": "gvisor-abcde", "labels": map[string]any{karpenterPoolTag: "gvisor"}},
		"spec":     map[string]any{"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": "gvisor"}}})
	k.put(kubeObject{"apiVersion": kindNodePool.APIVersion, "kind": kindNodePool.Kind, "metadata": map[string]any{"name": "substrate-general-purpose", "labels": owned}})
	k.put(kubeObject{"apiVersion": kindEC2NodeClass.APIVersion, "kind": kindEC2NodeClass.Kind, "metadata": map[string]any{"name": "substrate-eks", "labels": owned}})
	k.put(kubeObject{"apiVersion": kindNodePool.APIVersion, "kind": kindNodePool.Kind, "metadata": map[string]any{"name": "theirs"}})
	k.onDelete = func(kind kubeKind, name string) {
		if kind != kindNodePool || kd.stuck {
			return
		}
		delete(k.objs, kindNodeClaim.path("", name+"-abcde"))
		f.mu.Lock()
		defer f.mu.Unlock()
		if o := f.objs[kInstance+"|i-"+name]; o != nil {
			o.state = "terminated"
		}
	}
	f.kube = k.client()
	return kd
}

// cutOffFirst checks that teardown cut the controller off (its role's
// inline policy) before it terminated the instances itself.
func cutOffFirst(t *testing.T, calls []string, terminate string) {
	t.Helper()
	if countCalls(calls, terminate) != 1 {
		t.Errorf("want one %q: %v", terminate, calls)
	}
	before(t, calls, "DeleteRolePolicy demo-us-west-2-karpenter", terminate)
}

// shortKarpenterTeardown bounds the Kubernetes waits and retries of
// teardown.
func shortKarpenterTeardown(t *testing.T) {
	t.Helper()
	a, b, c := karpenterDrainTimeout, karpenterClassTimeout, karpenterKubeRetry
	karpenterDrainTimeout, karpenterClassTimeout, karpenterKubeRetry = 30*time.Millisecond, 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() { karpenterDrainTimeout, karpenterClassTimeout, karpenterKubeRetry = a, b, c })
}

// The whole of a Karpenter deployment goes, in order: the gvisor NodePool,
// which Karpenter drains, before the node groups; the launch template; the
// EC2NodeClass once its NodeClaims are gone; each rule's targets, then the
// rule, then the queue; the association, then the role; the instance
// profile after the cluster. Someone else's NodePool, and those labeled as
// setup-aws's that are not a Karpenter pool, are left to the cluster.
// Nothing is left.
func TestTeardownKarpenter(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	// The final inventory passed; what the fake keeps is others' or a record.
	for _, k := range f.remaining() {
		o := f.objs[k]
		if !strings.Contains(k, "vpc-default") && !strings.Contains(k, "other") && !strings.Contains(k, "demo-2") &&
			!strings.Contains(k, "someone-elses") && o.state != "terminated" && o.state != "deleted" {
			t.Errorf("left %s", k)
		}
	}
	kw := kd.kube.writes()
	want := []string{"DELETE " + kindNodePool.path("", "gvisor"), "DELETE " + kindEC2NodeClass.path("", "gvisor")}
	if !slices.Equal(kw, want) {
		t.Errorf("Kubernetes writes %v, want %v", kw, want)
	}
	if kd.kube.get(kindNodePool, "", "theirs") == nil || kd.kube.get(kindNodePool, "", "substrate-general-purpose") == nil ||
		kd.kube.get(kindEC2NodeClass, "", "substrate-eks") == nil {
		t.Error("deleted a NodePool or EC2NodeClass that is not a Karpenter pool's")
	}
	c := f.calls
	// Karpenter removed its instances: teardown terminated none.
	for _, x := range c {
		if strings.HasPrefix(x, "TerminateInstances") {
			t.Errorf("terminated instances Karpenter removed: %s", x)
		}
	}
	before(t, c, "DeleteLaunchTemplate lt-k", "RemoveTargets demo-karpenter-health")
	for _, n := range karpenterRuleNames("demo") {
		before(t, c, "RemoveTargets "+n, "DeleteRule "+n)
		before(t, c, "DeleteRule "+n, "DeleteQueue demo-karpenter")
	}
	before(t, c, "DeleteQueue demo-karpenter", "DeletePodIdentityAssociation a-karpenter")
	before(t, c, "DeletePodIdentityAssociation a-karpenter", "DeleteRole demo-us-west-2-karpenter")
	before(t, c, "DeleteRole demo-us-west-2-karpenter", "DeleteNodegroup system")
	before(t, c, "DeleteCluster demo", "DeleteInstanceProfile demo-us-west-2-eks-node")
	// The controller keeps its policy until its role goes.
	before(t, c, "DeletePodIdentityAssociation a-karpenter", "DeleteRolePolicy demo-us-west-2-karpenter")
	if !strings.Contains(logs.String(), "Karpenter removed the nodes of NodePools gvisor") {
		t.Errorf("no drain line:\n%s", logs)
	}
	if strings.Contains(logs.String(), "111122223333") {
		t.Error("the log carries the account id")
	}
	phaseOrder(t, logs.String())
}

// When the Kubernetes API does not answer, teardown cuts the controller
// off, terminates Karpenter's instances itself, never touches a finalizer,
// and goes on to delete the rest and the cluster. One series of tries is
// all it spends on the API.
func TestTeardownKarpenterUnreachableCluster(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	f.kube = unreachableKube(t)
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	cutOffFirst(t, f.calls, "TerminateInstances i-gvisor")
	before(t, f.calls, "TerminateInstances i-gvisor", "DeleteLaunchTemplate lt-k")
	before(t, f.calls, "TerminateInstances i-gvisor", "DeleteNodegroup system")
	if !strings.Contains(logs.String(), "did not answer") || !strings.Contains(logs.String(), "failed before") || f.get("cluster", "demo") != nil {
		t.Errorf("cluster left, or no note:\n%s", logs)
	}
	if n := strings.Count(logs.String(), "did not answer"); n != 1 {
		t.Errorf("the API was tried %d times over, want once:\n%s", n, logs)
	}
	if len(kd.kube.calls) != 0 {
		t.Errorf("reached the other API: %v", kd.kube.calls)
	}
}

// A Kubernetes API that answers but refuses (403 on the NodePool list, or
// on the NodePool's delete) is the same: the controller is cut off and the
// instances terminated, with the status named, and teardown does not wait
// for a drain of NodePools it did not delete. A 503 for a moment is tried
// again, and the drain goes as it should.
func TestTeardownKarpenterKubernetesFails(t *testing.T) {
	for _, c := range []struct {
		name, key string
		code      int
		once      bool
	}{
		{"list denied", "GET " + kindNodePool.path("", ""), 403, false},
		{"delete denied", "DELETE " + kindNodePool.path("", "gvisor"), 403, false},
		{"list unavailable once", "GET " + kindNodePool.path("", ""), 503, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			kd := karpenterDeployment(t)
			kd.kube.deny[c.key] = c.code
			if c.once {
				kd.kube.denyLeft[c.key] = 1
			}
			e, _, logs := tdTestEnv(t, kd.f, false, false)
			if err := e.teardown(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, logs)
			}
			if c.once {
				if countCalls(kd.f.calls, "TerminateInstances i-gvisor") != 0 || !strings.Contains(logs.String(), "Karpenter removed the nodes of NodePools gvisor") {
					t.Errorf("a 503 for a moment: calls %v\n%s", kd.f.calls, logs)
				}
				return
			}
			cutOffFirst(t, kd.f.calls, "TerminateInstances i-gvisor")
			if !strings.Contains(logs.String(), "answered 403 Forbidden") || strings.Contains(logs.String(), "did not answer") ||
				strings.Contains(logs.String(), "are left after") || strings.Contains(logs.String(), "Karpenter removed the nodes") {
				t.Errorf("log:\n%s", logs)
			}
			if strings.Contains(logs.String(), "111122223333") {
				t.Error("the log carries the account id")
			}
		})
	}
}

// An instance Karpenter launches after teardown terminated the others (the
// controller still had its policy for a moment) is terminated while
// teardown waits.
func TestTeardownKarpenterTerminatesLateInstances(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	f.kube = unreachableKube(t)
	launched := false
	f.onTerminate = func() {
		if !launched {
			launched = true
			f.put(&tdObj{kind: kInstance, id: "i-late", vpc: "vpc-1", state: "pending", tags: own(karpenterPoolTag, "gvisor", karpenterClassTag, "gvisor")})
		}
	}
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	before(t, f.calls, "TerminateInstances i-gvisor", "TerminateInstances i-late")
	if o := f.objs[kInstance+"|i-late"]; o == nil || o.state != "terminated" || !strings.Contains(logs.String(), "launched while teardown ran") {
		t.Errorf("i-late %+v\n%s", o, logs)
	}
}

// With the Kubernetes API down and no Karpenter instance at that moment,
// teardown still cuts the controller off: it may still run, and an
// instance it launches just before the cut is terminated while teardown
// waits. When Karpenter confirmed the drain, there is no cut-off.
func TestTeardownKarpenterCutsOffWithNoInstance(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	delete(f.objs, kInstance+"|i-gvisor")
	f.kube = unreachableKube(t)
	launched := false
	f.onDeleteRolePolicy = func() {
		// Only the controller's own policy: the Substrate roles go first.
		if !launched && f.calls[len(f.calls)-1] == "DeleteRolePolicy demo-us-west-2-karpenter" {
			launched = true
			f.put(&tdObj{kind: kInstance, id: "i-late", vpc: "vpc-1", state: "pending", tags: own(karpenterPoolTag, "gvisor", karpenterClassTag, "gvisor")})
		}
	}
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	// The cut-off and the late instance's end come in the instance step,
	// before the launch templates, not with the role at the end.
	before(t, f.calls, "DeleteRolePolicy demo-us-west-2-karpenter", "TerminateInstances i-late")
	before(t, f.calls, "TerminateInstances i-late", "DeleteLaunchTemplate lt-k")
	if o := f.objs[kInstance+"|i-late"]; o == nil || o.state != "terminated" {
		t.Errorf("i-late %+v\n%s", o, logs)
	}
	if !strings.Contains(logs.String(), "inline policy access of IAM role demo-us-west-2-karpenter (Karpenter's controller)") {
		t.Errorf("no cut-off in the log:\n%s", logs)
	}

	// A drain Karpenter confirmed needs no cut-off.
	kd = karpenterDeployment(t)
	e, _, logs = tdTestEnv(t, kd.f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "Karpenter removed the nodes") || strings.Contains(logs.String(), "(Karpenter's controller)") {
		t.Errorf("a confirmed drain:\n%s", logs)
	}
}

// When Karpenter does not remove its nodes in time, teardown terminates
// them, and leaves the EC2NodeClasses that still have NodeClaims to the
// cluster: it never strips a finalizer.
func TestTeardownKarpenterDrainTimesOut(t *testing.T) {
	shortKarpenterTeardown(t)
	kd := karpenterDeployment(t)
	kd.stuck = true
	f := kd.f
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	cutOffFirst(t, f.calls, "TerminateInstances i-gvisor")
	for _, w := range kd.kube.writes() {
		if strings.Contains(w, "ec2nodeclasses") {
			t.Errorf("deleted an EC2NodeClass with NodeClaims: %s", w)
		}
	}
	for _, want := range []string{"1 NodeClaims and 1 instances of NodePools gvisor are left", "EC2NodeClass gvisor still has NodeClaims"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no line %q:\n%s", want, logs)
		}
	}
	// An EC2NodeClass whose finalizer Karpenter does not run is left too.
	kd = karpenterDeployment(t)
	kd.kube.finalizing[kindEC2NodeClass.path("", "gvisor")] = 1000
	e, _, logs = tdTestEnv(t, kd.f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "EC2NodeClass gvisor is still there after") {
		t.Errorf("no note of the EC2NodeClass:\n%s", logs)
	}
}

// --keep-cluster keeps all of Karpenter, its Kubernetes objects too, and
// the final check passes: each of its rows is the cluster's.
func TestTeardownKarpenterKeepCluster(t *testing.T) {
	kd := karpenterDeployment(t)
	e, _, logs := tdTestEnv(t, kd.f, false, true)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	for _, c := range kd.f.calls {
		for _, op := range []string{"TerminateInstances", "DeleteQueue", "DeleteRule", "RemoveTargets", "DeleteRole demo-us-west-2-karpenter",
			"DeletePodIdentityAssociation a-karpenter", "DeleteLaunchTemplate lt-k", "DeleteInstanceProfile", "DeleteRolePolicy demo-us-west-2-karpenter"} {
			if strings.HasPrefix(c, op) {
				t.Errorf("--keep-cluster called %s", c)
			}
		}
	}
	if w := kd.kube.writes(); len(w) != 0 {
		t.Errorf("--keep-cluster changed the cluster: %v", w)
	}
}

// --plan deletes nothing, in AWS or the cluster, and names what it would.
func TestTeardownKarpenterPlan(t *testing.T) {
	kd := karpenterDeployment(t)
	e, _, logs := tdTestEnv(t, kd.f, true, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if len(kd.f.calls) != 0 || len(kd.kube.writes()) != 0 {
		t.Errorf("--plan wrote %v, Kubernetes %v", kd.f.calls, kd.kube.writes())
	}
	for _, want := range []string{"plan: would delete Karpenter NodePool gvisor", "plan: would delete Karpenter instances i-gvisor, only if instances are left after the NodePools drain",
		"plan: would delete inline policy access of IAM role demo-us-west-2-karpenter (Karpenter's controller), only if instances are left after the NodePools drain",
		"plan: would delete SQS queue demo-karpenter", "plan: would delete EventBridge rule demo-karpenter-spot (and its targets)",
		"plan: would delete IAM role demo-us-west-2-karpenter", "plan: would delete Karpenter EC2NodeClass gvisor"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no line %q", want)
		}
	}
}

// A queue, rule or controller role of the derived names without the owner
// tag, and an association whose role is someone else's, stop teardown
// before it deletes anything.
func TestTeardownKarpenterRefusesForeign(t *testing.T) {
	for name, edit := range map[string]func(f *tdFake){
		"queue": func(f *tdFake) { f.objs["queue|demo-karpenter"].tags = map[string]string{} },
		"rule":  func(f *tdFake) { f.objs["rule|demo-karpenter-spot"].tags = map[string]string{"team": "x"} },
		"role":  func(f *tdFake) { f.objs["role|demo-us-west-2-karpenter"].tags = map[string]string{} },
		// Without its association, the role is refused for its own name.
		"role without its association": func(f *tdFake) {
			f.objs["role|demo-us-west-2-karpenter"].tags = map[string]string{}
			delete(f.objs, "assoc|a-karpenter")
		},
		"role of the association": func(f *tdFake) {
			f.objs["assoc|a-karpenter"].role = roleARN("someone-elses")
		},
	} {
		t.Run(name, func(t *testing.T) {
			kd := karpenterDeployment(t)
			edit(kd.f)
			e, _, _ := tdTestEnv(t, kd.f, false, false)
			err := e.teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), "teardown stopped before deleting anything") {
				t.Fatalf("got %v", err)
			}
			if len(kd.f.calls) != 0 || len(kd.kube.writes()) != 0 {
				t.Errorf("deleted %v, Kubernetes %v", kd.f.calls, kd.kube.writes())
			}
		})
	}
}

// A denied SQS or EventBridge read stops inventory and teardown; it is never
// "not there".
func TestTeardownKarpenterDeniedReadStops(t *testing.T) {
	for _, op := range []string{"GetQueueUrl", "DescribeRule"} {
		kd := karpenterDeployment(t)
		kd.f.deny[op] = true
		e, _, _ := tdTestEnv(t, kd.f, false, false)
		if err := e.teardown(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") || len(kd.f.calls) != 0 {
			t.Errorf("%s denied: %v, calls %v", op, err, kd.f.calls)
		}
	}
}

// Inventory lists Karpenter's resources, each as the cluster's: the queue
// and the rules (read from SQS and EventBridge, not the tagging API), the
// role, the association, the instance profile, the instance, the launch
// template, and the gvisor NodePool and EC2NodeClass, not the other
// objects labeled as setup-aws's. A Kubernetes API that fails leaves the
// last two out, with a note, and no error.
func TestInventoryKarpenter(t *testing.T) {
	kd := karpenterDeployment(t)
	e, _, _ := tdTestEnv(t, kd.f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Kind+" "+r.ID] = r.Cluster
		if strings.HasPrefix(r.Kind, "sqs") || strings.HasPrefix(r.Kind, "events") {
			t.Errorf("a tagging-API row %s %s", r.Kind, r.ID)
		}
	}
	want := []string{"SQS queue demo-karpenter", "IAM role demo-us-west-2-karpenter", "IAM instance profile demo-us-west-2-eks-node",
		"Pod Identity association kube-system/karpenter (a-karpenter)", "EC2 instance i-gvisor",
		"launch template karpenter.k8s.aws/1234 (lt-k)", "Karpenter NodePool gvisor", "Karpenter EC2NodeClass gvisor"}
	for _, n := range karpenterRuleNames("demo") {
		want = append(want, "EventBridge rule "+n)
	}
	for _, w := range want {
		if cluster, ok := have[w]; !ok || !cluster {
			t.Errorf("row %q: listed %v, cluster %v", w, ok, cluster)
		}
	}
	for _, n := range []string{"Karpenter NodePool substrate-general-purpose", "Karpenter NodePool theirs", "Karpenter EC2NodeClass substrate-eks"} {
		if _, ok := have[n]; ok {
			t.Errorf("row %q", n)
		}
	}
	kd.f.kube = unreachableKube(t)
	e, _, logs := tdTestEnv(t, kd.f, false, false)
	rows, err = e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Kind, "Karpenter ") {
			t.Errorf("row %s %s from an API that does not answer", r.Kind, r.ID)
		}
	}
	if !strings.Contains(logs.String(), "inventory does not list its NodePools and EC2NodeClasses") {
		t.Errorf("no note:\n%s", logs)
	}
}

// Once the cluster is gone, the tagging API's entries of the queue and the
// rules still lead teardown to them.
func TestTeardownKarpenterAfterTheCluster(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	for k, o := range f.objs {
		if o.kind == "cluster" || o.kind == "nodegroup" || o.kind == "assoc" || o.tags["eks:nodegroup-name"] != "" || o.kind == kInstance {
			delete(f.objs, k)
		}
	}
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if f.get("queue", "demo-karpenter") != nil || f.get("rule", "demo-karpenter-spot") != nil || f.get("role", "demo-us-west-2-karpenter") != nil {
		t.Errorf("left %v", f.remaining())
	}
	if len(kd.kube.calls) != 0 {
		t.Errorf("read the Kubernetes API of a cluster that is gone: %v", kd.kube.calls)
	}
}

// Once the cluster, the queue and the rules are gone, a Karpenter instance
// and launch template that are still there (a teardown that stopped part
// way) are found by their tags alone, from the inventory, and deleted.
func TestTeardownKarpenterInstancesOutliveTheRest(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	for k, o := range f.objs {
		if o.kind == "cluster" || o.kind == "nodegroup" || o.kind == "assoc" || o.kind == "queue" || o.kind == "rule" ||
			o.tags["eks:nodegroup-name"] != "" || (o.kind == "role" && o.id == "demo-us-west-2-karpenter") {
			delete(f.objs, k)
		}
	}
	f.tagged = nil
	f.objs[kInstance+"|i-gvisor"].vpc = ""
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if countCalls(f.calls, "TerminateInstances i-gvisor") != 1 || countCalls(f.calls, "DeleteLaunchTemplate lt-k") != 1 {
		t.Errorf("calls %v", f.calls)
	}
}

// A cluster made without KARPENTER=on, with an owner-tagged queue of an
// older deployment of the same name left over: teardown deletes the queue
// and the rules (AWS resources of this deployment's names and owner tag),
// and never reads or writes the cluster's Kubernetes objects, Auto Mode's
// NodePools among them.
func TestTeardownLeftoverQueueOfAnUnmarkedCluster(t *testing.T) {
	kd := karpenterDeployment(t)
	f := kd.f
	f.objs["cluster|demo"].tags = own()
	delete(f.objs, "assoc|a-karpenter")
	delete(f.objs, kInstance+"|i-gvisor")
	delete(f.objs, kLT+"|lt-k")
	e, _, logs := tdTestEnv(t, f, false, false)
	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if len(kd.kube.calls) != 0 {
		t.Errorf("reached the Kubernetes API of a cluster without the marker: %v", kd.kube.calls)
	}
	if f.get("queue", "demo-karpenter") != nil || f.get("rule", "demo-karpenter-spot") != nil {
		t.Errorf("left %v", f.remaining())
	}
}

// An EKS Auto Mode instance or launch template that carries a Karpenter tag
// next to the owner tag is EKS's: teardown leaves it to EKS, and on a
// deployment without KARPENTER=on does not even look for Karpenter's.
func TestTeardownLeavesAutoModeInstances(t *testing.T) {
	f := fullDeployment()
	f.put(&tdObj{kind: kInstance, id: "i-auto", vpc: "vpc-1", state: "running",
		tags: own(karpenterPoolTag, "general-purpose", autoModePoolTag, "general-purpose")})
	f.put(&tdObj{kind: kLT, id: "lt-auto", name: "eks-auto", tags: own(karpenterClassTag, "default", autoModePoolTag, "general-purpose")})
	e, _, logs := tdTestEnv(t, f, false, false)
	if _, err := e.inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.karpenterEC2 || e.karpenterAWS() {
		t.Errorf("Auto Mode's instance counts as Karpenter's")
	}
	ts, _, err := e.stepKarpenterInstances(context.Background())
	lts, _, lerr := e.stepKarpenterLaunchTemplates(context.Background())
	if err != nil || lerr != nil || len(ts)+len(lts) != 0 {
		t.Errorf("targets %v %v (%v, %v)", ts, lts, err, lerr)
	}
	// The filter itself, past the gate: Auto Mode's instance is not listed.
	insts, err := e.karpenterInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range insts {
		if it.id == "i-auto" {
			t.Errorf("karpenterInstances lists Auto Mode's instance")
		}
	}
	// Even on a Karpenter deployment, the Auto Mode ones are not its.
	kd := karpenterDeployment(t)
	kd.f.put(&tdObj{kind: kInstance, id: "i-auto", state: "running",
		tags: own(karpenterPoolTag, "general-purpose", autoModePoolTag, "general-purpose")})
	kd.f.kube = unreachableKube(t)
	e, _, logs = tdTestEnv(t, kd.f, false, false)
	// It fails at the end: the fake keeps EKS's instance, which inventory
	// lists. What matters is what it terminated.
	_ = e.teardown(context.Background())
	gvisor := false
	for _, c := range kd.f.calls {
		if strings.HasPrefix(c, "TerminateInstances") && strings.Contains(c, "i-auto") {
			t.Errorf("terminated Auto Mode's instance: %s\n%s", c, logs)
		}
		gvisor = gvisor || strings.HasPrefix(c, "TerminateInstances") && strings.Contains(c, "i-gvisor")
	}
	if !gvisor {
		t.Errorf("teardown did not terminate Karpenter's instance i-gvisor: %v\n%s", kd.f.calls, logs)
	}
}
