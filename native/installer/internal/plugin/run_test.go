package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// fakeCluster is Exec for Run's tests: it keeps the record ConfigMap and
// logs every child and plugin hook in order.
type fakeCluster struct {
	// record, pending and nitro are the data values of the record's
	// plugins, pending and nitro-compute keys; all "" is no ConfigMap.
	record, pending, nitro string
	// extra, when set, is the value of a data key this installer does not
	// know, as a newer installer would write it.
	extra string
	// rv counts the writes of the record (apply, create, replace, as
	// another run's in meanwhile): its resourceVersion moves on with each,
	// as the API server's does (resourceVersion).
	rv  int
	log []string
	// applies is the stdin of every record apply, byte for byte;
	// conditional that of every create and replace that went through.
	applies, conditional []string
	// writes is the kubectl verb of every record write tried, refused ones
	// included: apply, create, replace or delete.
	writes []string
	// meanwhile, when set, runs before each create or replace of the record
	// is judged, with the number of record writes tried so far, that one
	// included (applies and deletes count too): another run's write between
	// this run's read and its write, which bumps rv.
	meanwhile func(try int)
	// refuse makes `setup-aws plugin <id> --delete`, with or without
	// --plan, fail as a refusal does.
	refuse bool
	// readErr, when set, fails every read of the record.
	readErr error
	// reads counts the reads of the record.
	reads int
}

var (
	recordLine  = regexp.MustCompile(`(?m)^  (plugins|pending|nitro-compute): "([^"]*)"$`)
	versionLine = regexp.MustCompile(`(?m)^  resourceVersion: "([^"]*)"$`)
)

// exists reports whether the record ConfigMap is there.
func (f *fakeCluster) exists() bool {
	return f.record != "" || f.pending != "" || f.nitro != "" || f.extra != ""
}

// resourceVersion is the record's metadata.resourceVersion: 1 for the one a
// test starts with, one more for each write since.
func (f *fakeCluster) resourceVersion() string { return strconv.Itoa(f.rv + 1) }

