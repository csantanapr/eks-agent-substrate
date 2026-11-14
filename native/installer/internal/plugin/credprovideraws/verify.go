package credprovideraws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Verify proves egress credential injection end to end without printing the
// secret (values are compared by SHA-256):
//  1. an actor's HTTPS request with a placeholder header reaches a
//     header-echo endpoint with the Secrets Manager value in that header;
//  2. the actor holds only the placeholder: the value is in neither its
//     environment, its template nor its EgressPolicy;
//  3. a secret without the ate.dev/injectable tag is refused (403): IAM
//     denies the provider's read;
//  4. an atespace the provider policy does not list is refused (403);
//  5. the provider logs the actor and the secret name, never the value.
//
// Each run makes fresh probe actors and deletes them when it passes. The echo
// endpoint sends the received headers back, so in this check the actor does
// see the value in that one response; an ordinary API never echoes a
// credential.
func Verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	ac, account, _, err := caller(ctx, o)
	if err != nil {
		return err
	}
	inj, unt := secretNames(o.Cluster)
	value, err := readSecret(ctx, ac, inj)
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, io.Discard, o, nil, nil, "-n", ns, "rollout", "status", deploy, "--timeout=2m"); err != nil {
		return err
	}
	if err := gatewayServes(ctx, o); err != nil {
		return err
	}
	k, err := kubectlAte(ctx, w, o)
	if err != nil {
		return err
	}
	echo := echoURL()
	u, err := url.Parse(echo)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("CREDPROVIDER_ECHO_URL %q is not a URL", echo)
	}
	c := check{k: k, o: o, w: w, value: value, echo: echo}
	defer c.stopAll()
	return c.run(ctx, uri(account, o.Region, inj), uri(account, o.Region, unt), u.Hostname())
}

// The actors and the placeholder Verify sends.
const (
	allowedActor = "probe-allowed"
	deniedActor  = "probe-denied"
	placeholder  = "Bearer placeholder"
)

// echoURL is a public HTTPS endpoint that echoes the request headers it got.
func echoURL() string {
	if v := os.Getenv("CREDPROVIDER_ECHO_URL"); v != "" {
		return v
	}
	return "https://postman-echo.com/headers"
}

// uri is the credential URI an EgressPolicy names a secret by.
func uri(account, region, secret string) string {
	return fmt.Sprintf("ate-secret://%s/accounts/%s/regions/%s/secrets/%s/stages/AWSCURRENT", ProviderName, account, region, secret)
}

// readSecret is the injectable secret's value, kept in memory only. Replaced
// in tests.
var readSecret = func(ctx context.Context, ac aws.Config, name string) (string, error) {
	out, err := secretsmanager.NewFromConfig(ac).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("read %s (run --plugin-action deploy --plugins %s): %w", name, ID, err)
	}
	v := strings.TrimRight(aws.ToString(out.SecretString), "\n")
	if v == "" {
		return "", fmt.Errorf("%s is empty", name)
	}
	return v, nil
}

// httpsRule is an EgressPolicy with one https rule for host that replaces
// each header with the credential its URI names.
func httpsRule(host string, headers ...[2]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rules:\n- https:\n    hostnames: [%q]\n    effects:\n      replaceHeaders:\n", host)
	for _, h := range headers {
		fmt.Fprintf(&b, "      - header: %s\n", h[0])
		if h[0] == "Authorization" {
			b.WriteString("        prefix: \"Bearer \"\n")
		}
		fmt.Fprintf(&b, "        credentialUri: %s\n", h[1])
	}
	return b.String()
}

// fetchResult is the probe's /fetch answer.
type fetchResult struct {
	Status string `json:"status"`
	Body   string `json:"body"`
	Error  string `json:"error"`
}

