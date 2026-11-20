package cmd

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

// overlayFake is karpenterFake with the Nitro Sandbox AMI nitroAMI.
func overlayFake(t *testing.T) *clusterFake {
	f := karpenterFake(t)
	im := testImage(ec2types.ArchitectureValuesX8664, "/dev/xvda", 30)
	im.ImageId, im.Name = aws.String(nitroAMI), aws.String("amazon-eks-node-al2023-x86_64-pod-isolation-1.37")
	f.images[nitroAMI] = im
	return f
}

// withOverlayNitro is KARPENTER_NODE_OVERLAY=on with the nitro group too:
// the four groups, the nitro one on nitroAMI.
func withOverlayNitro(c *clusterConfig) { withOverlay(c); withNitro(c) }

// gateOf is the controller's FEATURE_GATES in the cluster.
func gateOf(t *testing.T, f *clusterFake) string {
	t.Helper()
	dep := f.objects.get(kindDeployment, "kube-system", "karpenter")
	for _, c := range dep.field("spec", "template", "spec", "containers").([]any) {
		for _, e := range c.(map[string]any)["env"].([]any) {
			if em := e.(map[string]any); em["name"] == featureGate {
				return em["value"].(string)
			}
		}
	}
	t.Fatal("no FEATURE_GATES")
	return ""
}

