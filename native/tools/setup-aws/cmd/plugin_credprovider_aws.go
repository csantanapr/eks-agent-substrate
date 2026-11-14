package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// The credprovider-aws plugin's AWS half, demos/credprovider-aws/deploy.sh's
// AWS part and clean.sh: the provider's Pod Identity role, which may read only
// this deployment's secrets under ate/ tagged ate.dev/injectable=true; the
// two probe secrets, one injectable and one not; and the ECR repositories the
// installer pushes the provider and probe images to. The installer's
// credprovideraws package uses the same names.
const (
	cpNamespace      = "ate-system"
	cpServiceAccount = "aws-sm-credential-provider"
	cpAtespace       = "ate-demo-credprovider"
	cpInjectableTag  = "ate.dev/injectable"
)

// cpRepositories are under <cluster>/, which the core teardown also deletes.
const cpRepositories = "aws-sm-credential-provider credprovider-probe"

// cpSecretNames are the injectable secret and the one without the tag. They
// carry the cluster name, so clusters in one account and Region do not share
// them.
func cpSecretNames(cluster string) (injectable, untagged string) {
	p := "ate/" + cpAtespace + "/" + cluster
	return p + "-demo-token", p + "-untagged-token"
}

// cpSecretsAPI is what Create calls on Secrets Manager.
type cpSecretsAPI interface {
	DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	CreateSecret(context.Context, *secretsmanager.CreateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	TagResource(context.Context, *secretsmanager.TagResourceInput, ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error)
	UntagResource(context.Context, *secretsmanager.UntagResourceInput, ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error)
}

var cpSecretsFor = func(ctx context.Context, c Config) (cpSecretsAPI, error) {
	pc, err := newPluginClients(ctx, c)
	if err != nil {
		return nil, err
	}
	return pc.SM, nil
}

// cpRetry paces the create of a secret whose name a delete still holds.
var cpRetry = struct {
	tries int
	wait  time.Duration
}{12, 10 * time.Second}

func init() {
	registerPlugin(pluginDef{ID: "credprovider-aws",
		Short:  "Create the Secrets Manager credential provider's IAM role, probe secrets and ECR repositories",
		Create: func(ctx context.Context, e *env) error { return e.createCredprovider(ctx) },
		Teardown: func(e *tdEnv) []pluginStep {
			inj, unt := cpSecretNames(e.n.Cluster)
			return append(e.podIdentitySteps(cpNamespace, cpServiceAccount),
				pluginStep{Step: func(ctx context.Context) ([]tdTarget, []string, error) { return e.cpSecretTargets(ctx, inj, unt) }},
				pluginStep{Step: e.cpRepositoryTargets})
		},
		Inventory: func(ctx context.Context, e *tdEnv) ([]tdRow, error) { return e.cpInventory(ctx) },
	})
}

// cpPolicy lets the provider read a secret only under ate/, tagged
// injectable, and owned by this deployment.
func (e *env) cpPolicy() string {
	n := e.names
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"ReadInjectableSecrets","Effect":"Allow",`+
		`"Action":"secretsmanager:GetSecretValue","Resource":"arn:%s:secretsmanager:%s:%s:secret:ate/*",`+
		`"Condition":{"StringEquals":{"aws:ResourceTag/%s":"true","aws:ResourceTag/%s":"%s"}}}]}`,
		n.Partition, n.Region, n.Account, cpInjectableTag, OwnerTagKey, n.OwnerValue)
}

func (e *env) createCredprovider(ctx context.Context) error {
	// First, so the association has applied by the time the provider's pods start.
	if err := e.roleForSA(ctx, PodIdentity{cpNamespace, cpServiceAccount, e.cpPolicy()}); err != nil {
		return err
	}
	sm, err := cpSecretsFor(ctx, e.cfg)
	if err != nil {
		return err
	}
	inj, unt := cpSecretNames(e.names.Cluster)
	if err := e.cpSecret(ctx, sm, inj, true, "Agent Substrate credential injection demo token ("+e.names.Cluster+")"); err != nil {
		return err
	}
	if err := e.cpSecret(ctx, sm, unt, false, "Agent Substrate demo: not injectable, the provider must be refused ("+e.names.Cluster+")"); err != nil {
		return err
	}
	re := *e
	re.cfg.ECRRepositories = cpRepositories
	return re.createECR(ctx)
}

// cpSecret makes name with a random value the first time, owner-tagged, and
// sets or removes the injectable tag every time. The value goes only into
// the CreateSecret request: never an argv, a file or a log line.
func (e *env) cpSecret(ctx context.Context, sm cpSecretsAPI, name string, injectable bool, desc string) error {
	d, err := sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	switch {
	case err == nil && d.DeletedDate == nil:
		if got := kvMap(d.Tags, smKV)[OwnerTagKey]; got != e.names.OwnerValue {
			return fmt.Errorf("secret %s exists but is not owned by %s (%s=%q)", name, e.names.OwnerValue, OwnerTagKey, got)
		}
		slog.Info("secret exists", "secret", name)
	case err != nil && !isNotFound(err):
		return fmt.Errorf("describe secret %s: %w", name, err)
	default: // missing, or being deleted: its name frees shortly
		if err := e.act("create secret "+name, func() error { return e.cpCreateSecret(ctx, sm, name, desc) }); err != nil {
			return err
		}
	}
	if injectable {
		return e.act("tag secret "+name+" "+cpInjectableTag+"=true", func() error {
			_, err := sm.TagResource(ctx, &secretsmanager.TagResourceInput{SecretId: aws.String(name),
				Tags: []smtypes.Tag{{Key: aws.String(cpInjectableTag), Value: aws.String("true")}}})
			return err
		})
	}
	return e.act("remove tag "+cpInjectableTag+" from secret "+name, func() error {
		_, err := sm.UntagResource(ctx, &secretsmanager.UntagResourceInput{SecretId: aws.String(name), TagKeys: []string{cpInjectableTag}})
		return err
	})
}

func (e *env) cpCreateSecret(ctx context.Context, sm cpSecretsAPI, name, desc string) error {
	value, err := randomToken(40)
	if err != nil {
		return err
	}
	tags := make([]smtypes.Tag, 0, len(e.tags))
	for _, k := range sortedKeys(e.tags) {
		tags = append(tags, smtypes.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
	}
	for i := 1; ; i++ {
		_, err = sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String(name), Description: aws.String(desc),
			SecretString: aws.String(value), Tags: tags})
		// A secret deleted moments ago holds its name for a short while.
		if err == nil || i >= cpRetry.tries || !strings.Contains(err.Error(), "scheduled for deletion") {
			break
		}
		slog.Info("secret is still being deleted; retrying", "secret", name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cpRetry.wait):
		}
	}
	if err != nil {
		return fmt.Errorf("create secret %s: %w", name, err)
	}
	return nil
}

const tokenChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomToken is n characters from tokenChars, from crypto/rand.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(tokenChars)))
	for i := range b {
		x, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = tokenChars[x.Int64()]
	}
	return string(b), nil
}

// ---- teardown and inventory ---------------------------------------------------

// cpSecretTargets deletes the named secrets with no recovery window, so a new
// deploy can create them again at once, and waits for the names to be free.
func (e *tdEnv) cpSecretTargets(ctx context.Context, names ...string) ([]tdTarget, []string, error) {
	var ts []tdTarget
	var rs []string
	for _, name := range names {
		s, err := e.secret(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if s == nil {
			continue
		}
		if r := e.refuse("secret", name, kvMap(s.Tags, smKV)); r != "" {
			rs = append(rs, r)
			continue
		}
		n := name
		t := tdTarget{kind: "secret", id: n, wait: gone(func(ctx context.Context) (bool, error) {
			s, err := e.secret(ctx, n)
			return s != nil, err
		})}
		if s.DeletedDate == nil {
			t.del = func(ctx context.Context) error {
				_, err := e.c.SM.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: aws.String(n), ForceDeleteWithoutRecovery: aws.Bool(true)})
				return err
			}
		}
		ts = append(ts, t)
	}
	return ts, rs, nil
}

// cpRepoNames are the full names of the plugin's repositories.
func (e *tdEnv) cpRepoNames() map[string]bool {
	m := map[string]bool{}
	for _, r := range strings.Fields(cpRepositories) {
		m[e.n.ECRPrefix+"/"+r] = true
	}
	return m
}

// cpRepositoryTargets deletes the plugin's two repositories with their images.
func (e *tdEnv) cpRepositoryTargets(ctx context.Context) ([]tdTarget, []string, error) {
	repos, err := e.repositories(ctx)
	if err != nil {
		return nil, nil, err
	}
	mine := e.cpRepoNames()
	var ts []tdTarget
	var rs []string
	for _, name := range tdKeys(repos) {
		if !mine[name] {
			continue
		}
		if r := e.refuse("ECR repository", name, repos[name]); r != "" {
			rs = append(rs, r)
			continue
		}
		n := name
		ts = append(ts, tdTarget{kind: "ECR repository", id: n + " (and its images)", del: func(ctx context.Context) error {
			_, err := e.c.ECR.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{RepositoryName: aws.String(n), Force: true})
			return err
		}})
	}
	return ts, rs, nil
}

// cpInventory is what is left of the plugin with this deployment's owner tag,
// labeled as the core inventory labels it.
func (e *tdEnv) cpInventory(ctx context.Context) ([]tdRow, error) {
	rows, err := e.podIdentityRows(ctx, cpNamespace, cpServiceAccount)
	if err != nil {
		return nil, err
	}
	inj, unt := cpSecretNames(e.n.Cluster)
	for _, name := range []string{inj, unt} {
		s, err := e.secret(ctx, name)
		if err != nil {
			return nil, err
		}
		if s != nil && s.DeletedDate == nil && e.owned(kvMap(s.Tags, smKV)) {
			rows = append(rows, tdRow{Kind: "secret", ID: name, Tags: kvMap(s.Tags, smKV)})
		}
	}
	repos, err := e.repositories(ctx)
	if err != nil {
		return nil, err
	}
	mine := e.cpRepoNames()
	for _, name := range tdKeys(repos) {
		if mine[name] && e.owned(repos[name]) {
			rows = append(rows, tdRow{Kind: "ECR repository", ID: name, Tags: repos[name]})
		}
	}
	return rows, nil
}
