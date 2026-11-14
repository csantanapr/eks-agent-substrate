package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

const forkCommit = "9f36a0d7f2abca950f25fc7bd023752d3852504c"

// Both ways: a release tag is a shallow clone --branch; a full commit id
// (a fork's untagged pin) is init, a shallow fetch of the commit and a
// detached checkout, never clone --branch, which cannot fetch a commit.
func TestFetchTreeByTagOrCommit(t *testing.T) {
	prev := runCommand
	t.Cleanup(func() { runCommand = prev })
	for _, tc := range []struct {
		ref  string
		want []string
	}{
		{"v1.0.0", []string{"git clone --quiet --depth 1 --branch v1.0.0 R STAGE"}},
		{forkCommit, []string{
			"git init --quiet --initial-branch=substrate STAGE",
			"git -C STAGE fetch --quiet --depth 1 R " + forkCommit,
			"git -C STAGE checkout --quiet --detach FETCH_HEAD",
		}},
		// Not 40 hex characters: a branch or tag name.
		{"9f36a0d", []string{"git clone --quiet --depth 1 --branch 9f36a0d R STAGE"}},
	} {
		var got []string
		root := t.TempDir()
		dir := TreeDir(root, "x")
		runCommand = func(_ context.Context, _ io.Writer, c command) error {
			l := strings.Join(c.Argv, " ")
			for _, a := range c.Argv {
				if strings.HasPrefix(a, dir+".stage-") {
					l = strings.ReplaceAll(l, a, "STAGE")
				}
			}
			got = append(got, l)
			return nil
		}
		if err := FetchTree(context.Background(), io.Discard, dir, "R", tc.ref); err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.ref, got, tc.want)
		}
	}
}

// Test plugins, registered once for this package's tests: one that needs a
// gate, a provider and an ingress client, and one that replaces the release.
func init() {
	plugin.Register(plugin.Plugin{ID: "cp-test-gate", Order: 1, Needs: plugin.Needs{
		Preview:             []string{"ExternalVolumes"},
		CredentialProviders: []string{`{"name":"aws-secrets"}`},
		IngressIDs:          []string{"spiffe://cluster.local/ns/x/sa/y"},
	}})
	plugin.Register(plugin.Plugin{ID: "cp-test-fork", Order: 2, Needs: plugin.Needs{
		Source: &plugin.Source{Repo: "https://example.invalid/fork.git", Commit: forkCommit, Tag: "v1.0.0-fork", ImageRepo: "ghcr.io/example/fork"},
	}})
}

