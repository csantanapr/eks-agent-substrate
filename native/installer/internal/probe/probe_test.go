package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
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
// An absent cluster's record is not read.
func TestInstalledPluginsAreReadFromTheRecord(t *testing.T) {
	full := map[string]string{"namespace": "ate-system", "daemonset": "daemonset.apps/atelet",
		"deployment": "ghcr.io/x/ate-api-server:v1.0.0", "configmap": "mode3,efs"}
	r := checkThenParse(t, fakeKubectl(full, nil))
	if !r.PluginsRead || strings.Join(r.Plugins, ",") != "efs,mode3" || r.Version != "v1.0.0" || !r.Atelet {
		t.Errorf("recorded plugins read as %+v", r)
	}
	partial := map[string]string{"namespace": "ate-system", "configmap": "efs"}
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
	if line, _ := Check(context.Background(), fakeKubectl(map[string]string{"configmap": "efs"}, nil)); strings.Contains(line, "plugins") {
		t.Errorf("absent cluster: %q", line)
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
	for name, want := range map[string]string{"x-installed": "installed v1.0.0", "x-partial": "partial install", "x": "ready"} {
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
