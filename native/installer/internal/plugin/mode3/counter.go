package mode3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Counter is one mode 3 counter pool: the counter demo's gVisor pool under
// new names, with the network transport and a microVM RuntimeClass. The
// mode3 and mode3-iso plugins each deploy, check and remove one.
type Counter struct {
	// Namespace is the Kubernetes Namespace and the atespace.
	Namespace string
	// Name is the WorkerPool, the ActorTemplate and their workload label.
	Name string
	// Actor is the actor RoundTrip suspends and resumes; DeleteActor is the
	// one DeleteCheck creates and deletes.
	Actor, DeleteActor string
	// RuntimeClass is what every worker Pod must run under; NodeLabels are
	// labels every worker Pod's node must carry.
	RuntimeClass string
	NodeLabels   map[string]string
	// Pool (Namespace and WorkerPool, with ${WORKER_IMAGE}) and Template
	// (the ActorTemplate, with ${COUNTER_IMAGE}) are the embedded manifests.
	Pool, Template string
}

// DemoFuncs is the counter demo's code the plugins run (internal/demo's
// exports); tests replace Demo.
type DemoFuncs struct {
	CounterImage func(ctx context.Context, w io.Writer, o demo.Options, tree, pool, tag string) (string, error)
	Pin          func(ctx context.Context, o demo.Options, tagged string) (string, error)
	KubectlAte   func(ctx context.Context, w io.Writer, o demo.Options, tree string) (string, error)
	Ate          func(ctx context.Context, o demo.Options, args ...string) ([]byte, error)
	WaitGolden   func(ctx context.Context, w io.Writer, o demo.Options, p demo.Pool) error
	RoundTrip    func(ctx context.Context, w io.Writer, o demo.Options, p demo.Pool) (string, error)
}

// Demo is the real demo code.
var Demo = DemoFuncs{CounterImage: demo.CounterImage, Pin: demo.Pin, KubectlAte: demo.KubectlAte,
	Ate: demo.Ate, WaitGolden: demo.WaitGolden, RoundTrip: demo.RoundTrip}

// Interval paces every poll; tests set it to 0.
var Interval = 2 * time.Second

// DemoOptions are the demo options for the fork's release: its tree, its
// images and its kubectl-ate.
func DemoOptions(o plugin.Options) demo.Options {
	return demo.Options{Cluster: o.Cluster, Region: o.Region, Profile: o.Profile,
		Tag: Tag, TreeRoot: o.TreeRoot, ImageRepo: ImageRepo}
}

// Tree is the fork's tree, which the control plane deploy fetched.
func Tree(o plugin.Options) (string, error) {
	tree := controlplane.TreeDir(o.TreeRoot, Tag)
	if _, err := os.Stat(filepath.Join(tree, "demos", "counter")); err != nil {
		return "", fmt.Errorf("no mode 3 tree (substrate %s) at %s: run --deploy-control-plane --plugins %s first", Tag, tree, ID)
	}
	return tree, nil
}

