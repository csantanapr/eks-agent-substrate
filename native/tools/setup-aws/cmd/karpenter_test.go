package cmd

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

const (
	karpenterRoleARN = "arn:aws:iam::111122223333:role/demo-us-west-2-karpenter"
	queueARN         = "arn:aws:sqs:us-west-2:111122223333:demo-karpenter"
)

// withKarpenter is KARPENTER=on, on a cluster without Auto Mode.
func withKarpenter(c *clusterConfig) { c.AutoMode, c.Karpenter = "", nodegroups.KarpenterOn }

// karpenterFake is an account whose cluster has a Kubernetes API, and
// whose managed node groups get the node role's access entry from EKS.
func karpenterFake(t *testing.T) *clusterFake {
	f := newClusterFake()
	f.objects = newKubeFake(t)
	f.eksNodeEntry = true
	return f
}

// karpenterCreates is what a fresh account gets with KARPENTER=on and the
// default NODE_GROUPS: freshCreates without Auto Mode's node role and the
// gvisor node group and template (the microvm group stays), with the node
// role's instance profile, the controller role and its association, and
// the queue. (The rules are made by PutRule; EKS makes the node role's
// access entry with the managed node groups.)
func karpenterCreates() map[string]int {
	m := maps.Clone(freshCreates)
	for _, k := range []string{"CreateRole demo-us-west-2-eks-auto-node", "CreateNodegroup gvisor", "CreateLaunchTemplate demo-gvisor-nodes"} {
		delete(m, k)
	}
	for _, k := range []string{"CreateInstanceProfile " + nodeProfile, "CreateRole demo-us-west-2-karpenter",
		"CreatePodIdentityAssociation kube-system/karpenter", "CreateQueue demo-karpenter"} {
		m[k] = 1
	}
	return m
}

// withOverlay is KARPENTER_NODE_OVERLAY=on, with KARPENTER=on: the microvm
// and nitro groups are Karpenter NodePools too, each with a NodeOverlay.
func withOverlay(c *clusterConfig) { withKarpenter(c); c.KarpenterNodeOverlay = nodegroups.KarpenterOn }

// amiOf is the AMI ID pool's EC2NodeClass names.
func amiOf(f *clusterFake, pool string) any {
	return f.objects.get(kindEC2NodeClass, "", pool).field("spec", "amiSelectorTerms").([]any)[0].(map[string]any)["id"]
}

// shortKarpenterWaits bounds the waits of the Kubernetes phase for a test.
func shortKarpenterWaits(t *testing.T) {
	t.Helper()
	a, b, c := crdWaitTimeout, karpenterReadyTimeout, poolReadyTimeout
	crdWaitTimeout, karpenterReadyTimeout, poolReadyTimeout = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { crdWaitTimeout, karpenterReadyTimeout, poolReadyTimeout = a, b, c })
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A fresh account with KARPENTER=on: the system and microvm node groups and
// no gvisor one; the cluster marked; the controller role with the reference
// policy and its association; the queue and rules; then the chart, and the
// gvisor pool's EC2NodeClass and NodePool, in the cluster, in order. The
// microvm group keeps its node group: its workers request ate.dev/kvm,
// which Karpenter cannot know without a NodeOverlay.
func TestCreateClusterKarpenter(t *testing.T) {
	f := karpenterFake(t)
	logs := captureLog(t)
	r := clusterTestRun(t, f, false, withKarpenter)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got, want := creates(f.writes), karpenterCreates(); !maps.Equal(got, want) {
		t.Errorf("creates:\n got %v\nwant %v", got, want)
	}
	if want := len(karpenterCreates()) + len(karpenterRules) + 20; len(r.creates) != want {
		t.Errorf("logged %d creates, want %d: %v", len(r.creates), want, r.creates)
	}
	if f.clusterIn.Tags[nodegroups.KarpenterTagKey] != "on" || f.clusterIn.Tags[OwnerTagKey] != "demo.us-west-2" {
		t.Errorf("cluster tags %v", f.clusterIn.Tags)
	}
	if got := sortedKeysOf(f.ngIn); !slices.Equal(got, []string{"microvm", "system"}) {
		t.Errorf("node groups %v, want microvm and system", got)
	}
	if lt := f.ngIn["microvm"].LaunchTemplate; lt == nil || f.lts[aws.ToString(lt.Id)] == nil {
		t.Errorf("the microvm node group has no launch template of its own: %+v", f.ngIn["microvm"])
	}
	own := map[string]string{OwnerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no", CreatedByTagKey: CreatedByTagValue}

	// The controller role: EKS Pod Identity for this cluster, the reference
	// policy, and the association.
	role := f.roles["demo-us-west-2-karpenter"]
	if role == nil || !sameJSON(role.trust, r.e.trustPolicy()) || !maps.Equal(role.tags, own) || len(role.attached) != 0 ||
		role.inline["access"] != karpenterPolicy(r.e.names, nodeRoleARN) {
		t.Fatalf("controller role %+v", role)
	}
	if len(f.assocs) != 1 {
		t.Fatalf("associations %v", f.assocs)
	}
	for _, a := range f.assocs {
		if aws.ToString(a.Namespace)+"/"+aws.ToString(a.ServiceAccount) != "kube-system/karpenter" || aws.ToString(a.RoleArn) != karpenterRoleARN || !maps.Equal(a.Tags, own) {
			t.Errorf("association %+v", a)
		}
	}
	// The node role's instance profile, and the EC2_LINUX access entry
	// EKS made for the managed groups' role, kept as it is.
	if p := f.profiles[nodeProfile]; p == nil || !slices.Equal(p.roles, []string{"demo-us-west-2-eks-node"}) || !maps.Equal(p.tags, own) {
		t.Errorf("instance profile %+v", p)
	}
	if f.entryTypes[nodeRoleARN] != accessTypeNode || f.entries[nodeRoleARN] != nil {
		t.Errorf("node access entry %q %v", f.entryTypes[nodeRoleARN], f.entries[nodeRoleARN])
	}

	// The queue and the rules that send to it.
	q := f.queues["demo-karpenter"]
	if q == nil || !maps.Equal(q.attrs, karpenterQueueAttributes(r.e.names)) || !maps.Equal(q.tags, own) {
		t.Fatalf("queue %+v", q)
	}
	if got := sortedKeysOf(f.rules); len(got) != 5 {
		t.Errorf("rules %v", got)
	}
	for _, k := range karpenterRules {
		rule := f.rules["demo-karpenter-"+k.suffix]
		if rule == nil || !sameJSON(rule.pattern, `{"source":["`+k.source+`"],"detail-type":["`+k.detailType+`"]}`) ||
			rule.state != ebtypes.RuleStateEnabled || !maps.Equal(rule.tags, own) ||
			!maps.Equal(rule.targets, map[string]string{karpenterQueueTarget: queueARN}) {
			t.Errorf("rule %s: %+v", k.suffix, rule)
		}
	}

	// The Kubernetes objects: 18 of the chart and two of the gvisor pool,
	// each labeled, the CRDs first and the pool last.
	kf := f.objects
	if len(kf.objs) != 20 {
		t.Errorf("%d objects in the cluster, want 20", len(kf.objs))
	}
	for p, o := range kf.objs {
		if !ownedObject(o) {
			t.Errorf("%s is not labeled %s=%s", p, managedByKey, managedByValue)
		}
	}
	w := kf.writes()
	last := func(prefix string) int {
		i := -1
		for j, c := range w {
			if strings.HasPrefix(c, "PATCH "+prefix) {
				i = j
			}
		}
		return i
	}
	first := func(prefix string) int {
		return slices.IndexFunc(w, func(c string) bool { return strings.HasPrefix(c, "PATCH "+prefix) })
	}
	if len(w) != 20 || last(kindCRD.path("", "")) != 4 || last(kindDeployment.path("kube-system", "")) != 17 ||
		first(kindEC2NodeClass.path("", "")) != 18 || first(kindEC2NodeClass.path("", "")) > first(kindNodePool.path("", "")) {
		t.Errorf("Kubernetes writes, in order:\n  %s", strings.Join(w, "\n  "))
	}
	dep := kf.get(kindDeployment, "kube-system", "karpenter")
	if dep.field("spec", "replicas") != 2.0 {
		t.Errorf("controller replicas %v", dep.field("spec", "replicas"))
	}
	env := map[string]string{}
	for _, c := range dep.field("spec", "template", "spec", "containers").([]any) {
		for _, e := range c.(map[string]any)["env"].([]any) {
			em := e.(map[string]any)
			if v, ok := em["value"].(string); ok {
				env[em["name"].(string)] = v
			}
		}
	}
	if env["CLUSTER_NAME"] != "demo" || env["CLUSTER_ENDPOINT"] != "https://FAKE.gr7.us-west-2.eks.amazonaws.com" ||
		env["INTERRUPTION_QUEUE"] != "demo-karpenter" || env["EKS_CONTROL_PLANE"] != "true" || env["AWS_REGION"] != "us-west-2" ||
		env["AWS_EC2_METADATA_DISABLED"] != "true" {
		t.Errorf("controller env %v", env)
	}

	// The gvisor pool's EC2NodeClass and NodePool, and none for microvm.
	if kf.get(kindEC2NodeClass, "", "microvm") != nil || kf.get(kindNodePool, "", "microvm") != nil {
		t.Error("a microvm EC2NodeClass or NodePool")
	}
	sg := aws.ToString(f.cluster.ResourcesVpcConfig.ClusterSecurityGroupId)
	var subnets []any
	for _, s := range f.clusterIn.ResourcesVpcConfig.SubnetIds {
		subnets = append(subnets, map[string]any{"id": s})
	}
	nodeTags := map[string]any{OwnerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no"}
	for _, pool := range []string{"gvisor"} {
		nc := kf.get(kindEC2NodeClass, "", pool)
		if nc == nil {
			t.Fatalf("no EC2NodeClass %s", pool)
		}
		spec := nc["spec"].(map[string]any)
		want := map[string]any{
			"amiFamily": "AL2023", "amiSelectorTerms": []any{map[string]any{"id": eksAMIx86}}, "instanceProfile": nodeProfile,
			"subnetSelectorTerms": subnets, "securityGroupSelectorTerms": []any{map[string]any{"id": sg}},
			"metadataOptions": map[string]any{"httpEndpoint": "enabled", "httpProtocolIPv6": "disabled", "httpPutResponseHopLimit": 1.0, "httpTokens": "required"},
			"blockDeviceMappings": []any{map[string]any{"deviceName": "/dev/xvda", "rootVolume": true, "ebs": map[string]any{
				"volumeSize": "200Gi", "volumeType": "gp3", "encrypted": true, "deleteOnTermination": true}}},
			"tags": nodeTags,
		}
		if asJSON(t, spec) != asJSON(t, want) {
			t.Errorf("EC2NodeClass %s:\n got %s\nwant %s", pool, asJSON(t, spec), asJSON(t, want))
		}
		np := kf.get(kindNodePool, "", pool)
		wantPool := map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"ate.dev/pool": pool}},
				"spec": map[string]any{
					"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": pool},
					"requirements": []any{
						map[string]any{"key": "node.kubernetes.io/instance-type", "operator": "In", "values": []any{"m8i.2xlarge"}},
						map[string]any{"key": "karpenter.sh/capacity-type", "operator": "In", "values": []any{"on-demand"}},
						map[string]any{"key": "kubernetes.io/arch", "operator": "In", "values": []any{"amd64"}},
						map[string]any{"key": "kubernetes.io/os", "operator": "In", "values": []any{"linux"}},
					},
					"taints":                 []any{map[string]any{"key": "ate.dev/sandboxClass", "value": pool, "effect": "NoSchedule"}},
					"expireAfter":            "Never",
					"terminationGracePeriod": "15m",
				},
			},
			"limits":     map[string]any{"nodes": "2", "cpu": "16"},
			"disruption": map[string]any{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "5m"},
		}
		if asJSON(t, np["spec"]) != asJSON(t, wantPool) {
			t.Errorf("NodePool %s:\n got %s\nwant %s", pool, asJSON(t, np["spec"]), asJSON(t, wantPool))
		}
	}
	list := phases.ClusterFor(nodegroups.Managed, true)
	for i := range list {
		if !strings.Contains(logs.String(), phases.Line(list, i)) {
			t.Errorf("no line %q", phases.Line(list, i))
		}
	}
	if strings.Contains(logs.String(), "111122223333") {
		t.Errorf("the log carries the account id:\n%s", logs)
	}
}

