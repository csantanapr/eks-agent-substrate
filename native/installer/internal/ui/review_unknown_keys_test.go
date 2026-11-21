package ui

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/probe"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// On a record-only cluster whose record holds a data key this installer
// does not know (a newer installer's), the control plane step refuses
// (Record.CheckKeys in controlplane.resolvePlugins), but only after Provision
// and RDS have run. Review has the record (Setup.ClusterRecord), so it
// refuses before anything runs, as it does a recorded NITRO_COMPUTE mismatch. The probe line comes from probe.Check on a fake kubectl, so
// the whole path is exercised: Check, the marker line, ParseInstalled,
// Result.Record, Review. Both ways: without the key, y starts.
func TestReviewRefusesARecordOnlyClusterWithUnknownKeys(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		data := `"plugins":"t-efs"`
		if unknown {
			data += `,"future-key":"from a newer installer"`
		}
		k := func(_ context.Context, args ...string) (string, error) {
			switch {
			case slices.Contains(args, "configmap"):
				return `{"metadata":{"name":"substrate-eks-plugins","namespace":"kube-system"},"data":{` + data + `}}`, nil
			case slices.Contains(args, "nodes"):
				return "gvisor microvm system", nil
			}
			return "", nil
		}
		line, err := probe.Check(context.Background(), k)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		r := &recorder{probes: map[string]string{"new": line}}
		s := newSetup("new")
		m := sized(toStep(t, New(testDeps(r), s), state.Review))
		v := strings.Join(strings.Fields(stripANSI(m.View())), " ")
		m = press(m, "y")
		ran := len(r.ran()) > 0
		const refusal = "kube-system/substrate-eks-plugins holds data keys this installer does not know (future-key)"
		if unknown && (ran || !strings.Contains(v, "Nothing can start: "+refusal[:30]) || !strings.Contains(m.notice, "nothing started: "+refusal)) {
			t.Errorf("record with future-key (probe line %q): y ran %q, at %s, notice %q:\n%s", line, r.ran(), m.m.Current().Title(), m.notice, v)
		}
		if !unknown && !ran {
			t.Errorf("record without the key: y ran nothing, at %s:\n%s", m.m.Current().Title(), v)
		}
	}
}
