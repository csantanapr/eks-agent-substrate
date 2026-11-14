// Package mode3iso is the mode3-iso plugin: mode 3 on EKS Auto Mode isolated
// sandboxes. As in mode 3, each worker Pod is a microVM that runs gVisor
// actors inside; here the microVM is RuntimeClass isolated-sandbox, which
// EKS Auto Mode provides, on nodes Auto Mode launches for those Pods. No
// node group and no Kata install is involved.
//
//   - Requires mode3, whose control plane (the fork, with NetworkWorkers) and
//     counter image it uses, and an Auto Mode cluster.
//   - Deploy: NodePool ate-isolated (a copy of general-purpose, labeled and
//     tainted) with NodeClass ate-isolated (a copy of its NodeClass that tags
//     the instances with the owner tag and TAGS), atelet-iso (a copy of
//     atelet for those Bottlerocket nodes), and the counter pool counter-iso
//     on RuntimeClass isolated-sandbox.
//   - Verify: every worker Pod runs under isolated-sandbox on an ate-isolated
//     node, the counter continues across a suspend and resume, and deleting
//     an actor tears its sandbox down in the guest.
//   - Remove: the pool, atelet-iso and its ConfigMap, then the NodePool,
//     whose nodes Auto Mode drains and terminates; it waits for them, then
//     deletes the NodeClass.
//
// It makes no AWS resources of its own (Auto Mode owns the nodes), so it has
// no setup-aws half.
package mode3iso

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
)

// ID is the plugin ID.
const ID = "mode3-iso"

// The names of the plugin's objects.
const (
	RuntimeClass                = "isolated-sandbox"
	NodePoolName                = "ate-isolated"
	PoolLabel                   = "ate.dev/pool"
	PoolValue                   = "isolated"
	TaintKey                    = "ate.dev/sandboxClass"
	AteletName                  = "atelet-iso"
	VariantLabel                = "ate.dev/atelet-variant"
	CredentialProviderConfigMap = "atelet-iso-credential-provider"
	// nodePoolLabel is the label Auto Mode's Karpenter puts on the nodes of
	// a NodePool.
	nodePoolLabel = "karpenter.sh/nodepool"
)

var (
	//go:embed counter-iso.yaml
	poolManifest string
	//go:embed counter-iso-template.yaml
	templateManifest string
	//go:embed atelet-iso-credential-provider.yaml
	providerManifest []byte
)

// Iso is the mode 3 counter pool on isolated sandboxes.
var Iso = mode3.Counter{
	Namespace: "ate-demo-counter-iso", Name: "counter-iso",
	Actor: "demo-iso", DeleteActor: "demo-iso-delete",
	RuntimeClass: RuntimeClass, NodeLabels: map[string]string{PoolLabel: PoolValue, nodePoolLabel: NodePoolName},
	Pool: poolManifest, Template: templateManifest,
}

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Mode 3: actor in a microVM Pod (isolated-sandbox)",
		Summary: "Mode 3 with each worker Pod an EKS Auto Mode isolated sandbox (RuntimeClass isolated-sandbox), " +
			"on nodes Auto Mode launches for them (NodePool ate-isolated). Needs Auto Mode and mode 3, which it turns on; " +
			"adds a counter pool that it checks.",
		Order: mode3.Order + 1, Minutes: 12, Requires: []string{mode3.ID}, NeedsAutoMode: needsAutoMode,
		// One node for the two worker Pods, launched by Auto Mode (an
		// m8a.large on-demand, with the Auto Mode fee).
		HourlyUSD: 0.12,
		Deploy:    deploy, Verify: Iso.Verify, Remove: remove,
	})
}

// needsAutoMode is why the plugin needs EKS Auto Mode (Plugin.NeedsAutoMode):
// the wizard offers it only there, and Run refuses its deploy and verify
// anywhere else, so Deploy and Verify do not check it again.
const needsAutoMode = "needs EKS Auto Mode: its isolated-sandbox RuntimeClass and NodePool come from Auto Mode"

// createdHere reports a cluster `setup-aws create cluster` made: its role
// carries the auto-mode-node-tags policy, which lets Auto Mode launch nodes
// of a NodeClass with the deployment's tags. On another cluster that would
// fail validation, so the NodeClass gets no tags there, as the StorageClass
// gets none. Tests replace it.
var createdHere = func(ctx context.Context, o plugin.Options) (bool, error) {
	c, err := awsx.New(ctx, o.Profile, o.Region)
	if err != nil {
		return false, err
	}
	cl, err := c.Describe(ctx, o.Cluster)
	if err != nil {
		return false, err
	}
	return cl.CreatedByInstaller(o.Region), nil
}

