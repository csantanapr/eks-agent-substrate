package eksoverlay

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/kio"
)

// Render parity (issue #5, AC4.2): on a pristine tree, deploy/eks must
// produce what the reference patch, patches/substrate-<tag>-eks.patch,
// produces from the same release.
//
//	patch path:  git apply patches/substrate-<tag>-eks.patch, then write the
//	             Region over REPLACE_WITH_AWS_REGION
//	native path: eksoverlay.Apply
//
// Both trees render manifests/ate-install/base with kustomize, and the
// renders must hold the same objects with the same content. Their Go changes
// (git diff -- cmd/) must be identical too. It clones upstream, so it runs
// only with SUBSTRATE_PARITY=1 (make parity, and CI).

const (
	parityTag    = "v1.0.0"
	parityRegion = "us-west-2"
	// refPatch is the reference patch, from this package's directory.
	refPatch     = "../../../../patches/substrate-" + parityTag + "-eks.patch"
	regionMarker = "value: REPLACE_WITH_AWS_REGION"
)

// parityRepo is upstream, or PARITY_REPO (a local mirror) to save the fetch.
func parityRepo() string {
	if r := os.Getenv("PARITY_REPO"); r != "" {
		return r
	}
	return "https://github.com/agent-substrate/substrate.git"
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// pristine clones only cmd/ and manifests/ of the release, which is all
// either path touches or renders.
func pristine(t *testing.T, dir string) string {
	t.Helper()
	git(t, "", "clone", "--quiet", "--depth", "1", "--filter=blob:none", "--sparse", "--branch", parityTag, parityRepo(), dir)
	git(t, dir, "sparse-checkout", "set", "cmd", "manifests")
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("fresh clone is not clean:\n%s", st)
	}
	return dir
}

// patchPath applies the reference patch and writes the Region.
func patchPath(t *testing.T, dir string) {
	t.Helper()
	patch, err := filepath.Abs(refPatch)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "apply", patch)
	k := filepath.Join(dir, BaseKustomization)
	b, err := os.ReadFile(k)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(regionMarker)) {
		t.Fatalf("no %q in the patched %s", regionMarker, BaseKustomization)
	}
	b = bytes.ReplaceAll(b, []byte(regionMarker), []byte("value: "+parityRegion))
	if err := os.WriteFile(k, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// kustomizeArgv is a kustomize CLI, or kubectl's built-in one. The base
// lists files from its parent directory, which ate-setup's own kustomize
// build allows, so the load restrictor is off here too.
func kustomizeArgv(t *testing.T) []string {
	t.Helper()
	if p, err := exec.LookPath("kustomize"); err == nil {
		return []string{p, "build", "--load-restrictor", "LoadRestrictionsNone"}
	}
	if p, err := exec.LookPath("kubectl"); err == nil {
		return []string{p, "kustomize", "--load-restrictor", "LoadRestrictionsNone"}
	}
	t.Fatal("render parity needs kustomize or kubectl on PATH")
	return nil
}

// render builds the base kustomization and indexes its objects by
// apiVersion/kind/namespace/name, each as key-sorted JSON.
func render(t *testing.T, dir string) map[string]string {
	t.Helper()
	argv := append(kustomizeArgv(t), filepath.Join(dir, filepath.Dir(BaseKustomization)))
	c := exec.Command(argv[0], argv[1:]...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, stderr.String())
	}
	nodes, err := kio.FromBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	objs := map[string]string{}
	for _, n := range nodes {
		key := n.GetApiVersion() + "/" + n.GetKind() + "/" + n.GetNamespace() + "/" + n.GetName()
		j, err := n.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := objs[key]; dup {
			t.Fatalf("render of %s has %s twice", dir, key)
		}
		objs[key] = string(j)
	}
	if len(objs) == 0 {
		t.Fatalf("render of %s is empty", dir)
	}
	return objs
}

