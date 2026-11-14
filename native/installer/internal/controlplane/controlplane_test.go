package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func opts() Options {
	return Options{Cluster: "demo", Region: "us-west-2", Profile: "p", Tag: "v1.0.0",
		ImageRepo: "ghcr.io/agent-substrate/substrate", SnapBucket: "demo-snap-111122223333-us-west-2",
		AssetBucket: "demo-assets-111122223333-us-west-2", SandboxClasses: "gvisor,microvm"}
}

func flag(args []string, name string) string {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// The flags an EKS install passes to ate-setup.
func TestAteSetupArgsForEKS(t *testing.T) {
	o := opts()
	o.caFile = "/tmp/x/rds-ca.pem"
	a := AteSetupArgs(o, "demo")
	for name, want := range map[string]string{
		"--context": "demo", "--image-tag": "v1.0.0", "--storage-bucket-name": "demo-snap-111122223333-us-west-2",
		"--snapshot-location": "s3://demo-snap-111122223333-us-west-2/", "--sandbox-classes": "gvisor,microvm",
		"--credential-provider":            `{"name":"k8s.io"}`,
		"--ingress-allowed-spiffe-ids":     "spiffe://cluster.local/ns/ate-system/sa/atenet-client",
		"--ateapi-postgres-server-ca-file": "/tmp/x/rds-ca.pem",
	} {
		if got := flag(a, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !slices.Equal(a[:4], []string{"run", "./cmd/ate-setup", "deploy", "ate-system"}) {
		t.Errorf("not an ate-setup deploy: %v", a[:4])
	}
	if slices.Contains(a, "--ateapi-postgres-storage-class") || slices.Contains(a, "--cordon-system-infra-components") {
		t.Errorf("a standard cluster got an Auto Mode flag: %v", a)
	}
}

// AC4.3 and AC3.3: Auto Mode cordons the control plane with RDS; the bundled
// PostgreSQL is never cordoned (ate-setup would then want an ate-postgres
// pool) and names the mode's StorageClass on both modes (review F2: a
// standard cluster has no default class).
func TestStorageClassFlagPerModeAndDatabase(t *testing.T) {
	for _, tc := range []struct {
		auto           bool
		db, class      string
		cordon, caFile bool
	}{
		{auto: true, db: DatabaseRDS, cordon: true, caFile: true},
		{auto: false, db: DatabaseRDS, caFile: true},
		{auto: true, db: DatabaseBundled, class: "substrate-eks-auto-ebs"},
		{auto: false, db: DatabaseBundled, class: "substrate-eks-ebs"},
	} {
		o := opts()
		o.AutoMode, o.Database, o.caFile = tc.auto, tc.db, "/tmp/x/rds-ca.pem"
		a := AteSetupArgs(o, "demo")
		if got := flag(a, "--ateapi-postgres-storage-class"); got != tc.class || (tc.class == "") == slices.Contains(a, "--ateapi-postgres-storage-class") {
			t.Errorf("auto=%v %s: storage class %q, want %q: %v", tc.auto, tc.db, got, tc.class, a)
		}
		if slices.Contains(a, "--cordon-system-infra-components") != tc.cordon || slices.Contains(a, "--ateapi-postgres-server-ca-file") != tc.caFile {
			t.Errorf("auto=%v %s: %v", tc.auto, tc.db, a)
		}
	}
}

func TestEnvSelectsS3AndBlanksAmbientContext(t *testing.T) {
	env := AteSetupEnv(opts(), "/k")
	for _, want := range []string{"ATE_INSTALL_STORAGE_BACKEND=s3", "ATE_ASSET_BUCKET=demo-assets-111122223333-us-west-2", "KUBECONFIG=/k", "KUBECTL_CONTEXT="} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

func TestUnknownDatabaseIsRefused(t *testing.T) {
	o := opts()
	o.Overlay, o.Database = "/x", "aurora"
	if err := Deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "aurora") {
		t.Errorf("want a refusal of the database, got %v", err)
	}
}

// --- the RDS path, against a fake account and a fake kubectl --------------

const fakeSecret = "Fake0Secret0Value0Zz9"

type fakeRDS struct{ owner string }

func (f fakeRDS) DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{
		DBInstanceStatus: aws.String("available"),
		Endpoint:         &rdstypes.Endpoint{Address: aws.String("demo-pg.abc.us-west-2.rds.amazonaws.com"), Port: aws.Int32(5432)},
		MasterUserSecret: &rdstypes.MasterUserSecret{SecretArn: aws.String("rds!db-demo")},
		TagList:          []rdstypes.Tag{{Key: aws.String(ownerTagKey), Value: aws.String(f.owner)}},
	}}}, nil
}