// Both directions: with no plugin the flags are install.sh's defaults (one
// provider object, no --preview); with needs they are added.
func TestAteSetupArgsCarryPluginNeeds(t *testing.T) {
	o := opts()
	args := AteSetupArgs(o, "demo")
	if got := flag(args, "--credential-provider"); got != `{"name":"k8s.io"}` {
		t.Errorf("no plugins: provider = %s", got)
	}
	if got := flag(args, "--ingress-allowed-spiffe-ids"); got != AtenetClientID {
		t.Errorf("no plugins: ingress = %s", got)
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--preview") }) {
		t.Errorf("no plugins: has a --preview flag: %v", args)
	}

	ps, err := plugin.Resolve([]string{"cp-test-gate"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Needs, err = plugin.Merge(ps); err != nil {
		t.Fatal(err)
	}
	args = AteSetupArgs(o, "demo")
	if got := flag(args, "--credential-provider"); got != `[{"name":"k8s.io"},{"name":"aws-secrets"}]` {
		t.Errorf("provider = %s", got)
	}
	if got := flag(args, "--ingress-allowed-spiffe-ids"); got != AtenetClientID+",spiffe://cluster.local/ns/x/sa/y" {
		t.Errorf("ingress = %s", got)
	}
	if !slices.Contains(args, "--preview=ExternalVolumes") {
		t.Errorf("no --preview=ExternalVolumes: %v", args)
	}
}

type deployFakes struct {
	calls    []string
	record   []string
	recorded []string
	runErr   error
}

// deployEnv stubs every seam Deploy reaches, for the bundled database.
func deployEnv(t *testing.T, f *deployFakes) Options {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	prev := []any{describeCluster, writeKubeconfig, readRecord, fetchTree, checkCommit, applyOverlay, recordPlugins, runCommand}
	t.Cleanup(func() {
		describeCluster = prev[0].(func(context.Context, string, string, string) (awsx.Cluster, error))
		writeKubeconfig = prev[1].(func(string, awsx.Cluster, string, string) error)
		readRecord = prev[2].(func(context.Context, io.Writer, plugin.Options) ([]string, error))
		fetchTree = prev[3].(func(context.Context, io.Writer, string, string, string) error)
		checkCommit = prev[4].(func(context.Context, string, string) error)
		applyOverlay = prev[5].(func(io.Writer, string, string, string) error)
		recordPlugins = prev[6].(func(context.Context, io.Writer, plugin.Options, ...string) error)
		runCommand = prev[7].(func(context.Context, io.Writer, command) error)
	})
	describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
		return awsx.Cluster{Name: "demo", Status: "ACTIVE", Endpoint: "https://example.invalid",
			CA: base64.StdEncoding.EncodeToString([]byte("ca"))}, nil
	}
	writeKubeconfig = func(string, awsx.Cluster, string, string) error { f.calls = append(f.calls, "kubeconfig"); return nil }
	readRecord = func(_ context.Context, _ io.Writer, po plugin.Options) ([]string, error) {
		if po.Kubeconfig == "" {
			return nil, errors.New("record read without a kubeconfig")
		}
		f.calls = append(f.calls, "read-record")
		return f.record, nil
	}
	fetchTree = func(_ context.Context, _ io.Writer, _, repo, tag string) error {
		f.calls = append(f.calls, "fetch "+repo+" "+tag)
		return nil
	}
	checkCommit = func(_ context.Context, _, commit string) error {
		f.calls = append(f.calls, "commit "+commit)
		return nil
	}
	applyOverlay = func(io.Writer, string, string, string) error { f.calls = append(f.calls, "overlay"); return nil }
	recordPlugins = func(_ context.Context, _ io.Writer, _ plugin.Options, ids ...string) error {
		f.recorded = append(f.recorded, ids...)
		return nil
	}
	runCommand = func(_ context.Context, _ io.Writer, c command) error {
		if slices.Contains(c.Argv, "./cmd/ate-setup") {
			f.calls = append(f.calls, "ate-setup "+strings.Join(c.Argv, " "))
			return f.runErr
		}
		return nil
	}
	o := opts()
	o.Database, o.Overlay, o.TreeRoot = DatabaseBundled, "/overlay", t.TempDir()
	return o
}

// The record is read through the installer's kubeconfig before the tree is
// fetched, a recorded plugin's needs reach ate-setup with --plugins', a
// Source replaces the release (and its commit is checked), and the plugins
// are recorded only once the install is ready.
func TestDeployMergesRecordedAndChosenPlugins(t *testing.T) {
	f := &deployFakes{record: []string{"cp-test-gate"}}
	o := deployEnv(t, f)
	o.Plugins = []string{"cp-test-fork"}
	var out bytes.Buffer
	if err := Deploy(context.Background(), &out, o); err != nil {
		t.Fatal(err)
	}
	// The fork is fetched by its pinned commit (it has no tags), checked,
	// and installed with the Source's tag as the image tag.
	want := []string{"kubeconfig", "read-record", "fetch https://example.invalid/fork.git " + forkCommit, "commit " + forkCommit, "overlay"}
	if len(f.calls) != len(want)+1 || !slices.Equal(f.calls[:len(want)], want) {
		t.Fatalf("calls = %q, want %q then ate-setup", f.calls, want)
	}
	ate := f.calls[len(want)]
	for _, w := range []string{"--preview=ExternalVolumes", "--image-repo ghcr.io/example/fork", "--image-tag v1.0.0-fork", `{"name":"aws-secrets"}`} {
		if !strings.Contains(ate, w) {
			t.Errorf("ate-setup lacks %s: %s", w, ate)
		}
	}
	if !slices.Equal(f.recorded, []string{"cp-test-gate", "cp-test-fork"}) {
		t.Errorf("recorded %v", f.recorded)
	}
	if !strings.Contains(out.String(), PluginsLine+"cp-test-gate, cp-test-fork") {
		t.Errorf("no plugins line:\n%s", out.String())
	}
}

// The other direction: no plugin keeps the release and records nothing; a
// failed install records nothing either.
func TestDeployWithoutPluginsOrFailingRecordsNothing(t *testing.T) {
	f := &deployFakes{}
	o := deployEnv(t, f)
	if err := Deploy(context.Background(), io.Discard, o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "fetch "+o.Repo+" v1.0.0") || slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "commit") }) {
		t.Errorf("calls = %q", f.calls)
	}
	if len(f.recorded) != 0 {
		t.Errorf("recorded %v with no plugin", f.recorded)
	}

	f = &deployFakes{runErr: errors.New("exit status 1")}
	o = deployEnv(t, f)
	o.Plugins = []string{"cp-test-gate"}
	if err := Deploy(context.Background(), io.Discard, o); err == nil {
		t.Fatal("want the ate-setup error")
	}
	if len(f.recorded) != 0 {
		t.Errorf("recorded %v after a failed install", f.recorded)
	}
}

// An unknown plugin stops the deploy before the tree is fetched.
func TestDeployRefusesAnUnknownPlugin(t *testing.T) {
	f := &deployFakes{}
	o := deployEnv(t, f)
	o.Plugins = []string{"no-such-plugin"}
	if err := Deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "no-such-plugin") {
		t.Fatalf("want an unknown-plugin error, got %v", err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "fetch") }) {
		t.Errorf("fetched after an unknown plugin: %q", f.calls)
	}
}
