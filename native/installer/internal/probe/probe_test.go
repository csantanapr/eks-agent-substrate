package probe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// fakeKubectl answers by the resource kind in the argv; a kind mapped to an
// error fails that read.
func fakeKubectl(answers map[string]string, fail map[string]error) Kubectl {
	return func(_ context.Context, args ...string) (string, error) {
		kind := ""
		for i, a := range args {
			if a == "get" && i+1 < len(args) {
				kind = args[i+1]
			}
		}
		if err := fail[kind]; err != nil {
			return "", err
		}
		return answers[kind], nil
	}
}

// recordJSON is a record listing plugins as kubectl prints it
// (plugin.RecordArgs), with only the plugins key, as older installers write it.
func recordJSON(plugins string) string {
	return `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"substrate-eks-plugins","namespace":"kube-system"},"data":{"plugins":"` + plugins + `"}}`
}

func checkThenParse(t *testing.T, k Kubectl) Result {
	t.Helper()
	line, err := Check(context.Background(), k)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	r, err := ParseInstalled([]string{"some log line", line})
	if err != nil {
		t.Fatalf("ParseInstalled(%q): %v", line, err)
	}
	return r
}

func TestAbsentClusterReadsReady(t *testing.T) {
	r := checkThenParse(t, fakeKubectl(map[string]string{}, nil))
	if r.Installed || r.Partial() || r.Badge() != "ready" {
		t.Errorf("absent cluster read as %+v (%s)", r, r.Badge())
	}
}

func TestNamespaceWithoutAteletIsPartial(t *testing.T) {
	r := checkThenParse(t, fakeKubectl(map[string]string{"namespace": "ate-system"}, nil))
	if !r.Partial() || r.Badge() != "partial install" {
		t.Errorf("namespace without atelet read as %+v (%s)", r, r.Badge())
	}
}

func TestInstalledClusterReportsItsVersion(t *testing.T) {
	r := checkThenParse(t, fakeKubectl(map[string]string{
		"namespace":  "ate-system",
		"daemonset":  "daemonset.apps/atelet",
		"deployment": "ghcr.io/agent-substrate/substrate/ate-api-server:v1.0.0@sha256:" + strings.Repeat("0", 64),
	}, nil))
	if !r.Installed || r.Partial() || r.Version != "v1.0.0" || r.Badge() != "installed v1.0.0" {
		t.Errorf("installed cluster read as %+v (%s)", r, r.Badge())
	}
}

// The image read is best effort: a failure leaves the version unknown but
// the cluster still reads installed.
func TestUnreadableImageStillReadsInstalled(t *testing.T) {
	r := checkThenParse(t, fakeKubectl(
		map[string]string{"namespace": "ate-system", "daemonset": "daemonset.apps/atelet"},
		map[string]error{"deployment": errors.New("forbidden")}))
	if !r.Installed || r.Version != "" || r.Badge() != "installed" {
		t.Errorf("read as %+v (%s)", r, r.Badge())
	}
}

// A failed namespace or DaemonSet read is an error, never "absent" or
// "partial": a cluster the probe cannot read must not be offered for install.
func TestFailedReadsAreErrors(t *testing.T) {
	for _, kind := range []string{"namespace", "nodes", "daemonset"} {
		k := fakeKubectl(map[string]string{"namespace": "ate-system"}, map[string]error{kind: errors.New("Unauthorized")})
		if line, err := Check(context.Background(), k); err == nil {
			t.Errorf("%s read failed but Check returned %q", kind, line)
		}
	}
	// The nodes' read fails with kubectl's own error, as before Karpenter.
	k := fakeKubectl(nil, map[string]error{"nodes": errors.New("kubectl get nodes: Unauthorized")})
	if _, err := Check(context.Background(), k); err == nil || err.Error() != "kubectl get nodes: Unauthorized" {
		t.Errorf("nodes read: %v", err)
	}
}

