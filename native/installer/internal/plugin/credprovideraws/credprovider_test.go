package credprovideraws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"sigs.k8s.io/kustomize/kyaml/kio"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

func TestRegistered(t *testing.T) {
	p, ok := plugin.Get(ID)
	if !ok || !p.SetupAWS || p.Default || p.Deploy == nil || p.Verify == nil || p.Remove == nil {
		t.Fatalf("plugin = %+v", p)
	}
	want := `{"name":"secretsmanager.amazonaws.com","address":"aws-sm-credential-provider.ate-system.svc:50051"}`
	if len(p.Needs.CredentialProviders) != 1 || p.Needs.CredentialProviders[0] != want {
		t.Errorf("needs = %q", p.Needs.CredentialProviders)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(p.Needs.CredentialProviders[0]), &v); err != nil || len(v) != 2 {
		t.Errorf("not a JSON object: %v", err)
	}
}

// providerBase is the provider's kustomize output, trimmed.
const providerBase = `apiVersion: v1
kind: Service
metadata:
  name: aws-sm-credential-provider
  namespace: ate-system
spec:
  type: ClusterIP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: aws-sm-credential-provider
  namespace: ate-system
spec:
  template:
    metadata:
      labels:
        app: aws-sm-credential-provider
    spec:
      containers:
      - name: aws-sm-credential-provider
        image: ko://plugins.go.ate.dev/credprovider-aws-secret-manager/cmd/aws-sm-credential-provider
        args:
        - "--listen-address=:50051"
        - "--region=REPLACE_WITH_AWS_REGION"
        - "--expected-role-arn=arn:REPLACE_WITH_AWS_PARTITION:iam::REPLACE_WITH_AWS_ACCOUNT_ID:role/aws-sm-credential-provider"
        - "--log-level=info"
`

// Both directions: the edits deploy.sh made, and a refusal for a base that
// is not the provider's (no ko:// image, no --region, two containers).
func TestProviderManifest(t *testing.T) {
	role := "arn:aws:iam::111122223333:role/demo-us-west-2-aws-sm-credential-provider"
	out, err := providerManifest([]byte(providerBase), "reg/demo/aws-sm-credential-provider:x@sha256:1", "us-west-2", role, "abc")
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kio.FromBytes(out)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("%d documents, %v:\n%s", len(nodes), err, out)
	}
	s := string(out)
	for _, want := range []string{"image: reg/demo/aws-sm-credential-provider:x@sha256:1", "--region=us-west-2", "--expected-role-arn=" + role,
		"--listen-address=:50051", "ate.dev/pool: system", "value: ate-system-infra", policySHA + ": abc", "kind: Service"} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "REPLACE_WITH") || strings.Count(s, "--expected-role-arn=") != 1 {
		t.Errorf("a placeholder arg is left:\n%s", s)
	}
	for name, bad := range map[string]string{
		"no ko image":    strings.Replace(providerBase, "ko://", "docker.io/", 1),
		"no region":      strings.Replace(providerBase, `- "--region=REPLACE_WITH_AWS_REGION"`, "", 1),
		"two containers": providerBase + "      - name: second\n        image: ko://x\n        args: [\"--region=x\"]\n",
	} {
		if _, err := providerManifest([]byte(bad), "img", "us-west-2", role, "abc"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestServesProvider(t *testing.T) {
	d := func(args ...string) []byte {
		b, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"args": args}}}}}})
		return b
	}
	if ok, err := servesProvider(d("--credential-provider-name=k8s.io", "--credential-provider-name=secretsmanager.amazonaws.com")); err != nil || !ok {
		t.Errorf("serving gateway refused: %v %v", ok, err)
	}
	if ok, _ := servesProvider(d("--credential-provider-name=k8s.io", "--credential-provider-name=secretsmanager.amazonaws.com.evil")); ok {
		t.Error("a gateway with only k8s.io passed")
	}
}

func TestRenderPolicy(t *testing.T) {
	p, err := renderPolicy("111122223333", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`account: "111122223333"`, "region: us-west-2", "namePrefix: ate/ate-demo-credprovider/", "- atespace: ate-demo-credprovider\n"} {
		if !strings.Contains(p, s) {
			t.Errorf("policy lacks %q:\n%s", s, p)
		}
	}
	if strings.Contains(p, "- atespace: ate-demo-credprovider-denied") {
		t.Error("the denied atespace is granted")
	}
}

