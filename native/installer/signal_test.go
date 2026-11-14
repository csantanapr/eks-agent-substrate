package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const mainArgsEnv = "SUBSTRATE_EKS_TEST_MAIN_ARGS"

// TestSubcommandHelper is main() in the child process that
// TestSubcommandStopsOnASignal starts; in a normal run it does nothing.
func TestSubcommandHelper(t *testing.T) {
	args, ok := os.LookupEnv(mainArgsEnv)
	if !ok {
		t.Skip("runs only as the child of TestSubcommandStopsOnASignal")
	}
	os.Args = append([]string{"substrate-eks"}, strings.Split(args, "\n")...)
	registerSettingsProbe()
	main()
}

// A subcommand stopped by SIGTERM or SIGINT ends through its context: it
// exits 1 with the cancelled call's error, where it would die of the signal
// (and skip its cleanup) without signal.NotifyContext. The subcommand is
// --plugin-action remove, blocked on an EKS DescribeCluster that a local
// server never answers.
func TestSubcommandStopsOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			asked := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case asked <- struct{}{}:
				default:
				}
				<-r.Context().Done() // never answers
			}))
			defer srv.Close()

			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestSubcommandHelper$")
			cmd.Env = append(os.Environ(),
				mainArgsEnv+"="+strings.Join([]string{"--plugin-action", "remove", "--cluster", "demo", "--region", "us-west-2", "--profile", ""}, "\n"),
				// An offline account: placeholder keys, no config files, every
				// AWS endpoint the local server.
				"AWS_PROFILE=", "AWS_CONFIG_FILE="+dir+"/none", "AWS_SHARED_CREDENTIALS_FILE="+dir+"/none",
				"AWS_ACCESS_KEY_ID=placeholder", "AWS_SECRET_ACCESS_KEY=placeholder", "AWS_SESSION_TOKEN=",
				"AWS_EC2_METADATA_DISABLED=true", "AWS_ENDPOINT_URL="+srv.URL, "XDG_CACHE_HOME="+dir)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-asked:
			case err := <-done:
				t.Fatalf("the subcommand ended before calling EKS: %v\n%s", err, stderr.String())
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("the subcommand never called EKS:\n%s", stderr.String())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("still running 30s after %s:\n%s", sig, stderr.String())
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(stderr.String(), "context canceled") {
				t.Errorf("after %s: %v (exit code %d), stderr:\n%s; want exit 1 with the cancelled call's error", sig, err, exitCode(ee), stderr.String())
			}
		})
	}
}

func exitCode(ee *exec.ExitError) int {
	if ee == nil {
		return 0
	}
	return ee.ExitCode()
}