type fakeSM struct{}

func (fakeSM) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	v := map[string]string{
		"rds!db-demo":        `{"username":"ateadmin","password":"admin-` + fakeSecret + `"}`,
		"demo/ate-owner":     `{"username":"substrate_owner_user","password":"owner-` + fakeSecret + `"}`,
		"demo/ate-readwrite": `{"username":"substrate_readwrite_user_clone","password":"rw-` + fakeSecret + `"}`,
	}[aws.ToString(in.SecretId)]
	if v == "" {
		return nil, errors.New("no such secret")
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(v)}, nil
}

var passwords = []string{"admin-" + fakeSecret, "owner-" + fakeSecret, "rw-" + fakeSecret}

// fakeKube records every command and answers kubectl get nodes. fail names
// an argv substring whose command fails; noNodes a selector with no node.
type fakeKube struct {
	cmds          []command
	fail, noNodes string
	caMode        os.FileMode // of the CA file, when ate-setup ran
	class         string      // what kubectl get storageclass prints
}

func (k *fakeKube) run(_ context.Context, w io.Writer, c command) error {
	k.cmds = append(k.cmds, c)
	line := strings.Join(c.Argv, " ")
	if k.fail != "" && strings.Contains(line, k.fail) {
		return fmt.Errorf("%s failed", c.Argv[0])
	}
	if slices.Contains(c.Argv, "nodes") && !slices.Contains(c.Argv, k.noNodes) {
		fmt.Fprintln(c.Stdout, "node/ip-10-0-0-1")
	}
	if slices.Contains(c.Argv, "get") && slices.Contains(c.Argv, "storageclass") {
		fmt.Fprint(c.Stdout, k.class)
	}
	if ca := flag(c.Argv, "--ateapi-postgres-server-ca-file"); ca != "" {
		if fi, err := os.Stat(ca); err == nil {
			k.caMode = fi.Mode().Perm()
		}
	}
	fmt.Fprintln(w, "ok: "+c.Argv[0]) // child output reaches the log too
	return nil
}

func (k *fakeKube) find(sub string) *command {
	for i := range k.cmds {
		if strings.Contains(strings.Join(k.cmds[i].Argv, " "), sub) {
			return &k.cmds[i]
		}
	}
	return nil
}

// rdsFixture swaps in the fakes and returns options and a Substrate tree
// with setup.sql. The overlay is the real deploy/eks.
func rdsFixture(t *testing.T, k *fakeKube, owner string) (Options, string) {
	t.Helper()
	// The server serves its own certificate as the "CA bundle".
	var ca *httptest.Server
	ca = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate().Raw})
	}))
	t.Cleanup(ca.Close)
	prevURL, prevClient, prevDB, prevRun, prevNodes := rdsCABundleURL, httpClient, newDBClients, runCommand, autoNodes
	t.Cleanup(func() {
		rdsCABundleURL, httpClient, newDBClients, runCommand, autoNodes = prevURL, prevClient, prevDB, prevRun, prevNodes
	})
	// The tagged Auto Mode nodes step has its own tests (autonodes_test.go).
	autoNodes = func(context.Context, io.Writer, Options, string) error { return nil }
	rdsCABundleURL, httpClient = ca.URL, ca.Client()
	newDBClients = func(context.Context, string, string) (RDSAPI, SecretsAPI, error) {
		return fakeRDS{owner}, fakeSM{}, nil
	}
	runCommand = k.run

	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "pkg", "postgressetup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "pkg", "postgressetup", "setup.sql"), []byte("-- setup.sql\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay, err := filepath.Abs("../../../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	o := opts()
	o.Overlay, o.AutoMode = overlay, true
	return o, tree
}

