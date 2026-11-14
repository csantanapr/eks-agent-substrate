package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/execx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// holdRunner holds the first start of hold open until its context ends,
// then closes the channel with no final event, as execx.Real does once its
// context is cancelled. Every other start is the recorder's.
type holdRunner struct {
	*recorder
	hold string
	held bool
}

func (h *holdRunner) Start(ctx context.Context, spec execx.Spec) <-chan execx.Event {
	if spec.Label != h.hold || h.held {
		return h.recorder.Start(ctx, spec)
	}
	h.held = true
	h.mu.Lock()
	h.started = append(h.started, spec.Label)
	h.mu.Unlock()
	ch := make(chan execx.Event, 1)
	ch <- execx.Event{Line: "Step 1/4: Checking the EKS cluster..."}
	go func() { <-ctx.Done(); close(ch) }()
	return ch
}

// Review F10: ctrl+c on a running step asks first, then stops the process
// tree and the step, which is marked stopped (not done) and rerun by enter;
// n keeps it running. Off a running step, ctrl+c quits.
func TestCtrlCStopsARunningStep(t *testing.T) {
	r := &recorder{}
	h := &holdRunner{recorder: r, hold: "bootstrap"}
	d := testDeps(r)
	d.Runner = h
	stops := 0
	d.StopTree = func() { stops++ }
	m := toBootstrap(t, New(d, newSetup("new")))
	next, cmd := m.Update(key(startKey))
	m = next.(Model)
	next, pending := m.Update(cmd()) // the first line; pending waits for more
	m = next.(Model)
	if m.runningTask() == nil || !strings.Contains(m.View(), "ctrl+c stop this step") {
		t.Fatalf("no running step:\n%s", m.View())
	}
	m = press(m, "ctrl+c")
	if !m.stopAsk || m.quitting || !strings.Contains(m.View(), "Stop bootstrap?") {
		t.Fatalf("ctrl+c did not ask:\n%s", m.View())
	}
	m = press(m, "n")
	if m.stopAsk || stops != 0 || m.runningTask() == nil {
		t.Fatal("n did not keep the step running")
	}
	m = press(m, "ctrl+c")
	next, stop := m.Update(key("y"))
	m = next.(Model)
	if stop == nil || !m.tasks[state.Provision].stopped || !strings.Contains(m.View(), "Stopping") {
		t.Fatal("y did not stop the step")
	}
	// Review R7: ctrl+c while the stop is still ending the tree neither
	// quits nor asks again, and says the stop is in progress.
	m = press(m, "ctrl+c")
	if m.quitting || m.stopAsk || !strings.Contains(m.View(), "stopping…") {
		t.Fatalf("ctrl+c during the stop: quitting %v, asking %v:\n%s", m.quitting, m.stopAsk, m.View())
	}
	stop()
	next, _ = m.Update(pending()) // the runner's channel closes
	m = next.(Model)
	tk := m.tasks[state.Provision]
	if stops != 1 || !tk.done || !errors.Is(tk.err, errStopped) || !strings.Contains(m.View(), "Stopped with ctrl+c") || m.canAdvance() {
		t.Fatalf("stops %d, done %v, err %v:\n%s", stops, tk.done, tk.err, m.View())
	}
	m = press(m, "enter")
	if r.count("bootstrap") != 2 || !m.tasks[state.Provision].done || m.tasks[state.Provision].err != nil {
		t.Fatalf("enter did not rerun it: ran %q", r.ran())
	}
	m = toStep(t, m, state.Complete)
	if m = press(m, "ctrl+c"); !m.quitting {
		t.Error("ctrl+c with nothing running did not quit")
	}
}