// A rerun writes nothing, in AWS or the cluster, and reads no SSM
// parameter: each pool keeps the AMI of its EC2NodeClass.
func TestCreateClusterKarpenterSecondRunWritesNothing(t *testing.T) {
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes, f.objects.calls = nil, nil
	f.deny["GetParameter"] = "AccessDenied"
	r := clusterTestRun(t, f, false, withKarpenter)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(r.creates) != 0 || len(r.changes) != 0 || len(f.objects.writes()) != 0 {
		t.Errorf("rerun: writes %v, creates %v, changes %v, Kubernetes writes %v", f.writes, r.creates, r.changes, f.objects.writes())
	}
	if !r.seeded["gvisor"] || len(r.seeded) != 1 {
		t.Errorf("seeded %v", r.seeded)
	}
}

// A pool keeps the AMI its EC2NodeClass was made with, whatever the SSM
// parameter or NODE_AMI_ID name later; a pool added later (microvm, with
// KARPENTER_NODE_OVERLAY=on) gets the AMI of the day. SSM is read only for a
// pool still to make.
func TestCreateClusterKarpenterKeepsEachPoolsAMI(t *testing.T) {
	f := karpenterFake(t)
	gvisorOnly := func(c *clusterConfig) { c.NodeGroups = "system,gvisor" }
	if err := clusterTestRun(t, f, false, withOverlay, gvisorOnly).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A new AMI release.
	const newer = "ami-0e1e0000000000099"
	im := f.images[eksAMIx86]
	im.ImageId = aws.String(newer)
	f.images[newer] = im
	f.params["/aws/service/eks/optimized-ami/1.37/amazon-linux-2023/x86_64/standard/recommended/image_id"] = newer
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withOverlay).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if amiOf(f, "gvisor") != eksAMIx86 || amiOf(f, "microvm") != newer {
		t.Errorf("gvisor runs %v (want the first AMI), microvm %v (want the new one)", amiOf(f, "gvisor"), amiOf(f, "microvm"))
	}
	// NODE_AMI_ID set later: the pools keep theirs, with a warning.
	const custom = "ami-0c0c0000000000001"
	cim := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 20)
	cim.ImageId = aws.String(custom)
	f.images[custom] = cim
	f.writes = nil
	logs = captureLog(t)
	if err := clusterTestRun(t, f, false, withOverlay, func(c *clusterConfig) { c.NodeAMIID = custom }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if amiOf(f, "gvisor") != eksAMIx86 || amiOf(f, "microvm") != newer || !strings.Contains(logs.String(), "keeps the AMI of its EC2NodeClass") {
		t.Errorf("gvisor %v, microvm %v; log:\n%s", amiOf(f, "gvisor"), amiOf(f, "microvm"), logs)
	}
}

