package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
)

// --nitro-compute reaches the plugin: on a cluster without Auto Mode,
// --plugin-action verify --plugins mode3-iso is refused for Auto Mode by
// default and with auto-mode, and gets past that gate with node-group (it
// stops later, at the missing tree); a value that is not one, from the flag
// or NITRO_COMPUTE, stops the run before any cluster read.
func TestNitroComputeFlag(t *testing.T) {
	for _, tc := range []struct {
		args, env []string
		want, not string
	}{
		{nil, nil, "plugin mode3-iso needs EKS Auto Mode", "no mode 3 tree"},
		{[]string{"--nitro-compute", "auto-mode"}, nil, "demo has EKS Auto Mode off", "no mode 3 tree"},
		{[]string{"--nitro-compute", "node-group"}, nil, "no mode 3 tree", "Auto Mode off"},
		{nil, []string{"NITRO_COMPUTE=node-group"}, "no mode 3 tree", "Auto Mode off"},
		{[]string{"--nitro-compute", "nodegroup"}, nil, `NITRO_COMPUTE="nodegroup" is not auto-mode or node-group`, "kubeconfig"},
		{nil, []string{"NITRO_COMPUTE=bogus"}, `NITRO_COMPUTE="bogus" is not auto-mode or node-group`, "kubeconfig"},
	} {
		args := append([]string{"--plugin-action", "verify", "--plugins", "mode3-iso"}, tc.args...)
		env := append([]string{"NITRO_COMPUTE="}, tc.env...)
		err, out := runMainIn(t, "", env, cluster("", "v1.0.0"), args...)
		if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
			t.Errorf("%v %v: %v, want %q and not %q:\n%s", tc.args, tc.env, err, tc.want, tc.not, out)
		}
	}
}

// Through main(), both removal commands with no --nitro-compute (and
// NITRO_COMPUTE unset, so the run's is auto-mode) remove mode3-iso with the
// NITRO_COMPUTE its record holds, and say so; with --nitro-compute
// node-group, the same as the record, they say nothing, and so they do on a
// record an older installer wrote, with none. Each stops after that, at the
// fork's tree this test does not fetch.
func TestCLIRemovalFollowsTheRecordedNitroCompute(t *testing.T) {
	recorded := `substrate-eks-plugins={"data":{"plugins":"mode3-iso","nitro-compute":"node-group"}}` + "\n"
	older := `substrate-eks-plugins={"data":{"plugins":"mode3-iso"}}` + "\n"
	const says = "NITRO_COMPUTE recorded at deploy: mode3-iso was deployed with NITRO_COMPUTE=node-group, as kube-system/substrate-eks-plugins records, so it is removed with node-group, not the run's auto-mode"
	for _, cmd := range [][]string{{"--plugin-action", "remove"}, {"--remove-control-plane"}} {
		for _, tc := range []struct {
			record string
			args   []string
			want   bool
		}{
			{recorded, nil, true},
			{recorded, []string{"--nitro-compute", "node-group"}, false},
			{older, nil, false},
		} {
			err, out := runMainIn(t, "", []string{"NITRO_COMPUTE="}, tc.record+cluster("", mode3.Tag), append(slices.Clone(cmd), tc.args...)...)
			if err == nil || strings.Contains(out, says) != tc.want || !strings.Contains(out, "Removing plugin mode3-iso") || !strings.Contains(out, "no mode 3 tree") {
				t.Errorf("%v %v, record %q: %v, says it %v:\n%s", cmd, tc.args, tc.record, err, !tc.want, out)
			}
		}
	}
}
