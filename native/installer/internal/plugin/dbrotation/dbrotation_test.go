package dbrotation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// esoFixture has the shape of ESO's release manifest: the controller, the
// webhook and the cert controller, in namespace default.
const esoFixture = `apiVersion: v1
kind: ServiceAccount
metadata: {name: external-secrets-cert-controller, namespace: default}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: external-secrets, namespace: default}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: external-secrets-webhook, namespace: default}
---
apiVersion: v1
kind: Secret
metadata: {name: external-secrets-webhook, namespace: default}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: externalsecrets.external-secrets.io, labels: {external-secrets.io/component: controller}}
spec: {group: external-secrets.io}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: external-secrets-controller}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: external-secrets-controller}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: external-secrets-controller}
subjects:
- {kind: ServiceAccount, name: external-secrets, namespace: default}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: external-secrets-leaderelection, namespace: default}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: external-secrets-leaderelection}
subjects:
- {kind: ServiceAccount, name: external-secrets, namespace: default}
---
apiVersion: v1
kind: Service
metadata: {name: external-secrets-webhook, namespace: default}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: external-secrets-cert-controller, namespace: default}
spec: {template: {spec: {containers: [{name: cert-controller, image: "ghcr.io/external-secrets/external-secrets:v2.15.0"}]}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: external-secrets, namespace: default}
spec:
  template:
    spec:
      containers:
      - name: external-secrets
        image: ghcr.io/external-secrets/external-secrets:v2.15.0
        args: [--concurrent=1]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: external-secrets-webhook, namespace: default}
spec: {template: {spec: {containers: [{name: webhook, image: "ghcr.io/external-secrets/external-secrets:v2.15.0"}]}}}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata: {name: secretstore-validate}
`