// recordJSON is the record as `kubectl get configmap -o json` prints it, or
// nothing when there is none: only the keys that hold something, besides
// plugins, as WriteRecord writes them, and its resourceVersion.
func (f *fakeCluster) recordJSON() string {
	if !f.exists() {
		return ""
	}
	data := map[string]string{RecordKey: f.record}
	for k, v := range map[string]string{PendingKey: f.pending, NitroComputeKey: f.nitro, "future-key": f.extra} {
		if v != "" {
			data[k] = v
		}
	}
	b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": RecordName, "namespace": RecordNamespace, "resourceVersion": f.resourceVersion()}, "data": data})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// state is the record as the log shows it: the plugins, then the other keys
// that hold something.
func (f *fakeCluster) state() string {
	s := "record=" + f.record
	if f.pending != "" {
		s += " pending=" + f.pending
	}
	if f.nitro != "" {
		s += " nitro-compute=" + f.nitro
	}
	return s
}

// set makes the record what manifest m holds, as a write of it does: the
// keys this installer writes that m lacks go.
func (f *fakeCluster) set(m []byte) error {
	ms := recordLine.FindAllSubmatch(m, -1)
	if !bytes.Contains(m, []byte("  name: substrate-eks-plugins\n")) || len(ms) == 0 || string(ms[0][1]) != RecordKey {
		return fmt.Errorf("a write of something else:\n%s", m)
	}
	f.record, f.pending, f.nitro = "", "", ""
	for _, m := range ms {
		switch string(m[1]) {
		case RecordKey:
			f.record = string(m[2])
		case PendingKey:
			f.pending = string(m[2])
		case NitroComputeKey:
			f.nitro = string(m[2])
		}
	}
	f.rv++
	return nil
}

// conditionalWrite is a create or replace of the record, judged as the API
// server does: a create of one that exists fails with AlreadyExists, a
// replace with Conflict unless it carries the resourceVersion the record is
// at. kubectl says each on stderr, which reaches w, and exits 1.
func (f *fakeCluster) conditionalWrite(w io.Writer, verb string, m []byte) error {
	f.writes = append(f.writes, verb)
	if f.meanwhile != nil {
		f.meanwhile(len(f.writes))
	}
	v := versionLine.FindSubmatch(m)
	switch {
	case verb == "create" && v != nil:
		return fmt.Errorf("a create with a resourceVersion:\n%s", m)
	case verb == "replace" && v == nil:
		// kubectl would fill in the one on the cluster: no precondition.
		return fmt.Errorf("a replace with no resourceVersion:\n%s", m)
	case verb == "create" && f.exists():
		io.WriteString(w, `Error from server (AlreadyExists): error when creating "STDIN": configmaps "substrate-eks-plugins" already exists`+"\n")
		f.log = append(f.log, "create refused: AlreadyExists")
		return errors.New("exit status 1")
	case verb == "replace" && !f.exists():
		io.WriteString(w, `Error from server (NotFound): error when replacing "STDIN": configmaps "substrate-eks-plugins" not found`+"\n")
		f.log = append(f.log, "replace refused: NotFound")
		return errors.New("exit status 1")
	case verb == "replace" && string(v[1]) != f.resourceVersion():
		io.WriteString(w, `Error from server (Conflict): error when replacing "STDIN": Operation cannot be fulfilled on configmaps "substrate-eks-plugins": `+
			"the object has been modified; please apply your changes to the latest version and try again\n")
		f.log = append(f.log, "replace refused: Conflict")
		return errors.New("exit status 1")
	}
	if err := f.set(m); err != nil {
		return err
	}
	f.conditional = append(f.conditional, string(m))
	f.log = append(f.log, verb+" "+f.state())
	return nil
}

func (f *fakeCluster) exec(_ context.Context, w io.Writer, c Cmd) error {
	argv := strings.Join(c.Argv, " ")
	for _, a := range c.Argv {
		if a == "-c" || a == "bash" || a == "sh" {
			return fmt.Errorf("a shell ran: %s", argv)
		}
	}
	switch {
	case argv == "kubectl --kubeconfig /kc -n kube-system get configmap substrate-eks-plugins --ignore-not-found -o json":
		f.reads++
		if f.readErr != nil {
			return f.readErr
		}
		io.WriteString(c.Stdout, f.recordJSON())
	case argv == "kubectl --kubeconfig /kc apply -f -":
		f.writes = append(f.writes, "apply")
		if versionLine.Match(c.Stdin) {
			return fmt.Errorf("an apply with a resourceVersion:\n%s", c.Stdin)
		}
		if err := f.set(c.Stdin); err != nil {
			return err
		}
		f.applies = append(f.applies, string(c.Stdin))
		f.log = append(f.log, f.state())
	case argv == "kubectl --kubeconfig /kc create --save-config -f -":
		return f.conditionalWrite(w, "create", c.Stdin)
	case argv == "kubectl --kubeconfig /kc replace --save-config -f -":
		return f.conditionalWrite(w, "replace", c.Stdin)
	case argv == "kubectl --kubeconfig /kc -n kube-system delete configmap substrate-eks-plugins --ignore-not-found":
		f.writes = append(f.writes, "delete")
		f.record, f.pending, f.nitro, f.extra = "", "", "", ""
		f.log = append(f.log, "record deleted")
	case strings.HasPrefix(argv, "go -C /setup-aws run . plugin "):
		if !slices.Contains(c.Env, "CLUSTER_NAME=demo") || !slices.Contains(c.Env, "PLAN=false") || !slices.Contains(c.Env, "TAGS=auto-delete=no") {
			return fmt.Errorf("setup-aws env %v", c.Env)
		}
		f.log = append(f.log, "setup-aws "+strings.TrimPrefix(argv, "go -C /setup-aws run . plugin "))
		if f.refuse && strings.Contains(argv, " --delete") {
			return errors.New("exit status 1: teardown stopped before deleting anything it does not own")
		}
	default:
		return fmt.Errorf("unexpected child: %s", argv)
	}
	return nil
}

func hook(f *fakeCluster, what string, err error) func(context.Context, io.Writer, Options) error {
	return func(_ context.Context, _ io.Writer, o Options) error {
		if o.Kubeconfig != "/kc" {
			return errors.New("hook without the kubeconfig")
		}
		f.log = append(f.log, what)
		return err
	}
}

// runFixture registers a base plugin with AWS resources and Kubernetes
// objects, one that requires it and needs an ingress client, and one with
// nothing but objects, with hooks that log to f.
func runFixture(t *testing.T, f *fakeCluster, deployErr error) Options {
	t.Helper()
	withRegistry(t)
	prev := Exec
	t.Cleanup(func() { Exec = prev })
	Exec = f.exec
	Register(Plugin{ID: "base", Order: 1, SetupAWS: true,
		Deploy: hook(f, "deploy base", deployErr), Verify: hook(f, "verify base", nil), Remove: hook(f, "remove base", nil)})
	Register(Plugin{ID: "top", Order: 2, Requires: []string{"base"}, Needs: Needs{IngressIDs: []string{"spiffe://x"}},
		Deploy: hook(f, "deploy top", nil), Verify: hook(f, "verify top", nil), Remove: hook(f, "remove top", nil)})
	Register(Plugin{ID: "solo", Order: 3,
		Deploy: hook(f, "deploy solo", nil), Verify: hook(f, "verify solo", nil), Remove: hook(f, "remove solo", nil)})
	if err := Check(); err != nil {
		t.Fatal(err)
	}
	return Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", SetupAWS: "/setup-aws", Tags: "auto-delete=no"}
}

// Deploy runs each plugin's AWS half, objects and check in install order
// (what it requires first), recording each once it is checked.
func TestRunDeployOrderAndRecord(t *testing.T) {
	f := &fakeCluster{record: "top"} // the control plane carries top's needs
	o := runFixture(t, f, nil)
	var out bytes.Buffer
	if err := Run(context.Background(), &out, ActionDeploy, []string{"top"}, o); err != nil {
		t.Fatal(err)
	}
	want := []string{"setup-aws base", "deploy base", "verify base", "record=base,top", "deploy top", "verify top"}
	if !slices.Equal(f.log, want) {
		t.Errorf("log = %q\nwant  %q", f.log, want)
	}
	for _, l := range []string{DeployLine + "base", VerifyLine + "base", ReadyLine + "base", ReadyLine + "top"} {
		if !strings.Contains(out.String(), l+"\n") {
			t.Errorf("output lacks %q:\n%s", l, out.String())
		}
	}
}

// Both directions: a plugin with needs the control plane does not carry is
// refused before anything runs; a failed Deploy is not recorded.
func TestRunDeployRefusesUncarriedNeedsAndRecordsNoFailure(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	err := Run(context.Background(), io.Discard, ActionDeploy, []string{"top"}, o)
	if err == nil || !strings.Contains(err.Error(), "--deploy-control-plane --plugins top") {
		t.Fatalf("want the needs refusal, got %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("ran after the refusal: %q", f.log)
	}

	f = &fakeCluster{}
	o = runFixture(t, f, errors.New("boom"))
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"base"}, o); err == nil || !strings.Contains(err.Error(), "plugin base: boom") {
		t.Fatalf("want the deploy error, got %v", err)
	}
	if f.record != "" {
		t.Errorf("recorded %q after a failed deploy", f.record)
	}
}

