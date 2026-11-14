package mode3iso

import (
	"encoding/json"
	"errors"
	"fmt"
)

// obj is a Kubernetes object as `kubectl get -o json` prints it.
type obj = map[string]any

// child returns m[key] as an object, making it when missing; an existing
// value of another type is an error.
func child(m obj, key string) (obj, error) {
	v, ok := m[key]
	if !ok || v == nil {
		c := obj{}
		m[key] = c
		return c, nil
	}
	c, ok := v.(obj)
	if !ok {
		return nil, fmt.Errorf("%s is %T, not an object", key, v)
	}
	return c, nil
}

// path walks child through keys.
func path(m obj, keys ...string) (obj, error) {
	var err error
	for _, k := range keys {
		if m, err = child(m, k); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// NodePool is demos/mode3-iso/deploy.sh's NodePool ate-isolated, made from
// Auto Mode's built-in general-purpose NodePool (`kubectl get nodepool
// general-purpose -o json`): the same node class and requirements, its nodes
// labeled ate.dev/pool=isolated and tainted ate.dev/sandboxClass=isolated, so
// only the worker Pods and atelet-iso run there. An empty node is kept a few
// minutes, so a resume rarely waits for a new one.
func NodePool(generalPurpose []byte) ([]byte, error) {
	var gp obj
	if err := json.Unmarshal(generalPurpose, &gp); err != nil {
		return nil, fmt.Errorf("read NodePool general-purpose: %w", err)
	}
	if gp["kind"] != "NodePool" {
		return nil, fmt.Errorf("general-purpose is a %v, not a NodePool", gp["kind"])
	}
	spec, ok := gp["spec"].(obj)
	if !ok {
		return nil, errors.New("NodePool general-purpose has no spec")
	}
	labels, err := path(spec, "template", "metadata", "labels")
	if err != nil {
		return nil, fmt.Errorf("NodePool general-purpose: %w", err)
	}
	labels[PoolLabel] = PoolValue
	tspec, err := path(spec, "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("NodePool general-purpose: %w", err)
	}
	tspec["taints"] = []any{obj{"key": TaintKey, "value": PoolValue, "effect": "NoSchedule"}}
	spec["disruption"] = obj{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "300s", "budgets": []any{obj{"nodes": "10%"}}}
	return json.Marshal(obj{"apiVersion": gp["apiVersion"], "kind": "NodePool",
		"metadata": obj{"name": NodePoolName}, "spec": spec})
}

// The atelet volumes that hold the kubelet's image credential provider: its
// binaries and its config. deploy/eks points them at the AL2023 paths.
const (
	providerBin    = "image-credential-provider-bin"
	providerConfig = "image-credential-provider-config"
)

// AteletIso is demos/mode3-iso/deploy.sh's atelet-iso, made from the atelet
// DaemonSet (`kubectl -n ate-system get ds atelet -o json`): the same Pods,
// labeled ate.dev/atelet-variant=iso and kept app=atelet (so the ate-system
// NetworkPolicies cover them), on the isolated nodes only, with the
// Bottlerocket path of the ECR credential provider and the provider config of
// ConfigMap atelet-iso-credential-provider. It is a copy, so a Substrate
// upgrade that changes atelet needs the plugin deployed again.
func AteletIso(atelet []byte) ([]byte, error) {
	var d obj
	if err := json.Unmarshal(atelet, &d); err != nil {
		return nil, fmt.Errorf("read DaemonSet atelet: %w", err)
	}
	if d["kind"] != "DaemonSet" {
		return nil, fmt.Errorf("atelet is a %v, not a DaemonSet", d["kind"])
	}
	m, ok := d["metadata"].(obj)
	if !ok {
		return nil, errors.New("DaemonSet atelet has no metadata")
	}
	meta := obj{"name": AteletName, "namespace": m["namespace"]}
	if l, ok := m["labels"]; ok {
		meta["labels"] = l
	}
	d["metadata"] = meta
	delete(d, "status")
	spec, ok := d["spec"].(obj)
	if !ok {
		return nil, errors.New("DaemonSet atelet has no spec")
	}
	sel, err := path(spec, "selector", "matchLabels")
	if err != nil {
		return nil, fmt.Errorf("DaemonSet atelet: %w", err)
	}
	sel[VariantLabel] = "iso"
	tl, err := path(spec, "template", "metadata", "labels")
	if err != nil {
		return nil, fmt.Errorf("DaemonSet atelet: %w", err)
	}
	tl[VariantLabel] = "iso"
	p, err := path(spec, "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("DaemonSet atelet: %w", err)
	}
	p["affinity"] = obj{"nodeAffinity": obj{"requiredDuringSchedulingIgnoredDuringExecution": obj{"nodeSelectorTerms": []any{
		obj{"matchExpressions": []any{obj{"key": PoolLabel, "operator": "In", "values": []any{PoolValue}}}}}}}}
	vols, _ := p["volumes"].([]any)
	found := map[string]bool{}
	for _, v := range vols {
		vol, ok := v.(obj)
		if !ok {
			continue
		}
		switch vol["name"] {
		case providerBin:
			vol["hostPath"] = obj{"path": "/usr/libexec/kubernetes/kubelet/plugins", "type": "Directory"}
			found[providerBin] = true
		case providerConfig:
			delete(vol, "hostPath")
			vol["configMap"] = obj{"name": CredentialProviderConfigMap}
			found[providerConfig] = true
		}
	}
	if !found[providerBin] || !found[providerConfig] {
		return nil, fmt.Errorf("DaemonSet atelet has no %s and %s volumes (found %v)", providerBin, providerConfig, found)
	}
	cs, _ := p["containers"].([]any)
	for _, c := range cs {
		ct, _ := c.(obj)
		mounts, _ := ct["volumeMounts"].([]any)
		for _, vm := range mounts {
			if mt, ok := vm.(obj); ok && mt["name"] == providerConfig {
				mt["subPath"] = "config.yaml"
			}
		}
	}
	return json.Marshal(d)
}
