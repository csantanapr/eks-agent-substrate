package efs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// fakeKube answers the children the efs plugin starts, by argv prefix, and
// logs each one; an argv it does not know fails the test's run.
type stdinOf struct {
	argv string
	data []byte
}

type fakeKube struct {
	t       *testing.T
	answers map[string]string // argv prefix -> stdout
	fail    map[string]bool   // argv prefix -> exit 1
	log     []string
	stdin   []stdinOf
}

func (f *fakeKube) exec(_ context.Context, w io.Writer, c plugin.Cmd) error {
	argv := strings.Join(c.Argv, " ")
	for _, a := range c.Argv {
		if a == "-c" || a == "sh" || a == "bash" {
			return fmt.Errorf("a shell ran: %s", argv)
		}
	}
	f.log = append(f.log, argv)
	if c.Stdin != nil {
		f.stdin = append(f.stdin, stdinOf{argv, c.Stdin})
	}
	best := ""
	for p := range f.answers {
		if strings.HasPrefix(argv, p) && len(p) > len(best) {
			best = p
		}
	}
	for p := range f.fail {
		if strings.HasPrefix(argv, p) && len(p) >= len(best) {
			io.WriteString(w, "NotFound")
			return fmt.Errorf("%s: exit status 1", argv)
		}
	}
	if best == "" {
		return nil
	}
	out := c.Stdout
	if out == nil {
		out = w
	}
	io.WriteString(out, f.answers[best])
	return nil
}

func install(t *testing.T, f *fakeKube) plugin.Options {
	t.Helper()
	f.t = t
	prevExec, prevWait := plugin.Exec, Wait
	plugin.Exec, Wait = f.exec, 0
	t.Cleanup(func() { plugin.Exec, Wait = prevExec, prevWait })
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "substrate-v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "substrate-v1.0.0", "go.mod"), []byte("module go.ate.dev/substrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return plugin.Options{Cluster: "demo", Region: "us-west-2", Profile: "p", Release: plugin.Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate",
		Tree: filepath.Join(tree, "substrate-v1.0.0")}, TreeRoot: tree, Kubeconfig: "/kc", Tags: "project=agent-substrate auto-delete=no"}
}

// The counter demo's template as `kubectl ate get actor-template -o json`
// prints it.
const counterJSON = `{"metadata":{"atespace":"ate-demo-counter","name":"counter","uid":"u1","version":"3"},
 "workerSelector":{"matchLabels":{"workload":"counter"}},
 "containers":[{"name":"counter","image":"111122223333.dkr.ecr.us-west-2.amazonaws.com/demo/counter@sha256:abc",
   "command":["/ko-app/counter","--extra-port=9090"],"wakeupProbe":{"httpGet":{"path":"/readyz","port":80}},
   "volumeMounts":[{"name":"data","mountPath":"/home/counter"}]}],
 "resources":{"limits":{"cpuMillicores":1000,"memoryBytes":536870912}},
 "snapshotConfig":{"preferredFidelity":"SNAPSHOT_FIDELITY_MEMORY"},
 "volumes":[{"name":"data","durableDir":{}}],
 "status":{"goldenSnapshotStatus":{"goldenTag":{"name":"g1"}}}}`