// AC3.2: the database passwords reach the bootstrap Job only through
// kubectl's stdin, and ate-setup only through its environment; never an
// argv, an environment other than ate-setup's, or a line the deploy logs.
func TestRDSDeployKeepsSecretsOutOfArgvAndLogs(t *testing.T) {
	k := &fakeKube{}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	var log bytes.Buffer
	if err := install(context.Background(), &log, o, "demo", "/k", tree); err != nil {
		t.Fatal(err)
	}
	ate := k.find("./cmd/ate-setup")
	secretApply := slices.IndexFunc(k.cmds, func(c command) bool { return bytes.Contains(c.Stdin, []byte(`"kind":"Secret"`)) })
	if ate == nil || secretApply < 0 {
		t.Fatalf("no ate-setup run or Secret apply: %v", k.cmds)
	}
	for _, pw := range passwords {
		if strings.Contains(log.String(), pw) {
			t.Errorf("a password reached the log: %s", log.String())
		}
		for i, c := range k.cmds {
			if strings.Contains(strings.Join(c.Argv, " "), pw) {
				t.Errorf("a password is in argv: %v", c.Argv)
			}
			if &k.cmds[i] != ate && strings.Contains(strings.Join(c.Env, " "), pw) {
				t.Errorf("a password is in the environment of %v", c.Argv)
			}
			if i != secretApply && bytes.Contains(c.Stdin, []byte(base64.StdEncoding.EncodeToString([]byte(pw)))) {
				t.Errorf("a password is in the stdin of %v", c.Argv)
			}
		}
		// And it does reach the two places it must (both directions).
		if !bytes.Contains(k.cmds[secretApply].Stdin, []byte(base64.StdEncoding.EncodeToString([]byte(pw)))) {
			t.Errorf("the bootstrap Secret lacks a login")
		}
	}
	env := strings.Join(ate.Env, "\n")
	for _, want := range []string{
		"ATE_API_POSTGRES_OWNER_CONNECTION_STRING=postgres://substrate_owner_user:owner-" + fakeSecret + "@demo-pg.abc.us-west-2.rds.amazonaws.com:5432/atepg?sslmode=verify-full&sslrootcert=/run/postgres-server-ca/server-ca.pem",
		"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING=postgres://substrate_readwrite_user_clone:rw-" + fakeSecret + "@",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("ate-setup env lacks %s", want)
		}
	}
	if strings.Contains(env, "admin-"+fakeSecret) {
		t.Error("ate-setup got the master password")
	}
	// AC3.1: verify-full against the CA bundle, which was a 0600 file during
	// the run and is gone after it.
	ca := flag(ate.Argv, "--ateapi-postgres-server-ca-file")
	if ca == "" || k.caMode != 0o600 {
		t.Errorf("CA file %q mode %v", ca, k.caMode)
	}
	if _, err := os.Stat(ca); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CA file left behind: %v", err)
	}
	if !slices.Contains(ate.Argv, "--cordon-system-infra-components") {
		t.Error("Auto Mode + RDS without --cordon-system-infra-components")
	}
	// The Job is the real deploy/eks file, filled in, applied from stdin.
	job := k.find("apply -f -")
	for _, c := range k.cmds {
		if bytes.Contains(c.Stdin, []byte("kind: Job")) {
			job = &c
		}
	}
	for _, want := range []string{"namespace: ate-db-bootstrap", bootstrapImage, `value: "demo-pg.abc.us-west-2.rds.amazonaws.com"`, `DO \$\$BEGIN`} {
		if !bytes.Contains(job.Stdin, []byte(want)) {
			t.Errorf("the applied Job lacks %q", want)
		}
	}
	// The ConfigMap carries both SQL files, with both alternates of each login.
	cm := slices.IndexFunc(k.cmds, func(c command) bool { return bytes.Contains(c.Stdin, []byte(`"kind":"ConfigMap"`)) })
	for _, want := range []string{"setup.sql", "('substrate_owner_user_clone', 'substrate_owner')", "('substrate_readwrite_user', 'substrate_readwrite')", "CERTIFICATE"} {
		if cm < 0 || !bytes.Contains(k.cmds[cm].Stdin, []byte(want)) {
			t.Errorf("the setup ConfigMap lacks %q", want)
		}
	}
	if k.find("delete namespace ate-db-bootstrap --timeout") == nil {
		t.Error("the bootstrap namespace was not deleted")
	}
}

