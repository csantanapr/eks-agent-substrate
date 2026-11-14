package mode3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"sigs.k8s.io/kustomize/kyaml/kio"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

const (
	testCounter = "111122223333.dkr.ecr.us-east-1.amazonaws.com/c1/counter:" + Tag + "@sha256:c0ffee"
	testWorker  = ImageRepo + "/ateworker-gvisor:" + Tag + "@sha256:pinned"
)

// fake is the cluster, the ate API and the demo code, as the plugin reaches
// them: every kubectl argv through plugin.Exec, every demo call through Demo.
type fake struct {
	t   *testing.T
	log []string
	// out answers a kubectl argv (after --kubeconfig) that starts with a key.
	out map[string]string
	// stdin is the last stdin of each kubectl argv.
	stdin map[string]string
	// The ate API: atespaces, templates (name -> image), actors (name ->
	// state), and the templates created (name -> manifest).
	atespaces map[string]bool
	templates map[string]string
	created   map[string]string
	actors    map[string]string
	ateErr    map[string]error
	ecr       *fakeECR
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, out: map[string]string{}, stdin: map[string]string{}, atespaces: map[string]bool{},
		templates: map[string]string{}, created: map[string]string{}, actors: map[string]string{}, ateErr: map[string]error{},
		ecr: &fakeECR{owner: "c1.us-east-1"}}
	prevExec, prevDemo, prevInterval, prevECR := plugin.Exec, Demo, Interval, ecrFor
	t.Cleanup(func() { plugin.Exec, Demo, Interval, ecrFor = prevExec, prevDemo, prevInterval, prevECR })
	plugin.Exec = f.exec
	Interval = 0
	ecrFor = func(context.Context, plugin.Options) (ecrAPI, error) { return f.ecr, nil }
	Demo = DemoFuncs{
		CounterImage: func(_ context.Context, _ io.Writer, o demo.Options, tree, pool, tag string) (string, error) {
			f.checkOptions(o)
			f.log = append(f.log, "counter image "+filepath.Base(tree)+" "+pool+" "+tag)
			return testCounter, nil
		},
		Pin: func(_ context.Context, o demo.Options, tagged string) (string, error) {
			f.checkOptions(o)
			f.log = append(f.log, "pin "+tagged)
			return tagged + "@sha256:pinned", nil
		},
		KubectlAte: func(_ context.Context, _ io.Writer, o demo.Options, tree string) (string, error) {
			f.checkOptions(o)
			f.log = append(f.log, "build kubectl-ate "+filepath.Base(tree))
			return "/bin/kubectl-ate", nil
		},
		Ate: func(_ context.Context, o demo.Options, args ...string) ([]byte, error) {
			f.checkOptions(o)
			return f.ate(args)
		},
		WaitGolden: func(_ context.Context, _ io.Writer, o demo.Options, p demo.Pool) error {
			f.checkOptions(o)
			f.log = append(f.log, "golden "+p.Atespace+"/"+p.Template)
			if _, ok := f.templates[p.Template]; !ok {
				return errors.New("no template")
			}
			return nil
		},
		RoundTrip: func(_ context.Context, _ io.Writer, o demo.Options, p demo.Pool) (string, error) {
			f.checkOptions(o)
			f.log = append(f.log, "roundtrip "+p.Atespace+"/"+p.Template+"/"+p.Actor)
			return "resumed", nil
		},
	}
	return f
}

// checkOptions: the demo code runs the fork's release, never --tag's.
func (f *fake) checkOptions(o demo.Options) {
	if o.Tag != Tag || o.ImageRepo != ImageRepo || o.Cluster != "c1" || o.Region != "us-east-1" {
		f.t.Fatalf("demo options %+v are not the fork's on c1", o)
	}
}

func (f *fake) exec(_ context.Context, w io.Writer, c plugin.Cmd) error {
	if len(c.Argv) < 3 || c.Argv[0] != "kubectl" || c.Argv[1] != "--kubeconfig" || c.Argv[2] != "/kc" {
		return fmt.Errorf("not kubectl with the installer's kubeconfig: %q", c.Argv)
	}
	for _, a := range c.Argv {
		if a == "-c" || a == "bash" || a == "sh" || a == "helm" || a == "docker" {
			return fmt.Errorf("ran %q", c.Argv)
		}
	}
	args := strings.Join(c.Argv[3:], " ")
	f.log = append(f.log, "kubectl "+args)
	if c.Stdin != nil {
		f.stdin[args] = string(c.Stdin)
	}
	for k, v := range f.out {
		if strings.HasPrefix(args, k) {
			if v == "ERROR" {
				return errors.New("kubectl failed")
			}
			out := c.Stdout
			if out == nil {
				out = w
			}
			io.WriteString(out, v)
			break
		}
	}
	return nil
}