func TestVolumeTemplate(t *testing.T) {
	for name, in := range map[string]string{"top level": counterJSON, "spec": `{"spec":` + counterJSON + `}`} {
		out, err := VolumeTemplate([]byte(in), "ate-demo-counter", "counter-efs", "efs-ate")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		s := string(out)
		for _, want := range []string{`"name":"counter-efs"`, `"storageClassName":"efs-ate"`, `"capacityBytes":1073741824`,
			`"image":"111122223333.dkr.ecr.us-west-2.amazonaws.com/demo/counter@sha256:abc"`, `"--extra-port=9090"`, `"workload":"counter"`,
			`"preferredFidelity":"SNAPSHOT_FIDELITY_MEMORY"`} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: lacks %s in %s", name, want, s)
			}
		}
		for _, never := range []string{"durableDir", "status", "goldenTag", `"uid"`, `"version"`} {
			if strings.Contains(s, never) {
				t.Errorf("%s: kept %s in %s", name, never, s)
			}
		}
	}
	noData := strings.Replace(counterJSON, `"volumeMounts":[{"name":"data","mountPath":"/home/counter"}]`, `"volumeMounts":[]`, 1)
	if _, err := VolumeTemplate([]byte(noData), "a", "b", "c"); err == nil || !strings.Contains(err.Error(), "no data volume") {
		t.Errorf("a template without the data mount: %v", err)
	}
}

func TestOwnerTagsArg(t *testing.T) {
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Tags: "project=agent-substrate auto-delete=no"}
	got, err := OwnerTagsArg(o)
	if want := "efs.csi.aws.com/cluster:true agent-substrate-on-eks/deployment:demo.us-west-2 auto-delete:no project:agent-substrate"; err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
	o.Tags = "team=a:b"
	if _, err := OwnerTagsArg(o); err == nil {
		t.Error("accepted a tag value with ':'")
	}
}

func TestFileCounter(t *testing.T) {
	if n, err := FileCounter("preserved memory count: 3 | preserved file counter: 7 | preserved rootfs counter: 3"); err != nil || n != 7 {
		t.Errorf("got %d, %v", n, err)
	}
	if _, err := FileCounter("hello"); err == nil {
		t.Error("read a counter from an answer without one")
	}
}

// classJSON is `kubectl get storageclass -o json` for a class with the
// given labels and parameters.
func classJSON(labels, fs, uid string) string {
	return `{"metadata":{"name":"efs-ate","labels":{` + labels + `}},"parameters":{"fileSystemId":"` + fs + `","uid":"` + uid + `"}}`
}

const ours = `"app.kubernetes.io/managed-by":"substrate-eks"`

func TestApplyStorageClassReplacesOnlyAChangedClass(t *testing.T) {
	for _, tc := range []struct {
		have        string
		wantReplace bool
	}{{"", false}, {classJSON(ours, "fs-1", "0"), false}, {classJSON(ours, "fs-2", "0"), true}, {classJSON(ours, "fs-1", "1000"), true}} {
		f := &fakeKube{answers: map[string]string{"kubectl --kubeconfig /kc get storageclass efs-ate": tc.have}}
		o := install(t, f)
		if err := ApplyStorageClass(context.Background(), io.Discard, o, "efs-ate", "fs-1", "0", []byte("sc")); err != nil {
			t.Fatal(err)
		}
		deleted := strings.Contains(strings.Join(f.log, "\n"), "delete storageclass efs-ate")
		if deleted != tc.wantReplace {
			t.Errorf("have %q: replaced=%v, want %v (%v)", tc.have, deleted, tc.wantReplace, f.log)
		}
	}
}

// Review R10, both ways: a class of the same name without the installer's
// managed-by label (none, or another manager's) is refused by name and
// neither replaced nor applied over, even with the same parameters; the
// installer's own is replaced or kept as above.
func TestApplyStorageClassRefusesAClassItDidNotMake(t *testing.T) {
	for _, labels := range []string{"", `"app.kubernetes.io/managed-by":"Helm"`} {
		for _, fs := range []string{"fs-1", "fs-2"} {
			f := &fakeKube{answers: map[string]string{"kubectl --kubeconfig /kc get storageclass efs-ate": classJSON(labels, fs, "0")}}
			o := install(t, f)
			err := ApplyStorageClass(context.Background(), io.Discard, o, "efs-ate", "fs-1", "0", []byte("sc"))
			if err == nil || !strings.Contains(err.Error(), "StorageClass efs-ate exists without the label "+managedByKey+"="+managedBy) {
				t.Errorf("labels {%s}, %s: want the refusal, got %v", labels, fs, err)
			}
			if l := strings.Join(f.log, "\n"); strings.Contains(l, "delete storageclass") || strings.Contains(l, "apply") {
				t.Errorf("labels {%s}, %s: touched the class:\n%s", labels, fs, l)
			}
		}
	}
}

