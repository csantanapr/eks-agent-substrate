package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// The bootstrap's cleanup runs, in this order: the Secret with the logins,
// the namespace delete without waiting, the Job's delete, which waits for it
// and its pod (review R6: a kill during that wait leaves nothing the API
// server does not then remove).
var cleanupCmds = []string{
	"-n ate-db-bootstrap delete secret creds --ignore-not-found",
	"delete namespace ate-db-bootstrap --ignore-not-found --wait=false",
	"-n ate-db-bootstrap delete job bootstrap --ignore-not-found --cascade=foreground",
}

// nsWait is the wait for the namespace to go, after a success only.
const nsWait = "wait --for=delete namespace/ate-db-bootstrap --timeout=2m"

// cancelAt wraps k so that the command containing at cancels ctx and fails
// as exec.CommandContext does, and so that, like CommandContext, a command
// started on a done context fails without running. live records, per
// command, whether its context was still live.
func cancelAt(k *fakeKube, at string, cancel context.CancelFunc, live map[string]bool) func(context.Context, io.Writer, command) error {
	return func(ctx context.Context, w io.Writer, c command) error {
		line := strings.Join(c.Argv, " ")
		live[line] = ctx.Err() == nil
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.Contains(line, at) {
			k.cmds = append(k.cmds, c)
			cancel()
			return ctx.Err()
		}
		return k.run(ctx, w, c)
	}
}

// ran returns the index in k.cmds of each cleanup command, -1 if missing.
func ran(k *fakeKube) []int {
	var at []int
	for _, want := range cleanupCmds {
		at = append(at, slices.IndexFunc(k.cmds, func(c command) bool { return strings.Contains(strings.Join(c.Argv, " "), want) }))
	}
	return at
}

// Cancelling the deploy (ctrl+c or SIGTERM) at any point after the
// namespace is created still deletes the Secret, the Job and the namespace,
// each with a live context of their own; ate-setup never runs.
func TestCancelledBootstrapStillDeletesTheSecretAndJob(t *testing.T) {
	for _, at := range []string{
		"apply -f -", // the namespace: kubectl may have created it before it died
		"wait --for=condition=complete",
		"logs job/bootstrap",
	} {
		t.Run(at, func(t *testing.T) {
			k := &fakeKube{}
			o, tree := rdsFixture(t, k, "demo.us-west-2")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			live := map[string]bool{}
			runCommand = cancelAt(k, at, cancel, live)
			var log bytes.Buffer
			err := install(ctx, &log, o, "demo", "/k", tree)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context.Canceled, got %v", err)
			}
			idx := ran(k)
			for i, c := range cleanupCmds {
				if idx[i] < 0 {
					t.Fatalf("not run with a live context after the cancel: kubectl %s (tried: %v)", c, live)
				}
				if line := strings.Join(k.cmds[idx[i]].Argv, " "); !live[line] {
					t.Errorf("%s ran on the cancelled context", line)
				}
			}
			if !slices.IsSorted(idx) {
				t.Errorf("cleanup order %v, want secret, namespace (no wait), job", idx)
			}
			if k.find(nsWait) != nil {
				t.Error("after a cancel the cleanup waits for the namespace")
			}
			if k.find("./cmd/ate-setup") != nil || strings.Contains(log.String(), BootstrapDone) {
				t.Errorf("ate-setup ran or the bootstrap reported done after a cancel:\n%s", log.String())
			}
		})
	}
}

// The normal path runs the same three after the Job's logs, then waits for
// the namespace to go, and only then reports the bootstrap done.
func TestBootstrapDeletesTheSecretAndJobWhenItSucceeds(t *testing.T) {
	k := &fakeKube{}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	var log bytes.Buffer
	if err := install(context.Background(), &log, o, "demo", "/k", tree); err != nil {
		t.Fatal(err)
	}
	idx := ran(k)
	logs := slices.IndexFunc(k.cmds, func(c command) bool { return strings.Contains(strings.Join(c.Argv, " "), "logs job/bootstrap") })
	wait := slices.IndexFunc(k.cmds, func(c command) bool { return strings.Join(c.Argv, " ") == "kubectl "+nsWait })
	ate := slices.IndexFunc(k.cmds, func(c command) bool { return slices.Contains(c.Argv, "./cmd/ate-setup") })
	if slices.Contains(idx, -1) || wait < 0 || !slices.IsSorted(append([]int{logs}, append(idx, wait, ate)...)) {
		t.Fatalf("logs at %d, cleanup at %v, namespace wait at %d, ate-setup at %d: want logs, secret, namespace, job, namespace wait, ate-setup",
			logs, idx, wait, ate)
	}
	if !strings.Contains(log.String(), BootstrapDone+bootstrapNS) {
		t.Errorf("no done line:\n%s", log.String())
	}
}

// A failed cleanup after a success fails the deploy, so a Secret left
// behind is never reported as removed.
func TestBootstrapCleanupFailureFailsTheDeploy(t *testing.T) {
	k := &fakeKube{fail: "delete secret creds"}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	var log bytes.Buffer
	err := install(context.Background(), &log, o, "demo", "/k", tree)
	if err == nil || strings.Contains(log.String(), BootstrapDone) || k.find("./cmd/ate-setup") != nil {
		t.Fatalf("err %v, log:\n%s", err, log.String())
	}
	// The other two still ran.
	if idx := ran(k); idx[1] < 0 || idx[2] < 0 {
		t.Errorf("cleanup stopped at the first failure: %v", idx)
	}
}
