package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

func list() []Profile {
	return []Profile{
		{"us.anthropic.claude-3-haiku-20240307-v1:0", "ACTIVE"},
		{"us.anthropic.claude-haiku-4-5-20251001-v1:0", "ACTIVE"},
		{"us.anthropic.claude-haiku-5-5", "ACTIVE"},
		{"us.anthropic.claude-sonnet-4-20250514-v1:0", "ACTIVE"},
		{"us.anthropic.claude-sonnet-5-5", "ACTIVE"},
		{"us.anthropic.claude-sonnet-9", "LEGACY"},
		{"us.anthropic.claude-opus-5", "ACTIVE"},
		{"us.anthropic.claude-opus-4-8", "ACTIVE"},
		{"eu.anthropic.claude-opus-9", "ACTIVE"},
	}
}

// Both directions: each family's newest ACTIVE profile with the prefix, and
// an error for a missing default or family. setup-aws's TestNewestProfile
// holds the same cases for the IAM policy's side.
func TestModels(t *testing.T) {
	m, err := Models(list(), DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"claude": "us.anthropic.claude-haiku-5-5", "claude-haiku": "us.anthropic.claude-haiku-5-5",
		"claude-sonnet": "us.anthropic.claude-sonnet-5-5", "claude-opus": "us.anthropic.claude-opus-5"}
	if fmt.Sprint(m) != fmt.Sprint(want) {
		t.Errorf("models = %v\nwant     %v", m, want)
	}
	if _, err := Models(list(), "us.anthropic.claude-nope"); err == nil {
		t.Error("an unlisted default passed")
	}
	if _, err := Models(list(), "eu.anthropic.claude-opus-9"); err == nil {
		t.Error("eu has no haiku, but passed")
	}
	if id, ok := Newest([]Profile{{"us.anthropic.claude-sonnet-4-20250514-v1:0", "ACTIVE"}, {"us.anthropic.claude-sonnet-4-1", "ACTIVE"}},
		"us.anthropic.claude-sonnet-"); !ok || id != "us.anthropic.claude-sonnet-4-1" {
		t.Errorf("4.1 must beat a dated 4.0, got %s", id)
	}
}

func TestRender(t *testing.T) {
	m, _ := Models(list(), DefaultProfile)
	out, err := render("us-west-2", m)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"awsRegion: us-west-2", "model: us.anthropic.claude-opus-5", "type: ClusterIP", "name: llm-gateway-ingress"} {
		if !strings.Contains(out, s) {
			t.Errorf("manifest lacks %q", s)
		}
	}
	for _, bad := range []string{"LoadBalancer", "NodePort", "kind: Ingress"} {
		if strings.Contains(out, bad) {
			t.Errorf("manifest exposes the gateway: %s", bad)
		}
	}
	delete(m, "claude-sonnet")
	if _, err := render("us-west-2", m); err == nil {
		t.Error("a missing model rendered")
	}
}

