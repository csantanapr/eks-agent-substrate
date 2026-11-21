package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// mainRecord is the record ConfigMap as the installer wrote it before the
// pending and nitro-compute keys, byte for byte, for plugins.
func mainRecord(plugins string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: substrate-eks-plugins\n  namespace: kube-system\n" +
		"  labels:\n    app.kubernetes.io/managed-by: substrate-eks\ndata:\n  plugins: \"" + plugins + "\"\n"
}

// The record reads and writes, with the new keys and without: a missing
// ConfigMap is no record, an older installer's (plugins only) reads with
// nothing pending and no NITRO_COMPUTE, and keys this installer does not
// know are kept in Unknown, never written. Output that is not the
// ConfigMap is an error, never an empty record. With only plugins the
// manifest is the one written before, byte for byte; the other keys are
// written only when they hold something.
func TestRecordParseAndManifest(t *testing.T) {
	for _, empty := range []string{"", " \n"} {
		r, err := ParseRecordJSON([]byte(empty))
		if err != nil || !r.Empty() || r.NitroCompute != "" || !r.Equal(Record{}) {
			t.Errorf("%q: %+v, %v", empty, r, err)
		}
	}
	old := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"substrate-eks-plugins"},"data":{"plugins":"mode3-iso,efs"}}`
	r, err := ParseRecordJSON([]byte(old))
	if err != nil || !slices.Equal(r.Plugins, []string{"efs", "mode3-iso"}) || len(r.Pending) != 0 || r.NitroCompute != "" {
		t.Errorf("an older installer's record: %+v, %v", r, err)
	}
	full := `{"data":{"plugins":"efs","pending":"mode3-iso, efs","nitro-compute":" node-group ","future-key":"x"},"binaryData":{"b":"eA=="}}`
	r, err = ParseRecordJSON([]byte(full))
	want := Record{Plugins: []string{"efs"}, Pending: []string{"efs", "mode3-iso"}, NitroCompute: NitroNodeGroup}
	if err != nil || !r.Equal(want) || !slices.Equal(r.Unknown, []string{"future-key"}) {
		t.Errorf("a record with every key and an unknown one: %+v, %v", r, err)
	}
	if len(want.Unknown) != 0 || !r.Equal(want) || r.CheckKeys() == nil || want.CheckKeys() != nil {
		t.Errorf("Equal ignores Unknown, CheckKeys refuses only a record with some: %+v", r)
	}
	if got := string(r.Manifest()); strings.Contains(got, "future-key") {
		t.Errorf("Manifest writes an unknown key:\n%s", got)
	}
	if got := r.All(); !slices.Equal(got, []string{"efs", "mode3-iso"}) {
		t.Errorf("All = %v", got)
	}
	for _, bad := range []string{"mode3-iso", `{"data":`, `{"data":{"plugins":1}}`} {
		if r, err := ParseRecordJSON([]byte(bad)); err == nil || !strings.Contains(err.Error(), "read ConfigMap kube-system/substrate-eks-plugins") {
			t.Errorf("%q read as %+v, %v", bad, r, err)
		}
	}

	if got := string(Record{Plugins: []string{"llm-gateway", "efs"}}.Manifest()); got != mainRecord("efs,llm-gateway") {
		t.Errorf("plugins only:\n%s\nwant\n%s", got, mainRecord("efs,llm-gateway"))
	}
	if got, want := string(want.Manifest()), mainRecord("efs")+"  pending: \"efs,mode3-iso\"\n  nitro-compute: \"node-group\"\n"; got != want {
		t.Errorf("every key:\n%s\nwant\n%s", got, want)
	}
	if got, want := string(Record{Pending: []string{"mode3-iso"}, NitroCompute: NitroAutoMode}.Manifest()),
		mainRecord("")+"  pending: \"mode3-iso\"\n  nitro-compute: \"auto-mode\"\n"; got != want {
		t.Errorf("pending only:\n%s\nwant\n%s", got, want)
	}
	if (Record{Pending: []string{"x"}}).Empty() || !(Record{NitroCompute: NitroNodeGroup}).Empty() {
		t.Error("Empty: a pending plugin is something, a NITRO_COMPUTE alone is not")
	}

	// The NITRO_COMPUTE belongs to a plugin that follows it, while it is
	// installed or pending.
	follows, other := Plugin{ID: "mode3-iso", FollowsNitroCompute: true}, Plugin{ID: "efs"}
	for _, c := range []struct {
		r    Record
		p    Plugin
		want string
	}{
		{Record{Plugins: []string{"mode3-iso"}, NitroCompute: NitroNodeGroup}, follows, NitroNodeGroup},
		{Record{Pending: []string{"mode3-iso"}, NitroCompute: NitroNodeGroup}, follows, NitroNodeGroup},
		{Record{Plugins: []string{"efs"}, NitroCompute: NitroNodeGroup}, follows, ""},
		{Record{Plugins: []string{"efs", "mode3-iso"}, NitroCompute: NitroNodeGroup}, other, ""},
		{Record{Plugins: []string{"mode3-iso"}}, follows, ""},
	} {
		if got := c.r.NitroComputeOf(c.p); got != c.want {
			t.Errorf("%+v, %s: %q, want %q", c.r, c.p.ID, got, c.want)
		}
	}
}

// modeHook logs what with the NITRO_COMPUTE the hook ran with.
func modeHook(f *fakeCluster, what string, err error) func(context.Context, io.Writer, Options) error {
	return func(_ context.Context, _ io.Writer, o Options) error {
		f.log = append(f.log, what+" NITRO_COMPUTE="+o.NitroCompute)
		return err
	}
}

// nitroFixture is runFixture plus iso, a plugin that follows NITRO_COMPUTE
// and has AWS resources, as mode3-iso would with an AWS half; deployErr and
// verifyErr fail its Deploy and Verify.
func nitroFixture(t *testing.T, f *fakeCluster, deployErr, verifyErr error) Options {
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "iso", Order: 5, SetupAWS: true, FollowsNitroCompute: true,
		Deploy: modeHook(f, "deploy iso", deployErr), Verify: modeHook(f, "verify iso", verifyErr), Remove: modeHook(f, "remove iso", nil)})
	return o
}

// Deploy records a plugin that follows NITRO_COMPUTE as pending, with the
// run's NITRO_COMPUTE (auto-mode for an empty one), before its first write
// (its AWS half, then Deploy); once Deploy and Verify pass it moves to the
// installed plugins, and the NITRO_COMPUTE stays. With no record the pending
// write is a create, and the move an apply, as every other record write.
func TestRunDeployRecordsPendingAndTheModeBeforeTheFirstWrite(t *testing.T) {
	for run, recorded := range map[string]string{NitroNodeGroup: NitroNodeGroup, NitroAutoMode: NitroAutoMode, "": NitroAutoMode} {
		f := &fakeCluster{}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = run
		var out bytes.Buffer
		if err := Run(context.Background(), &out, ActionDeploy, []string{"iso"}, o); err != nil {
			t.Fatal(err)
		}
		want := []string{"create record= pending=iso nitro-compute=" + recorded, "setup-aws iso", "deploy iso NITRO_COMPUTE=" + run,
			"verify iso NITRO_COMPUTE=" + run, "record=iso nitro-compute=" + recorded}
		if !slices.Equal(f.log, want) {
			t.Errorf("NITRO_COMPUTE %q: log = %q\nwant  %q", run, f.log, want)
		}
		if !strings.Contains(out.String(), DeployLine+"iso\n"+PendingLine+"iso with NITRO_COMPUTE="+recorded+" (kube-system/substrate-eks-plugins)\n") {
			t.Errorf("output:\n%s", out.String())
		}
	}
}

// A deploy that fails after its first write, in Deploy or in Verify,
// leaves the plugin pending with its NITRO_COMPUTE, never installed. A
// removal of every plugin then removes it, with that NITRO_COMPUTE, not the
// run's, says so, and deletes the record.
func TestRunFailedDeployStaysPendingAndIsRemoved(t *testing.T) {
	for name, errs := range map[string][2]error{"Deploy": {errors.New("boom"), nil}, "Verify": {nil, errors.New("boom")}} {
		f := &fakeCluster{}
		o := nitroFixture(t, f, errs[0], errs[1])
		o.NitroCompute = NitroNodeGroup
		if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err == nil || !strings.Contains(err.Error(), "plugin iso: boom") {
			t.Fatalf("%s: want the failure, got %v", name, err)
		}
		if f.record != "" || f.pending != "iso" || f.nitro != NitroNodeGroup {
			t.Fatalf("%s failed: %s", name, f.state())
		}
		f.log = nil
		o.NitroCompute = NitroAutoMode
		var out bytes.Buffer
		if err := Run(context.Background(), &out, ActionRemove, nil, o); err != nil {
			t.Fatal(err)
		}
		want := []string{"setup-aws iso --delete --plan", "remove iso NITRO_COMPUTE=node-group", "setup-aws iso --delete", "record deleted"}
		if !slices.Equal(f.log, want) {
			t.Errorf("%s: log = %q\nwant  %q", name, f.log, want)
		}
		if !strings.Contains(out.String(), RecordedNitroComputeLine+"iso was deployed with NITRO_COMPUTE=node-group, as kube-system/substrate-eks-plugins records, so it is removed with node-group, not the run's auto-mode\n") {
			t.Errorf("%s: output:\n%s", name, out.String())
		}
	}
}

// Removal uses the NITRO_COMPUTE the record holds, both ways, and says so
// only when it differs from the run's (auto-mode for an empty one); a record
// an older installer wrote, with none, removes with the run's, as before,
// and says nothing. The NITRO_COMPUTE goes with the plugin: what stays is
// the record as written before, byte for byte.
func TestRunRemoveUsesTheRecordedNitroCompute(t *testing.T) {
	for _, c := range []struct {
		recorded, run, with string
		says                bool
	}{
		{NitroAutoMode, NitroNodeGroup, NitroAutoMode, true},
		{NitroNodeGroup, NitroAutoMode, NitroNodeGroup, true},
		{NitroNodeGroup, "", NitroNodeGroup, true},
		{NitroNodeGroup, NitroNodeGroup, NitroNodeGroup, false},
		{NitroAutoMode, "", NitroAutoMode, false},
		{"", NitroNodeGroup, NitroNodeGroup, false},
		{"", "", "", false},
	} {
		f := &fakeCluster{record: "iso,solo", nitro: c.recorded}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = c.run
		var out bytes.Buffer
		if err := Run(context.Background(), &out, ActionRemove, []string{"iso"}, o); err != nil {
			t.Fatal(err)
		}
		want := []string{"setup-aws iso --delete --plan", "remove iso NITRO_COMPUTE=" + c.with, "setup-aws iso --delete", "record=solo"}
		if !slices.Equal(f.log, want) {
			t.Errorf("%+v: log = %q\nwant  %q", c, f.log, want)
		}
		if says := strings.Contains(out.String(), RecordedNitroComputeLine); says != c.says {
			t.Errorf("%+v: says %v:\n%s", c, says, out.String())
		}
		if len(f.applies) != 1 || f.applies[0] != mainRecord("solo") {
			t.Errorf("%+v: wrote %q", c, f.applies)
		}
	}
}

// Both ways: a deploy with another NITRO_COMPUTE than the one recorded for
// the plugin, installed or pending, is refused before anything is written,
// with the two ways out; with the same one, with a record that holds none
// (an older installer's), or with one recorded while the plugin is neither
// installed nor pending, it deploys and records the run's.
func TestRunDeployRefusesAnotherNitroComputeThanTheRecorded(t *testing.T) {
	for _, c := range []struct {
		record, pending, recorded, run string
	}{
		{"iso", "", NitroAutoMode, NitroNodeGroup},
		{"iso", "", NitroNodeGroup, NitroAutoMode},
		{"iso", "", NitroNodeGroup, ""},
		{"", "iso", NitroAutoMode, NitroNodeGroup},
	} {
		f := &fakeCluster{record: c.record, pending: c.pending, nitro: c.recorded}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = c.run
		err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o)
		if err == nil || !strings.Contains(err.Error(), "plugin iso was deployed with NITRO_COMPUTE="+c.recorded+" (recorded in kube-system/substrate-eks-plugins)") ||
			!strings.Contains(err.Error(), "deploy it with --nitro-compute "+c.recorded) || !strings.Contains(err.Error(), "--plugin-action remove --plugins iso") ||
			strings.Contains(err.Error(), "--deploy-control-plane") {
			// iso has no control plane needs: no control plane step.
			t.Errorf("%+v: want the refusal, got %v", c, err)
		}
		if len(f.log) != 0 || len(f.applies) != 0 {
			t.Errorf("%+v: ran %q after the refusal", c, f.log)
		}
	}
	for _, c := range []struct {
		record, recorded, run, want string
	}{
		{"iso", NitroNodeGroup, NitroNodeGroup, NitroNodeGroup},
		{"iso", NitroAutoMode, "", NitroAutoMode},
		{"iso", "", NitroNodeGroup, NitroNodeGroup},
		{"solo", NitroAutoMode, NitroNodeGroup, NitroNodeGroup},
	} {
		f := &fakeCluster{record: c.record, nitro: c.recorded}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = c.run
		if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err != nil {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		if !slices.Contains(f.log, "deploy iso NITRO_COMPUTE="+c.run) || f.nitro != c.want || f.pending != "" {
			t.Errorf("%+v: log %q, %s", c, f.log, f.state())
		}
	}
}

// #22 (review N2 of #21), both ways: a deploy with another NITRO_COMPUTE
// than the one recorded for its plugin gets the record's refusal, with its
// ways out, before the Auto Mode check and the node check judge the run's
// value: on a cluster without Auto Mode (isoauto runs there only with
// node-group, as mode3-iso does) and on one without the nodes isonodes
// runs on. With nothing recorded those checks still refuse the run's
// value, before any write.
func TestRunDeployChecksTheRecordedNitroComputeFirst(t *testing.T) {
	for _, c := range []struct{ id, check string }{
		{"isoauto", "plugin isoauto needs EKS Auto Mode; demo has EKS Auto Mode off"},
		{"isonodes", "plugin isonodes runs on the nitro node group, and demo has no node labeled ate.dev/pool=nitro"},
	} {
		for _, recorded := range []bool{true, false} {
			f := &fakeCluster{}
			if recorded {
				f.record, f.nitro = c.id, NitroNodeGroup
			}
			o := nitroFixture(t, f, nil, nil)
			Register(Plugin{ID: "isoauto", Order: 6, FollowsNitroCompute: true, NeedsAutoMode: "needs EKS Auto Mode",
				AutoModeOptional: func(o Options) bool { return o.NitroCompute == NitroNodeGroup }, Deploy: modeHook(f, "deploy isoauto", nil)})
			Register(Plugin{ID: "isonodes", Order: 7, FollowsNitroCompute: true, Nodes: []string{"nitro"}, Deploy: modeHook(f, "deploy isonodes", nil)})
			o.NitroCompute, o.Pools = NitroAutoMode, []string{"system"}
			err := Run(context.Background(), io.Discard, ActionDeploy, []string{c.id}, o)
			want := c.check
			if recorded {
				want = "plugin " + c.id + " was deployed with NITRO_COMPUTE=node-group (recorded in kube-system/substrate-eks-plugins), not auto-mode: " +
					"deploy it with --nitro-compute node-group, or remove it first"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s, recorded %v: want %q, got %v", c.id, recorded, want, err)
			}
			if len(f.log) != 0 || len(f.writes) != 0 || f.reads != 1 {
				t.Errorf("%s, recorded %v: ran %q, record writes %q, %d record reads (want 1)", c.id, recorded, f.log, f.writes, f.reads)
			}
		}
	}
}

// #22 (review N2 of #21): the deploy reads the record before its checks
// only for a plugin that follows NITRO_COMPUTE, as verify does, and then
// uses that read, so neither makes a read more than before: a deploy of
// base reads it in Run and in AddToRecord, one of iso also in markPending;
// a verify reads it once for iso and never for base.
func TestRunReadsTheRecordOnceBeforeItsChecks(t *testing.T) {
	for _, c := range []struct {
		action, id, record string
		reads              int
	}{
		{ActionDeploy, "base", "solo", 2},
		{ActionDeploy, "iso", "solo", 3},
		{ActionVerify, "base", "base,iso", 0},
		{ActionVerify, "iso", "base,iso", 1},
	} {
		f := &fakeCluster{record: c.record}
		o := nitroFixture(t, f, nil, nil)
		if err := Run(context.Background(), io.Discard, c.action, []string{c.id}, o); err != nil {
			t.Fatalf("%s %s: %v", c.action, c.id, err)
		}
		if f.reads != c.reads {
			t.Errorf("%s %s: %d record reads, want %d", c.action, c.id, f.reads, c.reads)
		}
	}
}

// A NITRO_COMPUTE in the record that this installer does not know stops a
// removal, a deploy or a check of its plugin before anything is removed,
// written or checked, as an unknown plugin does; the record is left as it
// was. A removal of another plugin leaves the value as it is.
func TestRunRefusesAnUnknownRecordedNitroCompute(t *testing.T) {
	f := &fakeCluster{record: "iso,solo", nitro: "on-the-moon"}
	o := nitroFixture(t, f, nil, nil)
	for _, action := range []string{ActionRemove, ActionDeploy, ActionVerify} {
		err := Run(context.Background(), io.Discard, action, []string{"iso"}, o)
		if err == nil || !strings.Contains(err.Error(), `plugin iso: kube-system/substrate-eks-plugins records nitro-compute="on-the-moon", which is not auto-mode or node-group: this installer cannot deploy, check or remove it`) {
			t.Errorf("%s: want the refusal, got %v", action, err)
		}
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err == nil {
		t.Error("removal of every plugin went on")
	}
	if len(f.log) != 0 || f.state() != "record=iso,solo nitro-compute=on-the-moon" {
		t.Errorf("ran after the refusal: %q, %s", f.log, f.state())
	}
	// solo does not follow NITRO_COMPUTE, so the value is not its.
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"solo"}, o); err != nil {
		t.Errorf("solo: %v", err)
	}
	if f.state() != "record=iso nitro-compute=on-the-moon" {
		t.Errorf("after solo's removal: %s", f.state())
	}
}

// A pending plugin that stays keeps what it requires, as an installed one
// does: removing that alone is refused before anything is removed.
func TestRunRemoveKeepsWhatAPendingPluginRequires(t *testing.T) {
	f := &fakeCluster{record: "base", pending: "needy", nitro: NitroNodeGroup}
	o := nitroFixture(t, f, nil, nil)
	Register(Plugin{ID: "needy", Order: 6, Requires: []string{"base"}, FollowsNitroCompute: true, Remove: modeHook(f, "remove needy", nil)})
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"base"}, o); err == nil || !strings.Contains(err.Error(), "plugin needy requires base: remove it too") {
		t.Errorf("want the requirement refusal, got %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("removed after the refusal: %q", f.log)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err != nil {
		t.Fatal(err)
	}
	if want := []string{"setup-aws base --delete --plan", "remove needy NITRO_COMPUTE=node-group", "record=base", "remove base", "setup-aws base --delete", "record deleted"}; !slices.Equal(f.log, want) {
		t.Errorf("log = %q\nwant  %q", f.log, want)
	}
}

// A deploy, the control plane's record and a removal of plugins that do not
// follow NITRO_COMPUTE write the record as before, byte for byte, with any
// NITRO_COMPUTE in the run: one write per change, plugins only.
func TestRecordWritesAreAsBeforeWithoutTheNitroClass(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	o.NitroCompute = NitroNodeGroup
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"solo"}, o); err != nil {
		t.Fatal(err)
	}
	f.record = "top"
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"top"}, o); err != nil {
		t.Fatal(err)
	}
	if err := AddToRecord(context.Background(), io.Discard, o, "base", "solo", "top"); err != nil {
		t.Fatal(err)
	}
	if err := AddToRecord(context.Background(), io.Discard, o, "solo"); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"solo"}, o); err != nil {
		t.Fatal(err)
	}
	want := []string{mainRecord("solo"), mainRecord("base,top"), mainRecord("base,solo,top"), mainRecord("base,top")}
	if !slices.Equal(f.applies, want) {
		t.Errorf("writes:\n%s\nwant\n%s", strings.Join(f.applies, "---\n"), strings.Join(want, "---\n"))
	}
	if strings.Contains(strings.Join(f.applies, ""), PendingKey) || strings.Contains(strings.Join(f.applies, ""), NitroComputeKey) {
		t.Error("wrote a key the nitro class alone uses")
	}
}

// The control plane's record (AddToRecord) moves a pending plugin to the
// installed ones and keeps its NITRO_COMPUTE, so its removal still uses it.
func TestAddToRecordMovesPendingAndKeepsTheMode(t *testing.T) {
	f := &fakeCluster{record: "base", pending: "iso", nitro: NitroNodeGroup}
	o := nitroFixture(t, f, nil, nil)
	if err := AddToRecord(context.Background(), io.Discard, o, "base", "iso"); err != nil {
		t.Fatal(err)
	}
	if f.state() != "record=base,iso nitro-compute=node-group" {
		t.Errorf("after the control plane's record: %s", f.state())
	}
	f.log = nil
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"iso"}, o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "remove iso NITRO_COMPUTE=node-group") || f.state() != "record=base" {
		t.Errorf("removal: %q, %s", f.log, f.state())
	}
}

// A record that cannot be read, or read as the ConfigMap, stops a deploy, a
// removal and the control plane's record before anything is written: it is
// never taken as an empty record. A record with a key this installer does
// not know stays readable.
func TestRecordReadFailuresStop(t *testing.T) {
	denied := errors.New("the server is currently unable to handle the request")
	f := &fakeCluster{record: "base", readErr: denied}
	o := nitroFixture(t, f, nil, nil)
	for name, run := range map[string]func() error{
		"deploy":       func() error { return Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o) },
		"remove":       func() error { return Run(context.Background(), io.Discard, ActionRemove, nil, o) },
		"AddToRecord":  func() error { return AddToRecord(context.Background(), io.Discard, o, "iso") },
		"ReadRecord":   func() error { _, err := ReadRecord(context.Background(), io.Discard, o); return err },
		"ReadRelease":  func() error { _, err := ReadRelease(context.Background(), io.Discard, o, nil); return err },
		"remove named": func() error { return Run(context.Background(), io.Discard, ActionRemove, []string{"iso"}, o) },
	} {
		if err := run(); !errors.Is(err, denied) || !strings.Contains(err.Error(), "read the plugin record") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("ran after a failed read: %q", f.log)
	}

	prev := Exec
	t.Cleanup(func() { Exec = prev })
	var wrote []string
	Exec = func(_ context.Context, _ io.Writer, c Cmd) error {
		if slices.Contains(c.Argv, "get") {
			io.WriteString(c.Stdout, "plugins: base\n")
			return nil
		}
		wrote = append(wrote, strings.Join(c.Argv, " "))
		return nil
	}
	if _, err := LoadRecord(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "read the plugin record: read ConfigMap") {
		t.Errorf("unreadable output: %v", err)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err == nil || len(wrote) != 0 {
		t.Errorf("remove on unreadable output: %v, wrote %q", err, wrote)
	}

	f = &fakeCluster{record: "iso", nitro: NitroNodeGroup, extra: "from a newer installer"}
	o = nitroFixture(t, f, nil, nil)
	r, err := LoadRecord(context.Background(), io.Discard, o)
	if err != nil || !r.Equal(Record{Plugins: []string{"iso"}, NitroCompute: NitroNodeGroup}) {
		t.Errorf("a record with an unknown key: %+v, %v", r, err)
	}
}

// A pending plugin is not installed, so the control plane is not taken to
// carry its needs: with nothing else recorded, deploying it again is refused
// as a plugin with another release is, before anything is written; with a
// recorded plugin that carries them it deploys and moves to the installed
// ones.
func TestRunPendingPluginDoesNotCarryItsNeeds(t *testing.T) {
	for record, carried := range map[string]bool{"base": false, "base,fork": true} {
		f := &fakeCluster{record: record, pending: "iso-fork", nitro: NitroNodeGroup}
		o := releaseFixture(t, f)
		o.NitroCompute = NitroNodeGroup
		same := *testFork
		Register(Plugin{ID: "iso-fork", Order: 6, FollowsNitroCompute: true, Needs: Needs{Preview: []string{"NetworkWorkers"}, Source: &same},
			Deploy: modeHook(f, "deploy iso-fork", nil), Verify: modeHook(f, "verify iso-fork", nil)})
		err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso-fork"}, o)
		if !carried {
			if err == nil || !strings.Contains(err.Error(), "plugin iso-fork installs another Substrate release than the control plane runs") || len(f.log) != 0 {
				t.Errorf("only pending: %v, ran %q", err, f.log)
			}
			continue
		}
		if err != nil || f.state() != "record=base,fork,iso-fork nitro-compute=node-group" {
			t.Errorf("carried by fork: %v, %s", err, f.state())
		}
	}
}

// lazyFixture is nitroFixture plus lazy, a plugin that follows
// NITRO_COMPUTE with no AWS half, as mode3-iso: its Deploy reads, refuses
// while *refuse is set (too little capacity, say), calls o.BeforeWrite
// calls times, writes, and fails with deployErr.
func lazyFixture(t *testing.T, f *fakeCluster, refuse *bool, calls int, deployErr error) Options {
	o := nitroFixture(t, f, nil, nil)
	Register(Plugin{ID: "lazy", Order: 6, FollowsNitroCompute: true,
		Deploy: func(_ context.Context, _ io.Writer, o Options) error {
			f.log = append(f.log, "read lazy")
			if *refuse {
				return errors.New("too little capacity")
			}
			for range calls {
				if err := o.BeforeWrite(); err != nil {
					return err
				}
			}
			f.log = append(f.log, "write lazy")
			return deployErr
		},
		Verify: modeHook(f, "verify lazy", nil), Remove: modeHook(f, "remove lazy", nil)})
	return o
}

// A deploy whose record changed between its checks and its first write
// (another run recorded the plugin with the other NITRO_COMPUTE) stops at
// the pending write, before it writes anything.
func TestRunPendingRechecksTheRecordedNitroCompute(t *testing.T) {
	f := &fakeCluster{}
	o := nitroFixture(t, f, nil, nil)
	Register(Plugin{ID: "racer", Order: 6, FollowsNitroCompute: true,
		Deploy: func(_ context.Context, _ io.Writer, o Options) error {
			// Another run's auto-mode deploy, between this one's reads and
			// its first write.
			f.pending, f.nitro = "racer", NitroAutoMode
			if err := o.BeforeWrite(); err != nil {
				return err
			}
			f.log = append(f.log, "write racer")
			return nil
		}})
	o.NitroCompute = NitroNodeGroup
	err := Run(context.Background(), io.Discard, ActionDeploy, []string{"racer"}, o)
	if err == nil || !strings.Contains(err.Error(), "another run recorded racer meanwhile") ||
		!strings.Contains(err.Error(), "was deployed with NITRO_COMPUTE=auto-mode") || slices.Contains(f.log, "write racer") || len(f.applies) != 0 {
		t.Errorf("err %v, log %q, writes %q", err, f.log, f.applies)
	}
}

// The record read keeps the ConfigMap's resourceVersion ("" with none), for
// the pending write alone: Equal ignores it, and Manifest, which every other
// write sends, never carries it, so with only plugins it is still the
// record older installers write. The pending write's manifest carries it
// in metadata.
func TestRecordResourceVersion(t *testing.T) {
	js := `{"metadata":{"name":"substrate-eks-plugins","resourceVersion":"812"},"data":{"plugins":"efs","pending":"mode3-iso","nitro-compute":"node-group"}}`
	r, err := ParseRecordJSON([]byte(js))
	if err != nil || r.ResourceVersion != "812" {
		t.Fatalf("%+v, %v", r, err)
	}
	if !r.Equal(Record{Plugins: []string{"efs"}, Pending: []string{"mode3-iso"}, NitroCompute: NitroNodeGroup}) {
		t.Errorf("Equal minds the resourceVersion: %+v", r)
	}
	if r, err := ParseRecordJSON([]byte(`{"data":{"plugins":"efs"}}`)); err != nil || r.ResourceVersion != "" {
		t.Errorf("no metadata: %+v, %v", r, err)
	}
	if got := string(Record{Plugins: []string{"efs"}, ResourceVersion: "812"}.Manifest()); got != mainRecord("efs") {
		t.Errorf("Manifest with a resourceVersion read:\n%s\nwant\n%s", got, mainRecord("efs"))
	}
	want := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: substrate-eks-plugins\n  namespace: kube-system\n  resourceVersion: \"812\"\n" +
		"  labels:\n    app.kubernetes.io/managed-by: substrate-eks\ndata:\n  plugins: \"efs\"\n  pending: \"mode3-iso\"\n  nitro-compute: \"node-group\"\n"
	if got := string(r.manifest(r.ResourceVersion)); got != want {
		t.Errorf("the pending write's manifest:\n%s\nwant\n%s", got, want)
	}
}

// The pending write is conditional both ways, with nothing in between: with
// no record it is a create, and with one a replace that carries the
// resourceVersion read, byte for byte; each then goes through.
func TestRunPendingWriteIsCreateOrReplace(t *testing.T) {
	for _, c := range []struct {
		record, write string
	}{
		{"", "create record= pending=iso nitro-compute=node-group"},
		{"solo", "replace record=solo pending=iso nitro-compute=node-group"},
	} {
		f := &fakeCluster{record: c.record}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = NitroNodeGroup
		rv := f.resourceVersion()
		if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err != nil {
			t.Fatal(err)
		}
		if len(f.log) == 0 || f.log[0] != c.write || len(f.conditional) != 1 {
			t.Errorf("%s: log %q", c.write, f.log)
			continue
		}
		pending := Record{Plugins: ParseRecord(c.record), Pending: []string{"iso"}, NitroCompute: NitroNodeGroup}
		if want := string(pending.manifest(rv)); strings.HasPrefix(c.write, "replace") && f.conditional[0] != want {
			t.Errorf("replace:\n%s\nwant\n%s", f.conditional[0], want)
		}
		if want := string(pending.Manifest()); strings.HasPrefix(c.write, "create") && f.conditional[0] != want {
			t.Errorf("create:\n%s\nwant\n%s", f.conditional[0], want)
		}
	}
}

// (a) Another run's deploy with the opposite NITRO_COMPUTE writes its
// pending entry between this deploy's read and its pending write: the write
// is refused (AlreadyExists when none was read, Conflict when one was), the
// record is read again, and the check refuses this deploy. Nothing else is
// written or run: the other run's entry stays as it wrote it.
func TestRunPendingWriteConflictsWithAnOppositeModeDeploy(t *testing.T) {
	for _, c := range []struct {
		record, refused string
	}{
		{"", "create refused: AlreadyExists"},
		{"solo", "replace refused: Conflict"},
	} {
		f := &fakeCluster{record: c.record}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = NitroNodeGroup
		f.meanwhile = func(try int) {
			if try == 1 {
				f.pending, f.nitro = "iso", NitroAutoMode
				f.rv++
			}
		}
		var out bytes.Buffer
		err := Run(context.Background(), &out, ActionDeploy, []string{"iso"}, o)
		if err == nil || !strings.Contains(err.Error(), "plugin iso: another run recorded iso meanwhile: plugin iso was deployed with NITRO_COMPUTE=auto-mode") {
			t.Errorf("%s: want the refusal, got %v", c.refused, err)
		}
		if !slices.Equal(f.log, []string{c.refused}) || len(f.writes) != 1 || len(f.conditional)+len(f.applies) != 0 || f.reads != 3 {
			t.Errorf("%s: log %q, writes %q, %d reads (Run's, the pending write's, the one after the refusal)", c.refused, f.log, f.writes, f.reads)
		}
		if want := "record=" + c.record + " pending=iso nitro-compute=auto-mode"; f.state() != want {
			t.Errorf("%s: %s, want the other run's %s", c.refused, f.state(), want)
		}
		if !strings.Contains(out.String(), RecordChangedLine+"iso\n") {
			t.Errorf("%s: output:\n%s", c.refused, out.String())
		}
	}
}

// (b) Another run makes the record between this deploy's read, which found
// none, and its create: the create fails with AlreadyExists, the record is
// read again, and the deploy goes on with the same NITRO_COMPUTE. Over the
// other run's control plane record (solo) it replaces it, conditional on
// the resourceVersion now read, keeping solo; over its pending entry with
// the same NITRO_COMPUTE there is nothing left to write.
func TestRunPendingCreateMeetsARecordMadeMeanwhile(t *testing.T) {
	for _, c := range []struct {
		name     string
		other    func(f *fakeCluster)
		log      []string
		writes   []string
		finalRec string
	}{
		{"the control plane step's record", func(f *fakeCluster) { f.record = "solo" },
			[]string{"create refused: AlreadyExists", "replace record=solo pending=iso nitro-compute=node-group", "setup-aws iso",
				"deploy iso NITRO_COMPUTE=node-group", "verify iso NITRO_COMPUTE=node-group", "record=iso,solo nitro-compute=node-group"},
			[]string{"create", "replace", "apply"}, "record=iso,solo nitro-compute=node-group"},
		{"a pending entry with the same NITRO_COMPUTE", func(f *fakeCluster) { f.pending, f.nitro = "iso", NitroNodeGroup },
			[]string{"create refused: AlreadyExists", "setup-aws iso", "deploy iso NITRO_COMPUTE=node-group", "verify iso NITRO_COMPUTE=node-group",
				"record=iso nitro-compute=node-group"},
			[]string{"create", "apply"}, "record=iso nitro-compute=node-group"},
	} {
		f := &fakeCluster{}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = NitroNodeGroup
		f.meanwhile = func(try int) {
			if try == 1 {
				c.other(f)
				f.rv++
			}
		}
		if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !slices.Equal(f.log, c.log) || !slices.Equal(f.writes, c.writes) || f.state() != c.finalRec {
			t.Errorf("%s: log = %q\nwant  %q\nwrites %q, %s", c.name, f.log, c.log, f.writes, f.state())
		}
	}
}

// (c) The record changes between every read and the write after it (other
// runs deploy and remove base over and over): the pending write stops after
// three tries with an error that says so, before the plugin writes
// anything.
func TestRunPendingWriteStopsAfterThreeConflicts(t *testing.T) {
	f := &fakeCluster{record: "solo"}
	o := nitroFixture(t, f, nil, nil)
	o.NitroCompute = NitroNodeGroup
	f.meanwhile = func(try int) {
		f.record = []string{"solo", "base,solo"}[try%2]
		f.rv++
	}
	var out bytes.Buffer
	err := Run(context.Background(), &out, ActionDeploy, []string{"iso"}, o)
	if !errors.Is(err, errRecordChanged) || !strings.Contains(err.Error(),
		"plugin iso: kube-system/substrate-eks-plugins changed between the read and the write on each of 3 tries to record iso as pending: another run is writing it; let it finish, then deploy again") {
		t.Errorf("want the error after 3 tries, got %v", err)
	}
	want := []string{"replace refused: Conflict", "replace refused: Conflict", "replace refused: Conflict"}
	if !slices.Equal(f.log, want) || !slices.Equal(f.writes, []string{"replace", "replace", "replace"}) || f.reads != 4 {
		t.Errorf("log = %q, writes %q, %d reads\nwant  %q, 4 reads", f.log, f.writes, f.reads, want)
	}
	if n := strings.Count(out.String(), RecordChangedLine+"iso\n"); n != 2 {
		t.Errorf("read again %d times, want 2:\n%s", n, out.String())
	}
}

// A replace after another run deleted the record (NotFound) counts as the
// record changed: the next try reads none and creates it. Any other failure
// of the pending write is returned as it is, with no second read or try.
func TestRunPendingWriteReturnsOtherErrors(t *testing.T) {
	f := &fakeCluster{record: "solo"}
	o := nitroFixture(t, f, nil, nil)
	o.NitroCompute = NitroNodeGroup
	gone := false
	f.meanwhile = func(int) {
		if !gone {
			gone, f.record = true, ""
		}
	}
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err != nil {
		t.Errorf("a replace that finds the record deleted: %v", err)
	}
	if len(f.log) == 0 || f.log[0] != "replace refused: NotFound" || !slices.Contains(f.writes, "create") {
		t.Errorf("log %q, writes %q", f.log, f.writes)
	}

	prev := Exec
	t.Cleanup(func() { Exec = prev })
	denied := errors.New("exit status 1")
	tries := 0
	Exec = func(ctx context.Context, w io.Writer, c Cmd) error {
		if slices.Contains(c.Argv, "create") {
			tries++
			io.WriteString(w, `Error from server (Forbidden): configmaps is forbidden: User "x" cannot create resource "configmaps"`+"\n")
			return denied
		}
		return f.exec(ctx, w, c)
	}
	f = &fakeCluster{}
	o.NitroCompute = NitroNodeGroup
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); !errors.Is(err, denied) || errors.Is(err, errRecordChanged) || tries != 1 {
		t.Errorf("a denied create: %v, %d tries", err, tries)
	}
}

// (d) Only the pending write is conditional. A deploy, the control plane's
// record and a removal of plugins that do not follow NITRO_COMPUTE make
// only kubectl apply (and delete) calls, as before; so do the move of a
// pending plugin, the control plane's record of it and its removal.
func TestRecordWritesOtherThanPendingAreApply(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"solo"}, o); err != nil {
		t.Fatal(err)
	}
	f.record = "top"
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"top"}, o); err != nil {
		t.Fatal(err)
	}
	if err := AddToRecord(context.Background(), io.Discard, o, "base", "solo", "top"); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply", "apply", "apply", "apply", "apply", "delete"}; !slices.Equal(f.writes, want) || len(f.conditional) != 0 {
		t.Errorf("the default path wrote with %q, want %q", f.writes, want)
	}

	f = &fakeCluster{record: "solo"}
	o = nitroFixture(t, f, nil, nil)
	o.NitroCompute = NitroNodeGroup
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err != nil {
		t.Fatal(err)
	}
	if err := AddToRecord(context.Background(), io.Discard, o, "base", "iso", "solo"); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err != nil {
		t.Fatal(err)
	}
	if want := []string{"replace", "apply", "apply", "apply", "apply", "delete"}; !slices.Equal(f.writes, want) {
		t.Errorf("with iso: wrote with %q, want %q", f.writes, want)
	}
}

// The pending entry is written just before the plugin's first write, when
// Deploy calls o.BeforeWrite after its reads: a Deploy that refuses after
// its reads leaves the record as it was, so a deploy with another
// NITRO_COMPUTE then goes on (review 1, finding 1). Called twice it writes
// once and reads once. A Deploy that never calls it is recorded once it
// returns, before Verify, so its NITRO_COMPUTE is recorded all the same.
func TestRunPendingJustBeforeTheFirstWrite(t *testing.T) {
	f := &fakeCluster{}
	refuse := true
	o := lazyFixture(t, f, &refuse, 1, nil)
	o.NitroCompute = NitroNodeGroup
	var out bytes.Buffer
	if err := Run(context.Background(), &out, ActionDeploy, []string{"lazy"}, o); err == nil || !strings.Contains(err.Error(), "plugin lazy: too little capacity") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if !slices.Equal(f.log, []string{"read lazy"}) || len(f.applies) != 0 || f.state() != "record=" || strings.Contains(out.String(), PendingLine) {
		t.Errorf("a refusal after the reads: %q, %s\n%s", f.log, f.state(), out.String())
	}
	f.log, refuse = nil, false
	o.NitroCompute = NitroAutoMode
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"lazy"}, o); err != nil {
		t.Fatalf("the auto-mode deploy after the refusal: %v", err)
	}
	if want := []string{"read lazy", "create record= pending=lazy nitro-compute=auto-mode", "write lazy", "verify lazy NITRO_COMPUTE=auto-mode", "record=lazy nitro-compute=auto-mode"}; !slices.Equal(f.log, want) {
		t.Errorf("log = %q\nwant  %q", f.log, want)
	}

	f = &fakeCluster{}
	o = lazyFixture(t, f, &refuse, 2, errors.New("boom"))
	o.NitroCompute = NitroNodeGroup
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"lazy"}, o); err == nil || !strings.Contains(err.Error(), "plugin lazy: boom") {
		t.Fatalf("want the failure, got %v", err)
	}
	if want := []string{"read lazy", "create record= pending=lazy nitro-compute=node-group", "write lazy"}; !slices.Equal(f.log, want) || f.reads != 2 {
		t.Errorf("called twice: log = %q, %d reads (Run's and one for the pending entry)\nwant  %q", f.log, f.reads, want)
	}

	f = &fakeCluster{}
	o = lazyFixture(t, f, &refuse, 0, nil)
	o.NitroCompute = NitroNodeGroup
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"lazy"}, o); err != nil {
		t.Fatal(err)
	}
	if want := []string{"read lazy", "write lazy", "create record= pending=lazy nitro-compute=node-group", "verify lazy NITRO_COMPUTE=node-group",
		"record=lazy nitro-compute=node-group"}; !slices.Equal(f.log, want) {
		t.Errorf("never called: log = %q\nwant  %q", f.log, want)
	}
}

// A plugin still pending with the same NITRO_COMPUTE (a deploy that failed,
// run again) gets no second pending write and no pending line: its deploy
// writes the record once, to move it to the installed ones.
func TestRunDeployOfAPendingPluginWritesOnlyTheMove(t *testing.T) {
	f := &fakeCluster{pending: "iso", nitro: NitroNodeGroup}
	o := nitroFixture(t, f, nil, nil)
	o.NitroCompute = NitroNodeGroup
	var out bytes.Buffer
	if err := Run(context.Background(), &out, ActionDeploy, []string{"iso"}, o); err != nil {
		t.Fatal(err)
	}
	if want := []string{mainRecord("iso") + "  nitro-compute: \"node-group\"\n"}; !slices.Equal(f.applies, want) {
		t.Errorf("writes:\n%s\nwant\n%s", strings.Join(f.applies, "---\n"), strings.Join(want, "---\n"))
	}
	if strings.Contains(out.String(), PendingLine) {
		t.Errorf("said it records it as pending:\n%s", out.String())
	}
}

// The refusal's second way out works to the end from the CLI, for a plugin
// with control plane needs (the fork's release, as mode3-iso): the removal
// takes it out of the record, so a deploy is refused for the release until
// the control plane deploy records it again; then the deploy with the new
// NITRO_COMPUTE goes on and records it (review 1, finding 2). The first way
// out, the recorded value, deploys at once.
func TestRunRefusalWaysOutWork(t *testing.T) {
	f := &fakeCluster{record: "base,iso-fork", nitro: NitroAutoMode}
	o := nitroFixture(t, f, nil, nil)
	same := *testFork
	Register(Plugin{ID: "iso-fork", Order: 6, FollowsNitroCompute: true, Needs: Needs{Preview: []string{"NetworkWorkers"}, Source: &same},
		Deploy: modeHook(f, "deploy iso-fork", nil), Verify: modeHook(f, "verify iso-fork", nil), Remove: modeHook(f, "remove iso-fork", nil)})
	o.NitroCompute = NitroNodeGroup
	deploy := func(o Options) error {
		return Run(context.Background(), io.Discard, ActionDeploy, []string{"iso-fork"}, o)
	}
	const want = "plugin iso-fork was deployed with NITRO_COMPUTE=auto-mode (recorded in kube-system/substrate-eks-plugins), not node-group: " +
		"deploy it with --nitro-compute auto-mode, or remove it first with --plugin-action remove --plugins iso-fork (which removes it with NITRO_COMPUTE=auto-mode), " +
		"run --deploy-control-plane --plugins iso-fork --nitro-compute node-group again (or rerun the wizard, whose control plane step does it), " +
		"then deploy it with --nitro-compute node-group"
	if err := deploy(o); err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	if err := CheckNitroRecord(context.Background(), io.Discard, o, []string{"iso-fork"}); err == nil || err.Error() != want {
		t.Errorf("the control plane deploy's check: %v", err)
	}
	auto := o
	auto.NitroCompute = NitroAutoMode
	if err := deploy(auto); err != nil || f.state() != "record=base,iso-fork nitro-compute=auto-mode" {
		t.Fatalf("with the recorded value: %v, %s", err, f.state())
	}

	f.log = nil
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"iso-fork"}, o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "remove iso-fork NITRO_COMPUTE=auto-mode") || f.state() != "record=base" {
		t.Fatalf("removal: %q, %s", f.log, f.state())
	}
	if err := deploy(o); err == nil || !strings.Contains(err.Error(), "plugin iso-fork installs another Substrate release than the control plane runs") {
		t.Fatalf("without the control plane step: %v", err)
	}
	// The control plane step: its check passes, and it records the plugin
	// (controlplane.Deploy's recordPlugins is AddToRecord).
	if err := CheckNitroRecord(context.Background(), io.Discard, o, []string{"iso-fork"}); err != nil {
		t.Fatal(err)
	}
	if err := AddToRecord(context.Background(), io.Discard, o, "base", "iso-fork"); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	if err := deploy(o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "deploy iso-fork NITRO_COMPUTE=node-group") || f.state() != "record=base,iso-fork nitro-compute=node-group" {
		t.Errorf("the deploy with the new value: %q, %s", f.log, f.state())
	}
}

// The control plane deploy's check reads the record only when a chosen
// plugin follows NITRO_COMPUTE, refuses another NITRO_COMPUTE than the
// recorded one, and passes the same one or none; a failed read stops it.
func TestCheckNitroRecord(t *testing.T) {
	f := &fakeCluster{record: "iso,solo", nitro: NitroNodeGroup}
	o := nitroFixture(t, f, nil, nil)
	ctx := context.Background()
	if err := CheckNitroRecord(ctx, io.Discard, o, []string{"solo", "base"}); err != nil || f.reads != 0 {
		t.Errorf("no plugin that follows NITRO_COMPUTE: %v, %d reads", err, f.reads)
	}
	for run, refused := range map[string]bool{"": true, NitroAutoMode: true, NitroNodeGroup: false} {
		o.NitroCompute = run
		err := CheckNitroRecord(ctx, io.Discard, o, []string{"solo", "iso"})
		if refused != (err != nil) || refused && !strings.Contains(err.Error(), "plugin iso was deployed with NITRO_COMPUTE=node-group") {
			t.Errorf("NITRO_COMPUTE %q: %v", run, err)
		}
	}
	f.nitro = ""
	o.NitroCompute = NitroAutoMode
	if err := CheckNitroRecord(ctx, io.Discard, o, []string{"iso"}); err != nil {
		t.Errorf("none recorded: %v", err)
	}
	denied := errors.New("denied")
	f.readErr = denied
	if err := CheckNitroRecord(ctx, io.Discard, o, []string{"iso"}); !errors.Is(err, denied) {
		t.Errorf("failed read: %v", err)
	}
	if len(f.log) != 0 || len(f.applies) != 0 {
		t.Errorf("the check ran or wrote: %q", f.log)
	}
}

// A removal from a record with the keys a plugin that follows NITRO_COMPUTE
// brings reads the record again before it writes it, and drops only the
// plugin it removed: what another run recorded while Remove ran stays
// (review 1, finding 3). A removal from a record without them writes from
// the record it read, with no extra read, as before.
func TestRunRemoveKeepsWhatAnotherRunRecordedMeanwhile(t *testing.T) {
	f := &fakeCluster{record: "base,slow", pending: "iso", nitro: NitroNodeGroup}
	o := nitroFixture(t, f, nil, nil)
	Register(Plugin{ID: "slow", Order: 6, Remove: func(context.Context, io.Writer, Options) error {
		// Another run's deploys, while this Remove runs.
		f.record, f.pending, f.nitro = "base,slow,top", "iso", NitroNodeGroup
		f.log = append(f.log, "remove slow")
		return nil
	}})
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"slow"}, o); err != nil {
		t.Fatal(err)
	}
	want := string(Record{Plugins: []string{"base", "top"}, Pending: []string{"iso"}, NitroCompute: NitroNodeGroup}.Manifest())
	if len(f.applies) != 1 || f.applies[0] != want {
		t.Errorf("writes:\n%s\nwant\n%s", strings.Join(f.applies, "---\n"), want)
	}
	// A failed read there stops the removal before it writes.
	f = &fakeCluster{record: "base,slow", pending: "iso", nitro: NitroNodeGroup}
	o = nitroFixture(t, f, nil, nil)
	denied := errors.New("denied")
	Register(Plugin{ID: "slow", Order: 6, Remove: func(context.Context, io.Writer, Options) error { f.readErr = denied; return nil }})
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"slow"}, o); !errors.Is(err, denied) || !strings.Contains(err.Error(), "plugin slow was removed, but the record still lists it") || len(f.applies) != 0 {
		t.Errorf("failed read after Remove: %v, wrote %q", err, f.applies)
	}
}

// A plain record (no pending entry, no NITRO_COMPUTE) and no plugin that
// follows NITRO_COMPUTE: the removal reads the record once and writes from
// it, exactly as before this change.
func TestRunRemovePlainRecordReadsOnce(t *testing.T) {
	f := &fakeCluster{record: "base,slow"}
	o := nitroFixture(t, f, nil, nil)
	Register(Plugin{ID: "slow", Order: 6, Remove: func(context.Context, io.Writer, Options) error {
		f.record = "base,slow,top"
		return nil
	}})
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"slow"}, o); err != nil {
		t.Fatal(err)
	}
	want := string(Record{Plugins: []string{"base"}}.Manifest())
	if f.reads != 1 || len(f.applies) != 1 || f.applies[0] != want {
		t.Errorf("reads %d, writes:\n%s\nwant\n%s", f.reads, strings.Join(f.applies, "---\n"), want)
	}
}

// A removal of a plugin that does not follow NITRO_COMPUTE leaves the
// pending entry and the NITRO_COMPUTE of the one that does, byte for byte;
// a removal of every plugin then removes it with the recorded value.
func TestRunRemoveOfAnotherPluginKeepsTheNitroKeys(t *testing.T) {
	for _, start := range []Record{
		{Plugins: []string{"solo"}, Pending: []string{"iso"}, NitroCompute: NitroNodeGroup},
		{Plugins: []string{"iso", "solo"}, NitroCompute: NitroNodeGroup},
	} {
		f := &fakeCluster{record: FormatRecord(start.Plugins), pending: FormatRecord(start.Pending), nitro: start.NitroCompute}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = NitroAutoMode
		if err := Run(context.Background(), io.Discard, ActionRemove, []string{"solo"}, o); err != nil {
			t.Fatal(err)
		}
		left := start
		left.Plugins = without(start.Plugins, "solo")
		if want := string(left.Manifest()); len(f.applies) != 1 || f.applies[0] != want {
			t.Errorf("%+v: writes:\n%s\nwant\n%s", start, strings.Join(f.applies, "---\n"), want)
		}
		f.log = nil
		if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err != nil {
			t.Fatal(err)
		}
		if want := []string{"setup-aws iso --delete --plan", "remove iso NITRO_COMPUTE=node-group", "setup-aws iso --delete", "record deleted"}; !slices.Equal(f.log, want) {
			t.Errorf("%+v: log = %q\nwant  %q", start, f.log, want)
		}
	}
}

// ReadRecord, which the control plane deploy and ReadRelease read, is the
// installed plugins only: a pending plugin's needs and release are not
// carried, and the control plane deploy does not move it to the installed
// ones unless it is chosen again.
func TestReadRecordIsTheInstalledPluginsOnly(t *testing.T) {
	f := &fakeCluster{record: "base", pending: "fork", nitro: NitroNodeGroup}
	o := releaseFixture(t, f)
	if ids, err := ReadRecord(context.Background(), io.Discard, o); err != nil || !slices.Equal(ids, []string{"base"}) {
		t.Errorf("ReadRecord = %q, %v", ids, err)
	}
	if r, err := ReadRelease(context.Background(), io.Discard, o, nil); err != nil || r != upstream.Locate(o.TreeRoot, o.OverlayDigest, o.Region) {
		t.Errorf("ReadRelease with a pending fork = %+v, %v", r, err)
	}
}

// Verify checks a plugin that follows NITRO_COMPUTE with the recorded one,
// both ways, and says so only when it differs from the run's; a record
// with none checks with the run's, as before. Without such a plugin the
// record is not read.
func TestRunVerifyUsesTheRecordedNitroCompute(t *testing.T) {
	for _, c := range []struct {
		recorded, run, with string
		says                bool
	}{
		{NitroNodeGroup, NitroAutoMode, NitroNodeGroup, true},
		{NitroAutoMode, NitroNodeGroup, NitroAutoMode, true},
		{NitroNodeGroup, "", NitroNodeGroup, true},
		{NitroNodeGroup, NitroNodeGroup, NitroNodeGroup, false},
		{"", NitroNodeGroup, NitroNodeGroup, false},
		{"", "", "", false},
	} {
		f := &fakeCluster{record: "iso,solo", nitro: c.recorded}
		o := nitroFixture(t, f, nil, nil)
		o.NitroCompute = c.run
		var out bytes.Buffer
		if err := Run(context.Background(), &out, ActionVerify, []string{"iso"}, o); err != nil {
			t.Fatal(err)
		}
		if want := []string{"verify iso NITRO_COMPUTE=" + c.with}; !slices.Equal(f.log, want) || f.reads != 1 || len(f.applies) != 0 {
			t.Errorf("%+v: log = %q, %d reads, want %q", c, f.log, f.reads, want)
		}
		line := RecordedNitroComputeLine + "iso was deployed with NITRO_COMPUTE=" + c.recorded + ", as kube-system/substrate-eks-plugins records, so it is checked with " + c.recorded
		if says := strings.Contains(out.String(), line); says != c.says {
			t.Errorf("%+v: says %v:\n%s", c, says, out.String())
		}
		f.reads = 0
		if err := Run(context.Background(), io.Discard, ActionVerify, []string{"solo"}, o); err != nil || f.reads != 0 {
			t.Errorf("%+v: solo: %v, %d reads", c, err, f.reads)
		}
	}
}