// Both ways: Remove's DeleteStorageClass deletes the installer's class and
// leaves one without its label, saying so; a missing class is done.
func TestDeleteStorageClassOnlyTheInstallers(t *testing.T) {
	for have, wantDelete := range map[string]bool{classJSON(ours, "fs-1", "0"): true, classJSON("", "fs-1", "0"): false, "": false} {
		f := &fakeKube{answers: map[string]string{"kubectl --kubeconfig /kc get storageclass efs-ate": have}}
		o := install(t, f)
		var out strings.Builder
		if err := DeleteStorageClass(context.Background(), &out, o, "efs-ate"); err != nil {
			t.Fatal(err)
		}
		deleted := strings.Contains(strings.Join(f.log, "\n"), "delete storageclass efs-ate --ignore-not-found")
		if deleted != wantDelete {
			t.Errorf("have %q: deleted=%v, want %v", have, deleted, wantDelete)
		}
		if have != "" && !wantDelete && !strings.Contains(out.String(), "left alone") {
			t.Errorf("have %q: no reason given:\n%s", have, out.String())
		}
	}
}

// Every class the plugins render carries the label the two functions above
// look for.
func TestStorageTmplCarriesTheManagedByLabel(t *testing.T) {
	sc, err := Render(StorageTmpl, StorageArgs{Name: "efs-ate", Mode: "efs-ap", FileSystem: "fs-1", UID: UID, GID: GID, BasePath: "/ate"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sc), "\n    "+managedByKey+": "+managedBy+"\n") {
		t.Errorf("rendered StorageClass lacks %s: %s:\n%s", managedByKey, managedBy, sc)
	}
}

// deployAnswers are the reads of a cluster with the add-on, the efs
// plugin's file system and the counter demo on gVisor only.
func deployAnswers() map[string]string {
	return map[string]string{
		"kubectl --kubeconfig /kc -n kube-system get deployment/efs-csi-controller -o jsonpath={.spec.selector.matchLabels}":     `{"app":"efs-csi-controller"}`,
		"kubectl --kubeconfig /kc -n kube-system get deployment/efs-csi-controller-ate -o jsonpath={.spec.selector.matchLabels}": `{"app":"efs-csi-controller-ate"}`,
		"kubectl --kubeconfig /kc -n kube-system get pods":                                                                       `{"items":[{"metadata":{"name":"p"},"spec":{"containers":[{"env":[{"name":"AWS_CONTAINER_CREDENTIALS_FULL_URI"}]}]}}]}`,
		"kubectl --kubeconfig /kc -n kube-system get deployment efs-csi-controller -o":                                           "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-efs-csi-driver:v3.5.3",
		"aws efs describe-file-systems --creation-token demo-actors":                                                             "fs-1",
		"go build": "",
	}
}

