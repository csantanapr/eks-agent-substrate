package controlplane

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// A plugin that cannot run on Karpenter's nodes, on the microvm node group,
// as mode 3 is.
func init() {
	plugin.Register(plugin.Plugin{ID: "cp-test-kata", Order: 5, Nodes: []string{"microvm"},
		NoKarpenter: "needs a label kata-deploy sets on a running node"})
}

// The control plane's deploy refuses a plugin with NoKarpenter whose node
// group is a Karpenter NodePool, chosen or recorded, before the tree is
// fetched; it goes on when the NodePools are others' (KARPENTER=on's
// gvisor) or there are none. A failed read of the pools stops it, and a
// run without such a plugin does not read them.
func TestDeployRefusesNoKarpenterOnANodePool(t *testing.T) {
	microvmPool := plugin.Inventory{Nodes: []string{"system", "microvm"}, NodePools: map[string][]string{"microvm": {"amd64"}}, Karpenter: true}
	gvisorPool := plugin.Inventory{Nodes: []string{"system", "microvm"}, NodePools: map[string][]string{"gvisor": {"amd64"}}, Karpenter: true}
	for _, c := range []struct {
		name            string
		chosen, record  []string
		pools           plugin.Inventory
		poolsErr        error
		want            string
		read, refetched bool
	}{
		{"chosen, microvm is a NodePool", []string{"cp-test-kata"}, nil, microvmPool, nil,
			"plugin cp-test-kata needs a label kata-deploy sets on a running node; the microvm node group of demo is a Karpenter NodePool", true, false},
		{"recorded, microvm is a NodePool", nil, []string{"cp-test-kata"}, microvmPool, nil,
			"the microvm node group of demo is a Karpenter NodePool", true, false},
		{"gvisor is a NodePool", []string{"cp-test-kata"}, nil, gvisorPool, nil, "", true, true},
		{"no Karpenter", []string{"cp-test-kata"}, nil, plugin.Inventory{Nodes: []string{"system", "microvm"}}, nil, "", true, true},
		{"the read fails", []string{"cp-test-kata"}, nil, plugin.Inventory{}, errors.New("read the NodePools: Forbidden"), "Forbidden", true, false},
		{"no such plugin", []string{"cp-test-gate"}, nil, microvmPool, nil, "", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &deployFakes{record: c.record, pools: c.pools, poolsErr: c.poolsErr}
			o := deployEnv(t, f)
			o.Plugins = c.chosen
			err := Deploy(context.Background(), io.Discard, o)
			fetched := slices.ContainsFunc(f.calls, func(x string) bool { return strings.HasPrefix(x, "fetch") })
			if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) || fetched != c.refetched {
				t.Errorf("err %v, want %q; fetched %v; calls %v", err, c.want, fetched, f.calls)
			}
			if slices.Contains(f.calls, "pools") != c.read {
				t.Errorf("read the pools: %v, want %v (calls %v)", slices.Contains(f.calls, "pools"), c.read, f.calls)
			}
		})
	}
}
