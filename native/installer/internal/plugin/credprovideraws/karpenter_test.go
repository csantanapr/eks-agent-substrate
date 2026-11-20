package credprovideraws

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Both ways: the provider and the probe are built for the system and gvisor
// nodes' architecture, and for a gvisor pool with a Karpenter NodePool and
// no node yet, the one its NodePool requires (o.PoolArchs). Two
// architectures, or none, are refused as before.
func TestNodeArchTakesANodelessNodePool(t *testing.T) {
	prev := plugin.Exec
	t.Cleanup(func() { plugin.Exec = prev })
	nodes := ""
	plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
		if want := "kubectl --kubeconfig /kc get nodes -l ate.dev/pool in (system,gvisor) -o jsonpath={.items[*].metadata.labels.kubernetes\\.io/arch}"; strings.Join(c.Argv, " ") != want {
			t.Fatalf("argv %q", c.Argv)
		}
		io.WriteString(c.Stdout, nodes)
		return nil
	}
	for _, c := range []struct {
		nodes string
		archs map[string][]string
		want  string
	}{
		{"amd64 amd64", nil, "amd64"},
		{"arm64", map[string][]string{"gvisor": {"arm64"}}, "arm64"},
		{"arm64", map[string][]string{"microvm": {"amd64"}}, "arm64"},
		{"arm64", map[string][]string{"gvisor": {"amd64"}}, "error: the nodes labeled ate.dev/pool in (system,gvisor) are amd64 and arm64"},
		{"", map[string][]string{"gvisor": {"amd64"}}, "amd64"},
		{"", nil, "error: no node labeled ate.dev/pool in (system,gvisor)"},
	} {
		nodes = c.nodes
		arch, err := nodeArch(context.Background(), io.Discard, plugin.Options{Kubeconfig: "/kc", PoolArchs: c.archs})
		got := arch
		if err != nil {
			got = "error: " + err.Error()
		}
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("nodes %q, NodePools %v: %q, want %q", c.nodes, c.archs, got, c.want)
		}
	}
}