func pods(envs ...[]string) string {
	var items []string
	for i, e := range envs {
		var vars []string
		for _, n := range e {
			vars = append(vars, fmt.Sprintf(`{"name":%q}`, n))
		}
		items = append(items, fmt.Sprintf(`{"metadata":{"name":"p%d"},"spec":{"containers":[{"env":[%s]}]}}`, i, strings.Join(vars, ",")))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// Both directions: a pod without the credentials variable is named, one
// with it is not, and no pod at all is an error.
func TestLacksPodIdentity(t *testing.T) {
	got, err := lacksPodIdentity([]byte(pods([]string{"RUST_LOG", credentialsEnv}, []string{"RUST_LOG"})))
	if err != nil || len(got) != 1 || got[0] != "p1" {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := lacksPodIdentity([]byte(pods([]string{credentialsEnv}))); err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := lacksPodIdentity([]byte(`{"items":[]}`)); err == nil {
		t.Error("no pods passed")
	}
}

func TestAnswered(t *testing.T) {
	if !answered(`{"id":"m","type":"message","content":[{"type":"text","text":"ready"}]}` + "\n") {
		t.Error("a Messages answer was refused")
	}
	for _, s := range []string{"", `{"type":"error","error":{"message":"AccessDenied"}}`, `{"type":"message","content":[]}`, "upstream connect error"} {
		if answered(s) {
			t.Errorf("accepted %q", s)
		}
	}
}

// fakeKube is Exec for Deploy and Verify: every child is kubectl with the
// installer's kubeconfig, never a shell.
type fakeKube struct {
	argv      []string
	applied   string
	podsJSON  []string // successive `get pods` answers
	smokeFail int      // smoke runs that get no answer
}

func (f *fakeKube) exec(_ context.Context, _ io.Writer, c plugin.Cmd) error {
	a := strings.Join(c.Argv, " ")
	if c.Argv[0] != "kubectl" || c.Argv[1] != "--kubeconfig" || c.Argv[2] != "/kc" {
		return fmt.Errorf("not kubectl with the installer's kubeconfig: %s", a)
	}
	rest := strings.Join(c.Argv[3:], " ")
	f.argv = append(f.argv, rest)
	switch {
	case rest == "apply -f -":
		f.applied = string(c.Stdin)
	case strings.HasPrefix(rest, "-n llm-gateway get pods"):
		io.WriteString(c.Stdout, f.podsJSON[0])
		if len(f.podsJSON) > 1 {
			f.podsJSON = f.podsJSON[1:]
		}
	case strings.HasPrefix(rest, "-n llm-gateway run llm-smoke"):
		if f.smokeFail > 0 {
			f.smokeFail--
			return errors.New("exit status 1")
		}
		io.WriteString(c.Stdout, `{"type":"message","content":[{"type":"text","text":"ready"}]}`)
	}
	return nil
}

func withFake(t *testing.T, f *fakeKube) plugin.Options {
	t.Helper()
	prevExec, prevList, prevPause := plugin.Exec, listProfiles, pause
	t.Cleanup(func() { plugin.Exec, listProfiles, pause = prevExec, prevList, prevPause })
	plugin.Exec = f.exec
	listProfiles = func(context.Context, string, string) ([]Profile, error) { return list(), nil }
	pause = func(context.Context, time.Duration) error { return nil }
	return plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc"}
}

// Deploy applies the rendered gateway, restarts it, and restarts it again
// while a pod lacks Pod Identity credentials.
func TestDeploy(t *testing.T) {
	f := &fakeKube{podsJSON: []string{pods([]string{"RUST_LOG"}), pods([]string{credentialsEnv})}}
	o := withFake(t, f)
	if err := Deploy(context.Background(), io.Discard, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.applied, "model: us.anthropic.claude-sonnet-5-5") {
		t.Errorf("applied:\n%s", f.applied)
	}
	if n := strings.Count(strings.Join(f.argv, "\n"), "rollout restart deploy/llm-gateway"); n != 2 {
		t.Errorf("%d restarts, want 2 (deploy, then for the missing credentials):\n%s", n, strings.Join(f.argv, "\n"))
	}
	f = &fakeKube{podsJSON: []string{pods([]string{"RUST_LOG"})}}
	o = withFake(t, f)
	if err := Deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "still lack") {
		t.Errorf("want the missing credentials error, got %v", err)
	}
}

// Both directions: Verify passes once each model answers (retrying while
// IAM settles) and fails when one never does.
func TestVerify(t *testing.T) {
	f := &fakeKube{smokeFail: 3}
	o := withFake(t, f)
	if err := Verify(context.Background(), io.Discard, o); err != nil {
		t.Fatal(err)
	}
	f = &fakeKube{smokeFail: smokeTries}
	o = withFake(t, f)
	if err := Verify(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "claude:") {
		t.Errorf("want claude to fail, got %v", err)
	}
}

func TestRegistered(t *testing.T) {
	p, ok := plugin.Get(ID)
	if !ok || !p.Default || !p.SetupAWS || p.Deploy == nil || p.Verify == nil || p.Remove == nil {
		t.Errorf("plugin = %+v", p)
	}
}
