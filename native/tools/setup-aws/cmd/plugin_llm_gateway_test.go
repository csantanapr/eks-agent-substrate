package cmd

import (
	"context"
	"strings"
	"testing"
)

func prof(id, status string, models ...string) bedrockProfile {
	p := bedrockProfile{ID: id, ARN: "arn:aws:bedrock:us-west-2:111122223333:inference-profile/" + id, Status: status}
	for _, m := range models {
		p.Models = append(p.Models, struct {
			ARN string `json:"modelArn"`
		}{m})
	}
	return p
}

// A Region's list as Bedrock returns it, newest last per family but not sorted.
func llmList() []bedrockProfile {
	fm := "arn:aws:bedrock:us-east-1::foundation-model/x"
	return []bedrockProfile{
		prof("us.anthropic.claude-3-haiku-20240307-v1:0", "ACTIVE", fm),
		prof("us.anthropic.claude-haiku-4-5-20251001-v1:0", "ACTIVE", fm),
		prof("us.anthropic.claude-haiku-5-5", "ACTIVE", fm, fm+"2"),
		prof("us.anthropic.claude-sonnet-5-5", "ACTIVE", fm),
		prof("us.anthropic.claude-sonnet-4-20250514-v1:0", "ACTIVE", fm),
		prof("us.anthropic.claude-sonnet-9", "LEGACY", fm),
		prof("us.anthropic.claude-opus-4-8", "ACTIVE", fm),
		prof("us.anthropic.claude-opus-5", "ACTIVE", fm),
		prof("eu.anthropic.claude-opus-9", "ACTIVE", fm),
	}
}

// Both directions: the newest ACTIVE profile of the prefix wins (5.5 over
// 4.5 with a date; a date reads as .0; a LEGACY or other-prefix one never);
// no match is reported.
func TestNewestProfile(t *testing.T) {
	ps := llmList()
	for prefix, want := range map[string]string{
		"us.anthropic.claude-haiku-":  "us.anthropic.claude-haiku-5-5",
		"us.anthropic.claude-sonnet-": "us.anthropic.claude-sonnet-5-5",
		"us.anthropic.claude-opus-":   "us.anthropic.claude-opus-5",
	} {
		if got, ok := newestProfile(ps, prefix); !ok || got.ID != want {
			t.Errorf("%s: got %q %v, want %s", prefix, got.ID, ok, want)
		}
	}
	if got, ok := newestProfile(ps, "us.anthropic.claude-fable-"); ok {
		t.Errorf("no fable profile, got %s", got.ID)
	}
	if got, _ := newestProfile([]bedrockProfile{prof("us.anthropic.claude-sonnet-4-20250514-v1:0", "ACTIVE"), prof("us.anthropic.claude-sonnet-4-1", "ACTIVE")},
		"us.anthropic.claude-sonnet-"); got.ID != "us.anthropic.claude-sonnet-4-1" {
		t.Errorf("4.1 must beat a dated 4.0, got %s", got.ID)
	}
}

func TestLLMModels(t *testing.T) {
	m, err := llmModels(llmList(), "us.anthropic.claude-haiku-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if m["claude"].ID != "us.anthropic.claude-haiku-5-5" || m["claude-opus"].ID != "us.anthropic.claude-opus-5" || len(m) != 4 {
		t.Errorf("models = %v", m)
	}
	if _, err := llmModels(llmList(), "us.anthropic.claude-nope"); err == nil {
		t.Error("an unlisted default profile passed")
	}
	if _, err := llmModels(llmList(), "eu.anthropic.claude-opus-9"); err == nil || !strings.Contains(err.Error(), "haiku") {
		t.Errorf("eu has no haiku, got %v", err)
	}
	if _, err := llmModels(llmList(), "noprefix"); err == nil {
		t.Error("a profile without a Region prefix passed")
	}
}