// The Karpenter phase, not preflight, has the last word on a pool's AMI. A
// rerun whose preflight cannot read the EC2NodeClasses (the cluster is
// UPDATING, or the Kubernetes API answers 401 or 403, as it does a caller
// that has no access entry yet) gets past preflight with the AMI of the day,
// and the phase still keeps the AMI the EC2NodeClass names: nothing in the
// cluster is written.
func TestCreateClusterKarpenterPhaseKeepsTheAMI(t *testing.T) {
	deny := func(code int) func(*clusterFake) {
		return func(f *clusterFake) {
			k := "GET " + kindEC2NodeClass.path("", "gvisor")
			f.objects.deny[k], f.objects.denyLeft[k] = code, 1
		}
	}
	for _, c := range []struct {
		name string
		edit func(*clusterFake)
		note bool
	}{
		{"UPDATING", func(f *clusterFake) { f.cluster.Status, f.updateEnds = ekstypes.ClusterStatusUpdating, true }, false},
		{"401", deny(401), true},
		{"403", deny(403), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := karpenterFake(t)
			if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			const newer = "ami-0e1e0000000000099"
			im := f.images[eksAMIx86]
			im.ImageId = aws.String(newer)
			f.images[newer] = im
			f.params["/aws/service/eks/optimized-ami/1.37/amazon-linux-2023/x86_64/standard/recommended/image_id"] = newer
			c.edit(f)
			f.writes, f.objects.calls = nil, nil
			logs := captureLog(t)
			r := clusterTestRun(t, f, false, withKarpenter)
			if err := r.run(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, logs)
			}
			if amiOf(f, "gvisor") != eksAMIx86 || len(f.objects.writes()) != 0 || len(r.changes) != 0 {
				t.Errorf("gvisor runs %v, Kubernetes writes %v, changes %v", amiOf(f, "gvisor"), f.objects.writes(), r.changes)
			}
			if r.poolImages["gvisor"].ID != eksAMIx86 || !r.seeded["gvisor"] {
				t.Errorf("the phase kept %v, seeded %v", r.poolImages["gvisor"].ID, r.seeded)
			}
			if got := strings.Contains(logs.String(), "cannot read the EC2NodeClasses yet"); got != c.note || strings.Contains(logs.String(), "111122223333") {
				t.Errorf("note %v, want %v (and no account id):\n%s", got, c.note, logs)
			}
		})
	}
}

// A rerun compares what it would apply with the object as it is right
// before, and not with the status: Karpenter writes a NodePool's status (its
// nodes) and adds annotations of its own while setup-aws works, and a rerun
// still writes nothing and counts no change.
func TestCreateClusterKarpenterRerunIgnoresTheControllers(t *testing.T) {
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes, f.objects.calls = nil, nil
	f.objects.statusChurn = true
	np, dep := kindNodePool.path("", "gvisor"), kindDeployment.path("kube-system", "karpenter")
	reads := 0
	f.objects.onGet = func(path string) {
		// After the real run has read every object (the plan reads the
		// Deployment once, the run's first read is its second), Karpenter
		// annotates the NodePool.
		if path == dep {
			if reads++; reads == 3 {
				md := f.objects.objs[np]["metadata"].(map[string]any)
				md["annotations"] = map[string]any{"karpenter.sh/nodepool-hash": "123"}
			}
		}
	}
	for _, plan := range []bool{true, false} {
		r := clusterTestRun(t, f, plan, withKarpenter)
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(r.changes) != 0 || len(f.objects.writes()) != 0 {
			t.Errorf("plan %v: changes %v, Kubernetes writes %v", plan, r.changes, f.objects.writes())
		}
	}
	if reads < 3 || f.objects.get(kindNodePool, "", "gvisor").field("metadata", "annotations") == nil {
		t.Errorf("the controller did not annotate the NodePool (%d reads)", reads)
	}
}

// --plan writes nothing, in AWS or the cluster, and lists what a real run
// creates; on a complete deployment it has nothing to create or change.
func TestCreateClusterKarpenterPlan(t *testing.T) {
	f := karpenterFake(t)
	p := clusterTestRun(t, f, true, withKarpenter)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(f.objects.calls) != 0 {
		t.Fatalf("--plan made writes %v, Kubernetes calls %v", f.writes, f.objects.calls)
	}
	real := clusterTestRun(t, karpenterFake(t), false, withKarpenter)
	if err := real.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.creates, real.creates) {
		t.Errorf("plan lists\n %v\nthe run created\n %v", p.creates, real.creates)
	}
	// So with self-managed groups, whose access phase makes the node role's
	// entry.
	sp := clusterTestRun(t, karpenterFake(t), true, withKarpenter, selfManaged)
	if err := sp.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	sr := clusterTestRun(t, karpenterFake(t), false, withKarpenter, selfManaged)
	if err := sr.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sp.creates, sr.creates) {
		t.Errorf("self-managed: plan lists\n %v\nthe run created\n %v", sp.creates, sr.creates)
	}
	done := karpenterFake(t)
	if err := clusterTestRun(t, done, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done.writes, done.objects.calls = nil, nil
	p = clusterTestRun(t, done, true, withKarpenter)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(done.writes) != 0 || len(p.creates) != 0 || len(p.changes) != 0 || len(done.objects.writes()) != 0 {
		t.Errorf("plan on a complete deployment: writes %v, creates %v, changes %v, Kubernetes writes %v",
			done.writes, p.creates, p.changes, done.objects.writes())
	}
	// A plan counts what would change, and changes nothing.
	np := done.objects.get(kindNodePool, "", "gvisor")
	np["spec"].(map[string]any)["limits"] = map[string]any{"nodes": "9"}
	done.queues["demo-karpenter"].attrs["MessageRetentionPeriod"] = "600"
	p = clusterTestRun(t, done, true, withKarpenter)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(done.writes) != 0 || len(done.objects.writes()) != 0 || len(p.changes) != 2 {
		t.Errorf("plan of a drift: writes %v, Kubernetes writes %v, changes %v", done.writes, done.objects.writes(), p.changes)
	}
	// A cluster that is not ACTIVE: the plan says it would read the
	// objects then, and counts none of them as a create.
	done.cluster.Status = ekstypes.ClusterStatusUpdating
	logs := captureLog(t)
	p = clusterTestRun(t, done, true, withKarpenter)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range p.creates {
		if strings.HasPrefix(c, "Kubernetes ") {
			t.Errorf("plan on an UPDATING cluster counts %q", c)
		}
	}
	if !strings.Contains(logs.String(), "plan: would read Karpenter's objects once the cluster is ACTIVE") {
		t.Errorf("no note of the cluster's status:\n%s", logs)
	}
}

// A rerun puts back what drifted: the queue's settings, a rule's pattern
// and target, the controller's policy, the association's role, and a
// NodePool changed in the cluster; each counted as a change.
func TestCreateClusterKarpenterReconciles(t *testing.T) {
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.queues["demo-karpenter"].attrs["MessageRetentionPeriod"] = "600"
	f.rules["demo-karpenter-spot"].pattern = `{"source":["aws.ec2"]}`
	delete(f.rules["demo-karpenter-health"].targets, karpenterQueueTarget)
	f.roles["demo-us-west-2-karpenter"].inline["access"] = `{"Version":"2012-10-17","Statement":[]}`
	for _, a := range f.assocs {
		a.RoleArn = aws.String("arn:aws:iam::111122223333:role/demo-us-west-2-old")
	}
	np := f.objects.get(kindNodePool, "", "gvisor")
	np["spec"].(map[string]any)["limits"] = map[string]any{"nodes": "9"}
	f.writes, f.objects.calls = nil, nil
	r := clusterTestRun(t, f, false, withKarpenter)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"PutRolePolicy demo-us-west-2-karpenter access", "UpdatePodIdentityAssociation", "SetQueueAttributes demo-karpenter MessageRetentionPeriod",
		"PutTargets demo-karpenter-health", "PutRule demo-karpenter-spot"}
	for i, w := range want {
		if i >= len(f.writes) || !strings.HasPrefix(f.writes[i], w) {
			t.Errorf("writes %v, want %v", f.writes, want)
			break
		}
	}
	if len(f.writes) != len(want) || len(r.changes) != 6 || len(r.creates) != 0 {
		t.Errorf("writes %v, changes %v, creates %v", f.writes, r.changes, r.creates)
	}
	if kw := f.objects.writes(); len(kw) != 1 || kw[0] != "PATCH "+kindNodePool.path("", "gvisor") {
		t.Errorf("Kubernetes writes %v", kw)
	}
	if got := f.objects.get(kindNodePool, "", "gvisor").field("spec", "limits"); asJSON(t, got) != `{"cpu":"16","nodes":"2"}` {
		t.Errorf("NodePool limits %v", got)
	}
}

