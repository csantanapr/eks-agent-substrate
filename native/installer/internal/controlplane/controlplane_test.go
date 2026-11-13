package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
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

// The flags eks-talk's substrate/install.sh passes.
func TestAteSetupArgsMatchInstallScript(t *testing.T) {
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
// PostgreSQL keeps the POC's StorageClass and is never cordoned (ate-setup
// would then want an ate-postgres pool).
func TestAutoModeFlagsPerDatabase(t *testing.T) {
	o := opts()
	o.AutoMode = true
	a := AteSetupArgs(o, "demo")
	if !slices.Contains(a, "--cordon-system-infra-components") || slices.Contains(a, "--ateapi-postgres-storage-class") {
		t.Errorf("Auto Mode + RDS: %v", a)
	}
	o.Database = DatabaseBundled
	a = AteSetupArgs(o, "demo")
	if flag(a, "--ateapi-postgres-storage-class") != AutoModeStorageClass || slices.Contains(a, "--cordon-system-infra-components") ||
		slices.Contains(a, "--ateapi-postgres-server-ca-file") {
		t.Errorf("Auto Mode + bundled: %v", a)
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
	prevURL, prevClient, prevDB, prevRun := rdsCABundleURL, httpClient, newDBClients, runCommand
	t.Cleanup(func() { rdsCABundleURL, httpClient, newDBClients, runCommand = prevURL, prevClient, prevDB, prevRun })
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

// AC3.3: bundled keeps the POC path: the Auto Mode StorageClass prerequisite,
// no database reads, no connection strings.
func TestBundledKeepsThePOCPath(t *testing.T) {
	k := &fakeKube{}
	o, tree := rdsFixture(t, k, "demo.us-west-2")
	o.Database = DatabaseBundled
	newDBClients = func(context.Context, string, string) (RDSAPI, SecretsAPI, error) {
		t.Fatal("bundled read RDS")
		return nil, nil, nil
	}
	var log bytes.Buffer
	if err := install(context.Background(), &log, o, "demo", "/k", tree); err != nil {
		t.Fatal(err)
	}
	if k.find("apply -f "+filepath.Join(o.Overlay, "prereqs")) == nil || !strings.Contains(log.String(), PrereqLine) {
		t.Error("bundled on Auto Mode skipped the StorageClass prerequisite")
	}
	ate := k.find("./cmd/ate-setup")
	if ate == nil || strings.Contains(strings.Join(ate.Env, " "), "CONNECTION_STRING") || k.find("ate-db-bootstrap") != nil {
		t.Errorf("bundled took the RDS path: %v", k.cmds)
	}
}
