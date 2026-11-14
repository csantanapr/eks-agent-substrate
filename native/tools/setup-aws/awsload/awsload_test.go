package awsload

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The caller ARNs of the fake's keys. OLD and NEW are two sessions of one
// role; the others are another account and another role of the same one.
var fakeCallers = map[string]string{
	"AKIDOLD":      "arn:aws:sts::111122223333:assumed-role/Admin/session-1",
	"AKIDNEW":      "arn:aws:sts::111122223333:assumed-role/Admin/session-2",
	"AKIDACCOUNT":  "arn:aws:sts::other-account:assumed-role/Admin/session-3",
	"AKIDREADONLY": "arn:aws:sts::111122223333:assumed-role/ReadOnly/session-4",
}

// fakeSTS answers sts:GetCallerIdentity like STS: a key of fakeCallers
// that has not been expired succeeds, AKIDDENIED is denied, AKIDHALF (a
// session token not written yet) is InvalidClientTokenId, and any other
// key has expired. It records the access key of every request, in order.
type fakeSTS struct {
	mu      sync.Mutex
	expired map[string]bool
	keys    []string
}

var credentialRE = regexp.MustCompile(`Credential=([^/]+)/`)

func (f *fakeSTS) expire(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired[key] = true
}

func (f *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := ""
	if m := credentialRE.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		key = m[1]
	}
	f.mu.Lock()
	f.keys = append(f.keys, key)
	arn, ok := fakeCallers[key]
	ok = ok && !f.expired[key]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	fault := func(status int, code, msg string) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>`+
			code+`</Code><Message>`+msg+`</Message></Error><RequestId>r</RequestId></ErrorResponse>`)
	}
	switch {
	case ok:
		account := strings.Split(arn, ":")[4]
		_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>`+
			`<Arn>`+arn+`</Arn><UserId>AROAEXAMPLE:session</UserId><Account>`+account+`</Account>`+
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
	case key == "AKIDDENIED":
		fault(http.StatusForbidden, "AccessDenied", "not allowed")
	case key == "AKIDHALF":
		fault(http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid.")
	default:
		fault(http.StatusBadRequest, "ExpiredToken", "The security token included in the request is expired")
	}
}

func (f *fakeSTS) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

// setup points the SDK at a temporary profile p holding key, and every AWS
// endpoint at a fake STS; nothing reaches AWS. write replaces the profile's
// file the way a credentials refresh does, in one rename; an empty key
// writes the empty file a refresh leaves before it writes the profile.
func setup(t *testing.T, key string) (write func(string), f *fakeSTS) {
	t.Helper()
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	write = func(k string) {
		body := ""
		if k != "" {
			body = "[p]\naws_access_key_id = " + k + "\naws_secret_access_key = s\naws_session_token = t\n"
		}
		tmp := creds + ".new"
		if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.Rename(tmp, creds); err != nil {
			t.Error(err)
		}
	}
	write(key)
	if err := os.WriteFile(filepath.Join(dir, "config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f = &fakeSTS{expired: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	for k, v := range map[string]string{
		"AWS_SHARED_CREDENTIALS_FILE": creds, "AWS_CONFIG_FILE": filepath.Join(dir, "config"),
		"AWS_ENDPOINT_URL": srv.URL, "AWS_ENDPOINT_URL_STS": "", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_RETRY_MODE": "", "AWS_MAX_ATTEMPTS": "",
	} {
		t.Setenv(k, v)
	}
	poll, wait := pollEvery, maxWait
	pollEvery, maxWait = 20*time.Millisecond, 5*time.Second
	t.Cleanup(func() { pollEvery, maxWait = poll, wait })
	// Each test is a new process as far as Config's configs go.
	forget := func() {
		configsMu.Lock()
		defer configsMu.Unlock()
		clear(configs)
	}
	forget()
	t.Cleanup(forget)
	return write, f
}

func caller(ctx context.Context, cfg aws.Config) (string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Arn), nil
}

// count is how many of keys are k.
func count(keys []string, k string) int {
	n := 0
	for _, x := range keys {
		if x == k {
			n++
		}
	}
	return n
}