// A fresh account with KARPENTER_NODE_OVERLAY=on and the four groups: the
// system node group alone, and no launch template of a sandbox group; the
// cluster marked node-overlay; the chart with the NodeOverlay gate on; the
// gvisor, microvm and nitro pools, each with its EC2NodeClass and NodePool,
// and the microvm and nitro ones with their NodeOverlay, after their
// NodePool. The nitro pool runs NITRO_AMI_ID with Nitro Enclaves, and its
// nodes get both of the RuntimeClass's labels. Nothing waits for Nitro
// sandbox capacity: the pool starts with no node.
func TestCreateClusterNodeOverlay(t *testing.T) {
	f := overlayFake(t)
	logs := captureLog(t)
	r := clusterTestRun(t, f, false, withOverlayNitro)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got := sortedKeysOf(f.ngIn); !slices.Equal(got, []string{"system"}) {
		t.Errorf("node groups %v, want system only", got)
	}
	for _, w := range f.writes {
		for _, g := range []string{"gvisor", "microvm", "nitro"} {
			if strings.HasPrefix(w, "CreateLaunchTemplate ") && strings.Contains(w, g) {
				t.Errorf("a launch template of the %s pool: %s", g, w)
			}
		}
	}
	if f.clusterIn.Tags[nodegroups.KarpenterTagKey] != "node-overlay" {
		t.Errorf("cluster tags %v", f.clusterIn.Tags)
	}
	if f.kubeCalls != 0 || slices.Contains(f.reads, "ListNodes") || strings.Contains(logs.String(), "advertise Nitro sandbox capacity") {
		t.Errorf("waited for the nitro pool's capacity: %d node listers, reads %v", f.kubeCalls, f.reads)
	}

	// The gate, and nothing else of the chart's FEATURE_GATES.
	want := strings.Replace(karpenterChartGates(t), "NodeOverlay=false", "NodeOverlay=true", 1)
	if g := gateOf(t, f); g != want || !strings.Contains(g, "NodeOverlay=true") {
		t.Errorf("FEATURE_GATES %q, want %q", g, want)
	}

	kf := f.objects
	if len(kf.objs) != 18+3*2+2 {
		t.Errorf("%d objects, want 26", len(kf.objs))
	}
	for p, o := range kf.objs {
		if !ownedObject(o) {
			t.Errorf("%s is not labeled %s=%s", p, managedByKey, managedByValue)
		}
	}
	for _, pool := range []string{"gvisor", "microvm", "nitro"} {
		nc := kf.get(kindEC2NodeClass, "", pool)
		if nc == nil || kf.get(kindNodePool, "", pool) == nil {
			t.Fatalf("no EC2NodeClass or NodePool %s", pool)
		}
		cpu, enclave := asJSON(t, nc.field("spec", "cpuOptions")), asJSON(t, nc.field("spec", "enclaveOptions"))
		wantCPU := map[string]string{"gvisor": "null", "microvm": `{"nestedVirtualization":"enabled"}`, "nitro": "null"}[pool]
		wantEnclave := map[string]string{"gvisor": "null", "microvm": "null", "nitro": `{"enabled":true}`}[pool]
		if cpu != wantCPU || enclave != wantEnclave {
			t.Errorf("%s: cpuOptions %s, enclaveOptions %s; want %s, %s", pool, cpu, enclave, wantCPU, wantEnclave)
		}
		size := map[string]string{"gvisor": "200Gi", "microvm": "200Gi", "nitro": "100Gi"}[pool]
		if v := nc.field("spec", "blockDeviceMappings").([]any)[0].(map[string]any)["ebs"].(map[string]any)["volumeSize"]; v != size {
			t.Errorf("%s root volume %v, want %s", pool, v, size)
		}
	}
	if amiOf(f, "nitro") != nitroAMI || amiOf(f, "gvisor") != eksAMIx86 || amiOf(f, "microvm") != eksAMIx86 {
		t.Errorf("AMIs: gvisor %v, microvm %v, nitro %v", amiOf(f, "gvisor"), amiOf(f, "microvm"), amiOf(f, "nitro"))
	}
	np := kf.get(kindNodePool, "", "nitro")
	wantNitro := map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"ate.dev/pool": "nitro", "eks.amazonaws.com/nitro-sandbox": "true"}},
			"spec": map[string]any{
				"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": "nitro"},
				"requirements": []any{
					map[string]any{"key": "node.kubernetes.io/instance-type", "operator": "In", "values": []any{"m8i.xlarge"}},
					map[string]any{"key": "karpenter.sh/capacity-type", "operator": "In", "values": []any{"on-demand"}},
					map[string]any{"key": "kubernetes.io/arch", "operator": "In", "values": []any{"amd64"}},
					map[string]any{"key": "kubernetes.io/os", "operator": "In", "values": []any{"linux"}},
				},
				"taints":                 []any{map[string]any{"key": "ate.dev/sandboxClass", "value": "nitro", "effect": "NoSchedule"}},
				"expireAfter":            "Never",
				"terminationGracePeriod": "15m",
			},
		},
		"limits":     map[string]any{"nodes": "1", "cpu": "4"},
		"disruption": map[string]any{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "5m"},
	}
	if asJSON(t, np["spec"]) != asJSON(t, wantNitro) {
		t.Errorf("NodePool nitro:\n got %s\nwant %s", asJSON(t, np["spec"]), asJSON(t, wantNitro))
	}
	if l := asJSON(t, kf.get(kindNodePool, "", "microvm").field("spec", "template", "metadata", "labels")); l != `{"ate.dev/pool":"microvm"}` {
		t.Errorf("microvm NodePool labels %s", l)
	}

	// The NodeOverlays: one per pool that needs one, scoped to its NodePool.
	if kf.get(kindNodeOverlay, "", "gvisor") != nil {
		t.Error("a gvisor NodeOverlay: gVisor workers request no extended resource")
	}
	for pool, capacity := range map[string]string{"microvm": `{"ate.dev/kvm":"4096"}`, "nitro": `{"eks.amazonaws.com/nitro-sandbox":"20"}`} {
		o := kf.get(kindNodeOverlay, "", pool)
		if o == nil {
			t.Fatalf("no NodeOverlay %s", pool)
		}
		spec := asJSON(t, o["spec"])
		want := `{"capacity":` + capacity + `,"requirements":[{"key":"karpenter.sh/nodepool","operator":"In","values":["` + pool + `"]}]}`
		if spec != want || o.str("apiVersion") != "karpenter.sh/v1alpha1" {
			t.Errorf("NodeOverlay %s: %s %s, want %s", pool, o.str("apiVersion"), spec, want)
		}
	}
	w := kf.writes()
	at := func(k kubeKind, name string) int { return slices.Index(w, "PATCH "+k.path("", name)) }
	dep := slices.Index(w, "PATCH "+kindDeployment.path("kube-system", "karpenter"))
	for _, pool := range []string{"microvm", "nitro"} {
		if !(dep < at(kindEC2NodeClass, pool) && at(kindEC2NodeClass, pool) < at(kindNodePool, pool) && at(kindNodePool, pool) < at(kindNodeOverlay, pool)) {
			t.Errorf("%s: Kubernetes writes, in order:\n  %s", pool, strings.Join(w, "\n  "))
		}
	}
	// Each one was read before anything was written, and waited on.
	if i := slices.Index(kf.calls, "GET "+kindNodeOverlay.path("", "nitro")); i < 0 || i > slices.Index(kf.calls, "PATCH "+kindCRD.path("", "capacitybuffers.autoscaling.x-k8s.io")) {
		t.Errorf("NodeOverlay nitro not read before the first write: %v", kf.calls)
	}
	if !slices.Contains(r.creates, "Kubernetes NodeOverlay nitro") || !slices.Contains(r.creates, "Kubernetes NodeOverlay microvm") {
		t.Errorf("creates %v", r.creates)
	}
	list := phases.ClusterFor(nodegroups.Managed, true)
	for i := range list {
		if !strings.Contains(logs.String(), phases.Line(list, i)) {
			t.Errorf("no line %q", phases.Line(list, i))
		}
	}
	if !strings.Contains(logs.String(), `msg="Karpenter is ready" pools=3 nodeOverlays=2`) {
		t.Errorf("no ready line:\n%s", logs)
	}
	if strings.Contains(logs.String(), "111122223333") {
		t.Errorf("the log carries the account id")
	}
}

