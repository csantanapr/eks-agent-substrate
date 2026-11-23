package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// The nitro node group runs the AWS Nitro sandbox class outside EKS Auto
// Mode (the installer's NITRO_COMPUTE=node-group): a group of the run's
// type, managed or self-managed, of NITRO_INSTANCE_TYPE with Nitro Enclaves
// on, labeled ate.dev/pool=nitro and eks.amazonaws.com/nitro-sandbox=true and
// tainted ate.dev/sandboxClass=nitro, on NITRO_AMI_ID. The EKS optimized AMI
// has no Nitro sandbox runtime, so that AMI must be a Nitro Sandbox AMI the
// operator builds with awslabs/amazon-eks-ami; the run waits until its nodes
// advertise the capacity (waitNitroCapacity).
//
//	https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-ami-create-update.html

// nitroVersion is the Kubernetes version a Nitro Sandbox AMI is built for:
// the cluster's, or KUBERNETES_VERSION for a new one.
func (r *clusterRun) nitroVersion() string {
	if r.cluster != nil && aws.ToString(r.cluster.Version) != "" {
		return aws.ToString(r.cluster.Version)
	}
	return r.cfg.KubernetesVersion
}

// nitroAMIHelp is how to get a Nitro Sandbox AMI: "build one in a clone of
// ...: make k8s=... , see <doc>".
func (r *clusterRun) nitroAMIHelp() string {
	return "build one " + nodegroups.NitroAMIBuild(r.nitroVersion()) + ", see " + nodegroups.NitroAMIDoc
}

// requireNitroAMI refuses a run that wants the nitro group without
// NITRO_AMI_ID, before anything is read or made for the groups: no other
// AMI runs the Nitro sandbox. That holds for the nitro NodePool of
// KARPENTER_NODE_OVERLAY=on too, whose EC2NodeClass names that AMI.
func (r *clusterRun) requireNitroAMI() error {
	if !slices.Contains(r.wanted, nodegroups.Nitro) || strings.TrimSpace(r.cfg.NitroAMIID) != "" {
		return nil
	}
	what := "nitro node group"
	if r.isPool(nodegroups.Nitro) {
		what = "nitro NodePool"
	}
	return fmt.Errorf("NITRO_AMI_ID is empty, and the %s needs a Nitro Sandbox AMI (the EKS optimized AMI has no Nitro sandbox runtime): %s; then set NITRO_AMI_ID to its ID", what, r.nitroAMIHelp())
}

// nitroWaitTimeout bounds waitNitroCapacity. Replaced in tests.
var nitroWaitTimeout = 10 * time.Minute

// nitroNodeProblems are why nodes (the nitro group's) cannot run the Nitro
// sandbox, one per node: not Ready, or no allocatable NitroSandboxResource.
// want is how many nodes the group should have: fewer is a problem too.
// noCapacity reports a Ready node that advertises no Nitro sandbox, which
// only the AMI explains; a node that has not joined or is not Ready yet
// says nothing about the AMI.
func nitroNodeProblems(nodes []kubeNode, want int) (bad []string, noCapacity bool) {
	if len(nodes) < want {
		bad = append(bad, fmt.Sprintf("%d of %d nodes have joined", len(nodes), want))
	}
	for _, n := range nodes {
		q, ok := n.Allocatable[nodegroups.NitroSandboxResource]
		c, _ := nodegroups.SandboxCount(q)
		switch {
		case !n.Ready:
			bad = append(bad, "node "+n.Name+" is not Ready")
		case !ok:
			bad, noCapacity = append(bad, "node "+n.Name+" has no allocatable "+nodegroups.NitroSandboxResource), true
		case c < 1:
			bad, noCapacity = append(bad, fmt.Sprintf("node %s has allocatable %s %q", n.Name, nodegroups.NitroSandboxResource, q)), true
		}
	}
	return bad, noCapacity
}

// waitNitroCapacity waits, once the nitro group's nodes have joined (the
// node group ACTIVE, or the self-managed nodes Ready), until it has want
// nodes and every node labeled ate.dev/pool=nitro is Ready and advertises an
// allocatable eks.amazonaws.com/nitro-sandbox of 1 or more. A node of
// another AMI never does, so after nitroWaitTimeout the run stops and, when
// a Ready node advertises none, says what NITRO_AMI_ID must be; nodes that
// have only not joined or become Ready in time are named as such. A blip of
// the Kubernetes API (no answer, 429, a 5xx) is "not yet"; any other failed
// list stops the wait.
func (r *clusterRun) waitNitroCapacity(ctx context.Context, kube nodeLister, want int32) error {
	sel := "ate.dev/pool=" + nodegroups.Nitro
	var bad []string
	noCapacity := false
	var lastErr, readErr error
	err := waitFor(ctx, "the nodes "+sel+" to advertise Nitro sandbox capacity", nitroWaitTimeout, func() (bool, error) {
		nodes, err := kube.Nodes(ctx, sel)
		if kubeTransient(err) {
			lastErr = err
			slog.Info("the Kubernetes API did not answer the node list; retrying", "error", err)
			return false, nil
		}
		if err != nil {
			readErr = err
			return false, err
		}
		lastErr = nil
		bad, noCapacity = nitroNodeProblems(nodes, int(want))
		return len(bad) == 0, nil
	})
	switch {
	case err == nil:
		slog.Info("the nitro nodes advertise Nitro sandbox capacity", "nodes", want)
		return nil
	case readErr != nil || ctx.Err() != nil:
		return err
	case lastErr != nil:
		return fmt.Errorf("%w; the last list of the nodes failed: %v", err, lastErr)
	case !noCapacity:
		return fmt.Errorf("%w (%s): the nitro node group's nodes have not all joined and become Ready; run create cluster again to wait for them",
			err, strings.Join(bad, "; "))
	}
	ami := r.cfg.NitroAMIID
	if r.nitroImage != nil {
		ami = r.nitroImage.ID
	}
	return fmt.Errorf("%w (%s): the nodes of AMI %s advertise no Nitro sandbox capacity; NITRO_AMI_ID must be a Nitro Sandbox AMI (%s)",
		err, strings.Join(bad, "; "), ami, r.nitroAMIHelp())
}