// Remove with no IDs removes every recorded plugin, a plugin before what it
// requires, Kubernetes objects before AWS resources, and deletes the record
// once it is empty.
func TestRunRemoveAll(t *testing.T) {
	f := &fakeCluster{record: "base,solo,top"}
	o := runFixture(t, f, nil)
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err != nil {
		t.Fatal(err)
	}
	want := []string{"setup-aws base --delete --plan", "remove solo", "record=base,top", "remove top", "record=base", "remove base", "setup-aws base --delete", "record deleted"}
	if !slices.Equal(f.log, want) {
		t.Errorf("log = %q\nwant  %q", f.log, want)
	}
}

// Review R5, both ways: AWS refusals are found before anything is removed.
// A refusal from `--delete --plan` stops the removal with every plugin's
// Kubernetes objects and the record left as they were; with none, the
// removal runs as TestRunRemoveAll shows.
func TestRunRemoveStopsOnAnAWSRefusalWithNothingRemoved(t *testing.T) {
	f := &fakeCluster{record: "base,solo,top", refuse: true}
	o := runFixture(t, f, nil)
	var out bytes.Buffer
	err := Run(context.Background(), &out, ActionRemove, nil, o)
	if err == nil || !strings.Contains(err.Error(), "plugin base: its AWS resources cannot be deleted, so nothing was removed") ||
		!strings.Contains(err.Error(), "does not own") {
		t.Errorf("want the refusal, got %v", err)
	}
	if want := []string{"setup-aws base --delete --plan"}; !slices.Equal(f.log, want) {
		t.Errorf("log = %q\nwant  %q (nothing removed)", f.log, want)
	}
	if f.record != "base,solo,top" {
		t.Errorf("record = %q after a refusal", f.record)
	}
	if !strings.Contains(out.String(), PlanLine+"base\n") || strings.Contains(out.String(), RemoveLine) {
		t.Errorf("output:\n%s", out.String())
	}

	f = &fakeCluster{record: "base,solo,top"}
	o = runFixture(t, f, nil)
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err != nil {
		t.Fatal(err)
	}
	if f.record != "" || !slices.Contains(f.log, "remove base") || !slices.Contains(f.log, "setup-aws base --delete") {
		t.Errorf("record %q, log %q", f.record, f.log)
	}
}