// Nothing of another owner is used or changed: a queue, a rule, a role or an
// association of the derived names without the owner tag, and a Kubernetes
// object without the ownership label, stop the run before it writes to it.
func TestCreateClusterKarpenterRefusesForeign(t *testing.T) {
	for _, c := range []struct {
		name, want string
		setup      func(f *clusterFake)
		noWrite    string
	}{
		{"queue", "SQS queue demo-karpenter exists but is not owned", func(f *clusterFake) {
			f.queues["demo-karpenter"] = &fakeQueue{attrs: map[string]string{}, tags: map[string]string{"team": "other"}}
		}, "CreateQueue"},
		{"rule", "EventBridge rule demo-karpenter-rebalance exists but is not owned", func(f *clusterFake) {
			f.rules["demo-karpenter-rebalance"] = &fakeRule{tags: map[string]string{}, targets: map[string]string{}}
		}, "PutRule demo-karpenter-rebalance"},
		{"role", "IAM role demo-us-west-2-karpenter exists but is not owned", func(f *clusterFake) {
			f.roles["demo-us-west-2-karpenter"] = &fakeRole{arn: karpenterRoleARN, tags: map[string]string{}}
		}, "PutRolePolicy demo-us-west-2-karpenter"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := karpenterFake(t)
			c.setup(f)
			err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			for _, w := range f.writes {
				if strings.HasPrefix(w, c.noWrite) {
					t.Errorf("wrote %s", w)
				}
			}
			if len(f.objects.calls) != 0 {
				t.Errorf("reached the cluster: %v", f.objects.calls)
			}
		})
	}

	// On a deployment that exists: an association of another owner stops
	// the run before the controller role is touched.
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.assocs {
		a.Tags = map[string]string{}
	}
	f.roles["demo-us-west-2-karpenter"].inline["access"] = "{}"
	f.writes = nil
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Pod Identity association kube-system/karpenter") || len(f.writes) != 0 {
		t.Errorf("foreign association: %v, writes %v", err, f.writes)
	}

	// A CRD of another install: nothing in the cluster is applied.
	f = karpenterFake(t)
	f.objects.put(kubeObject{"apiVersion": kindCRD.APIVersion, "kind": kindCRD.Kind,
		"metadata": map[string]any{"name": "nodepools.karpenter.sh", "labels": map[string]any{managedByKey: "Helm"}}})
	err = clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Kubernetes CustomResourceDefinition nodepools.karpenter.sh exists without the label app.kubernetes.io/managed-by=substrate-eks") ||
		len(f.objects.writes()) != 0 {
		t.Errorf("foreign CRD: %v, Kubernetes writes %v", err, f.objects.writes())
	}

	// An EC2NodeClass someone else made on the cluster stops the rerun in
	// preflight, before anything is written.
	f = karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	nc := f.objects.get(kindEC2NodeClass, "", "gvisor")
	delete(nc["metadata"].(map[string]any)["labels"].(map[string]any), managedByKey)
	f.writes, f.objects.calls = nil, nil
	logs := captureLog(t)
	err = clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Kubernetes EC2NodeClass gvisor exists without the label") ||
		len(f.writes) != 0 || len(f.objects.writes()) != 0 || strings.Contains(logs.String(), "Step 2/11") {
		t.Errorf("foreign EC2NodeClass: %v, writes %v, Kubernetes writes %v; the run went past preflight: %v",
			err, f.writes, f.objects.writes(), strings.Contains(logs.String(), "Step 2/11"))
	}
}

// A denied read stops the run: SQS, EventBridge, the association, and the
// Kubernetes API; none of them is taken as "not there".
func TestCreateClusterKarpenterDeniedReadStops(t *testing.T) {
	for _, op := range []string{"GetQueueUrl", "ListQueueTags", "GetQueueAttributes", "DescribeRule", "ListTagsForResource", "ListTargetsByRule",
		"ListPodIdentityAssociations", "DescribePodIdentityAssociation"} {
		t.Run(op, func(t *testing.T) {
			f := karpenterFake(t)
			// On a complete deployment, so each read is reached.
			if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.writes, f.objects.calls = nil, nil
			f.deny[op] = "AccessDenied"
			err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "AccessDenied") || len(f.writes) != 0 || len(f.objects.writes()) != 0 {
				t.Errorf("%s denied: %v, writes %v, Kubernetes writes %v", op, err, f.writes, f.objects.writes())
			}
		})
	}
	f := karpenterFake(t)
	f.objects.deny["GET "+kindCRD.path("", "nodeclaims.karpenter.sh")] = 403
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") || len(f.objects.writes()) != 0 {
		t.Errorf("a denied Kubernetes read: %v, writes %v", err, f.objects.writes())
	}
	if strings.Contains(err.Error(), "k8s-aws-v1") || strings.Contains(err.Error(), "111122223333") || !strings.Contains(err.Error(), "********3333") {
		t.Errorf("the error carries the token or the account id: %v", err)
	}
}

// KARPENTER is fixed when the cluster is made: a cluster made with it is
// refused without it, one made without it is refused with it, and TAGS
// can never set the marker.
func TestCreateClusterKarpenterMarker(t *testing.T) {
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.AutoMode = "" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `was made with KARPENTER=on, but KARPENTER=""; set KARPENTER=on to match, or `) || len(f.writes) != 0 {
		t.Errorf("KARPENTER unset on a Karpenter cluster: %v, writes %v", err, f.writes)
	}

	f = karpenterFake(t)
	if err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.AutoMode = "" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.clusterIn.Tags[nodegroups.KarpenterTagKey]; ok {
		t.Errorf("a cluster without Karpenter is marked: %v", f.clusterIn.Tags)
	}
	f.writes = nil
	err = clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "applies to a new cluster only") || len(f.writes) != 0 {
		t.Errorf("KARPENTER=on on a cluster without it: %v, writes %v", err, f.writes)
	}

	for _, karp := range []bool{false, true} {
		f = karpenterFake(t)
		r := clusterTestRun(t, f, false, func(c *clusterConfig) {
			if karp {
				withKarpenter(c)
			}
		})
		r.e.cfg.Tags += " " + nodegroups.KarpenterTagKey + "=on"
		err = r.run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "TAGS sets "+nodegroups.KarpenterTagKey) || len(f.reads) != 0 {
			t.Errorf("TAGS with the marker (KARPENTER=on %v): %v, reads %v", karp, err, f.reads)
		}
	}
}