// BUG5's shape: a step loads the profile, its session ends, and the
// profile is refreshed on disk with a new session of the same role. The
// SDK's own config goes on signing with the expired key; Config's client
// retries with the refreshed one.
func TestACallAfterTheSessionEndsUsesTheRefreshedProfile(t *testing.T) {
	write, f := setup(t, "AKIDOLD")
	ctx := context.Background()
	plain, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-west-2"), awsconfig.WithSharedConfigProfile("p"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Config(ctx, "us-west-2", "p")
	if err != nil {
		t.Fatal(err)
	}
	f.expire("AKIDOLD")
	write("AKIDNEW")

	if _, err := caller(ctx, plain); !isExpired(err) {
		t.Fatalf("the SDK's own config: err = %v, want ExpiredToken (it keeps the key it loaded)", err)
	}
	// Config's pin, then the SDK config's one request.
	if got := f.seen(); !slices.Equal(got, []string{"AKIDOLD", "AKIDOLD"}) {
		t.Fatalf("sent %v, want Config's pin and one request with the old key", got)
	}

	arn, err := caller(ctx, cfg)
	if err != nil || arn != fakeCallers["AKIDNEW"] {
		t.Fatalf("Config's client: caller %q, err %v; want the call to succeed with the refreshed key", arn, err)
	}
	// Its first try with the old key, the check of the refreshed profile,
	// and the retry with the new key.
	if got, want := f.seen()[2:], []string{"AKIDOLD", "AKIDNEW", "AKIDNEW"}; !slices.Equal(got, want) {
		t.Fatalf("Config's client sent %v, want %v", got, want)
	}
}

// While the profile still holds the expired key, then a file being
// rewritten (empty), then a session token that is not valid yet, the call
// waits; then it goes on with the key the refresh writes.
func TestACallWaitsUntilTheProfileIsRefreshed(t *testing.T) {
	write, f := setup(t, "AKIDOLD")
	ctx := context.Background()
	cfg, err := Config(ctx, "us-west-2", "p")
	if err != nil {
		t.Fatal(err)
	}
	f.expire("AKIDOLD")
	const step = 150 * time.Millisecond
	refreshed := make(chan struct{})
	t.Cleanup(func() { <-refreshed }) // the writer never outlives the test
	go func() {
		defer close(refreshed)
		for _, k := range []string{"", "AKIDHALF", "AKIDNEW"} {
			time.Sleep(step)
			write(k)
		}
	}()
	start := time.Now()
	arn, err := caller(ctx, cfg)
	if err != nil || arn != fakeCallers["AKIDNEW"] {
		t.Fatalf("caller %q, err %v; want the call to succeed once the profile is refreshed", arn, err)
	}
	if took := time.Since(start); took < 3*step {
		t.Fatalf("returned after %s, before the refresh at %s", took, 3*step)
	}
	got := f.seen()
	// The pin, the failed call and at least one check that found the old
	// key; checks that found the half-written token; then the check and the
	// retry with the new key.
	if count(got, "AKIDOLD") < 3 || count(got, "AKIDHALF") < 1 || !slices.Equal(got[len(got)-2:], []string{"AKIDNEW", "AKIDNEW"}) {
		t.Fatalf("sent %v; want the old key at least 3 times, the half-written one at least once, ending with the new key twice", got)
	}
}

// Credentials for another account, or another role of the same account,
// stop the call at once: no switch, and no wait for the profile to change
// back.
func TestRefreshedCredentialsForAnotherIdentityAreRefused(t *testing.T) {
	for _, key := range []string{"AKIDACCOUNT", "AKIDREADONLY"} {
		t.Run(key, func(t *testing.T) {
			write, f := setup(t, "AKIDOLD")
			ctx := context.Background()
			cfg, err := Config(ctx, "us-west-2", "p")
			if err != nil {
				t.Fatal(err)
			}
			f.expire("AKIDOLD")
			write(key)
			arn, err := caller(ctx, cfg)
			if err == nil || !strings.Contains(err.Error(), "profile p: the profile now holds credentials for a different identity; refusing to continue") {
				t.Fatalf("caller %q, err %v; want the refusal", arn, err)
			}
			// The pin, the failed call, and one check of the other identity:
			// a wait would have polled again, a switch would have retried.
			if got, want := f.seen(), []string{"AKIDOLD", "AKIDOLD", key}; !slices.Equal(got, want) {
				t.Fatalf("sent %v, want %v", got, want)
			}
		})
	}
}