// fill replaces each ${KEY} in s with vals[KEY], and fails on any left.
func fill(s string, vals map[string]string) (string, error) {
	for k, v := range vals {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	if m := regexp.MustCompile(`\$\{[A-Z_]+\}`).FindString(s); m != "" {
		return "", fmt.Errorf("no value for %s", m)
	}
	return s, nil
}

func (c Counter) pool() demo.Pool {
	return demo.Pool{Atespace: c.Namespace, Template: c.Name, Actor: c.Actor}
}

// Deploy applies the pool with worker as its worker image, creates the
// atespace and the ActorTemplate with counter as its image, then waits for
// the template's golden snapshot. A template that already runs counter is
// kept; one that runs another image is an error naming how to replace it
// (templates are immutable). beforeTemplate, when set, runs before a new
// template is created (mode3-iso waits there for its nodes). Build kubectl-ate first (Demo.KubectlAte).
func (c Counter) Deploy(ctx context.Context, w io.Writer, o plugin.Options, worker, counter string, beforeTemplate func() error) error {
	do := DemoOptions(o)
	pool, err := fill(c.Pool, map[string]string{"WORKER_IMAGE": worker})
	if err != nil {
		return fmt.Errorf("pool %s: %w", c.Name, err)
	}
	if err := plugin.Kubectl(ctx, w, o, []byte(pool), nil, "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := Demo.Ate(ctx, do, "create", "atespace", c.Namespace); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("create atespace %s: %w", c.Namespace, err)
	}
	if out, err := Demo.Ate(ctx, do, "get", "actor-template", c.Name, "-a", c.Namespace, "-o", "json"); err == nil {
		have, err := demo.TemplateImage(out)
		if err != nil {
			return fmt.Errorf("read actor-template %s/%s: %w", c.Namespace, c.Name, err)
		}
		if have != counter {
			return fmt.Errorf("actor-template %s/%s runs %s, not %s; remove the plugin (--plugin-action remove) to replace it", c.Namespace, c.Name, have, counter)
		}
		fmt.Fprintf(w, "ActorTemplate %s/%s already runs this counter image\n", c.Namespace, c.Name)
	} else {
		if beforeTemplate != nil {
			if err := beforeTemplate(); err != nil {
				return err
			}
		}
		tmpl, err := fill(c.Template, map[string]string{"COUNTER_IMAGE": counter})
		if err != nil {
			return fmt.Errorf("template %s: %w", c.Name, err)
		}
		f, err := os.CreateTemp("", "substrate-eks-"+c.Name+"-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(tmpl); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if _, err := Demo.Ate(ctx, do, "create", "actor-template", "-f", f.Name()); err != nil {
			return fmt.Errorf("create actor-template %s/%s: %w", c.Namespace, c.Name, err)
		}
		fmt.Fprintf(w, "Created ActorTemplate %s/%s\n", c.Namespace, c.Name)
	}
	if err := Demo.WaitGolden(ctx, w, do, c.pool()); err != nil {
		return err
	}
	return plugin.Kubectl(ctx, w, o, nil, nil, "-n", c.Namespace, "get", "pods", "-o",
		"custom-columns=POD:.metadata.name,RUNTIME:.spec.runtimeClassName,NODE:.spec.nodeName")
}

// Verify checks the pool: every worker Pod runs under c.RuntimeClass on a
// node with c.NodeLabels; the actor continues its counters across a suspend
// and resume (with its snapshot in S3); and deleting an actor tears its
// sandbox down inside the worker's guest.
func (c Counter) Verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	tree, err := Tree(o)
	if err != nil {
		return err
	}
	do := DemoOptions(o)
	if _, err := Demo.KubectlAte(ctx, w, do, tree); err != nil {
		return err
	}
	var pods, nodes bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &pods, "-n", c.Namespace, "get", "pods", "-l", networkTransportLabel, "-o", "json"); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, nil, &nodes, "get", "nodes", "-o", "json"); err != nil {
		return err
	}
	table, err := CheckWorkers(pods.Bytes(), nodes.Bytes(), c.RuntimeClass, c.NodeLabels)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Namespace, err)
	}
	fmt.Fprint(w, table)
	sum, err := Demo.RoundTrip(ctx, w, do, c.pool())
	if err != nil {
		return fmt.Errorf("%s: %w", c.Actor, err)
	}
	fmt.Fprintf(w, "PASS: %s (%s): %s\n", c.Actor, c.Namespace, sum)
	return c.DeleteCheck(ctx, w, o)
}

// networkTransportLabel marks the worker Pods of a pool with
// spec.networkTransport.
const networkTransportLabel = "ate.dev/worker-transport=network"