// The policy grants each distinct profile and its models only through it; a
// profile without models is refused.
func TestLLMPolicy(t *testing.T) {
	m, _ := llmModels(llmList(), "us.anthropic.claude-haiku-5-5")
	p, err := llmPolicy(m)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(p, `"bedrock:InferenceProfileArn"`); n != 3 {
		t.Errorf("want 3 conditioned grants (claude and claude-haiku share one profile), got %d:\n%s", n, p)
	}
	for _, s := range []string{"inference-profile/us.anthropic.claude-haiku-5-5", "inference-profile/us.anthropic.claude-sonnet-5-5",
		"inference-profile/us.anthropic.claude-opus-5", "bedrock:InvokeModelWithResponseStream"} {
		if !strings.Contains(p, s) {
			t.Errorf("policy lacks %s", s)
		}
	}
	if strings.Contains(p, "claude-opus-4-8") || strings.Contains(p, `"*"`) {
		t.Errorf("policy grants more than the four models:\n%s", p)
	}
	m["claude-opus"] = prof("us.anthropic.claude-opus-5", "ACTIVE")
	if _, err := llmPolicy(m); err == nil {
		t.Error("a profile listing no models passed")
	}
}

// Create makes the owner-tagged role with that policy and the association;
// --plan writes nothing.
func TestLLMGatewayCreate(t *testing.T) {
	prev := bedrockProfiles
	t.Cleanup(func() { bedrockProfiles = prev })
	bedrockProfiles = func(context.Context, Config) ([]bedrockProfile, error) { return llmList(), nil }
	d := pluginDefs["llm-gateway"]

	f := newFake()
	if err := d.Create(context.Background(), testEnv(t, f, true)); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("plan wrote %v", f.writes)
	}
	if err := d.Create(context.Background(), testEnv(t, f, false)); err != nil {
		t.Fatal(err)
	}
	role := "demo-us-west-2-llm-gateway"
	if f.roles[role][OwnerTagKey] != "demo.us-west-2" || f.roles[role]["auto-delete"] != "no" {
		t.Errorf("role tags = %v", f.roles[role])
	}
	if !strings.Contains(f.policies[role], "claude-sonnet-5-5") {
		t.Errorf("policy = %s", f.policies[role])
	}
	if f.assocs["llm-gateway/llm-gateway"] != "arn:aws:iam::111122223333:role/"+role {
		t.Errorf("association = %v", f.assocs)
	}
}

// Both directions for `setup-aws plugin llm-gateway --delete`: it deletes the
// association and the role and leaves the rest; a role with another owner
// stops it before any delete.
func TestLLMGatewayDelete(t *testing.T) {
	d := pluginDefs["llm-gateway"]
	f := fullDeployment()
	f.put(&tdObj{kind: "assoc", id: "a-llm", ns: "llm-gateway", sa: "llm-gateway", role: roleARN("demo-us-west-2-llm-gateway"), tags: own()})
	f.put(&tdObj{kind: "role", id: "demo-us-west-2-llm-gateway", inline: []string{"access"}, tags: own()})
	e, _, _ := tdTestEnv(t, f, false, false)
	rows, err := d.Inventory(context.Background(), e)
	if err != nil || len(rows) != 2 {
		t.Fatalf("inventory before = %v, %v", rows, err)
	}
	if err := e.deletePlugin(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if f.objs["assoc|a-llm"] != nil || f.objs["role|demo-us-west-2-llm-gateway"] != nil {
		t.Errorf("left: %v", f.remaining())
	}
	if f.objs["assoc|a-api"] == nil || f.objs["role|demo-us-west-2-atelet"] == nil {
		t.Error("deleted a core resource")
	}

	f = fullDeployment()
	f.put(&tdObj{kind: "assoc", id: "a-llm", ns: "llm-gateway", sa: "llm-gateway", role: roleARN("demo-us-west-2-llm-gateway"), tags: own()})
	f.put(&tdObj{kind: "role", id: "demo-us-west-2-llm-gateway", tags: map[string]string{OwnerTagKey: "other.us-west-2"}})
	e, _, _ = tdTestEnv(t, f, false, false)
	if err := e.deletePlugin(context.Background(), d); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("want the refusal, got %v", err)
	}
	if f.objs["assoc|a-llm"] == nil {
		t.Error("deleted the association after a refusal")
	}
}
