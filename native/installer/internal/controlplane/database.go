package controlplane

// The RDS half of the deploy, in Go: eks-talk's aws/db-bootstrap.sh (the
// in-cluster schema bootstrap) and the connection strings of
// substrate/install.sh. setup-aws create rds made the instance and the
// logins; this reads them with the AWS SDK and hands them on without a
// password ever reaching an argv, a log line or a file:
//
//   - the bootstrap Job gets the three logins from a Secret that kubectl
//     reads from stdin, in a throwaway namespace deleted however the
//     bootstrap ends;
//   - ate-setup gets the two connection strings in its environment, and
//     stores them in the Secret ate-api-server reads.

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Lines the RDS path prints for the progress checklist.
const (
	DatabaseLine  = "Using RDS instance "
	CALine        = "Downloading the RDS CA bundle to "
	BootstrapLine = "Bootstrapping the database schema in namespace "
	BootstrapDone = "Database bootstrap done; removed namespace "
)

const (
	bootstrapNS = "ate-db-bootstrap"
	// The postgres client image, pinned by digest (aws/db-bootstrap.sh).
	bootstrapImage = "public.ecr.aws/docker/library/postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5"
	// bootstrapJob is under Options.Overlay (deploy/eks).
	bootstrapJob     = "db-bootstrap/job.yaml"
	bootstrapTimeout = "180s"
	// ownerTagKey is setup-aws's OwnerTagKey; the installer does not import
	// setup-aws's cmd package (cobra and the whole SDK).
	ownerTagKey = "agent-substrate-on-eks/deployment"
	dbName      = "atepg"
	// ate-setup stores the --ateapi-postgres-server-ca-file bundle in the
	// postgres-server-ca Secret, and ate-api-server mounts it here.
	serverCAMount = "/run/postgres-server-ca/server-ca.pem"
	// The bootstrap Job's node pool, and the pool
	// --cordon-system-infra-components pins the control plane to. On an
	// Auto Mode cluster from setup-aws create cluster, the system group
	// carries both labels.
	systemPoolSelector  = "ate.dev/pool=system"
	systemInfraSelector = "ate.dev/workloadType=ate-system-infra"
)

// DBInstanceID is the RDS instance setup-aws create rds makes (DB_ID).
func DBInstanceID(cluster string) string { return cluster + "-pg" }

func dbSecretName(cluster, who string) string { return cluster + "/ate-" + who }

// Replaced in tests.
var (
	rdsCABundleURL = "https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem"
	httpClient     = &http.Client{Timeout: time.Minute}
	dbWaitTimeout  = 20 * time.Minute
	newDBClients   = func(ctx context.Context, profile, region string) (RDSAPI, SecretsAPI, error) {
		opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
		if profile != "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, nil, err
		}
		return rds.NewFromConfig(cfg), secretsmanager.NewFromConfig(cfg), nil
	}
)

// RDSAPI and SecretsAPI are the reads the RDS path makes.
type (
	RDSAPI interface {
		DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	}
	SecretsAPI interface {
		GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	}
)

type login struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type rdsDB struct {
	id, host         string
	port             int
	admin, owner, rw login
}

// prepareRDS checks the node pools, reads the instance and its logins,
// downloads the CA bundle into o.caFile (0600, in a 0700 directory that
// cleanup removes), runs the schema bootstrap, and returns ate-setup's
// connection-string environment. cleanup is never nil.
func prepareRDS(ctx context.Context, w io.Writer, o *Options, kc, tree string) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	if err := requireNodes(ctx, kc, systemPoolSelector, "the database bootstrap Job runs on the system node group"); err != nil {
		return nil, cleanup, err
	}
	if o.AutoMode {
		if err := requireNodes(ctx, kc, systemInfraSelector, "on Auto Mode, --cordon-system-infra-components pins the control plane to them"); err != nil {
			return nil, cleanup, err
		}
	}
	db, err := readRDS(ctx, *o)
	if err != nil {
		return nil, cleanup, err
	}
	fmt.Fprintln(w, DatabaseLine+db.id+" at "+db.host)
	dir, err := os.MkdirTemp("", "substrate-eks-db-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	o.caFile = filepath.Join(dir, "rds-ca.pem")
	fmt.Fprintln(w, CALine+o.caFile)
	ca, err := downloadCA(ctx, o.caFile)
	if err != nil {
		return nil, cleanup, err
	}
	if err := bootstrapSchema(ctx, w, kc, o.Overlay, tree, db, ca); err != nil {
		return nil, cleanup, err
	}
	return []string{
		"ATE_API_POSTGRES_OWNER_CONNECTION_STRING=" + dsn(db.owner, db.host, db.port),
		"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING=" + dsn(db.rw, db.host, db.port),
	}, cleanup, nil
}

