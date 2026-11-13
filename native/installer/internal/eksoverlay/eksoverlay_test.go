package eksoverlay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseKust = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../atelet.yaml
  # upstream comment that must survive
  - ../atenet-router-monitoring.yaml
  - ../ate-otel-config.yaml
`

func tree(t *testing.T) (root, overlay string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "manifests/ate-install/base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BaseKustomization), []byte(baseKust), 0o644); err != nil {
		t.Fatal(err)
	}
	// The real deploy/eks, two levels up from this package's directory tree.
	overlay, err := filepath.Abs("../../../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	return root, overlay
}

func TestAddFillsRegionDropsPodMonitoringAndIsIdempotent(t *testing.T) {
	root, overlay := tree(t)
	changed, err := Add(root, overlay, "us-west-2")
	if err != nil || !changed {
		t.Fatalf("first Add: changed=%v err=%v", changed, err)
	}
	k, _ := os.ReadFile(filepath.Join(root, BaseKustomization))
	ks := string(k)
	if strings.Contains(ks, "atenet-router-monitoring.yaml") {
		t.Error("GKE PodMonitoring still listed")
	}
	if !strings.Contains(ks, componentEntry) {
		t.Error("component not listed")
	}
	if !strings.Contains(ks, "../ate-otel-config.yaml") || !strings.Contains(ks, "../atelet.yaml") {
		t.Error("other resources lost")
	}
	comp, err := os.ReadFile(filepath.Join(root, ComponentDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(comp), regionToken) || !strings.Contains(string(comp), "value: us-west-2") {
		t.Error("region not filled in")
	}
	changed, err = Add(root, overlay, "us-west-2")
	if err != nil || changed {
		t.Fatalf("second Add should change nothing: changed=%v err=%v", changed, err)
	}
	// A new Region rewrites the copy (both directions of the idempotence check).
	changed, err = Add(root, overlay, "us-east-1")
	if err != nil || !changed {
		t.Fatalf("Add with a new region: changed=%v err=%v", changed, err)
	}
}

func TestAddRefusesNonSubstrateTree(t *testing.T) {
	_, overlay := tree(t)
	if _, err := Add(t.TempDir(), overlay, "us-west-2"); err == nil {
		t.Fatal("Add accepted a directory with no base kustomization")
	}
}

func TestAddRequiresRegion(t *testing.T) {
	root, overlay := tree(t)
	if _, err := Add(root, overlay, ""); err == nil {
		t.Fatal("Add accepted an empty region")
	}
}