// karpenterChartGates is FEATURE_GATES as the embedded chart renders it.
func karpenterChartGates(t *testing.T) string {
	t.Helper()
	objs, err := karpenterChart("demo", "https://e", "q", "r", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.str("kind") != "Deployment" {
			continue
		}
		for _, c := range o.field("spec", "template", "spec", "containers").([]any) {
			for _, e := range c.(map[string]any)["env"].([]any) {
				if em := e.(map[string]any); em["name"] == featureGate {
					return em["value"].(string)
				}
			}
		}
	}
	t.Fatal("no FEATURE_GATES in the chart")
	return ""
}

// The chart's NodeOverlay gate: off as rendered and with KARPENTER=on alone;
// on with KARPENTER_NODE_OVERLAY=on, the other gates as they are. A chart
// whose FEATURE_GATES lacks the gate, or has it twice, is refused.
func TestKarpenterChartNodeOverlayGate(t *testing.T) {
	off := karpenterChartGates(t)
	if strings.Count(off, "NodeOverlay=false") != 1 || strings.Contains(off, "NodeOverlay=true") {
		t.Fatalf("the chart's gates %q", off)
	}
	on, err := karpenterChart("demo", "https://e", "q", "r", 2, true)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := karpenterChart("demo", "https://e", "q", "r", 2, false)
	for i, o := range on {
		if o.str("kind") != "Deployment" {
			if asJSON(t, o) != asJSON(t, plain[i]) {
				t.Errorf("%s changed with the gate", describe(o))
			}
			continue
		}
		got := asJSON(t, o)
		if want := strings.Replace(asJSON(t, plain[i]), "NodeOverlay=false", "NodeOverlay=true", 1); got != want {
			t.Errorf("the Deployment differs in more than the gate:\n got %s\nwant %s", got, want)
		}
	}
	prev := karpenterChartYAML
	t.Cleanup(func() { karpenterChartYAML = prev })
	for name, gates := range map[string]string{"no gate": "ReservedCapacity=true", "twice": "NodeOverlay=false,NodeOverlay=false"} {
		karpenterChartYAML = []byte(strings.Replace(string(prev), off, gates, 1))
		if _, err := karpenterChart("demo", "https://e", "q", "r", 2, true); err == nil || !strings.Contains(err.Error(), "FEATURE_GATES has") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := karpenterChart("demo", "https://e", "q", "r", 2, false); err != nil {
			t.Errorf("%s, without the overlay: %v", name, err)
		}
	}
}

