package controlplane

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
)

// Both ways, for each kind of release change: a deploy over an installed
// control plane of another release (v1.0.0 to the fork, the fork back to
// v1.0.0, one tag to another) is refused after the record and probe reads
// and before anything is fetched, applied or recorded, naming the fix; a
// deploy at the release installed (adding a plugin with needs) and one on a
// cluster with no readable release go on.
func TestDeployRefusesAReleaseChange(t *testing.T) {
	installed := func(v string) probe.Result { return probe.Result{Installed: true, Atelet: true, Version: v} }
	for _, tc := range []struct {
		name      string
		installed probe.Result
		tag       string
		record    []string
		chosen    []string
		refused   string // what the refusal names; "" goes on
	}{
		{name: "v1.0.0 to the fork", installed: installed("v1.0.0"), tag: "v1.0.0", chosen: []string{"cp-test-fork"},
			refused: "runs Substrate v1.0.0, not v1.0.0-fork"},
		{name: "the fork back to v1.0.0", installed: installed("v1.0.0-fork"), tag: "v1.0.0",
			refused: "runs Substrate v1.0.0-fork, not v1.0.0"},
		{name: "one tag to another", installed: installed("v1.0.0"), tag: "v1.0.1",
			refused: "runs Substrate v1.0.0, not v1.0.1"},
		{name: "same release, a plugin with needs added", installed: installed("v1.0.0"), tag: "v1.0.0", chosen: []string{"cp-test-gate"}},
		{name: "same fork release, recorded", installed: installed("v1.0.0-fork"), tag: "v1.0.0", record: []string{"cp-test-fork"}, chosen: []string{"cp-test-gate"}},
		{name: "nothing installed", tag: "v1.0.0", chosen: []string{"cp-test-fork"}},
		{name: "release not readable", installed: probe.Result{Installed: true}, tag: "v1.0.0", chosen: []string{"cp-test-fork"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &deployFakes{installed: tc.installed, record: tc.record}
			o := deployEnv(t, f)
			o.Tag, o.Plugins = tc.tag, tc.chosen
			err := Deploy(context.Background(), io.Discard, o)
			changed := slices.ContainsFunc(f.calls, func(c string) bool {
				return strings.HasPrefix(c, "fetch") || strings.HasPrefix(c, "overlay") || strings.HasPrefix(c, "ate-setup")
			})
			if tc.refused == "" {
				if err != nil || !changed {
					t.Fatalf("want the deploy, got %v (calls %q)", err, f.calls)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refused) || !strings.Contains(err.Error(), "mixed-version") ||
				!strings.Contains(err.Error(), "run --remove-control-plane, then install again") {
				t.Fatalf("want a refusal naming %q and the fix, got %v", tc.refused, err)
			}
			if changed || len(f.recorded) > 0 || !slices.Equal(f.calls, []string{"kubeconfig", "read-record", "probe"}) {
				t.Errorf("the refusal changed something: calls %q, recorded %q", f.calls, f.recorded)
			}
		})
	}
}

// The fix names the plugin whose Source is one of the two releases, and the
// tag otherwise.
func TestSameReleaseNamesTheFix(t *testing.T) {
	prev := readInstalled
	t.Cleanup(func() { readInstalled = prev })
	for have, want := range map[string]string{
		"v1.0.0-fork": "with or without --plugins cp-test-fork",
		"v1.0.1":      "with --tag v1.0.0",
	} {
		readInstalled = func(context.Context, string) (probe.Result, error) {
			return probe.Result{Installed: true, Atelet: true, Version: have}, nil
		}
		err := SameRelease(context.Background(), "/kc", "demo", plugin.Release{Tag: "v1.0.0"})
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("installed %s: %v, want it to end %q", have, err, want)
		}
	}
}