// The settings KARPENTER=on refuses, each before anything is read.
func TestCreateClusterKarpenterRefusals(t *testing.T) {
	for _, c := range []struct {
		name, want, tags string
		edit             func(*clusterConfig)
	}{
		{"Auto Mode", "KARPENTER=on needs AUTO_MODE off", "", func(c *clusterConfig) { withKarpenter(c); c.AutoMode = "on" }},
		{"no pool", "KARPENTER=on makes a NodePool for the gvisor sandbox class, and NODE_GROUPS=system,nitro does not name gvisor", "", func(c *clusterConfig) {
			withKarpenter(c)
			c.NodeGroups, c.NitroAMIID, c.NitroNodes = "system,nitro", "ami-0123456789abcdef0", 1
		}},
		{"microvm only", "NODE_GROUPS=system,microvm does not name gvisor", "", func(c *clusterConfig) {
			withKarpenter(c)
			c.NodeGroups = "system,microvm"
		}},
		{"a value", `KARPENTER must be "on", "off" or empty, not "yes"`, "", func(c *clusterConfig) { c.AutoMode, c.Karpenter = "", "yes" }},
		{"a pool limit of 0", "the gvisor NodePool needs a limit of at least one node (GVISOR_NODES)", "", func(c *clusterConfig) {
			withKarpenter(c)
			c.GvisorNodes = 0
		}},
		{"owner tag", "TAGS sets " + OwnerTagKey, OwnerTagKey + "=x.y", withKarpenter},
		{"created-by tag", "TAGS sets " + CreatedByTagKey, CreatedByTagKey + "=me", withKarpenter},
		{"a cluster tag", "TAGS sets kubernetes.io/cluster/demo, a key Karpenter sets", "kubernetes.io/cluster/demo=owned", withKarpenter},
		{"a cluster tag without the slash", "TAGS sets kubernetes.io/cluster, a key Karpenter sets", "kubernetes.io/cluster=x", withKarpenter},
		{"a nodepool tag", "TAGS sets karpenter.sh/nodepool", "karpenter.sh/nodepool=x", withKarpenter},
		{"an EC2NodeClass tag", "TAGS sets karpenter.k8s.aws/ec2nodeclass", "karpenter.k8s.aws/ec2nodeclass=x", withKarpenter},
		{"the EKS cluster tag", "TAGS sets eks:eks-cluster-name", "eks:eks-cluster-name=demo", withKarpenter},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := karpenterFake(t)
			r := clusterTestRun(t, f, false, c.edit)
			r.e.cfg.Tags += " " + c.tags
			err := r.run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if len(f.reads) != 0 || len(f.writes) != 0 {
				t.Errorf("refused after reads %v, writes %v", f.reads, f.writes)
			}
		})
	}
	// Without KARPENTER=on, TAGS is as free as before: the owner and
	// Karpenter keys pass validation.
	f := karpenterFake(t)
	r := clusterTestRun(t, f, false, func(c *clusterConfig) { c.Karpenter = "off" })
	r.e.cfg.Tags += " " + OwnerTagKey + "=demo.us-west-2 karpenter.sh/nodepool=x"
	if err := r.validate(); err != nil {
		t.Errorf("TAGS refused without KARPENTER=on: %v", err)
	}
	// nodegroups keeps the owner and created-by keys cmd's.
	if nodegroups.CheckTags(OwnerTagKey+"=a", true) == nil || nodegroups.CheckTags(CreatedByTagKey+"=a", true) == nil {
		t.Error("nodegroups.CheckTags does not know the owner or created-by key")
	}
}

// The pools' instance types are checked as the groups' are: nested
// virtualization for microvm, a node group here and a NodePool with a
// NodeOverlay; an arm64 type gets the arm64 AMI and the arm64 requirement.
func TestCreateClusterKarpenterInstanceTypes(t *testing.T) {
	f := karpenterFake(t)
	microvm := func(c *clusterConfig) { c.MicrovmInstanceType = "m7i.2xlarge" }
	err := clusterTestRun(t, f, false, withKarpenter, microvm).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "m7i.2xlarge does not support nested virtualization, which the microvm node group needs") || len(f.writes) != 0 {
		t.Errorf("microvm group without nested virtualization: %v, writes %v", err, f.writes)
	}
	f = karpenterFake(t)
	err = clusterTestRun(t, f, false, withOverlay, microvm).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "m7i.2xlarge does not support nested virtualization, which the microvm NodePool needs") || len(f.writes) != 0 {
		t.Errorf("microvm pool without nested virtualization: %v, writes %v", err, f.writes)
	}
	f = karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter, func(c *clusterConfig) { c.GvisorInstanceType = "m8g.2xlarge" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	nc := f.objects.get(kindEC2NodeClass, "", "gvisor")
	np := f.objects.get(kindNodePool, "", "gvisor")
	reqs := asJSON(t, np.field("spec", "template", "spec", "requirements"))
	if nc.field("spec", "amiSelectorTerms").([]any)[0].(map[string]any)["id"] != eksAMIarm || !strings.Contains(reqs, `"values":["arm64"]`) {
		t.Errorf("arm64 pool: AMI %v, requirements %s", nc.field("spec", "amiSelectorTerms"), reqs)
	}
}

// The Kubernetes phase's waits stop with why: a NodePool that is not Ready,
// a controller that is not available.
func TestCreateClusterKarpenterWaitsSayWhy(t *testing.T) {
	shortKarpenterWaits(t)
	f := karpenterFake(t)
	f.objects.notReady[kindNodePool.path("", "gvisor")] = "NodeClass not found"
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "NodePool gvisor") || !strings.Contains(err.Error(), "NodeClass not found") {
		t.Errorf("a NodePool not Ready: %v", err)
	}
	f = karpenterFake(t)
	f.objects.notAvailable = true
	err = clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Deployment karpenter") || !strings.Contains(err.Error(), "minimum availability") ||
		f.objects.get(kindNodePool, "", "gvisor") != nil {
		t.Errorf("a controller not available: %v (and the pools are applied only after it)", err)
	}
	if !strings.Contains(err.Error(), "the controller logs say why") {
		t.Errorf("no pointer to the logs: %v", err)
	}
	// Pods with credentials, and another Pod without them that is not the
	// controller's: the error points at the logs, and nothing is deleted.
	f = karpenterFake(t)
	f.objects.notAvailable = true
	f.objects.put(karpenterPod("karpenter-a", "AWS_REGION", podIdentityCredentials))
	f.objects.put(karpenterPod("other", "AWS_REGION"))
	f.objects.get(kindPod, "kube-system", "other")["metadata"].(map[string]any)["labels"] = map[string]any{"app": "other"}
	err = clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || strings.Contains(err.Error(), "no EKS Pod Identity credentials") || !strings.Contains(err.Error(), "the controller logs say why") {
		t.Errorf("Pods with credentials (and another Pod without): %v", err)
	}
	if d := podDeletes(f.objects); len(d) != 0 {
		t.Errorf("Pods with credentials (and another Pod without): deleted %v", d)
	}
}

// karpenterPod is a controller Pod, with the chart's labels, whose
// controller container has the variables env.
func karpenterPod(name string, env ...string) kubeObject {
	var envs []any
	for _, e := range env {
		envs = append(envs, map[string]any{"name": e, "value": "x"})
	}
	return kubeObject{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name, "namespace": "kube-system",
		"labels": map[string]any{"app.kubernetes.io/name": "karpenter", "app.kubernetes.io/instance": "karpenter"}},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "controller", "env": envs}}}}
}

// staleController leaves the controller as a run that applied the chart
// before the association took effect does: Pods a and b without
// credentials, the Deployment not available.
func staleController(k *kubeFake) {
	k.put(karpenterPod("karpenter-a", "AWS_REGION"))
	k.put(karpenterPod("karpenter-b", "AWS_REGION"))
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.objs[kindDeployment.path("kube-system", "karpenter")]; d != nil {
		d["status"] = map[string]any{"observedGeneration": d.field("metadata", "generation"), "availableReplicas": 0.0,
			"conditions": []any{map[string]any{"type": "Available", "status": "False", "reason": "MinimumReplicasUnavailable",
				"message": "Deployment does not have minimum availability."}}}
	}
}

// remakePods stands in for the controller's ReplicaSet and EKS Pod
// Identity's webhook: each deleted controller Pod is made again as
// <name>x, with credentials when creds, and once no controller Pod lacks
// them (one being deleted aside) the Deployment is available.
func remakePods(k *kubeFake, creds bool) {
	k.onDelete = func(kind kubeKind, name string) {
		if kind != kindPod {
			return
		}
		env := []string{"AWS_REGION"}
		if creds {
			env = append(env, podIdentityCredentials)
		}
		k.objs[kindPod.path("kube-system", name+"x")] = karpenterPod(name+"x", env...)
		for p, o := range k.objs {
			if kk, _, n, _ := route(p); kk == kindPod && n != "" && o.field("metadata", "deletionTimestamp") == nil && withoutCredentials(o) {
				return
			}
		}
		d := k.objs[kindDeployment.path("kube-system", "karpenter")]
		d["status"] = map[string]any{"observedGeneration": d.field("metadata", "generation"), "availableReplicas": d.field("spec", "replicas"),
			"conditions": []any{map[string]any{"type": "Available", "status": "True"}}}
	}
}

// podDeletes are the fake's DELETE requests of Pods.
func podDeletes(k *kubeFake) []string {
	var out []string
	for _, w := range k.writes() {
		if strings.HasPrefix(w, "DELETE "+kindPod.path("kube-system", "")) {
			out = append(out, w)
		}
	}
	return out
}

