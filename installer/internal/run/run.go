// Package run runs the planned scripts in the target checkout and streams
// their output as events, or replays a recorded run (--dry-run).
package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/csantanapr/eks-agent-substrate/installer/internal/plan"
)

// Kind of event.
type Kind string

const (
	Start Kind = "start" // a command started
	Line  Kind = "line"  // one output line (stdout and stderr together)
	Exit  Kind = "exit"  // a command ended with Code
	End   Kind = "end"   // the whole run ended (Code: the first failure, or 0)
)

// Event is one thing that happened in a run.
type Event struct {
	Kind    Kind      `json:"k"`
	Ms      int64     `json:"t"` // milliseconds since the run started
	ID      string    `json:"id,omitempty"`
	Display string    `json:"cmd,omitempty"`
	Text    string    `json:"s,omitempty"`
	Code    int       `json:"code,omitempty"`
	Time    time.Time `json:"-"`
}

// Header is the first line of a record file.
type Header struct {
	Kind    string    `json:"k"` // "header"
	Version int       `json:"v"`
	Started time.Time `json:"started"`
	Cluster string    `json:"cluster"`
	Region  string    `json:"region"`
	// Values are the settings the run used; Checks the preflight results
	// (JSON), so a dry run shows the same screens.
	Values map[string]string `json:"values,omitempty"`
	Demos  []string          `json:"demos,omitempty"`
	Checks json.RawMessage   `json:"checks,omitempty"`
}

// Runner runs (or replays) commands and sends their events to out, ending
// with an End event. It closes nothing; the caller owns out.
type Runner interface {
	Run(ctx context.Context, cmds []plan.Command, out chan<- Event)
}

// Exec runs the scripts with bash in Dir. Each line goes to the plain log
// and, when Record is set, to a JSON-lines record a dry run can replay.
type Exec struct {
	Dir     string
	LogPath string
	Record  string
	Header  Header
	// Env is added to the environment of every command.
	Env []string
}

// childEnv is the installer's environment without SUBSTRATE_TAG (which the
// scripts let win over config.local.env), plus extra.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SUBSTRATE_TAG=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// Run implements Runner.
func (e *Exec) Run(ctx context.Context, cmds []plan.Command, out chan<- Event) {
	t0 := time.Now()
	var logf, recf io.WriteCloser
	if e.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(e.LogPath), 0o700); err == nil {
			logf, _ = os.OpenFile(e.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
	}
	if e.Record != "" {
		if err := os.MkdirAll(filepath.Dir(e.Record), 0o700); err == nil {
			recf, _ = os.OpenFile(e.Record, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		}
	}
	var mu sync.Mutex
	emit := func(ev Event) {
		ev.Time = time.Now()
		ev.Ms = ev.Time.Sub(t0).Milliseconds()
		mu.Lock()
		if recf != nil {
			b, _ := json.Marshal(ev)
			recf.Write(append(b, '\n'))
		}
		if logf != nil {
			switch ev.Kind {
			case Start:
				fmt.Fprintf(logf, "\n### %s  (started %s)\n", ev.Display, ev.Time.Format(time.RFC3339))
			case Line:
				fmt.Fprintln(logf, plan.StripANSI(ev.Text))
			case Exit:
				fmt.Fprintf(logf, "### exit %d  (%s)\n", ev.Code, ev.Time.Format(time.RFC3339))
			}
		}
		mu.Unlock()
		out <- ev
	}
	if recf != nil {
		h := e.Header
		h.Kind, h.Version, h.Started = "header", 1, t0
		b, _ := json.Marshal(h)
		recf.Write(append(b, '\n'))
	}
	code := 0
	for _, c := range cmds {
		if ctx.Err() != nil {
			code = 130
			break
		}
		emit(Event{Kind: Start, ID: c.ID, Display: c.Display()})
		rc := e.one(ctx, c, func(s string) { emit(Event{Kind: Line, ID: c.ID, Text: s}) })
		emit(Event{Kind: Exit, ID: c.ID, Code: rc})
		if rc != 0 {
			code = rc
			break
		}
	}
	emit(Event{Kind: End, Code: code})
	if logf != nil {
		logf.Close()
	}
	if recf != nil {
		recf.Close()
	}
}

// one runs a command and returns its exit code (127 when it cannot start).
func (e *Exec) one(ctx context.Context, c plan.Command, line func(string)) int {
	cmd := exec.Command("bash", c.Args...)
	cmd.Dir = e.Dir
	cmd.Env = childEnv(append(append([]string{}, e.Env...), c.Env...)...)
	cmd.Stdin = nil // /dev/null: a script that asks a question fails instead of waiting
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, w, err := os.Pipe()
	if err != nil {
		line("installer: " + err.Error())
		return 127
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		w.Close()
		r.Close()
		line("installer: " + err.Error())
		return 127
	}
	w.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// The whole process group: eksctl, aws and kubectl children too.
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		case <-done:
		}
	}()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		s, err := br.ReadString('\n')
		if s != "" {
			line(strings.TrimRight(s, "\n"))
		}
		if err != nil {
			break
		}
	}
	r.Close()
	err = cmd.Wait()
	close(done)
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 128 + int(syscall.SIGTERM)
	}
	return 127
}

