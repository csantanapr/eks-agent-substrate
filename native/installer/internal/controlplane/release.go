package controlplane

import (
	"context"
	"fmt"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
)

// readInstalled is what --probe-installed reads off the cluster behind
// kubeconfig kc: whether Substrate runs there, and the release tag of its API
// server image. Tests replace it.
var readInstalled = func(ctx context.Context, kc string) (probe.Result, error) {
	line, err := probe.Check(ctx, probe.KubectlWith(kc))
	if err != nil {
		return probe.Result{}, err
	}
	return probe.ParseInstalled([]string{line})
}

// SameRelease refuses rel when the cluster behind kc already runs another
// Substrate release: v1.0.0 to a fork, a fork back to v1.0.0, or one tag to
// another. The installed control plane would keep its worker pools on one
// release while it moved to the other, and a worker can reject what another
// release's controller gives it, so the only way to change the release is to
// remove the control plane and install again: the wizard's reinstall guard
// applies the same rule. A cluster with no Substrate, or whose release cannot
// be read (an interrupted install), is let through; a failed read is an
// error.
func SameRelease(ctx context.Context, kc, cluster string, rel plugin.Release) error {
	r, err := readInstalled(ctx, kc)
	if err != nil {
		return fmt.Errorf("read the release %s runs: %w", cluster, err)
	}
	if !r.Installed || r.Version == "" || r.Version == rel.Tag {
		return nil
	}
	fix := "run --remove-control-plane, then install again with --tag " + rel.Tag
	if id := plugin.SourceOf(r.Version); id != "" {
		fix = plugin.ReleaseFix(id)
	} else if id := plugin.SourceOf(rel.Tag); id != "" {
		fix = plugin.ReleaseFix(id)
	}
	return fmt.Errorf("%s runs Substrate %s, not %s: changing the release of an installed control plane would leave a mixed-version cluster "+
		"(worker pools of one release under a control plane of another), so nothing was changed; %s", cluster, r.Version, rel.Tag, fix)
}
