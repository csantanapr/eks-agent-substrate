package demo

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

// useFake makes the exports run on f's env, the one the demo's tests use.
func useFake(t *testing.T, f *fake) {
	t.Helper()
	prev := exportEnv
	t.Cleanup(func() { exportEnv = prev })
	exportEnv = func(Options) *env { return f.env() }
}

var testPool = Pool{Atespace: "ate-demo-counter-mode3", Template: "counter-mode3", Actor: "demo-mode3"}

// CounterImage is Deploy's counter build and push for one node pool: built
// in the tree for that pool's architecture, pushed as <cluster>/counter:<tag>.
// Nodes of two architectures are refused before anything is pushed.
func TestCounterImageBuildsAndPushesLikeDeploy(t *testing.T) {
	_, tree := testTree(t)
	f := newFake(t)
	f.archs = "arm64 arm64"
	useFake(t, f)
	o := testOptions(t.TempDir())
	o.Tag = "v1.0.0-mode3.abc1234"
	var out bytes.Buffer
	// The tag is the caller's, not the release's: the demo pushes that one.
	ref, err := CounterImage(context.Background(), &out, o, tree, "microvm", "mode3-v1.0.0-mode3.abc1234")
	if err != nil || ref != testCounter {
		t.Fatalf("CounterImage = %q, %v", ref, err)
	}
	if want := []string{"counter|" + testBase + "|arm64|c1/counter|mode3-v1.0.0-mode3.abc1234"}; !slices.Equal(f.pushed, want) {
		t.Errorf("pushed %q, want %q", f.pushed, want)
	}
	var sel, build bool
	for _, c := range f.calls {
		sel = sel || (c.argv[0] == "kubectl" && slices.Contains(c.argv, PoolLabel+" in (microvm)"))
		build = build || (slices.Contains(c.argv, "./demos/counter") && c.dir == tree && slices.Contains(c.env, "GOARCH=arm64"))
	}
	if !sel || !build {
		t.Errorf("node read by ate.dev/pool=microvm %v, build in the tree for arm64 %v: %+v", sel, build, f.calls)
	}
	if !strings.Contains(out.String(), "Pushed "+testCounter+"\n") {
		t.Errorf("output:\n%s", out.String())
	}

	f = newFake(t)
	f.archs = "amd64 arm64"
	useFake(t, f)
	if _, err := CounterImage(context.Background(), io.Discard, o, tree, "microvm", "t"); err == nil || len(f.pushed) != 0 {
		t.Errorf("mixed architectures: err = %v, pushed %q", err, f.pushed)
	}
}

// Pin, KubectlAte and Ate run what Deploy and Verify run.
func TestPinKubectlAteAndAte(t *testing.T) {
	_, tree := testTree(t)
	f := newFake(t)
	useFake(t, f)
	o := testOptions(t.TempDir())
	if ref, err := Pin(context.Background(), o, "ghcr.io/x/ateworker-gvisor:t"); err != nil || ref != "ghcr.io/x/ateworker-gvisor:t@sha256:pinned" {
		t.Errorf("Pin = %q, %v", ref, err)
	}
	bin, err := KubectlAte(context.Background(), io.Discard, o, tree)
	if err != nil || bin != testAte {
		t.Fatalf("KubectlAte = %q, %v", bin, err)
	}
	if c := f.calls[0]; !slices.Equal(c.argv, []string{"go", "build", "-o", testAte, "./cmd/kubectl-ate"}) || c.dir != tree {
		t.Errorf("build = %+v", c)
	}
	// The fake fails any kubectl-ate argv without the binary, kubeconfig and context.
	if _, err := Ate(context.Background(), o, "create", "atespace", "ns1"); err != nil || !f.atespaces["ns1"] {
		t.Errorf("Ate create atespace: %v", err)
	}
	if _, err := Ate(context.Background(), o, "create", "atespace", "ns1"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second create: %v", err)
	}
}

// WaitGolden and TemplateImage read a template as Deploy does: a template
// with a golden tag passes and names its image; a missing one times out.
func TestWaitGoldenAndTemplateImage(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	o := testOptions(t.TempDir())
	f.templates[testPool.Atespace+"/"+testPool.Template] = testCounter
	if err := WaitGolden(context.Background(), io.Discard, o, testPool); err != nil {
		t.Errorf("WaitGolden: %v", err)
	}
	out, err := Ate(context.Background(), o, "get", "actor-template", testPool.Template, "-a", testPool.Atespace, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if img, err := TemplateImage(out); err != nil || img != testCounter {
		t.Errorf("TemplateImage = %q, %v", img, err)
	}
	other := Pool{Atespace: "ns", Template: "missing", Actor: "a"}
	if err := WaitGolden(context.Background(), io.Discard, o, other); err == nil || !strings.Contains(err.Error(), "golden snapshot of ns/missing not ready") {
		t.Errorf("missing template: %v", err)
	}
}

// RoundTrip is Verify's roundtrip on a plugin's pool: create, proxy,
// suspend, resume, then the new snapshot in this deployment's bucket. A
// counter the resume loses fails it.
func TestRoundTripChecksAPluginPool(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	o := testOptions(t.TempDir())
	sum, err := RoundTrip(context.Background(), io.Discard, o, testPool)
	if err != nil || !strings.Contains(sum, "memory, file and rootfs counters continued") {
		t.Fatalf("RoundTrip = %q, %v", sum, err)
	}
	var seq []string
	for _, v := range f.verbs() {
		if strings.HasSuffix(v, " demo-mode3") && !strings.HasPrefix(v, "get ") {
			seq = append(seq, strings.Fields(v)[0])
		}
	}
	if !slices.Equal(seq, []string{"create", "proxy", "suspend", "resume"}) {
		t.Errorf("kubectl-ate verbs = %v", seq)
	}
	want := []string{SnapBucket("c1", testAccount, "us-west-2") + "|atespaces/ate-demo-counter-mode3/actors/uid-demo-mode3/snapshots/snap-1/"}
	if !slices.Equal(f.listed, want) {
		t.Errorf("S3 lists = %q, want %q", f.listed, want)
	}

	f = newFake(t)
	f.lose["demo-mode3"] = "memory"
	useFake(t, f)
	if _, err := RoundTrip(context.Background(), io.Discard, o, testPool); err == nil || !strings.Contains(err.Error(), "the memory counter") {
		t.Errorf("lost memory counter: %v", err)
	}
}