// requireNodes fails unless a node matches selector: a pod pinned to a
// missing pool stays Pending until a timeout that names nothing.
func requireNodes(ctx context.Context, kc, selector, why string) error {
	var out strings.Builder
	if err := runCommand(ctx, io.Discard, command{Env: []string{"KUBECONFIG=" + kc}, Stdout: &out,
		Argv: []string{"kubectl", "get", "nodes", "-l", selector, "-o", "name"}}); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) == "" {
		return fmt.Errorf("no node is labeled %s, and %s; add the system node group (setup-aws create cluster makes it)", selector, why)
	}
	return nil
}

// readRDS waits for the instance setup-aws made, checks it is this
// deployment's, and reads its endpoint and the three logins.
func readRDS(ctx context.Context, o Options) (rdsDB, error) {
	r, sm, err := newDBClients(ctx, o.Profile, o.Region)
	if err != nil {
		return rdsDB{}, err
	}
	db := rdsDB{id: DBInstanceID(o.Cluster)}
	out, err := rds.NewDBInstanceAvailableWaiter(r).WaitForOutput(ctx,
		&rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(db.id)}, dbWaitTimeout)
	if err != nil {
		return db, fmt.Errorf("RDS instance %s is not available (setup-aws create rds makes it): %w", db.id, err)
	}
	inst := out.DBInstances[0]
	owner := ""
	for _, t := range inst.TagList {
		if aws.ToString(t.Key) == ownerTagKey {
			owner = aws.ToString(t.Value)
		}
	}
	if want := o.Cluster + "." + o.Region; owner != want {
		return db, fmt.Errorf("RDS instance %s is not owned by %s (%s=%q); refusing to bootstrap it", db.id, want, ownerTagKey, owner)
	}
	if inst.Endpoint == nil || inst.MasterUserSecret == nil {
		return db, fmt.Errorf("RDS instance %s has no endpoint or managed master user secret", db.id)
	}
	db.host, db.port = aws.ToString(inst.Endpoint.Address), int(aws.ToInt32(inst.Endpoint.Port))
	if db.port == 0 {
		db.port = 5432
	}
	if !hostName.MatchString(db.host) {
		return db, fmt.Errorf("RDS instance %s has an unexpected endpoint %q", db.id, db.host)
	}
	for _, l := range []struct {
		dst  *login
		id   string
		what string
	}{
		{&db.admin, aws.ToString(inst.MasterUserSecret.SecretArn), "the master user secret of " + db.id},
		{&db.owner, dbSecretName(o.Cluster, "owner"), dbSecretName(o.Cluster, "owner")},
		{&db.rw, dbSecretName(o.Cluster, "readwrite"), dbSecretName(o.Cluster, "readwrite")},
	} {
		v, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(l.id)})
		if err != nil {
			return db, fmt.Errorf("read %s: %w", l.what, err)
		}
		// The error names the secret, never its value.
		if err := json.Unmarshal([]byte(aws.ToString(v.SecretString)), l.dst); err != nil || l.dst.Username == "" || l.dst.Password == "" {
			return db, fmt.Errorf("%s is not a {username, password} login", l.what)
		}
	}
	return db, nil
}