const reloaderFixture = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: configs.reloader.external-secrets.io}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: reloader, namespace: external-secrets-reloader}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: reloader-controller}
subjects:
- {kind: ServiceAccount, name: reloader, namespace: external-secrets-reloader}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: reloader, namespace: external-secrets-reloader}
spec: {template: {spec: {containers: [{name: reloader, image: "ghcr.io/external-secrets/reloader:v0.3.0"}]}}}
`

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fixtures points fetch at the fixtures and pins their sums.
func fixtures(t *testing.T, eso, reloader string) {
	t.Helper()
	prevFetch, prevE, prevR := fetch, esoSHA256, reloaderSHA256
	fetch = func(_ context.Context, url string) ([]byte, error) {
		switch url {
		case esoURL:
			return []byte(eso), nil
		case reloaderURL:
			return []byte(reloader), nil
		}
		return nil, fmt.Errorf("unexpected URL %s", url)
	}
	esoSHA256, reloaderSHA256 = sum(esoFixture), sum(reloaderFixture)
	t.Cleanup(func() { fetch, esoSHA256, reloaderSHA256 = prevFetch, prevE, prevR })
}

func parse(t *testing.T, s string) []*yaml.RNode {
	t.Helper()
	ns, err := kio.FromBytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func field(t *testing.T, n *yaml.RNode, path ...string) string {
	t.Helper()
	v, err := n.Pipe(yaml.Lookup(path...))
	if err != nil || v == nil {
		return ""
	}
	s, _ := v.String()
	return strings.TrimSpace(s)
}

// images are the container images of the Deployments in s.
func images(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	for _, n := range parse(t, s) {
		if n.GetKind() != "Deployment" {
			continue
		}
		c, err := container(n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, yaml.GetValue(c.Field("image").Value))
	}
	return out
}

func TestTransformESO(t *testing.T) {
	out, err := transformESO([]byte(esoFixture))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, n := range parse(t, out) {
		kinds = append(kinds, n.GetKind()+"/"+n.GetName())
		if n.GetLabels()[partOfKey] != partOfValue {
			t.Errorf("%s/%s has no %s label", n.GetKind(), n.GetName(), partOfKey)
		}
		if ns := n.GetNamespace(); ns != "" && ns != esoNS {
			t.Errorf("%s/%s is in %s", n.GetKind(), n.GetName(), ns)
		}
		if strings.HasSuffix(n.GetKind(), "Binding") && !strings.Contains(field(t, n, "subjects"), "namespace: "+esoNS) {
			t.Errorf("%s subjects: %s", n.GetName(), field(t, n, "subjects"))
		}
		if n.GetKind() != "Deployment" {
			continue
		}
		c := field(t, n, "spec", "template", "spec", "containers")
		for _, want := range []string{"image: " + esoPinned, "--enable-cluster-store-reconciler=false",
			"--enable-push-secret-reconciler=false", "--enable-cluster-push-secret-reconciler=false", "--concurrent=1",
			"name: AWS_EC2_METADATA_DISABLED"} {
			if !strings.Contains(c, want) {
				t.Errorf("controller container lacks %q:\n%s", want, c)
			}
		}
		if field(t, n, "spec", "template", "spec", "nodeSelector", poolKey) != poolValue ||
			!strings.Contains(field(t, n, "spec", "template", "spec", "tolerations"), "key: "+taintKey) {
			t.Errorf("controller not on the system node group")
		}
	}
	want := "ServiceAccount/external-secrets CustomResourceDefinition/externalsecrets.external-secrets.io " +
		"ClusterRole/external-secrets-controller ClusterRoleBinding/external-secrets-controller " +
		"RoleBinding/external-secrets-leaderelection Deployment/external-secrets"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("kept\n %s\nwant\n %s", got, want)
	}
}

// The other direction: a manifest the plugin was not pinned against stops it.
func TestTransformESORefusesAChangedRelease(t *testing.T) {
	for name, m := range map[string]string{
		"another image":      strings.Replace(esoFixture, "image: ghcr.io/external-secrets/external-secrets:v2.15.0\n", "image: ghcr.io/x/y:v9\n", 1),
		"an unknown Service": esoFixture + "---\napiVersion: v1\nkind: Service\nmetadata: {name: metrics, namespace: default}\n",
		"a second Deployment": esoFixture + "---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: extra, namespace: default}\n" +
			"spec: {template: {spec: {containers: [{name: x, image: y}]}}}\n",
	} {
		if _, err := transformESO([]byte(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTransformReloader(t *testing.T) {
	out, err := transformReloader([]byte(reloaderFixture))
	if err != nil {
		t.Fatal(err)
	}
	nodes := parse(t, out)
	for _, n := range nodes {
		if n.GetLabels()[partOfKey] != partOfValue {
			t.Errorf("%s/%s has no %s label", n.GetKind(), n.GetName(), partOfKey)
		}
		if n.GetKind() == "Deployment" && field(t, n, "spec", "template", "spec", "nodeSelector", poolKey) != poolValue {
			t.Error("Reloader not on the system node group")
		}
	}
	if got := images(t, out); len(nodes) != 4 || len(got) != 1 || got[0] != reloaderPinned {
		t.Errorf("%d objects, images %v", len(nodes), got)
	}
	if _, err := transformReloader([]byte(strings.Replace(reloaderFixture, "reloader:v0.3.0", "reloader:v0.4.0", 1))); err == nil {
		t.Error("another Reloader image was accepted")
	}
}

func TestPinnedRefusesAChangedFile(t *testing.T) {
	fixtures(t, esoFixture, reloaderFixture)
	if _, err := pinned(context.Background(), esoURL, esoSHA256); err != nil {
		t.Fatal(err)
	}
	fixtures(t, esoFixture+"# changed\n", reloaderFixture)
	if _, err := pinned(context.Background(), esoURL, esoSHA256); err == nil || !strings.Contains(err.Error(), "refusing to apply") {
		t.Errorf("err = %v", err)
	}
}

func TestRender(t *testing.T) {
	out, err := render(externalSecretYAML, plugin.Options{Cluster: "demo", Region: "us-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "region: us-west-2") || !strings.Contains(out, `key: "demo/ate-owner"`) || !strings.Contains(out, `key: "demo/ate-readwrite"`) {
		t.Errorf("rendered:\n%s", out)
	}
	if _, err := render(externalSecretYAML+"\n# ${OTHER}\n", plugin.Options{Cluster: "demo", Region: "us-west-2"}); err == nil {
		t.Error("an unfilled placeholder was accepted")
	}
}

// ---- a fake cluster and Secrets Manager ------------------------------------

const testPassword = "pw-s3cr3t-value"

// fakeSM is two logins that alternate users on each rotation.
type fakeSM struct {
	users   map[string]string
	version map[string]int
	// pendingReads is how many reads a new rotation stays pending.
	pendingReads, pending int
	brokenRotation        bool
}

func newFakeSM() *fakeSM {
	return &fakeSM{users: map[string]string{"demo/ate-owner": "substrate_owner_user", "demo/ate-readwrite": "substrate_readwrite_user"},
		version: map[string]int{"demo/ate-owner": 1, "demo/ate-readwrite": 1}, pendingReads: 1}
}

func (s *fakeSM) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	n := aws.ToString(in.SecretId)
	v := fmt.Sprintf("v%d", s.version[n])
	st := map[string][]string{v: {"AWSCURRENT"}}
	if s.pending > 0 {
		s.pending--
		st = map[string][]string{fmt.Sprintf("v%d", s.version[n]-1): {"AWSCURRENT"}, v: {"AWSPENDING"}}
	}
	return &secretsmanager.DescribeSecretOutput{VersionIdsToStages: st}, nil
}
func (s *fakeSM) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	b, _ := json.Marshal(map[string]string{"username": s.users[aws.ToString(in.SecretId)], "password": testPassword})
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(string(b))}, nil
}
func (s *fakeSM) RotateSecret(_ context.Context, in *secretsmanager.RotateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.RotateSecretOutput, error) {
	n := aws.ToString(in.SecretId)
	if s.brokenRotation {
		return &secretsmanager.RotateSecretOutput{}, nil
	}
	s.version[n]++
	s.pending = s.pendingReads
	if u := s.users[n]; strings.HasSuffix(u, "_clone") {
		s.users[n] = strings.TrimSuffix(u, "_clone")
	} else {
		s.users[n] = u + "_clone"
	}
	return &secretsmanager.RotateSecretOutput{}, nil
}

// fakeK8s answers the plugin's kubectl and kubectl-ate calls. ESO copies
// the logins into the Secret one read after they change, and the Reloader
// rolls the pods once the Secret changed.
type fakeK8s struct {
	sm                *fakeSM
	bundled, noSecret bool
	crds              bool
	envOK             bool // ESO pods have Pod Identity credentials
	restarts          int
	reloaderOff       bool
	leakRawFields     bool
	loginFailure      bool
	secretUsers       map[string]string
	gen               int
	atespaces         map[string]bool
	argv, applied     []string
	reads             int
}

func newFakeK8s(sm *fakeSM) *fakeK8s {
	return &fakeK8s{sm: sm, crds: true, envOK: true, atespaces: map[string]bool{},
		secretUsers: map[string]string{ownerDSNKey: sm.users["demo/ate-owner"], rwDSNKey: sm.users["demo/ate-readwrite"]}}
}

func (f *fakeK8s) dsn(user string) string {
	return base64.StdEncoding.EncodeToString([]byte("postgres://" + user + ":" + testPassword + "@demo-pg.x.rds.amazonaws.com:5432/atepg?sslmode=verify-full"))
}

// sync is ESO's refresh: the Secret follows Secrets Manager; a change rolls
// the pods unless the Reloader is off.
func (f *fakeK8s) sync() {
	want := map[string]string{ownerDSNKey: f.sm.users["demo/ate-owner"], rwDSNKey: f.sm.users["demo/ate-readwrite"]}
	if want[ownerDSNKey] != f.secretUsers[ownerDSNKey] || want[rwDSNKey] != f.secretUsers[rwDSNKey] {
		f.secretUsers = want
		if !f.reloaderOff {
			f.gen++
		}
	}
}

func (f *fakeK8s) exec(_ context.Context, _ io.Writer, c plugin.Cmd) error {
	argv := strings.Join(c.Argv, " ")
	f.argv = append(f.argv, argv)
	for _, a := range c.Argv {
		if a == "-c" || a == "bash" || a == "sh" {
			return fmt.Errorf("a shell ran: %s", argv)
		}
	}
	out := c.Stdout
	if out == nil {
		out = io.Discard
	}
	if strings.HasPrefix(argv, "/bin/kubectl-ate ") {
		if !strings.HasSuffix(argv, " --kubeconfig /kc --context demo") {
			return fmt.Errorf("kubectl-ate without the installer's kubeconfig: %s", argv)
		}
		a := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(argv, "/bin/kubectl-ate "), " --kubeconfig /kc --context demo"))
		switch a[0] {
		case "create":
			f.atespaces[a[2]] = true
		case "get":
			if !f.atespaces[a[2]] {
				return fmt.Errorf("not found")
			}
			fmt.Fprintf(out, `{"metadata":{"name":%q}}`, a[2])
		case "delete":
			if !f.atespaces[a[2]] {
				return fmt.Errorf("not found")
			}
			delete(f.atespaces, a[2])
		}
		return nil
	}
	args, ok := strings.CutPrefix(argv, "kubectl --kubeconfig /kc ")
	if !ok {
		return fmt.Errorf("unexpected child: %s", argv)
	}
	switch {
	case args == "-n ate-system get secret ate-api-server-secret-envvars --ignore-not-found -o name":
		if !f.noSecret {
			io.WriteString(out, "secret/ate-api-server-secret-envvars\n")
		}
	case args == "-n ate-system get statefulset postgres --ignore-not-found -o name":
		if f.bundled {
			io.WriteString(out, "statefulset.apps/postgres\n")
		}
	case strings.HasPrefix(args, "get crd ") && strings.HasSuffix(args, " --ignore-not-found -o name"):
		if f.crds {
			io.WriteString(out, "customresourcedefinition/x\n")
		}
	case args == "apply -f -" || strings.HasPrefix(args, "apply --server-side "):
		f.applied = append(f.applied, string(c.Stdin))
	case strings.HasPrefix(args, "-n external-secrets get pods "):
		env := `[]`
		if f.envOK {
			env = `[{"name":"AWS_CONTAINER_CREDENTIALS_FULL_URI"}]`
		}
		fmt.Fprintf(out, `{"items":[{"metadata":{"name":"eso-1"},"spec":{"containers":[{"env":%s}]}}]}`, env)
	case args == "-n external-secrets rollout restart deploy/external-secrets":
		f.restarts++
		f.envOK = true
	case args == "-n ate-system get secret ate-api-server-secret-envvars -o json":
		f.reads++
		if f.reads > 1 {
			f.sync()
		}
		d := map[string]string{ownerDSNKey: f.dsn(f.secretUsers[ownerDSNKey]), rwDSNKey: f.dsn(f.secretUsers[rwDSNKey]),
			schemaKey: base64.StdEncoding.EncodeToString([]byte("ate"))}
		if f.leakRawFields {
			d["owner_password"] = base64.StdEncoding.EncodeToString([]byte(testPassword))
		}
		b, _ := json.Marshal(map[string]any{"data": d})
		out.Write(b)
	case args == "-n ate-system get deployment ate-api-server -o json":
		mark := ""
		if f.gen > 0 {
			mark = fmt.Sprintf("2026-11-14T00:00:%02dZ", f.gen)
		}
		fmt.Fprintf(out, `{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, reloadedKey, mark)
	case args == "-n ate-system get pods -l app=ate-api-server -o json":
		fmt.Fprintf(out, `{"items":[{"metadata":{"name":"api-%[1]d-a"}},{"metadata":{"name":"api-%[1]d-b"}},`+
			`{"metadata":{"name":"api-old-terminating","deletionTimestamp":"x"}}]}`, f.gen)
	case strings.HasPrefix(args, "-n ate-system logs "):
		if f.loginFailure {
			io.WriteString(out, `FATAL: password authentication failed for user "substrate_owner_user_clone"`)
		}
	case strings.HasPrefix(args, "wait "), strings.HasPrefix(args, "-n ate-system wait "),
		strings.Contains(args, " rollout status "), strings.HasPrefix(args, "delete "), strings.HasPrefix(args, "-n ate-system delete "):
	default:
		return fmt.Errorf("unexpected kubectl %s", args)
	}
	return nil
}