// A controller Pod made before the association took effect has no
// credentials, and never gets them. While the controller is not available,
// create cluster deletes each such Pod, so the ReplicaSet makes a new one
// that EKS gives them: on a fresh cluster, whose Pods the ReplicaSet makes
// right after the chart, and on a rerun over Pods an earlier run left so.
// Each restart is a change, logged.
func TestCreateClusterKarpenterRestartsPodsWithoutCredentials(t *testing.T) {
	shortKarpenterWaits(t)
	// Long enough for the restarts, however slow the machine.
	karpenterReadyTimeout = time.Second
	restarted := []string{
		"restart Kubernetes Pod kube-system/karpenter-a: it has no EKS Pod Identity credentials",
		"restart Kubernetes Pod kube-system/karpenter-b: it has no EKS Pod Identity credentials",
	}
	deletes := []string{"DELETE " + kindPod.path("kube-system", "karpenter-a"), "DELETE " + kindPod.path("kube-system", "karpenter-b")}

	// Fresh: the Pods appear once the Deployment exists, without credentials.
	f := karpenterFake(t)
	f.objects.notAvailable = true
	remakePods(f.objects, true)
	deploy, made := kindDeployment.path("kube-system", "karpenter"), false
	f.objects.onGet = func(path string) {
		if path == deploy && f.objects.objs[deploy] != nil && !made {
			made = true
			for _, n := range []string{"karpenter-a", "karpenter-b"} {
				f.objects.objs[kindPod.path("kube-system", n)] = karpenterPod(n, "AWS_REGION")
			}
		}
	}
	logs := captureLog(t)
	r := clusterTestRun(t, f, false, withKarpenter)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if d := podDeletes(f.objects); !made || !slices.Equal(d, deletes) {
		t.Errorf("fresh: Pods made %v, deleted %v", made, d)
	}
	for _, w := range restarted {
		if !slices.Contains(r.changes, w) || strings.Count(logs.String(), w) != 1 {
			t.Errorf("fresh: %q in changes %v, logged %d times", w, r.changes, strings.Count(logs.String(), w))
		}
	}
	if f.objects.get(kindNodePool, "", "gvisor") == nil {
		t.Error("fresh: the NodePool is not applied")
	}

	// Rerun over Pods an earlier run left without credentials: the restarts
	// are all it changes. A Pod being deleted already is left to go.
	f = karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	staleController(f.objects)
	old := karpenterPod("karpenter-0", "AWS_REGION")
	old["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-19T00:00:00Z"
	f.objects.put(old)
	f.objects.finalizing[kindPod.path("kube-system", "karpenter-0")] = 1000
	remakePods(f.objects, true)
	f.objects.calls = nil
	r = clusterTestRun(t, f, false, withKarpenter)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if d := podDeletes(f.objects); !slices.Equal(d, deletes) || !slices.Equal(r.changes, restarted) {
		t.Errorf("rerun: deleted %v, changes %v", d, r.changes)
	}
	if ax := f.objects.get(kindPod, "kube-system", "karpenter-ax"); ax == nil || withoutCredentials(ax) || f.objects.get(kindPod, "kube-system", "karpenter-a") != nil {
		t.Error("rerun: the Pods are not the new ones with credentials")
	}
}

// Pods that never get credentials (an association that does not take
// effect) are restarted karpenterRestarts times, no more, and create
// cluster stops with why; the pools are not applied.
func TestCreateClusterKarpenterPodsNeverGetCredentials(t *testing.T) {
	shortKarpenterWaits(t)
	// Long enough for every restart, however slow the machine.
	karpenterReadyTimeout = time.Second
	f := karpenterFake(t)
	f.objects.notAvailable = true
	staleController(f.objects)
	remakePods(f.objects, false)
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Deployment karpenter") || !strings.Contains(err.Error(), "has no EKS Pod Identity credentials") ||
		!strings.Contains(err.Error(), "restarts such Pods up to 3 times") || strings.Contains(err.Error(), "kubectl") {
		t.Errorf("Pods without credentials after the restarts: %v", err)
	}
	if d := podDeletes(f.objects); len(d) != 2*karpenterRestarts {
		t.Errorf("%d Pod deletes, want %d: %v", len(d), 2*karpenterRestarts, d)
	}
	if f.objects.get(kindNodePool, "", "gvisor") != nil {
		t.Error("the NodePool is applied")
	}
}

// --plan restarts no Pod: it writes nothing, and does not wait.
func TestCreateClusterKarpenterPlanRestartsNoPod(t *testing.T) {
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	staleController(f.objects)
	remakePods(f.objects, true)
	f.objects.calls = nil
	if err := clusterTestRun(t, f, true, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.objects.writes(); len(w) != 0 || f.objects.get(kindPod, "kube-system", "karpenter-a") == nil {
		t.Errorf("--plan wrote %v", w)
	}
}

// A failed read or delete of the controller Pods stops the step: it is
// never taken for Pods that have credentials.
func TestCreateClusterKarpenterPodReadOrDeleteFails(t *testing.T) {
	shortKarpenterWaits(t)
	for _, call := range []string{"GET " + kindPod.path("kube-system", ""), "DELETE " + kindPod.path("kube-system", "karpenter-a")} {
		f := karpenterFake(t)
		f.objects.notAvailable = true
		staleController(f.objects)
		remakePods(f.objects, true)
		f.objects.deny[call] = 403
		err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
		want := "list the pods"
		if strings.HasPrefix(call, "DELETE") {
			want = "delete Pod karpenter-a"
		}
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "403 Forbidden") ||
			strings.Contains(err.Error(), "111122223333") || f.objects.get(kindNodePool, "", "gvisor") != nil {
			t.Errorf("%s refused: %v", call, err)
		}
		if d := podDeletes(f.objects); len(d) > 1 {
			t.Errorf("%s refused: deleted %v", call, d)
		}
	}
}

// One system node runs one controller Pod: the Pods need different nodes.
func TestCreateClusterKarpenterOneSystemNode(t *testing.T) {
	f := karpenterFake(t)
	if err := clusterTestRun(t, f, false, withKarpenter, func(c *clusterConfig) { c.SystemNodes = 1 }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.objects.get(kindDeployment, "kube-system", "karpenter").field("spec", "replicas"); got != 1.0 {
		t.Errorf("replicas %v", got)
	}
}

// The node role's access entry EKS made for the managed system group's
// role is kept; one of another type is refused.
func TestCreateClusterKarpenterNodeAccessEntry(t *testing.T) {
	f := karpenterFake(t)
	f.entries[nodeRoleARN], f.entryTypes[nodeRoleARN] = nil, accessTypeNode
	if err := clusterTestRun(t, f, false, withKarpenter).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.writes, "CreateAccessEntry "+nodeRoleARN) || f.entries[nodeRoleARN] != nil {
		t.Errorf("EKS's entry was replaced: writes %v", f.writes)
	}
	f = karpenterFake(t)
	f.entries[nodeRoleARN], f.entryTypes[nodeRoleARN] = nil, "STANDARD"
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Karpenter nodes need EC2_LINUX") {
		t.Errorf("a STANDARD entry: %v", err)
	}
}

// With self-managed node groups, the system and microvm groups are Auto
// Scaling groups, the gvisor pool is Karpenter's, and the phases are the
// self-managed ones and Karpenter's.
func TestCreateClusterKarpenterSelfManaged(t *testing.T) {
	f := karpenterFake(t)
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withKarpenter, selfManaged).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got := sortedKeysOf(f.asgIn); !slices.Equal(got, []string{"demo-microvm", "demo-system"}) {
		t.Errorf("Auto Scaling groups %v", got)
	}
	if n := creates(f.writes)["CreateInstanceProfile "+nodeProfile] + creates(f.writes)["CreateAccessEntry "+nodeRoleARN]; n != 2 {
		t.Errorf("instance profile and access entry made %d times, writes %v", n, f.writes)
	}
	list := phases.ClusterFor(nodegroups.SelfManaged, true)
	for i := range list {
		if !strings.Contains(logs.String(), phases.Line(list, i)) {
			t.Errorf("no line %q", phases.Line(list, i))
		}
	}
	if f.objects.get(kindNodePool, "", "gvisor") == nil || f.objects.get(kindNodePool, "", "microvm") != nil {
		t.Error("no gvisor NodePool, or a microvm one")
	}
}

