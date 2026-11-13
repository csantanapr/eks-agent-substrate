// Package demo deploys the counter demo and checks it, the native form of
// eks-talk's demos/counter/deploy.sh and verify.sh, and the EKS counterpart of
// substrate-gke's Demo step (make run-demo).
//
// Deploy does what deploy.sh does. Releases publish the worker images but not
// the counter's, so `ate-setup deploy demo --image-repo` cannot install it:
// Deploy builds the counter image from the cached Substrate tree and pushes
// it to the private ECR repository <cluster>/counter (image.go), renders
// upstream's WorkerPool and ActorTemplate templates with that image and the
// release worker images, each pinned by digest, and applies them with
// kubectl and kubectl-ate. Verify suspends and resumes one actor per sandbox
// class through `kubectl ate proxy`, checks that every counter continues, and
// finds the snapshot in S3. Every child process is started from an argv;
// nothing runs a shell.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
)

// Lines Deploy and Verify print for the wizard's checklist.
const (
	DeployLine     = "Deploying the counter demo on "
	VerifyLine     = "Checking suspend and resume on "
	VerifyPassLine = "PASS: "
)

// Options are the install answers the demo needs.
type Options struct {
	Cluster, Region, Profile string
	// Tag is the Substrate release. TreeRoot is where fetched trees are
	// cached (the control plane's --tree-root); controlplane.TreeDir(TreeRoot,
	// Tag) holds demos/counter.
	Tag, TreeRoot string
	// ImageRepo is where the release images come from.
	ImageRepo string
	// SandboxClasses are the classes to deploy and check, e.g. "gvisor,microvm".
	SandboxClasses string
}

// PoolLabel is the node label of the gvisor and microvm node groups, the only
// nodes atelet runs on (deploy/eks pins it there).
const PoolLabel = "ate.dev/pool"

// class is one sandbox class's counter demo, as upstream's ate-setup names it.
type class struct {
	name     string // the sandbox class, and the node group's PoolLabel value
	demo     string // upstream's demo name (`ate-setup deploy demo <demo>`)
	atespace string
	template string
	pool     string // the Namespace and WorkerPool template, in the tree
	tmplFile string // the ActorTemplate template, in the tree
	actor    string // the actor Verify suspends and resumes
}

var classes = map[string]class{
	"gvisor": {name: "gvisor", demo: "counter", atespace: "ate-demo-counter", template: "counter",
		pool: "demos/counter/counter.yaml.tmpl", tmplFile: "demos/counter/counter-template.yaml.tmpl", actor: "demo-gvisor"},
	"microvm": {name: "microvm", demo: "counter-microvm", atespace: "ate-demo-counter-microvm", template: "counter-microvm",
		pool: "demos/counter/counter-microvm.yaml.tmpl", tmplFile: "demos/counter/counter-microvm-template.yaml.tmpl", actor: "demo-microvm"},
}

// workerPkg is the ko package of a class's worker image, published by
// releases as <image repo>/ateworker-<class>.
func (c class) workerPkg() string { return modulePath + "/cmd/ateworker-" + c.name }

func parseClasses(s string) ([]class, error) {
	var out []class
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		c, ok := classes[n]
		if !ok {
			return nil, fmt.Errorf("no counter demo for sandbox class %q (want gvisor or microvm)", n)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no sandbox class to run the counter demo on")
	}
	return out, nil
}

func (o Options) check() error {
	if o.Cluster == "" || o.Region == "" || o.Tag == "" || o.TreeRoot == "" || o.ImageRepo == "" {
		return errors.New("cluster, region, tag, tree root and image repo are required")
	}
	return nil
}

// treeDir is the cached Substrate tree the control plane step fetched.
func treeDir(o Options) (string, error) {
	tree := controlplane.TreeDir(o.TreeRoot, o.Tag)
	if _, err := os.Stat(filepath.Join(tree, "demos", "counter")); err != nil {
		return "", fmt.Errorf("no substrate %s tree with demos/counter at %s; turn on Substrate first", o.Tag, tree)
	}
	return tree, nil
}