// The namespace holding the master password goes even when the Job fails,
// and ate-setup never runs on an unbootstrapped database.
func TestFailedBootstrapStillDeletesTheNamespace(t *testing.T) {
	k := &fakeKube{fail: "--for=condition=complete"}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	err := install(context.Background(), io.Discard, o, "demo", "/k", tree)
	if err == nil || !strings.Contains(err.Error(), "bootstrap Job did not complete") {
		t.Fatalf("want a bootstrap failure, got %v", err)
	}
	if k.find("delete namespace ate-db-bootstrap --ignore-not-found") == nil {
		t.Error("the namespace with the master password was left behind")
	}
	if k.find("./cmd/ate-setup") != nil {
		t.Error("ate-setup ran after a failed bootstrap")
	}
}

func TestRDSRefusesAnotherDeploymentsInstance(t *testing.T) {
	k := &fakeKube{}
	o, tree := rdsFixture(t, k, "other.us-west-2")
	err := install(context.Background(), io.Discard, o, "demo", "/k", tree)
	if err == nil || !strings.Contains(err.Error(), "not owned by demo.us-west-2") {
		t.Fatalf("want an ownership refusal, got %v", err)
	}
	if k.find("apply") != nil {
		t.Error("applied something for an instance it does not own")
	}
}

// A missing pool fails at once, naming the label, instead of leaving the
// Job or the cordoned control plane Pending.
func TestMissingNodePoolStopsBeforeTheDatabase(t *testing.T) {
	for _, sel := range []string{systemPoolSelector, systemInfraSelector} {
		k := &fakeKube{noNodes: sel}
		o, tree := rdsFixture(t, k, "demo.us-west-2")
		newDBClients = func(context.Context, string, string) (RDSAPI, SecretsAPI, error) {
			t.Fatal("read the database before checking the nodes")
			return nil, nil, nil
		}
		err := install(context.Background(), io.Discard, o, "demo", "/k", tree)
		if err == nil || !strings.Contains(err.Error(), sel) {
			t.Errorf("%s: want an error naming it, got %v", sel, err)
		}
	}
}

// appliedClass is the StorageClass a command sent kubectl on stdin, and its
// index in k.cmds (-1 when none was).
func (k *fakeKube) appliedClass(t *testing.T) (storageClass, int) {
	t.Helper()
	for i, c := range k.cmds {
		if bytes.Contains(c.Stdin, []byte(`"kind":"StorageClass"`)) {
			var sc storageClass
			if err := json.Unmarshal(c.Stdin, &sc); err != nil {
				t.Fatal(err)
			}
			return sc, i
		}
	}
	return storageClass{}, -1
}

var testTags = map[string]string{ownerTagKey: "demo.us-west-2", "project": "agent-substrate", "auto-delete": "no"}