// CheckWorkers checks `kubectl get pods -o json` of a pool's network worker
// Pods against `kubectl get nodes -o json`: there is at least one, and every
// one runs under runtimeClass on a node carrying labels. It returns a table
// of the Pods.
func CheckWorkers(podsJSON, nodesJSON []byte, runtimeClass string, labels map[string]string) (string, error) {
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				RuntimeClassName string `json:"runtimeClassName"`
				NodeName         string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(podsJSON, &pods); err != nil {
		return "", fmt.Errorf("read the worker Pods: %w", err)
	}
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return "", fmt.Errorf("read the nodes: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", errors.New("no worker Pod with " + networkTransportLabel)
	}
	nodeLabels := map[string]map[string]string{}
	for _, n := range nodes.Items {
		nodeLabels[n.Metadata.Name] = n.Metadata.Labels
	}
	keys := slices.Sorted(maps.Keys(labels))
	var b strings.Builder
	var bad []string
	for _, p := range pods.Items {
		name, rc, node := p.Metadata.Name, p.Spec.RuntimeClassName, p.Spec.NodeName
		if rc != runtimeClass {
			bad = append(bad, fmt.Sprintf("worker Pod %s runs under %q, not RuntimeClass %s", name, rc, runtimeClass))
		}
		nl, ok := nodeLabels[node]
		if node == "" || !ok {
			bad = append(bad, fmt.Sprintf("worker Pod %s is on no known node (%q)", name, node))
		}
		var shown []string
		for _, k := range keys {
			if nl[k] != labels[k] {
				bad = append(bad, fmt.Sprintf("worker Pod %s is on node %s, whose %s is %q, not %q", name, node, k, nl[k], labels[k]))
			}
			shown = append(shown, k+"="+nl[k])
		}
		fmt.Fprintf(&b, "  worker Pod %s  RuntimeClass %s  node %s (%s)\n", name, rc, node, strings.Join(shown, ", "))
	}
	if len(bad) > 0 {
		return "", errors.New(strings.Join(bad, "; "))
	}
	return b.String(), nil
}

// Actor states in `kubectl ate get actor -o json`.
const (
	stateRunning   = "ACTOR_STATE_RUNNING"
	stateSuspended = "ACTOR_STATE_SUSPENDED"
)

// actorInfo is the part of an actor DeleteCheck reads.
type actorInfo struct {
	Metadata struct {
		UID string `json:"uid"`
	} `json:"metadata"`
	Status struct {
		State            string `json:"state"`
		WorkerAssignment struct {
			WorkerPod string `json:"workerPod"`
		} `json:"workerAssignment"`
	} `json:"status"`
}

func (c Counter) getActor(ctx context.Context, do demo.Options, name string) (actorInfo, error) {
	out, err := Demo.Ate(ctx, do, "get", "actor", name, "-a", c.Namespace, "-o", "json")
	if err != nil {
		return actorInfo{}, err
	}
	var a actorInfo
	if err := json.Unmarshal(out, &a); err != nil {
		return actorInfo{}, fmt.Errorf("read actor %s: %w", name, err)
	}
	return a, nil
}

func poll(ctx context.Context, tries int, f func() bool) bool {
	for i := 0; i < tries; i++ {
		if f() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(Interval):
		}
	}
	return false
}

// Poll bounds at the 2s interval: an actor starts within 3 minutes, and its
// worker logs the teardown within a minute (30 reads, 2s apart).
const (
	startTries    = 90
	teardownTries = 30
)

// DeleteCheck checks that deleting an actor tears its sandbox down inside
// the worker's guest, so the worker Pod logs a TerminateWorkload call for the actor's UID.
func (c Counter) DeleteCheck(ctx context.Context, w io.Writer, o plugin.Options) error {
	do := DemoOptions(o)
	name := c.DeleteActor
	fmt.Fprintf(w, "%s (%s): delete tears down the sandbox in the guest\n", name, c.Namespace)
	if _, err := c.getActor(ctx, do, name); err != nil {
		if _, cerr := Demo.Ate(ctx, do, "create", "actor", name, "-a", c.Namespace, "--template", c.Name); cerr != nil {
			return fmt.Errorf("create actor %s: %w (get: %v)", name, cerr, err)
		}
	}
	var a actorInfo
	var err error
	resumed := false
	if !poll(ctx, startTries, func() bool {
		a, err = c.getActor(ctx, do, name)
		if err == nil && a.Status.State == stateSuspended && !resumed {
			// A suspended actor runs again on resume, as on a request.
			_, err = Demo.Ate(ctx, do, "resume", "actor", name, "-a", c.Namespace)
			resumed = true
		}
		return err == nil && a.Status.State == stateRunning && a.Status.WorkerAssignment.WorkerPod != ""
	}) {
		if err != nil {
			return fmt.Errorf("actor %s did not start: %w", name, err)
		}
		return fmt.Errorf("actor %s is %s on worker Pod %q, not running on one", name, a.Status.State, a.Status.WorkerAssignment.WorkerPod)
	}
	uid, pod := a.Metadata.UID, a.Status.WorkerAssignment.WorkerPod
	if uid == "" {
		return fmt.Errorf("actor %s has no UID", name)
	}
	if _, err := Demo.Ate(ctx, do, "delete", "actor", name, "-a", c.Namespace); err != nil {
		return fmt.Errorf("delete actor %s: %w", name, err)
	}
	var logs bytes.Buffer
	if !poll(ctx, teardownTries, func() bool {
		logs.Reset()
		if plugin.Kubectl(ctx, io.Discard, o, nil, &logs, "-n", c.Namespace, "logs", pod, "--all-containers") != nil {
			return false
		}
		return LogsTerminate(logs.String(), uid)
	}) {
		return fmt.Errorf("worker Pod %s logged no TerminateWorkload for %s (%s)", pod, name, uid)
	}
	fmt.Fprintf(w, "PASS: the worker in Pod %s tore down %s's sandbox\n", pod, name)
	return nil
}