// Both ways: removing a plugin with Needs says the control plane still
// carries them and how to drop them; removing one without says nothing.
func TestRunRemoveSaysNeedsAreKept(t *testing.T) {
	f := &fakeCluster{record: "base,top"}
	o := runFixture(t, f, nil)
	var out bytes.Buffer
	if err := Run(context.Background(), &out, ActionRemove, []string{"top"}, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), NeedsKeptLine+"top;") || !strings.Contains(out.String(), "--deploy-control-plane") {
		t.Errorf("no needs line for top:\n%s", out.String())
	}
	out.Reset()
	if err := Run(context.Background(), &out, ActionRemove, []string{"base"}, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), NeedsKeptLine) {
		t.Errorf("needs line for base, which has none:\n%s", out.String())
	}
}

// Both directions: removing what a staying plugin requires is refused before
// anything is removed; removing both is allowed.
func TestRunRemoveKeepsRequirements(t *testing.T) {
	f := &fakeCluster{record: "base,top"}
	o := runFixture(t, f, nil)
	err := Run(context.Background(), io.Discard, ActionRemove, []string{"base"}, o)
	if err == nil || !strings.Contains(err.Error(), "top requires base") {
		t.Fatalf("want the requirement refusal, got %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("removed after the refusal: %q", f.log)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"base", "top"}, o); err != nil {
		t.Fatal(err)
	}
	if f.record != "" {
		t.Errorf("record = %q after removing both", f.record)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	if err := Run(context.Background(), io.Discard, "upgrade", nil, o); err == nil {
		t.Error("unknown action accepted")
	}
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"nope"}, o); err == nil {
		t.Error("unknown plugin accepted")
	}
	f.record = "nope"
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err == nil || !strings.Contains(err.Error(), "cannot remove it") {
		t.Errorf("unknown recorded plugin: %v", err)
	}
	o.Kubeconfig = ""
	if err := Run(context.Background(), io.Discard, ActionRemove, nil, o); err == nil {
		t.Error("ran without a kubeconfig")
	}
}

// Check catches a requirement whose Order is not lower, and an unknown one.
func TestCheck(t *testing.T) {
	withRegistry(t)
	Register(Plugin{ID: "a", Order: 2})
	Register(Plugin{ID: "b", Order: 1, Requires: []string{"a"}})
	if err := Check(); err == nil {
		t.Error("b (order 1) requiring a (order 2) passed")
	}
	withRegistry(t)
	Register(Plugin{ID: "a", Order: 1})
	Register(Plugin{ID: "b", Order: 2, Requires: []string{"a"}})
	if err := Check(); err != nil {
		t.Errorf("valid registry: %v", err)
	}
	Register(Plugin{ID: "c", Order: 3, Requires: []string{"zz"}})
	if err := Check(); err == nil {
		t.Error("unknown requirement passed")
	}
}

