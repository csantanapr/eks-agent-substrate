package probe

import (
	"slices"
	"testing"
)

// The marker line is split on whitespace, so a record value goes into it as
// one field. A nitro-compute value with a space (only a hand edit or a newer
// installer could write one) must not start a field of its own and override
// one written before it: here the pools, which would make a cluster with only
// a system node group read as having a nitro one.
func TestARecordValueWithASpaceStaysOneMarkerField(t *testing.T) {
	k := fakeKubectl(map[string]string{"nodes": "system",
		"configmap": `{"data":{"plugins":"efs","nitro-compute":"future pools=nitro,system"}}`}, nil)
	r := checkThenParse(t, k)
	if slices.Contains(r.Pools, "nitro") {
		t.Errorf("pools read as %q from nodes labeled system only; nitro-compute read as %q", r.Pools, r.NitroCompute)
	}
}