// echoed is the value the echo endpoint received in header (names compared
// case-insensitively), from its JSON {"headers": {...}} body.
func echoed(body, header string) (string, error) {
	var e struct {
		Headers map[string]any `json:"headers"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return "", errors.New("the echo endpoint did not answer with JSON headers")
	}
	for k, v := range e.Headers {
		if strings.EqualFold(k, header) {
			s, _ := v.(string)
			return s, nil
		}
	}
	return "", nil
}

// Seams replaced in tests: a free localhost port, an HTTP GET of the probe
// through its proxy, and the pace of retries.
var (
	freePort = func() (int, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port, nil
	}
	probeGet = func(ctx context.Context, u string, timeout time.Duration) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("%s", resp.Status)
		}
		return b, err
	}
)

// check is one Verify run: the proxies it started and the value it hunts.
type check struct {
	k     ate
	o     plugin.Options
	w     io.Writer
	value string
	echo  string
	stops []func()
}

func (c *check) stopAll() {
	for _, s := range c.stops {
		s()
	}
}

// holds reports whether s contains the secret value.
func (c *check) holds(s string) bool { return strings.Contains(s, c.value) }

func (c *check) run(ctx context.Context, injURI, untURI, host string) error {
	inj, unt := secretNames(c.o.Cluster)
	say := func(f string, a ...any) { fmt.Fprintf(c.w, "  "+f+"\n", a...) }

	say("1. the upstream receives the Secrets Manager value")
	allowed, err := c.freshActor(ctx, atespace, allowedActor,
		httpsRule(host, [2]string{"Authorization", injURI}, [2]string{"X-Untagged-Token", untURI}))
	if err != nil {
		return err
	}
	r, err := c.fetch(ctx, allowed, "Authorization:"+placeholder)
	if err != nil {
		return err
	}
	if r.Status != "200" {
		return fmt.Errorf("the injected request got status %q (%s)", r.Status, r.Error)
	}
	got, err := echoed(r.Body, "Authorization")
	if err != nil {
		return err
	}
	switch {
	case got == "":
		return errors.New("the echo endpoint received no Authorization header")
	case got == placeholder:
		return errors.New("the echo endpoint received the placeholder: nothing was injected")
	case !strings.HasPrefix(got, "Bearer "):
		return errors.New("the echoed Authorization header lacks the Bearer prefix")
	}
	gotSHA, wantSHA := sha(strings.TrimPrefix(got, "Bearer ")), sha(c.value)
	if gotSHA != wantSHA {
		return fmt.Errorf("the echoed value (sha256 %s) is not %s (sha256 %s)", gotSHA[:12], inj, wantSHA[:12])
	}
	say("sent Authorization: %s; received Bearer <sha256 %s...>, the value of %s: PASS", placeholder, gotSHA[:12], inj)

	say("2. the actor holds only the placeholder")
	b, err := probeGet(ctx, allowed+"readfile?path="+url.QueryEscape("/proc/self/environ"), 30*time.Second)
	if err != nil {
		return fmt.Errorf("read the actor's environment: %w", err)
	}
	var env struct{ Content, Error string }
	if err := json.Unmarshal(b, &env); err != nil || env.Error != "" {
		return fmt.Errorf("cannot read the actor's environment: %s", env.Error)
	}
	if c.holds(env.Content) {
		return errors.New("the actor's environment holds the value")
	}
	tmpl, err := c.k.out(ctx, nil, "get", "actor-template", template, "-a", atespace, "-o", "json")
	if err != nil {
		return err
	}
	if c.holds(string(tmpl)) {
		return errors.New("the template holds the value")
	}
	pol, err := c.k.out(ctx, nil, "get", "egress-policy", allowedActor, "-a", atespace, "-o", "json")
	if err != nil {
		return err
	}
	if c.holds(string(pol)) {
		return errors.New("the EgressPolicy holds the value")
	}
	if !strings.Contains(string(pol), injURI) {
		return fmt.Errorf("the EgressPolicy does not name %s", inj)
	}
	say("environment, template and EgressPolicy: no value; the policy names only the URI: PASS")

	say("3. a secret without the %s tag is refused", injectableTag)
	if r, err = c.fetch(ctx, allowed, "X-Untagged-Token:placeholder"); err != nil {
		return err
	}
	if r.Status != "403" {
		return fmt.Errorf("the untagged secret got status %q, want 403 (%s)", r.Status, r.Error)
	}
	say("%s: 403 (the provider policy allows it; its IAM role does not): PASS", unt)

	say("4. an atespace the provider policy does not list is refused")
	denied, err := c.freshActor(ctx, deniedAtespace, deniedActor, httpsRule(host, [2]string{"Authorization", injURI}))
	if err != nil {
		return err
	}
	if r, err = c.fetch(ctx, denied, "Authorization:"+placeholder); err != nil {
		return err
	}
	if r.Status != "403" {
		return fmt.Errorf("%s got status %q for %s, want 403", deniedAtespace, r.Status, inj)
	}
	say("%s/%s -> %s: 403: PASS", deniedAtespace, deniedActor, inj)

	say("5. the provider logs the actor and the secret, never the value")
	var logs bytes.Buffer
	if err := plugin.Kubectl(ctx, io.Discard, c.o, nil, &logs, "-n", ns, "logs", "-l", "app="+sa, "--tail=-1", "--max-log-requests=10"); err != nil {
		return err
	}
	actorID := "spiffe://substrate-actor.local/actor/" + atespace + "/" + allowedActor
	deniedID := "spiffe://substrate-actor.local/actor/" + deniedAtespace + "/" + deniedActor
	for _, want := range []struct{ msg, actor, secret string }{
		{"resolving credential", actorID, inj},
		{"credential request failed", actorID, unt},
		{"credential request denied: atespace not granted the secret", deniedID, inj},
	} {
		if !logged(logs.String(), want.msg, want.actor, want.secret) {
			return fmt.Errorf("no %q log for %s and %s", want.msg, want.actor, want.secret)
		}
	}
	if c.holds(logs.String()) {
		return errors.New("the provider's logs hold the value")
	}
	var gw bytes.Buffer
	if plugin.Kubectl(ctx, io.Discard, c.o, nil, &gw, "-n", ns, "logs", "-l", "app=atenet-egress", "-c", "ext-proc", "--tail=-1", "--max-log-requests=10") == nil && c.holds(gw.String()) {
		return errors.New("the egress gateway's logs hold the value")
	}
	say("served, IAM-refused and policy-refused requests are logged with actor and secret; no value: PASS")

	c.stopAll()
	c.stops = nil
	for _, a := range [][2]string{{atespace, allowedActor}, {deniedAtespace, deniedActor}} {
		if _, err := c.k.out(ctx, nil, "delete", "actor", a[1], "-a", a[0]); err != nil {
			return err
		}
	}
	say("PASS: injected from Secrets Manager; untagged secret and unlisted atespace refused; no value in logs")
	return nil
}

// logged reports a provider JSON log line with msg for actor whose secret
// field names secret (as ":secret:<name> ").
func logged(logs, msg, actor, secret string) bool {
	for _, l := range strings.Split(logs, "\n") {
		var e struct {
			Msg    string `json:"msg"`
			Actor  string `json:"actor"`
			Secret string `json:"secret"`
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Msg == msg && e.Actor == actor && strings.Contains(e.Secret+" ", ":secret:"+secret+" ") {
			return true
		}
	}
	return false
}

// freshActor makes a new actor from the probe template with this
// EgressPolicy, reached through `kubectl ate proxy`; it returns the proxy's
// base URL once the actor answers.
func (c *check) freshActor(ctx context.Context, space, name, policy string) (string, error) {
	if _, err := c.k.out(ctx, nil, "get", "actor", name, "-a", space); err == nil {
		if _, err := c.k.out(ctx, nil, "delete", "actor", name, "-a", space); err != nil {
			return "", err
		}
	}
	if _, err := c.k.out(ctx, nil, "create", "actor", name, "-a", space, "--template", template); err != nil {
		return "", fmt.Errorf("create actor %s/%s: %w", space, name, err)
	}
	// Created before the first request, so the actor starts with it. `get
	// egress-policy` exits 0 when there is none and prints nothing.
	if b, err := c.k.out(ctx, nil, "get", "egress-policy", name, "-a", space, "-o", "json"); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if _, err := c.k.out(ctx, nil, "delete", "egress-policy", name, "-a", space); err != nil {
			return "", err
		}
	}
	if _, err := c.k.out(ctx, []byte(policy), "create", "egress-policy", name, "-a", space, "-f", "-"); err != nil {
		return "", fmt.Errorf("create egress-policy %s/%s: %w", space, name, err)
	}
	port, err := freePort()
	if err != nil {
		return "", err
	}
	pctx, cancel := context.WithCancel(ctx)
	var log tail
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = plugin.Exec(pctx, &log, c.k.cmd(nil, nil, "proxy", "actor", name, "-a", space, fmt.Sprintf("%d:80", port)))
	}()
	c.stops = append(c.stops, func() { cancel(); <-done })
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	// The first request resumes the actor.
	for i := 0; i < 60; i++ {
		if _, err := probeGet(ctx, base+"healthz", 10*time.Second); err == nil {
			return base, nil
		}
		if err := pause(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s/%s did not answer through kubectl ate proxy: %s", space, name, log.String())
}

// fetch is the probe's GET of the echo endpoint with one header. It retries
// a 503 or a certificate error for up to two minutes: the gateway
// reconnecting to a new provider, or its CA reaching the actor.
func (c *check) fetch(ctx context.Context, base, header string) (fetchResult, error) {
	q := url.Values{"url": {c.echo}, "header": {header}}
	var r fetchResult
	for i := 0; i < 24; i++ {
		b, err := probeGet(ctx, base+"fetch?"+q.Encode(), 30*time.Second)
		r = fetchResult{}
		if err == nil {
			err = json.Unmarshal(b, &r)
		}
		retry := err != nil || r.Status == "503" || strings.Contains(strings.ToLower(r.Error), "certificate") || strings.Contains(r.Error, "x509")
		if !retry {
			return r, nil
		}
		fmt.Fprintf(c.w, "  status %q %s; retrying\n", r.Status, r.Error)
		if err := pause(ctx, 5*time.Second); err != nil {
			return r, err
		}
	}
	return r, nil
}

// tail keeps the last few KB a child wrote, for an error message; the proxy
// writes from another goroutine, hence the lock.
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}
