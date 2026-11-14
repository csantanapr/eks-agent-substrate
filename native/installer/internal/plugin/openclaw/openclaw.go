package openclaw

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
)

// Names of the demo (always-on-agent's manifests/eks).
const (
	ID           = "openclaw"
	StorageClass = "efs-openclaw"
	Namespace    = "openclaw"
	Atespace     = "openclaw-demo"
	Actor        = "oc-agent"
	Template     = "openclaw-agent"
	Pool         = "openclaw"
	GatewayPVC   = "openclaw-gateway-pvc"
	GatewayID    = "spiffe://cluster.local/ns/openclaw/sa/openclaw-gateway"
	DashboardID  = "spiffe://cluster.local/ns/openclaw/sa/openclaw-dashboard"
	// gatewayPrincipal is how ateapi names the gateway's ServiceAccount.
	gatewayPrincipal = "user:system:serviceaccount:openclaw:openclaw-gateway"
	// The gateway runs as uid 1000 and refuses a state directory it does
	// not own; EFS ignores a pod's fsGroup, so its access point is 1000's.
	gatewayUID = "1000"
	secretName = "openclaw-secrets"
)

// The demo's images, published from csantanapr/always-on-agent for
// linux/amd64 and pinned by digest (actor snapshots require a pinned image).
// OPENCLAW_GATEWAY_IMAGE, OPENCLAW_ACTOR_IMAGE and OPENCLAW_DASHBOARD_IMAGE
// replace them. The actor image is built with
// ACTOR_CONFIG=build/actor/openclaw.eks.json.
const (
	GatewayImage   = "ghcr.io/csantanapr/always-on-agent/openclaw-gateway"
	ActorImage     = "ghcr.io/csantanapr/always-on-agent/openclaw-actor"
	DashboardImage = "ghcr.io/csantanapr/always-on-agent/openclaw-dashboard"
)

// The manifests of github.com/csantanapr/always-on-agent, manifests/eks at
// commit cfca8780fbf029e6a7283a31ae2e6ce1bb9ec907, unchanged: the plugin
// renders their REPLACE_WITH_ placeholders itself, as the repository's
// demo/eks/deploy-demo.sh does after its image build.
var (
	//go:embed manifests/gateway.yaml
	gatewayYAML string
	//go:embed manifests/gateway-config.yaml
	gatewayConfigYAML string
	//go:embed manifests/workerpool.yaml
	workerPoolYAML string
	//go:embed manifests/actortemplate.yaml
	actorTemplateYAML string
	//go:embed manifests/egress-policy.json
	egressPolicyJSON string
	//go:embed manifests/dashboard.yaml
	dashboardYAML string
)

// Seams replaced in tests.
var (
	pinImage = func(ctx context.Context, tagged string) (string, error) {
		ref, err := name.ParseReference(tagged)
		if err != nil {
			return "", err
		}
		d, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous))
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", tagged, err)
		}
		return tagged + "@" + d.Digest.String(), nil
	}
	newToken = func() (string, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
	// goldenTries bounds the golden snapshot wait, at efs.Wait each: 10
	// minutes, deploy-demo.sh's GOLDEN_TIMEOUT.
	goldenTries = 300
)

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "OpenClaw always-on agent",
		Summary: "An always-on OpenClaw gateway with its state on EFS; each conversation's agent is an actor that suspends when idle " +
			"and resumes on the next message. Reached with kubectl port-forward only. Needs its gateway, actor and dashboard images " +
			"pinned by digest (OPENCLAW_GATEWAY_IMAGE, OPENCLAW_ACTOR_IMAGE, OPENCLAW_DASHBOARD_IMAGE).",
		Order:    40,
		Minutes:  15,
		Requires: []string{efs.ID},
		Needs:    plugin.Needs{IngressIDs: []string{GatewayID, DashboardID}},
		Deploy:   deploy,
		Verify:   verify,
		Remove:   remove,
	})
}

var digestRef = regexp.MustCompile(`^[a-z0-9./:_-]+@sha256:[0-9a-f]{64}$`)

// image is the pinned ref of one of the demo's images: env, or def.
func image(env, def string) (string, error) {
	ref := def
	if v := os.Getenv(env); v != "" {
		ref = v
	}
	if !digestRef.MatchString(ref) {
		return "", fmt.Errorf("image %s is not pinned by digest: set %s=<image>@sha256:<digest>", ref, env)
	}
	return ref, nil
}

// images are the gateway, actor and dashboard refs.
type images struct{ gateway, actor, dashboard, worker string }

// pinnedImages are the gateway, actor and dashboard refs, or one error
// naming every variable still to set. Deploy and Verify call it before
// they touch the cluster.
func pinnedImages() (images, error) {
	var im images
	var missing []string
	for _, x := range []struct {
		ref      *string
		env, def string
	}{{&im.gateway, "OPENCLAW_GATEWAY_IMAGE", GatewayImage}, {&im.actor, "OPENCLAW_ACTOR_IMAGE", ActorImage},
		{&im.dashboard, "OPENCLAW_DASHBOARD_IMAGE", DashboardImage}} {
		ref, err := image(x.env, x.def)
		if err != nil {
			missing = append(missing, x.env)
		}
		*x.ref = ref
	}
	if len(missing) > 0 {
		return im, fmt.Errorf("the OpenClaw images are not pinned by digest: set %s to <image>@sha256:<digest> "+
			"(the gateway, actor and dashboard images published from csantanapr/always-on-agent)", strings.Join(missing, ", "))
	}
	return im, nil
}