// Both ways: the plugin record is read, on a full or a partial install, and
// its plugins reported (none when the record is missing); a failed read
// leaves them unknown, never "none", and the cluster still reads installed.
func TestInstalledPluginsAreReadFromTheRecord(t *testing.T) {
	full := map[string]string{"namespace": "ate-system", "daemonset": "daemonset.apps/atelet",
		"deployment": "ghcr.io/x/ate-api-server:v1.0.0", "configmap": recordJSON("mode3,efs")}
	r := checkThenParse(t, fakeKubectl(full, nil))
	if !r.PluginsRead || strings.Join(r.Plugins, ",") != "efs,mode3" || r.Version != "v1.0.0" || !r.Atelet {
		t.Errorf("recorded plugins read as %+v", r)
	}
	partial := map[string]string{"namespace": "ate-system", "configmap": recordJSON("efs")}
	if r := checkThenParse(t, fakeKubectl(partial, nil)); !r.Partial() || !r.PluginsRead || strings.Join(r.Plugins, ",") != "efs" {
		t.Errorf("partial install's plugins read as %+v", r)
	}
	delete(full, "configmap")
	if r := checkThenParse(t, fakeKubectl(full, nil)); !r.PluginsRead || len(r.Plugins) != 0 {
		t.Errorf("no record read as %+v, want read with no plugins", r)
	}
	r = checkThenParse(t, fakeKubectl(full, map[string]error{"configmap": errors.New("forbidden")}))
	if r.PluginsRead || len(r.Plugins) != 0 || !r.Installed || r.Version != "v1.0.0" {
		t.Errorf("an unreadable record read as %+v, want unknown plugins on an installed cluster", r)
	}
}

func init() {
	plugin.Register(plugin.Plugin{ID: "probe-test-iso", Order: 1, FollowsNitroCompute: true})
}

// countingKubectl is k, counting its calls in n.
func countingKubectl(k Kubectl, n *int) Kubectl {
	return func(ctx context.Context, args ...string) (string, error) {
		*n++
		return k(ctx, args...)
	}
}

// Issue #22, both ways: without ate-system, a record that lists plugins,
// installed or pending, is read and reported with its NITRO_COMPUTE, and the
// cluster reads record-only, badged with what the record lists; it is not
// installed and not partial, so the install goes on there. No record, or
// one that lists no plugin, gives the line written before, byte for byte,
// and "ready". The probe makes one kubectl call more than before: four
// (namespace, nodes, Karpenter's CRDs, record), and five with the NodePool
// CRD, whose NodePools it also reads. A record-only cluster with Karpenter
// NodePools gets both sets of fields on its line.
func TestARecordWithoutTheControlPlaneIsReported(t *testing.T) {
	absent := func(rec string) map[string]string {
		return map[string]string{"nodes": "system nitro", "configmap": rec}
	}
	n := 0
	line, err := Check(context.Background(), countingKubectl(fakeKubectl(absent(
		`{"data":{"plugins":"efs","pending":"probe-test-iso","nitro-compute":"node-group","future-key":"x"}}`), nil), &n))
	if want := Marker + "false pools=nitro,system plugins=efs pending=probe-test-iso nitro-compute=node-group unknown=future-key"; err != nil || line != want || n != 4 {
		t.Fatalf("record-only: %q (%v, %d calls), want %q", line, err, n, want)
	}
	r, err := ParseInstalled([]string{line})
	if err != nil || r.Installed || r.Partial() || !r.RecordOnly() || !r.PluginsRead || r.NitroCompute != plugin.NitroNodeGroup ||
		strings.Join(r.Unknown, ",") != "future-key" || r.Record().CheckKeys() == nil ||
		strings.Join(r.Removed(), ",") != "efs,probe-test-iso" || !r.Record().Equal(plugin.Record{Plugins: []string{"efs"}, Pending: []string{"probe-test-iso"}, NitroCompute: plugin.NitroNodeGroup}) {
		t.Errorf("record-only read as %+v (%v)", r, err)
	}
	if got, want := r.Badge(), "partial install: plugins efs, probe-test-iso"; got != want {
		t.Errorf("badge %q, want %q", got, want)
	}
	if got, want := r.Recorded(), "efs, probe-test-iso (pending, node-group)"; got != want {
		t.Errorf("recorded %q, want %q", got, want)
	}
	// Installed only, and the NITRO_COMPUTE only on the plugin that follows it.
	r = checkThenParse(t, fakeKubectl(absent(`{"data":{"plugins":"efs,probe-test-iso","nitro-compute":"auto-mode"}}`), nil))
	if got, want := r.Recorded(), "efs, probe-test-iso (auto-mode)"; !r.RecordOnly() || got != want {
		t.Errorf("installed only: %+v, recorded %q, want %q", r, got, want)
	}
	for name, rec := range map[string]string{"no record": "", "no plugin": `{"data":{"plugins":"","nitro-compute":"auto-mode"}}`} {
		n = 0
		line, err := Check(context.Background(), countingKubectl(fakeKubectl(absent(rec), nil), &n))
		if err != nil || line != Marker+"false pools=nitro,system" || n != 4 {
			t.Errorf("%s: %q (%v, %d calls)", name, line, err, n)
		}
		if r := checkThenParse(t, fakeKubectl(absent(rec), nil)); r.RecordOnly() || r.Badge() != "ready" {
			t.Errorf("%s read as %+v (%s)", name, r, r.Badge())
		}
	}
	karp := absent(`{"data":{"plugins":"","pending":"probe-test-iso","nitro-compute":"node-group"}}`)
	karp["crd"] = "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh\ncustomresourcedefinition.apiextensions.k8s.io/ec2nodeclasses.karpenter.k8s.aws\n"
	karp["nodepools.karpenter.sh"] = `{"items":[{"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"gvisor"}}}}}]}`
	n = 0
	line, err = Check(context.Background(), countingKubectl(fakeKubectl(karp, nil), &n))
	if want := Marker + "false pools=gvisor,nitro,system nodepools=gvisor karpenter plugins= pending=probe-test-iso nitro-compute=node-group"; err != nil || line != want || n != 5 {
		t.Fatalf("record-only with Karpenter: %q (%v, %d calls), want %q", line, err, n, want)
	}
	if r := checkThenParse(t, fakeKubectl(karp, nil)); !r.RecordOnly() || !r.Karpenter || strings.Join(r.NodePools, ",") != "gvisor" ||
		strings.Join(r.Pools, ",") != "gvisor,nitro,system" || r.NitroCompute != plugin.NitroNodeGroup || strings.Join(r.Removed(), ",") != "probe-test-iso" {
		t.Errorf("record-only with Karpenter read as %+v", r)
	}
}