// AC3.3, review F2 and F3: bundled applies the mode's StorageClass before
// ate-setup on both modes, with the owner tag and TAGS where the provisioner
// can add them, and takes no RDS step.
func TestBundledAppliesTheStorageClassFirst(t *testing.T) {
	for _, tc := range []struct {
		name               string
		auto, owned        bool
		class, provisioner string
		tagged             bool
	}{
		{"standard, made by the installer", false, true, "substrate-eks-ebs", "ebs.csi.aws.com", true},
		// The add-on's policy allows any tag, whoever made the cluster.
		{"standard, brought by the user", false, false, "substrate-eks-ebs", "ebs.csi.aws.com", true},
		{"Auto Mode, made by the installer", true, true, "substrate-eks-auto-ebs", "ebs.csi.eks.amazonaws.com", true},
		// Its role may lack the tag statement: no custom tags, and it says so.
		{"Auto Mode, brought by the user", true, false, "substrate-eks-auto-ebs", "ebs.csi.eks.amazonaws.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &fakeKube{}
			o, tree := rdsFixture(t, k, "demo.us-west-2")
			o.Database, o.AutoMode, o.clusterOwned, o.volumeTags = DatabaseBundled, tc.auto, tc.owned, testTags
			newDBClients = func(context.Context, string, string) (RDSAPI, SecretsAPI, error) {
				t.Fatal("bundled read RDS")
				return nil, nil, nil
			}
			var log bytes.Buffer
			if err := install(context.Background(), &log, o, "demo", "/k", tree); err != nil {
				t.Fatal(err)
			}
			sc, at := k.appliedClass(t)
			ate := slices.IndexFunc(k.cmds, func(c command) bool { return slices.Contains(c.Argv, "./cmd/ate-setup") })
			if at < 0 || ate < at || !slices.Equal(k.cmds[at].Argv, []string{"kubectl", "apply", "-f", "-"}) {
				t.Fatalf("StorageClass not applied before ate-setup (class %d, ate-setup %d): %v", at, ate, k.cmds)
			}
			if sc.Metadata.Name != tc.class || sc.Provisioner != tc.provisioner || flag(k.cmds[ate].Argv, "--ateapi-postgres-storage-class") != tc.class {
				t.Errorf("class %s on %s, ate-setup names %q", sc.Metadata.Name, sc.Provisioner, flag(k.cmds[ate].Argv, "--ateapi-postgres-storage-class"))
			}
			n := 0
			for p, v := range sc.Parameters {
				if key, val, _ := strings.Cut(v, "="); strings.HasPrefix(p, "tagSpecification_") && testTags[key] == val {
					n++
				}
			}
			if tc.tagged != (n == len(testTags)) || (!tc.tagged && n != 0) {
				t.Errorf("tagged=%v but %d of %d tags are in %v", tc.tagged, n, len(testTags), sc.Parameters)
			}
			if strings.Contains(log.String(), NoVolumeTagsLine) == tc.tagged {
				t.Errorf("tagged=%v, and the no-tags line printed=%v", tc.tagged, !tc.tagged)
			}
			if (sc.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true") != tc.owned {
				t.Errorf("owned=%v, default class annotations %v", tc.owned, sc.Metadata.Annotations)
			}
			if got := k.find("get csidriver ebs.csi.aws.com") != nil; got == tc.auto {
				t.Errorf("auto=%v, checked the EBS CSI driver=%v", tc.auto, got)
			}
			if !strings.Contains(log.String(), PrereqLine+"StorageClass "+tc.class) {
				t.Errorf("no %q line:\n%s", PrereqLine, log.String())
			}
			if a := k.cmds[ate]; strings.Contains(strings.Join(a.Env, " "), "CONNECTION_STRING") || k.find("ate-db-bootstrap") != nil {
				t.Errorf("bundled took the RDS path: %v", k.cmds)
			}
		})
	}
}

// With RDS, the class is the default class of a cluster this installer made
// (R1), applied before the bootstrap; on a cluster the user brought, RDS
// leaves its storage alone.
func TestRDSAppliesTheClassOnlyOnAnOwnedCluster(t *testing.T) {
	for _, owned := range []bool{true, false} {
		k := &fakeKube{}
		o, tree := rdsFixture(t, k, "demo.us-west-2")
		o.clusterOwned, o.volumeTags = owned, testTags
		if err := install(context.Background(), io.Discard, o, "demo", "/k", tree); err != nil {
			t.Fatal(err)
		}
		_, at := k.appliedClass(t)
		boot := slices.IndexFunc(k.cmds, func(c command) bool { return strings.Contains(strings.Join(c.Argv, " "), "ate-db-bootstrap") })
		if owned != (at >= 0) || (owned && at > boot) {
			t.Errorf("owned=%v: class applied at %d, bootstrap at %d", owned, at, boot)
		}
		if ate := k.find("./cmd/ate-setup"); ate == nil || slices.Contains(ate.Argv, "--ateapi-postgres-storage-class") {
			t.Errorf("owned=%v: ate-setup %v", owned, ate)
		}
	}
}