// matches reports what Karpenter v1.15.1's NodeOverlay controller decides
// for an overlay and one NodePool's instance type (getOverlaidOfferings):
// the instance type's labels are its requirements, karpenter.sh/nodepool
// with the NodePool's name, and the NodePool's template labels; each of the
// overlay's In requirements must hold one of them.
func matches(overlay, nodePool kubeObject) bool {
	labels := map[string]string{karpenterPoolTag: nodePool.name()}
	for k, v := range nodePool.field("spec", "template", "metadata", "labels").(map[string]any) {
		labels[k] = v.(string)
	}
	for _, r := range nodePool.field("spec", "template", "spec", "requirements").([]any) {
		rm := r.(map[string]any)
		labels[rm["key"].(string)] = rm["values"].([]any)[0].(string)
	}
	for _, r := range overlay.field("spec", "requirements").([]any) {
		rm := r.(map[string]any)
		v, ok := labels[rm["key"].(string)]
		if rm["operator"] != "In" || !ok || !slices.Contains(rm["values"].([]any), any(v)) {
			return false
		}
	}
	return true
}

// Each NodeOverlay applies to its own NodePool only, even where two pools
// share an instance type (gvisor and microvm are both m8i.2xlarge by
// default): it requires karpenter.sh/nodepool, not the instance type. Were
// it to apply to another pool as well, that would add capacity Karpenter
// only simulates, harmless only because each worker Pod selects its own
// pool's labels; scoped by NodePool, it never does.
func TestNodeOverlayScope(t *testing.T) {
	f := overlayFake(t)
	if err := clusterTestRun(t, f, false, withOverlayNitro, func(c *clusterConfig) { c.NitroInstanceType = "m8i.2xlarge" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, o := range []string{"microvm", "nitro"} {
		overlay := f.objects.get(kindNodeOverlay, "", o)
		for _, p := range []string{"gvisor", "microvm", "nitro"} {
			if got := matches(overlay, f.objects.get(kindNodePool, "", p)); got != (o == p) {
				t.Errorf("NodeOverlay %s applies to NodePool %s: %v", o, p, got)
			}
		}
	}
	// The instance type alone would not tell the pools apart.
	byType := kubeObject{"spec": map[string]any{"requirements": []any{map[string]any{"key": "node.kubernetes.io/instance-type", "operator": "In", "values": []any{"m8i.2xlarge"}}}}}
	if !matches(byType, f.objects.get(kindNodePool, "", "gvisor")) || !matches(byType, f.objects.get(kindNodePool, "", "microvm")) {
		t.Error("internal: matches does not see the shared instance type")
	}
}

// A rerun writes nothing and reads no SSM parameter: each pool keeps the
// AMI of its EC2NodeClass, the nitro one too, whatever NITRO_AMI_ID names
// later (with a warning that names NITRO_AMI_ID).
func TestCreateClusterNodeOverlayRerun(t *testing.T) {
	f := overlayFake(t)
	if err := clusterTestRun(t, f, false, withOverlayNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes, f.objects.calls = nil, nil
	f.deny["GetParameter"] = "AccessDenied"
	r := clusterTestRun(t, f, false, withOverlayNitro)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(r.creates) != 0 || len(r.changes) != 0 || len(f.objects.writes()) != 0 {
		t.Errorf("rerun: writes %v, creates %v, changes %v, Kubernetes writes %v", f.writes, r.creates, r.changes, f.objects.writes())
	}
	if !maps.Equal(r.seeded, map[string]bool{"gvisor": true, "microvm": true, "nitro": true}) {
		t.Errorf("seeded %v", r.seeded)
	}
	const newer = "ami-0a17000000000000f"
	im := f.images[nitroAMI]
	im.ImageId = aws.String(newer)
	f.images[newer] = im
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withOverlayNitro, func(c *clusterConfig) { c.NitroAMIID = newer }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if amiOf(f, "nitro") != nitroAMI || !strings.Contains(logs.String(), "keeps the AMI of its EC2NodeClass, not NITRO_AMI_ID") ||
		!strings.Contains(logs.String(), "NITRO_AMI_ID="+newer) {
		t.Errorf("nitro runs %v; log:\n%s", amiOf(f, "nitro"), logs)
	}
	if len(f.objects.writes()) != 0 {
		t.Errorf("Kubernetes writes %v", f.objects.writes())
	}
}

// A rerun puts back a NodeOverlay changed in the cluster, as a change.
func TestCreateClusterNodeOverlayReconciles(t *testing.T) {
	f := overlayFake(t)
	if err := clusterTestRun(t, f, false, withOverlayNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := f.objects.get(kindNodeOverlay, "", "nitro")
	o["spec"].(map[string]any)["capacity"] = map[string]any{nodegroups.NitroSandboxResource: "2"}
	r := clusterTestRun(t, f, false, withOverlayNitro)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.changes, []string{"apply Kubernetes NodeOverlay nitro"}) {
		t.Errorf("changes %v", r.changes)
	}
	if c := asJSON(t, f.objects.get(kindNodeOverlay, "", "nitro").field("spec", "capacity")); c != `{"eks.amazonaws.com/nitro-sandbox":"20"}` {
		t.Errorf("capacity %s", c)
	}
}

// A NodeOverlay of the pools' names that setup-aws did not make (no
// ownership label) stops the run before anything is applied; one that is
// not Ready (a conflict with another overlay) stops it with why.
func TestCreateClusterNodeOverlayRefusesForeignAndWaits(t *testing.T) {
	f := overlayFake(t)
	f.objects.put(kubeObject{"apiVersion": kindCRD.APIVersion, "kind": kindCRD.Kind,
		"metadata": map[string]any{"name": "nodeoverlays.karpenter.sh", "labels": map[string]any{managedByKey: managedByValue}}})
	f.objects.put(kubeObject{"apiVersion": kindNodeOverlay.APIVersion, "kind": kindNodeOverlay.Kind, "metadata": map[string]any{"name": "microvm"}})
	err := clusterTestRun(t, f, false, withOverlayNitro).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Kubernetes NodeOverlay microvm exists without the label app.kubernetes.io/managed-by=substrate-eks") {
		t.Errorf("a foreign NodeOverlay: %v", err)
	}
	if w := f.objects.writes(); len(w) != 0 {
		t.Errorf("Kubernetes writes %v", w)
	}

	shortKarpenterWaits(t)
	f = overlayFake(t)
	f.objects.notReady[kindNodeOverlay.path("", "nitro")] = "conflict with another overlay"
	err = clusterTestRun(t, f, false, withOverlayNitro).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "NodeOverlay nitro") || !strings.Contains(err.Error(), "conflict with another overlay") {
		t.Errorf("a NodeOverlay not Ready: %v", err)
	}
}

// --plan on a fresh account lists the NodeOverlays with the rest, and
// writes nothing; on the complete deployment it has nothing to do.
func TestCreateClusterNodeOverlayPlan(t *testing.T) {
	f := overlayFake(t)
	r := clusterTestRun(t, f, true, withOverlayNitro)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Kubernetes NodeOverlay microvm", "Kubernetes NodeOverlay nitro", "Kubernetes NodePool nitro", "Kubernetes EC2NodeClass nitro"} {
		if !slices.Contains(r.creates, want) {
			t.Errorf("plan: no %q in %v", want, r.creates)
		}
	}
	if len(f.writes) != 0 || len(f.objects.calls) != 0 {
		t.Errorf("plan wrote %v, Kubernetes %v", f.writes, f.objects.calls)
	}
	f = overlayFake(t)
	if err := clusterTestRun(t, f, false, withOverlayNitro).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes, f.objects.calls = nil, nil
	p := clusterTestRun(t, f, true, withOverlayNitro)
	if err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.creates)+len(p.changes) != 0 || len(f.writes) != 0 || len(f.objects.writes()) != 0 {
		t.Errorf("plan on the complete deployment: creates %v, changes %v", p.creates, p.changes)
	}
}

