package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// fakeCluster is Exec for Run's tests: it keeps the record ConfigMap and
// logs every child and plugin hook in order.
type fakeCluster struct {
	record string // the data value; "" is no ConfigMap
	log    []string
}

var recordValue = regexp.MustCompile(`(?m)^  plugins: "([^"]*)"$`)

func (f *fakeCluster) exec(_ context.Context, _ io.Writer, c Cmd) error {
	argv := strings.Join(c.Argv, " ")
	for _, a := range c.Argv {
		if a == "-c" || a == "bash" || a == "sh" {
			return fmt.Errorf("a shell ran: %s", argv)
		}
	}
	switch {
	case strings.HasPrefix(argv, "kubectl --kubeconfig /kc -n kube-system get configmap substrate-eks-plugins"):
		io.WriteString(c.Stdout, f.record)
	case argv == "kubectl --kubeconfig /kc apply -f -":
		m := recordValue.FindSubmatch(c.Stdin)
		if m == nil {
			return fmt.Errorf("apply of something else:\n%s", c.Stdin)
		}
		f.record = string(m[1])
		f.log = append(f.log, "record="+f.record)
	case argv == "kubectl --kubeconfig /kc -n kube-system delete configmap substrate-eks-plugins --ignore-not-found":
		f.record = ""
		f.log = append(f.log, "record deleted")
	case strings.HasPrefix(argv, "go -C /setup-aws run . plugin "):
		if !slices.Contains(c.Env, "CLUSTER_NAME=demo") || !slices.Contains(c.Env, "PLAN=false") || !slices.Contains(c.Env, "TAGS=auto-delete=no") {
			return fmt.Errorf("setup-aws env %v", c.Env)
		}
		f.log = append(f.log, "setup-aws "+strings.TrimPrefix(argv, "go -C /setup-aws run . plugin "))
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
	want := []string{"remove solo", "record=base,top", "remove top", "record=base", "remove base", "setup-aws base --delete", "record deleted"}
	if !slices.Equal(f.log, want) {
		t.Errorf("log = %q\nwant  %q", f.log, want)
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
