package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	cases := map[string][2]string{
		"aws":      {"aws-cli/2.38.0 Python/3.13 Linux/6 exe/x86_64", "2.38.0"},
		"kubectl":  {`{"clientVersion":{"gitVersion":"v1.35.0"}}`, "1.35.0"},
		"eksctl":   {"0.230.0", "0.230.0"},
		"go":       {"go version go1.27.2 linux/amd64", "1.27.2"},
		"envsubst": {"envsubst (GNU gettext-runtime) 0.21\nCopyright", "GNU 0.21"},
	}
	for id, c := range cases {
		if got := version(id, c[0]); got != c[1] {
			t.Errorf("version(%s) = %q, want %q", id, got, c[1])
		}
	}
	if v := version("envsubst", "envsubst 1.4 (BSD)"); !strings.Contains(v, "not GNU") {
		t.Errorf("BSD envsubst: %q", v)
	}
}

func TestAtLeast(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{{"0.230.0", "0.230", true}, {"0.229.1", "0.230", false}, {"1.27.2", "1.27", true}, {"1.26.9", "1.27", false}, {"2.0", "2.0", true}} {
		if atLeast(c.have, c.want) != c.ok {
			t.Errorf("atLeast(%s, %s) != %v", c.have, c.want, c.ok)
		}
	}
}

func TestRole(t *testing.T) {
	if r := Role("arn:aws:sts::111122223333:assumed-role/Admin/alice"); r != "role Admin" {
		t.Error(r)
	}
	if r := Role("arn:aws:iam::111122223333:user/dev/bob"); r != "user bob" {
		t.Error(r)
	}
}

// fake answers aws calls by their subcommand.
func fake(replies map[string]string) Runner {
	return func(_ context.Context, _ string, name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		for k, v := range replies {
			if strings.Contains(joined, k) {
				if strings.HasPrefix(v, "ERR ") {
					return "", errors.New(v[4:])
				}
				return v, nil
			}
		}
		return "", errors.New("unexpected: " + name + " " + joined)
	}
}

func input() Input {
	return Input{Region: "us-west-2", Profile: "p", Cluster: "substrate-tui", Groups: []NodeGroup{
		{Name: "system", Type: "m8i.xlarge", Nodes: 2, Max: 3},
		{Name: "gvisor", Type: "m8i.2xlarge", Nodes: 2, Max: 4},
		{Name: "microvm", Type: "m8i.2xlarge", Nodes: 2, Max: 4},
	}}
}

func TestVCPU(t *testing.T) {
	types := "m8i.xlarge\t4\nm8i.2xlarge\t8\n"
	run := fake(map[string]string{
		"describe-instance-types": types,
		"get-service-quota":       "80.0\n",
		"describe-instances":      "m8i.2xlarge\t4\t2\nm7i.large\t1\t2\ng5.xlarge\t2\t2\n",
	})
	r := vcpu(context.Background(), run, input())
	// needs 2*4 + 2*8 + 2*8 = 40, up to 3*4 + 4*8 + 4*8 = 76; in use 8 + 2 (g5 is not a standard family)
	if r.Status != Warn || r.Detail != "needs 40 vCPUs (up to 76), 10 in use, quota 80; scaling to max would not fit" {
		t.Errorf("%v %q", r.Status, r.Detail)
	}
	run = fake(map[string]string{"describe-instance-types": types, "get-service-quota": "32.0", "describe-instances": ""})
	if r := vcpu(context.Background(), run, input()); r.Status != Fail {
		t.Errorf("40 needed with a quota of 32 must fail: %v %q", r.Status, r.Detail)
	}
	run = fake(map[string]string{"describe-instance-types": types, "get-service-quota": "1152.0", "describe-instances": ""})
	if r := vcpu(context.Background(), run, input()); r.Status != Pass {
		t.Errorf("%v %q", r.Status, r.Detail)
	}
}

func byID(run Runner, id string) Check {
	for _, c := range Checks(run) {
		if c.ID == id {
			return c
		}
	}
	panic("no check " + id)
}

func TestClusterCheck(t *testing.T) {
	free := fake(map[string]string{"describe-cluster": "ERR An error occurred (ResourceNotFoundException)"})
	if r := byID(free, "cluster").Run(context.Background(), input()); r.Status != Pass {
		t.Errorf("free name: %v %q", r.Status, r.Detail)
	}
	exists := fake(map[string]string{"describe-cluster": "ACTIVE\n"})
	if r := byID(exists, "cluster").Run(context.Background(), input()); r.Status != Warn || !strings.Contains(r.Detail, "ACTIVE") {
		t.Errorf("existing cluster: %v %q", r.Status, r.Detail)
	}
	denied := fake(map[string]string{"describe-cluster": "ERR AccessDenied"})
	if r := byID(denied, "cluster").Run(context.Background(), input()); r.Status != Fail {
		t.Errorf("a denied read is not 'free': %v %q", r.Status, r.Detail)
	}
}

func TestIdentity(t *testing.T) {
	run := fake(map[string]string{"get-caller-identity": `{"Account":"111122223333","Arn":"arn:aws:sts::111122223333:assumed-role/Admin/x"}`})
	r := byID(run, "identity").Run(context.Background(), input())
	if r.Status != Pass || r.Detail != "account 111122223333, role Admin" {
		t.Errorf("%v %q", r.Status, r.Detail)
	}
}
