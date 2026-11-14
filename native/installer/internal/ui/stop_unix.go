//go:build unix

package ui

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// StopProcessTree ends every process this installer started and all their
// children: the Deps.StopTree of a real run, for ctrl+c on a running step
// (review F10). A step is `go run` of setup-aws, or the installer itself
// re-executed; each starts children of its own (the built program, kubectl,
// ate-setup), and execx's context cancel kills only the first, leaving the
// rest running and holding the step's output open.
//
// The installer runs one step at a time, so its children are that step's.
func StopProcessTree() { stopTree(os.Getpid(), 5*time.Second) }

// proc is one row of ps. start (ps lstart) tells a process from a later one
// that reuses its pid.
type proc struct {
	pid, ppid int
	stat      string
	start     string
}

// listProcs is every process, from ps (POSIX options, the same on Linux and
// macOS), without the ps itself.
func listProcs() (map[int]proc, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "stat=", "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	self := cmd.Process.Pid
	procs := map[int]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || pid == self {
			continue
		}
		procs[pid] = proc{pid: pid, ppid: ppid, stat: f[2], start: strings.Join(f[3:], " ")}
	}
	return procs, nil
}

// descendants are root's children, theirs, and so on.
func descendants(procs map[int]proc, root int) []proc {
	kids := map[int][]proc{}
	for _, p := range procs {
		kids[p.ppid] = append(kids[p.ppid], p)
	}
	var out []proc
	queue := []int{root}
	for len(queue) > 0 {
		for _, k := range kids[queue[0]] {
			out = append(out, k)
			queue = append(queue, k.pid)
		}
		queue = queue[1:]
	}
	return out
}

// alive are the processes of known still running: the same pid and start
// time, and not a zombie waiting to be reaped.
func alive(known map[int]proc) []proc {
	procs, err := listProcs()
	if err != nil {
		return nil
	}
	var out []proc
	for pid, k := range known {
		if p, ok := procs[pid]; ok && p.start == k.start && !strings.HasPrefix(p.stat, "Z") {
			out = append(out, k)
		}
	}
	return out
}

// stopTree freezes root's tree, so that no process in it starts another
// while it is listed (SIGSTOP each newly found descendant until a listing
// finds none), sends each SIGTERM and SIGCONT, waits up to grace for them to
// exit, and SIGKILLs what is left. A process that exits and leaves orphans
// cannot hide them: they were found and frozen before anything was killed.
func stopTree(root int, grace time.Duration) {
	known := map[int]proc{}
	for range 50 {
		procs, err := listProcs()
		if err != nil {
			break
		}
		fresh := 0
		for _, p := range descendants(procs, root) {
			if _, ok := known[p.pid]; !ok {
				known[p.pid] = p
				_ = syscall.Kill(p.pid, syscall.SIGSTOP)
				fresh++
			}
		}
		if fresh == 0 {
			break
		}
	}
	for pid := range known {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		_ = syscall.Kill(pid, syscall.SIGCONT)
	}
	for end := time.Now().Add(grace); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if len(alive(known)) == 0 {
			return
		}
	}
	for _, p := range alive(known) {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
}
