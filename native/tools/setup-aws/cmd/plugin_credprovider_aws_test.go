package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// fakeSM is the Secrets Manager the credprovider-aws Create calls.
type fakeSM struct {
	tags     map[string]map[string]string // name -> tags
	values   map[string]string
	deleting map[string]bool
	busy     int // CreateSecret calls that fail "scheduled for deletion"
	writes   []string
}

func newFakeSM() *fakeSM {
	return &fakeSM{tags: map[string]map[string]string{}, values: map[string]string{}, deleting: map[string]bool{}}
}

func (f *fakeSM) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	n := aws.ToString(in.SecretId)
	t, ok := f.tags[n]
	if !ok {
		return nil, apiErr("ResourceNotFoundException")
	}
	out := &secretsmanager.DescribeSecretOutput{Name: aws.String(n)}
	for _, k := range sortedKeys(t) {
		out.Tags = append(out.Tags, smtypes.Tag{Key: aws.String(k), Value: aws.String(t[k])})
	}
	if f.deleting[n] {
		out.DeletedDate = aws.Time(time.Unix(0, 0))
	}
	return out, nil
}
func (f *fakeSM) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	n := aws.ToString(in.Name)
	if f.busy > 0 {
		f.busy--
		return nil, errors.New("InvalidRequestException: You can't create this secret because a secret with this name is already scheduled for deletion.")
	}
	m := map[string]string{}
	for _, t := range in.Tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.tags[n], f.values[n] = m, aws.ToString(in.SecretString)
	delete(f.deleting, n)
	f.writes = append(f.writes, "CreateSecret "+n)
	return &secretsmanager.CreateSecretOutput{}, nil
}
func (f *fakeSM) TagResource(_ context.Context, in *secretsmanager.TagResourceInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	n := aws.ToString(in.SecretId)
	for _, t := range in.Tags {
		f.tags[n][aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.writes = append(f.writes, "TagResource "+n)
	return &secretsmanager.TagResourceOutput{}, nil
}
func (f *fakeSM) UntagResource(_ context.Context, in *secretsmanager.UntagResourceInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	n := aws.ToString(in.SecretId)
	for _, k := range in.TagKeys {
		delete(f.tags[n], k)
	}
	f.writes = append(f.writes, "UntagResource "+n)
	return &secretsmanager.UntagResourceOutput{}, nil
}

func withFakeSM(t *testing.T, sm *fakeSM) {
	t.Helper()
	prev, prevRetry := cpSecretsFor, cpRetry
	t.Cleanup(func() { cpSecretsFor, cpRetry = prev, prevRetry })
	cpSecretsFor = func(context.Context, Config) (cpSecretsAPI, error) { return sm, nil }
	cpRetry.wait = time.Millisecond
}

const (
	cpInj = "ate/ate-demo-credprovider/demo-demo-token"
	cpUnt = "ate/ate-demo-credprovider/demo-untagged-token"
)

// Create makes the role (only tagged, owned secrets under ate/), the two
// secrets (only the first injectable) and the two repositories, all
// owner-tagged; --plan writes nothing and a rerun creates nothing.
func TestCredproviderCreate(t *testing.T) {
	d := pluginDefs["credprovider-aws"]
	f, sm := newFake(), newFakeSM()
	withFakeSM(t, sm)
	if err := d.Create(context.Background(), testEnv(t, f, true)); err != nil {
		t.Fatal(err)
	}
	if len(f.writes)+len(sm.writes) != 0 {
		t.Fatalf("plan wrote %v %v", f.writes, sm.writes)
	}
	if err := d.Create(context.Background(), testEnv(t, f, false)); err != nil {
		t.Fatal(err)
	}
	role := "demo-us-west-2-aws-sm-credential-provider"
	p := f.policies[role]
	for _, s := range []string{`"secretsmanager:GetSecretValue"`, `secret:ate/*"`, `"aws:ResourceTag/ate.dev/injectable":"true"`,
		`"aws:ResourceTag/agent-substrate-on-eks/deployment":"demo.us-west-2"`} {
		if !strings.Contains(p, s) {
			t.Errorf("policy lacks %s:\n%s", s, p)
		}
	}
	if f.assocs["ate-system/aws-sm-credential-provider"] == "" || f.roles[role][OwnerTagKey] != "demo.us-west-2" {
		t.Errorf("role %v, associations %v", f.roles[role], f.assocs)
	}
	if sm.tags[cpInj][cpInjectableTag] != "true" || sm.tags[cpUnt][cpInjectableTag] != "" {
		t.Errorf("injectable tags: %v / %v", sm.tags[cpInj], sm.tags[cpUnt])
	}
	for _, n := range []string{cpInj, cpUnt} {
		if sm.tags[n][OwnerTagKey] != "demo.us-west-2" || sm.tags[n]["auto-delete"] != "no" || len(sm.values[n]) != 40 {
			t.Errorf("secret %s: tags %v, value length %d", n, sm.tags[n], len(sm.values[n]))
		}
	}
	if sm.values[cpInj] == sm.values[cpUnt] {
		t.Error("the two secrets have one value")
	}
	for _, r := range []string{"demo/aws-sm-credential-provider", "demo/credprovider-probe"} {
		if !f.repos[r] || f.repoTags[r][OwnerTagKey] != "demo.us-west-2" {
			t.Errorf("repository %s: %v", r, f.repoTags[r])
		}
	}
	v := sm.values[cpInj]
	f.writes, sm.writes = nil, nil
	if err := d.Create(context.Background(), testEnv(t, f, false)); err != nil {
		t.Fatal(err)
	}
	if count(f.writes, "Create") != 0 || count(sm.writes, "Create") != 0 || sm.values[cpInj] != v {
		t.Errorf("rerun created %v %v", f.writes, sm.writes)
	}
}

// Both directions: a same-named secret of another owner stops Create; one
// still being deleted is created once its name is free.
func TestCredproviderSecretOwnerAndRetry(t *testing.T) {
	d := pluginDefs["credprovider-aws"]
	f, sm := newFake(), newFakeSM()
	withFakeSM(t, sm)
	sm.tags[cpInj] = map[string]string{OwnerTagKey: "other.us-west-2"}
	err := d.Create(context.Background(), testEnv(t, f, false))
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("want the owner refusal, got %v", err)
	}
	if count(sm.writes, "CreateSecret") != 0 {
		t.Errorf("wrote %v", sm.writes)
	}

	f, sm = newFake(), newFakeSM()
	withFakeSM(t, sm)
	sm.tags[cpInj] = map[string]string{OwnerTagKey: "demo.us-west-2"}
	sm.deleting[cpInj], sm.busy = true, 2
	if err := d.Create(context.Background(), testEnv(t, f, false)); err != nil {
		t.Fatal(err)
	}
	if sm.deleting[cpInj] || len(sm.values[cpInj]) != 40 {
		t.Errorf("not recreated: %v", sm.writes)
	}
	sm.busy = 99
	sm.tags = map[string]map[string]string{}
	if err := d.Create(context.Background(), testEnv(t, f, false)); err == nil {
		t.Error("a name held past the retries passed")
	}
}

func TestRandomToken(t *testing.T) {
	a, err := randomToken(40)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := randomToken(40)
	if len(a) != 40 || a == b || strings.Trim(a, tokenChars) != "" {
		t.Errorf("tokens %q %q", a, b)
	}
}

// Both directions for `setup-aws plugin credprovider-aws --delete`: it
// deletes the association, role, both secrets and both repositories and
// leaves every other resource; a secret of another owner stops it first.
func TestCredproviderDelete(t *testing.T) {
	d := pluginDefs["credprovider-aws"]
	add := func(f *tdFake, secretTags map[string]string) {
		f.put(&tdObj{kind: "assoc", id: "a-cp", ns: "ate-system", sa: "aws-sm-credential-provider", role: roleARN("demo-us-west-2-aws-sm-credential-provider"), tags: own()})
		f.put(&tdObj{kind: "role", id: "demo-us-west-2-aws-sm-credential-provider", inline: []string{"access"}, tags: own()})
		f.put(&tdObj{kind: "secret", id: cpInj, tags: secretTags})
		f.put(&tdObj{kind: "secret", id: cpUnt, tags: own()})
		f.put(&tdObj{kind: "repo", id: "demo/aws-sm-credential-provider", tags: own()})
		f.put(&tdObj{kind: "repo", id: "demo/credprovider-probe", tags: own()})
	}
	f := fullDeployment()
	add(f, own(cpInjectableTag, "true"))
	e, _, _ := tdTestEnv(t, f, false, false)
	rows, err := d.Inventory(context.Background(), e)
	if err != nil || len(rows) != 6 {
		t.Fatalf("inventory before = %v, %v", rows, err)
	}
	if err := e.deletePlugin(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"assoc|a-cp", "role|demo-us-west-2-aws-sm-credential-provider", "secret|" + cpInj, "secret|" + cpUnt,
		"repo|demo/aws-sm-credential-provider", "repo|demo/credprovider-probe"} {
		if f.objs[k] != nil {
			t.Errorf("%s left", k)
		}
	}
	for _, k := range []string{"repo|demo/counter", "assoc|a-api", "secret|demo/ate-owner"} {
		if f.objs[k] == nil {
			t.Errorf("deleted %s, which is not the plugin's", k)
		}
	}

	f = fullDeployment()
	add(f, map[string]string{OwnerTagKey: "other.us-west-2"})
	e, _, _ = tdTestEnv(t, f, false, false)
	if err := e.deletePlugin(context.Background(), d); err == nil || !strings.Contains(err.Error(), cpInj) {
		t.Fatalf("want the refusal naming %s, got %v", cpInj, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("deleted after a refusal: %v", f.calls)
	}
}