// goEnv builds from the tree's vendored modules with the local toolchain, as
// the control plane step does: nothing is downloaded.
var goEnv = []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=vendor", "CGO_ENABLED=0"}

// Deploy deploys the counter demo on every class in o.SandboxClasses.
func Deploy(ctx context.Context, w io.Writer, o Options) error {
	return newReal(o).deploy(ctx, w, o)
}

// Verify proves, on every class, that suspend and resume continue the
// memory, file and rootfs counters and that a snapshot exists in S3.
func Verify(ctx context.Context, w io.Writer, o Options) error {
	return newReal(o).verify(ctx, w, o)
}

// env is everything Deploy and Verify reach outside the process; tests
// replace it with fakes.
type env struct {
	// run starts argv in dir and streams its output to w.
	run func(ctx context.Context, w io.Writer, dir string, extra []string, argv ...string) error
	// output runs argv and returns its stdout; stderr goes into the error.
	output func(ctx context.Context, extra []string, argv ...string) ([]byte, error)
	// start starts a long-running argv (the proxy) and returns how to stop it.
	start func(ctx context.Context, w io.Writer, extra []string, argv ...string) (stop func(), err error)
	// get is an HTTP GET of url; a non-200 answer is an error.
	get func(ctx context.Context, url string, timeout time.Duration) (string, error)
	// objects counts the objects under prefix in bucket.
	objects func(ctx context.Context, bucket, prefix string) (int, error)
	// account is the caller's AWS account id.
	account func(ctx context.Context) (string, error)
	// kubeconfig returns the installer's kubeconfig for the cluster.
	kubeconfig func(ctx context.Context) (string, error)
	// freePort is a free localhost port for the proxy.
	freePort func() (int, error)
	// pushCounter builds the counter image from binary on base for arch,
	// pushes it to repo:tag in the caller's ECR registry and returns the
	// reference to install it by (pushCounter in image.go).
	pushCounter func(ctx context.Context, binary, base, arch, repo, tag string) (string, error)
	// pin is a public image's tagged reference with its digest appended.
	pin func(ctx context.Context, tagged string) (string, error)
	// kubectlAte is where kubectl-ate is built.
	kubectlAte string
	// interval paces every poll.
	interval time.Duration
}

func (e *env) deploy(ctx context.Context, w io.Writer, o Options) error {
	if err := o.check(); err != nil {
		return err
	}
	cls, err := parseClasses(o.SandboxClasses)
	if err != nil {
		return err
	}
	tree, err := treeDir(o)
	if err != nil {
		return err
	}
	kc, err := e.kubeconfig(ctx)
	if err != nil {
		return err
	}
	kubectl := func(args ...string) []string {
		return append([]string{"kubectl", "--kubeconfig", kc, "--context", o.Cluster}, args...)
	}
	dir, err := os.MkdirTemp("", "substrate-eks-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	k := ate{e: e, bin: e.kubectlAte, kubeconfig: kc, context: o.Cluster}
	var images map[string]string
	for i, c := range cls {
		fmt.Fprintln(w, DeployLine+c.name)
		if i == 0 {
			if images, err = e.demoImages(ctx, w, o, tree, dir, kubectl, cls); err != nil {
				return err
			}
			if err := e.run(ctx, w, tree, goEnv, "go", "build", "-o", e.kubectlAte, "./cmd/kubectl-ate"); err != nil {
				return fmt.Errorf("build kubectl-ate: %w", err)
			}
		}
		if err := e.deployClass(ctx, w, k, kubectl, tree, dir, c, images); err != nil {
			return fmt.Errorf("deploy demo %s: %w", c.demo, err)
		}
	}
	return nil
}

// demoImages builds and pushes the counter image and pins every class's
// release worker image, keyed by the ko package the templates name.
func (e *env) demoImages(ctx context.Context, w io.Writer, o Options, tree, dir string, kubectl func(...string) []string, cls []class) (map[string]string, error) {
	arch, err := e.nodeArch(ctx, kubectl, cls)
	if err != nil {
		return nil, err
	}
	base, err := counterBase(tree)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "Building the counter image for linux/%s on %s\n", arch, base)
	bin := filepath.Join(dir, "counter")
	buildEnv := append(append([]string{}, goEnv...), "GOOS=linux", "GOARCH="+arch)
	if err := e.run(ctx, w, tree, buildEnv, "go", "build", "-trimpath", "-o", bin, "./demos/counter"); err != nil {
		return nil, fmt.Errorf("build the counter: %w", err)
	}
	counter, err := e.pushCounter(ctx, bin, base, arch, o.Cluster+"/counter", o.Tag)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(w, "Pushed "+counter)
	images := map[string]string{counterPkg: counter}
	for _, c := range cls {
		ref, err := e.pin(ctx, o.ImageRepo+"/ateworker-"+c.name+":"+o.Tag)
		if err != nil {
			return nil, err
		}
		images[c.workerPkg()] = ref
	}
	return images, nil
}