// What KARPENTER_NODE_OVERLAY refuses, each before anything is read or
// written: no KARPENTER=on, a value, and the nitro pool's settings as the
// nitro group's (NITRO_AMI_ID, Nitro Enclaves, its architecture, a limit).
func TestCreateClusterNodeOverlayRefusals(t *testing.T) {
	for _, c := range []struct {
		name, want string
		reads      bool
		edit       func(*clusterFake, *clusterConfig)
	}{
		{"no KARPENTER", "KARPENTER_NODE_OVERLAY=on needs KARPENTER=on", false,
			func(_ *clusterFake, c *clusterConfig) { c.AutoMode, c.KarpenterNodeOverlay = "", "on" }},
		{"KARPENTER=off", `KARPENTER_NODE_OVERLAY=on needs KARPENTER=on, and KARPENTER="off"`, false,
			func(_ *clusterFake, c *clusterConfig) {
				c.AutoMode, c.Karpenter, c.KarpenterNodeOverlay = "", "off", "on"
			}},
		{"a value", `KARPENTER_NODE_OVERLAY must be "on", "off" or empty, not "yes"`, false,
			func(_ *clusterFake, c *clusterConfig) { withKarpenter(c); c.KarpenterNodeOverlay = "yes" }},
		{"no NITRO_AMI_ID", "NITRO_AMI_ID is empty, and the nitro NodePool needs a Nitro Sandbox AMI", true,
			func(_ *clusterFake, c *clusterConfig) { withOverlayNitro(c); c.NitroAMIID = "" }},
		{"a nitro pool limit of 0", "the nitro NodePool needs a limit of at least one node (NITRO_NODES)", false,
			func(_ *clusterFake, c *clusterConfig) { withOverlayNitro(c); c.NitroNodes = 0 }},
		{"no Nitro Enclaves", "t3.xlarge does not support Nitro Enclaves, which the nitro NodePool needs", true,
			func(_ *clusterFake, c *clusterConfig) { withOverlayNitro(c); c.NitroInstanceType = "t3.xlarge" }},
		{"another architecture", "NITRO_AMI_ID=" + nitroAMI + " is arm64, but the nitro NodePool's m8i.xlarge runs x86_64", true,
			func(f *clusterFake, c *clusterConfig) {
				withOverlayNitro(c)
				im := f.images[nitroAMI]
				im.Architecture = ec2types.ArchitectureValuesArm64
				f.images[nitroAMI] = im
			}},
		{"no such AMI", "NITRO_AMI_ID=ami-0b17000000000000e: no such AMI", true,
			func(_ *clusterFake, c *clusterConfig) { withOverlayNitro(c); c.NitroAMIID = "ami-0b17000000000000e" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := overlayFake(t)
			err := clusterTestRun(t, f, false, func(cc *clusterConfig) { c.edit(f, cc) }).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if len(f.writes) != 0 || len(f.objects.calls) != 0 || (!c.reads && len(f.reads) != 0) {
				t.Errorf("refused after reads %v, writes %v, Kubernetes %v", f.reads, f.writes, f.objects.calls)
			}
		})
	}
}