// With KARPENTER empty or off, a run is what it was: no SQS, EventBridge,
// association or Kubernetes object call, no marker, the phases of before,
// and the same plan.
func TestCreateClusterWithoutKarpenterIsUnchanged(t *testing.T) {
	var plans [][]string
	for _, v := range []string{"", "off"} {
		f := karpenterFake(t)
		logs := captureLog(t)
		r := clusterTestRun(t, f, false, func(c *clusterConfig) { c.Karpenter = v })
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := creates(f.writes); !maps.Equal(got, freshCreates) {
			t.Errorf("KARPENTER=%q: creates %v", v, got)
		}
		for _, op := range []string{"GetQueueUrl", "DescribeRule", "ListPodIdentityAssociations", "KubeObjects"} {
			if slices.Contains(f.reads, op) {
				t.Errorf("KARPENTER=%q read %s", v, op)
			}
		}
		if len(f.objects.calls) != 0 || len(f.queues)+len(f.rules)+len(f.assocs) != 0 {
			t.Errorf("KARPENTER=%q: Kubernetes calls %v, queues %v, rules %v", v, f.objects.calls, f.queues, f.rules)
		}
		if _, ok := f.clusterIn.Tags[nodegroups.KarpenterTagKey]; ok {
			t.Errorf("KARPENTER=%q marked the cluster", v)
		}
		for i := range phases.Cluster {
			if !strings.Contains(logs.String(), phases.Line(phases.Cluster, i)) {
				t.Errorf("no line %q", phases.Line(phases.Cluster, i))
			}
		}
		if strings.Contains(logs.String(), "Karpenter") {
			t.Errorf("KARPENTER=%q logged Karpenter lines", v)
		}
		p := clusterTestRun(t, karpenterFake(t), true, func(c *clusterConfig) { c.Karpenter = v })
		if err := p.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, p.creates)
	}
	if !slices.Equal(plans[0], plans[1]) {
		t.Errorf("plans differ:\n %v\n %v", plans[0], plans[1])
	}
	if !slices.Equal(phases.ClusterFor(nodegroups.Managed, false), phases.Cluster) ||
		!slices.Equal(phases.ClusterFor(nodegroups.SelfManaged, false), phases.ClusterSelfManaged) {
		t.Error("phases.ClusterFor without Karpenter changed")
	}
}

// ClusterFor with Karpenter is the type's list and Karpenter's two phases;
// the lists without it are not changed by it.
func TestPhasesClusterForKarpenter(t *testing.T) {
	for _, typ := range []string{nodegroups.Managed, nodegroups.SelfManaged} {
		base := phases.ClusterFor(typ, false)
		k := phases.ClusterFor(typ, true)
		if len(k) != len(base)+2 || !slices.Equal(k[:len(base)], base) || !slices.Equal(k[len(base):], phases.KarpenterPhases) {
			t.Errorf("%s: %q", typ, k)
		}
		if len(phases.ClusterFor(typ, false)) != 9 {
			t.Errorf("%s: appending Karpenter's phases changed the list without them", typ)
		}
	}
}

