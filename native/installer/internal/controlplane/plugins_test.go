package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
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
		dir := filepath.Join(root, "substrate-x")
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
	plugin.Register(plugin.Plugin{ID: "cp-test-auto", Order: 3, NeedsAutoMode: "needs EKS Auto Mode"})
	plugin.Register(plugin.Plugin{ID: "cp-test-nitro", Order: 4, NeedsAutoMode: "needs EKS Auto Mode",
		AutoModeOptional: func(o plugin.Options) bool { return o.NitroCompute == plugin.NitroNodeGroup }})
	plugin.Register(plugin.Plugin{ID: "cp-test-follows", Order: 5, FollowsNitroCompute: true})
	// The combination mode3-iso has: it needs Auto Mode unless
	// NITRO_COMPUTE=node-group, and it follows NITRO_COMPUTE.
	plugin.Register(plugin.Plugin{ID: "cp-test-iso", Order: 6, NeedsAutoMode: "needs EKS Auto Mode", FollowsNitroCompute: true,
		AutoModeOptional: func(o plugin.Options) bool { return o.NitroCompute == plugin.NitroNodeGroup }})
}

// A chosen plugin whose own deploy would refuse the run's NITRO_COMPUTE
// (another than the record holds for it, as on a wizard rerun with
// another one) stops the control plane deploy before the tree is fetched
// or ate-setup runs; with the recorded one, or with the plugin only
// recorded and not chosen, it goes on. Without such a plugin the record
// is read no more than before (review 1, finding 8).
func TestDeployRefusesAnotherNitroComputeThanTheRecorded(t *testing.T) {
	for _, c := range []struct {
		chosen  []string
		compute string
		refused bool
		reads   int
	}{
		{[]string{"cp-test-follows"}, "", true, 1},
		{[]string{"cp-test-follows"}, plugin.NitroAutoMode, true, 1},
		{[]string{"cp-test-follows"}, plugin.NitroNodeGroup, false, 1},
		{[]string{"cp-test-gate"}, plugin.NitroAutoMode, false, 0},
	} {
		f := &deployFakes{record: []string{"cp-test-follows"}}
		o := deployEnv(t, f)
		reads := 0
		prev := plugin.Exec
		t.Cleanup(func() { plugin.Exec = prev })
		plugin.Exec = func(_ context.Context, _ io.Writer, cmd plugin.Cmd) error {
			if !slices.Contains(cmd.Argv, plugin.RecordName) || !slices.Contains(cmd.Argv, "get") {
				return errors.New("unexpected child: " + strings.Join(cmd.Argv, " "))
			}
			reads++
			io.WriteString(cmd.Stdout, `{"data":{"plugins":"cp-test-follows","nitro-compute":"node-group"}}`)
			return nil
		}
		o.Plugins, o.Plugin.NitroCompute = c.chosen, c.compute
		err := Deploy(context.Background(), io.Discard, o)
		ran := slices.ContainsFunc(f.calls, func(s string) bool { return strings.HasPrefix(s, "fetch") || strings.HasPrefix(s, "ate-setup") })
		if c.refused != (err != nil) || c.refused == ran || reads != c.reads {
			t.Errorf("%+v: %v, fetched or ran ate-setup %v, %d reads", c, err, ran, reads)
		}
		if c.refused && !strings.Contains(err.Error(), "plugin cp-test-follows was deployed with NITRO_COMPUTE=node-group (recorded in kube-system/substrate-eks-plugins)") {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// On a cluster without Auto Mode, a plugin like mode3-iso recorded with
// node-group and asked for with the run's default (auto-mode) gets the
// recorded-mode refusal and its ways out, not the generic Auto Mode one: the
// record check comes first.
func TestDeployOffAutoModeNamesTheRecordedNitroCompute(t *testing.T) {
	f := &deployFakes{}
	o := deployEnv(t, f)
	describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
		return awsx.Cluster{Name: "demo", Status: "ACTIVE", Endpoint: "https://example.invalid",
			CA: base64.StdEncoding.EncodeToString([]byte("ca"))}, nil
	}
	prev := plugin.Exec
	t.Cleanup(func() { plugin.Exec = prev })
	plugin.Exec = func(_ context.Context, _ io.Writer, cmd plugin.Cmd) error {
		if !slices.Contains(cmd.Argv, plugin.RecordName) || !slices.Contains(cmd.Argv, "get") {
			return errors.New("unexpected child: " + strings.Join(cmd.Argv, " "))
		}
		io.WriteString(cmd.Stdout, `{"data":{"plugins":"cp-test-iso","nitro-compute":"node-group"}}`)
		return nil
	}
	o.Plugins, o.Plugin.NitroCompute = []string{"cp-test-iso"}, ""
	err := Deploy(context.Background(), io.Discard, o)
	if err == nil || !strings.Contains(err.Error(), "plugin cp-test-iso was deployed with NITRO_COMPUTE=node-group") || strings.Contains(err.Error(), "has EKS Auto Mode off") {
		t.Errorf("got %v", err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "fetch") || strings.HasPrefix(c, "ate-setup") }) {
		t.Errorf("went on: %q", f.calls)
	}
}

// A plugin that may run without Auto Mode (mode3-iso with
// NITRO_COMPUTE=node-group) is refused on a standard cluster only without
// that setting: the control plane deploy reads it from the plugin options.
func TestDeployRefusesAutoModeOnlyWithoutTheNodeGroupSetting(t *testing.T) {
	for _, compute := range []string{"", plugin.NitroAutoMode, plugin.NitroNodeGroup} {
		f := &deployFakes{}
		o := deployEnv(t, f)
		describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
			return awsx.Cluster{Name: "demo", Status: "ACTIVE", Endpoint: "https://example.invalid",
				CA: base64.StdEncoding.EncodeToString([]byte("ca"))}, nil
		}
		o.Plugins, o.Plugin.NitroCompute = []string{"cp-test-nitro"}, compute
		err := Deploy(context.Background(), io.Discard, o)
		fetched := slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "fetch") })
		runs := compute == plugin.NitroNodeGroup
		if runs != (err == nil) || runs != fetched {
			t.Errorf("NITRO_COMPUTE=%q: %v, fetched %v", compute, err, fetched)
		}
		if !runs && (err == nil || !strings.Contains(err.Error(), "cp-test-nitro needs EKS Auto Mode; demo has EKS Auto Mode off")) {
			t.Errorf("NITRO_COMPUTE=%q: %v", compute, err)
		}
	}
}