// nodeArch is the CPU architecture of the classes' node groups, which the
// counter image is built for.
func (e *env) nodeArch(ctx context.Context, kubectl func(...string) []string, cls []class) (string, error) {
	names := make([]string, len(cls))
	for i, c := range cls {
		names[i] = c.name
	}
	sel := PoolLabel + " in (" + strings.Join(names, ",") + ")"
	out, err := e.output(ctx, nil, kubectl("get", "nodes", "-l", sel, "-o", `jsonpath={.items[*].metadata.labels.kubernetes\.io/arch}`)...)
	if err != nil {
		return "", err
	}
	archs := slices.Compact(slices.Sorted(slices.Values(strings.Fields(string(out)))))
	switch len(archs) {
	case 0:
		return "", fmt.Errorf("no node labeled %s to run the counter demo on", sel)
	case 1:
		return archs[0], nil
	}
	// shortcut: one single-architecture image, as deploy.sh builds; push an
	// index if the node groups ever need to differ.
	return "", fmt.Errorf("the nodes labeled %s are %s: the counter image is built for one architecture", sel, strings.Join(archs, " and "))
}

// deployClass is deploy.sh's deploy for one class: the Namespace and the
// WorkerPool pinned to the class's node group, the atespace, the
// ActorTemplate, then its golden snapshot.
func (e *env) deployClass(ctx context.Context, w io.Writer, k ate, kubectl func(...string) []string, tree, dir string, c class, images map[string]string) error {
	// Upstream's WorkerPools carry no nodeSelector. On EKS an unpinned
	// gVisor pool can land on a node with no atelet (an Auto Mode or system
	// node) and never come up, so pin it to its node group, as deploy.sh does.
	changed, err := PinPool(filepath.Join(tree, c.pool), c.name)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(w, "Pinned WorkerPool %s to nodes labeled %s=%s\n", c.demo, PoolLabel, c.name)
	}
	pool, err := writeRendered(filepath.Join(tree, c.pool), filepath.Join(dir, c.demo+"-pool.yaml"), images)
	if err != nil {
		return err
	}
	if err := e.run(ctx, w, "", nil, kubectl("apply", "-f", pool)...); err != nil {
		return err
	}
	if _, err := k.do(ctx, "create", "atespace", c.atespace); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("create atespace %s: %w", c.atespace, err)
	}
	want := images[counterPkg]
	if out, err := k.do(ctx, "get", "actor-template", c.template, "-a", c.atespace, "-o", "json"); err == nil {
		// Templates are immutable: keep one that runs this image, and stop
		// on one that runs another rather than install a mixed demo.
		have, err := templateImage(out)
		if err != nil {
			return fmt.Errorf("read actor-template %s/%s: %w", c.atespace, c.template, err)
		}
		if have != want {
			return fmt.Errorf("actor-template %s/%s runs %s, not %s; delete it (kubectl ate delete actor-template %s -a %s) to replace it",
				c.atespace, c.template, have, want, c.template, c.atespace)
		}
		fmt.Fprintf(w, "ActorTemplate %s/%s already runs this counter image\n", c.atespace, c.template)
	} else {
		tmpl, err := writeRendered(filepath.Join(tree, c.tmplFile), filepath.Join(dir, c.demo+"-template.yaml"), images)
		if err != nil {
			return err
		}
		if _, err := k.do(ctx, "create", "actor-template", "-f", tmpl); err != nil {
			return fmt.Errorf("create actor-template %s/%s: %w", c.atespace, c.template, err)
		}
		fmt.Fprintf(w, "Created ActorTemplate %s/%s\n", c.atespace, c.template)
	}
	return e.waitGolden(ctx, w, k, c)
}

