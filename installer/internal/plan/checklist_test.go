package plan

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func marker(s string) string { return "\x1b[1;36m== " + s + "\x1b[0m" }

func statuses(g *Group) string {
	var b []string
	for _, r := range g.Rows {
		b = append(b, r.Status.String())
	}
	return strings.Join(b, " ")
}

func TestChecklistFreshCreate(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	g := NewGroup(createCluster)
	g.Begin(at(0))
	g.Line(at(1), marker("Preflight"))
	g.Line(at(2), "   m8i.2xlarge supports nested virtualization")
	g.Line(at(3), marker("Launch template for the microvm node group (nested virtualization)"))
	g.Line(at(5), marker("eksctl create cluster substrate-tui (1.37, us-west-2)"))
	g.Line(at(6), `2026-11-05 20:19:24 [ℹ]  waiting for CloudFormation stack "eksctl-substrate-tui-cluster"`)
	if r := g.Current(); r == nil || r.Title != "EKS control plane, add-ons, node groups" {
		t.Fatalf("current row = %+v", r)
	}
	if d := g.Current().Detail; d != `waiting for CloudFormation stack "eksctl-substrate-tui-cluster"` {
		t.Errorf("detail = %q: the eksctl timestamp and level are dropped", d)
	}
	g.Line(at(1005), marker("Tag the node group Auto Scaling groups (EKS does not copy node group tags to them)"))
	g.Line(at(1040), marker("Default StorageClass: encrypted gp3"))
	g.Line(at(1042), marker("Node IMDS settings"))
	g.Line(at(1050), marker("Timings: create-cluster.sh"))
	g.Line(at(1050), "   Preflight + launch template           0:05")
	g.Line(at(1050), "   eksctl: control plane, add-ons, node groups  16:40")
	g.Line(at(1050), "   Tags, StorageClass, checks              0:45  (failed)")
	g.Line(at(1050), "   total                                  17:30")
	g.Exit(at(1050), 0)

	if s := statuses(g); s != "done done done done done done" {
		t.Errorf("rows: %s", s)
	}
	if d := g.Rows[2].Duration(at(2000)); d != 1000*time.Second {
		t.Errorf("eksctl row took %v, want 1000s", d)
	}
	want := []Timing{{"Preflight + launch template", 5, false}, {"eksctl: control plane, add-ons, node groups", 1000, false}, {"Tags, StorageClass, checks", 45, true}}
	if len(g.Timings) != len(want) {
		t.Fatalf("timings %+v", g.Timings)
	}
	for i := range want {
		if g.Timings[i] != want[i] {
			t.Errorf("timing %d = %+v, want %+v", i, g.Timings[i], want[i])
		}
	}
	if g.Total != 1050 {
		t.Errorf("total %d", g.Total)
	}
	if g.Status != Done {
		t.Errorf("group %v", g.Status)
	}
}

// A rerun on an existing cluster prints no eksctl line; the optional row
// is skipped and the next one still matches.
func TestChecklistRerunSkipsOptional(t *testing.T) {
	now := time.Now()
	g := NewGroup(createCluster)
	g.Begin(now)
	for _, s := range []string{"Preflight", "Launch template for the microvm node group", "Tag the node group Auto Scaling groups", "Default StorageClass: encrypted gp3", "Node IMDS settings"} {
		g.Line(now, marker(s))
	}
	g.Exit(now, 0)
	if s := statuses(g); s != "done done skipped done done done" {
		t.Errorf("rows: %s", s)
	}
}