// StorageClass parameters cannot be updated: a class from an older install
// (the untagged Auto Mode one) is replaced, and one already right is applied.
func TestStorageClassIsReplacedOnlyWhenItDiffers(t *testing.T) {
	cur := renderStorageClass(true, true, testTags)
	same, _ := json.Marshal(cur)
	old := renderStorageClass(true, false, nil)
	differ, _ := json.Marshal(old)
	for _, tc := range []struct {
		name, class string
		verb        []string
	}{
		{"absent", "", []string{"apply", "-f", "-"}},
		{"same settings", string(same), []string{"apply", "-f", "-"}},
		{"untagged from an older install", string(differ), []string{"replace", "--force", "-f", "-"}},
	} {
		k := &fakeKube{class: tc.class}
		o, tree := rdsFixture(t, k, "demo.us-west-2")
		o.Database, o.clusterOwned, o.volumeTags = DatabaseBundled, true, testTags
		if err := install(context.Background(), io.Discard, o, "demo", "/k", tree); err != nil {
			t.Fatal(err)
		}
		if _, at := k.appliedClass(t); at < 0 || !slices.Equal(k.cmds[at].Argv[1:], tc.verb) {
			t.Errorf("%s: want kubectl %v, got %v", tc.name, tc.verb, k.cmds)
		}
	}
}

// A standard cluster without the EBS CSI driver stops before ate-setup,
// naming the add-on, instead of a PostgreSQL rollout that times out.
func TestBundledOnStandardNeedsTheEBSDriver(t *testing.T) {
	k := &fakeKube{fail: "get csidriver"}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	o.Database, o.AutoMode, o.volumeTags = DatabaseBundled, false, testTags
	err := install(context.Background(), io.Discard, o, "demo", "/k", tree)
	if err == nil || !strings.Contains(err.Error(), "aws-ebs-csi-driver") {
		t.Fatalf("want an error naming the add-on, got %v", err)
	}
	if k.find("./cmd/ate-setup") != nil || k.find("apply") != nil {
		t.Errorf("went on without the driver: %v", k.cmds)
	}
}

func TestRenderedStorageClassOnBothModes(t *testing.T) {
	for _, auto := range []bool{true, false} {
		sc := renderStorageClass(auto, false, testTags)
		p := sc.Parameters
		if p["type"] != "gp3" || p["encrypted"] != "true" || sc.VolumeBindingMode != "WaitForFirstConsumer" || sc.ReclaimPolicy != "Delete" {
			t.Errorf("auto=%v: %+v", auto, sc)
		}
		// One tagSpecification_N per tag, numbered from 1 in key order.
		want := map[string]string{"tagSpecification_1": ownerTagKey + "=demo.us-west-2",
			"tagSpecification_2": "auto-delete=no", "tagSpecification_3": "project=agent-substrate"}
		for k, v := range want {
			if p[k] != v {
				t.Errorf("auto=%v: %s=%q, want %q", auto, k, p[k], v)
			}
		}
		if len(p) != 2+len(want) {
			t.Errorf("auto=%v: parameters %v", auto, p)
		}
		topo := len(sc.AllowedTopologies) == 1 && sc.AllowedTopologies[0].MatchLabelExpressions[0].Key == "eks.amazonaws.com/compute-type"
		if topo != auto {
			t.Errorf("auto=%v: allowedTopologies %+v", auto, sc.AllowedTopologies)
		}
	}
}

func TestVolumeTagsAreTheOwnerTagAndTAGS(t *testing.T) {
	got, err := volumeTags("demo", "us-west-2", DefaultTags)
	if err != nil || !maps.Equal(got, testTags) {
		t.Errorf("tags %v (%v), want %v", got, err, testTags)
	}
	if _, err := volumeTags("demo", "us-west-2", "project"); err == nil {
		t.Error("accepted a tag without a value")
	}
}