// writeRendered renders the template at src (render) into dst.
func writeRendered(src, dst string, images map[string]string) (string, error) {
	b, err := render(src, images)
	if err != nil {
		return "", err
	}
	return dst, os.WriteFile(dst, b, 0o644)
}

// templateImage is the first container image of an ActorTemplate as
// `kubectl ate get actor-template -o json` prints it, with or without spec.
func templateImage(out []byte) (string, error) {
	type containers struct {
		Containers []struct {
			Image string `json:"image"`
		} `json:"containers"`
	}
	var t struct {
		containers
		Spec *containers `json:"spec"`
	}
	if err := json.Unmarshal(out, &t); err != nil {
		return "", err
	}
	cs := t.Containers
	if t.Spec != nil {
		cs = t.Spec.Containers
	}
	if len(cs) == 0 {
		return "", errors.New("it has no container")
	}
	return cs[0].Image, nil
}

// Poll bounds at the 2s interval: waitGolden waits 15 minutes, deploy.sh's
// wait for the golden snapshot, and the snapshot's S3 copy is waited for 10
// minutes, verify.sh's wait.
const (
	goldenTries = 450
	s3Tries     = 300
)

// waitGolden waits for the ActorTemplate's golden snapshot, which new actors
// restore from; ate-setup deploy demo waits for it too.
func (e *env) waitGolden(ctx context.Context, w io.Writer, k ate, c class) error {
	var last string
	if e.poll(ctx, goldenTries, func() bool {
		out, err := k.do(ctx, "get", "actor-template", c.template, "-a", c.atespace, "-o", "json")
		if err != nil {
			last = err.Error()
			return false
		}
		var t struct {
			Status struct {
				Golden struct {
					Tag struct {
						Name string `json:"name"`
					} `json:"goldenTag"`
					Error string `json:"errorMessage"`
				} `json:"goldenSnapshotStatus"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &t); err != nil {
			last = err.Error()
			return false
		}
		last = t.Status.Golden.Error
		return t.Status.Golden.Tag.Name != ""
	}) {
		fmt.Fprintf(w, "Golden snapshot ready for %s/%s\n", c.atespace, c.template)
		return nil
	}
	return fmt.Errorf("golden snapshot of %s/%s not ready (%s)", c.atespace, c.template, orNone(last))
}

// SnapBucket is setup-aws's snapshot bucket name (cmd.deriveNames).
func SnapBucket(cluster, account, region string) string {
	return fmt.Sprintf("%s-snap-%s-%s", cluster, account, region)
}

func (e *env) verify(ctx context.Context, w io.Writer, o Options) error {
	if err := o.check(); err != nil {
		return err
	}
	cls, err := parseClasses(o.SandboxClasses)
	if err != nil {
		return err
	}
	tree, err := treeDir(o)
	if err != nil {
		return err
	}
	kc, err := e.kubeconfig(ctx)
	if err != nil {
		return err
	}
	account, err := e.account(ctx)
	if err != nil {
		return err
	}
	bucket := SnapBucket(o.Cluster, account, o.Region)
	fmt.Fprintln(w, "Building kubectl-ate from substrate "+o.Tag)
	if err := e.run(ctx, w, tree, goEnv, "go", "build", "-o", e.kubectlAte, "./cmd/kubectl-ate"); err != nil {
		return fmt.Errorf("build kubectl-ate: %w", err)
	}
	k := ate{e: e, bin: e.kubectlAte, kubeconfig: kc, context: o.Cluster}
	for _, c := range cls {
		fmt.Fprintln(w, VerifyLine+c.name)
		summary, err := e.roundtrip(ctx, w, k, c, bucket)
		if err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		fmt.Fprintln(w, VerifyPassLine+c.name+": "+summary)
	}
	return nil
}

// Actor states and snapshot values in `kubectl ate get actor -o json`.
const (
	stateRunning     = "ACTOR_STATE_RUNNING"
	stateSuspended   = "ACTOR_STATE_SUSPENDED"
	stateCrashed     = "ACTOR_STATE_CRASHED"
	ownerActor       = "SNAPSHOT_OWNER_ACTOR"
	storageCompleted = "SNAPSHOT_STORAGE_STATE_COMPLETED"
)

// actor is the part of an Actor the check reads.
type actor struct {
	Metadata struct {
		UID string `json:"uid"`
	} `json:"metadata"`
	Status struct {
		State     string     `json:"state"`
		Snapshots []snapshot `json:"snapshots"`
	} `json:"status"`
}

type snapshot struct {
	Generation      int    `json:"generation"`
	Owner           string `json:"owner"`
	DurableSnapshot struct {
		State  string `json:"state"`
		Object struct {
			SnapshotURI string `json:"snapshotUri"`
		} `json:"object"`
	} `json:"durableSnapshot"`
}

// latest is the actor's newest own snapshot, if any.
func (a actor) latest() (snapshot, bool) {
	var best snapshot
	found := false
	for _, s := range a.Status.Snapshots {
		if s.Owner == ownerActor && (!found || s.Generation > best.Generation) {
			best, found = s, true
		}
	}
	return best, found
}

// ate runs kubectl-ate against the installer's kubeconfig and context.
type ate struct {
	e                        *env
	bin, kubeconfig, context string
}

func (k ate) argv(args ...string) []string {
	return append(append([]string{k.bin}, args...), "--kubeconfig", k.kubeconfig, "--context", k.context)
}

func (k ate) env() []string { return []string{"KUBECONFIG=" + k.kubeconfig} }

func (k ate) do(ctx context.Context, args ...string) ([]byte, error) {
	return k.e.output(ctx, k.env(), k.argv(args...)...)
}

func (k ate) get(ctx context.Context, c class) (actor, error) {
	out, err := k.do(ctx, "get", "actor", c.actor, "-a", c.atespace, "-o", "json")
	if err != nil {
		return actor{}, err
	}
	var a actor
	if err := json.Unmarshal(out, &a); err != nil {
		return actor{}, fmt.Errorf("read actor %s: %w", c.actor, err)
	}
	return a, nil
}

// poll calls f until it reports done, up to tries times.
func (e *env) poll(ctx context.Context, tries int, f func() bool) bool {
	for i := 0; i < tries; i++ {
		if f() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(e.interval):
		}
	}
	return false
}

// waitState waits for the actor to reach want and returns its last read.
func (e *env) waitState(ctx context.Context, k ate, c class, want string, tries int) (actor, error) {
	var a actor
	var err error
	if e.poll(ctx, tries, func() bool {
		a, err = k.get(ctx, c)
		return err == nil && a.Status.State == want
	}) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	return a, fmt.Errorf("actor %s is %s, not %s", c.actor, a.Status.State, want)
}

// roundtrip is verify.sh's roundtrip for one class.
func (e *env) roundtrip(ctx context.Context, w io.Writer, k ate, c class, bucket string) (string, error) {
	a, err := k.get(ctx, c)
	if err != nil {
		if _, cerr := k.do(ctx, "create", "actor", c.actor, "-a", c.atespace, "--template", c.template); cerr != nil {
			return "", fmt.Errorf("create actor %s: %w (get: %v)", c.actor, cerr, err)
		}
		fmt.Fprintln(w, "Created actor "+c.actor+" in atespace "+c.atespace)
		if a, err = k.get(ctx, c); err != nil {
			return "", err
		}
	}
	// Reinstalling Substrate or deleting worker pods crashes a running
	// actor. Revert it to its last snapshot; the check starts from there.
	if a.Status.State == stateCrashed {
		if _, err := k.do(ctx, "revert", "actor", c.actor, "-a", c.atespace); err != nil {
			return "", fmt.Errorf("actor %s is CRASHED and does not revert: %w", c.actor, err)
		}
		fmt.Fprintln(w, "Reverted actor "+c.actor+" from CRASHED")
	}

	port, err := e.freePort()
	if err != nil {
		return "", err
	}
	var proxyLog tail
	stop, err := e.start(ctx, &proxyLog, k.env(), k.argv("proxy", "actor", c.actor, "-a", c.atespace, fmt.Sprintf("%d:80", port))...)
	if err != nil {
		return "", fmt.Errorf("kubectl ate proxy: %w", err)
	}
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	// /readyz answers without moving the counters, and wakes a suspended actor.
	if !e.poll(ctx, 60, func() bool { _, err := e.get(ctx, base+"readyz", 10*time.Second); return err == nil }) {
		return "", fmt.Errorf("actor %s did not answer through kubectl ate proxy: %s", c.actor, proxyLog.String())
	}
	first, err := e.read(ctx, base, 30*time.Second)
	if err != nil {
		return "", err
	}
	before, err := e.read(ctx, base, 30*time.Second)
	if err != nil {
		return "", err
	}
	if err := before.continues(first); err != nil {
		return "", fmt.Errorf("the counters do not count: %w", err)
	}
	fmt.Fprintln(w, "  before suspend: "+before.String())

	if a, err = k.get(ctx, c); err != nil {
		return "", err
	}
	prev, _ := a.latest()
	if _, err := k.do(ctx, "suspend", "actor", c.actor, "-a", c.atespace); err != nil {
		return "", fmt.Errorf("suspend actor %s: %w", c.actor, err)
	}
	if _, err := e.waitState(ctx, k, c, stateSuspended, 60); err != nil {
		return "", fmt.Errorf("%w after suspend", err)
	}
	fmt.Fprintln(w, "  suspended")
	if _, err := k.do(ctx, "resume", "actor", c.actor, "-a", c.atespace); err != nil {
		return "", fmt.Errorf("resume actor %s: %w", c.actor, err)
	}
	if _, err := e.waitState(ctx, k, c, stateRunning, 90); err != nil {
		return "", fmt.Errorf("%w after resume", err)
	}
	after, err := e.read(ctx, base, 120*time.Second)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(w, "  after resume:   "+after.String())
	if err := after.continues(before); err != nil {
		return "", fmt.Errorf("suspend and resume lost state: %w", err)
	}

	// The resume can use the node's copy while the S3 upload of the same
	// snapshot finishes in the background, minutes later: 4.5 to 6 minutes
	// on sub-nt6. Wait at least verify.sh's 10 minutes.
	var snap snapshot
	if !e.poll(ctx, s3Tries, func() bool {
		a, err = k.get(ctx, c)
		var ok bool
		snap, ok = a.latest()
		return err == nil && ok && snap.Generation > prev.Generation && snap.DurableSnapshot.State == storageCompleted
	}) {
		return "", fmt.Errorf("actor %s's new snapshot did not reach S3 (state: %s)", c.actor, orNone(snap.DurableSnapshot.State))
	}
	key, err := snapshotKey(snap.DurableSnapshot.Object.SnapshotURI, bucket, c.atespace, a.Metadata.UID)
	if err != nil {
		return "", err
	}
	n, err := e.objects(ctx, bucket, key+"/")
	if err != nil {
		return "", fmt.Errorf("list the snapshot under %s: %w", key, err)
	}
	if n == 0 {
		return "", fmt.Errorf("no objects under %s in the snapshot bucket", key)
	}
	return fmt.Sprintf("resumed and the memory, file and rootfs counters continued; snapshot in S3: %d objects under %s", n, key), nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// snapshotKey checks that uri is an object of this actor in bucket, under
// Substrate's layout atespaces/<atespace>/actors/<uid>/snapshots/<name>, and
// returns its key. Errors never print the bucket, whose name carries the
// account id.
func snapshotKey(uri, bucket, atespace, uid string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", errors.New("the snapshot URI does not parse")
	}
	if u.Scheme != "s3" {
		return "", fmt.Errorf("the snapshot URI is %s://, not s3://", u.Scheme)
	}
	if u.Host != bucket {
		return "", errors.New("the snapshot is not in this deployment's snapshot bucket")
	}
	key := strings.Trim(u.Path, "/")
	prefix := "atespaces/" + atespace + "/actors/" + uid + "/snapshots/"
	name, ok := strings.CutPrefix(key, prefix)
	if uid == "" || !ok || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("snapshot %s is not under %s<name>", key, prefix)
	}
	return key, nil
}

// counters is one answer of the counter demo.
type counters struct{ memory, file, rootfs int }

var counterRe = map[string]*regexp.Regexp{
	"memory": regexp.MustCompile(`preserved memory count: (\d+)`),
	"file":   regexp.MustCompile(`preserved file counter: (\d+)`),
	"rootfs": regexp.MustCompile(`preserved rootfs counter: (\d+)`),
}

// parseCounters reads "preserved memory count: 3 | preserved file counter: 3
// | preserved rootfs counter: 3".
func parseCounters(body string) (counters, error) {
	var c counters
	for name, p := range map[string]*int{"memory": &c.memory, "file": &c.file, "rootfs": &c.rootfs} {
		m := counterRe[name].FindStringSubmatch(body)
		if m == nil {
			return counters{}, fmt.Errorf("no %s counter in the counter's answer %q", name, strings.TrimSpace(body))
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return counters{}, err
		}
		*p = n
	}
	return c, nil
}

func (c counters) String() string {
	return fmt.Sprintf("memory %d, file %d, rootfs %d", c.memory, c.file, c.rootfs)
}

// continues checks that c is the request right after prev: every counter
// went up by exactly one.
func (c counters) continues(prev counters) error {
	var bad []string
	for _, x := range []struct {
		name        string
		got, before int
	}{{"memory", c.memory, prev.memory}, {"file", c.file, prev.file}, {"rootfs", c.rootfs, prev.rootfs}} {
		if x.got != x.before+1 {
			bad = append(bad, fmt.Sprintf("the %s counter is %d after %d (want %d)", x.name, x.got, x.before, x.before+1))
		}
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

func (e *env) read(ctx context.Context, base string, timeout time.Duration) (counters, error) {
	body, err := e.get(ctx, base, timeout)
	if err != nil {
		return counters{}, fmt.Errorf("GET %s: %w", base, err)
	}
	return parseCounters(body)
}

// tail keeps the last few KB a child wrote, for an error message. The child's
// output is copied in from another goroutine, hence the lock.
type tail struct {
	mu sync.Mutex
	b  []byte
}

const tailMax = 4096

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = t.b[len(t.b)-tailMax:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}