// A step the plan does not list becomes its own row, in place, and does not
// make the checklist jump past planned rows.
func TestChecklistDynamicRow(t *testing.T) {
	now := time.Now()
	g := NewGroup(Teardown()[0])
	g.Begin(now)
	g.Line(now, marker("Demos and the LLM gateway"))
	g.Line(now, marker("Actors and templates on EFS and S3 Files"))
	g.Line(now, marker("IAM roles")) // demos/efs/clean.sh's own
	g.Line(now, marker("Agent Substrate"))
	if g.Rows[1].Title != "Actors and templates on EFS and S3 Files" || !g.Rows[1].Dynamic {
		t.Fatalf("row 1 = %+v", g.Rows[1])
	}
	if g.Rows[2].Title != "IAM roles" || !g.Rows[2].Dynamic {
		t.Fatalf("row 2 = %+v: the demo's IAM roles line must not take the planned IAM roles row", g.Rows[2])
	}
	if r := g.Current(); r.Title != "Agent Substrate uninstall" {
		t.Errorf("current = %q", r.Title)
	}
	g.Exit(now, 1)
	if g.Status != Failed || g.Current() != nil {
		t.Errorf("after a failure the group is failed and nothing runs")
	}
	failed := 0
	for _, r := range g.Rows {
		if r.Status == Failed {
			failed++
			if r.Title != "Agent Substrate uninstall" {
				t.Errorf("failed row %q", r.Title)
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d failed rows", failed)
	}
}

func TestMarker(t *testing.T) {
	if s, ok := Marker("\x1b[1;36m== S3 buckets\x1b[0m"); !ok || s != "S3 buckets" {
		t.Errorf("Marker = %q %v", s, ok)
	}
	if _, ok := Marker("   == not a marker? yes it is trimmed"); !ok {
		t.Error("leading spaces are trimmed")
	}
	if _, ok := Marker("info line"); ok {
		t.Error("plain line taken as a marker")
	}
}

// Every planned row matches a log "..." line that a script really prints:
// the scripts' wording is the interface, so a reworded step fails here.
func TestPlanMatchesScripts(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var files []string
	for _, pat := range []string{"*.sh", "aws/*.sh", "substrate/*.sh", "llm-gateway/*.sh", "demos/*/*.sh"} {
		m, _ := filepath.Glob(filepath.Join(root, pat))
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Skip("no eks-talk scripts next to installer/")
	}
	logRe := regexp.MustCompile(`\blog "([^"]*)"`)
	var logs []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range logRe.FindAllStringSubmatch(string(b), -1) {
			logs = append(logs, m[1])
		}
	}
	// A log text with $VAR or $(...) in it matches anything there.
	varRe := regexp.MustCompile(`\$(\{[^}]*\}|\([^)]*\)|[A-Za-z0-9_]+)`)
	matches := func(prefix string) bool {
		for _, l := range logs {
			parts := varRe.Split(l, -1)
			if strings.TrimSpace(strings.Join(parts, "")) == "" {
				continue // log "$1": a helper's parameter, not a step text
			}
			re := "^"
			for i, p := range parts {
				if i > 0 {
					re += ".*"
				}
				re += regexp.QuoteMeta(p)
			}
			if regexp.MustCompile(re).MatchString(prefix) || strings.HasPrefix(l, prefix) {
				return true
			}
		}
		return false
	}
	all := append(Install(map[string]bool{"counter-verify": true, "s3files": true, "db-rotation": true,
		"credprovider-aws": true, "openclaw": true}), Teardown()...)
	for _, c := range all {
		for _, r := range c.Rows {
			ok := false
			for _, p := range r.Match {
				if matches(p) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: row %q matches no log line in the scripts (%q)", c.ID, r.Title, r.Match)
			}
		}
	}
}

func TestInstallOrder(t *testing.T) {
	ids := func(demos map[string]bool) string {
		var s []string
		for _, c := range Install(demos) {
			s = append(s, c.ID)
		}
		return strings.Join(s, " ")
	}
	for _, tc := range []struct {
		demos map[string]bool
		want  string
	}{
		{map[string]bool{"counter-verify": true}, "create-cluster deploy counter-verify"},
		// OpenClaw keeps its gateway state on EFS, and goes after the plugins.
		{map[string]bool{"openclaw": true}, "create-cluster deploy efs efs-verify openclaw openclaw-verify"},
		// Mode 3 goes before the counter check, which checks it too.
		{map[string]bool{"mode3": true, "counter-verify": true}, "create-cluster deploy mode3 counter-verify"},
		{map[string]bool{"db-rotation": true, "credprovider-aws": true, "openclaw": true},
			"create-cluster deploy efs efs-verify db-rotation credprovider-aws credprovider-aws-verify openclaw openclaw-verify"},
	} {
		if got := ids(tc.demos); got != tc.want {
			t.Errorf("Install(%v) = %s, want %s", tc.demos, got, tc.want)
		}
	}
	if c, ok := ByID("db-rotation-verify"); !ok || c.Args[0] != "demos/db-rotation/verify.sh" {
		t.Error("ByID still finds the retired db-rotation-verify, to replay older logs")
	}
}