// One pin per process (review R4): a plugin step that loads its own config
// after the profile was re-vended for another account must not pin that
// account while the clients made before it still sign as the first. A second
// Config call for the region and profile shares the first one's pin, so once
// the first session ends both refuse the other identity; a call for another
// profile is its own load.
func TestConfigCallsShareOnePin(t *testing.T) {
	write, f := setup(t, "AKIDOLD")
	ctx := context.Background()
	first, err := Config(ctx, "us-west-2", "p")
	if err != nil {
		t.Fatal(err)
	}
	write("AKIDACCOUNT") // re-vended for another account mid-run
	second, err := Config(ctx, "us-west-2", "p")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.seen(); !slices.Equal(got, []string{"AKIDOLD"}) {
		t.Fatalf("sent %v; want the first call's pin only (the second call loads nothing)", got)
	}
	// Until the first session ends, both still sign as the pinned identity.
	if arn, err := caller(ctx, second); err != nil || arn != fakeCallers["AKIDOLD"] {
		t.Fatalf("second config: caller %q, err %v; want the pinned identity", arn, err)
	}
	f.expire("AKIDOLD")
	for name, cfg := range map[string]aws.Config{"first": first, "second": second} {
		arn, err := caller(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "refusing to continue") {
			t.Errorf("%s config: caller %q, err %v; want the refusal of the other identity", name, arn, err)
		}
	}
	if _, err := Config(ctx, "us-west-2", ""); err == nil {
		t.Error("the default chain (no profile, no credentials) shared profile p's config")
	}
}

func TestACallGivesUpWhenTheProfileIsNeverRefreshed(t *testing.T) {
	_, f := setup(t, "AKIDOLD")
	maxWait = 150 * time.Millisecond
	cfg, err := Config(context.Background(), "us-west-2", "p")
	if err != nil {
		t.Fatal(err)
	}
	f.expire("AKIDOLD")
	_, err = caller(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "the AWS credentials of profile p expired and were not refreshed within 150ms") ||
		!isExpired(err) {
		t.Fatalf("err = %v; want the give-up error naming the profile and the bound, wrapping ExpiredToken", err)
	}
}

// Another error, a denied call among them, is not waited on or retried.
func TestOtherErrorsAreNotRetried(t *testing.T) {
	_, f := setup(t, "AKIDOLD")
	cfg, err := Config(context.Background(), "us-west-2", "p")
	if err != nil {
		t.Fatal(err)
	}
	// The client keeps Config's retryer; only its credentials are denied.
	c := sts.NewFromConfig(cfg, func(o *sts.Options) { o.Credentials = credentialsFor("AKIDDENIED") })
	if _, err := c.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v; want AccessDenied", err)
	}
	if got, want := f.seen(), []string{"AKIDOLD", "AKIDDENIED"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v; want the pin and the one denied request", got)
	}
}

func TestPrincipalDropsOnlyTheSessionName(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:sts::111122223333:assumed-role/Admin/carrlos-session": "arn:aws:sts::111122223333:assumed-role/Admin",
		"arn:aws:sts::111122223333:assumed-role/Admin":                 "arn:aws:sts::111122223333:assumed-role/Admin",
		"arn:aws:iam::111122223333:user/ops/carrlos":                   "arn:aws:iam::111122223333:user/ops/carrlos",
		"arn:aws:iam::111122223333:root":                               "arn:aws:iam::111122223333:root",
	} {
		if got := principal(arn); got != want {
			t.Errorf("principal(%q) = %q, want %q", arn, got, want)
		}
	}
}

func credentialsFor(key string) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: key, SecretAccessKey: "s", SessionToken: "t"}, nil
	})
}
