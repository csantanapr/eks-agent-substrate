package dbrotation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/awsload"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Verify rotates both logins now and checks the chain end to end, as
// demos/db-rotation/verify.sh did: Secrets Manager switches each login to
// the other user, External Secrets writes the new connection strings, the
// ESO Reloader rolls ate-api-server, and the new pods serve the API as the
// new users. Each run flips both logins (user <-> user_clone).
//
// Secrets Manager labels secret versions: AWSCURRENT is the one in use, and
// AWSPENDING the one a rotation is making. A rotation is done when
// AWSCURRENT has moved to a new version and no version is pending on its own.
//
//	https://docs.aws.amazon.com/secretsmanager/latest/userguide/rotate-secrets_lambda-functions.html
//	https://github.com/external-secrets/reloader
//
// No password is printed: usernames are read from the logins, and the
// connection strings are parsed in memory.

// Lines Verify prints.
const (
	RotatedLine   = "Rotated "
	SyncedLine    = "External Secrets wrote the new connection strings in "
	RolledLine    = "The ESO Reloader rolled ate-api-server in "
	APILine       = "ate-api-server serves the API as the new users: "
	VerifyPass    = "PASS: rotation, sync, rollout and API all done"
	reloadedKey   = "reloader.external-secrets.io/last-reloaded"
	ownerDSNKey   = "ATE_API_POSTGRES_OWNER_CONNECTION_STRING"
	rwDSNKey      = "ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING"
	schemaKey     = "ATE_API_POSTGRES_SCHEMA"
	checkAtespace = "db-rotation-check"
)

// smAPI is what Verify calls in Secrets Manager.
type smAPI interface {
	DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	RotateSecret(context.Context, *secretsmanager.RotateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.RotateSecretOutput, error)
}

// Seams replaced in tests.
var (
	newSM = func(ctx context.Context, profile, region string) (smAPI, error) {
		// awsload: the rotation wait can outlive the session.
		cfg, err := awsload.Config(ctx, region, profile)
		if err != nil {
			return nil, err
		}
		return secretsmanager.NewFromConfig(cfg), nil
	}
	// interval is the wait between reads; each wait allows waitTries of them.
	interval  = 5 * time.Second
	waitTries = 60
	// kubectlAte builds kubectl-ate from the cached tree and returns its path.
	kubectlAte = buildKubectlAte
)

var logins = []string{"owner", "readwrite"}

func secretName(o plugin.Options, who string) string { return o.Cluster + "/ate-" + who }

func verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	sm, err := newSM(ctx, o.Profile, o.Region)
	if err != nil {
		return err
	}
	v := &verifier{w: w, o: o, sm: sm}
	return v.run(ctx)
}

type verifier struct {
	w  io.Writer
	o  plugin.Options
	sm smAPI
}

