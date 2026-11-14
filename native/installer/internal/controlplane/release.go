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

// InstalledRelease is the release tag the control plane behind kc runs, as
// --probe-installed reads it: "" when Substrate is not installed or its
// release cannot be read (an interrupted install). A failed read is an
// error.
func InstalledRelease(ctx context.Context, kc string) (string, error) {
	r, err := readInstalled(ctx, kc)
	if err != nil {
		return "", err
	}
	if !r.Installed {
		return "", nil
	}
	return r.Version, nil
}

// CheckRelease refuses rel on a cluster whose control plane already runs
// release have: v1.0.0 to a fork, a fork back to v1.0.0, or one tag to
// another. The installed control plane would keep its worker pools on one
// release while it moved to the other, and a worker can reject what another
// release's controller gives it, so the only way to change the release is to
// remove the control plane and install again: the wizard's reinstall guard
// applies the same rule. have "" (no Substrate, or a release that cannot be
// read) is let through.
func CheckRelease(cluster, have string, rel plugin.Release) error {
	if have == "" || have == rel.Tag {
		return nil
	}
	return fmt.Errorf("%s runs Substrate %s, not %s: changing the release of an installed control plane would leave a mixed-version cluster "+
		"(worker pools of one release under a control plane of another), so nothing was changed; %s", cluster, have, rel.Tag, plugin.ReinstallFix(have, rel.Tag))
}

// SameRelease is CheckRelease on what the cluster behind kc runs.
func SameRelease(ctx context.Context, kc, cluster string, rel plugin.Release) error {
	have, err := InstalledRelease(ctx, kc)
	if err != nil {
		return fmt.Errorf("read the release %s runs: %w", cluster, err)
	}
	return CheckRelease(cluster, have, rel)
}