func TestDeploy(t *testing.T) {
	f := &fakeKube{answers: deployAnswers(), fail: map[string]bool{}}
	o := install(t, f)
	bin := KubectlAtePath("v1.0.0")
	f.answers[bin+" get actor-template counter -a ate-demo-counter"] = counterJSON
	f.fail[bin+" get actor-template counter-efs"] = true
	f.fail[bin+" get actor-template counter-microvm"] = true
	if err := deploy(context.Background(), io.Discard, o); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.log, "\n"))
	}
	var controller, storage, tmpl []byte
	for _, si := range f.stdin {
		argv, in := si.argv, si.data
		switch {
		case bytes.Contains(in, []byte("kind: Deployment")):
			controller = in
		case bytes.Contains(in, []byte("kind: StorageClass")):
			storage = in
		case strings.Contains(argv, "--preview create actor-template"):
			tmpl = in
		}
	}
	for _, want := range []string{"type: ClusterIP", `image: "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-efs-csi-driver:v3.5.3"`,
		`"--tags=efs.csi.aws.com/cluster:true agent-substrate-on-eks/deployment:demo.us-west-2 auto-delete:no project:agent-substrate"`,
		`spiffe://cluster.local/ns/ate-system/sa/ate-api-server`, "image: \"" + envoyImage + "\""} {
		if !bytes.Contains(controller, []byte(want)) {
			t.Errorf("controller lacks %s", want)
		}
	}
	for _, never := range []string{"LoadBalancer", "NodePort", "kind: Ingress", "{{"} {
		if bytes.Contains(controller, []byte(never)) || bytes.Contains(storage, []byte(never)) {
			t.Errorf("rendered %s", never)
		}
	}
	for _, want := range []string{"kind: CSIDriverConfig", "name: efs-ate", "provisioningMode: efs-ap", "fileSystemId: fs-1", `uid: "0"`, "basePath: /ate"} {
		if !bytes.Contains(storage, []byte(want)) {
			t.Errorf("storage lacks %s:\n%s", want, storage)
		}
	}
	if !bytes.Contains(tmpl, []byte(`"storageClassName":"efs-ate"`)) {
		t.Errorf("template %s", tmpl)
	}
	log := strings.Join(f.log, "\n")
	if strings.Contains(log, "counter-microvm -a ate-demo-counter-microvm -o json\n"+bin+" --preview") {
		t.Error("made a microvm template with no microvm counter")
	}
	if !strings.Contains(log, "aws efs describe-file-systems --creation-token demo-actors --query FileSystems[0].FileSystemId --output text --region us-west-2 --profile p") {
		t.Errorf("file system read without the profile and Region:\n%s", log)
	}
}

// Deploy stops when the counter demo is not there to add counter-efs to.
func TestDeployNeedsTheCounterDemo(t *testing.T) {
	f := &fakeKube{answers: deployAnswers(), fail: map[string]bool{KubectlAtePath("v1.0.0") + " get actor-template": true}}
	o := install(t, f)
	if err := deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "deploy it first (--deploy-demo)") {
		t.Fatalf("want the counter demo prerequisite, got %v", err)
	}
}

// roundTripKube is a cluster where actor efs-gvisor counts on access point
// fsap-9. lose makes its file counter restart on resume; keep makes the
// access point outlive the actor.
func roundTripKube(t *testing.T, lose, keep bool) (*fakeKube, plugin.Options, Storage) {
	return roundTripKubeDial(t, lose, keep, 0)
}