func TestOwnerTags(t *testing.T) {
	o := Options{Cluster: "demo", Region: "us-west-2", Tags: "project=x auto-delete=no"}
	m, err := o.OwnerTags()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || m["auto-delete"] != "no" || m["project"] != "x" || m[awsx.OwnerTagKey] != "demo.us-west-2" {
		t.Errorf("tags = %v", m)
	}
	o.Tags = "bad"
	if _, err := o.OwnerTags(); err == nil {
		t.Error("a tag without = passed")
	}
}

// Both ways: a plugin that needs EKS Auto Mode is refused, before anything
// runs, on a cluster without it, and deployed on one with it.
func TestRunDeployNeedsAutoMode(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "iso", Order: 4, NeedsAutoMode: "needs EKS Auto Mode",
		Deploy: hook(f, "deploy iso", nil), Verify: hook(f, "verify iso", nil)})
	err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o)
	if err == nil || !strings.Contains(err.Error(), "Auto Mode off") {
		t.Fatalf("want the Auto Mode refusal, got %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("ran after the refusal: %q", f.log)
	}
	// Verify is gated the same way, so a plugin's own Verify never runs on a
	// cluster it cannot work on.
	err = Run(context.Background(), io.Discard, ActionVerify, []string{"iso"}, o)
	if err == nil || !strings.Contains(err.Error(), "Auto Mode off") {
		t.Fatalf("verify: want the Auto Mode refusal, got %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("verify ran after the refusal: %q", f.log)
	}
	o.AutoMode = true
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"iso"}, o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "deploy iso") {
		t.Errorf("not deployed on Auto Mode: %q", f.log)
	}
	if err := Run(context.Background(), io.Discard, ActionVerify, []string{"iso"}, o); err != nil {
		t.Fatalf("verify on Auto Mode: %v", err)
	}
}

// Both ways: a plugin that needs RDS (db-rotation) is refused, deploy and
// verify alike, before any child (its `setup-aws plugin` AWS half among
// them), on a control plane on the bundled PostgreSQL or on a cluster with
// no control plane, and deployed and checked on one with RDS. Removing it
// is never refused.
func TestRunRefusesAPluginThatNeedsRDSOnTheBundledDatabase(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "rot", Order: 4, SetupAWS: true, NeedsRDS: "rotates the RDS logins",
		Deploy: hook(f, "deploy rot", nil), Verify: hook(f, "verify rot", nil), Remove: hook(f, "remove rot", nil)})
	o.BundledDatabase = true
	for _, action := range []string{ActionDeploy, ActionVerify} {
		err := Run(context.Background(), io.Discard, action, []string{"rot"}, o)
		if err == nil || !strings.Contains(err.Error(), "plugin rot rotates the RDS logins; demo's control plane uses the bundled PostgreSQL") {
			t.Errorf("%s: want the RDS refusal, got %v", action, err)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("ran after the refusal: %q", f.log)
	}
	if err := Run(context.Background(), io.Discard, ActionRemove, []string{"rot"}, o); err != nil || !slices.Contains(f.log, "remove rot") {
		t.Errorf("remove on the bundled PostgreSQL: %v, %q", err, f.log)
	}
	// No control plane (Running ""), so no database at all: refused the
	// same way, before any child (review of 1ca284d, NIT-5).
	f.log = nil
	o.BundledDatabase = false
	for _, action := range []string{ActionDeploy, ActionVerify} {
		err := Run(context.Background(), io.Discard, action, []string{"rot"}, o)
		if err == nil || !strings.Contains(err.Error(), "plugin rot rotates the RDS logins; no control plane is installed on demo") ||
			!strings.Contains(err.Error(), "run --deploy-control-plane --database rds first") {
			t.Errorf("%s with no control plane: want the refusal, got %v", action, err)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("ran with no control plane: %q", f.log)
	}
	o.Running = "v1.0.0"
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"rot"}, o); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), io.Discard, ActionVerify, []string{"rot"}, o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "setup-aws rot") || !slices.Contains(f.log, "deploy rot") || !slices.Contains(f.log, "verify rot") {
		t.Errorf("not deployed and checked with RDS: %q", f.log)
	}
}

