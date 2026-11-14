package efs

// Counter actors on per-actor volumes: the templates the efs and s3files
// plugins add to the counter demo, and the check that an actor's file
// counter lives on its own access point, also after the actor moves to
// another node.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Class is one sandbox class of the counter demo (installer --deploy-demo):
// its atespace and the ActorTemplate the volume templates are made from.
type Class struct {
	Name, Atespace, Counter string
}

// Classes are the counter demo's classes.
var Classes = []Class{
	{Name: "gvisor", Atespace: "ate-demo-counter", Counter: "counter"},
	{Name: "microvm", Atespace: "ate-demo-counter-microvm", Counter: "counter-microvm"},
}

// counterTemplate is what VolumeTemplate keeps of the counter's template.
type counterTemplate struct {
	WorkerSelector json.RawMessage `json:"workerSelector"`
	Containers     json.RawMessage `json:"containers"`
	Resources      json.RawMessage `json:"resources"`
	SnapshotConfig json.RawMessage `json:"snapshotConfig"`
}

// VolumeTemplate is the counter demo's template t (`kubectl ate get
// actor-template -o json`, with or without a spec wrapper) as template name,
// its data volume a new volume of StorageClass sc per actor: same image,
// command, pool and size, nothing of its status.
func VolumeTemplate(t []byte, atespace, name, sc string) ([]byte, error) {
	var wrapped struct {
		Spec *counterTemplate `json:"spec"`
	}
	var top counterTemplate
	if err := json.Unmarshal(t, &wrapped); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(t, &top); err != nil {
		return nil, err
	}
	src := top
	if wrapped.Spec != nil && len(wrapped.Spec.Containers) > 0 {
		src = *wrapped.Spec
	}
	if len(src.Containers) == 0 || len(src.WorkerSelector) == 0 {
		return nil, fmt.Errorf("the counter template has no containers or worker selector")
	}
	var containers []map[string]any
	if err := json.Unmarshal(src.Containers, &containers); err != nil || len(containers) == 0 {
		return nil, fmt.Errorf("the counter template's containers: %v", err)
	}
	mounts, _ := containers[0]["volumeMounts"].([]any)
	if !slices.ContainsFunc(mounts, func(m any) bool { mm, _ := m.(map[string]any); return mm["name"] == "data" }) {
		return nil, fmt.Errorf("the counter template mounts no data volume")
	}
	out := map[string]any{
		"metadata":       map[string]string{"atespace": atespace, "name": name},
		"workerSelector": src.WorkerSelector,
		"containers":     containers,
		"volumes": []any{map[string]any{"name": "data", "external": map[string]any{
			"template": map[string]any{"capacityBytes": 1 << 30, "storageClassName": sc}}}},
	}
	if len(src.Resources) > 0 {
		out["resources"] = src.Resources
	}
	if len(src.SnapshotConfig) > 0 {
		out["snapshotConfig"] = src.SnapshotConfig
	}
	return json.Marshal(out)
}

// CreateTemplates makes template name, on StorageClass sc, in each counter
// class's atespace that has the counter demo, keeping one that exists
// (templates are immutable). It fails when the counter demo is not deployed.
func CreateTemplates(ctx context.Context, w io.Writer, k Ate, name, sc string) error {
	made := 0
	for _, c := range Classes {
		if _, err := k.Do(ctx, nil, "get", "actor-template", name, "-a", c.Atespace, "-o", "json"); err == nil {
			fmt.Fprintf(w, "ActorTemplate %s/%s exists\n", c.Atespace, name)
			made++
			continue
		}
		t, err := k.Do(ctx, nil, "get", "actor-template", c.Counter, "-a", c.Atespace, "-o", "json")
		if err != nil {
			fmt.Fprintf(w, "No counter demo on %s (atespace %s); skipping it\n", c.Name, c.Atespace)
			continue
		}
		vt, err := VolumeTemplate(t, c.Atespace, name, sc)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", c.Atespace, c.Counter, err)
		}
		if _, err := k.Do(ctx, vt, "--preview", "create", "actor-template", "-f", "-"); err != nil {
			return fmt.Errorf("create actor-template %s/%s: %w", c.Atespace, name, err)
		}
		fmt.Fprintf(w, "Created ActorTemplate %s/%s (data volume on StorageClass %s)\n", c.Atespace, name, sc)
		made++
	}
	if made == 0 {
		return fmt.Errorf("no counter demo to add %s to: deploy it first (--deploy-demo)", name)
	}
	return nil
}

