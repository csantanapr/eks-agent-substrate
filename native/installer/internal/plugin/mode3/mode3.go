// Package mode3 is the mode3 plugin: mode 3, a proposal for upstream
// Substrate in csantanapr/substrate. The worker Pod itself is a microVM (Kata
// Containers on Cloud Hypervisor) and runs actors with gVisor inside, so an
// escape from gVisor still lands in the guest kernel, not on the node.
//
//   - Needs: the control plane installs the fork's pinned commit with its
//     images (ghcr.io/csantanapr/substrate) and the NetworkWorkers preview
//     gate, so atelet reaches a worker over the Pod network.
//   - Deploy: Kata 3.32.0 from kata-deploy's own manifests (kata-deploy.yaml)
//     on the microvm node group, then the counter pool counter-mode3
//     (counter-mode3*.yaml) with RuntimeClass kata, its counter image built
//     from the fork's tree and pushed to the deployment's ECR repository.
//   - Verify: every worker Pod runs under kata on a microvm node, the counter
//     continues across a suspend and resume, and deleting an actor tears its
//     sandbox down in the guest.
//   - Remove: the pool and its ate API objects, Kata (kata-deploy undoes its
//     install on each node), and the counter image. The control plane, and
//     every worker pool, keep running the fork: a control plane deploy
//     refuses to change the release of an installed one, so going back to
//     the upstream release is --remove-control-plane, then an install
//     without mode3.
//
// The fork is the install's one release (plugin.Release): the demo's and the
// other plugins' worker pools run its worker images too, because its
// controller gives every worker the NetworkWorkers gate, on which an upstream
// v1.0.0 worker exits.
//
// It makes no AWS resources of its own, so it has no setup-aws half.
package mode3

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// The fork: the commit its tag names, and where its images are published
// (the eks-talk repository's .github/workflows/mode3-images.yaml builds them).
const (
	ID        = "mode3"
	Repo      = "https://github.com/csantanapr/substrate.git"
	Commit    = "9f36a0d7f2abca950f25fc7bd023752d3852504c"
	Tag       = "v1.0.0-mode3.9f36a0d"
	ImageRepo = "ghcr.io/csantanapr/substrate"
	// Preview is the gate that lets a WorkerPool set spec.networkTransport.
	Preview = "NetworkWorkers"
	// CounterTag is the tag of mode 3's counter image in the deployment's
	// counter repository. The demo, on the same release, pushes its counter
	// as <cluster>/counter:<Tag>, so mode 3 uses its own tag: Remove deletes
	// it, and must not delete the image the demo's template runs.
	CounterTag = ID + "-" + Tag
)

// Order places mode 3 after the plugins that only add volumes or providers;
// mode3-iso, which requires it, comes right after.
const Order = 70

var (
	//go:embed kata-deploy.yaml
	kataManifest []byte
	//go:embed counter-mode3.yaml
	poolManifest string
	//go:embed counter-mode3-template.yaml
	templateManifest string
)

// Kata is the mode 3 counter pool on the microvm node group.
var Kata = Counter{
	Namespace: "ate-demo-counter-mode3", Name: "counter-mode3",
	Actor: "demo-mode3", DeleteActor: "demo-mode3-delete",
	RuntimeClass: "kata", NodeLabels: map[string]string{"ate.dev/pool": "microvm"},
	Pool: poolManifest, Template: templateManifest,
}

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Mode 3: actor in a microVM Pod (kata-clh)",
		Summary: "A proposal for upstream: worker Pods are Kata microVMs on the microvm nodes, with gVisor actors inside, " +
			"so an escape from gVisor lands in the guest kernel. Installs Substrate from csantanapr/substrate " +
			"(" + Tag + "), Kata 3.32.0 with kata-deploy, and a counter pool on RuntimeClass kata that it checks.",
		Order: Order, Minutes: 12,
		Needs: plugin.Needs{
			Preview: []string{Preview},
			Source:  &plugin.Source{Repo: Repo, Commit: Commit, Tag: Tag, ImageRepo: ImageRepo},
		},
		Deploy: deploy, Verify: Kata.Verify, Remove: remove,
	})
}

// Kata's DaemonSet, the label it puts on a node it is done with, and how long
// to wait for it.
const (
	kataDaemonSet   = "ds/kata-deploy"
	kataNodeLabel   = "katacontainers.io/kata-runtime=true"
	kataRollout     = "--timeout=10m"
	kataUninstall   = "--timeout=12m" // its Pods' grace period is 10 minutes
	microvmPoolName = "microvm"
)

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	tree, err := Tree(o)
	if err != nil {
		return err
	}
	do := DemoOptions(o)
	if _, err := Demo.KubectlAte(ctx, w, do, tree); err != nil {
		return err
	}
	// The counter first: it reads the microvm nodes' architecture, so a
	// cluster without them stops before Kata is installed.
	counter, err := Demo.CounterImage(ctx, w, do, tree, microvmPoolName, CounterTag)
	if err != nil {
		return err
	}
	worker, err := Demo.Pin(ctx, do, ImageRepo+"/ateworker-gvisor:"+Tag)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Worker image "+worker)

	fmt.Fprintln(w, "Kata Containers 3.32.0 on the microvm node group (kata-deploy)")
	if err := plugin.Kubectl(ctx, w, o, kataManifest, nil, "apply", "-f", "-"); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", "kube-system", "rollout", "status", kataDaemonSet, kataRollout); err != nil {
		return err
	}
	var handler, nodes bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &handler, "get", "runtimeclass", Kata.RuntimeClass, "-o", "jsonpath={.handler}"); err != nil {
		return fmt.Errorf("no RuntimeClass %s after kata-deploy: %w", Kata.RuntimeClass, err)
	}
	if err := plugin.Kubectl(ctx, w, o, nil, &nodes, "get", "nodes", "-l", kataNodeLabel, "-o", "name"); err != nil {
		return err
	}
	if strings.TrimSpace(nodes.String()) == "" {
		return fmt.Errorf("kata-deploy is ready but labeled no node %s", kataNodeLabel)
	}
	fmt.Fprintf(w, "RuntimeClass %s -> handler %s on %s\n", Kata.RuntimeClass, handler.String(), strings.Join(strings.Fields(nodes.String()), ", "))

	fmt.Fprintln(w, "Counter on mode 3 (gVisor in a Kata microVM Pod)")
	return Kata.Deploy(ctx, w, o, worker, counter, nil)
}

// RemovedNote is what Remove prints last: the control plane is left as is
// (plugin.Run then prints how to leave the release).
const RemovedNote = "The control plane still runs Substrate " + Tag + " from csantanapr/substrate with the " + Preview +
	" gate, and so does every worker pool."

func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if err := Kata.Remove(ctx, w, o); err != nil {
		return err
	}
	// Foreground: the DaemonSet stays until its Pods are gone, and each Pod
	// undoes the install on its node when it gets SIGTERM, which needs the
	// RBAC deleted after it.
	fmt.Fprintln(w, "Removing Kata Containers (kata-deploy undoes its install on each node)")
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", "kube-system", "delete", kataDaemonSet, "--ignore-not-found",
		"--cascade=foreground", "--wait=true", kataUninstall); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, kataManifest, nil, "delete", "-f", "-", "--ignore-not-found", "--wait=true"); err != nil {
		return err
	}
	if err := deleteCounterImage(ctx, w, o); err != nil {
		return err
	}
	fmt.Fprintln(w, RemovedNote)
	return nil
}