func testOptions() plugin.Options {
	return plugin.Options{Cluster: "demo", Region: "us-west-2", Profile: "p", Kubeconfig: "/kc", Tag: "v1.0.0", TreeRoot: "/trees"}
}

// cluster installs f as Exec, sm as Secrets Manager, and fast waits.
func cluster(t *testing.T, f *fakeK8s) {
	t.Helper()
	prevExec, prevSM, prevInterval, prevTries, prevAte := plugin.Exec, newSM, interval, waitTries, kubectlAte
	plugin.Exec = f.exec
	newSM = func(context.Context, string, string) (smAPI, error) { return f.sm, nil }
	interval, waitTries = 0, 6
	kubectlAte = func(context.Context, io.Writer, plugin.Options) (string, error) { return "/bin/kubectl-ate", nil }
	t.Cleanup(func() {
		plugin.Exec, newSM, interval, waitTries, kubectlAte = prevExec, prevSM, prevInterval, prevTries, prevAte
	})
}

func TestDeploy(t *testing.T) {
	fixtures(t, esoFixture, reloaderFixture)
	f := newFakeK8s(newFakeSM())
	f.envOK = false // the first pod predates its association
	cluster(t, f)
	var out strings.Builder
	if err := deploy(context.Background(), &out, testOptions()); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(f.argv, "\n"))
	}
	if f.restarts != 1 {
		t.Errorf("restarts = %d, want 1", f.restarts)
	}
	all := strings.Join(f.applied, "\n---\n")
	for _, want := range []string{"name: " + esoNS, "region: us-west-2", "kind: ExternalSecret", "name: " + reloaderNS, "kind: Config"} {
		if !strings.Contains(all, want) {
			t.Errorf("nothing applied has %q", want)
		}
	}
	if got := strings.Join(images(t, all), " "); got != esoPinned+" "+reloaderPinned {
		t.Errorf("images applied: %s", got)
	}
	if strings.Contains(all, "external-secrets-webhook") || strings.Contains(all, "ValidatingWebhookConfiguration") {
		t.Error("the webhook was applied")
	}
	i := strings.Index(strings.Join(f.argv, "\n"), "statefulset postgres")
	j := strings.Index(strings.Join(f.argv, "\n"), "apply")
	if i < 0 || j < i {
		t.Error("applied before checking the database")
	}
}

