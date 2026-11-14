package dbrotation

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// ID is the plugin's --plugins value and setup-aws subcommand.
const ID = "db-rotation"

var (
	//go:embed external-secret.yaml
	externalSecretYAML string
	//go:embed reloader-config.yaml
	reloaderConfigYAML []byte
)

// Objects the plugin makes in ate-system, and what it reads there.
const (
	ateNS          = "ate-system"
	externalSecret = "ate-api-server-db"
	secretStore    = "aws-secrets-manager"
	envSecret      = "ate-api-server-secret-envvars"
	apiServer      = "ate-api-server"
	reloaderConfig = "ate-api-server-db"
	// bundledDB is the StatefulSet of ate-setup's bundled PostgreSQL.
	bundledDB = "postgres"
)

const (
	esoCRD      = "externalsecrets.external-secrets.io"
	storeCRD    = "secretstores.external-secrets.io"
	reloaderCRD = "configs.reloader.external-secrets.io"
)

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Secrets Manager login rotation",
		Summary: "Rotates the two RDS logins with Secrets Manager (alternating users); External Secrets syncs them " +
			"and the ESO Reloader rolls ate-api-server. Needs the RDS database.",
		Order:    40,
		Minutes:  8,
		SetupAWS: true,
		// Two Lambda functions that run once a rotation, and one small
		// controller pod each: cents a month.
		HourlyUSD: 0,
		Deploy:    deploy,
		Verify:    verify,
		Remove:    remove,
	})
}

func kubectl(ctx context.Context, w io.Writer, o plugin.Options, stdin []byte, args ...string) error {
	return plugin.Kubectl(ctx, w, o, stdin, nil, args...)
}

// output is kubectl's stdout, which stays in memory.
func output(ctx context.Context, w io.Writer, o plugin.Options, args ...string) (string, error) {
	var b bytes.Buffer
	err := plugin.Kubectl(ctx, w, o, nil, &b, args...)
	return b.String(), err
}

// requireRDS refuses an install whose control plane uses the bundled
// PostgreSQL, whose logins are not in Secrets Manager.
func requireRDS(ctx context.Context, w io.Writer, o plugin.Options) error {
	s, err := output(ctx, w, o, "-n", ateNS, "get", "secret", envSecret, "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("the control plane is not installed (no Secret %s/%s): run --deploy-control-plane --database rds first", ateNS, envSecret)
	}
	b, err := output(ctx, w, o, "-n", ateNS, "get", "statefulset", bundledDB, "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(b) != "" {
		return fmt.Errorf("db-rotation rotates the RDS logins, and this install uses the bundled PostgreSQL (StatefulSet %s/%s): "+
			"install with --database rds to use it; remove the plugin to delete its AWS resources", ateNS, bundledDB)
	}
	return nil
}

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	if err := requireRDS(ctx, w, o); err != nil {
		return err
	}

	fmt.Fprintf(w, "External Secrets Operator %s (the controller only) in namespace %s\n", esoVersion, esoNS)
	raw, err := pinned(ctx, esoURL, esoSHA256)
	if err != nil {
		return err
	}
	eso, err := transformESO(raw)
	if err != nil {
		return err
	}
	if err := applyRelease(ctx, w, o, esoNS, eso); err != nil {
		return err
	}
	if err := kubectl(ctx, w, o, nil, "wait", "--for=condition=Established", "crd/"+esoCRD, "crd/"+storeCRD, "--timeout=2m"); err != nil {
		return err
	}
	if err := kubectl(ctx, w, o, nil, "-n", esoNS, "rollout", "status", "deploy/"+esoDeployment, "--timeout=5m"); err != nil {
		return err
	}
	if err := ensurePodIdentity(ctx, w, o); err != nil {
		return err
	}

	fmt.Fprintf(w, "SecretStore %s and ExternalSecret %s in %s\n", secretStore, externalSecret, ateNS)
	es, err := render(externalSecretYAML, o)
	if err != nil {
		return err
	}
	if err := kubectl(ctx, w, o, []byte(es), "apply", "-f", "-"); err != nil {
		return err
	}
	if err := kubectl(ctx, w, o, nil, "-n", ateNS, "wait", "externalsecret", externalSecret, "--for=condition=Ready", "--timeout=3m"); err != nil {
		return fmt.Errorf("ExternalSecret %s/%s is not Ready (kubectl -n %s describe externalsecret %s): %w", ateNS, externalSecret, ateNS, externalSecret, err)
	}

	fmt.Fprintf(w, "ESO Reloader %s in namespace %s\n", reloaderVersion, reloaderNS)
	raw, err = pinned(ctx, reloaderURL, reloaderSHA256)
	if err != nil {
		return err
	}
	rl, err := transformReloader(raw)
	if err != nil {
		return err
	}
	if err := applyRelease(ctx, w, o, reloaderNS, rl); err != nil {
		return err
	}
	if err := kubectl(ctx, w, o, nil, "wait", "--for=condition=Established", "crd/"+reloaderCRD, "--timeout=2m"); err != nil {
		return err
	}
	if err := kubectl(ctx, w, o, nil, "-n", reloaderNS, "rollout", "status", "deploy/reloader", "--timeout=5m"); err != nil {
		return err
	}
	// A change to ate-api-server-secret-envvars rolls ate-api-server.
	return kubectl(ctx, w, o, reloaderConfigYAML, "apply", "-f", "-")
}