// nodeClassTries bounds the wait for NodeClass ate-isolated to be Ready, at
// mode3.Interval (2s): 3 minutes.
const nodeClassTries = 90

// waitNodeClass waits for NodeClass ate-isolated to be Ready: a NodeClass
// Auto Mode cannot validate launches no node, and the worker Pods would
// wait for one without saying why.
func waitNodeClass(ctx context.Context, w io.Writer, o plugin.Options) error {
	why := "no status yet"
	for i := 0; i < nodeClassTries; i++ {
		var out bytes.Buffer
		if err := plugin.Kubectl(ctx, io.Discard, o, nil, &out, "get", "nodeclass", NodePoolName, "-o", "json"); err != nil {
			return err
		}
		ok, msg, err := controlplane.NodeClassReady(out.Bytes())
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintln(w, "NodeClass "+NodePoolName+" is Ready")
			return nil
		}
		if msg != "" {
			why = msg
		}
		if err := sleep(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("NodeClass %s is not Ready (%s); on a cluster setup-aws created, run `setup-aws create cluster` again: it gives the cluster role the auto-mode-node-tags policy tagged NodeClasses need", NodePoolName, why)
}

// NoNodeTagsLine is printed on a cluster setup-aws did not create.
const NoNodeTagsLine = "The cluster was not created by setup-aws, so its role may not let Auto Mode add tags: the isolated nodes get no deployment tags"

// How long to wait: Auto Mode launches the nodes while the worker Pods
// wait.
const (
	poolRollout   = "--timeout=10m"
	ateletRollout = "--timeout=5m"
)

// deploy runs only on Auto Mode: plugin.Run refuses it elsewhere
// (NeedsAutoMode).
func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	tree, err := mode3.Tree(o)
	if err != nil {
		return err
	}
	var handler bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &handler, "get", "runtimeclass", RuntimeClass, "-o", "jsonpath={.handler}"); err != nil {
		return fmt.Errorf("this cluster has no RuntimeClass %s, which EKS Auto Mode adds: %w", RuntimeClass, err)
	}
	fmt.Fprintf(w, "RuntimeClass %s -> handler %s\n", RuntimeClass, handler.String())

	fmt.Fprintln(w, "NodePool "+NodePoolName+" (a copy of general-purpose, labeled and tainted) and its NodeClass (tagged)")
	var gp, src bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &gp, "get", "nodepool", "general-purpose", "-o", "json"); err != nil {
		return err
	}
	np, srcClass, err := NodePool(gp.Bytes())
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, nil, &src, "get", "nodeclass", srcClass, "-o", "json"); err != nil {
		return err
	}
	tags, err := o.OwnerTags()
	if err != nil {
		return err
	}
	here, err := createdHere(ctx, o)
	if err != nil {
		return err
	}
	if !here {
		fmt.Fprintln(w, NoNodeTagsLine)
		tags = nil
	}
	nc, err := NodeClass(src.Bytes(), tags)
	if err != nil {
		return err
	}
	// The NodeClass first: the NodePool names it.
	for _, m := range [][]byte{nc, np} {
		if err := plugin.Kubectl(ctx, w, o, m, nil, "apply", "-f", "-"); err != nil {
			return err
		}
	}
	if err := waitNodeClass(ctx, w, o); err != nil {
		return err
	}

	fmt.Fprintln(w, AteletName+" on the isolated nodes")
	if err := plugin.Kubectl(ctx, w, o, providerManifest, nil, "apply", "-f", "-"); err != nil {
		return err
	}
	var atelet bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &atelet, "-n", "ate-system", "get", "ds", "atelet", "-o", "json"); err != nil {
		return err
	}
	iso, err := AteletIso(atelet.Bytes())
	if err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, iso, nil, "apply", "-f", "-"); err != nil {
		return err
	}

	fmt.Fprintln(w, "Counter on mode 3, isolated sandboxes")
	do := mode3.DemoOptions(o)
	if _, err := mode3.Demo.KubectlAte(ctx, w, do, tree); err != nil {
		return err
	}
	counter, err := mode3Counter(ctx, do)
	if err != nil {
		return err
	}
	worker, err := mode3.Demo.Pin(ctx, do, mode3.ImageRepo+"/ateworker-gvisor:"+mode3.Tag)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Worker image "+worker+", counter image "+counter)
	// The worker Pods wait for Auto Mode to launch their nodes; the golden
	// snapshot needs a worker and atelet-iso there first.
	waitNodes := func() error {
		if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", Iso.Namespace, "rollout", "status", "deploy/"+Iso.Name, poolRollout); err != nil {
			return err
		}
		return plugin.Kubectl(ctx, w, o, nil, nil, "-n", "ate-system", "rollout", "status", "ds/"+AteletName, ateletRollout)
	}
	return Iso.Deploy(ctx, w, o, worker, counter, waitNodes)
}

