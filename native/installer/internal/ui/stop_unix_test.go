//go:build unix

package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
)

// TestMain doubles as the processes of the stop test: with UI_STOP_HELPER
// set, the test binary is a "step" that starts a child of its own, as `go
// run` starts the program it built, and both sleep.
func TestMain(m *testing.M) {
	switch os.Getenv("UI_STOP_HELPER") {
	case "parent":
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), "UI_STOP_HELPER=child")
		c.Stdout = os.Stdout // the child holds the step's output open, as kubectl does
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Printf("pids %d %d\n", os.Getpid(), c.Process.Pid)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "spawn":
		// A step that keeps starting children: one `sleep TOKEN` every few
		// milliseconds, so new processes appear while the tree is listed.
		fmt.Println("spawning")
		for range 600 {
			_ = exec.Command("sleep", os.Getenv("UI_STOP_TOKEN")).Start()
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// sleepers are the processes whose command line carries token.
func sleepers(t *testing.T, token string) []int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, l := range strings.Split(string(out), "\n") {
		var pid int
		if f := strings.Fields(l); len(f) >= 3 && f[1] == "sleep" && f[2] == token {
			if _, err := fmt.Sscan(f[0], &pid); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// The tree is frozen while it is listed, so a step that is starting
// children at the time leaves none behind: without the freeze, the children
// it starts between the last listing and its own end live on as orphans.
func TestStopTreeCatchesChildrenStartedWhileListing(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps")
	}
	token := fmt.Sprintf("600.%09d", time.Now().Nanosecond())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := (&execx.Real{}).Start(ctx, execx.Spec{Label: "spawner", Argv: []string{os.Args[0], "-test.run=^$"},
		Env: []string{"UI_STOP_HELPER=spawn", "UI_STOP_TOKEN=" + token}})
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("the spawner did not start")
	}
	time.Sleep(100 * time.Millisecond)
	stopTree(os.Getpid(), 5*time.Second)
	cancel()
	drained(ch, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	left := sleepers(t, token)
	for _, pid := range left {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if len(left) > 0 {
		t.Errorf("%d children outlived stopTree", len(left))
	}
}

// startHelper runs the helper step through execx.Real and returns its
// channel and the step's and its child's processes.
func startHelper(t *testing.T, ctx context.Context) (<-chan execx.Event, map[int]proc) {
	t.Helper()
	ch := (&execx.Real{}).Start(ctx, execx.Spec{Label: "helper", Argv: []string{os.Args[0], "-test.run=^$"}, Env: []string{"UI_STOP_HELPER=parent"}})
	var parent, child int
	select {
	case ev := <-ch:
		if _, err := fmt.Sscanf(ev.Line, "pids %d %d", &parent, &child); err != nil {
			t.Fatalf("helper said %q: %v", ev.Line, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the helper did not start")
	}
	procs, err := listProcs()
	if err != nil {
		t.Fatal(err)
	}
	return ch, map[int]proc{parent: procs[parent], child: procs[child]}
}

func drained(ch <-chan execx.Event, wait time.Duration) bool {
	end := time.After(wait)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true
			}
		case <-end:
			return false
		}
	}
}

// Review F10, measured on real processes: cancelling the step's context
// alone (what ctrl+c did) kills the step but leaves its child running, and
// the step's output open; stopTree ends both, and the step finishes.
func TestStopTreeEndsTheStepAndItsChildren(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, both := startHelper(t, ctx)
	stopTree(os.Getpid(), 5*time.Second)
	cancel()
	if !drained(ch, 10*time.Second) {
		t.Fatal("the step's output stayed open after stopTree")
	}
	for end := time.Now().Add(5 * time.Second); len(alive(both)) > 0 && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if left := alive(both); len(left) > 0 {
		t.Errorf("still running after stopTree: %+v", left)
	}

	ctx, cancel = context.WithCancel(context.Background())
	ch, both = startHelper(t, ctx)
	cancel()
	time.Sleep(500 * time.Millisecond)
	left := alive(both)
	for _, p := range left {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
	if len(left) != 1 || drained(ch, 0) {
		t.Errorf("a cancel alone should leave the child running and the output open; alive %+v", left)
	}
	if !drained(ch, 10*time.Second) {
		t.Error("the output stayed open after the child was killed")
	}
}

func TestDescendantsWalksTheTree(t *testing.T) {
	procs := map[int]proc{2: {pid: 2, ppid: 1}, 3: {pid: 3, ppid: 2}, 4: {pid: 4, ppid: 3}, 5: {pid: 5, ppid: 1}, 6: {pid: 6, ppid: 9}}
	var got []string
	for _, p := range descendants(procs, 2) {
		got = append(got, fmt.Sprint(p.pid))
	}
	if strings.Join(got, ",") != "3,4" {
		t.Errorf("descendants of 2: %v", got)
	}
}
