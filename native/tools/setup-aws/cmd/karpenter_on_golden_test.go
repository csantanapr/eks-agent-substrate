package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// karpenterOnRun is what one run of create cluster with KARPENTER=on, and no
// KARPENTER_NODE_OVERLAY, does: its AWS writes and reads (with the inputs the
// node group settings decide), what it counts for --plan, its Kubernetes
// requests, and what it leaves: the cluster's tags, the node groups, the
// controller role's policy, the queue, the rules, and every object in the
// cluster (a CRD by its digest).
type karpenterOnRun struct {
	Writes, Reads, Inputs []string
	Creates, Changes      []string
	KubeCalls             []string
	ClusterTags           map[string]string
	NodeGroups            []string
	Policy                string
	Queue                 map[string]string
	Rules                 map[string]string
	Objects               map[string]any
}

func snapshotKarpenterOn(t *testing.T, f *clusterFake, r *clusterRun) karpenterOnRun {
	t.Helper()
	s := karpenterOnRun{Writes: f.writes, Reads: f.reads, Inputs: f.inputs, Creates: r.creates, Changes: r.changes,
		NodeGroups: sortedKeysOf(f.ngIn), Rules: map[string]string{}, Objects: map[string]any{}}
	if f.clusterIn != nil {
		s.ClusterTags = f.clusterIn.Tags
	}
	if role := f.roles["demo-us-west-2-karpenter"]; role != nil {
		s.Policy = role.inline["access"]
	}
	if q := f.queues["demo-karpenter"]; q != nil {
		s.Queue = q.attrs
	}
	for n, rule := range f.rules {
		s.Rules[n] = rule.pattern
	}
	if f.objects != nil {
		s.KubeCalls = f.objects.calls
		var paths []string
		for p := range f.objects.objs {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			b, err := json.Marshal(f.objects.objs[p])
			if err != nil {
				t.Fatal(err)
			}
			// A CRD by its digest: the chart's CRDs are large, and are
			// applied as the chart has them.
			if strings.Contains(p, kindCRD.Resource) {
				sum := sha256.Sum256(b)
				s.Objects[p] = "sha256:" + hex.EncodeToString(sum[:])
				continue
			}
			var o any
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatal(err)
			}
			s.Objects[p] = o
		}
	}
	return s
}

// karpenterOnRuns are the runs the golden file holds: a fresh account, a
// second run on it, --plan on a fresh account and on the complete
// deployment, and a fresh account with self-managed node groups.
func karpenterOnRuns(t *testing.T) map[string]karpenterOnRun {
	t.Helper()
	out := map[string]karpenterOnRun{}
	run := func(f *clusterFake, plan bool, edit ...func(*clusterConfig)) karpenterOnRun {
		f.writes, f.reads, f.inputs = nil, nil, nil
		if f.objects != nil {
			f.objects.calls = nil
		}
		r := clusterTestRun(t, f, plan, append([]func(*clusterConfig){withKarpenter}, edit...)...)
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return snapshotKarpenterOn(t, f, r)
	}
	f := karpenterFake(t)
	out["fresh"] = run(f, false)
	out["rerun"] = run(f, false)
	out["plan, complete"] = run(f, true)
	out["plan, fresh"] = run(karpenterFake(t), true)
	out["fresh, self-managed"] = run(karpenterFake(t), false, selfManaged)
	return out
}

// KARPENTER=on without KARPENTER_NODE_OVERLAY makes what it made before
// KARPENTER_NODE_OVERLAY: testdata/karpenter-on-create.json was recorded on
// this branch before it, with the same fixtures.
func TestCreateClusterKarpenterOnIsUnchanged(t *testing.T) {
	got := karpenterOnRuns(t)
	raw, err := os.ReadFile("testdata/karpenter-on-create.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]karpenterOnRun
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d runs, want %d", len(got), len(want))
	}
	for name, w := range want {
		g := got[name]
		// Through JSON, as the file holds it.
		b, _ := json.Marshal(g)
		var gj karpenterOnRun
		if err := json.Unmarshal(b, &gj); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gj, w) {
			for _, d := range diffKarpenterOn(gj, w) {
				t.Errorf("%s: %s", name, d)
			}
		}
	}
}

// diffKarpenterOn names the fields of two runs that differ.
func diffKarpenterOn(got, want karpenterOnRun) []string {
	var out []string
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := 0; i < gv.NumField(); i++ {
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			gb, _ := json.Marshal(gv.Field(i).Interface())
			wb, _ := json.Marshal(wv.Field(i).Interface())
			out = append(out, gv.Type().Field(i).Name+" changed:\n got  "+string(gb)+"\n want "+string(wb))
		}
	}
	return out
}