// KARPENTER_NODE_OVERLAY is fixed when the cluster is made: the marker's
// value tells the two modes apart, and a run of the other mode, or without
// Karpenter, is refused before it writes; so is a value setup-aws does not
// set, with Karpenter. Without Karpenter such a value is ignored, as before.
func TestCreateClusterNodeOverlayMarker(t *testing.T) {
	made := func(edit func(*clusterConfig)) *clusterFake {
		f := overlayFake(t)
		if err := clusterTestRun(t, f, false, edit).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.writes = nil
		return f
	}
	noKarpenter := func(c *clusterConfig) { c.AutoMode = "" }
	for _, c := range []struct {
		name       string
		made, then func(*clusterConfig)
		want       string
	}{
		{"node-overlay, then on", withOverlay, withKarpenter, `was made with KARPENTER_NODE_OVERLAY=on, but KARPENTER_NODE_OVERLAY=""`},
		{"node-overlay, then off", withOverlay, func(c *clusterConfig) { withKarpenter(c); c.KarpenterNodeOverlay = "off" }, `but KARPENTER_NODE_OVERLAY="off"`},
		{"node-overlay, then no Karpenter", withOverlay, noKarpenter, `was made with KARPENTER=on and KARPENTER_NODE_OVERLAY=on, but KARPENTER=""; set KARPENTER=on and KARPENTER_NODE_OVERLAY=on to match, or `},
		{"node-overlay, then Karpenter off", withOverlay, func(c *clusterConfig) { noKarpenter(c); c.Karpenter = "off" }, `but KARPENTER="off"; set KARPENTER=on and KARPENTER_NODE_OVERLAY=on to match`},
		{"on, then node-overlay", withKarpenter, withOverlay, "was made without KARPENTER_NODE_OVERLAY, which applies to a new cluster only"},
		{"no Karpenter, then node-overlay", noKarpenter, withOverlay, "was made without Karpenter, and KARPENTER=on applies to a new cluster only"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := made(c.made)
			err := clusterTestRun(t, f, false, c.then).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) || len(f.writes) != 0 {
				t.Errorf("got %v, want %q; writes %v", err, c.want, f.writes)
			}
		})
	}
	f := made(withOverlay)
	if err := clusterTestRun(t, f, false, withOverlay).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("the same mode again: %v, writes %v", err, f.writes)
	}
	for _, edit := range []func(*clusterConfig){withKarpenter, withOverlay} {
		f := made(noKarpenter)
		f.cluster.Tags[nodegroups.KarpenterTagKey] = "maybe"
		err := clusterTestRun(t, f, false, edit).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), `carries agent-substrate-on-eks/karpenter="maybe", which setup-aws does not set`) || len(f.writes) != 0 {
			t.Errorf("an unknown marker: %v, writes %v", err, f.writes)
		}
	}
	f = made(noKarpenter)
	f.cluster.Tags[nodegroups.KarpenterTagKey] = "maybe"
	if err := clusterTestRun(t, f, false, noKarpenter).run(context.Background()); err != nil {
		t.Errorf("an unknown marker without Karpenter: %v", err)
	}
}

