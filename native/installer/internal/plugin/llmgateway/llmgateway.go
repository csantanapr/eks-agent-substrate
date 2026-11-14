package llmgateway

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// The gateway's names, as llm-gateway.yaml and setup-aws's
// plugin_llm_gateway.go have them.
const (
	ID        = "llm-gateway"
	namespace = "llm-gateway"
	deploy    = "deploy/llm-gateway"
	selector  = "app=llm-gateway"
	// DefaultProfile is BEDROCK_INFERENCE_PROFILE's default (config.env), the
	// model "claude"; setup-aws reads the same variable.
	DefaultProfile = "us.anthropic.claude-haiku-5-5"
	// smokeImage is the throwaway client of the smoke test.
	smokeImage = "public.ecr.aws/docker/library/busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
)

//go:embed llm-gateway.yaml
var manifest string

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "LLM gateway",
		Summary: "agentgateway on Amazon Bedrock with EKS Pod Identity: actors ask for claude, " +
			"claude-haiku, claude-sonnet or claude-opus and hold no AWS credentials",
		Order:    10,
		Minutes:  3,
		Default:  true,
		SetupAWS: true,
		Deploy:   Deploy,
		Verify:   Verify,
		Remove:   Remove,
	})
}

// Seams replaced in tests.
var (
	listProfiles = bedrockProfiles
	pause        = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
)

func defaultProfile() string {
	if v := os.Getenv("BEDROCK_INFERENCE_PROFILE"); v != "" {
		return v
	}
	return DefaultProfile
}

// render is llm-gateway.yaml with the Region and each model's profile.
func render(region string, models map[string]string) (string, error) {
	r := strings.NewReplacer(
		"REPLACE_WITH_AWS_REGION", region,
		"REPLACE_WITH_DEFAULT_PROFILE", models["claude"],
		"REPLACE_WITH_HAIKU_PROFILE", models["claude-haiku"],
		"REPLACE_WITH_SONNET_PROFILE", models["claude-sonnet"],
		"REPLACE_WITH_OPUS_PROFILE", models["claude-opus"])
	out := r.Replace(manifest)
	if i := strings.Index(out, "REPLACE_WITH_"); i >= 0 {
		return "", fmt.Errorf("llm-gateway.yaml: placeholder left at %q", out[i:min(len(out), i+40)])
	}
	for _, n := range ModelNames {
		if models[n] == "" {
			return "", fmt.Errorf("no inference profile for model %s", n)
		}
	}
	return out, nil
}

// Deploy makes the gateway after its IAM part: it picks the profiles,
// applies the gateway (ClusterIP only), restarts it so a changed model list
// takes effect, and makes sure its pods carry Pod Identity credentials.
func Deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	ps, err := listProfiles(ctx, o.Profile, o.Region)
	if err != nil {
		return err
	}
	models, err := Models(ps, defaultProfile())
	if err != nil {
		return err
	}
	for _, n := range ModelNames {
		fmt.Fprintf(w, "  %-14s %s\n", n, models[n])
	}
	m, err := render(o.Region, models)
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, []byte(m), nil, "apply", "-f", "-"); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", namespace, "rollout", "restart", deploy); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", namespace, "rollout", "status", deploy, "--timeout=180s"); err != nil {
		return err
	}
	return EnsurePodIdentity(ctx, w, o, namespace, deploy, selector)
}

// EnsurePodIdentity makes sure a workload's pods carry Pod Identity: EKS
// adds the Pod Identity credentials to a pod only when it is created, and a
// new association takes a few seconds to apply. It restarts the workload until
// every one of its pods has them.
func EnsurePodIdentity(ctx context.Context, w io.Writer, o plugin.Options, ns, obj, sel string) error {
	for attempt := 1; ; attempt++ {
		var out bytes.Buffer
		if err := plugin.Kubectl(ctx, w, o, nil, &out, "-n", ns, "get", "pods", "-l", sel, "-o", "json"); err != nil {
			return err
		}
		missing, err := lacksPodIdentity(out.Bytes())
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			fmt.Fprintf(w, "  %s/%s has Pod Identity credentials\n", ns, obj)
			return nil
		}
		if attempt == 4 {
			return fmt.Errorf("%s/%s pods still lack Pod Identity credentials: %s", ns, obj, strings.Join(missing, " "))
		}
		fmt.Fprintf(w, "  %s/%s started before its Pod Identity association applied; restarting\n", ns, obj)
		if err := pause(ctx, 10*time.Second); err != nil {
			return err
		}
		if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", ns, "rollout", "restart", obj); err != nil {
			return err
		}
		if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", ns, "rollout", "status", obj, "--timeout=5m"); err != nil {
			return err
		}
	}
}

