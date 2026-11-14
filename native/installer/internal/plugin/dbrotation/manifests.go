package dbrotation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// The upstream releases the plugin installs, each its own published install
// manifest, pinned by sha256, and its image pinned by digest. External
// Secrets Operator's manifest is its Helm chart rendered with the defaults,
// in namespace default; transformESO keeps only the controller and moves it.
//
//	https://github.com/external-secrets/external-secrets/releases
//	https://github.com/external-secrets/reloader/releases
const (
	esoVersion = "v2.15.0"
	esoURL     = "https://github.com/external-secrets/external-secrets/releases/download/" + esoVersion + "/external-secrets.yaml"
	esoImage   = "ghcr.io/external-secrets/external-secrets:" + esoVersion

	reloaderVersion = "v0.3.0"
	reloaderURL     = "https://github.com/external-secrets/reloader/releases/download/" + reloaderVersion + "/bundle.yaml"
	reloaderImage   = "ghcr.io/external-secrets/reloader:" + reloaderVersion
)

// The release files' sha256 (tests pin their fixtures instead), and the
// image digests (the multi-arch index), read from ghcr.io for each tag.
var (
	esoSHA256      = "34cf25e951da2aeefe465e6767036272a1db1f713b9be6a1b7bc811ae9a739c2"
	reloaderSHA256 = "8d0c536d9e225b1b08b2ea4c437c14dd2f43856284116d7e927d2878c730e975"
	esoPinned      = esoImage + "@sha256:6888f243e5f8f7592c6d7fae2a5547c9e33531441b47b53807b9093b25a00e31"
	reloaderPinned = reloaderImage + "@sha256:94d3628a82996bc4e393aa87c97d96684f4041552ae38d2359a598cd41938a2d"
)

const (
	esoNS      = "external-secrets"
	reloaderNS = "external-secrets-reloader"
	// partOf marks every object the plugin applies from the two releases,
	// so Remove deletes exactly those.
	partOfKey   = "app.kubernetes.io/part-of"
	partOfValue = "substrate-eks-db-rotation"
	partOf      = partOfKey + "=" + partOfValue
	// The system node group: Auto Mode taints it, and the control plane runs
	// there (setup-aws create cluster makes it).
	poolKey       = "ate.dev/pool"
	poolValue     = "system"
	taintKey      = "ate.dev/workloadType"
	taintValue    = "ate-system-infra"
	esoDeployment = "external-secrets"
)