func TestDeployRefuses(t *testing.T) {
	for name, set := range map[string]func(*fakeK8s){
		"bundled PostgreSQL":    func(f *fakeK8s) { f.bundled = true },
		"no control plane":      func(f *fakeK8s) { f.noSecret = true },
		"a changed ESO release": func(f *fakeK8s) {},
	} {
		t.Run(name, func(t *testing.T) {
			eso := esoFixture
			if name == "a changed ESO release" {
				eso += "# changed\n"
			}
			fixtures(t, eso, reloaderFixture)
			f := newFakeK8s(newFakeSM())
			set(f)
			cluster(t, f)
			err := deploy(context.Background(), io.Discard, testOptions())
			if err == nil {
				t.Fatal("deployed")
			}
			if name == "bundled PostgreSQL" && !strings.Contains(err.Error(), "--database rds") {
				t.Errorf("err = %v", err)
			}
			if len(f.applied) != 0 {
				t.Errorf("applied %d manifests before refusing", len(f.applied))
			}
		})
	}
}

func TestRemove(t *testing.T) {
	f := newFakeK8s(newFakeSM())
	cluster(t, f)
	if err := remove(context.Background(), io.Discard, testOptions()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.argv, "\n")
	for _, want := range []string{
		"delete configs.reloader.external-secrets.io ate-api-server-db --ignore-not-found",
		"-n ate-system delete externalsecret ate-api-server-db --ignore-not-found",
		"-n ate-system delete secretstore aws-secrets-manager --ignore-not-found",
		"delete namespace -l " + partOf,
		"delete crd,clusterrole,clusterrolebinding -l " + partOf,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "delete secret ") {
		t.Error("removed a Secret: ate-api-server-secret-envvars must stay")
	}

	// Without the CRDs (never deployed) no custom resource is named, which
	// kubectl would refuse.
	f = newFakeK8s(newFakeSM())
	f.crds = false
	cluster(t, f)
	if err := remove(context.Background(), io.Discard, testOptions()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.argv, "\n"); strings.Contains(got, "delete externalsecret ") || strings.Contains(got, "delete configs.reloader") {
		t.Errorf("named a custom resource with no CRD:\n%s", got)
	}
}