// withWorker adds the ateworker-gvisor of the release the cluster runs
// (o.Release, a fork's when a plugin installed one), pinned by digest.
func withWorker(ctx context.Context, o plugin.Options, im images) (images, error) {
	var err error
	im.worker, err = pinImage(ctx, o.ImageRepo+"/ateworker-gvisor:"+o.Tag)
	return im, err
}

// whatsapp is the WhatsApp binding: WHATSAPP_PEER is one E.164 number
// allowed to message the agent; empty, no WhatsApp.
type whatsapp struct {
	install                    bool
	pluginsAllow, bindings, ch string
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

func whatsappFor(peer string) (whatsapp, error) {
	if peer == "" {
		return whatsapp{pluginsAllow: `["substrate"]`, bindings: `[]`, ch: `{}`}, nil
	}
	if !e164.MatchString(peer) {
		return whatsapp{}, fmt.Errorf("WHATSAPP_PEER %q is not an E.164 number (+15555550100)", peer)
	}
	b, _ := json.Marshal([]any{map[string]any{"type": "acp", "agentId": "main",
		"match": map[string]any{"channel": "whatsapp", "accountId": "default", "peer": map[string]any{"kind": "direct", "id": peer}},
		"acp":   map[string]any{"backend": "substrate"}}})
	return whatsapp{install: true, pluginsAllow: `["substrate","whatsapp"]`, bindings: string(b),
		ch: `{"whatsapp":{"enabled":true,"dmPolicy":"open","allowFrom":["*"]}}`}, nil
}

// render replaces placeholders in a manifest and fails on one left over.
func render(manifest string, r *strings.Replacer) ([]byte, error) {
	out := r.Replace(manifest)
	if i := strings.Index(out, "REPLACE_WITH_"); i >= 0 {
		end := strings.IndexAny(out[i:], " \"\n@")
		if end < 0 {
			end = len(out) - i
		}
		return nil, fmt.Errorf("placeholder %s is not filled in", out[i:i+end])
	}
	return []byte(out), nil
}

// compactJSON is s on one line, as the gateway config embeds it.
func compactJSON(s string) (string, error) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// rendered are the demo's manifests, filled in.
type rendered struct {
	workerPool, gatewayConfig, gateway, dashboard, actorTemplate []byte
	configSHA                                                    string
}

var workerImageLine = regexp.MustCompile(`(?m)^(  workerImage: ).*$`)

func renderAll(im images, token, replicas string, wa whatsapp) (rendered, error) {
	var r rendered
	egress, err := compactJSON(egressPolicyJSON)
	if err != nil {
		return r, fmt.Errorf("egress-policy.json: %w", err)
	}
	pool := workerImageLine.ReplaceAllLiteralString(workerPoolYAML, "  workerImage: "+im.worker)
	if r.workerPool, err = render(pool, strings.NewReplacer("REPLACE_WITH_WORKER_REPLICAS", replicas)); err != nil {
		return r, err
	}
	if r.gatewayConfig, err = render(gatewayConfigYAML, strings.NewReplacer(
		"REPLACE_WITH_PLUGINS_ALLOW_JSON", wa.pluginsAllow, "REPLACE_WITH_BINDINGS_JSON", wa.bindings,
		"REPLACE_WITH_CHANNELS_JSON", wa.ch, "REPLACE_WITH_EGRESS_POLICY_JSON", egress)); err != nil {
		return r, err
	}
	if err := validConfig(r.gatewayConfig); err != nil {
		return r, err
	}
	sum := sha256.Sum256(r.gatewayConfig)
	r.configSHA = hex.EncodeToString(sum[:])
	if r.gateway, err = render(gatewayYAML, strings.NewReplacer(
		"REPLACE_WITH_GATEWAY_IMAGE@REPLACE_WITH_GATEWAY_DIGEST", im.gateway, "REPLACE_WITH_CONFIG_SHA256", r.configSHA,
		"REPLACE_WITH_INSTALL_WHATSAPP", strconv.FormatBool(wa.install), "REPLACE_WITH_STORAGE_CLASS", StorageClass)); err != nil {
		return r, err
	}
	if r.dashboard, err = render(dashboardYAML, strings.NewReplacer("REPLACE_WITH_DASHBOARD_IMAGE@REPLACE_WITH_DASHBOARD_DIGEST", im.dashboard)); err != nil {
		return r, err
	}
	// The template carries the gateway token: it goes to kubectl-ate on
	// stdin and is never written to a file or printed.
	r.actorTemplate, err = render(actorTemplateYAML, strings.NewReplacer(
		"REPLACE_WITH_ACTOR_IMAGE@REPLACE_WITH_ACTOR_DIGEST", im.actor, "REPLACE_WITH_GATEWAY_TOKEN", token))
	return r, err
}

// validConfig checks the rendered gateway ConfigMap's openclaw.json is JSON.
func validConfig(cm []byte) error {
	n, err := yaml.Parse(string(cm))
	if err != nil {
		return fmt.Errorf("gateway-config.yaml: %w", err)
	}
	if !json.Valid([]byte(n.GetDataMap()["openclaw.json"])) {
		return fmt.Errorf("the rendered gateway openclaw.json is not valid JSON")
	}
	return nil
}

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,}$`)

// gatewayToken is the token in the openclaw-secrets Secret, kept across
// runs: the gateway reads it at start and the golden snapshot carries it.
// Without one, OPENCLAW_GATEWAY_TOKEN, else a new random token.
func gatewayToken(ctx context.Context, o plugin.Options) (string, error) {
	enc, err := efs.Out(ctx, o, "-n", Namespace, "get", "secret", secretName, "--ignore-not-found", "-o", "jsonpath={.data.gateway-token}")
	if err != nil {
		return "", err
	}
	tok := ""
	if enc = strings.TrimSpace(enc); enc != "" {
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return "", fmt.Errorf("secret %s/%s: the gateway token is not base64", Namespace, secretName)
		}
		tok = string(b)
	} else if tok = os.Getenv("OPENCLAW_GATEWAY_TOKEN"); tok == "" {
		if tok, err = newToken(); err != nil {
			return "", err
		}
	}
	// It is written into a quoted YAML string in the ActorTemplate.
	if !tokenRe.MatchString(tok) {
		return "", fmt.Errorf("the gateway token must be at least 16 characters of [A-Za-z0-9._~-]")
	}
	return tok, nil
}

// prerequisites warns, as deploy-demo.sh does, about what the demo will
// lack; a missing control plane stops it.
func prerequisites(ctx context.Context, w io.Writer, o plugin.Options) error {
	if _, err := efs.Out(ctx, o, "-n", "ate-system", "get", "deployment", "ate-api-server", "-o", "name"); err != nil {
		return fmt.Errorf("ate-api-server not found in ate-system: install the control plane first")
	}
	if s, _ := efs.Out(ctx, o, "-n", "llm-gateway", "get", "service", "llm-gateway", "--ignore-not-found", "-o", "name"); strings.TrimSpace(s) == "" {
		fmt.Fprintln(w, "WARNING: service llm-gateway/llm-gateway not found; the actors have no model until the LLM gateway is deployed")
	}
	if n, _ := efs.Out(ctx, o, "get", "nodes", "-l", "ate.dev/pool=gvisor", "-o", "name"); strings.TrimSpace(n) == "" {
		fmt.Fprintln(w, "WARNING: no nodes labeled ate.dev/pool=gvisor; the worker pods will stay Pending")
	}
	return nil
}

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	im, err := pinnedImages()
	if err != nil {
		return err
	}
	if err := prerequisites(ctx, w, o); err != nil {
		return err
	}
	if im, err = withWorker(ctx, o, im); err != nil {
		return err
	}
	wa, err := whatsappFor(os.Getenv("WHATSAPP_PEER"))
	if err != nil {
		return err
	}
	replicas := os.Getenv("OPENCLAW_WORKERS")
	if replicas == "" {
		replicas = "3"
	}
	if n, err := strconv.Atoi(replicas); err != nil || n < 1 {
		return fmt.Errorf("OPENCLAW_WORKERS must be a positive number, got %q", replicas)
	}
	if err := storage(ctx, w, o); err != nil {
		return err
	}
	tok, err := gatewayToken(ctx, o)
	if err != nil {
		return err
	}
	m, err := renderAll(im, tok, replicas, wa)
	if err != nil {
		return err
	}
	k, err := efs.BuildAte(ctx, w, o)
	if err != nil {
		return err
	}

	// Namespace, labeled for ate-api-server's NetworkPolicy, and the
	// gateway token, the deployment's only secret (no LLM key anywhere).
	fmt.Fprintln(w, "Namespace "+Namespace+" and the gateway token")
	ns := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    ate.dev/api-access: \"true\"\n", Namespace)
	if err := efs.Apply(ctx, w, o, []byte(ns)); err != nil {
		return err
	}
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\nstringData:\n  gateway-token: %q\n", secretName, Namespace, tok)
	if err := efs.Apply(ctx, w, o, []byte(secret)); err != nil {
		return err
	}

	fmt.Fprintf(w, "WorkerPool %s (%s gVisor workers, %s)\n", Pool, replicas, im.worker)
	if err := efs.Apply(ctx, w, o, m.workerPool); err != nil {
		return err
	}
	var ready string
	efs.Poll(ctx, 150, func() bool {
		ready, _ = efs.Out(ctx, o, "-n", Namespace, "get", "workerpool", Pool, "-o", "jsonpath={.status.readyReplicas}")
		n, _ := strconv.Atoi(strings.TrimSpace(ready))
		return n >= 1
	})
	fmt.Fprintf(w, "  workers ready: %s/%s\n", orZero(ready), replicas)

	if err := atespaceAndPolicy(ctx, w, k); err != nil {
		return err
	}
	if err := actorTemplate(ctx, w, k, im.actor, tok, m.actorTemplate); err != nil {
		return err
	}

	fmt.Fprintln(w, "Gateway")
	if err := efs.Apply(ctx, w, o, m.gatewayConfig); err != nil {
		return err
	}
	if err := efs.Apply(ctx, w, o, m.gateway); err != nil {
		return err
	}
	// On EFS the init container takes about 3 minutes.
	if err := efs.Run(ctx, w, o, "-n", Namespace, "rollout", "status", "deployment/openclaw-gateway", "--timeout=600s"); err != nil {
		return err
	}
	fmt.Fprintln(w, "Lifecycle dashboard")
	if err := efs.Apply(ctx, w, o, m.dashboard); err != nil {
		return err
	}
	if err := efs.Run(ctx, w, o, "-n", Namespace, "rollout", "status", "deployment/openclaw-dashboard", "--timeout=180s"); err != nil {
		return err
	}

	if err := waitGolden(ctx, w, k); err != nil {
		return err
	}
	if out, err := k.Do(ctx, nil, "create", "actor", Actor, "-a", Atespace, "--template", Template); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()+string(out)), "already exists") {
		return fmt.Errorf("create actor %s: %w", Actor, err)
	}
	fmt.Fprintf(w, "Actor %s ready\n", Actor)
	if err := egressPolicy(ctx, w, k, Actor); err != nil {
		return err
	}
	if wa.install {
		statusPings(ctx, w, o, os.Getenv("WHATSAPP_PEER"))
	}
	if err := tagGatewayAccessPoint(ctx, w, o); err != nil {
		return err
	}
	fmt.Fprintf(w, `Everything is ClusterIP; reach it with port-forwards:
  gateway UI:  kubectl --kubeconfig %[1]s -n %[2]s port-forward svc/openclaw-gateway 18789:18789   (http://localhost:18789)
  dashboard:   kubectl --kubeconfig %[1]s -n %[2]s port-forward svc/openclaw-dashboard 8090:8090    (http://localhost:8090)
  the actor:   kubectl ate --kubeconfig %[1]s proxy actor %[3]s -a %[4]s 8000:80
`, o.Kubeconfig, Namespace, Actor, Atespace)
	return nil
}

func orZero(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "0"
	}
	return s
}

// storage applies StorageClass efs-openclaw on the efs plugin's file system
// and moves an older gateway's claim off another class.
func storage(ctx context.Context, w io.Writer, o plugin.Options) error {
	fs, err := efs.Out(ctx, o, "get", "storageclass", efs.StorageClass, "--ignore-not-found", "-o", "jsonpath={.parameters.fileSystemId}")
	if err != nil {
		return err
	}
	if fs = strings.TrimSpace(fs); fs == "" {
		return fmt.Errorf("no StorageClass %s: deploy the efs plugin first (the gateway keeps its state on EFS)", efs.StorageClass)
	}
	sc, err := efs.Render(efs.StorageTmpl, efs.StorageArgs{Name: StorageClass, Mode: "efs-ap", FileSystem: fs, UID: gatewayUID, GID: gatewayUID, BasePath: "/openclaw"})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "StorageClass %s for the gateway's state (file system %s)\n", StorageClass, fs)
	if err := efs.ApplyStorageClass(ctx, w, o, StorageClass, fs, gatewayUID, sc); err != nil {
		return err
	}
	// A claim's StorageClass cannot change: a gateway from an older install
	// keeps its state on EBS. Replace that claim, which starts it fresh.
	have, err := efs.Out(ctx, o, "-n", Namespace, "get", "pvc", GatewayPVC, "--ignore-not-found", "-o", "jsonpath={.spec.storageClassName}")
	if err != nil {
		return err
	}
	if have = strings.TrimSpace(have); have != "" && have != StorageClass {
		fmt.Fprintf(w, "Moving the gateway's state from %s to %s: it starts with empty state (a WhatsApp link must be paired again)\n", have, StorageClass)
		if err := efs.Run(ctx, w, o, "-n", Namespace, "delete", "deployment", "openclaw-gateway", "--ignore-not-found", "--wait=true"); err != nil {
			return err
		}
		return efs.Run(ctx, w, o, "-n", Namespace, "delete", "pvc", GatewayPVC, "--wait=true")
	}
	return nil
}

// atespaceAndPolicy makes the atespace and lets the gateway's
// ServiceAccount edit it (create, suspend and delete actors and their egress
// policies), nothing outside it.
func atespaceAndPolicy(ctx context.Context, w io.Writer, k efs.Ate) error {
	if _, err := k.Do(ctx, nil, "get", "atespace", Atespace); err != nil {
		if _, err := k.Do(ctx, nil, "create", "atespace", Atespace); err != nil {
			return fmt.Errorf("create atespace %s: %w", Atespace, err)
		}
	}
	out, err := k.Do(ctx, nil, "get", "access-policy", "-a", Atespace, "-o", "json")
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		p, _ := json.Marshal(map[string]any{"metadata": map[string]string{"name": "default"},
			"bindings": []any{map[string]any{"role": "editor", "members": []string{gatewayPrincipal}}}})
		if _, err := k.Do(ctx, p, "create", "access-policy", "-a", Atespace, "-f", "-"); err != nil {
			return fmt.Errorf("create the access policy of %s: %w", Atespace, err)
		}
		fmt.Fprintf(w, "Access policy of %s: %s is editor\n", Atespace, gatewayPrincipal)
		return nil
	}
	updated, changed, err := grantEditor(out, gatewayPrincipal)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(w, "Access policy of %s already grants %s\n", Atespace, gatewayPrincipal)
		return nil
	}
	if _, err := k.Do(ctx, updated, "update", "access-policy", "-a", Atespace, "-f", "-"); err != nil {
		return fmt.Errorf("update the access policy of %s: %w", Atespace, err)
	}
	fmt.Fprintf(w, "Access policy of %s updated: %s is editor\n", Atespace, gatewayPrincipal)
	return nil
}

// grantEditor adds member to the policy's editor binding (or a new one),
// unless an editor or owner binding already has it. The rest of the policy,
// its uid and version among it, is kept for the update.
func grantEditor(policy []byte, member string) ([]byte, bool, error) {
	var p map[string]any
	if err := json.Unmarshal(policy, &p); err != nil {
		return nil, false, fmt.Errorf("read the access policy: %w", err)
	}
	bindings, _ := p["bindings"].([]any)
	var editor map[string]any
	for _, b := range bindings {
		bm, _ := b.(map[string]any)
		role, _ := bm["role"].(string)
		members, _ := bm["members"].([]any)
		if (role == "editor" || role == "owner") && slices.Contains(members, any(member)) {
			return policy, false, nil
		}
		if role == "editor" && editor == nil {
			editor = bm
		}
	}
	if editor != nil {
		members, _ := editor["members"].([]any)
		editor["members"] = append(members, member)
	} else {
		p["bindings"] = append(bindings, map[string]any{"role": "editor", "members": []any{member}})
	}
	b, err := json.Marshal(p)
	return b, true, err
}

// demoActor names the demo's own actors in its atespace: the oc-agent fleet
// the dashboard makes and one conv-<hash> actor per conversation.
var demoActor = regexp.MustCompile(`^(oc-agent(-[0-9]+)?|conv-[0-9a-f]{12})$`)

// actorTemplate keeps a template that runs this image with this token and
// has no golden error, which keeps its golden snapshot and every actor on
// it; otherwise it deletes the demo's actors and the template and makes it
// again (templates are immutable).
func actorTemplate(ctx context.Context, w io.Writer, k efs.Ate, img, tok string, manifest []byte) error {
	if out, err := k.Do(ctx, nil, "get", "actor-template", Template, "-a", Atespace, "-o", "json"); err == nil {
		if keep, why := keepTemplate(out, img, tok); keep {
			fmt.Fprintln(w, "Actor template unchanged; keeping it and its golden snapshot")
			return nil
		} else {
			fmt.Fprintf(w, "Replacing actor template %s (%s)\n", Template, why)
		}
		list, err := k.Do(ctx, nil, "get", "actors", "-a", Atespace, "-o", "json")
		if err != nil {
			return fmt.Errorf("list actors of %s: %w", Atespace, err)
		}
		for _, a := range actorNames(list) {
			if !demoActor.MatchString(a) {
				continue
			}
			_, _ = k.Do(ctx, nil, "suspend", "actor", a, "-a", Atespace)
			if _, err := k.Do(ctx, nil, "delete", "actor", a, "-a", Atespace); err != nil {
				fmt.Fprintf(w, "WARNING: could not delete actor %s: %v\n", a, err)
			} else {
				fmt.Fprintf(w, "  deleted actor %s (its template is being replaced)\n", a)
			}
		}
		if _, err := k.Do(ctx, nil, "delete", "actor-template", Template, "-a", Atespace); err != nil {
			return fmt.Errorf("delete actor-template %s: %w", Template, err)
		}
	}
	if _, err := k.Do(ctx, manifest, "create", "actor-template", "-f", "-"); err != nil {
		return fmt.Errorf("create actor-template %s: %w", Template, err)
	}
	fmt.Fprintf(w, "Actor template %s created\n", Template)
	return nil
}

// keepTemplate reports whether an existing template (`get actor-template -o
// json`) is the one to keep, and why not. Its token is compared, never shown.
func keepTemplate(t []byte, img, tok string) (bool, string) {
	var tm struct {
		Containers []struct {
			Image string `json:"image"`
			Env   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		} `json:"containers"`
		Status struct {
			Golden struct {
				Error string `json:"errorMessage"`
			} `json:"goldenSnapshotStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(t, &tm); err != nil || len(tm.Containers) == 0 {
		return false, "it does not read"
	}
	c := tm.Containers[0]
	if c.Image != img {
		return false, "another image"
	}
	for _, e := range c.Env {
		if e.Name == "OPENCLAW_GATEWAY_TOKEN" && e.Value != "" && e.Value != tok {
			return false, "another gateway token"
		}
	}
	if tm.Status.Golden.Error != "" {
		return false, "its golden snapshot failed"
	}
	return true, ""
}

func actorNames(list []byte) []string {
	var l struct {
		Actors []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"actors"`
	}
	if json.Unmarshal(list, &l) != nil {
		return nil
	}
	var out []string
	for _, a := range l.Actors {
		out = append(out, a.Metadata.Name)
	}
	return out
}

// waitGolden waits for the template's golden snapshot, which every actor
// restores from; a failed golden stops the deploy.
func waitGolden(ctx context.Context, w io.Writer, k efs.Ate) error {
	var tag, gerr string
	efs.Poll(ctx, goldenTries, func() bool {
		out, err := k.Do(ctx, nil, "get", "actor-template", Template, "-a", Atespace, "-o", "json")
		if err != nil {
			return false
		}
		var t struct {
			Status struct {
				Golden struct {
					Tag struct {
						Name string `json:"name"`
					} `json:"goldenTag"`
					Error string `json:"errorMessage"`
				} `json:"goldenSnapshotStatus"`
			} `json:"status"`
		}
		if json.Unmarshal(out, &t) != nil {
			return false
		}
		tag, gerr = t.Status.Golden.Tag.Name, t.Status.Golden.Error
		return tag != "" || gerr != ""
	})
	switch {
	case gerr != "":
		return fmt.Errorf("the golden snapshot of %s failed: %s", Template, gerr)
	case tag == "":
		return fmt.Errorf("the golden snapshot of %s is not ready after %d tries", Template, goldenTries)
	}
	fmt.Fprintln(w, "Golden snapshot ready: tag "+tag)
	return nil
}

// egressPolicy gives the actor the policy the gateway plugin gives every
// conversation actor: without one it has no outbound network at all.
func egressPolicy(ctx context.Context, w io.Writer, k efs.Ate, actor string) error {
	want, err := compactJSON(egressPolicyJSON)
	if err != nil {
		return err
	}
	cur, err := k.Do(ctx, nil, "get", "egress-policy", actor, "-a", Atespace, "-o", "json")
	if err != nil || len(strings.TrimSpace(string(cur))) == 0 {
		if _, err := k.Do(ctx, []byte(want), "create", "egress-policy", actor, "-a", Atespace, "-f", "-"); err != nil {
			return fmt.Errorf("create the egress policy of %s: %w", actor, err)
		}
		fmt.Fprintln(w, "Egress policy created for "+actor)
		return nil
	}
	var have, wantP map[string]any
	if err := json.Unmarshal(cur, &have); err != nil {
		return fmt.Errorf("read the egress policy of %s: %w", actor, err)
	}
	_ = json.Unmarshal([]byte(want), &wantP)
	hr, _ := json.Marshal(have["rules"])
	wr, _ := json.Marshal(wantP["rules"])
	if string(hr) == string(wr) {
		fmt.Fprintln(w, "Egress policy of "+actor+" is current")
		return nil
	}
	// An update needs the stored uid and version, which get printed.
	have["rules"] = wantP["rules"]
	b, _ := json.Marshal(have)
	if _, err := k.Do(ctx, b, "update", "egress-policy", actor, "-a", Atespace, "-f", "-"); err != nil {
		return fmt.Errorf("update the egress policy of %s: %w", actor, err)
	}
	fmt.Fprintln(w, "Egress policy updated for "+actor)
	return nil
}

// statusPings adds a cron job on the always-on gateway agent that sends the
// WhatsApp peer a one-line status every 30 minutes. One that exists is left.
func statusPings(ctx context.Context, w io.Writer, o plugin.Options, peer string) {
	err := efs.Run(ctx, w, o, "-n", Namespace, "exec", "deploy/openclaw-gateway", "-c", "gateway", "--",
		"node", "/app/openclaw.mjs", "cron", "add", "--name", "status-30m", "--every", "30m", "--agent", "main",
		"--model", "llmgw/claude", "--light-context", "--channel", "whatsapp", "--to", peer,
		"--message", "Reply with a one-line system status.", "--announce", "--expect-final")
	if err != nil {
		fmt.Fprintln(w, "  status-30m may already exist; continuing")
	}
}

// gatewayAccessPoint is the EFS access point behind the gateway's claim.
func gatewayAccessPoint(ctx context.Context, o plugin.Options) (string, error) {
	out, err := efs.Out(ctx, o, "-n", Namespace, "get", "pvc", GatewayPVC, "-o", "jsonpath={.status.phase} {.spec.storageClassName} {.spec.volumeName}")
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) != 3 || f[0] != "Bound" || f[1] != StorageClass {
		return "", fmt.Errorf("claim %s/%s is %q, not Bound on %s", Namespace, GatewayPVC, out, StorageClass)
	}
	h, err := efs.Out(ctx, o, "get", "pv", f[2], "-o", "jsonpath={.spec.csi.driver} {.spec.csi.volumeHandle}")
	if err != nil {
		return "", err
	}
	p := strings.Fields(h)
	if len(p) != 2 || p[0] != "efs.csi.aws.com" || !strings.Contains(p[1], "::fsap-") {
		return "", fmt.Errorf("volume %s is %q, not an EFS access point", f[2], h)
	}
	return p[1][strings.LastIndex(p[1], "::")+2:], nil
}

// tagGatewayAccessPoint adds the owner tag and TAGS to the gateway's access
// point: the add-on's controller makes it with only the driver's tag (its
// add-on configuration takes no tags), so it is tagged after.
func tagGatewayAccessPoint(ctx context.Context, w io.Writer, o plugin.Options) error {
	ap, err := gatewayAccessPoint(ctx, o)
	if err != nil {
		return err
	}
	tags, err := o.OwnerTags()
	if err != nil {
		return err
	}
	args := []string{"efs", "tag-resource", "--resource-id", ap, "--tags"}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		v := tags[k]
		if strings.ContainsAny(k+v, ",= ") {
			return fmt.Errorf("tag %s=%s cannot be written in the CLI's shorthand", k, v)
		}
		args = append(args, "Key="+k+",Value="+v)
	}
	if _, err := efs.AWS(ctx, o, args...); err != nil {
		return fmt.Errorf("tag access point %s: %w", ap, err)
	}
	fmt.Fprintf(w, "Tagged the gateway's access point %s with the owner tag\n", ap)
	return nil
}

// gatewayCheck is run in the gateway with node: the plugin's own client
// reaches the demo actor over atenet-router's mTLS listener with the
// gateway's pod certificate, the path a chat turn takes. /readyz wakes the
// actor without a model call.
const gatewayCheck = `
const { fetchActor } = await import('/app/extensions/substrate/dist/actor-router.js');
const r = await fetchActor({ name: '` + Actor + `', atespace: '` + Atespace + `' }, '/readyz');
console.log('actor ` + Actor + ` /readyz through atenet-router:', r.status);
if (r.status !== 200) process.exit(1);
`

// verify checks the demo natively, in about 2 minutes (the first wake of the
// actor restores it from its golden snapshot): the router admits the
// gateway's and dashboard's SPIFFE IDs, every Service is ClusterIP and there
// is no Ingress, the gateway's state is on an owner-tagged EFS access point,
// the gateway answers /healthz and /readyz over kubectl port-forward, and
// the gateway reaches the demo actor through atenet-router. It sends no
// model turn, so it needs no LLM gateway.
func verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	im, err := pinnedImages()
	if err != nil {
		return err
	}
	args, err := efs.Out(ctx, o, "-n", "ate-system", "get", "deployment", "atenet-router", "-o", "jsonpath={.spec.template.spec.containers[*].args}")
	if err != nil {
		return err
	}
	for _, id := range []string{GatewayID, DashboardID} {
		if !strings.Contains(args, id) {
			return fmt.Errorf("atenet-router does not admit %s: run --deploy-control-plane --plugins %s", id, ID)
		}
	}
	fmt.Fprintln(w, "PASS: atenet-router admits the gateway's and dashboard's SPIFFE IDs")

	if err := exposure(ctx, o); err != nil {
		return err
	}
	fmt.Fprintln(w, "PASS: every Service in namespace openclaw is ClusterIP, and there is no Ingress")

	for _, d := range []struct{ name, timeout, image string }{
		{"openclaw-gateway", "600s", im.gateway}, {"openclaw-dashboard", "180s", im.dashboard}} {
		if err := efs.Run(ctx, w, o, "-n", Namespace, "rollout", "status", "deployment/"+d.name, "--timeout="+d.timeout); err != nil {
			return err
		}
		got, err := efs.Out(ctx, o, "-n", Namespace, "get", "deployment", d.name, "-o", "jsonpath={.spec.template.spec.containers[*].image}")
		if err != nil {
			return err
		}
		if !slices.Contains(strings.Fields(got), d.image) {
			return fmt.Errorf("deployment %s/%s runs %q, not the pinned %s: deploy the plugin again", Namespace, d.name, got, d.image)
		}
	}
	fmt.Fprintln(w, "PASS: the gateway and dashboard run the pinned images")
	ap, err := gatewayAccessPoint(ctx, o)
	if err != nil {
		return err
	}
	owner, err := efs.AWS(ctx, o, "efs", "describe-access-points", "--access-point-id", ap,
		"--query", "AccessPoints[0].Tags[?Key=='agent-substrate-on-eks/deployment'].Value | [0]", "--output", "text")
	if err != nil {
		return err
	}
	if want := o.Cluster + "." + o.Region; owner != want {
		return fmt.Errorf("the gateway's access point %s has owner tag %q, not %q", ap, owner, want)
	}
	fmt.Fprintf(w, "PASS: the gateway's state is on EFS access point %s, owner-tagged\n", ap)

	port, err := efs.FreePort()
	if err != nil {
		return err
	}
	stop, logs := efs.Start(ctx, plugin.Cmd{Argv: []string{"kubectl", "--kubeconfig", o.Kubeconfig, "-n", Namespace,
		"port-forward", "svc/openclaw-gateway", fmt.Sprintf("%d:18789", port)}})
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if !efs.Poll(ctx, 30, func() bool { _, err := efs.Get(ctx, base+"healthz", 5*time.Second); return err == nil }) {
		return fmt.Errorf("the gateway's /healthz did not answer over port-forward: %s", logs())
	}
	if _, err := efs.Get(ctx, base+"readyz", 5*time.Second); err != nil {
		return fmt.Errorf("the gateway's /readyz: %w", err)
	}
	stop()
	fmt.Fprintln(w, "PASS: the gateway answers /healthz and /readyz over kubectl port-forward")

	var last error
	if !efs.Poll(ctx, 10, func() bool {
		last = efs.Run(ctx, w, o, "-n", Namespace, "exec", "deploy/openclaw-gateway", "-c", "gateway", "--",
			"node", "--input-type=module", "-e", gatewayCheck)
		return last == nil
	}) {
		return fmt.Errorf("the gateway could not reach actor %s through atenet-router (check --ingress-allowed-spiffe-ids): %w", Actor, last)
	}
	fmt.Fprintln(w, "PASS: the gateway reached actor "+Actor+" over router mTLS with its pod certificate")
	return nil
}