// Without ate-system the record read is not best effort: a denied read, or
// one that cannot be parsed, stops the probe as a failed namespace read
// does, since only the record tells such a cluster from a clean one.
func TestAFailedRecordReadWithoutTheControlPlaneIsAnError(t *testing.T) {
	if line, err := Check(context.Background(), fakeKubectl(map[string]string{}, map[string]error{"configmap": errors.New("Forbidden")})); err == nil {
		t.Errorf("denied record read: %q", line)
	}
	if line, err := Check(context.Background(), fakeKubectl(map[string]string{"configmap": "efs"}, nil)); err == nil {
		t.Errorf("unparsable record: %q", line)
	}
}

// The record's pending plugins are reported, and removed with the installed
// ones (Removed), when there are some; without, and with a NITRO_COMPUTE,
// the line is the one written before, byte for byte. A record that cannot
// be read as the ConfigMap leaves the plugins unknown, as a failed read
// does.
func TestPendingPluginsAreReported(t *testing.T) {
	base := map[string]string{"namespace": "ate-system", "daemonset": "daemonset.apps/atelet", "nodes": "nitro system"}
	with := func(rec string) map[string]string {
		m := map[string]string{"configmap": rec}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	line, err := Check(context.Background(), fakeKubectl(with(`{"data":{"plugins":"efs","pending":"mode3-iso","nitro-compute":"node-group"}}`), nil))
	if err != nil || line != Marker+"true pools=nitro,system atelet plugins=efs pending=mode3-iso" {
		t.Errorf("pending: %q, %v", line, err)
	}
	r, err := ParseInstalled([]string{line})
	if err != nil || !r.PluginsRead || strings.Join(r.Plugins, ",") != "efs" || strings.Join(r.Pending, ",") != "mode3-iso" ||
		strings.Join(r.Removed(), ",") != "efs,mode3-iso" {
		t.Errorf("pending read as %+v (%v)", r, err)
	}
	line, err = Check(context.Background(), fakeKubectl(with(`{"data":{"plugins":"mode3-iso","nitro-compute":"auto-mode","future-key":"x"}}`), nil))
	if err != nil || line != Marker+"true pools=nitro,system atelet plugins=mode3-iso" {
		t.Errorf("no pending: %q, %v", line, err)
	}
	if r := checkThenParse(t, fakeKubectl(with(`{"data":{"pending":"mode3-iso"}}`), nil)); !r.PluginsRead || strings.Join(r.Removed(), ",") != "mode3-iso" {
		t.Errorf("pending only: %+v", r)
	}
	if r := checkThenParse(t, fakeKubectl(with("mode3-iso"), nil)); r.PluginsRead || len(r.Removed()) != 0 || !r.Installed {
		t.Errorf("an unreadable record read as %+v, want unknown plugins", r)
	}
}

// Both ways: the node groups of the cluster's nodes are reported, sorted and
// once each, on a clean cluster and an installed one alike; a cluster with
// no labeled node reports none.
func TestPoolsAreReadFromTheNodes(t *testing.T) {
	for name, answers := range map[string]map[string]string{
		"absent":    {"nodes": "system gvisor system isolated"},
		"installed": {"nodes": "system gvisor system isolated", "namespace": "ate-system", "daemonset": "daemonset.apps/atelet"},
	} {
		if r := checkThenParse(t, fakeKubectl(answers, nil)); strings.Join(r.Pools, ",") != "gvisor,isolated,system" || r.Installed != (name == "installed") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if r := checkThenParse(t, fakeKubectl(map[string]string{}, nil)); len(r.Pools) != 0 {
		t.Errorf("no labeled node read as pools %v", r.Pools)
	}
}

func TestParseInstalledNeedsTheMarker(t *testing.T) {
	if _, err := ParseInstalled([]string{"error: You must be logged in"}); err == nil {
		t.Error("parsed a probe that printed no marker")
	}
}

func TestImageTagVersion(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/agent-substrate/substrate/ate-api-server:v1.0.0":            "v1.0.0",
		"registry:5000/ate-api-server:v1.0.1@sha256:abc":                     "v1.0.1",
		"registry:5000/ate-api-server@sha256:abc":                            "",
		"ko.local/ate-api-server:latest":                                     "",
		"111122223333.dkr.ecr.us-west-2.amazonaws.com/x/ate-api-server:v2.0": "v2.0",
	} {
		if got := imageTagVersion(ref); got != want {
			t.Errorf("imageTagVersion(%q) = %q, want %q", ref, got, want)
		}
	}
}

// The dry run's fixtures cover each badge.
func TestSimLinesCoverEveryState(t *testing.T) {
	for name, want := range map[string]string{"x-installed": "installed v1.0.0", "x-partial": "partial install",
		"x-leftover": "partial install: plugins efs, mode3-iso", "x": "ready"} {
		r, err := ParseInstalled([]string{SimLine(name)})
		if err != nil || r.Badge() != want {
			t.Errorf("%s: %+v %v, want %s", name, r, err, want)
		}
	}
}

// Both ways: a cluster without Karpenter's CRDs reports what it did before,
// to the byte. With them, the pools of the NodePools this installer made
// join the nodes' (a pool can have no node yet), and the line names them and
// Karpenter. EKS Auto Mode's CRD alone, with NodePools that label no pool,
// reports neither.
func TestPoolsIncludeKarpenterNodePools(t *testing.T) {
	base := map[string]string{"nodes": "system microvm", "namespace": "ate-system", "daemonset": "daemonset.apps/atelet"}
	line, err := Check(context.Background(), fakeKubectl(base, nil))
	if err != nil || line != Marker+"true pools=microvm,system atelet plugins=" {
		t.Errorf("no CRD: %q %v", line, err)
	}
	auto := map[string]string{"nodes": "system", "crd": "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh",
		"nodepools.karpenter.sh": `{"items":[{"spec":{"template":{"metadata":{"labels":{"team":"a"}}}}}]}`}
	if line, err := Check(context.Background(), fakeKubectl(auto, nil)); err != nil || line != Marker+"false pools=system" {
		t.Errorf("Auto Mode's CRD: %q %v", line, err)
	}
	karp := map[string]string{"nodes": "system microvm",
		"crd":                    "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh\ncustomresourcedefinition.apiextensions.k8s.io/ec2nodeclasses.karpenter.k8s.aws\n",
		"nodepools.karpenter.sh": `{"items":[{"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"gvisor"}}}}},{"spec":{"template":{"metadata":{"labels":{"ate.dev/pool":"microvm"}}}}}]}`}
	line, err = Check(context.Background(), fakeKubectl(karp, nil))
	if err != nil || line != Marker+"false pools=gvisor,microvm,system nodepools=gvisor,microvm karpenter" {
		t.Fatalf("Karpenter: %q %v", line, err)
	}
	r := checkThenParse(t, fakeKubectl(karp, nil))
	if strings.Join(r.Pools, ",") != "gvisor,microvm,system" || strings.Join(r.NodePools, ",") != "gvisor,microvm" || !r.Karpenter || r.Installed {
		t.Errorf("parsed %+v", r)
	}
	if r := checkThenParse(t, fakeKubectl(base, nil)); r.Karpenter || r.NodePools != nil {
		t.Errorf("no CRD parsed %+v", r)
	}
}

// A failed read of the CRDs or of the NodePools stops the probe, as a failed
// node read does: a cluster whose pools cannot be read is not shown to lack
// them.
func TestFailedKarpenterReadsAreErrors(t *testing.T) {
	answers := map[string]string{"crd": "customresourcedefinition.apiextensions.k8s.io/nodepools.karpenter.sh"}
	for _, kind := range []string{"crd", "nodepools.karpenter.sh"} {
		if line, err := Check(context.Background(), fakeKubectl(answers, map[string]error{kind: errors.New("Forbidden")})); err == nil {
			t.Errorf("%s read failed but Check returned %q", kind, line)
		}
	}
}
