package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/kustomize/kyaml/kio"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
)

const (
	testAccount = "111122223333"
	testKube    = "/cache/kubeconfig/c1.us-west-2.yaml"
	testAte     = "/cache/bin/kubectl-ate-v1.0.0"
)

func testOptions(root string) Options {
	return Options{Cluster: "c1", Region: "us-west-2", Profile: "p", Tag: "v1.0.0",
		TreeRoot: root, ImageRepo: "ghcr.io/agent-substrate/substrate", SandboxClasses: "gvisor,microvm"}
}

// testBase is the counter's base image in the test tree's .ko.yaml.
const testBase = "gcr.io/distroless/static-debian13:latest@sha256:15f8"

// testKoYAML is the shape of upstream's .ko.yaml: a default base and the
// counter's root-user override.
const testKoYAML = `defaultBaseImage: gcr.io/distroless/static-debian13:nonroot@sha256:18f4
baseImageOverrides:
  # The actor images stay on the root base.
  go.ate.dev/substrate/demos/counter: ` + testBase + `
  go.ate.dev/substrate/demos/egress: gcr.io/distroless/static-debian13:latest@sha256:15f8
`

// testTree makes a cached tree with both demo pool and template files and
// upstream's .ko.yaml.
func testTree(t *testing.T) (root, tree string) {
	t.Helper()
	root = t.TempDir()
	tree = controlplane.TreeDir(root, "v1.0.0")
	pool, err := os.ReadFile("testdata/pool.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile("testdata/template.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range classes {
		p := filepath.Join(tree, c.pool)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.ReplaceAll(pool, []byte("ateworker-gvisor"), []byte("ateworker-"+c.name)), 0o644); err != nil {
			t.Fatal(err)
		}
		tt := strings.NewReplacer("ATESPACE", c.atespace, "TEMPLATE", c.template).Replace(string(tmpl))
		if err := os.WriteFile(filepath.Join(tree, c.tmplFile), []byte(tt), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, ".ko.yaml"), []byte(testKoYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, tree
}

type call struct {
	dir  string
	env  []string
	argv []string
}

// fakeActor is one actor in the fake cluster.
type fakeActor struct {
	ns, uid, state string
	c              counters
	snaps          []snapshot
	// lose names a counter the resume resets, as a lost snapshot would.
	lose string
	// noSnapshot makes a suspend record no new snapshot.
	noSnapshot bool
	// slowReads is how many reads leave a pending upload pending.
	slowReads int
}

// fake is a cluster, an S3 bucket and a localhost proxy, answering the argv
// Verify runs the way kubectl-ate and the counter do.
type fake struct {
	t       *testing.T
	calls   []call
	actors  map[string]*fakeActor
	proxies map[int]string // port -> actor
	stopped int
	port    int
	listed  []string // bucket|prefix of every S3 list
	objects int
	lose    map[string]string // actor -> counter its resume loses

	// The deploy's side: the node architectures kubectl reports, what was
	// pushed and pinned, every manifest applied (by file name, read when
	// applied), and the cluster's atespaces and templates (atespace/name ->
	// image) with each created template's manifest.
	archs     string
	pushed    []string
	pinned    []string
	applied   map[string]string
	atespaces map[string]bool
	templates map[string]string
	created   map[string]string
}

// testCounter is the reference the fake pushCounter returns.
const testCounter = testAccount + ".dkr.ecr.us-west-2.amazonaws.com/c1/counter:v1.0.0@sha256:c0ffee"

func newFake(t *testing.T) *fake {
	return &fake{t: t, actors: map[string]*fakeActor{}, proxies: map[int]string{}, port: 40000, objects: 7, lose: map[string]string{},
		archs: "amd64 amd64 amd64 amd64", applied: map[string]string{}, atespaces: map[string]bool{},
		templates: map[string]string{}, created: map[string]string{}}
}

func (f *fake) env() *env {
	return &env{
		run: func(_ context.Context, w io.Writer, dir string, extra []string, argv ...string) error {
			f.calls = append(f.calls, call{dir, extra, argv})
			if i := slices.Index(argv, "apply"); i >= 0 && argv[0] == "kubectl" && argv[i+1] == "-f" {
				b, err := os.ReadFile(argv[i+2])
				if err != nil {
					f.t.Fatal(err)
				}
				f.applied[filepath.Base(argv[i+2])] = string(b)
			}
			return nil
		},
		output: func(_ context.Context, extra []string, argv ...string) ([]byte, error) {
			f.calls = append(f.calls, call{"", extra, argv})
			if argv[0] == "kubectl" {
				if !slices.Equal(argv[1:5], []string{"--kubeconfig", testKube, "--context", "c1"}) {
					f.t.Fatalf("kubectl argv %q does not name the installer's kubeconfig and context", argv)
				}
				return []byte(f.archs), nil
			}
			return f.ate(argv)
		},
		pushCounter: func(_ context.Context, binary, base, arch, repo, tag string) (string, error) {
			f.pushed = append(f.pushed, strings.Join([]string{filepath.Base(binary), base, arch, repo, tag}, "|"))
			return testCounter, nil
		},
		pin: func(_ context.Context, tagged string) (string, error) {
			f.pinned = append(f.pinned, tagged)
			return tagged + "@sha256:pinned", nil
		},
		start: func(_ context.Context, _ io.Writer, extra []string, argv ...string) (func(), error) {
			f.calls = append(f.calls, call{"", extra, argv})
			args := ateArgs(argv)
			p, err := strconv.Atoi(strings.TrimSuffix(args[len(args)-1], ":80"))
			if err != nil {
				return nil, err
			}
			f.proxies[p] = args[2]
			return func() { f.stopped++ }, nil
		},
		get: f.get,
		objects: func(_ context.Context, bucket, prefix string) (int, error) {
			f.listed = append(f.listed, bucket+"|"+prefix)
			return f.objects, nil
		},
		account:    func(context.Context) (string, error) { return testAccount, nil },
		kubeconfig: func(context.Context) (string, error) { return testKube, nil },
		freePort:   func() (int, error) { f.port++; return f.port, nil },
		kubectlAte: testAte,
	}
}

// ateArgs strips the binary and the trailing --kubeconfig/--context.
func ateArgs(argv []string) []string {
	args := argv[1:]
	if i := slices.Index(args, "--kubeconfig"); i >= 0 {
		args = args[:i]
	}
	return args
}

func (f *fake) ate(argv []string) ([]byte, error) {
	if argv[0] != testAte || !slices.Equal(argv[len(argv)-4:], []string{"--kubeconfig", testKube, "--context", "c1"}) {
		f.t.Fatalf("kubectl-ate argv %q does not name the installer's binary, kubeconfig and context", argv)
	}
	args := ateArgs(argv)
	switch args[1] {
	case "atespace":
		if f.atespaces[args[2]] {
			return nil, fmt.Errorf("atespace %q already exists", args[2])
		}
		f.atespaces[args[2]] = true
		return nil, nil
	case "actor-template":
		return f.template(args)
	}
	verb, name, ns := args[0], args[2], args[4]
	a := f.actors[name]
	if verb != "create" && a == nil {
		return nil, fmt.Errorf("actor %s not found", name)
	}
	switch verb {
	case "get":
		// A pending upload completes on the next read, as it would in the
		// background, or after slowReads more reads.
		var out actor
		out.Metadata.UID = a.uid
		out.Status.State = a.state
		out.Status.Snapshots = slices.Clone(a.snaps)
		if a.slowReads > 0 {
			a.slowReads--
			return json.Marshal(out)
		}
		for i := range a.snaps {
			a.snaps[i].DurableSnapshot.State = storageCompleted
		}
		return json.Marshal(out)
	case "create":
		f.actors[name] = &fakeActor{ns: ns, uid: "uid-" + name, state: stateRunning, lose: f.lose[name]}
	case "revert":
		a.state = stateRunning
	case "suspend":
		a.state = stateSuspended
		if !a.noSnapshot {
			s := snapshot{Generation: len(a.snaps) + 1, Owner: ownerActor}
			s.DurableSnapshot.State = "SNAPSHOT_STORAGE_STATE_PENDING"
			s.DurableSnapshot.Object.SnapshotURI = fmt.Sprintf("s3://%s/atespaces/%s/actors/%s/snapshots/snap-%d",
				SnapBucket("c1", testAccount, "us-west-2"), ns, a.uid, s.Generation)
			a.snaps = append(a.snaps, s)
		}
	case "resume":
		a.state = stateRunning
		switch a.lose {
		case "memory":
			a.c.memory = 0
		case "rootfs":
			a.c.rootfs = 0
		}
	default:
		f.t.Fatalf("unexpected kubectl-ate %q", args)
	}
	return nil, nil
}

func (f *fake) get(_ context.Context, url string, _ time.Duration) (string, error) {
	hostPort, path, _ := strings.Cut(strings.TrimPrefix(url, "http://127.0.0.1:"), "/")
	port, err := strconv.Atoi(hostPort)
	if err != nil {
		return "", err
	}
	a := f.actors[f.proxies[port]]
	if a == nil {
		return "", errors.New("connection refused")
	}
	if a.state == stateSuspended {
		a.state = stateRunning // a request wakes a suspended actor
	}
	if path == "readyz" {
		return "ok\n", nil
	}
	a.c.memory++
	a.c.file++
	a.c.rootfs++
	return fmt.Sprintf("preserved memory count: %d | preserved file counter: %d | preserved rootfs counter: %d\n", a.c.memory, a.c.file, a.c.rootfs), nil
}

func (f *fake) verbs() []string {
	var out []string
	for _, c := range f.calls {
		if c.argv[0] == testAte {
			a := ateArgs(c.argv)
			out = append(out, a[0]+" "+a[2])
		}
	}
	return out
}

// templateRe reads what the fake needs from a rendered ActorTemplate.
var templateRe = regexp.MustCompile(`(?m)^  atespace: (\S+)\n  name: (\S+)\n[\s\S]*?^  image: (\S+)`)

// template answers `kubectl ate get|create actor-template`.
func (f *fake) template(args []string) ([]byte, error) {
	switch args[0] {
	case "get":
		img, ok := f.templates[args[4]+"/"+args[2]]
		if !ok {
			return nil, fmt.Errorf("actor-template %q not found", args[2])
		}
		return json.Marshal(map[string]any{
			"spec":   map[string]any{"containers": []any{map[string]any{"image": img}}},
			"status": map[string]any{"goldenSnapshotStatus": map[string]any{"goldenTag": map[string]any{"name": "golden-1"}}},
		})
	case "create":
		b, err := os.ReadFile(args[3])
		if err != nil {
			f.t.Fatal(err)
		}
		m := templateRe.FindStringSubmatch(string(b))
		if m == nil {
			f.t.Fatalf("created template does not parse:\n%s", b)
		}
		f.templates[m[1]+"/"+m[2]], f.created[m[1]+"/"+m[2]] = m[3], string(b)
		return nil, nil
	}
	f.t.Fatalf("unexpected kubectl-ate %q", args)
	return nil, nil
}

// AC7.1: releases publish no counter image, so the deploy builds it for the
// nodes' architecture on upstream's base, pushes it to <cluster>/counter, and
// installs both classes from rendered templates: every image pinned by
// digest, each pool on its node group, no ko:// or placeholder left.
func TestDeployBuildsTheCounterAndAppliesPinnedTemplates(t *testing.T) {
	root, tree := testTree(t)
	f := newFake(t)
	f.archs = "arm64 arm64"
	var out bytes.Buffer
	if err := f.env().deploy(context.Background(), &out, testOptions(root)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var build *call
	for i, c := range f.calls {
		if slices.Contains(c.argv, "./demos/counter") {
			build = &f.calls[i]
		}
	}
	if build == nil || build.dir != tree || !slices.Contains(build.env, "GOARCH=arm64") || !slices.Contains(build.env, "GOFLAGS=-mod=vendor") {
		t.Fatalf("counter build = %+v, want go build ./demos/counter in the tree for arm64", build)
	}
	if want := []string{"counter|" + testBase + "|arm64|c1/counter|v1.0.0"}; !slices.Equal(f.pushed, want) {
		t.Errorf("pushed %q, want %q", f.pushed, want)
	}
	if want := []string{"ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0", "ghcr.io/agent-substrate/substrate/ateworker-microvm:v1.0.0"}; !slices.Equal(f.pinned, want) {
		t.Errorf("pinned %q, want %q", f.pinned, want)
	}
	for _, c := range classes {
		pool := f.applied[c.demo+"-pool.yaml"]
		worker := "workerImage: ghcr.io/agent-substrate/substrate/ateworker-" + c.name + ":v1.0.0@sha256:pinned"
		if !strings.Contains(pool, worker) || !strings.Contains(pool, PoolLabel+": "+c.name) || strings.Contains(pool, "ko://go") {
			t.Errorf("%s: applied pool lacks %q or its nodeSelector, or keeps a ko:// reference:\n%s", c.name, worker, pool)
		}
		key := c.atespace + "/" + c.template
		if f.templates[key] != testCounter {
			t.Errorf("%s runs %q, want %q", key, f.templates[key], testCounter)
		}
		if tmpl := f.created[key]; strings.Contains(tmpl, "${") || strings.Contains(tmpl, "ko://go") {
			t.Errorf("%s: created template keeps a placeholder or ko:// reference:\n%s", key, tmpl)
		}
		if !f.atespaces[c.atespace] {
			t.Errorf("atespace %s not created", c.atespace)
		}
	}
	for _, l := range []string{DeployLine + "gvisor\n", DeployLine + "microvm\n", "Golden snapshot ready for ate-demo-counter/counter\n"} {
		if !strings.Contains(out.String(), l) {
			t.Errorf("output lacks %q:\n%s", l, out.String())
		}
	}
	for _, c := range f.calls {
		if c.argv[0] == "bash" || c.argv[0] == "sh" || c.argv[0] == "docker" || slices.Contains(c.argv, "-c") {
			t.Errorf("ran %q", c.argv)
		}
	}
}

// A rerun keeps a template that runs this counter image, and refuses one
// that runs another (templates are immutable) without creating anything.
func TestDeployKeepsItsTemplateAndRefusesAnother(t *testing.T) {
	root, _ := testTree(t)
	f := newFake(t)
	f.atespaces["ate-demo-counter"] = true
	f.templates["ate-demo-counter/counter"] = testCounter
	o := testOptions(root)
	o.SandboxClasses = "gvisor"
	if err := f.env().deploy(context.Background(), io.Discard, o); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("rerun created %v", f.created)
	}

	f = newFake(t)
	f.templates["ate-demo-counter/counter"] = "example.com/other/counter@sha256:beef"
	err := f.env().deploy(context.Background(), io.Discard, o)
	if err == nil || !strings.Contains(err.Error(), "runs example.com/other/counter@sha256:beef, not "+testCounter) {
		t.Fatalf("err = %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("created %v over another image", f.created)
	}
}

// The counter image is built for one architecture, so node groups of two
// are refused before anything is built or pushed.
func TestDeployRefusesMixedNodeArchitectures(t *testing.T) {
	root, _ := testTree(t)
	f := newFake(t)
	f.archs = "amd64 arm64 amd64"
	err := f.env().deploy(context.Background(), io.Discard, testOptions(root))
	if err == nil || !strings.Contains(err.Error(), "amd64 and arm64") || len(f.pushed) != 0 {
		t.Fatalf("err = %v, pushed %q", err, f.pushed)
	}
}

func TestRenderReplacesWholeReferencesAndDropsPlaceholders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.yaml")
	src := "# the ko:// image reference\nimage: ko://go.ate.dev/substrate/demos/counter\n${EXTERNAL_VOLUMES}\n  - --flag\n"
	os.WriteFile(p, []byte(src), 0o644)
	got, err := render(p, map[string]string{counterPkg: "r/counter@sha256:1"})
	if want := "# the ko:// image reference\nimage: r/counter@sha256:1\n  - --flag\n"; err != nil || string(got) != want {
		t.Errorf("render = %q, %v; want %q", got, err, want)
	}
	// A longer package is not the counter, and an unmapped reference fails.
	os.WriteFile(p, []byte("image: ko://go.ate.dev/substrate/demos/counter-x\n"), 0o644)
	if _, err := render(p, map[string]string{counterPkg: "r/counter@sha256:1"}); err == nil || !strings.Contains(err.Error(), "counter-x") {
		t.Errorf("unmapped reference: err = %v", err)
	}
}

func TestCounterBaseIsUpstreamsPinnedOverride(t *testing.T) {
	_, tree := testTree(t)
	if b, err := counterBase(tree); err != nil || b != testBase {
		t.Errorf("counterBase = %q, %v; want %q", b, err, testBase)
	}
	os.WriteFile(filepath.Join(tree, ".ko.yaml"), []byte("defaultBaseImage: cgr.dev/static@sha256:aa\n"), 0o644)
	if b, err := counterBase(tree); err != nil || b != "cgr.dev/static@sha256:aa" {
		t.Errorf("default base = %q, %v", b, err)
	}
	os.WriteFile(filepath.Join(tree, ".ko.yaml"), []byte("defaultBaseImage: cgr.dev/static:latest\n"), 0o644)
	if _, err := counterBase(tree); err == nil {
		t.Error("an unpinned base was accepted")
	}
}

func TestAppLayerIsReproducible(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "counter")
	os.WriteFile(bin, []byte("\x7fELF fake"), 0o755)
	a, err := appLayer(bin)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := appLayer(bin)
	da, _ := a.Digest()
	db, _ := b.Digest()
	if da != db {
		t.Errorf("same binary, two digests: %s %s", da, db)
	}
}

func TestDeployRefusesAMissingTreeAndAnUnknownClass(t *testing.T) {
	f := newFake(t)
	if err := f.env().deploy(context.Background(), io.Discard, testOptions(t.TempDir())); err == nil {
		t.Error("deploy ran without a cached tree")
	}
	root, _ := testTree(t)
	o := testOptions(root)
	o.SandboxClasses = "gvisor,kata"
	if err := f.env().deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "kata") {
		t.Errorf("unknown class: err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v before refusing", f.calls)
	}
}