func (v *verifier) run(ctx context.Context) error {
	before := map[string]string{} // login -> user
	version := map[string]string{}
	for _, who := range logins {
		u, err := v.user(ctx, who)
		if err != nil {
			return err
		}
		cur, _, err := v.stages(ctx, who)
		if err != nil {
			return err
		}
		before[who], version[who] = u, cur
	}
	oldPods, err := pods(ctx, v.w, v.o, ateNS, "app="+apiServer)
	if err != nil {
		return err
	}
	oldMark, err := v.reloaded(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(v.w, "Before: owner login %s, readwrite login %s, pods %s\n", before["owner"], before["readwrite"], names(oldPods))

	// Rotate both, then wait for each.
	t0 := time.Now()
	for _, who := range logins {
		if _, err := v.sm.RotateSecret(ctx, &secretsmanager.RotateSecretInput{SecretId: aws.String(secretName(v.o, who))}); err != nil {
			return fmt.Errorf("rotate %s: %w", secretName(v.o, who), err)
		}
	}
	after := map[string]string{}
	for _, who := range logins {
		if err := v.wait(ctx, secretName(v.o, who)+" to finish its rotation", func() (bool, error) {
			cur, pendingOnly, err := v.stages(ctx, who)
			return cur != version[who] && pendingOnly == 0, err
		}); err != nil {
			return fmt.Errorf("%w; see /aws/lambda/%s-db-%s in CloudWatch Logs", err, v.o.Cluster, who)
		}
		u, err := v.user(ctx, who)
		if err != nil {
			return err
		}
		if u == before[who] {
			return fmt.Errorf("%s still uses %s after its rotation", secretName(v.o, who), u)
		}
		after[who] = u
		fmt.Fprintf(v.w, "%s%s: %s -> %s\n", RotatedLine, secretName(v.o, who), before[who], u)
	}
	fmt.Fprintf(v.w, "Rotated both logins in %s\n", since(t0))

	t0 = time.Now()
	if err := v.wait(ctx, "External Secrets to write the new connection strings", func() (bool, error) {
		users, err := v.envUsers(ctx)
		return err == nil && users[ownerDSNKey] == after["owner"] && users[rwDSNKey] == after["readwrite"], err
	}); err != nil {
		return err
	}
	fmt.Fprintf(v.w, "%s%s\n", SyncedLine, since(t0))

	t0 = time.Now()
	if err := v.wait(ctx, "the ESO Reloader to restart ate-api-server", func() (bool, error) {
		m, err := v.reloaded(ctx)
		return m != oldMark, err
	}); err != nil {
		return err
	}
	if err := kubectl(ctx, v.w, v.o, nil, "-n", ateNS, "rollout", "status", "deploy/"+apiServer, "--timeout=5m"); err != nil {
		return err
	}
	var newPods []pod
	if err := v.wait(ctx, "the pods from before the rotation to be gone", func() (bool, error) {
		var err error
		newPods, err = pods(ctx, v.w, v.o, ateNS, "app="+apiServer)
		return err == nil && len(newPods) > 0 && len(stale(oldPods, newPods)) == 0, err
	}); err != nil {
		return err
	}
	mark, err := v.reloaded(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(v.w, "%s%s (%s=%s; pods %s)\n", RolledLine, since(t0), reloadedKey, mark, names(newPods))

	for _, p := range newPods {
		logs, err := output(ctx, v.w, v.o, "-n", ateNS, "logs", p.Metadata.Name, "--all-containers=true")
		if err != nil {
			return err
		}
		if loginFailed(logs) {
			return fmt.Errorf("pod %s cannot log in to the database or assume its role (kubectl -n %s logs %s)", p.Metadata.Name, ateNS, p.Metadata.Name)
		}
	}
	if err := v.api(ctx); err != nil {
		return err
	}
	fmt.Fprintln(v.w, VerifyPass)
	return nil
}

// wait polls done every interval, waitTries times.
func (v *verifier) wait(ctx context.Context, what string, done func() (bool, error)) error {
	var last error
	for i := 0; i < waitTries; i++ {
		ok, err := done()
		if ok {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	if last != nil {
		return fmt.Errorf("gave up waiting for %s: %w", what, last)
	}
	return fmt.Errorf("gave up waiting for %s after %s", what, time.Duration(waitTries)*interval)
}

// user is the login's username. The password is read with it and dropped.
func (v *verifier) user(ctx context.Context, who string) (string, error) {
	name := secretName(v.o, who)
	out, err := v.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	var l struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &l); err != nil || l.Username == "" {
		return "", fmt.Errorf("%s has no username", name)
	}
	return l.Username, nil
}

// stages returns the AWSCURRENT version and the count of versions that are
// AWSPENDING without being AWSCURRENT (a rotation still running).
func (v *verifier) stages(ctx context.Context, who string) (current string, pendingOnly int, err error) {
	name := secretName(v.o, who)
	out, err := v.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if err != nil {
		return "", 0, fmt.Errorf("describe %s: %w", name, err)
	}
	current, pendingOnly = stagesOf(out.VersionIdsToStages)
	return current, pendingOnly, nil
}

func stagesOf(m map[string][]string) (current string, pendingOnly int) {
	for id, st := range m {
		cur, pend := slices.Contains(st, "AWSCURRENT"), slices.Contains(st, "AWSPENDING")
		if cur {
			current = id
		}
		if pend && !cur {
			pendingOnly++
		}
	}
	return current, pendingOnly
}

// reloaded is the Reloader's mark on ate-api-server's pod template.
func (v *verifier) reloaded(ctx context.Context) (string, error) {
	s, err := output(ctx, v.w, v.o, "-n", ateNS, "get", "deployment", apiServer, "-o", "json")
	if err != nil {
		return "", err
	}
	var d struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return "", fmt.Errorf("read deployment %s/%s: %w", ateNS, apiServer, err)
	}
	return d.Spec.Template.Metadata.Annotations[reloadedKey], nil
}

// envUsers reads ate-api-server-secret-envvars and returns the user of each
// connection string, key -> user. Its keys must be exactly the two
// connection strings and the schema, so the raw login fields never land
// there. The values stay in memory.
func (v *verifier) envUsers(ctx context.Context) (map[string]string, error) {
	s, err := output(ctx, v.w, v.o, "-n", ateNS, "get", "secret", envSecret, "-o", "json")
	if err != nil {
		return nil, err
	}
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(s), &sec); err != nil {
		return nil, fmt.Errorf("read Secret %s/%s", ateNS, envSecret)
	}
	return dsnUsers(sec.Data)
}

// dsnUsers checks the Secret's keys and returns each connection string's user.
func dsnUsers(data map[string]string) (map[string]string, error) {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{ownerDSNKey, rwDSNKey, schemaKey}; !slices.Equal(keys, want) {
		return nil, fmt.Errorf("Secret %s/%s has keys %s, want exactly %s", ateNS, envSecret, strings.Join(keys, " "), strings.Join(want, " "))
	}
	out := map[string]string{}
	for _, k := range []string{ownerDSNKey, rwDSNKey} {
		b, err := base64.StdEncoding.DecodeString(data[k])
		if err != nil {
			return nil, fmt.Errorf("%s in %s is not base64", k, envSecret)
		}
		// The error never quotes the value: it holds the password.
		u, err := url.Parse(strings.TrimSpace(string(b)))
		if err != nil || u.User == nil || u.User.Username() == "" {
			return nil, fmt.Errorf("%s in %s is not a postgres:// URL with a user", k, envSecret)
		}
		out[k] = u.User.Username()
	}
	return out, nil
}

var loginFailure = regexp.MustCompile(`(?i)password authentication failed|assuming postgresql role`)

func loginFailed(logs string) bool { return loginFailure.MatchString(logs) }

// stale are the pods of before still running after.
func stale(before, after []pod) []string {
	var out []string
	for _, a := range after {
		for _, b := range before {
			if a.Metadata.Name == b.Metadata.Name {
				out = append(out, a.Metadata.Name)
			}
		}
	}
	return out
}

func names(ps []pod) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Metadata.Name)
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, " ")
}