// A plugin that needs Auto Mode (the AWS Nitro sandbox class's) stops a
// deploy on a standard cluster before the tree is fetched, naming it; on an
// Auto Mode cluster the same deploy goes on.
func TestDeployRefusesAnAutoModePluginOffAutoMode(t *testing.T) {
	for _, auto := range []bool{false, true} {
		f := &deployFakes{}
		o := deployEnv(t, f)
		describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
			return awsx.Cluster{Name: "demo", Status: "ACTIVE", AutoMode: auto, Endpoint: "https://example.invalid",
				CA: base64.StdEncoding.EncodeToString([]byte("ca"))}, nil
		}
		o.Plugins = []string{"cp-test-auto"}
		err := Deploy(context.Background(), io.Discard, o)
		fetched := slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "fetch") })
		if auto != (err == nil) || auto != fetched {
			t.Errorf("auto=%v: %v, fetched %v", auto, err, fetched)
		}
		if !auto && (err == nil || !strings.Contains(err.Error(), "cp-test-auto needs EKS Auto Mode")) {
			t.Errorf("auto=false: %v", err)
		}
	}
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
	// unknown are data keys the record holds that this installer does not
	// know (plugin.Record.Unknown).
	unknown []string
	runErr  error
	// installed is what the probe reads off the cluster (none by default).
	installed probe.Result
	// trees are the directories fetched into, in order.
	trees []string
	// pools is the cluster's pool inventory (readPools), and poolsErr its
	// read's failure.
	pools    plugin.Inventory
	poolsErr error
}