func poolSelector(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kio.FromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.GetKind() == "WorkerPool" {
			v, err := n.GetString("spec.template.nodeSelector." + strings.ReplaceAll(PoolLabel, ".", `\.`))
			if err != nil {
				return ""
			}
			return v
		}
	}
	t.Fatalf("no WorkerPool in %s", path)
	return ""
}

func TestPinPoolSetsTheNodeSelectorOnceAndKeepsTheRest(t *testing.T) {
	_, tree := testTree(t)
	p := filepath.Join(tree, classes["gvisor"].pool)
	changed, err := PinPool(p, "gvisor")
	if err != nil || !changed {
		t.Fatalf("first PinPool: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, keep := range []string{"# No configRef: the pool follows the class default.", "kind: Namespace", "name: ate-demo-counter",
		"workerImage: ko://go.ate.dev/substrate/cmd/ateworker-gvisor", "memory: 1Gi", "ate.dev/pool: gvisor"} {
		if !strings.Contains(s, keep) {
			t.Errorf("pinned manifest lacks %q:\n%s", keep, s)
		}
	}
	if got := poolSelector(t, p); got != "gvisor" {
		t.Errorf("selector = %q", got)
	}
	if changed, err := PinPool(p, "gvisor"); err != nil || changed {
		t.Errorf("second PinPool should change nothing: changed=%v err=%v", changed, err)
	}
	if changed, err := PinPool(p, "microvm"); err != nil || !changed || poolSelector(t, p) != "microvm" {
		t.Errorf("repin: changed=%v err=%v", changed, err)
	}
	ns := filepath.Join(t.TempDir(), "ns.yaml")
	os.WriteFile(ns, []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n"), 0o644)
	if _, err := PinPool(ns, "gvisor"); err == nil {
		t.Error("PinPool accepted a manifest with no WorkerPool")
	}
}

func TestParseCounters(t *testing.T) {
	c, err := parseCounters("preserved memory count: 5 | preserved file counter: 7 | preserved rootfs counter: 6 | preserved second file counter: 99\n")
	if err != nil || c != (counters{memory: 5, file: 7, rootfs: 6}) {
		t.Errorf("parse = %+v, %v", c, err)
	}
	if _, err := parseCounters("preserved memory count: 5 | preserved file counter: 7\n"); err == nil || !strings.Contains(err.Error(), "rootfs") {
		t.Errorf("missing rootfs counter: err = %v", err)
	}
	if _, err := parseCounters("upstream connect error"); err == nil {
		t.Error("parsed an answer with no counters")
	}
}

func TestContinuesNeedsEveryCounterUpByOne(t *testing.T) {
	before := counters{memory: 3, file: 3, rootfs: 3}
	if err := (counters{4, 4, 4}).continues(before); err != nil {
		t.Errorf("a continuing answer failed: %v", err)
	}
	for name, after := range map[string]counters{
		"memory": {1, 4, 4}, // a memory counter that cold-booted
		"file":   {4, 3, 4},
		"rootfs": {4, 4, 1},
	} {
		err := after.continues(before)
		if err == nil || !strings.Contains(err.Error(), "the "+name+" counter") {
			t.Errorf("%s did not continue: err = %v", name, err)
		}
	}
}

func TestVerifyPassesOnBothClasses(t *testing.T) {
	root, tree := testTree(t)
	f := newFake(t)
	var out bytes.Buffer
	if err := f.env().verify(context.Background(), &out, testOptions(root)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// kubectl-ate is built from the tree, then each actor goes through
	// create, suspend and resume, in that order.
	if c := f.calls[0]; !slices.Equal(c.argv, []string{"go", "build", "-o", testAte, "./cmd/kubectl-ate"}) || c.dir != tree {
		t.Errorf("first call = %+v, want the kubectl-ate build in the tree", c)
	}
	verbs := f.verbs()
	for _, actor := range []string{"demo-gvisor", "demo-microvm"} {
		var seq []string
		for _, v := range verbs {
			if strings.HasSuffix(v, " "+actor) && !strings.HasPrefix(v, "get ") {
				seq = append(seq, strings.Fields(v)[0])
			}
		}
		if !slices.Equal(seq, []string{"create", "proxy", "suspend", "resume"}) {
			t.Errorf("%s: kubectl-ate verbs = %v", actor, seq)
		}
	}
	bucket := SnapBucket("c1", testAccount, "us-west-2")
	wantList := []string{
		bucket + "|atespaces/ate-demo-counter/actors/uid-demo-gvisor/snapshots/snap-1/",
		bucket + "|atespaces/ate-demo-counter-microvm/actors/uid-demo-microvm/snapshots/snap-1/",
	}
	if !slices.Equal(f.listed, wantList) {
		t.Errorf("S3 lists = %q, want %q", f.listed, wantList)
	}
	if f.stopped != 2 {
		t.Errorf("stopped %d proxies, want 2", f.stopped)
	}
	for _, l := range []string{VerifyLine + "gvisor", VerifyPassLine + "gvisor: ", VerifyLine + "microvm", VerifyPassLine + "microvm: "} {
		if !strings.Contains(out.String(), l) {
			t.Errorf("output lacks %q:\n%s", l, out.String())
		}
	}
	if strings.Contains(out.String(), testAccount) {
		t.Errorf("output shows the account id:\n%s", out.String())
	}
	for _, c := range f.calls {
		if c.argv[0] == "bash" || c.argv[0] == "sh" || slices.Contains(c.argv, "-c") {
			t.Errorf("a shell ran: %q", c.argv)
		}
	}
}

// The S3 copy of a snapshot took 4.5 to 6 minutes on sub-nt6, at the edge
// of a 120-read wait; the check waits as long as verify.sh (10 minutes, 300
// reads at the 2s interval), so an upload done after 200 reads passes.
func TestVerifyWaitsForASlowSnapshotUpload(t *testing.T) {
	root, _ := testTree(t)
	f := newFake(t)
	f.actors["demo-gvisor"] = &fakeActor{ns: "ate-demo-counter", uid: "uid-demo-gvisor", state: stateRunning, slowReads: 200}
	o := testOptions(root)
	o.SandboxClasses = "gvisor"
	if err := f.env().verify(context.Background(), io.Discard, o); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyFailsWhenACounterDoesNotContinue(t *testing.T) {
	root, _ := testTree(t)
	f := newFake(t)
	f.lose["demo-microvm"] = "rootfs"
	var out bytes.Buffer
	err := f.env().verify(context.Background(), &out, testOptions(root))
	if err == nil || !strings.Contains(err.Error(), "microvm") || !strings.Contains(err.Error(), "the rootfs counter is 1 after 2 (want 3)") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), VerifyPassLine+"gvisor") || strings.Contains(out.String(), VerifyPassLine+"microvm") {
		t.Errorf("PASS lines wrong:\n%s", out.String())
	}
}

func TestVerifyRevertsACrashedActor(t *testing.T) {
	root, _ := testTree(t)
	f := newFake(t)
	f.actors["demo-gvisor"] = &fakeActor{ns: "ate-demo-counter", uid: "uid-demo-gvisor", state: stateCrashed}
	o := testOptions(root)
	o.SandboxClasses = "gvisor"
	var out bytes.Buffer
	if err := f.env().verify(context.Background(), &out, o); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	v := f.verbs()
	if i, j := slices.Index(v, "revert demo-gvisor"), slices.Index(v, "proxy demo-gvisor"); i < 0 || i > j || slices.Contains(v, "create demo-gvisor") {
		t.Errorf("verbs = %v, want a revert before the proxy and no create", v)
	}
}

func TestVerifyNeedsTheNewSnapshotInS3(t *testing.T) {
	root, _ := testTree(t)
	o := testOptions(root)
	o.SandboxClasses = "gvisor"

	// The suspend made no snapshot: an older completed one does not count.
	f := newFake(t)
	f.actors["demo-gvisor"] = &fakeActor{ns: "ate-demo-counter", uid: "uid-demo-gvisor", state: stateRunning, noSnapshot: true}
	old := snapshot{Generation: 1, Owner: ownerActor}
	old.DurableSnapshot.State = storageCompleted
	old.DurableSnapshot.Object.SnapshotURI = "s3://" + SnapBucket("c1", testAccount, "us-west-2") + "/atespaces/ate-demo-counter/actors/uid-demo-gvisor/snapshots/old"
	f.actors["demo-gvisor"].snaps = []snapshot{old}
	e := f.env()
	if err := e.verify(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "did not reach S3") {
		t.Errorf("old snapshot only: err = %v", err)
	}

	// The snapshot is recorded but its objects are not in the bucket.
	f = newFake(t)
	f.objects = 0
	if err := f.env().verify(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "no objects under atespaces/ate-demo-counter/actors/uid-demo-gvisor/snapshots/snap-1") {
		t.Errorf("empty snapshot prefix: err = %v", err)
	}
}

func TestSnapshotKey(t *testing.T) {
	b := SnapBucket("c1", testAccount, "us-west-2")
	good := "s3://" + b + "/atespaces/ns/actors/u1/snapshots/s1"
	if k, err := snapshotKey(good, b, "ns", "u1"); err != nil || k != "atespaces/ns/actors/u1/snapshots/s1" {
		t.Errorf("good = %q, %v", k, err)
	}
	for name, uri := range map[string]string{
		"other bucket":   "s3://other/atespaces/ns/actors/u1/snapshots/s1",
		"gcs":            "gs://" + b + "/atespaces/ns/actors/u1/snapshots/s1",
		"other actor":    "s3://" + b + "/atespaces/ns/actors/u2/snapshots/s1",
		"other atespace": "s3://" + b + "/atespaces/x/actors/u1/snapshots/s1",
		"no name":        "s3://" + b + "/atespaces/ns/actors/u1/snapshots/",
	} {
		_, err := snapshotKey(uri, b, "ns", "u1")
		if err == nil {
			t.Errorf("%s: accepted %s", name, uri)
		} else if strings.Contains(err.Error(), testAccount) {
			t.Errorf("%s: error shows the account id: %v", name, err)
		}
	}
}