func (f *fake) ate(args []string) ([]byte, error) {
	f.log = append(f.log, "ate "+strings.Join(args, " "))
	if err := f.ateErr[strings.Join(args[:min(3, len(args))], " ")]; err != nil {
		return nil, err
	}
	switch strings.Join(args[:2], " ") {
	case "create atespace":
		if f.atespaces[args[2]] {
			return nil, fmt.Errorf("atespace %q already exists", args[2])
		}
		f.atespaces[args[2]] = true
	case "get actor-template":
		img, ok := f.templates[args[2]]
		if !ok {
			return nil, fmt.Errorf("actor template %q not found", args[2])
		}
		return []byte(`{"spec":{"containers":[{"image":"` + img + `"}]}}`), nil
	case "create actor-template":
		b, err := os.ReadFile(args[3])
		if err != nil {
			f.t.Fatal(err)
		}
		name := "counter-mode3"
		if strings.Contains(string(b), "name: counter-iso") {
			name = "counter-iso"
		}
		f.templates[name], f.created[name] = testCounter, string(b)
	case "get actor":
		st, ok := f.actors[args[2]]
		if !ok {
			return nil, fmt.Errorf("actor %q not found", args[2])
		}
		return []byte(`{"metadata":{"uid":"uid-` + args[2] + `"},"status":{"state":"` + st +
			`","workerAssignment":{"workerPod":"pod-1"}}}`), nil
	case "create actor":
		f.actors[args[2]] = stateSuspended
	case "resume actor":
		f.actors[args[2]] = stateRunning
	case "delete actor":
		if _, ok := f.actors[args[2]]; !ok {
			return nil, fmt.Errorf("actor %q not found", args[2])
		}
		delete(f.actors, args[2])
	}
	return nil, nil
}

func (f *fake) has(entries ...string) error {
	i := 0
	for _, l := range f.log {
		if i < len(entries) && strings.HasPrefix(l, entries[i]) {
			i++
		}
	}
	if i < len(entries) {
		return fmt.Errorf("log lacks %q (in this order):\n%s", entries[i], strings.Join(f.log, "\n"))
	}
	return nil
}

// fakeECR is the counter repository: owner is its owner tag (newFake makes
// it c1.us-east-1's), descErr what DescribeRepositories fails with.
type fakeECR struct {
	in      *ecr.BatchDeleteImageInput
	out     *ecr.BatchDeleteImageOutput
	err     error
	owner   string
	descErr error
}

const counterARN = "arn:aws:ecr:us-east-1:111122223333:repository/c1/counter"

func (e *fakeECR) DescribeRepositories(_ context.Context, in *ecr.DescribeRepositoriesInput, _ ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	if e.descErr != nil {
		return nil, e.descErr
	}
	if !slices.Equal(in.RepositoryNames, []string{"c1/counter"}) {
		return nil, fmt.Errorf("described %v", in.RepositoryNames)
	}
	return &ecr.DescribeRepositoriesOutput{Repositories: []ecrtypes.Repository{{RepositoryArn: aws.String(counterARN)}}}, nil
}

func (e *fakeECR) ListTagsForResource(_ context.Context, in *ecr.ListTagsForResourceInput, _ ...func(*ecr.Options)) (*ecr.ListTagsForResourceOutput, error) {
	if aws.ToString(in.ResourceArn) != counterARN {
		return nil, fmt.Errorf("tags of %s", aws.ToString(in.ResourceArn))
	}
	tags := []ecrtypes.Tag{{Key: aws.String("auto-delete"), Value: aws.String("no")}}
	if e.owner != "" {
		tags = append(tags, ecrtypes.Tag{Key: aws.String(awsx.OwnerTagKey), Value: aws.String(e.owner)})
	}
	return &ecr.ListTagsForResourceOutput{Tags: tags}, nil
}

func (e *fakeECR) BatchDeleteImage(_ context.Context, in *ecr.BatchDeleteImageInput, _ ...func(*ecr.Options)) (*ecr.BatchDeleteImageOutput, error) {
	e.in = in
	if e.out == nil {
		e.out = &ecr.BatchDeleteImageOutput{}
	}
	return e.out, e.err
}