var (
	hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	// A role name with room for the _clone suffix, safe to put in SQL.
	roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,56}$`)
)

// downloadCA fetches the RDS CA bundle, which lets clients use
// sslmode=verify-full, and writes it to path, mode 0600.
func downloadCA(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rdsCABundleURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download the RDS CA bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download the RDS CA bundle from %s: %s", rdsCABundleURL, resp.Status)
	}
	pem, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("download the RDS CA bundle: %w", err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s did not return PEM certificates", rdsCABundleURL)
	}
	return pem, os.WriteFile(path, pem, 0o600)
}

// dsn is a connection string for ate-api-server: TLS, a server certificate
// that chains to the RDS CA, and a host name that matches it.
func dsn(l login, host string, port int) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(l.Username, l.Password),
		Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/" + dbName,
		RawQuery: "sslmode=verify-full&sslrootcert=" + serverCAMount}
	return u.String()
}

// alternatesSQL makes both logins of each pair with the attributes setup.sql
// requires, before it runs. Secrets Manager rotation (demos/db-rotation)
// alternates each login between USER and USER_clone: setup.sql manages
// whichever one the secret names now, and rotation only sets the other's
// password. The names are checked, since they are spliced into SQL.
func alternatesSQL(owner, rw string) (string, error) {
	ob, rb := strings.TrimSuffix(owner, "_clone"), strings.TrimSuffix(rw, "_clone")
	for _, n := range []string{ob, rb} {
		if !roleName.MatchString(n) {
			return "", fmt.Errorf("database login %q is not a plain role name", n)
		}
	}
	return fmt.Sprintf(`DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT * FROM (VALUES ('substrate_owner'), ('substrate_readwrite')) v(name) LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.name) THEN
      EXECUTE format('CREATE ROLE %%I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', r.name);
    END IF;
  END LOOP;
  FOR r IN SELECT * FROM (VALUES
      ('%[1]s', 'substrate_owner'), ('%[1]s_clone', 'substrate_owner'),
      ('%[2]s', 'substrate_readwrite'), ('%[2]s_clone', 'substrate_readwrite')) v(name, grp) LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.name) THEN
      EXECUTE format('CREATE ROLE %%I LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION', r.name);
    ELSE
      EXECUTE format('ALTER ROLE %%I NOINHERIT', r.name);
    END IF;
    EXECUTE format('GRANT %%I TO %%I', r.grp, r.name);
  END LOOP;