// goChanges is the tree's change to cmd/: the diff and any new files.
func goChanges(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "diff", "--no-color", "--no-ext-diff", "--", "cmd") +
		git(t, dir, "ls-files", "--others", "--exclude-standard", "--", "cmd")
}

// differences lists how a tree's render and Go changes differ from the
// patch path's.
func differences(t *testing.T, patched, tree string) []string {
	t.Helper()
	w, g := render(t, patched), render(t, tree)
	var out []string
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range w {
			if !yield(k) {
				return
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok && !yield(k) {
				return
			}
		}
	})
	for _, k := range keys {
		a, inW := w[k]
		b, inG := g[k]
		switch {
		case !inG:
			out = append(out, "only in the patched render: "+k)
		case !inW:
			out = append(out, "only in the tree's render: "+k)
		case a != b:
			out = append(out, fmt.Sprintf("%s differs:\n  patched: %s\n  tree:    %s", k, clip(a), clip(b)))
		}
	}
	if gw, gg := goChanges(t, patched), goChanges(t, tree); gw != gg {
		out = append(out, fmt.Sprintf("the Go changes under cmd/ differ:\n--- patched\n%s\n--- tree\n%s", clip(gw), clip(gg)))
	}
	return out
}

func clip(s string) string {
	if len(s) > 3000 {
		return s[:3000] + "…"
	}
	return s
}

func TestRenderParity(t *testing.T) {
	if os.Getenv("SUBSTRATE_PARITY") != "1" {
		t.Skip("clones upstream; run with SUBSTRATE_PARITY=1 (make parity)")
	}
	overlay, err := filepath.Abs("../../../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	patched := pristine(t, filepath.Join(tmp, "patched"))
	patchPath(t, patched)
	native := pristine(t, filepath.Join(tmp, "native"))
	if err := Apply(io.Discard, native, overlay, parityRegion); err != nil {
		t.Fatal(err)
	}
	if d := differences(t, patched, native); len(d) > 0 {
		t.Fatalf("deploy/eks does not render what %s does:\n%s", filepath.Base(refPatch), strings.Join(d, "\n"))
	}

	// Both ways: the comparison must see a tree without the EKS changes,
	// and an overlay that gets one value wrong.
	other := pristine(t, filepath.Join(tmp, "other"))
	d := differences(t, patched, other)
	for _, s := range d {
		t.Log("unpatched tree: " + strings.SplitN(s, "\n", 2)[0])
	}
	if !slices.ContainsFunc(d, func(s string) bool { return strings.HasPrefix(s, "the Go changes") }) ||
		!slices.ContainsFunc(d, func(s string) bool { return strings.HasPrefix(s, "apps/v1/DaemonSet/ate-system/atelet differs") }) {
		t.Fatalf("an unpatched tree was not told apart:\n%s", strings.Join(d, "\n"))
	}
	broken := t.TempDir()
	if err := os.CopyFS(broken, os.DirFS(overlay)); err != nil {
		t.Fatal(err)
	}
	comp := filepath.Join(broken, componentSrc, "kustomization.yaml")
	b, err := os.ReadFile(comp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("value: s3")) {
		t.Fatal("deploy/eks has no 'value: s3' to break")
	}
	if err := os.WriteFile(comp, bytes.Replace(b, []byte("value: s3"), []byte("value: gcs"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Apply(io.Discard, other, broken, parityRegion); err != nil {
		t.Fatal(err)
	}
	d = differences(t, patched, other)
	for _, s := range d {
		t.Log("broken overlay: " + strings.SplitN(s, "\n", 2)[0])
	}
	if len(d) != 1 || !strings.HasPrefix(d[0], "apps/v1/Deployment/ate-system/ate-api-server differs") {
		t.Fatalf("an overlay with ATE_STORAGE_BACKEND=gcs on ate-api-server was not told apart, or not alone:\n%s", strings.Join(d, "\n"))
	}
}
