package credprovideraws

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/llmgateway"
)

// Names shared with setup-aws's plugin_credprovider_aws.go and the provider's
// own manifests.
const (
	ID              = "credprovider-aws"
	ns              = "ate-system"
	sa              = "aws-sm-credential-provider"
	deploy          = "deploy/" + sa
	policyCM        = "aws-sm-credential-provider-secret-policy"
	ProviderName    = "secretsmanager.amazonaws.com"
	ProviderAddress = sa + "." + ns + ".svc:50051"
	// Actors in atespace may resolve its secrets; deniedAtespace is in no
	// grant of the provider's policy. Both use one gVisor WorkerPool in
	// poolNS.
	atespace       = "ate-demo-credprovider"
	deniedAtespace = "ate-demo-credprovider-denied"
	poolNS         = "ate-demo-credprovider"
	template       = "probe"
	injectableTag  = "ate.dev/injectable"
	// policySHA is the pod annotation that rolls the provider when its policy
	// changes (it reads the policy only at start).
	policySHA = "agent-substrate-on-eks/secret-policy-sha256"
	// releaseImages is where Substrate releases publish the worker images,
	// main.go's image repo.
	releaseImages = "ghcr.io/agent-substrate/substrate"
	poolLabel     = "ate.dev/pool"
)

// providerJSON is the --credential-provider object the control plane install
// adds after the bundled k8s.io one.
var providerJSON = fmt.Sprintf(`{"name":%q,"address":%q}`, ProviderName, ProviderAddress)

var (
	//go:embed secret-policy.yaml
	secretPolicy string
	//go:embed probe-pool.yaml
	probePool string
	//go:embed probe-template.yaml
	probeTemplate string
)

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Secrets Manager credential provider",
		Summary: "Egress credential injection from AWS Secrets Manager: actors send a placeholder, the egress " +
			"gateway swaps in the secret their EgressPolicy names, and the actor never holds it",
		Order:    40,
		Minutes:  12,
		SetupAWS: true,
		Needs:    plugin.Needs{CredentialProviders: []string{providerJSON}},
		Deploy:   Deploy,
		Verify:   Verify,
		Remove:   Remove,
	})
}

// secretNames are setup-aws's cpSecretNames: the injectable secret and the
// one without the tag, each carrying the cluster name.
func secretNames(cluster string) (injectable, untagged string) {
	p := "ate/" + atespace + "/" + cluster
	return p + "-demo-token", p + "-untagged-token"
}

// caller is the account and partition the plugin runs in. Replaced in tests.
var caller = func(ctx context.Context, o plugin.Options) (aws.Config, string, string, error) {
	ac, err := loadConfig(ctx, o)
	if err != nil {
		return aws.Config{}, "", "", err
	}
	id, err := sts.NewFromConfig(ac).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return aws.Config{}, "", "", fmt.Errorf("AWS credentials: %w", err)
	}
	parts := strings.Split(aws.ToString(id.Arn), ":")
	if len(parts) < 5 {
		return aws.Config{}, "", "", errors.New("unexpected caller ARN")
	}
	return ac, aws.ToString(id.Account), parts[1], nil
}

func loadConfig(ctx context.Context, o plugin.Options) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
	if o.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.Profile))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// pause waits d; replaced in tests.
var pause = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ---- kubectl-ate ----------------------------------------------------------

// ate runs the release's kubectl-ate against the installer's kubeconfig only.
type ate struct {
	bin string
	o   plugin.Options
}

