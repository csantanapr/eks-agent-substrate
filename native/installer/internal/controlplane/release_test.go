package controlplane

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
// install's release: the fork's when a recorded plugin installed it, or when
// the fork still runs after its plugin was removed (the record no longer
// names it, and removing the control plane is the fix the removal printed);
// the --tag one otherwise.
func TestDeleteRunsOnTheInstallsRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		record    []string
		installed string
		fork      bool
	}{
		{name: "upstream", installed: "v1.0.0"},
		{name: "nothing readable", installed: ""},
		{name: "fork recorded", record: []string{"cp-test-fork"}, installed: "v1.0.0-fork", fork: true},
		{name: "fork running, its plugin removed", installed: "v1.0.0-fork", fork: true},
		{name: "another unknown release running", installed: "v0.9.0"},
	} {
		o, calls, _ := deleteEnv(t, nil, nil)
		var removedOn plugin.Release
		removePlugins = func(_ context.Context, _ io.Writer, _ string, _ []string, po plugin.Options) error {
			removedOn = po.Release
			return nil
		}
		readRecord = func(context.Context, io.Writer, plugin.Options) ([]string, error) { return tc.record, nil }
		readInstalled = func(context.Context, string) (probe.Result, error) {
			return probe.Result{Installed: tc.installed != "", Atelet: tc.installed != "", Version: tc.installed}, nil
		}
		want := plugin.Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate", Repo: DefaultRepo, Ref: "v1.0.0"}
		if tc.fork {
			want = plugin.Release{Tag: "v1.0.0-fork", ImageRepo: "ghcr.io/example/fork", Repo: "https://example.invalid/fork.git", Ref: forkCommit}
			tree := TreeDir(o.TreeRoot, want.Tag)
			if err := os.MkdirAll(tree, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tree, completeMarker), []byte(forkCommit+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := Delete(context.Background(), io.Discard, o); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if removedOn != want {
			t.Errorf("%s: plugins removed on %+v, want %+v", tc.name, removedOn, want)
		}
		if len(*calls) == 0 || (*calls)[0].dir != TreeDir(o.TreeRoot, want.Tag) {
			t.Errorf("%s: ate-setup delete ran in %+v, want %s", tc.name, *calls, TreeDir(o.TreeRoot, want.Tag))
		}
	}
}
