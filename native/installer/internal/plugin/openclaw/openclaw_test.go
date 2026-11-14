package openclaw

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
)

const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testImages() images {
	return images{gateway: GatewayImage + digest, actor: ActorImage + digest, dashboard: DashboardImage + digest,
		worker: "ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0" + digest}
}

func TestImageMustBePinned(t *testing.T) {
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", "")
	if _, err := image("OPENCLAW_GATEWAY_IMAGE", GatewayImage); err == nil || !strings.Contains(err.Error(), "OPENCLAW_GATEWAY_IMAGE=<image>@sha256:") {
		t.Errorf("an unpinned default: %v", err)
	}
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", GatewayImage+":v1")
	if _, err := image("OPENCLAW_GATEWAY_IMAGE", GatewayImage+digest); err == nil {
		t.Error("accepted an override pinned by tag only")
	}
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", GatewayImage+":v1"+digest)
	if ref, err := image("OPENCLAW_GATEWAY_IMAGE", GatewayImage); err != nil || ref != GatewayImage+":v1"+digest {
		t.Errorf("a pinned override: %q, %v", ref, err)
	}
}

func TestRenderAll(t *testing.T) {
	wa, _ := whatsappFor("")
	m, err := renderAll(testImages(), "tok-0123456789abcdef", "3", wa)
	if err != nil {
		t.Fatal(err)
	}
	all := string(m.workerPool) + string(m.gatewayConfig) + string(m.gateway) + string(m.dashboard) + string(m.actorTemplate)
	if strings.Contains(all, "REPLACE_WITH_") {
		t.Error("a placeholder is left")
	}
	for name, c := range map[string]struct {
		got  []byte
		want []string
	}{
		"workerpool":     {m.workerPool, []string{"replicas: 3", "  workerImage: ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0" + digest}},
		"gateway":        {m.gateway, []string{"image: " + GatewayImage + digest, "storageClassName: " + StorageClass, m.configSHA, `value: "false"`, "type: ClusterIP"}},
		"dashboard":      {m.dashboard, []string{"image: " + DashboardImage + digest, "type: ClusterIP"}},
		"actor template": {m.actorTemplate, []string{"image: " + ActorImage + digest, `value: "tok-0123456789abcdef"`}},
		"config":         {m.gatewayConfig, []string{`"allow": ["substrate"]`, `"bindings": []`, `"egressPolicy": {"rules":[{"http":`}},
	} {
		for _, w := range c.want {
			if !strings.Contains(string(c.got), w) {
				t.Errorf("%s lacks %q", name, w)
			}
		}
	}
	for _, never := range []string{"type: LoadBalancer", "type: NodePort", "kind: Ingress"} {
		if strings.Contains(all, never) {
			t.Errorf("rendered %s", never)
		}
	}
	// WhatsApp binds one number, and the gateway installs its plugin.
	wa, err = whatsappFor("+15555550100")
	if err != nil {
		t.Fatal(err)
	}
	m, err = renderAll(testImages(), "tok-0123456789abcdef", "3", wa)
	if err != nil || !strings.Contains(string(m.gatewayConfig), `"id":"+15555550100"`) || !strings.Contains(string(m.gateway), `value: "true"`) {
		t.Errorf("WhatsApp: %v", err)
	}
	if _, err := whatsappFor("5555"); err == nil {
		t.Error("accepted a peer that is not E.164")
	}
}

func TestRenderRefusesAPlaceholderLeft(t *testing.T) {
	if _, err := render("image: REPLACE_WITH_NEW_IMAGE@x\n", strings.NewReplacer()); err == nil || !strings.Contains(err.Error(), "REPLACE_WITH_NEW_IMAGE") {
		t.Errorf("got %v", err)
	}
	if err := validConfig([]byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  openclaw.json: |\n    {\"a\": [1,}\n")); err == nil {
		t.Error("accepted a config that is not JSON")
	}
}

func TestGrantEditor(t *testing.T) {
	p := []byte(`{"metadata":{"name":"default","uid":"u"},"bindings":[{"role":"viewer","members":["user:x"]}]}`)
	out, changed, err := grantEditor(p, gatewayPrincipal)
	if err != nil || !changed || !strings.Contains(string(out), `{"members":["`+gatewayPrincipal+`"],"role":"editor"}`) || !strings.Contains(string(out), `"uid":"u"`) {
		t.Errorf("no editor binding: %s %v %v", out, changed, err)
	}
	p = []byte(`{"bindings":[{"role":"editor","members":["user:x"]}]}`)
	if out, changed, _ = grantEditor(p, gatewayPrincipal); !changed || !strings.Contains(string(out), `"members":["user:x","`+gatewayPrincipal+`"]`) {
		t.Errorf("an editor binding: %s", out)
	}
	p = []byte(`{"bindings":[{"role":"owner","members":["` + gatewayPrincipal + `"]}]}`)
	if _, changed, _ = grantEditor(p, gatewayPrincipal); changed {
		t.Error("changed a policy that already grants it")
	}
}

func TestKeepTemplate(t *testing.T) {
	img := ActorImage + digest
	tmpl := func(image, tok, gerr string) []byte {
		return []byte(fmt.Sprintf(`{"containers":[{"image":%q,"env":[{"name":"OPENCLAW_GATEWAY_TOKEN","value":%q}]}],"status":{"goldenSnapshotStatus":{"errorMessage":%q}}}`, image, tok, gerr))
	}
	if keep, why := keepTemplate(tmpl(img, "tok", ""), img, "tok"); !keep {
		t.Errorf("the same template: %s", why)
	}
	for why, t2 := range map[string][]byte{
		"another image": tmpl(ActorImage+"@sha256:"+strings.Repeat("f", 64), "tok", ""), "another gateway token": tmpl(img, "old", ""),
		"its golden snapshot failed": tmpl(img, "tok", "boom"),
	} {
		if keep, got := keepTemplate(t2, img, "tok"); keep || got != why {
			t.Errorf("%s: keep=%v why=%q", why, keep, got)
		}
	}
}