// kubectlAte builds kubectl-ate from the cached tree into the installer's
// cache (the path the counter demo uses), fetching the tree when it is gone.
func kubectlAte(ctx context.Context, w io.Writer, o plugin.Options) (ate, error) {
	tree := controlplane.TreeDir(o.TreeRoot, o.Tag)
	if _, err := os.Stat(filepath.Join(tree, "cmd", "kubectl-ate")); err != nil {
		// shortcut: the default release repository; a plugin that replaces
		// the release (mode 3) would need its Source here.
		if err := controlplane.FetchTree(ctx, w, tree, controlplane.DefaultRepo, o.Tag); err != nil {
			return ate{}, err
		}
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	bin := filepath.Join(dir, "substrate-eks", "bin", "kubectl-ate-"+o.Tag)
	if err := plugin.Exec(ctx, w, plugin.Cmd{Dir: tree, Env: treeGoEnv, Argv: []string{"go", "build", "-o", bin, "./cmd/kubectl-ate"}}); err != nil {
		return ate{}, fmt.Errorf("build kubectl-ate: %w", err)
	}
	return ate{bin: bin, o: o}, nil
}

func (k ate) cmd(stdin []byte, stdout io.Writer, args ...string) plugin.Cmd {
	return plugin.Cmd{Env: []string{"KUBECONFIG=" + k.o.Kubeconfig}, Stdin: stdin, Stdout: stdout,
		Argv: append(append([]string{k.bin}, args...), "--kubeconfig", k.o.Kubeconfig)}
}

// out runs kubectl-ate and returns its stdout; its stderr goes into the error.
func (k ate) out(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	var out, errb bytes.Buffer
	if err := plugin.Exec(ctx, &errb, k.cmd(stdin, &out, args...)); err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// actorNames are the actors in an atespace.
func (k ate) actorNames(ctx context.Context, space string) ([]string, error) {
	b, err := k.out(ctx, nil, "get", "actors", "-a", space, "-o", "json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Actors []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"actors"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("read actors of %s: %w", space, err)
	}
	var out []string
	for _, a := range l.Actors {
		out = append(out, a.Metadata.Name)
	}
	return out, nil
}

// ---- Deploy ---------------------------------------------------------------

// Deploy is demos/credprovider-aws/deploy.sh after its AWS part: the images,
// the secret policy, the provider (mTLS, egress gateway only), then the probe
// atespaces Verify uses. The control plane install already registered the
// provider with the egress gateway (Needs).
func Deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	ac, account, partition, err := caller(ctx, o)
	if err != nil {
		return err
	}
	k, err := kubectlAte(ctx, w, o)
	if err != nil {
		return err
	}
	arch, err := nodeArch(ctx, w, o)
	if err != nil {
		return err
	}
	provider, probe, err := images(ctx, w, o, ac, arch)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "  provider image "+provider)
	fmt.Fprintln(w, "  probe image "+probe)

	policy, err := renderPolicy(account, o.Region)
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, []byte(policy), nil, "apply", "-f", "-"); err != nil {
		return err
	}
	var base bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &base, "kustomize", filepath.Join(sourceDir(o.TreeRoot), "config")); err != nil {
		return err
	}
	role := fmt.Sprintf("arn:%s:iam::%s:role/%s-%s-%s", partition, account, o.Cluster, o.Region, sa)
	m, err := providerManifest(base.Bytes(), provider, o.Region, role, sha(policy))
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, m, nil, "apply", "-f", "-"); err != nil {
		return err
	}
	// A pod that started before the association applied exits at startup (no
	// Pod Identity credentials); EnsurePodIdentity restarts it.
	if err := plugin.Kubectl(ctx, io.Discard, o, nil, nil, "-n", ns, "rollout", "status", deploy, "--timeout=2m"); err != nil {
		fmt.Fprintln(w, "  not ready yet; checking its Pod Identity credentials")
	}
	if err := llmgateway.EnsurePodIdentity(ctx, w, o, ns, deploy, "app="+sa); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", ns, "rollout", "status", deploy, "--timeout=5m"); err != nil {
		return err
	}
	if err := gatewayServes(ctx, o); err != nil {
		return err
	}
	fmt.Fprintf(w, "  atenet-egress serves k8s.io and %s (%s)\n", ProviderName, ProviderAddress)

	worker, err := pinRelease(ctx, releaseImages+"/ateworker-gvisor:"+o.Tag)
	if err != nil {
		return err
	}
	pool := strings.ReplaceAll(probePool, "ko://go.ate.dev/substrate/cmd/ateworker-gvisor", worker)
	if err := plugin.Kubectl(ctx, w, o, []byte(pool), nil, "apply", "-f", "-"); err != nil {
		return err
	}
	for _, space := range []string{atespace, deniedAtespace} {
		if err := k.probeTemplate(ctx, w, space, probe); err != nil {
			return err
		}
	}
	return nil
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// renderPolicy is secret-policy.yaml for the account and Region; the account
// goes only into the cluster.
func renderPolicy(account, region string) (string, error) {
	out := strings.NewReplacer("${ACCOUNT}", account, "${AWS_REGION}", region).Replace(secretPolicy)
	if strings.Contains(out, "${") {
		return "", errors.New("secret-policy.yaml: a placeholder is left")
	}
	return out, nil
}

// nodeArch is the one CPU architecture of the system and gvisor nodes, where
// the provider and the probe actors run.
func nodeArch(ctx context.Context, w io.Writer, o plugin.Options) (string, error) {
	var out bytes.Buffer
	sel := poolLabel + " in (system,gvisor)"
	if err := plugin.Kubectl(ctx, w, o, nil, &out, "get", "nodes", "-l", sel, "-o", `jsonpath={.items[*].metadata.labels.kubernetes\.io/arch}`); err != nil {
		return "", err
	}
	archs := slices.Compact(slices.Sorted(slices.Values(strings.Fields(out.String()))))
	switch len(archs) {
	case 0:
		return "", fmt.Errorf("no node labeled %s", sel)
	case 1:
		return archs[0], nil
	}
	// shortcut: one single-architecture image per binary, as deploy.sh built;
	// push an index if the node groups ever need to differ.
	return "", fmt.Errorf("the nodes labeled %s are %s: the images are built for one architecture", sel, strings.Join(archs, " and "))
}