// Teardown removes the recorded plugins and runs ate-setup delete on the
// release the cluster runs, never on --tag when another runs (review N1):
// the fork's when a recorded plugin installed it, or when the fork still
// runs after its plugin was removed (the record no longer names it, and
// removing the control plane is the fix the removal printed); upstream's at
// the running tag when that is another than --tag (SUBSTRATE_TAG already
// names the release to move to); the --tag one only when it is the one that
// runs or none can be read.
func TestDeleteRunsOnTheInstallsRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tag       string // --tag; "" is opts()'s v1.0.0
		record    []string
		installed string
		fork      bool
		want      string // the upstream tag removed on, when not the fork
	}{
		{name: "upstream", installed: "v1.0.0", want: "v1.0.0"},
		{name: "nothing readable", installed: "", want: "v1.0.0"},
		{name: "nothing readable, another --tag", tag: "v1.0.1", installed: "", want: "v1.0.1"},
		{name: "fork recorded", record: []string{"cp-test-fork"}, installed: "v1.0.0-fork", fork: true},
		{name: "fork running, its plugin removed", installed: "v1.0.0-fork", fork: true},
		{name: "another unknown release running", installed: "v0.9.0", want: "v0.9.0"},
		{name: "--tag names the next release", tag: "v1.0.1", installed: "v1.0.0", want: "v1.0.0"},
	} {
		o, calls, _ := deleteEnv(t, nil, nil)
		if tc.tag != "" {
			o.Tag = tc.tag
		}
		var removedOn plugin.Release
		removePlugins = func(_ context.Context, _ io.Writer, _ string, _ []string, po plugin.Options) error {
			removedOn = po.Release
			return nil
		}
		readRecord = func(context.Context, io.Writer, plugin.Options) ([]string, error) { return tc.record, nil }
		readInstalled = func(context.Context, string) (probe.Result, error) {
			return probe.Result{Installed: tc.installed != "", Atelet: tc.installed != "", Version: tc.installed}, nil
		}
		want := plugin.Release{Tag: tc.want, ImageRepo: "ghcr.io/agent-substrate/substrate", Repo: DefaultRepo, Ref: tc.want}
		ref := tc.want
		if tc.fork {
			want = plugin.Release{Tag: "v1.0.0-fork", ImageRepo: "ghcr.io/example/fork", Repo: "https://example.invalid/fork.git", Ref: forkCommit}
			ref = forkCommit
		}
		// The release's tree is cached, as its install left it, so Delete
		// fetches nothing.
		cacheTree(t, treeOf(o, want), ref)
		want.Tree = treeOf(o, want)
		if err := Delete(context.Background(), io.Discard, o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if removedOn != want {
			t.Errorf("%s: plugins removed on %+v, want %+v", tc.name, removedOn, want)
		}
		if len(*calls) == 0 || (*calls)[0].dir != want.Tree {
			t.Errorf("%s: ate-setup delete ran in %+v, want %s", tc.name, *calls, want.Tree)
		}
	}
}

// writeOverlay makes a deploy/eks at dir with the given patch text.
func writeOverlay(t *testing.T, dir, patch string) string {
	t.Helper()
	for name, body := range map[string]string{
		"components/eks/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n",
		"patches/ate-setup.patch":           patch,
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Both ways, through Deploy with the real overlay digest: the tree is keyed
// by the overlay applied to it in place, so two checkouts whose deploy/eks
// differ (here only in the code patch) fetch into two trees, and a second
// checkout with the same deploy/eks reuses the first's. Another Region is
// another tree too, since the overlay is filled in for it.
func TestTreesAreKeyedByTheOverlay(t *testing.T) {
	a := writeOverlay(t, filepath.Join(t.TempDir(), "eks"), "patch one\n")
	sameAsA := writeOverlay(t, filepath.Join(t.TempDir(), "eks"), "patch one\n")
	b := writeOverlay(t, filepath.Join(t.TempDir(), "eks"), "patch two\n")
	f := &deployFakes{}
	o := deployEnv(t, f)
	overlayDigest = eksoverlay.Digest
	for _, tc := range []struct{ overlay, region string }{{a, "us-west-2"}, {sameAsA, "us-west-2"}, {b, "us-west-2"}, {a, "us-east-1"}} {
		o.Overlay, o.Region = tc.overlay, tc.region
		if err := Deploy(context.Background(), io.Discard, o); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.trees) != 4 {
		t.Fatalf("fetched %q", f.trees)
	}
	if f.trees[0] != f.trees[1] {
		t.Errorf("the same overlay got two trees: %s, %s", f.trees[0], f.trees[1])
	}
	if f.trees[2] == f.trees[0] || f.trees[3] == f.trees[0] || f.trees[2] == f.trees[3] {
		t.Errorf("another overlay or Region shares a tree: %q", f.trees)
	}
	for _, d := range f.trees {
		if filepath.Dir(d) != o.TreeRoot || !strings.HasPrefix(filepath.Base(d), "substrate-v1.0.0-") || len(filepath.Base(d)) != len("substrate-v1.0.0-")+8 {
			t.Errorf("tree %s is not <tree root>/substrate-v1.0.0-<8 hex>", d)
		}
	}
}