END
$$;
`, ob, rb), nil
}

// bootstrapSchema runs pkg/postgressetup/setup.sql from a Job inside the
// cluster (the database has no public endpoint), as db-bootstrap.sh does.
// The master password lives only in a Secret in a throwaway namespace for
// the length of the Job; removeBootstrap deletes the Secret, the Job and the
// namespace afterwards, also on failure and when ctx is cancelled.
func bootstrapSchema(ctx context.Context, w io.Writer, kc, overlay, tree string, db rdsDB, ca []byte) (err error) {
	setupSQL, err := os.ReadFile(filepath.Join(tree, "pkg", "postgressetup", "setup.sql"))
	if err != nil {
		return fmt.Errorf("no setup.sql in the Substrate tree: %w", err)
	}
	alternates, err := alternatesSQL(db.owner.Username, db.rw.Username)
	if err != nil {
		return err
	}
	job, err := renderJob(filepath.Join(overlay, bootstrapJob), db.host)
	if err != nil {
		return err
	}
	kubectl := func(stdin []byte, args ...string) error {
		return runCommand(ctx, w, command{Env: []string{"KUBECONFIG=" + kc}, Stdin: stdin, Argv: append([]string{"kubectl"}, args...)})
	}
	fmt.Fprintln(w, BootstrapLine+bootstrapNS)
	// The Secret holds the logins and the Job's pod reads them: remove both
	// and the namespace however this ends, also when ctx was cancelled
	// mid-run (ctrl+c or SIGTERM, see main.go), since a create kubectl was
	// killed in may still have been stored.
	defer func() {
		cerr := removeBootstrap(w, kc, err != nil)
		switch {
		case err != nil:
			err = errors.Join(err, cerr)
		case cerr != nil:
			err = cerr
		default:
			fmt.Fprintln(w, BootstrapDone+bootstrapNS)
		}
	}()
	if err := kubectl(manifest("Namespace", "", bootstrapNS, nil), "apply", "-f", "-"); err != nil {
		return err
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	// The logins reach kubectl on stdin, never as command-line arguments.
	if err := kubectl(manifest("Secret", bootstrapNS, "creds", map[string]string{
		"PGUSER": b64(db.admin.Username), "PGPASSWORD": b64(db.admin.Password),
		"OWNER_USER": b64(db.owner.Username), "OWNER_PW": b64(db.owner.Password),
		"RW_USER": b64(db.rw.Username), "RW_PW": b64(db.rw.Password),
	}), "apply", "-f", "-"); err != nil {
		return err
	}
	// The ConfigMap carries the SQL files and the CA bundle to /setup.
	if err := kubectl(manifest("ConfigMap", bootstrapNS, "setup", map[string]string{
		"setup.sql": string(setupSQL), "alternates.sql": alternates, "rds-ca.pem": string(ca),
	}), "apply", "-f", "-"); err != nil {
		return err
	}
	if err := kubectl(nil, "-n", bootstrapNS, "delete", "job", "bootstrap", "--ignore-not-found"); err != nil {
		return err
	}
	if err := kubectl(job, "apply", "-f", "-"); err != nil {
		return err
	}
	if err := kubectl(nil, "-n", bootstrapNS, "wait", "--for=condition=complete", "job/bootstrap", "--timeout="+bootstrapTimeout); err != nil {
		_ = kubectl(nil, "-n", bootstrapNS, "logs", "job/bootstrap")
		return fmt.Errorf("database bootstrap Job did not complete: %w", err)
	}
	return kubectl(nil, "-n", bootstrapNS, "logs", "job/bootstrap")
}

// bootstrapCleanup bounds removeBootstrap, which runs with a context of its
// own because the deploy's may be what ended the bootstrap.
var bootstrapCleanup = 3 * time.Minute

// removeBootstrap deletes the Secret with the logins first, then the Job
// with its pod (foreground), then their namespace. After a success it waits
// for the namespace to go; after a failure or a cancel it does not, so the
// installer exits soon.
func removeBootstrap(w io.Writer, kc string, failed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapCleanup)
	defer cancel()
	kubectl := func(args ...string) error {
		return runCommand(ctx, w, command{Env: []string{"KUBECONFIG=" + kc}, Argv: append([]string{"kubectl"}, args...)})
	}
	ns := []string{"delete", "namespace", bootstrapNS, "--timeout=2m"}
	if failed {
		ns = []string{"delete", "namespace", bootstrapNS, "--ignore-not-found", "--wait=false"}
	}
	return errors.Join(
		kubectl("-n", bootstrapNS, "delete", "secret", "creds", "--ignore-not-found"),
		kubectl("-n", bootstrapNS, "delete", "job", "bootstrap", "--ignore-not-found", "--cascade=foreground", "--timeout=1m"),
		kubectl(ns...),
	)
}

// renderJob fills the Job's three placeholders, the only ones it has.
func renderJob(path, host string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the bootstrap Job: %w", err)
	}
	out := strings.NewReplacer("${NS}", bootstrapNS, "${IMAGE}", bootstrapImage, "${host}", host).Replace(string(b))
	if strings.Contains(out, "${") {
		return nil, fmt.Errorf("%s has a placeholder the installer does not fill", path)
	}
	return []byte(out), nil
}

// manifest is a v1 object as JSON, which kubectl apply reads like YAML.
func manifest(kind, namespace, name string, data map[string]string) []byte {
	meta := map[string]string{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	obj := map[string]any{"apiVersion": "v1", "kind": kind, "metadata": meta}
	if kind == "Secret" {
		obj["type"] = "Opaque"
	}
	if data != nil {
		obj["data"] = data
	}
	b, _ := json.Marshal(obj)
	return b
}
