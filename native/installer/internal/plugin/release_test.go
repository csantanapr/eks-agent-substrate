package plugin

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

var testFork = &Source{Repo: "https://example.invalid/fork.git", Commit: "9f36a0d7f2abca950f25fc7bd023752d3852504c",
	Tag: "v1.0.0-fork", ImageRepo: "ghcr.io/example/fork"}

var upstream = Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate", Repo: UpstreamRepo, Ref: "v1.0.0"}

var forkRelease = Release{Tag: "v1.0.0-fork", ImageRepo: "ghcr.io/example/fork", Repo: "https://example.invalid/fork.git",
	Ref: "9f36a0d7f2abca950f25fc7bd023752d3852504c"}

// releaseFixture is runFixture plus a plugin whose Source replaces the
// release, with needs and hooks, and o on the --tag release.
func releaseFixture(t *testing.T, f *fakeCluster) Options {
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "fork", Order: 4, Needs: Needs{Preview: []string{"NetworkWorkers"}, Source: testFork},
		Deploy: hook(f, "deploy fork", nil), Verify: hook(f, "verify fork", nil), Remove: hook(f, "remove fork", nil)})
	o.Release = Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate"}
	return o
}

// Both ways: the release is the Source of a chosen plugin, or of a recorded
// one, with its pinned commit as the tree's ref; with neither, the --tag
// release on upstream's tree. Two Sources are refused.
func TestReadReleaseWithAndWithoutASource(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		chosen       []string
		want         Release
	}{
		{"none", "", nil, upstream},
		{"recorded, no Source", "base,top", []string{"solo"}, upstream},
		{"chosen", "base", []string{"fork"}, forkRelease},
		{"recorded", "base,fork", nil, forkRelease},
		{"recorded and chosen", "fork", []string{"top"}, forkRelease},
	} {
		f := &fakeCluster{record: tc.record}
		o := releaseFixture(t, f)
		got, err := ReadRelease(context.Background(), io.Discard, o, tc.chosen)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	f := &fakeCluster{record: "fork"}
	o := releaseFixture(t, f)
	Register(Plugin{ID: "fork2", Order: 5, Needs: Needs{Source: &Source{Tag: "v9"}}})
	if _, err := ReadRelease(context.Background(), io.Discard, o, []string{"fork2"}); err == nil || !strings.Contains(err.Error(), "two plugins replace") {
		t.Errorf("two Sources: %v", err)
	}
}

// The wizard's lookup: chosen IDs that resolve give their release, and ones
// that do not keep the --tag one.
func TestChosenRelease(t *testing.T) {
	releaseFixture(t, &fakeCluster{})
	if got := ChosenRelease([]string{"base", "fork"}, "v1.0.0", "ghcr.io/agent-substrate/substrate"); got != forkRelease {
		t.Errorf("with fork: %+v", got)
	}
	for _, ids := range [][]string{nil, {"base"}, {"no-such-plugin", "fork"}} {
		if got := ChosenRelease(ids, "v1.0.0", "ghcr.io/agent-substrate/substrate"); got != upstream {
			t.Errorf("%v: %+v", ids, got)
		}
	}
	if SourceOf("v1.0.0-fork") != "fork" || SourceOf("v1.0.0") != "" {
		t.Errorf("SourceOf: %q %q", SourceOf("v1.0.0-fork"), SourceOf("v1.0.0"))
	}
}

// Both ways: deploying a plugin that replaces the release on a control plane
// that does not carry it names the reinstall, not a control plane deploy,
// which would be refused; a plugin with other needs keeps its advice.
func TestRunDeployOfAnUncarriedSourceNamesTheReinstall(t *testing.T) {
	f := &fakeCluster{record: "base"}
	o := releaseFixture(t, f)
	err := Run(context.Background(), io.Discard, ActionDeploy, []string{"fork"}, o)
	if err == nil || !strings.Contains(err.Error(), "run --remove-control-plane, then install again with or without --plugins fork") ||
		strings.Contains(err.Error(), "--deploy-control-plane") {
		t.Fatalf("fork: %v", err)
	}
	err = Run(context.Background(), io.Discard, ActionDeploy, []string{"top"}, o)
	if err == nil || !strings.Contains(err.Error(), "run --deploy-control-plane --plugins top first") {
		t.Fatalf("top: %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("ran after a refusal: %q", f.log)
	}
}

// Both ways: removing the plugin that replaced the release says the release
// stays and gives the reinstall as the way out, never a control plane
// deploy; removing one with other needs still says a deploy drops them.
func TestRunRemoveOfASourceGivesTheReinstall(t *testing.T) {
	f := &fakeCluster{record: "base,fork,top"}
	o := releaseFixture(t, f)
	var out bytes.Buffer
	if err := Run(context.Background(), &out, ActionRemove, []string{"fork"}, o); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, NeedsKeptLine+"fork, its Substrate release v1.0.0-fork among them") ||
		!strings.Contains(s, "run --remove-control-plane, then install again with or without --plugins fork") || strings.Contains(s, "--deploy-control-plane") {
		t.Errorf("fork removal:\n%s", s)
	}
	out.Reset()
	if err := Run(context.Background(), &out, ActionRemove, []string{"top"}, o); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, NeedsKeptLine+"top;") || !strings.Contains(s, "run --deploy-control-plane again") {
		t.Errorf("top removal:\n%s", s)
	}
}
