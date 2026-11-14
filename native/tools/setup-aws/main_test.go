package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain runs main() when the test binary is re-executed as setup-aws.
func TestMain(m *testing.M) {
	if os.Getenv("SETUP_AWS_RUN_MAIN") == "1" {
		os.Args = append([]string{"setup-aws"}, strings.Fields(os.Getenv("SETUP_AWS_ARGS"))...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A failed command prints its error once (cobra's "Error: ..." line) and
// exits 1; it used to be printed twice, once more by main.
func TestAFailedCommandPrintsItsErrorOnce(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SETUP_AWS_RUN_MAIN=1", "SETUP_AWS_ARGS=inventory",
		"AWS_REGION=", "CLUSTER_NAME=", "AWS_PROFILE=")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("want exit 1, got %v\n%s", err, out)
	}
	if n := strings.Count(string(out), "--cluster-name (CLUSTER_NAME) is required"); n != 1 {
		t.Errorf("the error is printed %d times, want once:\n%s", n, out)
	}
}