// The controller's policy is the reference's statements for this cluster,
// but for the instance profile writes and zonal shift; PassRole is on the
// node role only.
func TestKarpenterPolicy(t *testing.T) {
	n := deriveNames("aws", "111122223333", "us-west-2", "demo")
	doc := karpenterPolicy(n, nodeRoleARN)
	if strings.Contains(doc, "${") || len(doc) > 10240 {
		t.Fatalf("policy of %d bytes: %s", len(doc), doc)
	}
	var p struct {
		Version   string
		Statement []struct {
			Sid, Effect string
			Resource    any
			Action      any
			Condition   map[string]map[string]any
		}
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	var sids []string
	actions := map[string][]string{}
	for _, s := range p.Statement {
		sids = append(sids, s.Sid)
		if s.Effect != "Allow" {
			t.Errorf("%s: %s", s.Sid, s.Effect)
		}
		switch a := s.Action.(type) {
		case string:
			actions[s.Sid] = []string{a}
		case []any:
			for _, x := range a {
				actions[s.Sid] = append(actions[s.Sid], x.(string))
			}
		}
	}
	want := []string{"AllowScopedEC2InstanceAccessActions", "AllowScopedEC2LaunchTemplateAccessActions", "AllowScopedEC2InstanceActionsWithTags",
		"AllowScopedResourceCreationTagging", "AllowScopedResourceTagging", "AllowScopedDeletion", "AllowPassingInstanceRole",
		"AllowAPIServerEndpointDiscovery", "AllowInterruptionQueueActions", "AllowRegionalReadActions", "AllowSSMReadActions",
		"AllowPricingReadActions", "AllowUnscopedInstanceProfileListAction", "AllowInstanceProfileReadActions"}
	if !slices.Equal(sids, want) {
		t.Errorf("statements %v\nwant %v", sids, want)
	}
	for sid, as := range actions {
		for _, a := range as {
			if strings.HasPrefix(a, "arc-zonal-shift:") || slices.Contains([]string{"iam:CreateInstanceProfile", "iam:TagInstanceProfile",
				"iam:AddRoleToInstanceProfile", "iam:RemoveRoleFromInstanceProfile", "iam:DeleteInstanceProfile"}, a) {
				t.Errorf("%s allows %s", sid, a)
			}
		}
	}
	for _, s := range p.Statement {
		got := asJSON(t, s.Resource)
		switch s.Sid {
		case "AllowPassingInstanceRole":
			if got != `"`+nodeRoleARN+`"` {
				t.Errorf("PassRole on %s", got)
			}
		case "AllowInterruptionQueueActions":
			if got != `"`+queueARN+`"` {
				t.Errorf("SQS on %s", got)
			}
		case "AllowAPIServerEndpointDiscovery":
			if got != `"arn:aws:eks:us-west-2:111122223333:cluster/demo"` {
				t.Errorf("DescribeCluster on %s", got)
			}
		case "AllowScopedEC2InstanceActionsWithTags", "AllowScopedResourceCreationTagging":
			eq := s.Condition["StringEquals"]
			if eq["aws:RequestTag/kubernetes.io/cluster/demo"] != "owned" || eq["aws:RequestTag/eks:eks-cluster-name"] != "demo" {
				t.Errorf("%s: condition %v", s.Sid, s.Condition)
			}
		case "AllowScopedDeletion":
			if s.Condition["StringEquals"]["aws:ResourceTag/kubernetes.io/cluster/demo"] != "owned" ||
				s.Condition["StringLike"]["aws:ResourceTag/karpenter.sh/nodepool"] != "*" {
				t.Errorf("deletion condition %v", s.Condition)
			}
		case "AllowRegionalReadActions":
			if s.Condition["StringEquals"]["aws:RequestedRegion"] != "us-west-2" {
				t.Errorf("read condition %v", s.Condition)
			}
		}
	}
	// The queue's policy: EventBridge may send, nothing without TLS.
	qp := karpenterQueuePolicy(n)
	if !sameJSON(qp, `{"Version":"2012-10-17","Id":"EC2InterruptionPolicy","Statement":[`+
		`{"Effect":"Allow","Principal":{"Service":["events.amazonaws.com","sqs.amazonaws.com"]},"Action":"sqs:SendMessage","Resource":"`+queueARN+`"},`+
		`{"Sid":"DenyHTTP","Effect":"Deny","Action":"sqs:*","Resource":"`+queueARN+`","Condition":{"Bool":{"aws:SecureTransport":"false"}},"Principal":"*"}]}`) {
		t.Errorf("queue policy %s", qp)
	}
	// The derived names fit EventBridge's 64 and SQS's 80 characters for the
	// longest cluster name.
	long := strings.Repeat("c", 21)
	for _, r := range karpenterRuleNames(long) {
		if len(r) > 64 {
			t.Errorf("rule name %s is %d characters", r, len(r))
		}
	}
	if len(karpenterQueueName(long)) > 80 || len(karpenterRoleName(deriveNames("aws", "111122223333", "us-isof-south-1", long))) > 64 {
		t.Error("a derived name is too long")
	}
}

// The embedded chart: its 18 objects, none a hook, every image pinned by
// digest, the Service ClusterIP, no host network, the controller on the
// system group, and every object labeled for setup-aws, not Helm.
func TestKarpenterChart(t *testing.T) {
	objs, err := karpenterChart("demo", "https://example", "demo-karpenter", "us-west-2", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, o := range objs {
		kinds[o.str("kind")]++
		if o.label(managedByKey) != managedByValue {
			t.Errorf("%s is labeled %s=%q", describe(o), managedByKey, o.label(managedByKey))
		}
		if a, _ := o.field("metadata", "annotations").(map[string]any); a != nil {
			for k := range a {
				if strings.HasPrefix(k, "helm.sh/") {
					t.Errorf("%s has %s", describe(o), k)
				}
			}
		}
		if ns := o.namespace(); ns != "" && ns != "kube-system" {
			t.Errorf("%s in namespace %s", describe(o), ns)
		}
	}
	if want := map[string]int{"CustomResourceDefinition": 5, "PodDisruptionBudget": 1, "ServiceAccount": 1, "ClusterRole": 3,
		"ClusterRoleBinding": 2, "Role": 2, "RoleBinding": 2, "Service": 1, "Deployment": 1}; !maps.Equal(kinds, want) {
		t.Errorf("kinds %v", kinds)
	}
	for _, o := range objs {
		switch o.str("kind") {
		case "Service":
			if o.str("spec", "type") != "ClusterIP" {
				t.Errorf("Service type %q", o.str("spec", "type"))
			}
		case "Deployment":
			pod := o.field("spec", "template", "spec").(map[string]any)
			if pod["hostNetwork"] == true {
				t.Error("the controller runs with hostNetwork")
			}
			if asJSON(t, pod["nodeSelector"]) != `{"ate.dev/pool":"system","kubernetes.io/os":"linux"}` {
				t.Errorf("nodeSelector %v", pod["nodeSelector"])
			}
			for _, c := range pod["containers"].([]any) {
				img := c.(map[string]any)["image"].(string)
				if !strings.Contains(img, "@sha256:") || img != "public.ecr.aws/karpenter/controller:1.15.2@sha256:4a7dc37f85ad8987ece1cf5df55e607aa17a879a6faa76f5258c5bf182fbdfff" {
					t.Errorf("image %s", img)
				}
			}
			if o.field("spec", "replicas") != 2.0 {
				t.Errorf("replicas %v", o.field("spec", "replicas"))
			}
			// Guaranteed, as the getting-started guide sets it.
			res := asJSON(t, pod["containers"].([]any)[0].(map[string]any)["resources"])
			if res != `{"limits":{"cpu":1,"memory":"1Gi"},"requests":{"cpu":1,"memory":"1Gi"}}` {
				t.Errorf("controller resources %s", res)
			}
		}
	}
	if strings.Contains(string(karpenterChartYAML), "helm.sh/hook") || !strings.Contains(string(karpenterChartYAML), "ba0b5e555e79abb2254e89ecea9aad318b7f2061644413003f2af045ae29dbd0") {
		t.Error("the chart has a hook, or its header no chart sha256")
	}
}

// More refusals of the pools' preflight, before anything is written: a type
// that reports no vCPUs (the CPU limit needs them), and a NODE_AMI_ID of
// another architecture than the pools' type.
func TestCreateClusterKarpenterPoolPreflight(t *testing.T) {
	f := karpenterFake(t)
	it := f.types["m8i.2xlarge"]
	it.VCpuInfo = nil
	f.types["m8i.2xlarge"] = it
	err := clusterTestRun(t, f, false, withKarpenter, func(c *clusterConfig) { c.SystemInstanceType = "m8i.xlarge" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "m8i.2xlarge (gvisor NodePool) reports no vCPU count") || len(f.writes) != 0 {
		t.Errorf("no vCPUs: %v, writes %v", err, f.writes)
	}
	f = karpenterFake(t)
	arm := testImage(ec2types.ArchitectureValuesArm64, "/dev/xvda", 20)
	arm.ImageId = aws.String("ami-0a0a0000000000001")
	f.images["ami-0a0a0000000000001"] = arm
	// The system group runs an arm64 type, so NODE_AMI_ID fits it, not the pool.
	err = clusterTestRun(t, f, false, withKarpenter, func(c *clusterConfig) {
		c.SystemInstanceType, c.NodeAMIID, c.NodeGroups = "m8g.2xlarge", "ami-0a0a0000000000001", "system,gvisor"
	}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "but the gvisor NodePool's m8i.2xlarge runs x86_64") || len(f.writes) != 0 {
		t.Errorf("an arm64 NODE_AMI_ID for an x86_64 pool: %v, writes %v", err, f.writes)
	}
}

// The CRDs must be Established before the rest is applied.
func TestCreateClusterKarpenterWaitsForTheCRDs(t *testing.T) {
	shortKarpenterWaits(t)
	f := karpenterFake(t)
	f.objects.notEstablished = true
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CustomResourceDefinition") || !strings.Contains(err.Error(), "the names are not accepted") ||
		f.objects.get(kindDeployment, "kube-system", "karpenter") != nil {
		t.Errorf("CRDs not Established: %v", err)
	}
}

// A namespaced object of another install (the ServiceAccount) is refused as
// the cluster-scoped ones are, before anything is applied.
func TestCreateClusterKarpenterRefusesAForeignServiceAccount(t *testing.T) {
	f := karpenterFake(t)
	f.objects.put(kubeObject{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "karpenter", "namespace": "kube-system"}})
	err := clusterTestRun(t, f, false, withKarpenter).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Kubernetes ServiceAccount kube-system/karpenter exists without the label") || len(f.objects.writes()) != 0 {
		t.Errorf("foreign ServiceAccount: %v, Kubernetes writes %v", err, f.objects.writes())
	}
}

// The chart must give the controller its settings: a chart without one of
// the env values, or with a ${...} left, is refused.
func TestKarpenterChartRefusals(t *testing.T) {
	prev := karpenterChartYAML
	t.Cleanup(func() { karpenterChartYAML = prev })
	dep := func(env string) []byte {
		return []byte(`apiVersion: apps/v1
kind: Deployment
metadata: {name: karpenter, namespace: kube-system}
spec:
  template:
    spec:
      containers:
        - name: controller
          env:
` + env)
	}
	karpenterChartYAML = dep("            - {name: CLUSTER_NAME, value: x}\n            - {name: CLUSTER_ENDPOINT, value: x}\n            - {name: AWS_REGION, value: x}\n")
	if _, err := karpenterChart("demo", "https://e", "q", "r", 2, false); err == nil || !strings.Contains(err.Error(), "no env INTERRUPTION_QUEUE") {
		t.Errorf("a missing env: %v", err)
	}
	karpenterChartYAML = dep("            - {name: CLUSTER_NAME, value: x}\n            - {name: CLUSTER_ENDPOINT, value: x}\n            - {name: AWS_REGION, value: x}\n            - {name: INTERRUPTION_QUEUE, value: x}\n            - {name: OTHER, value: '${OTHER}'}\n")
	if _, err := karpenterChart("demo", "https://e", "q", "r", 2, false); err == nil || !strings.Contains(err.Error(), "keeps a ${...} value") {
		t.Errorf("a ${...} left: %v", err)
	}
	karpenterChartYAML = []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: x, namespace: kube-system}\n")
	if _, err := karpenterChart("demo", "https://e", "q", "r", 2, false); err == nil || !strings.Contains(err.Error(), "does not apply v1 Secret") {
		t.Errorf("a Secret: %v", err)
	}
}