// applyRelease applies a release manifest into its namespace. Server-side,
// since a CRD is larger than client-side apply's annotation allows; forced,
// so an install a Helm release made (demos/db-rotation) is taken over.
func applyRelease(ctx context.Context, w io.Writer, o plugin.Options, ns, manifest string) error {
	if err := kubectl(ctx, w, o, namespace(ns), "apply", "-f", "-"); err != nil {
		return err
	}
	return kubectl(ctx, w, o, []byte(manifest), "apply", "--server-side", "--force-conflicts", "--field-manager=substrate-eks", "-f", "-")
}

// render fills the ExternalSecret's two placeholders.
func render(tmpl string, o plugin.Options) (string, error) {
	out := strings.NewReplacer("${AWS_REGION}", o.Region, "${SECRET_PREFIX}", o.Cluster).Replace(tmpl)
	if strings.Contains(out, "${") {
		return "", fmt.Errorf("external-secret.yaml has a placeholder the plugin does not fill")
	}
	return out, nil
}

// ensurePodIdentity restarts the controller when a pod started before its
// Pod Identity association applied: EKS injects the credentials only into
// pods created after it.
func ensurePodIdentity(ctx context.Context, w io.Writer, o plugin.Options) error {
	for attempt := 1; ; attempt++ {
		ps, err := pods(ctx, w, o, esoNS, "app.kubernetes.io/name="+esoDeployment)
		if err != nil {
			return err
		}
		missing := podsWithout(ps, "AWS_CONTAINER_CREDENTIALS_FULL_URI")
		if len(missing) == 0 {
			fmt.Fprintf(w, "%s/%s has Pod Identity credentials\n", esoNS, esoDeployment)
			return nil
		}
		if attempt == 4 {
			return fmt.Errorf("%s/%s pods still lack Pod Identity credentials: %s", esoNS, esoDeployment, strings.Join(missing, " "))
		}
		fmt.Fprintf(w, "%s/%s started before its Pod Identity association applied; restarting\n", esoNS, esoDeployment)
		if err := kubectl(ctx, w, o, nil, "-n", esoNS, "rollout", "restart", "deploy/"+esoDeployment); err != nil {
			return err
		}
		if err := kubectl(ctx, w, o, nil, "-n", esoNS, "rollout", "status", "deploy/"+esoDeployment, "--timeout=5m"); err != nil {
			return err
		}
	}
}

// pod is the part of a pod the plugin reads.
type pod struct {
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
}

// pods are the pods matching selector in ns that are not terminating.
func pods(ctx context.Context, w io.Writer, o plugin.Options, ns, selector string) ([]pod, error) {
	s, err := output(ctx, w, o, "-n", ns, "get", "pods", "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []pod `json:"items"`
	}
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return nil, fmt.Errorf("read the pods of %s (%s): %w", ns, selector, err)
	}
	var out []pod
	for _, p := range l.Items {
		if p.Metadata.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// podsWithout returns the pods whose first container's environment lacks
// name. No pod at all is one missing ("(none)").
func podsWithout(ps []pod, name string) []string {
	if len(ps) == 0 {
		return []string{"(none)"}
	}
	var missing []string
	for _, p := range ps {
		found := false
		if len(p.Spec.Containers) > 0 {
			for _, e := range p.Spec.Containers[0].Env {
				found = found || e.Name == name
			}
		}
		if !found {
			missing = append(missing, p.Metadata.Name)
		}
	}
	return missing
}

// remove deletes what deploy made: the Reloader Config, the ExternalSecret
// and SecretStore (the Secret ate-api-server-secret-envvars stays, with the
// current logins), then both releases, which carry the plugin's label. The
// logins in Secrets Manager and the database users stay; `setup-aws plugin
// db-rotation --delete`, which Run calls next, turns rotation off.
func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	has := func(crd string) (bool, error) {
		s, err := output(ctx, w, o, "get", "crd", crd, "--ignore-not-found", "-o", "name")
		return strings.TrimSpace(s) != "", err
	}
	for _, d := range []struct {
		crd  string
		args []string
	}{
		{reloaderCRD, []string{"delete", "configs.reloader.external-secrets.io", reloaderConfig, "--ignore-not-found"}},
		{esoCRD, []string{"-n", ateNS, "delete", "externalsecret", externalSecret, "--ignore-not-found"}},
		{storeCRD, []string{"-n", ateNS, "delete", "secretstore", secretStore, "--ignore-not-found"}},
	} {
		ok, err := has(d.crd)
		if err != nil {
			return err
		}
		if ok {
			if err := kubectl(ctx, w, o, nil, d.args...); err != nil {
				return err
			}
		}
	}
	if err := kubectl(ctx, w, o, nil, "delete", "namespace", "-l", partOf, "--ignore-not-found", "--timeout=5m"); err != nil {
		return err
	}
	return kubectl(ctx, w, o, nil, "delete", "crd,clusterrole,clusterrolebinding", "-l", partOf, "--ignore-not-found")
}
