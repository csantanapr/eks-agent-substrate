// Package eksoverlay adds this repository's EKS kustomize content (deploy/eks)
// to a fetched agent-substrate/substrate tree, so `ate-setup deploy ate-system`
// in that tree renders the control plane for EKS.
//
// It is the counterpart of substrate-gke's installer/internal/gkeoverlay and
// keeps its shape (Find, Add, Apply, the same file-sync and YAML-edit
// helpers) so a change upstream makes there can be carried here by reading
// one diff. What differs on EKS:
//
//   - It is always needed: no upstream release ships EKS manifests.
//   - It drops the GKE Managed Prometheus PodMonitoring from the base
//     kustomization, a CRD an EKS cluster does not have.
//   - It fills ${AWS_REGION} in the Component as it copies it.
//   - It applies deploy/eks/patches, the ate-setup Go change that stages
//     microVM assets in S3. Upstream hard-codes gs:// for every non-Kind
//     install; that is the one EKS change kustomize cannot carry.
//
// TODO: Like gkeoverlay, a stopgap until upstream ships `ate-setup deploy
// --component` (agent-substrate/substrate PR 3179) or `ate-setup render`
// (#3240), and an object-store scheme for microVM assets on non-Kind
// clusters. Then pass --component and delete this package.
package eksoverlay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// Dir is where the EKS kustomize content lives, relative to the native/ root.
const Dir = "deploy/eks"

const (
	componentSrc = "components/eks"
	// resourcesSrc holds plain resources, kept out of the Component for the
	// reason deploy/gke gives: ate-setup's deploy --component rejects
	// components that add resources.
	resourcesSrc = "resources"
	patchesSrc   = "patches"
)

const (
	installDir = "manifests/ate-install"
	// BaseKustomization is what `ate-setup deploy ate-system` renders.
	BaseKustomization = installDir + "/base/kustomization.yaml"
	// ComponentDir is where the copy goes; upstream has nothing named
	// substrate-eks, so it cannot collide.
	ComponentDir = installDir + "/components/substrate-eks"
	// ResourcesDir is where deploy/eks/resources is copied.
	ResourcesDir   = installDir + "/substrate-eks"
	resourcesEntry = "../substrate-eks/"
	// gkeMonitoringEntry is the base kustomization's GKE PodMonitoring.
	gkeMonitoringEntry = "../atenet-router-monitoring.yaml"
	componentEntry     = "../components/substrate-eks"
	// regionToken is replaced with the cluster's Region in the copy.
	regionToken = "${AWS_REGION}"
	// codeMarker records that deploy/eks/patches were applied.
	codeMarker = ".substrate-eks-code-patch"
)

// LinePrefix starts every line Apply prints; the deploy checklist keys off it.
const LinePrefix = "substrate-eks EKS overlay: "

// Find returns the absolute path of deploy/eks, searched above each start
// directory unless explicit is set (same rules as gkeoverlay.Find).
func Find(explicit string, starts ...string) (string, error) {
	if explicit != "" {
		if err := check(explicit); err != nil {
			return "", err
		}
		return filepath.Abs(explicit)
	}
	for _, start := range starts {
		if start == "" {
			continue
		}
		dir, err := filepath.Abs(start)
		if err != nil {
			continue
		}
		for {
			if candidate := filepath.Join(dir, Dir); check(candidate) == nil {
				return candidate, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("cannot find %s above %s; pass --eks-overlay", Dir, strings.Join(slices.Compact(slices.Clone(starts)), " or "))
}

func check(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, componentSrc, "kustomization.yaml")); err != nil {
		return fmt.Errorf("%s is not %s: %w", dir, Dir, err)
	}
	return nil
}

// Add copies the Component into the tree at root with region filled in,
// lists it in the base kustomization and drops the GKE PodMonitoring entry.
// Idempotent: a second run writes nothing. It reports whether it changed
// anything.
func Add(root, overlay, region string) (bool, error) {
	if region == "" {
		return false, errors.New("an AWS Region is required")
	}
	if _, err := os.Stat(filepath.Join(root, BaseKustomization)); err != nil {
		return false, fmt.Errorf("%s has no %s; it does not look like a substrate tree", root, BaseKustomization)
	}
	if err := check(overlay); err != nil {
		return false, err
	}
	fill := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte(regionToken), []byte(region)) }
	changed, err := syncDir(filepath.Join(overlay, componentSrc), filepath.Join(root, ComponentDir), fill)
	if err != nil {
		return false, err
	}
	resChanged := false
	if _, err := os.Stat(filepath.Join(overlay, resourcesSrc)); err == nil {
		if resChanged, err = syncDir(filepath.Join(overlay, resourcesSrc), filepath.Join(root, ResourcesDir), fill); err != nil {
			return false, err
		}
	} else if _, err := os.Stat(filepath.Join(root, ResourcesDir)); err == nil {
		// deploy/eks has no resources (any more): drop an older run's copies.
		if err := os.RemoveAll(filepath.Join(root, ResourcesDir)); err != nil {
			return false, err
		}
		resChanged = true
	}
	resources, err := resourceFiles(overlay)
	if err != nil {
		return false, err
	}
	kChanged, err := editKustomization(filepath.Join(root, BaseKustomization), componentEntry, gkeMonitoringEntry, resources)
	return changed || resChanged || kChanged, err
}