// providerManifest is the provider's kustomize base with deploy.sh's edits:
// the ECR image, this Region, its own role (it exits at startup on any
// other), the system node group and its taint, and the policy's hash, so a
// policy change rolls the pods. The base must have exactly one Deployment
// with one container on a ko:// image.
func providerManifest(base []byte, image, region, role, policySum string) ([]byte, error) {
	nodes, err := kio.FromBytes(base)
	if err != nil {
		return nil, fmt.Errorf("parse the provider manifests: %w", err)
	}
	var docs []string
	found := 0
	for _, n := range nodes {
		s, err := n.String()
		if err != nil {
			return nil, err
		}
		if n.GetKind() != "Deployment" {
			docs = append(docs, s)
			continue
		}
		var d map[string]any
		if err := yaml.Unmarshal([]byte(s), &d); err != nil {
			return nil, err
		}
		tpl, _ := dig(d, "spec", "template").(map[string]any)
		spec, _ := dig(d, "spec", "template", "spec").(map[string]any)
		if tpl == nil || spec == nil {
			return nil, errors.New("the provider Deployment has no pod template")
		}
		spec["nodeSelector"] = map[string]any{poolLabel: "system"}
		// The system group's taint (Auto Mode, setup-aws create cluster).
		spec["tolerations"] = []any{map[string]any{"key": "ate.dev/workloadType", "operator": "Equal",
			"value": "ate-system-infra", "effect": "NoSchedule"}}
		meta, _ := tpl["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			tpl["metadata"] = meta
		}
		ann, _ := meta["annotations"].(map[string]any)
		if ann == nil {
			ann = map[string]any{}
			meta["annotations"] = ann
		}
		ann[policySHA] = policySum
		cs, _ := spec["containers"].([]any)
		for _, x := range cs {
			c, _ := x.(map[string]any)
			if img, _ := c["image"].(string); !strings.HasPrefix(img, "ko://") {
				return nil, fmt.Errorf("the provider container's image is %q, not a ko:// reference", img)
			}
			c["image"] = image
			raw, _ := c["args"].([]any)
			var args []any
			hasRegion := false
			for _, a := range raw {
				s, _ := a.(string)
				switch {
				case strings.HasPrefix(s, "--region="):
					args, hasRegion = append(args, "--region="+region), true
				case strings.HasPrefix(s, "--expected-role-arn="):
				default:
					args = append(args, a)
				}
			}
			if !hasRegion {
				return nil, errors.New("no --region in the provider args")
			}
			c["args"] = append(args, "--expected-role-arn="+role)
			found++
		}
		b, err := yaml.Marshal(d)
		if err != nil {
			return nil, err
		}
		docs = append(docs, string(b))
	}
	if found != 1 {
		return nil, fmt.Errorf("want one provider container, found %d", found)
	}
	return []byte(strings.Join(docs, "---\n")), nil
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// gatewayServes checks that atenet-egress is configured with the provider:
// the control plane deploy carries it once the plugin is recorded.
func gatewayServes(ctx context.Context, o plugin.Options) error {
	var out bytes.Buffer
	if err := plugin.Kubectl(ctx, io.Discard, o, nil, &out, "-n", ns, "get", "deploy", "atenet-egress", "-o", "json"); err != nil {
		return err
	}
	ok, err := servesProvider(out.Bytes())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("atenet-egress does not serve %s: run --deploy-control-plane --plugins %s", ProviderName, ID)
	}
	return nil
}

// servesProvider reports whether a Deployment (JSON) has a container with
// the provider's --credential-provider-name argument.
func servesProvider(deployJSON []byte) (bool, error) {
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Args []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(deployJSON, &d); err != nil {
		return false, fmt.Errorf("read atenet-egress: %w", err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if slices.Contains(c.Args, "--credential-provider-name="+ProviderName) {
			return true, nil
		}
	}
	return false, nil
}

// probeTemplate makes the atespace and its probe ActorTemplate on image,
// replacing a template (and its actors) that runs another image, and waits
// for its golden snapshot.
func (k ate) probeTemplate(ctx context.Context, w io.Writer, space, image string) error {
	if _, err := k.out(ctx, nil, "create", "atespace", space); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("create atespace %s: %w", space, err)
	}
	if b, err := k.out(ctx, nil, "get", "actor-template", template, "-a", space, "-o", "json"); err == nil {
		have, err := templateImage(b)
		if err != nil {
			return fmt.Errorf("read actor-template %s/%s: %w", space, template, err)
		}
		if have == image {
			fmt.Fprintf(w, "  ActorTemplate %s/%s already runs this probe image\n", space, template)
			return k.waitGolden(ctx, w, space)
		}
		// Templates are immutable: replace it, with its actors (Verify makes new ones).
		fmt.Fprintf(w, "  replacing %s/%s (was %s)\n", space, template, have)
		names, err := k.actorNames(ctx, space)
		if err != nil {
			return err
		}
		for _, a := range names {
			if _, err := k.out(ctx, nil, "delete", "actor", a, "-a", space); err != nil {
				return err
			}
		}
		if _, err := k.out(ctx, nil, "delete", "actor-template", template, "-a", space); err != nil {
			return err
		}
	}
	t := strings.NewReplacer("REPLACE_WITH_ATESPACE", space, "REPLACE_WITH_PROBE_IMAGE", image).Replace(probeTemplate)
	if _, err := k.out(ctx, []byte(t), "create", "actor-template", "-f", "-"); err != nil {
		return fmt.Errorf("create actor-template %s/%s: %w", space, template, err)
	}
	fmt.Fprintf(w, "  created ActorTemplate %s/%s\n", space, template)
	return k.waitGolden(ctx, w, space)
}