// credentialsEnv is the variable EKS Pod Identity adds to each container.
const credentialsEnv = "AWS_CONTAINER_CREDENTIALS_FULL_URI"

// lacksPodIdentity names the pods (of `kubectl get pods -o json`), not being
// deleted, that have a container without credentialsEnv. A list with no pod
// is an error: there is nothing that could have the credentials.
func lacksPodIdentity(podsJSON []byte) ([]string, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Env []struct {
						Name string `json:"name"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(podsJSON, &l); err != nil {
		return nil, fmt.Errorf("read pods: %w", err)
	}
	var missing []string
	n := 0
	for _, p := range l.Items {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		n++
		for _, c := range p.Spec.Containers {
			has := false
			for _, e := range c.Env {
				has = has || e.Name == credentialsEnv
			}
			if !has {
				missing = append(missing, p.Metadata.Name)
				break
			}
		}
	}
	if n == 0 {
		return nil, fmt.Errorf("no running pod matches")
	}
	return missing, nil
}

// smokeTries and smokeWait give IAM about two minutes to settle.
const (
	smokeTries = 12
	smokeWait  = 10 * time.Second
)

// Verify is the smoke test: a throwaway pod in the llm-gateway
// namespace (which the NetworkPolicy admits) asks the gateway for each model
// name, and each must come back as a Bedrock message.
func Verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	for _, m := range ModelNames {
		if err := smoke(ctx, w, o, m); err != nil {
			return err
		}
	}
	return nil
}

func smoke(ctx context.Context, w io.Writer, o plugin.Options, model string) error {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":40,"messages":[{"role":"user","content":"Reply with: ready"}]}`, model)
	var last string
	for i := 1; i <= smokeTries; i++ {
		_ = plugin.Kubectl(ctx, io.Discard, o, nil, nil, "-n", namespace, "delete", "pod", "llm-smoke", "--ignore-not-found", "--wait=true")
		var out bytes.Buffer
		err := plugin.Kubectl(ctx, io.Discard, o, nil, &out, "-n", namespace, "run", "llm-smoke", "--rm", "-i",
			"--restart=Never", "--quiet", "--pod-running-timeout=3m", "--image="+smokeImage, "--",
			"wget", "-qO-", "--header", "content-type: application/json", "--post-data", body, "http://llm-gateway/v1/messages")
		if err == nil && answered(out.String()) {
			fmt.Fprintf(w, "  %s: Bedrock answered through the gateway\n", model)
			return nil
		}
		last = strings.TrimSpace(out.String())
		if err != nil {
			last = err.Error()
		}
		if i < smokeTries {
			if err := pause(ctx, smokeWait); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("%s: the LLM gateway did not get an answer from Bedrock (last: %.200s); see kubectl -n %s logs %s", model, last, namespace, deploy)
}

// answered reports an Anthropic Messages response: a JSON object of type
// "message" with some content. An error object, a gateway error page or an
// empty answer is not one.
func answered(s string) bool {
	var m struct {
		Type    string            `json:"type"`
		Content []json.RawMessage `json:"content"`
	}
	return json.Unmarshal([]byte(strings.TrimSpace(s)), &m) == nil && m.Type == "message" && len(m.Content) > 0
}

// Remove deletes the gateway's namespace and everything in it; Run then
// deletes its role and association (setup-aws plugin llm-gateway --delete).
func Remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	return plugin.Kubectl(ctx, w, o, nil, nil, "delete", "namespace", namespace, "--ignore-not-found", "--timeout=5m")
}