func testOptions(t *testing.T) plugin.Options {
	root := t.TempDir()
	tree := filepath.Join(root, "substrate-"+Tag)
	if err := os.MkdirAll(filepath.Join(tree, "demos", "counter"), 0o755); err != nil {
		t.Fatal(err)
	}
	return plugin.Options{Cluster: "c1", Region: "us-east-1", Profile: "p", Release: plugin.Release{Tag: Tag, ImageRepo: ImageRepo, Tree: tree}, TreeRoot: root, Kubeconfig: "/kc", AutoMode: true}
}

// The control plane installs the fork's pinned commit with its images and
// the NetworkWorkers gate; the plugin has no AWS half.
func TestRegisteredNeeds(t *testing.T) {
	p, ok := plugin.Get(ID)
	if !ok {
		t.Fatal("mode3 is not registered")
	}
	want := plugin.Source{Repo: "https://github.com/csantanapr/substrate.git", Commit: "9f36a0d7f2abca950f25fc7bd023752d3852504c",
		Tag: "v1.0.0-mode3.9f36a0d", ImageRepo: "ghcr.io/csantanapr/substrate"}
	if p.Needs.Source == nil || *p.Needs.Source != want || !slices.Equal(p.Needs.Preview, []string{"NetworkWorkers"}) {
		t.Errorf("needs = %+v", p.Needs)
	}
	if p.SetupAWS || p.Deploy == nil || p.Verify == nil || p.Remove == nil {
		t.Errorf("plugin = %+v", p)
	}
}

// checkKata is what the embedded Kata manifest must be: kata-deploy's six
// objects (no helm hook left), every image pinned by digest, and the
// DaemonSet on the microvm nodes only.
func checkKata(b []byte) error {
	nodes, err := kio.FromBytes(b)
	if err != nil {
		return err
	}
	var kinds []string
	for _, n := range nodes {
		kinds = append(kinds, n.GetKind()+"/"+n.GetName())
		if _, ok := n.GetAnnotations()["helm.sh/hook"]; ok {
			return fmt.Errorf("%s/%s is a helm hook", n.GetKind(), n.GetName())
		}
		if n.GetKind() == "DaemonSet" {
			sel, err := n.GetString(`spec.template.spec.nodeSelector.ate\.dev/pool`)
			if err != nil || sel != "microvm" {
				return fmt.Errorf("the DaemonSet's nodeSelector is %q (%v)", sel, err)
			}
		}
	}
	slices.Sort(kinds)
	want := []string{"ClusterRole/kata-deploy-role", "ClusterRoleBinding/kata-deploy-rb", "DaemonSet/kata-deploy",
		"RuntimeClass/kata", "RuntimeClass/kata-clh", "ServiceAccount/kata-deploy-sa"}
	if !slices.Equal(kinds, want) {
		return fmt.Errorf("objects = %q, want %q", kinds, want)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "image:" && !strings.Contains(f[1], "@sha256:") {
			return fmt.Errorf("image %s is not pinned by digest", f[1])
		}
	}
	return nil
}

func TestKataManifest(t *testing.T) {
	if err := checkKata(kataManifest); err != nil {
		t.Fatal(err)
	}
	hook := string(kataManifest) + "---\napiVersion: batch/v1\nkind: Job\nmetadata:\n  name: x\n  annotations:\n    helm.sh/hook: post-delete\n"
	if err := checkKata([]byte(hook)); err == nil {
		t.Error("a hook was accepted")
	}
	unpinned := strings.Replace(string(kataManifest), "kata-deploy@sha256:47c4cf29c9e8cf9bb7debab58761b672e1d4c93ee5e92db7b442ee00e7d561cc", "kata-deploy:3.32.0", 1)
	if err := checkKata([]byte(unpinned)); err == nil || !strings.Contains(err.Error(), "kata-deploy:3.32.0") {
		t.Errorf("an unpinned image: %v", err)
	}
}

