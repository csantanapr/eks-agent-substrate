package controlplane

import (
	"slices"
	"strings"
	"testing"
)

func opts() Options {
	return Options{Cluster: "demo", Region: "us-west-2", Profile: "p", Tag: "v1.0.0",
		ImageRepo: "ghcr.io/agent-substrate/substrate", SnapBucket: "demo-snap-111122223333-us-west-2",
		AssetBucket: "demo-assets-111122223333-us-west-2", SandboxClasses: "gvisor,microvm"}
}

func flag(args []string, name string) string {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// The flags eks-talk's substrate/install.sh passes, minus the RDS ones.
func TestAteSetupArgsMatchInstallScript(t *testing.T) {
	a := AteSetupArgs(opts(), "demo")
	for name, want := range map[string]string{
		"--context": "demo", "--image-tag": "v1.0.0", "--storage-bucket-name": "demo-snap-111122223333-us-west-2",
		"--snapshot-location": "s3://demo-snap-111122223333-us-west-2/", "--sandbox-classes": "gvisor,microvm",
		"--credential-provider":        `{"name":"k8s.io"}`,
		"--ingress-allowed-spiffe-ids": "spiffe://cluster.local/ns/ate-system/sa/atenet-client",
	} {
		if got := flag(a, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !slices.Equal(a[:4], []string{"run", "./cmd/ate-setup", "deploy", "ate-system"}) {
		t.Errorf("not an ate-setup deploy: %v", a[:4])
	}
}

// Secrets never ride in argv or env: the POC has none, and no postgres
// connection string may appear (that is the RDS component's job, via 0600 files).
func TestNoSecretsInArgvOrEnv(t *testing.T) {
	all := append(AteSetupArgs(opts(), "demo"), AteSetupEnv(opts(), "/k")...)
	for _, s := range all {
		l := strings.ToLower(s)
		if strings.Contains(l, "postgres://") || strings.Contains(l, "password") || strings.Contains(l, "secret_access_key") {
			t.Errorf("secret-looking value in argv/env: %q", s)
		}
	}
}

func TestEnvSelectsS3AndBlanksAmbientContext(t *testing.T) {
	env := AteSetupEnv(opts(), "/k")
	for _, want := range []string{"ATE_INSTALL_STORAGE_BACKEND=s3", "ATE_ASSET_BUCKET=demo-assets-111122223333-us-west-2", "KUBECONFIG=/k", "KUBECTL_CONTEXT="} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}
