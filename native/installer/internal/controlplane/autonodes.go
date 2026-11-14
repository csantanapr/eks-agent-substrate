package controlplane

// Tagged Auto Mode nodes. EKS Auto Mode's built-in NodePools,
// general-purpose and system, launch their nodes from the EKS-managed
// NodeClass default, which has no tags and which EKS reconciles, so their
// instances, root volumes and network interfaces carry neither the owner tag
// nor TAGS. On a cluster setup-aws created, whose role has the
// auto-mode-node-tags policy, the deploy gives Auto Mode tagged copies that
// outrank the built-ins:
//
//  1. NodeClass substrate-eks: default's spec plus spec.tags, the owner tag
//     and TAGS, without the keys a NodeClass may not set.
//  2. A wait for it to be Ready (a NodeClass Auto Mode cannot validate
//     launches no node).
//  3. NodePools substrate-general-purpose and substrate-system: copies of the
//     built-ins naming substrate-eks, with weight 10. Karpenter launches from
//     the highest-weight NodePool that fits, and the built-ins have none, so
//     every new node is tagged. The built-ins stay enabled: should
//     substrate-eks stop validating, Auto Mode falls back to them.
//  4. One replacement of the nodes the built-ins already launched: their
//     NodeClaims are deleted, Auto Mode drains them, and their Pods move to
//     tagged nodes. A rerun finds none and replaces nothing.
//
// It runs before the StorageClass and ate-setup, so the bundled PostgreSQL
// and every control plane Pod that is not cordoned onto the system node group
// start on tagged nodes. The NodeClass and NodePools are cluster
// infrastructure: --remove-control-plane leaves them, and deleting the
// cluster terminates their nodes.
//
//   https://docs.aws.amazon.com/eks/latest/userguide/create-node-class.html
//   https://docs.aws.amazon.com/eks/latest/userguide/create-node-pool.html
//   https://karpenter.sh/docs/concepts/scheduling/#weighted-nodepools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Names of the tagged NodeClass and NodePools, and the built-ins they copy.
const (
	AutoNodeClass  = "substrate-eks"
	autoNodeWeight = 10
	// AutoNodesSkipLine starts the line printed when the step does not run.
	AutoNodesSkipLine = "Auto Mode nodes stay on the built-in NodePools: "
)

// builtinPools are the built-in NodePools and the names of their copies.
var builtinPools = []struct{ from, to string }{
	{"general-purpose", "substrate-general-purpose"},
	{"system", "substrate-system"},
}

// reservedTagPrefixes are tag keys a NodeClass may not set.
var reservedTagPrefixes = []string{"kubernetes.io/cluster/", "karpenter.sh/", "eks:"}

// Polls at autoNodesPoll: the NodeClass's Ready wait (3 minutes) and the
// built-in nodes' replacement (kubectl wait, 15 minutes). Tests shorten the
// interval.
var autoNodesPoll = 2 * time.Second

const (
	nodeClassTries  = 90
	replaceDeadline = "--timeout=15m"
)

// autoNodes is the step install runs; tests of other steps replace it.
var autoNodes = applyAutoNodes

// applyAutoNodes is the step: only on an Auto Mode cluster this installer
// created (its role carries the policy the tags need); on any other it says
// why and does nothing.
func applyAutoNodes(ctx context.Context, w io.Writer, o Options, kc string) error {
	switch {
	case !o.AutoMode:
		fmt.Fprintln(w, AutoNodesSkipLine+"the cluster does not run EKS Auto Mode")
		return nil
	case !o.clusterOwned:
		fmt.Fprintln(w, AutoNodesSkipLine+"setup-aws did not create the cluster, so its role may not let Auto Mode tag nodes")
		return nil
	}
	kubectl := func(stdin []byte, stdout io.Writer, args ...string) error {
		return runCommand(ctx, w, command{Env: []string{"KUBECONFIG=" + kc}, Stdin: stdin, Stdout: stdout, Argv: append([]string{"kubectl"}, args...)})
	}
	read := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		err := kubectl(nil, &out, args...)
		return out.Bytes(), err
	}
	fmt.Fprintf(w, "%sNodeClass %s and NodePools %s, %s (tagged Auto Mode nodes)\n", PrereqLine, AutoNodeClass, builtinPools[0].to, builtinPools[1].to)

	src, err := read("get", "nodeclass", "default", "-o", "json")
	if err != nil {
		return err
	}
	nc, err := TaggedNodeClass(src, o.volumeTags)
	if err != nil {
		return err
	}
	if err := kubectl(nc, nil, "apply", "-f", "-"); err != nil {
		return err
	}
	if err := waitNodeClass(ctx, w, read); err != nil {
		return err
	}

	for _, p := range builtinPools {
		b, err := read("get", "nodepool", p.from, "-o", "json")
		if err != nil {
			return err
		}
		np, err := WeightedNodePool(b, p.to)
		if err != nil {
			return err
		}
		if err := kubectl(np, nil, "apply", "-f", "-"); err != nil {
			return err
		}
	}

	sel := "karpenter.sh/nodepool in (" + builtinPools[0].from + "," + builtinPools[1].from + ")"
	b, err := read("get", "nodeclaims", "-l", sel, "-o", "name")
	if err != nil {
		return err
	}
	claims := strings.Fields(string(b))
	if len(claims) == 0 {
		fmt.Fprintln(w, "No node of the built-in NodePools is left to replace")
		return nil
	}
	fmt.Fprintf(w, "Replacing %d node(s) of the built-in NodePools with tagged ones: %s\n", len(claims), strings.Join(claims, ", "))
	if err := kubectl(nil, nil, append(append([]string{"delete"}, claims...), "--wait=false")...); err != nil {
		return err
	}
	return kubectl(nil, nil, append(append([]string{"wait", "--for=delete"}, claims...), replaceDeadline)...)
}