// Both ways: a plugin pinned to a node group the cluster's nodes lack is
// refused, deploy and verify alike, before anything runs, naming the group;
// with the group's nodes (Options.Pools) it deploys and checks. A plugin it
// requires counts too: "needy" requires "pinned".
func TestRunRefusesAPluginWhoseNodesAreMissing(t *testing.T) {
	f := &fakeCluster{}
	o := runFixture(t, f, nil)
	Register(Plugin{ID: "pinned", Order: 4, Nodes: []string{"system", "gvisor"},
		Deploy: hook(f, "deploy pinned", nil), Verify: hook(f, "verify pinned", nil)})
	Register(Plugin{ID: "needy", Order: 5, Requires: []string{"pinned"},
		Deploy: hook(f, "deploy needy", nil), Verify: hook(f, "verify needy", nil)})
	o.Pools = []string{"system", "isolated"}
	for _, action := range []string{ActionDeploy, ActionVerify} {
		err := Run(context.Background(), io.Discard, action, []string{"needy"}, o)
		if err == nil || !strings.Contains(err.Error(), "plugin pinned runs on the gvisor node group") || !strings.Contains(err.Error(), "ate.dev/pool=gvisor") {
			t.Errorf("%s: want the gvisor refusal, got %v", action, err)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("ran after the refusal: %q", f.log)
	}
	o.Pools = []string{"gvisor", "system"}
	if err := Run(context.Background(), io.Discard, ActionDeploy, []string{"needy"}, o); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), io.Discard, ActionVerify, []string{"needy"}, o); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.log, "deploy pinned") || !slices.Contains(f.log, "verify needy") {
		t.Errorf("not deployed and checked with the nodes: %q", f.log)
	}
}

func TestParsePoolsAndForClass(t *testing.T) {
	if got := ParsePools("system gvisor system  microvm\n"); !slices.Equal(got, []string{"gvisor", "microvm", "system"}) {
		t.Errorf("ParsePools = %v", got)
	}
	if got := ParsePools(""); len(got) != 0 {
		t.Errorf("ParsePools of nothing = %v", got)
	}
	withRegistry(t)
	Register(Plugin{ID: "c1", Order: 2, Class: "nitro"})
	Register(Plugin{ID: "c0", Order: 1})
	if got := ForClass("nitro"); len(got) != 1 || got[0].ID != "c1" {
		t.Errorf("ForClass(nitro) = %v", got)
	}
	if got := ForClass(""); len(got) != 0 {
		t.Errorf("ForClass of no class = %v", got)
	}
}

// Both ways: SetupAWS runs a release's setup-aws binary itself, by its
// absolute path, and `go -C <dir> run .` on the source directory, with the
// same arguments and environment.
func TestSetupAWSRunsTheBinaryOrGoRun(t *testing.T) {
	var got []Cmd
	prev := Exec
	t.Cleanup(func() { Exec = prev })
	Exec = func(_ context.Context, _ io.Writer, c Cmd) error {
		got = append(got, c)
		return nil
	}
	dir, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is /private/var, as os.Getwd sees it
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "setup-aws")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "tools", "setup-aws")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, tc := range []struct {
		path string
		want []string
	}{
		{bin, []string{bin, "plugin", "efs", "--delete"}},
		{"setup-aws", []string{bin, "plugin", "efs", "--delete"}}, // relative: run by its absolute path, never looked up on PATH
		{src, []string{"go", "-C", src, "run", ".", "plugin", "efs", "--delete"}},
	} {
		got = nil
		o := Options{Cluster: "demo", Region: "us-west-2", Profile: "p", SetupAWS: tc.path, Tags: "auto-delete=no"}
		if err := SetupAWS(context.Background(), io.Discard, o, "plugin", "efs", "--delete"); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !slices.Equal(got[0].Argv, tc.want) {
			t.Errorf("%s: ran %q, want %q", tc.path, got, tc.want)
			continue
		}
		wantEnv := []string{"CLUSTER_NAME=demo", "AWS_REGION=us-west-2", "AWS_PROFILE=p", "PLAN=false", "KEEP_CLUSTER=false", "TAGS=auto-delete=no"}
		if !slices.Equal(got[0].Env, wantEnv) {
			t.Errorf("%s: env %q, want %q", tc.path, got[0].Env, wantEnv)
		}
	}
}