func TestVerifyRotates(t *testing.T) {
	sm := newFakeSM()
	f := newFakeK8s(sm)
	cluster(t, f)
	var out strings.Builder
	if err := verify(context.Background(), &out, testOptions()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	o := out.String()
	for _, want := range []string{RotatedLine + "demo/ate-owner: substrate_owner_user -> substrate_owner_user_clone",
		RotatedLine + "demo/ate-readwrite: substrate_readwrite_user -> substrate_readwrite_user_clone",
		SyncedLine, RolledLine, APILine, VerifyPass} {
		if !strings.Contains(o, want) {
			t.Errorf("no %q in\n%s", want, o)
		}
	}
	if strings.Contains(o, testPassword) {
		t.Error("a password was printed")
	}
	if len(f.atespaces) != 0 {
		t.Error("the check atespace was left behind")
	}
	// A second run flips both logins back.
	out.Reset()
	if err := verify(context.Background(), &out, testOptions()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "substrate_owner_user_clone -> substrate_owner_user\n") {
		t.Errorf("second run:\n%s", out.String())
	}
}

func TestVerifyFails(t *testing.T) {
	for name, c := range map[string]struct {
		set  func(*fakeK8s)
		want string
	}{
		"rotation never finishes": {func(f *fakeK8s) { f.sm.pendingReads = 100 }, "to finish its rotation"},
		"rotation keeps the user": {func(f *fakeK8s) { f.sm.brokenRotation = true }, "to finish its rotation"},
		"Reloader never rolls":    {func(f *fakeK8s) { f.reloaderOff = true }, "Reloader to restart ate-api-server"},
		"raw fields in Secret":    {func(f *fakeK8s) { f.leakRawFields = true }, "want exactly"},
		"new pods cannot log in":  {func(f *fakeK8s) { f.loginFailure = true }, "cannot log in"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeK8s(newFakeSM())
			c.set(f)
			cluster(t, f)
			var out strings.Builder
			err := verify(context.Background(), &out, testOptions())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if strings.Contains(err.Error()+out.String(), testPassword) {
				t.Error("a password was printed")
			}
		})
	}
}