// DeleteTemplates deletes template name and its actors in every counter
// atespace; deleting an actor deletes its volume, and with it its access
// point.
func DeleteTemplates(ctx context.Context, w io.Writer, k Ate, name string) error {
	for _, c := range Classes {
		if _, err := k.Do(ctx, nil, "get", "actor-template", name, "-a", c.Atespace, "-o", "json"); err != nil {
			continue // not there (or no atespace)
		}
		out, err := k.Do(ctx, nil, "get", "actors", "-a", c.Atespace, "-o", "json")
		if err != nil {
			return fmt.Errorf("list actors of %s: %w", c.Atespace, err)
		}
		var list struct {
			Actors []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				ActorTemplate struct {
					Name string `json:"name"`
				} `json:"actorTemplate"`
			} `json:"actors"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return fmt.Errorf("read actors of %s: %w", c.Atespace, err)
		}
		for _, a := range list.Actors {
			if a.ActorTemplate.Name != name {
				continue
			}
			if _, err := k.Do(ctx, nil, "delete", "actor", a.Metadata.Name, "-a", c.Atespace); err != nil {
				return fmt.Errorf("delete actor %s/%s: %w", c.Atespace, a.Metadata.Name, err)
			}
			fmt.Fprintf(w, "Deleted actor %s/%s\n", c.Atespace, a.Metadata.Name)
		}
		if _, err := k.Do(ctx, nil, "delete", "actor-template", name, "-a", c.Atespace); err != nil {
			return fmt.Errorf("delete actor-template %s/%s: %w", c.Atespace, name, err)
		}
		fmt.Fprintf(w, "Deleted ActorTemplate %s/%s\n", c.Atespace, name)
	}
	return nil
}

// Storage is one kind of per-actor volume the check runs on.
type Storage struct {
	// Name is "efs" or "s3files", the actors' name prefix; Template is the
	// ActorTemplate on its StorageClass.
	Name, Template string
	// AccessPoints lists the file system's access point ids.
	AccessPoints func(ctx context.Context) ([]string, error)
}

// actorState is the part of `kubectl ate get actor -o json` the check reads.
type actorState struct {
	Metadata struct {
		UID string `json:"uid"`
	} `json:"metadata"`
	Status struct {
		State           string `json:"state"`
		AssignedNode    string `json:"assignedNode"`
		ExternalVolumes struct {
			Volumes []struct {
				StorageVolumeID string `json:"storageVolumeId"`
			} `json:"volumes"`
		} `json:"externalVolumes"`
		Snapshots []struct {
			Generation      int    `json:"generation"`
			Owner           string `json:"owner"`
			DurableSnapshot struct {
				State string `json:"state"`
			} `json:"durableSnapshot"`
		} `json:"snapshots"`
	} `json:"status"`
}

func getActor(ctx context.Context, k Ate, name, atespace string) (actorState, error) {
	var a actorState
	out, err := k.Do(ctx, nil, "get", "actor", name, "-a", atespace, "-o", "json")
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(out, &a)
}

// accessPoint is the access point of the actor's volume: a CSI volume id
// ends with it, after "::".
func (a actorState) accessPoint() string {
	if len(a.Status.ExternalVolumes.Volumes) == 0 {
		return ""
	}
	id := a.Status.ExternalVolumes.Volumes[0].StorageVolumeID
	if i := strings.LastIndex(id, "::"); i >= 0 {
		return id[i+2:]
	}
	return ""
}

// completedSnapshot is the generation of the actor's newest own snapshot in
// S3, or -1.
func (a actorState) completedSnapshot() int {
	g := -1
	for _, s := range a.Status.Snapshots {
		if s.Owner == "SNAPSHOT_OWNER_ACTOR" && s.DurableSnapshot.State == "SNAPSHOT_STORAGE_STATE_COMPLETED" && s.Generation > g {
			g = s.Generation
		}
	}
	return g
}

var fileCounter = regexp.MustCompile(`preserved file counter: (\d+)`)

// FileCounter reads the counter demo's file counter from its answer.
func FileCounter(body string) (int, error) {
	m := fileCounter.FindStringSubmatch(body)
	if m == nil {
		return 0, fmt.Errorf("no file counter in the counter's answer %q", strings.TrimSpace(body))
	}
	return strconv.Atoi(m[1])
}

// Get is an HTTP GET of url; a non-200 answer is an error. Tests replace it.
var Get = func(ctx context.Context, url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// FreePort is a free localhost port for a proxy or port-forward. Tests
// replace it.
var FreePort = func() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func readCounter(ctx context.Context, base string, timeout time.Duration) (int, error) {
	body, err := Get(ctx, base, timeout)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", base, err)
	}
	return FileCounter(body)
}

// RoundTrip checks one class: an actor on s gets its own access
// point; its file counter, which lives on that volume, continues across
// suspend and resume; deleting the actor deletes the access point. With
// move it also checks a move: after the suspend reaches S3, the actor's
// node is cordoned and its pool's workers there deleted, so the resume lands
// on another node, which mounts the same access point. The node is
// uncordoned at the end, also on failure.
func RoundTrip(ctx context.Context, w io.Writer, o plugin.Options, k Ate, s Storage, c Class, move bool) error {
	name := s.Name + "-" + c.Name
	if _, err := k.Do(ctx, nil, "get", "actor-template", s.Template, "-a", c.Atespace, "-o", "json"); err != nil {
		fmt.Fprintf(w, "No %s/%s; skipping %s\n", c.Atespace, s.Template, c.Name)
		return nil
	}
	fmt.Fprintf(w, "%s: actor %s in %s\n", c.Name, name, c.Atespace)
	a, err := getActor(ctx, k, name, c.Atespace)
	if err != nil {
		if _, err := k.Do(ctx, nil, "--preview", "create", "actor", name, "-a", c.Atespace, "--template", s.Template); err != nil {
			return fmt.Errorf("create actor %s: %w", name, err)
		}
	} else if a.Status.State == "ACTOR_STATE_CRASHED" {
		// A reinstall or a deleted worker crashes a running actor.
		if _, err := k.Do(ctx, nil, "revert", "actor", name, "-a", c.Atespace); err != nil {
			return fmt.Errorf("actor %s is CRASHED and does not revert: %w", name, err)
		}
	}
	port, err := FreePort()
	if err != nil {
		return err
	}
	stop, logs := k.Start(ctx, "proxy", "actor", name, "-a", c.Atespace, fmt.Sprintf("%d:80", port))
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	// The first request resumes the actor, which creates and mounts its volume.
	if !Poll(ctx, 90, func() bool { _, err := Get(ctx, base+"readyz", 10*time.Second); return err == nil }) {
		return fmt.Errorf("actor %s did not answer through kubectl ate proxy: %s", name, logs())
	}
	first, err := readCounter(ctx, base, 30*time.Second)
	if err != nil {
		return err
	}
	before, err := readCounter(ctx, base, 30*time.Second)
	if err != nil {
		return err
	}
	if before != first+1 {
		return fmt.Errorf("the file counter went from %d to %d, not up by one", first, before)
	}
	if a, err = getActor(ctx, k, name, c.Atespace); err != nil {
		return err
	}
	ap := a.accessPoint()
	if ap == "" {
		return fmt.Errorf("actor %s has no external volume", name)
	}
	aps, err := s.AccessPoints(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(aps, ap) {
		return fmt.Errorf("access point %s of actor %s is not on the file system (%v)", ap, name, aps)
	}
	fmt.Fprintf(w, "  volume on access point %s; file counter %d\n", ap, before)
	nodeA, gen := a.Status.AssignedNode, a.completedSnapshot()

	if _, err := k.Do(ctx, nil, "suspend", "actor", name, "-a", c.Atespace); err != nil {
		return fmt.Errorf("suspend actor %s: %w", name, err)
	}
	if !Poll(ctx, 60, func() bool {
		a, err = getActor(ctx, k, name, c.Atespace)
		return err == nil && a.Status.State == "ACTOR_STATE_SUSPENDED"
	}) {
		return fmt.Errorf("actor %s did not suspend (%s)", name, a.Status.State)
	}
	fmt.Fprintln(w, "  suspended")
	if move {
		undo, err := moveOff(ctx, w, o, k, name, c, nodeA, gen)
		defer undo()
		if err != nil {
			return err
		}
	}
	after, err := readCounter(ctx, base, 180*time.Second)
	if err != nil {
		return err
	}
	if a, err = getActor(ctx, k, name, c.Atespace); err != nil || a.Status.State != "ACTOR_STATE_RUNNING" {
		return fmt.Errorf("actor %s did not resume (%s, %v)", name, a.Status.State, err)
	}
	if after != before+1 {
		return fmt.Errorf("actor %s lost its file counter on %s: %d after %d", name, s.Name, after, before)
	}
	if move {
		if a.Status.AssignedNode == nodeA {
			return fmt.Errorf("actor %s resumed on node %s again", name, nodeA)
		}
		fmt.Fprintf(w, "  PASS: resumed on node %s, not %s; the file counter on %s continued (%d)\n", a.Status.AssignedNode, nodeA, s.Name, after)
	} else {
		fmt.Fprintf(w, "  PASS: the file counter on %s continued across suspend and resume (%d)\n", s.Name, after)
	}
	stop()
	if _, err := k.Do(ctx, nil, "delete", "actor", name, "-a", c.Atespace); err != nil {
		return fmt.Errorf("delete actor %s: %w", name, err)
	}
	var left []string
	if !Poll(ctx, 60, func() bool { left, err = s.AccessPoints(ctx); return err == nil && !slices.Contains(left, ap) }) {
		return fmt.Errorf("access point %s is still there after actor %s was deleted (%v)", ap, name, err)
	}
	fmt.Fprintf(w, "  PASS: deleting the actor deleted access point %s\n", ap)
	return nil
}

// moveOff makes the next resume of the suspended actor land on a node other
// than nodeA: once the suspend's snapshot is in S3, it cordons nodeA and
// deletes the actor pool's workers there, then waits for the pool. The
// returned undo uncordons nodeA.
func moveOff(ctx context.Context, w io.Writer, o plugin.Options, k Ate, name string, c Class, nodeA string, gen int) (func(), error) {
	undo := func() {}
	if nodeA == "" {
		return undo, fmt.Errorf("actor %s has no assigned node", name)
	}
	// atelet uploads the snapshot in the background, minutes after the
	// suspend; a resume elsewhere reads it from S3.
	var a actorState
	var err error
	if !Poll(ctx, 450, func() bool {
		a, err = getActor(ctx, k, name, c.Atespace)
		return err == nil && a.completedSnapshot() > gen
	}) {
		return undo, fmt.Errorf("actor %s's snapshot did not reach S3 (%v)", name, err)
	}
	fmt.Fprintln(w, "  snapshot in S3")
	pool, err := Out(ctx, o, "-n", c.Atespace, "get", "workerpools", "-o", "jsonpath={.items[0].metadata.name} {.items[0].spec.replicas}")
	if err != nil {
		return undo, err
	}
	f := strings.Fields(pool)
	if len(f) != 2 {
		return undo, fmt.Errorf("no worker pool in %s (%q)", c.Atespace, pool)
	}
	if err := Run(ctx, w, o, "cordon", nodeA); err != nil {
		return undo, err
	}
	undo = func() {
		// A fresh context: the check's may be done.
		uctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := Run(uctx, w, o, "uncordon", nodeA); err != nil {
			fmt.Fprintf(w, "WARNING: uncordon %s failed: %v; run kubectl uncordon %s\n", nodeA, err, nodeA)
		}
	}
	if err := Run(ctx, w, o, "-n", c.Atespace, "delete", "pod", "-l", "ate.dev/worker-pool="+f[0],
		"--field-selector", "spec.nodeName="+nodeA, "--wait=true"); err != nil {
		return undo, err
	}
	if err := Run(ctx, w, o, "-n", c.Atespace, "wait", "workerpool", f[0], "--for=jsonpath={.status.readyReplicas}="+f[1], "--timeout=5m"); err != nil {
		return undo, err
	}
	fmt.Fprintf(w, "  node %s cordoned; its %s workers moved\n", nodeA, f[0])
	return undo, nil
}
