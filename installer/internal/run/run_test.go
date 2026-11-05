package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
)

func collect(r Runner, cmds []plan.Command) []Event {
	ch := make(chan Event, 1000)
	go r.Run(context.Background(), cmds, ch)
	var evs []Event
	for ev := range ch {
		evs = append(evs, ev)
		if ev.Kind == End {
			break
		}
	}
	return evs
}

func TestExecRunsInOrderAndRecords(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "one.sh"), []byte("printf '\\033[1;36m== Step one\\033[0m\\n'; echo out; echo err >&2; echo \"tag=${SUBSTRATE_TAG:-unset} extra=$EXTRA\"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "two.sh"), []byte("echo second; exit 3\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "three.sh"), []byte("echo never\n"), 0o755)
	t.Setenv("SUBSTRATE_TAG", "v9.9.9")
	e := &Exec{Dir: dir, LogPath: filepath.Join(dir, "w", "run.log"), Record: filepath.Join(dir, "w", "run.jsonl"),
		Header: Header{Cluster: "c1", Region: "us-west-2"}}
	cmds := []plan.Command{
		{ID: "one", Args: []string{"./one.sh"}, Env: []string{"EXTRA=yes"}},
		{ID: "two", Args: []string{"./two.sh"}},
		{ID: "three", Args: []string{"./three.sh"}},
	}
	evs := collect(e, cmds)
	var kinds, lines []string
	for _, ev := range evs {
		kinds = append(kinds, string(ev.Kind)+":"+ev.ID)
		if ev.Kind == Line {
			lines = append(lines, plan.StripANSI(ev.Text))
		}
	}
	if got := strings.Join(kinds, " "); !strings.HasPrefix(got, "start:one line:one") || !strings.HasSuffix(got, "exit:two end:") || strings.Contains(got, "three") {
		t.Errorf("events: %s", got)
	}
	joined := strings.Join(lines, "|")
	for _, want := range []string{"== Step one", "out", "err", "tag=unset extra=yes", "second"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no line %q in %s (stdout and stderr both stream; SUBSTRATE_TAG is not passed on)", want, joined)
		}
	}
	if end := evs[len(evs)-1]; end.Code != 3 {
		t.Errorf("end code %d, want the failing command's 3", end.Code)
	}
	h, rec, err := LoadRecord(e.Record)
	if err != nil {
		t.Fatal(err)
	}
	if h.Cluster != "c1" || h.Version != 1 || len(rec) != len(evs) {
		t.Errorf("record header %+v, %d events vs %d", h, len(rec), len(evs))
	}
	log, _ := os.ReadFile(e.LogPath)
	if !strings.Contains(string(log), "### EXTRA=yes ./one.sh") || strings.Contains(string(log), "\x1b[") {
		t.Errorf("plain log:\n%s", log)
	}
}

func TestReplay(t *testing.T) {
	r := strings.NewReader(`{"k":"header","v":1,"cluster":"c"}
{"k":"start","t":0,"id":"deploy","cmd":"./deploy.sh"}
{"k":"line","t":5000,"id":"deploy","s":"== S3 buckets"}
{"k":"exit","t":9000,"id":"deploy"}
`)
	h, evs, err := ReadRecord(r)
	if err != nil || h.Cluster != "c" || len(evs) != 3 {
		t.Fatalf("ReadRecord: %+v %d %v", h, len(evs), err)
	}
	got := collect(&Replay{Events: evs}, nil)
	if len(got) != 4 || got[3].Kind != End {
		t.Fatalf("replay: %+v (an End is added when the record has none)", got)
	}
	if d := got[2].Time.Sub(got[0].Time).Seconds(); d != 9 {
		t.Errorf("replayed times keep the recorded gaps: %vs", d)
	}
	if _, _, err := ReadRecord(strings.NewReader("{}\n")); err == nil {
		t.Error("a record without a header must fail")
	}
}