// fakeExec answers kubectl reads by argv prefix and fails on a shell.
func fakeExec(t *testing.T, answers map[string]string) *[]string {
	var log []string
	prev, prevWait := plugin.Exec, efs.Wait
	efs.Wait = 0
	plugin.Exec = func(_ context.Context, w io.Writer, c plugin.Cmd) error {
		argv := strings.Join(c.Argv, " ")
		for _, a := range c.Argv {
			if a == "bash" || a == "sh" || a == "-c" {
				return fmt.Errorf("a shell ran: %s", argv)
			}
		}
		log = append(log, argv)
		for p, a := range answers {
			if strings.HasPrefix(argv, p) && c.Stdout != nil {
				io.WriteString(c.Stdout, a)
			}
		}
		return nil
	}
	t.Cleanup(func() { plugin.Exec, efs.Wait = prev, prevWait })
	return &log
}

func TestExposure(t *testing.T) {
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc"}
	svc := func(types ...string) string {
		var items []string
		for i, ty := range types {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":"s%d"},"spec":{"type":%q}}`, i, ty))
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`
	}
	fakeExec(t, map[string]string{"kubectl --kubeconfig /kc -n openclaw get services": svc("ClusterIP", "ClusterIP")})
	if err := exposure(context.Background(), o); err != nil {
		t.Errorf("ClusterIP only: %v", err)
	}
	for _, ty := range []string{"NodePort", "LoadBalancer"} {
		fakeExec(t, map[string]string{"kubectl --kubeconfig /kc -n openclaw get services": svc("ClusterIP", ty)})
		if err := exposure(context.Background(), o); err == nil || !strings.Contains(err.Error(), ty) {
			t.Errorf("%s: %v", ty, err)
		}
	}
	fakeExec(t, map[string]string{"kubectl --kubeconfig /kc -n openclaw get services": svc("ClusterIP"),
		"kubectl --kubeconfig /kc -n openclaw get ingress": "ingress.networking.k8s.io/open\n"})
	if err := exposure(context.Background(), o); err == nil || !strings.Contains(err.Error(), "Ingress") {
		t.Errorf("an Ingress: %v", err)
	}
}

// recordExec records every child and fails each one, so a hook stops at
// its first child: what it ran shows whether it got past the image check.
func recordExec(t *testing.T) *[]string {
	var log []string
	prev := plugin.Exec
	plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
		log = append(log, strings.Join(c.Argv, " "))
		return fmt.Errorf("stop")
	}
	t.Cleanup(func() { plugin.Exec = prev })
	return &log
}

var imageVars = []string{"OPENCLAW_GATEWAY_IMAGE", "OPENCLAW_ACTOR_IMAGE", "OPENCLAW_DASHBOARD_IMAGE"}

// Without the digests, Deploy and Verify refuse naming all three variables
// and start no child: the cluster is never reached.
func TestHooksRefuseUnpinnedImagesBeforeTheCluster(t *testing.T) {
	for _, e := range imageVars {
		t.Setenv(e, "")
	}
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", Tag: "v1.0.0"}
	for name, hook := range map[string]func(context.Context, io.Writer, plugin.Options) error{"deploy": deploy, "verify": verify} {
		log := recordExec(t)
		err := hook(context.Background(), io.Discard, o)
		if err == nil || !strings.Contains(err.Error(), "set OPENCLAW_GATEWAY_IMAGE, OPENCLAW_ACTOR_IMAGE, OPENCLAW_DASHBOARD_IMAGE to <image>@sha256:<digest>") {
			t.Errorf("%s: %v", name, err)
		}
		if len(*log) > 0 {
			t.Errorf("%s ran %v before refusing", name, *log)
		}
	}
	// One left unpinned is named alone.
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", GatewayImage+digest)
	t.Setenv("OPENCLAW_DASHBOARD_IMAGE", DashboardImage+digest)
	if _, err := pinnedImages(); err == nil || !strings.Contains(err.Error(), "set OPENCLAW_ACTOR_IMAGE to") {
		t.Errorf("one missing: %v", err)
	}
}

// With the three set, Deploy and Verify go on to the cluster.
func TestHooksProceedWithPinnedImages(t *testing.T) {
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", GatewayImage+digest)
	t.Setenv("OPENCLAW_ACTOR_IMAGE", ActorImage+digest)
	t.Setenv("OPENCLAW_DASHBOARD_IMAGE", DashboardImage+digest)
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", Tag: "v1.0.0"}
	for name, first := range map[string]string{
		"deploy": "kubectl --kubeconfig /kc -n ate-system get deployment ate-api-server",
		"verify": "kubectl --kubeconfig /kc -n ate-system get deployment atenet-router",
	} {
		log := recordExec(t)
		hook := map[string]func(context.Context, io.Writer, plugin.Options) error{"deploy": deploy, "verify": verify}[name]
		if err := hook(context.Background(), io.Discard, o); err == nil || strings.Contains(err.Error(), "not pinned") {
			t.Errorf("%s: %v", name, err)
		}
		if len(*log) == 0 || !strings.HasPrefix((*log)[0], first) {
			t.Errorf("%s: first child %v, want %s", name, *log, first)
		}
	}
}