// exposure fails on a Service in the demo's namespace that is not ClusterIP,
// or on any Ingress there: the demo is reached with port-forward only.
func exposure(ctx context.Context, o plugin.Options) error {
	out, err := efs.Out(ctx, o, "-n", Namespace, "get", "services", "-o", "json")
	if err != nil {
		return err
	}
	var svcs struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Type string `json:"type"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &svcs); err != nil {
		return fmt.Errorf("read the services of %s: %w", Namespace, err)
	}
	if len(svcs.Items) == 0 {
		return fmt.Errorf("namespace %s has no Service", Namespace)
	}
	for _, s := range svcs.Items {
		if s.Spec.Type != "ClusterIP" {
			return fmt.Errorf("service %s/%s is %s: the demo must be reached with kubectl port-forward only", Namespace, s.Metadata.Name, s.Spec.Type)
		}
	}
	ing, err := efs.Out(ctx, o, "-n", Namespace, "get", "ingress", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(ing) != "" {
		return fmt.Errorf("namespace %s has an Ingress (%s): the demo must not be exposed", Namespace, strings.TrimSpace(ing))
	}
	return nil
}

// remove deletes the demo: its actors and their egress policies, the
// template and the atespace, then the namespace (its claim, and with it the
// gateway's access point), the cluster roles, the dashboard's role in
// ate-system, and the StorageClass.
func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if k, err := efs.BuildAte(ctx, w, o); err != nil {
		fmt.Fprintf(w, "WARNING: %v; leaving atespace %s to the control plane's removal\n", err, Atespace)
	} else if err := removeActors(ctx, w, k); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"delete", "namespace", Namespace, "--ignore-not-found", "--timeout=10m"},
		{"delete", "clusterrolebinding,clusterrole", "openclaw-gateway-eks", "openclaw-dashboard", "--ignore-not-found"},
		{"-n", "ate-system", "delete", "rolebinding,role", "openclaw-dashboard-mint-ate-token", "--ignore-not-found"},
	} {
		if err := efs.Run(ctx, w, o, args...); err != nil {
			return err
		}
	}
	// The claim's volume goes after the claim; wait for it, so its access
	// point is gone before the file system is.
	var left string
	if !efs.Poll(ctx, 60, func() bool {
		left, _ = efs.Out(ctx, o, "get", "pv", "-o", `jsonpath={.items[?(@.spec.storageClassName=="`+StorageClass+`")].metadata.name}`)
		return strings.TrimSpace(left) == ""
	}) {
		fmt.Fprintf(w, "WARNING: volume %s of StorageClass %s is still there\n", strings.TrimSpace(left), StorageClass)
	}
	return efs.DeleteStorageClass(ctx, w, o, StorageClass)
}

func removeActors(ctx context.Context, w io.Writer, k efs.Ate) error {
	out, err := k.Do(ctx, nil, "get", "actors", "-a", Atespace, "-o", "json")
	if err != nil {
		fmt.Fprintf(w, "No atespace %s\n", Atespace)
		return nil
	}
	for _, n := range actorNames(out) {
		if _, err := k.Do(ctx, nil, "get", "egress-policy", n, "-a", Atespace); err == nil {
			if _, err := k.Do(ctx, nil, "delete", "egress-policy", n, "-a", Atespace); err != nil {
				return fmt.Errorf("delete egress policy %s/%s: %w", Atespace, n, err)
			}
		}
		if _, err := k.Do(ctx, nil, "delete", "actor", n, "-a", Atespace); err != nil {
			return fmt.Errorf("delete actor %s/%s: %w", Atespace, n, err)
		}
		fmt.Fprintf(w, "Deleted actor %s/%s\n", Atespace, n)
	}
	if _, err := k.Do(ctx, nil, "get", "actor-template", Template, "-a", Atespace); err == nil {
		if _, err := k.Do(ctx, nil, "delete", "actor-template", Template, "-a", Atespace); err != nil {
			return fmt.Errorf("delete actor-template %s/%s: %w", Atespace, Template, err)
		}
	}
	if _, err := k.Do(ctx, nil, "get", "atespace", Atespace); err != nil {
		return nil // not there
	}
	if _, err := k.Do(ctx, nil, "delete", "atespace", Atespace); err != nil {
		fmt.Fprintf(w, "WARNING: delete atespace %s: %v\n", Atespace, err)
	} else {
		fmt.Fprintf(w, "Deleted atespace %s\n", Atespace)
	}
	return nil
}