// LogsTerminate reports whether a worker's logs hold a line that handles the
// TerminateWorkload RPC for uid: all three on one line.
func LogsTerminate(logs, uid string) bool {
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "Handle RPC") && strings.Contains(l, "TerminateWorkload") && strings.Contains(l, uid) {
			return true
		}
	}
	return false
}

// Remove deletes what Deploy and Verify made: the actors, the ActorTemplate
// (the server deletes its golden actor and snapshot with it) and the
// atespace through the ate API, then the Namespace, which takes the
// WorkerPool and its worker Pods. A pool the control plane no longer serves
// (no ate-system) has nothing left in the ate API.
func (c Counter) Remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if ok, err := served(ctx, w, o); err != nil {
		return err
	} else if ok {
		if err := c.removeAte(ctx, w, o); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "Deleting Namespace "+c.Namespace)
	return plugin.Kubectl(ctx, w, o, nil, nil, "delete", "namespace", c.Namespace, "--ignore-not-found", "--wait=true", "--timeout=10m")
}

// served reports whether the control plane runs (namespace ate-system).
func served(ctx context.Context, w io.Writer, o plugin.Options) (bool, error) {
	var out bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &out, "get", "namespace", "ate-system", "--ignore-not-found", "-o", "name"); err != nil {
		return false, err
	}
	return strings.TrimSpace(out.String()) != "", nil
}

// notFound is how the ate API answers for an object that is not there.
func notFound(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") || strings.Contains(s, "notfound")
}

// atespaceTries bounds the wait for the actors' deletion to finish, at the
// 2s interval: an atespace with actors left cannot be deleted.
const atespaceTries = 60

func (c Counter) removeAte(ctx context.Context, w io.Writer, o plugin.Options) error {
	tree, err := Tree(o)
	if err != nil {
		return err
	}
	do := DemoOptions(o)
	if _, err := Demo.KubectlAte(ctx, w, do, tree); err != nil {
		return err
	}
	for _, a := range []string{c.Actor, c.DeleteActor} {
		if _, err := Demo.Ate(ctx, do, "delete", "actor", a, "-a", c.Namespace); err != nil && !notFound(err) {
			return fmt.Errorf("delete actor %s: %w", a, err)
		}
	}
	if _, err := Demo.Ate(ctx, do, "delete", "actor-template", c.Name, "-a", c.Namespace); err != nil && !notFound(err) {
		return fmt.Errorf("delete actor-template %s/%s: %w", c.Namespace, c.Name, err)
	}
	var last error
	if !poll(ctx, atespaceTries, func() bool {
		_, last = Demo.Ate(ctx, do, "delete", "atespace", c.Namespace)
		return last == nil || notFound(last)
	}) {
		return fmt.Errorf("delete atespace %s: %w", c.Namespace, last)
	}
	fmt.Fprintf(w, "Deleted the actors, ActorTemplate and atespace %s\n", c.Namespace)
	return nil
}