// Both directions for the two base-image readers: a digest-pinned base is
// returned, an unpinned or missing one refused.
func TestBases(t *testing.T) {
	if b, err := dockerfileBase([]byte("ARG GO_IMAGE=golang\nARG BASE_IMAGE=gcr.io/distroless/static:latest@sha256:15f8\nFROM x\n")); err != nil || b != "gcr.io/distroless/static:latest@sha256:15f8" {
		t.Errorf("got %q %v", b, err)
	}
	for _, bad := range []string{"ARG BASE_IMAGE=gcr.io/distroless/static:latest\n", "FROM x\n"} {
		if _, err := dockerfileBase([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	ko := "defaultBaseImage: cgr.dev/static@sha256:aa\nbaseImageOverrides:\n  " + probePkg + ": gcr.io/distroless/static@sha256:bb\n"
	if b, err := koBase([]byte(ko), probePkg); err != nil || b != "gcr.io/distroless/static@sha256:bb" {
		t.Errorf("override: %q %v", b, err)
	}
	if b, err := koBase([]byte(ko), "other/pkg"); err != nil || b != "cgr.dev/static@sha256:aa" {
		t.Errorf("default: %q %v", b, err)
	}
	if _, err := koBase([]byte("defaultBaseImage: cgr.dev/static:latest\n"), probePkg); err == nil {
		t.Error("an unpinned base passed")
	}
}

// The image runs the binary at ko's path as the given user.
func TestWithApp(t *testing.T) {
	img, err := withApp(empty.Image, []byte("bin"), providerBin, providerUser)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	if len(cf.Config.Entrypoint) != 1 || cf.Config.Entrypoint[0] != providerBin || cf.Config.User != providerUser {
		t.Errorf("config = %+v", cf.Config)
	}
	ls, _ := img.Layers()
	if len(ls) != 1 {
		t.Errorf("%d layers", len(ls))
	}
	a, _ := img.Digest()
	img2, _ := withApp(empty.Image, []byte("bin"), providerBin, providerUser)
	if b, _ := img2.Digest(); a != b {
		t.Error("the same binary gave two digests")
	}
}

func TestHTTPSRuleAndEchoed(t *testing.T) {
	r := httpsRule("postman-echo.com", [2]string{"Authorization", "ate-secret://a"}, [2]string{"X-Untagged-Token", "ate-secret://b"})
	if strings.Count(r, "prefix:") != 1 || !strings.Contains(r, `hostnames: ["postman-echo.com"]`) || !strings.Contains(r, "credentialUri: ate-secret://b") {
		t.Errorf("rule:\n%s", r)
	}
	if v, err := echoed(`{"headers":{"authorization":"Bearer x","host":"h"}}`, "Authorization"); err != nil || v != "Bearer x" {
		t.Errorf("got %q %v", v, err)
	}
	if _, err := echoed("<html>", "Authorization"); err == nil {
		t.Error("a non-JSON body passed")
	}
}

// ---- Verify against a fake cluster and probe ---------------------------------

const testValue = "s3cretValueFromSecretsManager0123456789"

// fakeVerify answers kubectl, kubectl-ate and the probe like a working
// install; each field breaks one thing Verify must catch.
type fakeVerify struct {
	mu                         sync.Mutex
	echoValue                  string // what the echo endpoint receives in Authorization
	untaggedStatus, deniedStat string
	envHolds, logsHold         bool
	noDenialLog                bool
	policies                   map[string]string
	deleted                    []string
	ports                      int
}

func (f *fakeVerify) exec(ctx context.Context, _ io.Writer, c plugin.Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := strings.Join(c.Argv, " ")
	for _, x := range c.Argv {
		if x == "bash" || x == "sh" || x == "-c" {
			return fmt.Errorf("a shell ran: %s", a)
		}
	}
	w := c.Stdout
	if w == nil {
		w = io.Discard
	}
	switch {
	case c.Argv[0] == "kubectl":
		if strings.Contains(a, " logs -l app="+sa) {
			for _, l := range []string{
				fmt.Sprintf(`{"msg":"resolving credential","actor":"spiffe://substrate-actor.local/actor/%s/%s","secret":"arn:aws:secretsmanager:us-west-2:1:secret:ate/ate-demo-credprovider/demo-demo-token "}`, atespace, allowedActor),
				fmt.Sprintf(`{"msg":"credential request failed","actor":"spiffe://substrate-actor.local/actor/%s/%s","secret":"arn:aws:secretsmanager:us-west-2:1:secret:ate/ate-demo-credprovider/demo-untagged-token "}`, atespace, allowedActor),
			} {
				fmt.Fprintln(w, l)
			}
			if !f.noDenialLog {
				fmt.Fprintf(w, `{"msg":"credential request denied: atespace not granted the secret","actor":"spiffe://substrate-actor.local/actor/%s/%s","secret":"arn:aws:secretsmanager:us-west-2:1:secret:ate/ate-demo-credprovider/demo-demo-token (AWSCURRENT)"}`+"\n", deniedAtespace, deniedActor)
			}
			if f.logsHold {
				fmt.Fprintln(w, `{"msg":"debug","value":"`+testValue+`"}`)
			}
		}
		if strings.Contains(a, "get deploy atenet-egress") {
			io.WriteString(w, `{"spec":{"template":{"spec":{"containers":[{"args":["--credential-provider-name=secretsmanager.amazonaws.com"]}]}}}}`)
		}
		return nil
	case strings.HasSuffix(c.Argv[0], "kubectl-ate-v1.0.0"):
	case c.Argv[0] == "go":
		return nil
	default:
		return fmt.Errorf("unexpected child %s", a)
	}
	args := c.Argv[1:]
	verb := strings.Join(args[:min(2, len(args))], " ")
	switch verb {
	case "proxy actor":
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return ctx.Err()
	case "get actor":
		return fmt.Errorf("not found")
	case "get egress-policy":
		io.WriteString(w, f.policies[args[2]])
	case "create egress-policy":
		f.policies[args[2]] = string(c.Stdin)
	case "get actor-template":
		io.WriteString(w, `{"containers":[{"image":"probe"}]}`)
	case "delete actor":
		f.deleted = append(f.deleted, args[2])
	}
	return nil
}

func (f *fakeVerify) get(_ context.Context, raw string, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, _ := url.Parse(raw)
	denied := u.Port() == "2"
	switch u.Path {
	case "/healthz":
		return []byte("ok"), nil
	case "/readfile":
		env := "PATH=/bin\x00HOME=/"
		if f.envHolds {
			env += "\x00TOKEN=" + testValue
		}
		return json.Marshal(map[string]string{"content": env})
	case "/fetch":
		h := u.Query().Get("header")
		res := map[string]string{"status": "200"}
		switch {
		case denied:
			res["status"] = f.deniedStat
		case strings.HasPrefix(h, "X-Untagged-Token:"):
			res["status"] = f.untaggedStatus
		default:
			b, _ := json.Marshal(map[string]any{"headers": map[string]string{"authorization": f.echoValue}})
			res["body"] = string(b)
		}
		return json.Marshal(res)
	}
	return nil, fmt.Errorf("unexpected GET %s", raw)
}

func runVerify(t *testing.T, f *fakeVerify) error {
	t.Helper()
	prevExec, prevGet, prevPort, prevPause := plugin.Exec, probeGet, freePort, pause
	t.Cleanup(func() { plugin.Exec, probeGet, freePort, pause = prevExec, prevGet, prevPort, prevPause })
	f.policies = map[string]string{}
	plugin.Exec, probeGet = f.exec, f.get
	freePort = func() (int, error) { f.ports++; return f.ports, nil }
	pause = func(context.Context, time.Duration) error { return nil }
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", Tag: "v1.0.0", TreeRoot: t.TempDir()}
	c := check{k: ate{bin: "/cache/kubectl-ate-v1.0.0", o: o}, o: o, w: io.Discard, value: testValue, echo: "https://postman-echo.com/headers"}
	defer c.stopAll()
	return c.run(context.Background(), uri("111122223333", "us-west-2", "ate/ate-demo-credprovider/demo-demo-token"),
		uri("111122223333", "us-west-2", "ate/ate-demo-credprovider/demo-untagged-token"), "postman-echo.com")
}

func good() *fakeVerify {
	return &fakeVerify{echoValue: "Bearer " + testValue, untaggedStatus: "403", deniedStat: "403"}
}

// Verify passes on a working install, deleting its probe actors, and fails
// on each way injection can be wrong: nothing injected, the wrong value, an
// untagged secret served, the unlisted atespace served, the value in the
// actor's environment or the provider's logs, a missing denial log.
func TestVerifyBothWays(t *testing.T) {
	f := good()
	if err := runVerify(t, f); err != nil {
		t.Fatalf("working install: %v", err)
	}
	if strings.Join(f.deleted, ",") != allowedActor+","+deniedActor {
		t.Errorf("deleted %v", f.deleted)
	}
	if p := f.policies[allowedActor]; !strings.Contains(p, "demo-demo-token/stages/AWSCURRENT") || !strings.Contains(p, "X-Untagged-Token") {
		t.Errorf("allowed policy:\n%s", p)
	}
	for name, c := range map[string]struct {
		break_ func(*fakeVerify)
		want   string
	}{
		"placeholder echoed": {func(f *fakeVerify) { f.echoValue = placeholder }, "nothing was injected"},
		"wrong value":        {func(f *fakeVerify) { f.echoValue = "Bearer other" }, "is not"},
		"untagged served":    {func(f *fakeVerify) { f.untaggedStatus = "200" }, "untagged secret got status"},
		"denied served":      {func(f *fakeVerify) { f.deniedStat = "200" }, "want 403"},
		"value in env":       {func(f *fakeVerify) { f.envHolds = true }, "environment holds"},
		"value in logs":      {func(f *fakeVerify) { f.logsHold = true }, "logs hold"},
		"no denial log":      {func(f *fakeVerify) { f.noDenialLog = true }, "atespace not granted"},
	} {
		f := good()
		c.break_(f)
		err := runVerify(t, f)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), testValue) {
			t.Errorf("%s: the error prints the secret", name)
		}
	}
}