// With self-managed node groups, the system group is the one Auto Scaling
// group; the three pools are Karpenter's, and nothing lists the nitro
// nodes.
func TestCreateClusterNodeOverlaySelfManaged(t *testing.T) {
	f := overlayFake(t)
	logs := captureLog(t)
	if err := clusterTestRun(t, f, false, withOverlayNitro, selfManaged).run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got := sortedKeysOf(f.asgIn); !slices.Equal(got, []string{"demo-system"}) {
		t.Errorf("Auto Scaling groups %v", got)
	}
	if strings.Contains(logs.String(), "ate.dev/pool=nitro") || strings.Contains(logs.String(), "advertise Nitro sandbox capacity") {
		t.Errorf("waited for nitro nodes:\n%s", logs)
	}
	for _, p := range []string{"gvisor", "microvm", "nitro"} {
		if f.objects.get(kindNodePool, "", p) == nil {
			t.Errorf("no NodePool %s", p)
		}
	}
	list := phases.ClusterFor(nodegroups.SelfManaged, true)
	for i := range list {
		if !strings.Contains(logs.String(), phases.Line(list, i)) {
			t.Errorf("no line %q", phases.Line(list, i))
		}
	}
}

// KARPENTER_NODE_OVERLAY=on without the nitro group: the microvm pool and
// its NodeOverlay, and nothing of nitro, which needs no NITRO_AMI_ID then.
func TestCreateClusterNodeOverlayWithoutNitro(t *testing.T) {
	f := overlayFake(t)
	if err := clusterTestRun(t, f, false, withOverlay).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.objects.get(kindNodePool, "", "microvm") == nil || f.objects.get(kindNodeOverlay, "", "microvm") == nil ||
		f.objects.get(kindNodePool, "", "nitro") != nil || f.objects.get(kindNodeOverlay, "", "nitro") != nil || len(f.objects.objs) != 18+2*2+1 {
		t.Errorf("objects %v", sortedKeysOf(f.objects.objs))
	}
	if slices.ContainsFunc(f.inputs, func(s string) bool { return strings.Contains(s, nitroAMI) }) {
		t.Errorf("read the Nitro AMI: %v", f.inputs)
	}
}
