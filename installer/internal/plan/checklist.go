package plan

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Status of a checklist row or a command.
type Status int

const (
	Pending Status = iota
	Running
	Done
	Skipped
	Failed
)

func (s Status) String() string {
	return [...]string{"pending", "running", "done", "skipped", "failed"}[s]
}

// RowState is a checklist row as the run goes.
type RowState struct {
	Title    string
	Match    []string
	Optional bool
	Dynamic  bool // a "== <step>" line the plan did not list
	Status   Status
	Start    time.Time
	End      time.Time
	Detail   string // the last output line while it runs
}

// Duration of the row so far (or in total once it ended).
func (r *RowState) Duration(now time.Time) time.Duration {
	if r.Start.IsZero() {
		return 0
	}
	if !r.End.IsZero() {
		return r.End.Sub(r.Start)
	}
	return now.Sub(r.Start)
}

// Timing is one row of the "== Timings: <script>" table a script prints at
// exit (lib/common.sh _timings_exit).
type Timing struct {
	Phase  string
	Secs   int
	Failed bool
}

// Group is the checklist of one command.
type Group struct {
	Cmd      Command
	Status   Status
	Rows     []*RowState
	Start    time.Time
	End      time.Time
	ExitCode int
	Timings  []Timing
	Total    int // the Timings table's total, in seconds
	Lines    int
	cur      int
	inTiming bool
}

// NewGroup builds the pending checklist for c.
func NewGroup(c Command) *Group {
	g := &Group{Cmd: c, cur: -1}
	for _, r := range c.Rows {
		g.Rows = append(g.Rows, &RowState{Title: r.Title, Match: r.Match, Optional: r.Optional})
	}
	return g
}

var (
	ansiRe   = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\r`)
	timingRe = regexp.MustCompile(`^\s{3}(.*?)\s+([0-9]+):([0-9]{2})(\s+\(failed\))?$`)
	eksctlRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]{8} \[[^\]]*\]\s+`)
)

// StripANSI removes terminal color codes and carriage returns.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// Marker returns the step text of a "== <step>" line, and whether it is one.
func Marker(line string) (string, bool) {
	s := strings.TrimSpace(StripANSI(line))
	if !strings.HasPrefix(s, "== ") {
		return "", false
	}
	return strings.TrimSpace(s[3:]), true
}

// CleanDetail shortens an output line for the row detail: no colors, no
// eksctl timestamp and level, trimmed.
func CleanDetail(line string) string {
	s := strings.TrimSpace(StripANSI(line))
	s = eksctlRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func (r *RowState) matches(step string) bool {
	for _, m := range r.Match {
		if strings.HasPrefix(step, m) {
			return true
		}
	}
	return false
}

// Begin marks the command started.
func (g *Group) Begin(t time.Time) {
	g.Status = Running
	g.Start = t
}

// Line feeds one output line of the command into the checklist.
func (g *Group) Line(t time.Time, line string) {
	g.Lines++
	step, ok := Marker(line)
	if ok && strings.HasPrefix(step, "Timings: ") {
		g.inTiming = true
		return
	}
	if g.inTiming {
		s := StripANSI(line)
		if m := timingRe.FindStringSubmatch(s); m != nil {
			min, _ := strconv.Atoi(m[2])
			sec, _ := strconv.Atoi(m[3])
			secs := min*60 + sec
			if strings.TrimSpace(m[1]) == "total" {
				g.Total = secs
				g.inTiming = false
				return
			}
			g.Timings = append(g.Timings, Timing{Phase: strings.TrimSpace(m[1]), Secs: secs, Failed: m[4] != ""})
			return
		}
		g.inTiming = false
	}
	if ok {
		g.step(t, step)
		return
	}
	if d := CleanDetail(line); d != "" && g.cur >= 0 && g.Rows[g.cur].Status == Running {
		g.Rows[g.cur].Detail = d
	}
}

// step moves the checklist to the row for a "== <step>" line: the current
// row when it matches again, else the next planned row that matches (passing
// over optional rows only), else a new row right after the current one.
func (g *Group) step(t time.Time, step string) {
	if g.cur >= 0 && g.Rows[g.cur].matches(step) && g.Rows[g.cur].Status == Running {
		g.Rows[g.cur].Detail = ""
		return
	}
	for j := g.cur + 1; j < len(g.Rows); j++ {
		r := g.Rows[j]
		if r.Dynamic || r.Status != Pending {
			continue
		}
		if r.matches(step) {
			for k := g.cur + 1; k < j; k++ {
				if g.Rows[k].Status == Pending {
					g.Rows[k].Status = Skipped
				}
			}
			g.advance(t, j)
			return
		}
		if !r.Optional {
			break
		}
	}
	row := &RowState{Title: step, Dynamic: true}
	at := g.cur + 1
	g.Rows = append(g.Rows, nil)
	copy(g.Rows[at+1:], g.Rows[at:])
	g.Rows[at] = row
	g.advance(t, at)
}

func (g *Group) advance(t time.Time, j int) {
	if g.cur >= 0 && g.Rows[g.cur].Status == Running {
		g.Rows[g.cur].Status = Done
		g.Rows[g.cur].End = t
		g.Rows[g.cur].Detail = ""
	}
	g.cur = j
	g.Rows[j].Status = Running
	g.Rows[j].Start = t
}

// Exit ends the command: on success the running row is done and rows that
// never printed are skipped; on failure the running row (or the command)
// is failed.
func (g *Group) Exit(t time.Time, code int) {
	g.End = t
	g.ExitCode = code
	g.inTiming = false
	if code == 0 {
		g.Status = Done
		for _, r := range g.Rows {
			switch r.Status {
			case Running:
				r.Status, r.End, r.Detail = Done, t, ""
			case Pending:
				r.Status = Skipped
			}
		}
		return
	}
	g.Status = Failed
	if g.cur >= 0 && g.Rows[g.cur].Status == Running {
		g.Rows[g.cur].Status = Failed
		g.Rows[g.cur].End = t
	}
}

// Current is the running row, or nil.
func (g *Group) Current() *RowState {
	if g.cur >= 0 && g.Rows[g.cur].Status == Running {
		return g.Rows[g.cur]
	}
	return nil
}

// Progress counts finished rows (done or skipped) and all rows.
func (g *Group) Progress() (finished, total int) {
	for _, r := range g.Rows {
		if r.Status == Done || r.Status == Skipped {
			finished++
		}
	}
	return finished, len(g.Rows)
}
