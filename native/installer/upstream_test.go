package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type lockEntry struct{ kind, upstream, local string }

func readLock(t *testing.T) (commit string, entries []lockEntry) {
	t.Helper()
	f, err := os.Open("../upstream.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		switch {
		case len(fields) == 0 || strings.HasPrefix(fields[0], "#"):
		case fields[0] == "commit" && len(fields) == 2:
			commit = fields[1]
		case (fields[0] == "verbatim" || fields[0] == "adapted") && len(fields) == 3:
			entries = append(entries, lockEntry{fields[0], fields[1], fields[2]})
		default:
			t.Fatalf("upstream.lock: cannot parse %q", sc.Text())
		}
	}
	if commit == "" || len(entries) == 0 {
		t.Fatal("upstream.lock has no commit or no entries")
	}
	return commit, entries
}

// TestUpstreamLockNamesRealLocalFiles runs everywhere: every local path in the
// lock exists, so the map cannot rot silently.
func TestUpstreamLockNamesRealLocalFiles(t *testing.T) {
	_, entries := readLock(t)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join("..", e.local)); err != nil {
			t.Errorf("%s: %v", e.local, err)
		}
	}
}

// TestUpstreamSync needs a substrate-gke checkout in SUBSTRATE_GKE (fetched
// up to date). Verbatim files must match the locked commit exactly; for
// adapted files it logs the upstream commits since the lock that touch them.
func TestUpstreamSync(t *testing.T) {
	gke := os.Getenv("SUBSTRATE_GKE")
	if gke == "" {
		t.Skip("set SUBSTRATE_GKE to a substrate-gke checkout")
	}
	commit, entries := readLock(t)
	git := func(args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", gke}, args...)...).Output()
	}
	head, err := git("rev-parse", "origin/main")
	if err != nil {
		t.Fatalf("git rev-parse origin/main in %s: %v", gke, err)
	}
	t.Logf("locked at %s; upstream main is %s", commit[:12], strings.TrimSpace(string(head))[:12])
	pending := 0
	for _, e := range entries {
		if e.kind == "verbatim" {
			want, err := git("show", commit+":"+e.upstream)
			if err != nil {
				t.Errorf("%s at %s: %v", e.upstream, commit[:12], err)
				continue
			}
			got, err := os.ReadFile(filepath.Join("..", e.local))
			if err != nil {
				t.Error(err)
				continue
			}
			if !bytes.Equal(want, got) {
				t.Errorf("%s was edited locally; verbatim files must match substrate-gke@%s (change it upstream, or make it adapted)", e.local, commit[:12])
			}
		}
		log, err := git("log", "--oneline", commit+"..origin/main", "--", e.upstream)
		if err != nil {
			t.Errorf("git log %s: %v", e.upstream, err)
			continue
		}
		if s := strings.TrimSpace(string(log)); s != "" {
			pending++
			t.Logf("upstream changed %s (%s %s):\n%s", e.upstream, e.kind, e.local, s)
		}
	}
	if pending == 0 {
		t.Log("nothing to pick up: no upstream commit since the lock touches a followed file")
	}
}
