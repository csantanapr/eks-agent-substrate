package demo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A failed first AWS load is not kept for the rest of the demo step (review
// N4): the account read fails while the profile holds no credentials, and
// once they are written the next read loads them and works, in the same
// process. A load pinned once for the step (the sync.Once the demo had)
// returned the first error for good.
func TestAFailedAWSLoadIsNotKept(t *testing.T) {
	dir := t.TempDir()
	creds, config := filepath.Join(dir, "credentials"), filepath.Join(dir, "config")
	for _, f := range []string{creds, config} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Every endpoint is a fake STS that answers GetCallerIdentity; nothing
	// reaches AWS.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>`+
			`<Arn>arn:aws:sts::`+testAccount+`:assumed-role/Admin/s</Arn><UserId>AROAEXAMPLE:s</UserId><Account>`+testAccount+`</Account>`+
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
	}))
	t.Cleanup(srv.Close)
	for k, v := range map[string]string{
		"AWS_SHARED_CREDENTIALS_FILE": creds, "AWS_CONFIG_FILE": config,
		"AWS_ENDPOINT_URL": srv.URL, "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
	} {
		t.Setenv(k, v)
	}
	// A profile name no other test in this process loads.
	e := newReal(Options{Region: "us-west-2", Profile: "demo-n4"})
	ctx := context.Background()
	if _, err := e.account(ctx); err == nil {
		t.Fatal("account with no credentials in the profile: want an error")
	}
	body := "[demo-n4]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = s\naws_session_token = t\n"
	if err := os.WriteFile(creds, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := e.account(ctx); err != nil || got != testAccount {
		t.Fatalf("account once the profile is written: %q, %v; want %s", got, err, testAccount)
	}
}
