package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// kubectlProfileOut is where this test binary, run as "kubectl", writes the
// AWS_PROFILE it inherited.
const kubectlProfileOut = "SUBSTRATE_EKS_TEST_KUBECTL_PROFILE_OUT"

// When TestProfileFlagIsTheOnlyIdentity links this test binary as kubectl,
// it is the first child the installer starts: it records whether AWS_PROFILE
// reached it, and prints no plugin record, so the run ends there.
func init() {
	if filepath.Base(os.Args[0]) != "kubectl" {
		return
	}
	if answers, ok := os.LookupEnv(kubectlAnswers); ok {
		answerKubectl(answers) // release_cli_test.go's cluster; it exits
	}
	seen := ""
	for _, k := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		if v, ok := os.LookupEnv(k); ok {
			seen += k + "=" + v + " "
		}
	}
	_ = os.WriteFile(os.Getenv(kubectlProfileOut), []byte(strings.TrimSpace(seen)), 0o600)
	os.Exit(0)
}

var sigV4Key = regexp.MustCompile(`Credential=([^/]+)/`)

// Review R15, both ways, through main(): with AWS_PROFILE=exported in the
// environment, --profile "" makes the in-process AWS SDK load use the
// default profile and keeps AWS_PROFILE from every child; --profile chosen
// gives both the chosen profile, and drops an exported AWS_DEFAULT_PROFILE,
// which the aws CLI would read first. Before the fix, both the SDK load and
// the child used "exported" with --profile "", and the child did with
// --profile chosen. The run is --plugin-action remove on an offline
// account: EKS is a local server and kubectl is this test binary.
func TestProfileFlagIsTheOnlyIdentity(t *testing.T) {
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		profile, defaultProfile, wantKey, wantChild string
	}{
		// AWS_DEFAULT_PROFILE empty: what the SDK and the CLI both skip.
		{"", "", "KEYDEFAULT", "AWS_DEFAULT_PROFILE="},
		{"chosen", "other", "KEYCHOSEN", "AWS_PROFILE=chosen"},
	} {
		t.Run("profile="+tc.profile, func(t *testing.T) {
			var mu sync.Mutex
			var keys []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if m := sigV4Key.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
					keys = append(keys, m[1])
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"cluster":{"name":"demo","version":"1.37","status":"ACTIVE","endpoint":"https://127.0.0.1:1","certificateAuthority":{"data":"Y2E="}}}`)
			}))
			defer srv.Close()

			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(self, filepath.Join(bin, "kubectl")); err != nil {
				t.Fatal(err)
			}
			// Placeholder keys, one per profile, so the signed request names
			// the profile the SDK loaded.
			conf := "[default]\naws_access_key_id = KEYDEFAULT\naws_secret_access_key = placeholder\n" +
				"[profile exported]\naws_access_key_id = KEYEXPORTED\naws_secret_access_key = placeholder\n" +
				"[profile chosen]\naws_access_key_id = KEYCHOSEN\naws_secret_access_key = placeholder\n"
			if err := os.WriteFile(filepath.Join(dir, "config"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "kubectl-profile")
			cmd := exec.Command(self, "-test.run=^TestSubcommandHelper$")
			cmd.Env = append(os.Environ(),
				mainArgsEnv+"="+strings.Join([]string{"--plugin-action", "remove", "--cluster", "demo", "--region", "us-west-2", "--profile", tc.profile}, "\n"),
				"AWS_PROFILE=exported", "AWS_DEFAULT_PROFILE="+tc.defaultProfile,
				"AWS_CONFIG_FILE="+filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE="+filepath.Join(dir, "none"),
				"AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=",
				"AWS_EC2_METADATA_DISABLED=true", "AWS_ENDPOINT_URL="+srv.URL, "XDG_CACHE_HOME="+dir,
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), kubectlProfileOut+"="+out)
			var stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stderr, &stderr
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the installer failed: %v\n%s", err, stderr.String())
				}
			case <-time.After(60 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("the installer did not finish:\n%s", stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(keys) == 0 || keys[0] != tc.wantKey {
				t.Errorf("the in-process SDK load signed with %v, want %s (the profile --profile names)", keys, tc.wantKey)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("kubectl never ran: %v\n%s", err, stderr.String())
			}
			if string(got) != tc.wantChild {
				t.Errorf("kubectl inherited %q, want %q", got, tc.wantChild)
			}
		})
	}
}