// Replay sends a recorded run again. Gaps between events are divided by
// Speed and capped at MaxGap, so a 45-minute run replays in a minute or
// two; Speed 0 replays at once (tests).
type Replay struct {
	Events []Event
	Speed  float64
	MaxGap time.Duration
	// Clock is the time the replay pretends the run started at (default: now).
	Clock time.Time
}

// LoadRecord reads a record file written by Exec.
func LoadRecord(path string) (Header, []Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, nil, err
	}
	defer f.Close()
	return ReadRecord(f)
}

// ReadRecord parses a record: a header line, then one event per line.
func ReadRecord(r io.Reader) (Header, []Event, error) {
	var h Header
	var evs []Event
	br := bufio.NewReaderSize(r, 64*1024)
	n := 0
	for {
		s, err := br.ReadString('\n')
		if strings.TrimSpace(s) != "" {
			n++
			if n == 1 {
				if e := json.Unmarshal([]byte(s), &h); e != nil || h.Kind != "header" {
					return h, nil, fmt.Errorf("line 1: not a record header")
				}
			} else {
				var ev Event
				if e := json.Unmarshal([]byte(s), &ev); e != nil {
					return h, nil, fmt.Errorf("line %d: %w", n, e)
				}
				evs = append(evs, ev)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return h, nil, err
		}
	}
	if n == 0 {
		return h, nil, fmt.Errorf("empty record")
	}
	return h, evs, nil
}

// Run implements Runner. It replays the recorded commands, whatever cmds
// says, so the screens show what really ran.
func (p *Replay) Run(ctx context.Context, _ []plan.Command, out chan<- Event) {
	start := p.Clock
	if start.IsZero() {
		start = time.Now()
	}
	var last int64
	sawEnd := false
	for _, ev := range p.Events {
		if p.Speed > 0 {
			gap := time.Duration(float64(time.Duration(ev.Ms-last)*time.Millisecond) / p.Speed)
			if p.MaxGap > 0 && gap > p.MaxGap {
				gap = p.MaxGap
			}
			select {
			case <-ctx.Done():
				out <- Event{Kind: End, Code: 130, Ms: ev.Ms, Time: start.Add(time.Duration(ev.Ms) * time.Millisecond)}
				return
			case <-time.After(gap):
			}
		}
		last = ev.Ms
		ev.Time = start.Add(time.Duration(ev.Ms) * time.Millisecond)
		out <- ev
		if ev.Kind == End {
			sawEnd = true
		}
	}
	if !sawEnd {
		out <- Event{Kind: End, Ms: last, Time: start.Add(time.Duration(last) * time.Millisecond)}
	}
}