func since(t time.Time) string { return time.Since(t).Round(time.Second).String() }

// api writes, reads and deletes an atespace through ate-api-server with
// kubectl-ate: the read-write login on the new pods does the work.
func (v *verifier) api(ctx context.Context) error {
	bin, err := kubectlAte(ctx, v.w, v.o)
	if err != nil {
		return err
	}
	ate := func(w, stdout io.Writer, args ...string) error {
		argv := append(append([]string{bin}, args...), "--kubeconfig", v.o.Kubeconfig, "--context", v.o.Cluster)
		return plugin.Exec(ctx, w, plugin.Cmd{Env: []string{"KUBECONFIG=" + v.o.Kubeconfig}, Stdout: stdout, Argv: argv})
	}
	// One left over from a run that stopped halfway: its absence, the
	// usual case, is not worth a line.
	_ = ate(io.Discard, io.Discard, "delete", "atespace", checkAtespace)
	if err := ate(v.w, nil, "create", "atespace", checkAtespace); err != nil {
		return fmt.Errorf("create atespace %s through the API: %w", checkAtespace, err)
	}
	var got strings.Builder
	if err := ate(v.w, &got, "get", "atespace", checkAtespace, "-o", "json"); err != nil {
		return fmt.Errorf("read atespace %s through the API: %w", checkAtespace, err)
	}
	if !strings.Contains(got.String(), `"`+checkAtespace+`"`) {
		return fmt.Errorf("the API returned no atespace %s", checkAtespace)
	}
	if err := ate(v.w, nil, "delete", "atespace", checkAtespace); err != nil {
		return fmt.Errorf("delete atespace %s through the API: %w", checkAtespace, err)
	}
	fmt.Fprintf(v.w, "%screated, read and deleted atespace %s\n", APILine, checkAtespace)
	return nil
}

// buildKubectlAte builds kubectl-ate from the release tree the control plane
// deploy cached, beside the demo's build of it.
func buildKubectlAte(ctx context.Context, w io.Writer, o plugin.Options) (string, error) {
	tree := controlplane.TreeDir(o.TreeRoot, o.Tag)
	if _, err := os.Stat(filepath.Join(tree, "cmd", "kubectl-ate")); err != nil {
		return "", fmt.Errorf("no Substrate %s tree at %s to build kubectl-ate from: run --deploy-control-plane first", o.Tag, tree)
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	bin := filepath.Join(dir, "substrate-eks", "bin", "kubectl-ate-"+o.Tag)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintln(w, "Building kubectl-ate from substrate "+o.Tag)
	err = plugin.Exec(ctx, w, plugin.Cmd{Dir: tree, Env: []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=vendor", "CGO_ENABLED=0"},
		Argv: []string{"go", "build", "-o", bin, "./cmd/kubectl-ate"}})
	if err != nil {
		return "", errors.New("build kubectl-ate: " + err.Error())
	}
	return bin, nil
}
