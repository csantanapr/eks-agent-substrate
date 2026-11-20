package openclaw

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Both ways: with no gvisor node, the deploy warns that the worker pods
// will stay Pending, unless the gvisor pool has a Karpenter NodePool
// (o.PoolArchs), which makes a node for them; then it says so instead.
func TestPrerequisitesTakeAGVisorNodePool(t *testing.T) {
	prev := plugin.Exec
	t.Cleanup(func() { plugin.Exec = prev })
	plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
		if strings.Contains(strings.Join(c.Argv, " "), "get deployment ate-api-server") {
			io.WriteString(c.Stdout, "deployment.apps/ate-api-server")
		}
		return nil
	}
	o := plugin.Options{Kubeconfig: "/kc"}
	var out bytes.Buffer
	if err := prerequisites(context.Background(), &out, o); err != nil || !strings.Contains(out.String(), "WARNING: no nodes labeled ate.dev/pool=gvisor; the worker pods will stay Pending") {
		t.Errorf("no gvisor node: %v\n%s", err, out.String())
	}
	out.Reset()
	o.PoolArchs = map[string][]string{"gvisor": {"amd64"}}
	if err := prerequisites(context.Background(), &out, o); err != nil || strings.Contains(out.String(), "ate.dev/pool=gvisor; the worker pods will stay Pending") ||
		!strings.Contains(out.String(), "No node labeled ate.dev/pool=gvisor yet: Karpenter's gvisor NodePool makes one for the worker pods") {
		t.Errorf("a gvisor NodePool: %v\n%s", err, out.String())
	}
	out.Reset()
	o.PoolArchs = map[string][]string{"microvm": {"amd64"}}
	if err := prerequisites(context.Background(), &out, o); err != nil || !strings.Contains(out.String(), "WARNING: no nodes labeled ate.dev/pool=gvisor") {
		t.Errorf("a microvm NodePool only: %v\n%s", err, out.String())
	}
}