// templateImage is the first container image of an ActorTemplate as
// `kubectl ate get actor-template -o json` prints it, with or without spec.
func templateImage(b []byte) (string, error) {
	type containers struct {
		Containers []struct {
			Image string `json:"image"`
		} `json:"containers"`
	}
	var t struct {
		containers
		Spec *containers `json:"spec"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return "", err
	}
	cs := t.Containers
	if t.Spec != nil {
		cs = t.Spec.Containers
	}
	if len(cs) == 0 {
		return "", errors.New("it has no container")
	}
	return cs[0].Image, nil
}

// goldenTries at goldenWait is deploy.sh's 15-minute wait for the golden
// snapshot new actors restore from.
const (
	goldenTries = 90
	goldenWait  = 10 * time.Second
)

func (k ate) waitGolden(ctx context.Context, w io.Writer, space string) error {
	last := "none"
	for i := 0; i < goldenTries; i++ {
		b, err := k.out(ctx, nil, "get", "actor-template", template, "-a", space, "-o", "json")
		if err == nil {
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
			if json.Unmarshal(b, &t) == nil {
				if t.Status.Golden.Tag.Name != "" {
					fmt.Fprintf(w, "  golden snapshot ready for %s/%s\n", space, template)
					return nil
				}
				if t.Status.Golden.Error != "" {
					last = t.Status.Golden.Error
				}
			}
		} else {
			last = err.Error()
		}
		if err := pause(ctx, goldenWait); err != nil {
			return err
		}
	}
	return fmt.Errorf("golden snapshot of %s/%s not ready (%s)", space, template, last)
}

// ---- Remove ---------------------------------------------------------------

// Remove is clean.sh's Kubernetes part: the probe actors, templates and
// atespaces, the worker pool's namespace, then the provider and its policy.
// Run then deletes the AWS half (setup-aws plugin credprovider-aws --delete).
// The egress gateway keeps naming the provider until the next control plane
// deploy, which no longer carries it (the record drops it); until then only
// requests for its URIs fail, as they would with no provider.
func Remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	k, err := kubectlAte(ctx, w, o)
	if err != nil {
		return err
	}
	for _, space := range []string{atespace, deniedAtespace} {
		if _, err := k.out(ctx, nil, "get", "atespace", space); err != nil {
			continue
		}
		names, err := k.actorNames(ctx, space)
		if err != nil {
			return err
		}
		for _, a := range names {
			if _, err := k.out(ctx, nil, "delete", "actor", a, "-a", space); err != nil {
				return err
			}
		}
		if _, err := k.out(ctx, nil, "get", "actor-template", template, "-a", space); err == nil {
			if _, err := k.out(ctx, nil, "delete", "actor-template", template, "-a", space); err != nil {
				return err
			}
		}
		// Refused while an actor remains; deleted actors take a moment to go.
		var derr error
		for i := 0; i < 24; i++ {
			if _, derr = k.out(ctx, nil, "delete", "atespace", space); derr == nil {
				break
			}
			if err := pause(ctx, 5*time.Second); err != nil {
				return err
			}
		}
		if derr != nil {
			return fmt.Errorf("delete atespace %s: %w", space, derr)
		}
		fmt.Fprintln(w, "  deleted atespace "+space)
	}
	for _, args := range [][]string{
		{"delete", "namespace", poolNS, "--ignore-not-found", "--timeout=5m"},
		{"-n", ns, "delete", "deployment,service,networkpolicy,poddisruptionbudget,serviceaccount", sa, "--ignore-not-found"},
		{"-n", ns, "delete", "configmap", policyCM, "--ignore-not-found"},
	} {
		if err := plugin.Kubectl(ctx, w, o, nil, nil, args...); err != nil {
			return err
		}
	}
	return nil
}