// mode3Counter is the counter image mode 3's ActorTemplate runs, which this
// pool runs too: the isolated nodes do not exist before its Pods ask for them,
// and mode 3 built the image for the cluster's sandbox nodes.
func mode3Counter(ctx context.Context, do demo.Options) (string, error) {
	out, err := mode3.Demo.Ate(ctx, do, "get", "actor-template", mode3.Kata.Name, "-a", mode3.Kata.Namespace, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("read mode 3's actor-template %s/%s (deploy %s first): %w", mode3.Kata.Namespace, mode3.Kata.Name, mode3.ID, err)
	}
	img, err := demo.TemplateImage(out)
	if err != nil {
		return "", fmt.Errorf("read mode 3's actor-template %s/%s: %w", mode3.Kata.Namespace, mode3.Kata.Name, err)
	}
	return img, nil
}

// nodesTries bounds the wait for Auto Mode to remove the NodePool's nodes,
// at mode3.Interval (2s): 10 minutes.
const nodesTries = 300

func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if err := Iso.Remove(ctx, w, o); err != nil {
		return err
	}
	fmt.Fprintln(w, "Deleting "+AteletName+" and its credential provider config")
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "-n", "ate-system", "delete", "daemonset", AteletName, "--ignore-not-found", "--wait=true"); err != nil {
		return err
	}
	if err := plugin.Kubectl(ctx, w, o, providerManifest, nil, "delete", "-f", "-", "--ignore-not-found"); err != nil {
		return err
	}
	// Without Auto Mode there is no NodePool kind, and nothing to delete.
	var crd bytes.Buffer
	if err := plugin.Kubectl(ctx, w, o, nil, &crd, "get", "crd", "nodepools.karpenter.sh", "--ignore-not-found", "-o", "name"); err != nil {
		return err
	}
	if strings.TrimSpace(crd.String()) == "" {
		return nil
	}
	fmt.Fprintln(w, "Deleting NodePool "+NodePoolName+"; Auto Mode drains and removes its nodes")
	if err := plugin.Kubectl(ctx, w, o, nil, nil, "delete", "nodepool", NodePoolName, "--ignore-not-found", "--wait=true", "--timeout=10m"); err != nil {
		return err
	}
	if err := waitNoNodes(ctx, w, o); err != nil {
		return err
	}
	// Its nodes are gone, so the NodeClass's finalizer lets it go.
	fmt.Fprintln(w, "Deleting NodeClass "+NodePoolName)
	return plugin.Kubectl(ctx, w, o, nil, nil, "delete", "nodeclass", NodePoolName, "--ignore-not-found", "--wait=true", "--timeout=5m")
}

// waitNoNodes waits until no node carries the NodePool's label.
func waitNoNodes(ctx context.Context, w io.Writer, o plugin.Options) error {
	var left string
	var err error
	for i := 0; i < nodesTries; i++ {
		var out bytes.Buffer
		if err = plugin.Kubectl(ctx, io.Discard, o, nil, &out, "get", "nodes", "-l", nodePoolLabel+"="+NodePoolName, "-o", "name"); err == nil {
			if left = strings.Join(strings.Fields(out.String()), ", "); left == "" {
				fmt.Fprintln(w, "No node of NodePool "+NodePoolName+" is left")
				return nil
			}
		}
		if i == 0 {
			fmt.Fprintln(w, "Waiting for Auto Mode to remove "+left)
		}
		if err := sleep(ctx); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	return errors.New("nodes of NodePool " + NodePoolName + " are still there after 10 minutes: " + left)
}

func sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(mode3.Interval):
		return nil
	}
}