// resourceFiles lists deploy/eks/resources/*.yaml as base kustomization
// entries, sorted (as gkeoverlay.resourceFiles).
func resourceFiles(overlay string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(overlay, resourcesSrc))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, resourcesEntry+e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// ApplyCodePatches applies every deploy/eks/patches/*.patch that applies to
// the tree, once. A patch already applied (git apply --reverse --check
// succeeds) is skipped, so a rerun is a no-op.
func ApplyCodePatches(w io.Writer, root, overlay string) error {
	patches, _ := filepath.Glob(filepath.Join(overlay, patchesSrc, "*.patch"))
	slices.Sort(patches)
	applied := 0
	for _, p := range patches {
		if exec.Command("git", "-C", root, "apply", "--reverse", "--check", p).Run() == nil {
			fmt.Fprintln(w, LinePrefix+"code patch already applied: "+filepath.Base(p))
			applied++
			continue
		}
		if exec.Command("git", "-C", root, "apply", "--check", p).Run() != nil {
			continue // written for another release
		}
		if out, err := exec.Command("git", "-C", root, "apply", p).CombinedOutput(); err != nil {
			return fmt.Errorf("git apply %s: %w: %s", p, err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintln(w, LinePrefix+"applied code patch "+filepath.Base(p))
		applied++
	}
	if applied == 0 {
		return fmt.Errorf("no patch in %s applies to %s; rebase the newest one onto this release", filepath.Join(overlay, patchesSrc), root)
	}
	_, err := writeIfChanged(filepath.Join(root, codeMarker), []byte("applied\n"))
	return err
}

// Digest is the hex SHA-256 of the overlay at dir: each file under it (the
// Component, the resources and the code patches), by its path below dir and
// its content. Apply edits the tree in place, so the tree cache keys each
// tree by it (plugin.Release.Locate): two checkouts whose deploy/eks differ
// get two trees, and the same deploy/eks reuses one.
func Digest(dir string) (string, error) {
	if err := check(dir); err != nil {
		return "", err
	}
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", dir, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Apply runs Add and ApplyCodePatches and prints the outcome.
func Apply(w io.Writer, root, overlay, region string) error {
	changed, err := Add(root, overlay, region)
	if err != nil {
		return fmt.Errorf("adding %s to %s: %w", Dir, root, err)
	}
	if changed {
		fmt.Fprintln(w, LinePrefix+"added "+Dir+" to "+BaseKustomization+" ("+ComponentDir+", region "+region+")")
	} else {
		fmt.Fprintln(w, LinePrefix+Dir+" is already in "+BaseKustomization)
	}
	return ApplyCodePatches(w, root, overlay)
}

// syncDir makes dst's files match src's after transform; see gkeoverlay.
func syncDir(src, dst string, transform func([]byte) []byte) (bool, error) {
	changed := false
	want := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		want[rel] = true
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		wrote, err := writeIfChanged(filepath.Join(dst, rel), transform(data))
		changed = changed || wrote
		return err
	})
	if err != nil {
		return false, err
	}
	err = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if !want[rel] && !isTemp(d.Name()) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			changed = true
		}
		return nil
	})
	return changed, err
}

const tempInfix = ".tmp-"

func isTemp(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, tempInfix)
}

// writeIfChanged writes data through a temporary file and a rename, unless
// path already holds exactly data.
func writeIfChanged(path string, data []byte) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+tempInfix)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// editKustomization adds component under `components:` and each of
// resources under `resources:`, removes drop, and removes entries under
// resourcesEntry that resources no longer names (an older deploy/eks's). It
// edits the YAML tree so comments and other entries survive.
func editKustomization(path, component, drop string, resources []string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	node, err := yaml.Parse(string(data))
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	edited := false
	res, err := node.Pipe(yaml.LookupCreate(yaml.SequenceNode, "resources"))
	if err != nil {
		return false, fmt.Errorf("%s: resources: %w", path, err)
	}
	var have []string
	kept := res.Content()[:0]
	for _, el := range res.Content() {
		stale := strings.HasPrefix(el.Value, resourcesEntry) && !slices.Contains(resources, el.Value)
		if el.Value == drop || stale {
			edited = true
			continue
		}
		kept = append(kept, el)
		have = append(have, el.Value)
	}
	res.YNode().Content = kept
	for _, r := range resources {
		if slices.Contains(have, r) {
			continue
		}
		if err := res.PipeE(yaml.Append(yaml.NewScalarRNode(r).YNode())); err != nil {
			return false, err
		}
		edited = true
	}
	comps, err := node.Pipe(yaml.LookupCreate(yaml.SequenceNode, "components"))
	if err != nil {
		return false, fmt.Errorf("%s: components: %w", path, err)
	}
	if !slices.ContainsFunc(comps.Content(), func(n *yaml.Node) bool { return n.Value == component }) {
		if err := comps.PipeE(yaml.Append(yaml.NewScalarRNode(component).YNode())); err != nil {
			return false, err
		}
		edited = true
	}
	if !edited {
		return false, nil
	}
	out, err := node.String()
	if err != nil {
		return false, fmt.Errorf("render %s: %w", path, err)
	}
	return writeIfChanged(path, []byte(out))
}