// deployEnv stubs every seam Deploy reaches, for the bundled database.
func deployEnv(t *testing.T, f *deployFakes) Options {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	prev := []any{describeCluster, writeKubeconfig, loadRecord, fetchTree, checkCommit, applyOverlay, recordPlugins, runCommand, readInstalled, overlayDigest}
	t.Cleanup(func() {
		overlayDigest = prev[9].(func(string) (string, error))
		describeCluster = prev[0].(func(context.Context, string, string, string) (awsx.Cluster, error))
		writeKubeconfig = prev[1].(func(string, awsx.Cluster, string, string) error)
		loadRecord = prev[2].(func(context.Context, io.Writer, plugin.Options) (plugin.Record, error))
		fetchTree = prev[3].(func(context.Context, io.Writer, string, string, string) error)
		checkCommit = prev[4].(func(context.Context, string, string) error)
		applyOverlay = prev[5].(func(io.Writer, string, string, string) error)
		recordPlugins = prev[6].(func(context.Context, io.Writer, plugin.Options, ...string) error)
		runCommand = prev[7].(func(context.Context, io.Writer, command) error)
		readInstalled = prev[8].(func(context.Context, string) (probe.Result, error))
	})
	prevPools := readPools
	t.Cleanup(func() { readPools = prevPools })
	readPools = func(_ context.Context, _ io.Writer, po plugin.Options) (plugin.Inventory, error) {
		if po.Kubeconfig == "" {
			return plugin.Inventory{}, errors.New("pools read without a kubeconfig")
		}
		f.calls = append(f.calls, "pools")
		return f.pools, f.poolsErr
	}
	readInstalled = func(_ context.Context, kc string) (probe.Result, error) {
		if kc == "" {
			return probe.Result{}, errors.New("probe without a kubeconfig")
		}
		f.calls = append(f.calls, "probe")
		return f.installed, nil
	}
	overlayDigest = func(string) (string, error) { return fakeDigest, nil }
	describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
		return awsx.Cluster{Name: "demo", Status: "ACTIVE", Endpoint: "https://example.invalid",
			CA: base64.StdEncoding.EncodeToString([]byte("ca"))}, nil
	}
	writeKubeconfig = func(string, awsx.Cluster, string, string) error { f.calls = append(f.calls, "kubeconfig"); return nil }
	loadRecord = func(_ context.Context, _ io.Writer, po plugin.Options) (plugin.Record, error) {
		if po.Kubeconfig == "" {
			return plugin.Record{}, errors.New("record read without a kubeconfig")
		}
		f.calls = append(f.calls, "read-record")
		return plugin.Record{Plugins: f.record, Unknown: f.unknown}, nil
	}
	fetchTree = func(_ context.Context, _ io.Writer, dir, repo, tag string) error {
		f.calls = append(f.calls, "fetch "+repo+" "+tag)
		f.trees = append(f.trees, dir)
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
	want := []string{"kubeconfig", "read-record", "probe", "fetch https://example.invalid/fork.git " + forkCommit, "commit " + forkCommit, "overlay"}
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
	if !slices.Contains(f.calls, "fetch "+DefaultRepo+" v1.0.0") || slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "commit") }) {
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

// #22 (review R1 of #21), both ways: a record with a data key this
// installer does not know stops the control plane deploy right after the
// record is read, before the tree is fetched, the install runs or the
// plugins are recorded (a write that would drop the key), naming it;
// without it the deploy runs and records them.
func TestDeployRefusesARecordWithUnknownKeys(t *testing.T) {
	for _, unknown := range [][]string{{"future-key"}, nil} {
		f := &deployFakes{record: []string{"cp-test-gate"}, unknown: unknown}
		o := deployEnv(t, f)
		err := Deploy(context.Background(), io.Discard, o)
		if unknown == nil {
			if err != nil || !slices.Equal(f.recorded, []string{"cp-test-gate"}) {
				t.Errorf("without the key: %v, recorded %v", err, f.recorded)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "kube-system/substrate-eks-plugins holds data keys this installer does not know (future-key)") {
			t.Fatalf("want the refusal, got %v", err)
		}
		if want := []string{"kubeconfig", "read-record"}; !slices.Equal(f.calls, want) || len(f.recorded) != 0 {
			t.Errorf("calls %q (want %q), recorded %v after the refusal", f.calls, want, f.recorded)
		}
	}
}
