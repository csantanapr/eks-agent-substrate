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
	// The prereqs (applied before ate-setup) must never ride in the render too.
	if strings.Contains(ks, "storageclass-auto-ebs.yaml") {
		t.Error("Auto Mode StorageClass listed in the render; it is a prerequisite")
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

// An older run's resource copies and entries go when deploy/eks drops them,
// which is what happened to the Auto Mode StorageClass moving to prereqs.
func TestStaleResourceEntryIsRemoved(t *testing.T) {
	root, overlay := tree(t)
	k := filepath.Join(root, BaseKustomization)
	if err := os.WriteFile(k, []byte(baseKust+"  - "+resourcesEntry+"old.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ResourcesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ResourcesDir, "old.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, overlay, "us-west-2"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(k)
	if strings.Contains(string(b), "old.yaml") {
		t.Error("an entry deploy/eks no longer has was kept")
	}
	if _, err := os.Stat(filepath.Join(root, ResourcesDir)); err == nil {
		t.Error("an older run's resource copies were kept")
	}
}

// With resources present, they are copied and listed (gkeoverlay's mechanism).
func TestResourcesAreCopiedAndListed(t *testing.T) {
	root, real := tree(t)
	overlay := t.TempDir()
	if err := os.CopyFS(overlay, os.DirFS(real)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(overlay, resourcesSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, resourcesSrc, "a.yaml"), []byte("region: ${AWS_REGION}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, overlay, "us-west-2"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, BaseKustomization))
	if !strings.Contains(string(b), resourcesEntry+"a.yaml") {
		t.Error("resource not listed")
	}
	c, err := os.ReadFile(filepath.Join(root, ResourcesDir, "a.yaml"))
	if err != nil || string(c) != "region: us-west-2\n" {
		t.Errorf("resource copy = %q, %v", c, err)
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