// waitNodeClass waits for NodeClass substrate-eks to be Ready.
func waitNodeClass(ctx context.Context, w io.Writer, read func(...string) ([]byte, error)) error {
	why := "no status yet"
	for i := 0; i < nodeClassTries; i++ {
		b, err := read("get", "nodeclass", AutoNodeClass, "-o", "json")
		if err != nil {
			return err
		}
		ok, msg, err := NodeClassReady(b)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintln(w, "NodeClass "+AutoNodeClass+" is Ready")
			return nil
		}
		if msg != "" {
			why = msg
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(autoNodesPoll):
		}
	}
	return fmt.Errorf("NodeClass %s is not Ready (%s); run `setup-aws create cluster` again: it gives the cluster role the auto-mode-node-tags policy tagged NodeClasses need", AutoNodeClass, why)
}

// NodeClassReady reads `kubectl get nodeclass -o json`: whether it is Ready,
// and else the first condition that is False, as type: reason: message.
func NodeClassReady(b []byte) (bool, string, error) {
	var nc struct {
		Status struct {
			Conditions []struct{ Type, Status, Reason, Message string } `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &nc); err != nil {
		return false, "", fmt.Errorf("read the NodeClass: %w", err)
	}
	why := ""
	for _, c := range nc.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true, "", nil
		}
		if c.Status == "False" && c.Type != "Ready" && why == "" {
			why = c.Type + ": " + c.Reason + ": " + c.Message
		}
	}
	return false, why, nil
}

type kobj = map[string]any

// TaggedNodeClass is NodeClass substrate-eks: the spec of source (`kubectl
// get nodeclass default -o json`), its node role, subnets and security
// groups, with tags (the owner tag and TAGS) merged into spec.tags. Keys a
// NodeClass may not set are dropped from both.
func TaggedNodeClass(source []byte, tags map[string]string) ([]byte, error) {
	var nc kobj
	if err := json.Unmarshal(source, &nc); err != nil {
		return nil, fmt.Errorf("read NodeClass default: %w", err)
	}
	if nc["kind"] != "NodeClass" {
		return nil, fmt.Errorf("NodeClass default is a %v", nc["kind"])
	}
	spec, ok := nc["spec"].(kobj)
	if !ok {
		return nil, errors.New("NodeClass default has no spec")
	}
	t := kobj{}
	if have, ok := spec["tags"].(kobj); ok {
		t = have
	} else if spec["tags"] != nil {
		return nil, fmt.Errorf("NodeClass default's tags are %T", spec["tags"])
	}
	for k, v := range tags {
		t[k] = v
	}
	for k := range t {
		if reservedTag(k) {
			delete(t, k)
		}
	}
	spec["tags"] = t
	return json.Marshal(kobj{"apiVersion": nc["apiVersion"], "kind": "NodeClass",
		"metadata": kobj{"name": AutoNodeClass, "labels": kobj{"app.kubernetes.io/managed-by": "substrate-eks"}}, "spec": spec})
}

func reservedTag(k string) bool {
	for _, p := range reservedTagPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// WeightedNodePool is NodePool name: the spec of a built-in NodePool
// (`kubectl get nodepool <built-in> -o json`), with its requirements, taints
// (system's CriticalAddonsOnly) and disruption, naming NodeClass
// substrate-eks and weighted 10. It adds no label or taint: in particular no
// ate.dev/workloadType, which would draw the cordoned control plane.
func WeightedNodePool(source []byte, name string) ([]byte, error) {
	var np kobj
	if err := json.Unmarshal(source, &np); err != nil {
		return nil, fmt.Errorf("read the built-in NodePool: %w", err)
	}
	if np["kind"] != "NodePool" {
		return nil, fmt.Errorf("the built-in NodePool is a %v", np["kind"])
	}
	spec, ok := np["spec"].(kobj)
	if !ok {
		return nil, errors.New("the built-in NodePool has no spec")
	}
	tmpl, ok := spec["template"].(kobj)
	if !ok {
		return nil, errors.New("the built-in NodePool has no template")
	}
	ts, ok := tmpl["spec"].(kobj)
	if !ok {
		return nil, errors.New("the built-in NodePool has no template spec")
	}
	ref, ok := ts["nodeClassRef"].(kobj)
	if !ok {
		return nil, errors.New("the built-in NodePool names no NodeClass")
	}
	ref["name"] = AutoNodeClass
	spec["weight"] = autoNodeWeight
	return json.Marshal(kobj{"apiVersion": np["apiVersion"], "kind": "NodePool",
		"metadata": kobj{"name": name, "labels": kobj{"app.kubernetes.io/managed-by": "substrate-eks"}}, "spec": spec})
}