func TestFill(t *testing.T) {
	if s, err := fill("a ${X} b", map[string]string{"X": "1"}); err != nil || s != "a 1 b" {
		t.Errorf("fill = %q, %v", s, err)
	}
	if _, err := fill("a ${X} ${Y}", map[string]string{"X": "1"}); err == nil || !strings.Contains(err.Error(), "${Y}") {
		t.Errorf("a missing value: %v", err)
	}
	for name, m := range map[string]string{"pool": poolManifest, "template": templateManifest} {
		if _, err := fill(m, map[string]string{"WORKER_IMAGE": "w", "COUNTER_IMAGE": "c"}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Deploy builds the fork's kubectl-ate and counter, installs Kata, then the
// pool with the pinned fork worker and the counter image, and waits for the
// golden snapshot. Nothing runs helm, docker or a shell (fake.exec refuses).
func TestDeploy(t *testing.T) {
	f := newFake(t)
	f.out["get runtimeclass kata"] = "kata-clh"
	f.out["get nodes -l katacontainers.io/kata-runtime=true"] = "node/a\nnode/b\n"
	var out bytes.Buffer
	if err := deploy(context.Background(), &out, testOptions(t)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// The counter is pushed under mode 3's own tag, not the release tag the
	// demo's counter carries.
	if err := f.has("build kubectl-ate substrate-"+Tag, "counter image substrate-"+Tag+" microvm "+CounterTag, "pin "+ImageRepo+"/ateworker-gvisor:"+Tag,
		"kubectl apply -f -", "kubectl -n kube-system rollout status ds/kata-deploy --timeout=10m", "kubectl get runtimeclass kata",
		"kubectl get nodes -l katacontainers.io/kata-runtime=true", "kubectl apply -f -", "ate create atespace ate-demo-counter-mode3",
		"ate get actor-template counter-mode3", "ate create actor-template -f", "golden ate-demo-counter-mode3/counter-mode3"); err != nil {
		t.Error(err)
	}
	if f.stdin["apply -f -"] == string(kataManifest) {
		t.Error("the last apply was Kata, not the pool")
	}
	pool := f.stdin["apply -f -"]
	if !strings.Contains(pool, "workerImage: "+testWorker) || !strings.Contains(pool, "runtimeClassName: kata") || strings.Contains(pool, "${") {
		t.Errorf("pool applied:\n%s", pool)
	}
	if tmpl := f.created["counter-mode3"]; !strings.Contains(tmpl, "image: "+testCounter) || strings.Contains(tmpl, "${") {
		t.Errorf("template created:\n%s", tmpl)
	}
	if !strings.Contains(out.String(), "RuntimeClass kata -> handler kata-clh on node/a, node/b") {
		t.Errorf("output:\n%s", out.String())
	}
}

// Both directions for a rerun: a template that runs this counter is kept;
// one that runs another image stops the deploy, and nothing is created.
func TestDeployKeepsItsTemplateAndRefusesAnother(t *testing.T) {
	f := newFake(t)
	f.out["get nodes -l katacontainers.io/kata-runtime=true"] = "node/a\n"
	f.atespaces["ate-demo-counter-mode3"] = true
	f.templates["counter-mode3"] = testCounter
	if err := deploy(context.Background(), io.Discard, testOptions(t)); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("rerun created %v", f.created)
	}
	f = newFake(t)
	f.out["get nodes -l katacontainers.io/kata-runtime=true"] = "node/a\n"
	f.templates["counter-mode3"] = "example.com/other@sha256:beef"
	err := deploy(context.Background(), io.Discard, testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "runs example.com/other@sha256:beef, not "+testCounter) || len(f.created) != 0 {
		t.Errorf("other image: err = %v, created %v", err, f.created)
	}
}

// Kata that labels no node is a failed install; a missing fork tree stops
// before anything runs.
func TestDeployRefusesNoKataNodeAndNoTree(t *testing.T) {
	f := newFake(t)
	err := deploy(context.Background(), io.Discard, testOptions(t))
	if err == nil || !strings.Contains(err.Error(), "labeled no node") || len(f.created) != 0 {
		t.Errorf("no kata node: err = %v", err)
	}
	f = newFake(t)
	o := testOptions(t)
	o.Tree = filepath.Join(t.TempDir(), "substrate-"+Tag)
	if err := deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "--deploy-control-plane --plugins mode3") || len(f.log) != 0 {
		t.Errorf("no tree: err = %v, ran %q", err, f.log)
	}
}

const testNodes = `{"items":[
 {"metadata":{"name":"n1","labels":{"ate.dev/pool":"microvm","katacontainers.io/kata-runtime":"true"}}},
 {"metadata":{"name":"n2","labels":{"ate.dev/pool":"gvisor"}}}]}`

func pods(specs ...string) []byte {
	var items []string
	for i, s := range specs {
		rc, node, _ := strings.Cut(s, "@")
		items = append(items, fmt.Sprintf(`{"metadata":{"name":"w%d"},"spec":{"runtimeClassName":%q,"nodeName":%q}}`, i, rc, node))
	}
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

// Both directions: every worker under kata on a microvm node passes; one
// under another RuntimeClass, on another node, or none at all fails.
func TestCheckWorkers(t *testing.T) {
	labels := map[string]string{"ate.dev/pool": "microvm"}
	table, err := CheckWorkers(pods("kata@n1", "kata@n1"), []byte(testNodes), "kata", labels)
	if err != nil || !strings.Contains(table, "worker Pod w1  RuntimeClass kata  node n1 (ate.dev/pool=microvm)") {
		t.Errorf("good pool: %q, %v", table, err)
	}
	for name, c := range map[string]struct {
		pods []byte
		want string
	}{
		"runc":    {pods("kata@n1", "@n1"), `w1 runs under "", not RuntimeClass kata`},
		"node":    {pods("kata@n2"), `whose ate.dev/pool is "gvisor", not "microvm"`},
		"unknown": {pods("kata@n9"), "on no known node"},
		"none":    {pods(), "no worker Pod"},
	} {
		if _, err := CheckWorkers(c.pods, []byte(testNodes), "kata", labels); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// Verify checks the workers, runs the round trip, then the delete check:
// a new actor resumed, deleted, and its UID's TerminateWorkload in the
// worker's logs.
func TestVerify(t *testing.T) {
	f := newFake(t)
	f.out["-n ate-demo-counter-mode3 get pods -l ate.dev/worker-transport=network"] = string(pods("kata@n1"))
	f.out["get nodes -o json"] = testNodes
	f.out["-n ate-demo-counter-mode3 logs pod-1"] = "x Handle RPC method=TerminateWorkload actor=uid-demo-mode3-delete\n"
	var out bytes.Buffer
	if err := Kata.Verify(context.Background(), &out, testOptions(t)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := f.has("build kubectl-ate", "roundtrip ate-demo-counter-mode3/counter-mode3/demo-mode3",
		"ate get actor demo-mode3-delete", "ate create actor demo-mode3-delete -a ate-demo-counter-mode3 --template counter-mode3",
		"ate resume actor demo-mode3-delete", "ate delete actor demo-mode3-delete", "kubectl -n ate-demo-counter-mode3 logs pod-1 --all-containers"); err != nil {
		t.Error(err)
	}
	if !strings.Contains(out.String(), "PASS: the worker in Pod pod-1 tore down demo-mode3-delete's sandbox") {
		t.Errorf("output:\n%s", out.String())
	}

	// The worker logs another actor's teardown only: the check fails.
	f = newFake(t)
	f.out["-n ate-demo-counter-mode3 get pods -l ate.dev/worker-transport=network"] = string(pods("kata@n1"))
	f.out["get nodes -o json"] = testNodes
	f.out["-n ate-demo-counter-mode3 logs pod-1"] = "x Handle RPC method=TerminateWorkload actor=uid-other\n"
	if err := Kata.Verify(context.Background(), io.Discard, testOptions(t)); err == nil || !strings.Contains(err.Error(), "logged no TerminateWorkload") {
		t.Errorf("no teardown: %v", err)
	}
}

func TestLogsTerminate(t *testing.T) {
	if !LogsTerminate("a\nHandle RPC TerminateWorkload uid-1 ok\n", "uid-1") {
		t.Error("the teardown line was missed")
	}
	for _, logs := range []string{"Handle RPC TerminateWorkload uid-2", "Handle RPC StartWorkload uid-1", "TerminateWorkload uid-1"} {
		if LogsTerminate(logs, "uid-1") {
			t.Errorf("%q passed", logs)
		}
	}
}

// Remove deletes the ate API objects and the Namespace, then Kata's
// DaemonSet (waiting for its Pods' cleanup) before its RBAC and
// RuntimeClasses, then the counter image, and says the fork still runs.
func TestRemove(t *testing.T) {
	f := newFake(t)
	f.out["get namespace ate-system"] = "namespace/ate-system\n"
	f.actors["demo-mode3"] = stateRunning
	var out bytes.Buffer
	if err := remove(context.Background(), &out, testOptions(t)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := f.has("kubectl get namespace ate-system", "build kubectl-ate", "ate delete actor demo-mode3 ", "ate delete actor demo-mode3-delete",
		"ate delete actor-template counter-mode3 -a ate-demo-counter-mode3", "ate delete atespace ate-demo-counter-mode3",
		"kubectl delete namespace ate-demo-counter-mode3 --ignore-not-found --wait=true",
		"kubectl -n kube-system delete ds/kata-deploy --ignore-not-found --cascade=foreground --wait=true",
		"kubectl delete -f - --ignore-not-found --wait=true"); err != nil {
		t.Error(err)
	}
	if f.stdin["delete -f - --ignore-not-found --wait=true"] != string(kataManifest) {
		t.Error("the delete -f was not the Kata manifest")
	}
	// Its own tag, never the release tag the demo's counter carries.
	if in := f.ecr.in; in == nil || aws.ToString(in.RepositoryName) != "c1/counter" || aws.ToString(in.ImageIds[0].ImageTag) != CounterTag || CounterTag == Tag {
		t.Errorf("ECR delete = %+v", in)
	}
	// No advice to redeploy without mode3: that deploy is refused.
	if !strings.HasSuffix(out.String(), RemovedNote+"\n") || strings.Contains(RemovedNote, "--deploy-control-plane") {
		t.Errorf("output:\n%s", out.String())
	}

	// Without ate-system there is no ate API to delete from.
	f = newFake(t)
	if err := remove(context.Background(), io.Discard, testOptions(t)); err != nil {
		t.Fatal(err)
	}
	for _, l := range f.log {
		if strings.HasPrefix(l, "ate ") {
			t.Errorf("ran %q with no control plane", l)
		}
	}
	// An ate API error other than not found stops the removal.
	f = newFake(t)
	f.out["get namespace ate-system"] = "namespace/ate-system\n"
	f.ateErr["delete actor-template counter-mode3"] = errors.New("permission denied")
	if err := remove(context.Background(), io.Discard, testOptions(t)); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("ate error: %v", err)
	}
}

// Both directions: a deleted tag, a gone repository and a gone tag are done;
// any other failure is an error.
func TestDeleteCounterImage(t *testing.T) {
	o := testOptions(t)
	for name, c := range map[string]struct {
		out     *ecr.BatchDeleteImageOutput
		err     error
		descErr error
		wantErr bool
	}{
		"deleted":            {&ecr.BatchDeleteImageOutput{}, nil, nil, false},
		"no repo":            {nil, nil, &ecrtypes.RepositoryNotFoundException{}, false},
		"repo gone mid-call": {nil, &ecrtypes.RepositoryNotFoundException{}, nil, false},
		"no image":           {&ecr.BatchDeleteImageOutput{Failures: []ecrtypes.ImageFailure{{FailureCode: ecrtypes.ImageFailureCodeImageNotFound}}}, nil, nil, false},
		"denied":             {nil, errors.New("AccessDenied"), nil, true},
		"describe denied":    {nil, nil, errors.New("AccessDenied"), true},
		"failure":            {&ecr.BatchDeleteImageOutput{Failures: []ecrtypes.ImageFailure{{FailureCode: ecrtypes.ImageFailureCodeKmsError}}}, nil, nil, true},
	} {
		f := newFake(t)
		f.ecr.out, f.ecr.err, f.ecr.descErr = c.out, c.err, c.descErr
		if err := deleteCounterImage(context.Background(), io.Discard, o); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Review R9, both ways: the image is deleted from a repository carrying
// this deployment's owner tag, and left alone in one of the same name with
// another owner or none.
func TestDeleteCounterImageOnlyFromAnOwnedRepository(t *testing.T) {
	o := testOptions(t)
	for owner, deletes := range map[string]bool{"c1.us-east-1": true, "c1.us-west-2": false, "other.us-east-1": false, "": false} {
		f := newFake(t)
		f.ecr.owner = owner
		var out bytes.Buffer
		if err := deleteCounterImage(context.Background(), &out, o); err != nil {
			t.Fatalf("owner %q: %v", owner, err)
		}
		if (f.ecr.in != nil) != deletes {
			t.Errorf("owner %q: deleted %v, want %v\n%s", owner, f.ecr.in != nil, deletes, out.String())
		}
		if !deletes && !strings.Contains(out.String(), "is not owned by c1.us-east-1") {
			t.Errorf("owner %q: no reason given:\n%s", owner, out.String())
		}
	}
}