// fetch downloads url; replaced in tests.
var fetch = func(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// pinned downloads url and checks it against sum: a changed release file
// stops the deploy instead of being applied.
func pinned(ctx context.Context, url, sum string) ([]byte, error) {
	b, err := fetch(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	h := sha256.Sum256(b)
	if got := hex.EncodeToString(h[:]); got != sum {
		return nil, fmt.Errorf("%s has sha256 %s, not the pinned %s; refusing to apply it", url, got, sum)
	}
	return b, nil
}

// dropped are the parts of ESO's manifest the plugin does not run: the
// admission webhook and the controller that makes its certificate. Nothing
// is then listening but the metrics port, and nothing calls into the
// cluster's admission path.
func dropped(n *yaml.RNode) bool {
	name := n.GetName()
	switch n.GetKind() {
	case "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration":
		return true
	}
	return strings.HasPrefix(name, "external-secrets-webhook") || strings.HasPrefix(name, "external-secrets-cert-controller")
}

// transformESO is ESO's release manifest as the plugin applies it: the
// controller only, in its own namespace, its image pinned by digest, on the
// system node group, reading only namespaced SecretStores and with
// PushSecret off (the plugin only reads from Secrets Manager), and with the
// EC2 instance metadata service disabled so it uses Pod Identity and never
// the node's role.
func transformESO(manifest []byte) (string, error) {
	nodes, err := kio.FromBytes(manifest)
	if err != nil {
		return "", fmt.Errorf("parse the External Secrets manifest: %w", err)
	}
	var keep []*yaml.RNode
	deployments := 0
	for _, n := range nodes {
		if dropped(n) {
			continue
		}
		if k := n.GetKind(); k == "Service" || k == "Secret" {
			return "", fmt.Errorf("the External Secrets manifest has a %s %s the plugin does not know", k, n.GetName())
		}
		if err := common(n, esoNS); err != nil {
			return "", err
		}
		if n.GetKind() == "Deployment" {
			if n.GetName() != esoDeployment {
				return "", fmt.Errorf("the External Secrets manifest has a Deployment %s the plugin does not know", n.GetName())
			}
			deployments++
			if err := pinContainer(n, esoImage, esoPinned); err != nil {
				return "", err
			}
			for _, a := range []string{"--enable-cluster-store-reconciler=false", "--enable-cluster-push-secret-reconciler=false", "--enable-push-secret-reconciler=false"} {
				if err := appendSeq(n, "args", yaml.NewStringRNode(a)); err != nil {
					return "", err
				}
			}
			env, err := yaml.Parse("{name: AWS_EC2_METADATA_DISABLED, value: \"true\"}")
			if err != nil {
				return "", err
			}
			if err := appendSeq(n, "env", env); err != nil {
				return "", err
			}
		}
		keep = append(keep, n)
	}
	if deployments != 1 {
		return "", fmt.Errorf("the External Secrets manifest has %d controller Deployments, want 1", deployments)
	}
	return kio.StringAll(keep)
}

// transformReloader is the Reloader bundle with its image pinned by digest,
// on the system node group, and labeled.
func transformReloader(manifest []byte) (string, error) {
	nodes, err := kio.FromBytes(manifest)
	if err != nil {
		return "", fmt.Errorf("parse the Reloader bundle: %w", err)
	}
	deployments := 0
	for _, n := range nodes {
		if err := common(n, reloaderNS); err != nil {
			return "", err
		}
		if n.GetKind() == "Deployment" {
			deployments++
			if err := pinContainer(n, reloaderImage, reloaderPinned); err != nil {
				return "", err
			}
		}
	}
	if deployments != 1 {
		return "", fmt.Errorf("the Reloader bundle has %d Deployments, want 1", deployments)
	}
	return kio.StringAll(nodes)
}

// common labels n, moves a namespaced object (and a binding's
// ServiceAccount subjects) into ns, and puts a Deployment on the system
// node group.
func common(n *yaml.RNode, ns string) error {
	if err := n.PipeE(yaml.SetLabel(partOfKey, partOfValue)); err != nil {
		return err
	}
	if n.GetNamespace() != "" {
		if err := n.SetNamespace(ns); err != nil {
			return err
		}
	}
	switch n.GetKind() {
	case "ClusterRoleBinding", "RoleBinding":
		subjects, err := n.Pipe(yaml.Lookup("subjects"))
		if err != nil || subjects == nil {
			return err
		}
		items, err := subjects.Elements()
		if err != nil {
			return err
		}
		for _, s := range items {
			if yaml.GetValue(s.Field("kind").Value) == "ServiceAccount" {
				if err := s.PipeE(yaml.SetField("namespace", yaml.NewStringRNode(ns))); err != nil {
					return err
				}
			}
		}
	case "Deployment":
		sel, err := n.Pipe(yaml.LookupCreate(yaml.MappingNode, "spec", "template", "spec", "nodeSelector"))
		if err != nil {
			return err
		}
		if err := sel.PipeE(yaml.SetField(poolKey, yaml.NewStringRNode(poolValue))); err != nil {
			return err
		}
		tol, err := yaml.Parse(fmt.Sprintf("[{key: %s, operator: Equal, value: %s, effect: NoSchedule}]", taintKey, taintValue))
		if err != nil {
			return err
		}
		spec, err := n.Pipe(yaml.Lookup("spec", "template", "spec"))
		if err != nil {
			return err
		}
		if err := spec.PipeE(yaml.SetField("tolerations", tol)); err != nil {
			return err
		}
	}
	return nil
}

// pinContainer checks the Deployment's one container runs want (the tag the
// manifest was pinned with) and replaces it with pinned.
func pinContainer(n *yaml.RNode, want, pinned string) error {
	c, err := container(n)
	if err != nil {
		return err
	}
	if got := yaml.GetValue(c.Field("image").Value); got != want {
		return fmt.Errorf("Deployment %s runs %s, not %s: the release manifest changed", n.GetName(), got, want)
	}
	return c.PipeE(yaml.SetField("image", yaml.NewStringRNode(pinned)))
}

func container(n *yaml.RNode) (*yaml.RNode, error) {
	cs, err := n.Pipe(yaml.Lookup("spec", "template", "spec", "containers"))
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, fmt.Errorf("Deployment %s has no containers", n.GetName())
	}
	items, err := cs.Elements()
	if err != nil {
		return nil, err
	}
	if len(items) != 1 {
		return nil, fmt.Errorf("Deployment %s has %d containers, want 1", n.GetName(), len(items))
	}
	return items[0], nil
}

// appendSeq appends v to the Deployment's container field (args, env).
func appendSeq(n *yaml.RNode, field string, v *yaml.RNode) error {
	c, err := container(n)
	if err != nil {
		return err
	}
	seq, err := c.Pipe(yaml.LookupCreate(yaml.SequenceNode, field))
	if err != nil {
		return err
	}
	return seq.PipeE(yaml.Append(v.YNode()))
}

// namespace is a Namespace manifest with the plugin's label.
func namespace(name string) []byte {
	return []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    %s: %s\n", name, partOfKey, partOfValue))
}