func TestDSNUsers(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	good := map[string]string{ownerDSNKey: enc("postgres://o:" + testPassword + "@h:5432/atepg"), rwDSNKey: enc("postgres://r:x@h/atepg"), schemaKey: enc("ate")}
	u, err := dsnUsers(good)
	if err != nil || u[ownerDSNKey] != "o" || u[rwDSNKey] != "r" {
		t.Fatalf("%v %v", u, err)
	}
	bad := map[string]string{ownerDSNKey: enc("%zz" + testPassword), rwDSNKey: good[rwDSNKey], schemaKey: good[schemaKey]}
	if _, err := dsnUsers(bad); err == nil || strings.Contains(err.Error(), testPassword) {
		t.Errorf("err = %v", err)
	}
}

func TestStagesAndLogins(t *testing.T) {
	if cur, p := stagesOf(map[string][]string{"a": {"AWSCURRENT", "AWSPENDING"}, "b": {"AWSPREVIOUS"}}); cur != "a" || p != 0 {
		t.Errorf("done rotation: %s %d", cur, p)
	}
	if cur, p := stagesOf(map[string][]string{"a": {"AWSCURRENT"}, "b": {"AWSPENDING"}}); cur != "a" || p != 1 {
		t.Errorf("running rotation: %s %d", cur, p)
	}
	if !loginFailed(`ERROR: assuming PostgreSQL role substrate_owner`) || loginFailed("connected as substrate_owner_user") {
		t.Error("loginFailed")
	}
	var p pod
	p.Metadata.Name = "x"
	if got := podsWithout([]pod{p}, "AWS_CONTAINER_CREDENTIALS_FULL_URI"); len(got) != 1 {
		t.Errorf("podsWithout = %v", got)
	}
	if got := podsWithout(nil, "A"); len(got) != 1 || got[0] != "(none)" {
		t.Errorf("no pods = %v", got)
	}
}