// roundTripKubeDial is roundTripKube whose proxy fails the first resets
// requests after the suspend, as a dial with an expired cluster token does.
// Any request resumes the actor, readyz included, as on a real cluster.
func roundTripKubeDial(t *testing.T, lose, keep bool, resets int) (*fakeKube, plugin.Options, Storage) {
	bin := KubectlAtePath("v1.0.0")
	f := &fakeKube{answers: map[string]string{
		bin + " get actor-template counter-efs": "{}",
	}, fail: map[string]bool{}}
	o := install(t, f)
	state := "ACTOR_STATE_RUNNING"
	deleted := false
	prevGet, prevPort := Get, FreePort
	t.Cleanup(func() { Get, FreePort = prevGet, prevPort })
	FreePort = func() (int, error) { return 18080, nil }
	n := 0
	Get = func(_ context.Context, url string, _ time.Duration) (string, error) {
		if state == "ACTOR_STATE_SUSPENDED" {
			if resets > 0 {
				resets--
				return "", errors.New("read tcp 127.0.0.1:58104->127.0.0.1:18080: read: connection reset by peer")
			}
			state = "ACTOR_STATE_RUNNING"
			if lose {
				n = 0
			}
		}
		if strings.HasSuffix(url, "readyz") {
			return "ok", nil
		}
		n++
		return fmt.Sprintf("preserved file counter: %d", n), nil
	}
	plugin.Exec = func(ctx context.Context, w io.Writer, c plugin.Cmd) error {
		argv := strings.Join(c.Argv, " ")
		switch {
		case strings.HasPrefix(argv, bin+" get actor efs-gvisor"):
			fmt.Fprintf(c.Stdout, `{"status":{"state":%q,"assignedNode":"node-a","externalVolumes":{"volumes":[{"storageVolumeId":"fs-1::fsap-9"}]}}}`, state)
			return nil
		case strings.HasPrefix(argv, bin+" suspend actor efs-gvisor"):
			state = "ACTOR_STATE_SUSPENDED"
			return nil
		case strings.HasPrefix(argv, bin+" delete actor efs-gvisor"):
			deleted = true
			return nil
		}
		return f.exec(ctx, w, c)
	}
	s := Storage{Name: "efs", Template: Template, AccessPoints: func(context.Context) ([]string, error) {
		if deleted && !keep {
			return []string{"fsap-1"}, nil
		}
		return []string{"fsap-1", "fsap-9"}, nil
	}}
	return f, o, s
}

func TestRoundTrip(t *testing.T) {
	f, o, s := roundTripKube(t, false, false)
	var out bytes.Buffer
	if err := RoundTrip(context.Background(), &out, o, Ate{Bin: KubectlAtePath("v1.0.0"), Kubeconfig: "/kc"}, s, Classes[0], false); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), strings.Join(f.log, "\n"))
	}
	for _, want := range []string{"volume on access point fsap-9", "PASS: the file counter on efs continued", "PASS: deleting the actor deleted access point fsap-9"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// Measured live on sub-e2e: after the actor slept, the proxy's first dial
// failed with a connection reset (its cached cluster token had expired with
// the AWS session) and the check failed though the volume was fine. A reset
// after the suspend is now retried through readyz; the counter still has to
// go up by exactly one.
func TestRoundTripSurvivesAFailedDialAfterTheSuspend(t *testing.T) {
	_, o, s := roundTripKubeDial(t, false, false, 2)
	var out bytes.Buffer
	if err := RoundTrip(context.Background(), &out, o, Ate{Bin: KubectlAtePath("v1.0.0"), Kubeconfig: "/kc"}, s, Classes[0], false); err != nil {
		t.Fatalf("two resets after the suspend: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "PASS: the file counter on efs continued") {
		t.Errorf("no PASS line:\n%s", out.String())
	}
	// A lost counter is still caught through the retry.
	_, o, s = roundTripKubeDial(t, true, false, 2)
	if err := RoundTrip(context.Background(), io.Discard, o, Ate{Bin: KubectlAtePath("v1.0.0"), Kubeconfig: "/kc"}, s, Classes[0], false); err == nil || !strings.Contains(err.Error(), "lost its file counter") {
		t.Errorf("lost counter behind resets: %v", err)
	}
}

// The check fails when the file counter does not survive, and when the
// access point outlives its actor.
func TestRoundTripFails(t *testing.T) {
	_, o, s := roundTripKube(t, true, false)
	k := Ate{Bin: KubectlAtePath("v1.0.0"), Kubeconfig: "/kc"}
	if err := RoundTrip(context.Background(), io.Discard, o, k, s, Classes[0], false); err == nil || !strings.Contains(err.Error(), "lost its file counter") {
		t.Errorf("lost counter: %v", err)
	}
	_, o, s = roundTripKube(t, false, true)
	if err := RoundTrip(context.Background(), io.Discard, o, k, s, Classes[0], false); err == nil || !strings.Contains(err.Error(), "still there") {
		t.Errorf("kept access point: %v", err)
	}
}
