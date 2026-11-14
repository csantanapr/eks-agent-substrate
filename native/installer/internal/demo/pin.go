package demo

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// PinPool sets spec.template.nodeSelector[ate.dev/pool] to pool on every
// WorkerPool in the manifest at path, before the deploy applies the pool.
// The manifest is upstream's demo template in the installer's cached tree,
// which the deploy renders from that fixed path, so the edit is made in
// place. It reports whether it changed the
// file; a second run changes nothing.
func PinPool(path, pool string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	nodes, err := kio.FromBytes(data)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	found, changed := false, false
	for _, n := range nodes {
		if n.GetKind() != "WorkerPool" {
			continue
		}
		found = true
		sel, err := n.Pipe(yaml.LookupCreate(yaml.MappingNode, "spec", "template", "nodeSelector"))
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		if v := sel.Field(PoolLabel); v != nil && yaml.GetValue(v.Value) == pool {
			continue
		}
		if err := sel.PipeE(yaml.SetField(PoolLabel, yaml.NewStringRNode(pool))); err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		changed = true
	}
	if !found {
		return false, fmt.Errorf("no WorkerPool in %s", path)
	}
	if !changed {
		return false, nil
	}
	out, err := kio.StringAll(nodes)
	if err != nil {
		return false, err
	}
	if bytes.Equal([]byte(out), data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
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
