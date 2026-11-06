// Command installer is an interactive terminal installer for Agent Substrate
// on Amazon EKS. It drives eks-talk's own scripts (./create-cluster.sh,
// ./deploy.sh, the demos and plugins, ./clean.sh and ./delete-cluster.sh)
// and adds no install logic of its own: it asks the questions, writes
// config.local.env and shows the run.
//
//	go run . --repo /path/to/eks-talk           install
//	go run . --dry-run                          replay a recorded run
//	INSTALLER_SNAPSHOT_DIR=docs/shots go run .  also save every screen
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/config"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/preflight"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/run"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/snap"
	"github.com/csantanapr/eks-agent-substrate/installer/internal/ui"
)

// The run recorded from the live install of substrate-tui, account id
// replaced, for --dry-run.
//
//go:embed testdata/install.jsonl
var recorded []byte

func main() {
	repo := flag.String("repo", "", "the eks-talk checkout to install from (default: the current directory, or its parent)")
	cluster := flag.String("cluster", "", "CLUSTER_NAME (default: from the checkout's config files)")
	region := flag.String("region", "", "AWS_REGION")
	profile := flag.String("profile", "", "AWS_PROFILE")
	dry := flag.Bool("dry-run", false, "replay a recorded run through the same screens; nothing is written or run")
	record := flag.String("record", "", "the record to replay with --dry-run (default: the one built in)")
	speed := flag.Float64("speed", 60, "--dry-run: replay this many times faster than the recording")
	auto := flag.Bool("auto", false, "walk the screens on their own (with --dry-run: a hands-off demo)")
	redact := flag.Bool("redact", false, "mask the AWS account id on screen and in snapshots")
	size := flag.String("snapshot-size", "126x33", "snapshot size in cells, WxH")
	html := flag.String("html", "", "rewrite the HTML page of every NAME.ansi snapshot in this directory, then exit")
	last := flag.Bool("done", false, "open on the Done screen of the checkout's last install (its summary and Teardown)")
	flag.Parse()

	if *html != "" {
		n, err := snap.RebuildHTML(*html, "eks-talk installer — eks-talk-tui")
		if err != nil {
			fail(err)
		}
		fmt.Printf("%d pages written in %s\n", n, *html)
		return
	}

	dir, err := findRepo(*repo)
	if err != nil {
		fail(err)
	}
	r, err := config.LoadRepo(dir)
	if err != nil {
		fail(err)
	}
	// Run go the way the scripts do: lib/common.sh exports the checkout's
	// GOTOOLCHAIN ("local" in config.env). Without it, a go.mod in a parent
	// directory can make the Go check try to download another toolchain.
	if v := r.Effective("GOTOOLCHAIN"); v != "" {
		os.Setenv("GOTOOLCHAIN", v)
	}
	opt := ui.Options{
		Repo: r, RepoRev: gitRev(dir), Redact: *redact, Auto: *auto, AutoQuit: *auto && *dry,
		Seed:        map[string]string{"CLUSTER_NAME": *cluster, "AWS_REGION": *region, "AWS_PROFILE": *profile},
		SnapshotDir: os.Getenv("INSTALLER_SNAPSHOT_DIR"),
	}
	if _, err := fmt.Sscanf(*size, "%dx%d", &opt.SnapW, &opt.SnapH); err != nil {
		fail(fmt.Errorf("--snapshot-size %q: want WxH, e.g. 126x33", *size))
	}
	if *dry {
		data := recorded
		if *record != "" {
			if data, err = os.ReadFile(*record); err != nil {
				fail(err)
			}
		}
		h, evs, err := run.ReadRecord(bytes.NewReader(data))
		if err != nil {
			fail(fmt.Errorf("record: %w", err))
		}
		opt.DryRun = true
		// The replay keeps the recorded wall clock, so its times are the run's.
		opt.Replay = &run.Replay{Events: evs, Speed: *speed, MaxGap: 1500 * time.Millisecond, Clock: h.Started}
		h.Values = merge(h.Values, map[string]string{"CLUSTER_NAME": h.Cluster, "AWS_REGION": h.Region})
		for k, v := range h.Values {
			if opt.Seed[k] == "" {
				opt.Seed[k] = v
			}
		}
		opt.Demos = map[string]bool{}
		for _, d := range h.Demos {
			opt.Demos[d] = true
		}
		if err := json.Unmarshal(h.Checks, &opt.Checks); err != nil || opt.Checks == nil {
			opt.Checks = []preflight.Result{}
		}
	}
	if *last {
		l, err := lastRun(dir)
		if err != nil {
			fail(err)
		}
		opt.Last = l
		for k, v := range l.Header.Values {
			if opt.Seed[k] == "" {
				opt.Seed[k] = v
			}
		}
	}
	if opt.SnapshotDir != "" {
		// Snapshots carry full color whatever the terminal is.
		lipgloss.SetColorProfile(termenv.TrueColor)
		lipgloss.SetHasDarkBackground(true)
	}
	p := tea.NewProgram(ui.New(opt), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fail(err)
	}
}

// lastRun loads the newest install record in DIR/.work/installer/.
func lastRun(dir string) (*ui.LastRun, error) {
	recs, _ := filepath.Glob(filepath.Join(dir, ".work", "installer", "install-*.jsonl"))
	if len(recs) == 0 {
		return nil, fmt.Errorf("no install record in %s; run an install first", filepath.Join(dir, ".work", "installer"))
	}
	sort.Strings(recs) // install-YYYYMMDD-HHMMSS.jsonl sorts by time
	rec := recs[len(recs)-1]
	h, evs, err := run.LoadRecord(rec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rec, err)
	}
	return &ui.LastRun{Header: h, Events: evs, LogPath: strings.TrimSuffix(rec, ".jsonl") + ".log"}, nil
}

// findRepo returns dir, or the current directory, or its parent (go run .
// from installer/), whichever is an eks-talk checkout.
func findRepo(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	for _, d := range []string{".", ".."} {
		if _, err := os.Stat(filepath.Join(d, "config.env")); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("no eks-talk checkout here or in the parent directory; pass --repo DIR")
}

func gitRev(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// merge adds the non-empty values of b that a does not have.
func merge(a, b map[string]string) map[string]string {
	if a == nil {
		a = map[string]string{}
	}
	for k, v := range b {
		if a[k] == "" && v != "" {
			a[k] = v
		}
	}
	return a
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "installer:", err)
	os.Exit(1)
}
