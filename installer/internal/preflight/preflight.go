// Package preflight checks the tools, the AWS identity and the EC2 vCPU
// quota before an install. Every check is read-only.
package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Status of a check.
type Status int

const (
	Running Status = iota
	Pass
	Warn
	Fail
)

// Result of one check.
type Result struct {
	ID     string
	Group  string // "Tools", "AWS account", "Capacity"
	Name   string
	Status Status
	Detail string
	Fix    string // what to do when it fails
}

// Input is what the checks need from the settings.
type Input struct {
	Repo    string
	Profile string
	Region  string
	Cluster string
	// Node groups: instance type and desired count, plus the most nodes
	// each group may scale to (create-cluster.sh: +1 system, +2 others).
	Groups []NodeGroup
}

// NodeGroup is one managed node group the install creates.
type NodeGroup struct {
	Name  string
	Type  string
	Nodes int
	Max   int
}

// Check is one named check.
type Check struct {
	ID, Group, Name string
	Run             func(ctx context.Context, in Input) Result
}

// Runner runs a command; tests replace it.
type Runner func(ctx context.Context, dir string, name string, args ...string) (string, error)

// Exec is the default Runner: the command's stdout, or its stderr in the error.
func Exec(ctx context.Context, dir string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("%s", firstLine(msg))
	}
	return out.String(), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Checks returns every check, in display order.
func Checks(run Runner) []Check {
	tool := func(id, name, fix string, minimum string, args ...string) Check {
		return Check{ID: id, Group: "Tools", Name: name, Run: func(ctx context.Context, in Input) Result {
			r := Result{ID: id, Group: "Tools", Name: name, Fix: fix}
			if _, err := exec.LookPath(args[0]); err != nil {
				r.Status, r.Detail = Fail, "not on PATH"
				return r
			}
			out, err := run(ctx, in.Repo, args[0], args[1:]...)
			if err != nil {
				r.Status, r.Detail = Fail, err.Error()
				return r
			}
			v := version(id, out)
			r.Status, r.Detail = Pass, v
			if minimum != "" && v != "" && !atLeast(v, minimum) {
				r.Status, r.Detail = Fail, v+", needs "+minimum+" or newer"
			}
			return r
		}}
	}
	return []Check{
		tool("aws", "AWS CLI", "install AWS CLI v2", "2.0", "aws", "--version"),
		tool("kubectl", "kubectl", "install kubectl", "", "kubectl", "version", "--client", "-o", "json"),
		tool("eksctl", "eksctl", "install eksctl 0.230 or newer", "0.230", "eksctl", "version"),
		tool("helm", "helm", "install helm (demos/db-rotation)", "", "helm", "version", "--short"),
		tool("go", "Go", "install go 1.27 or newer (builds ate-setup)", "1.27", "go", "version"),
		tool("docker", "Docker", "start the Docker daemon (builds the counter image)", "", "docker", "version", "--format", "{{.Client.Version}} / engine {{.Server.Version}}"),
		tool("jq", "jq", "install jq", "", "jq", "--version"),
		tool("envsubst", "envsubst", "install GNU gettext", "", "envsubst", "--version"),
		tool("pyyaml", "python3 + PyYAML", "pip install pyyaml", "", "python3", "-c", "import yaml,sys; print(sys.version.split()[0], 'PyYAML', yaml.__version__)"),
		{ID: "identity", Group: "AWS account", Name: "Identity", Run: func(ctx context.Context, in Input) Result {
			r := Result{ID: "identity", Group: "AWS account", Name: "Identity", Fix: "sign in to the profile, e.g. aws sso login --profile " + in.Profile}
			out, err := run(ctx, in.Repo, "aws", awsArgs(in, "sts", "get-caller-identity", "--output", "json")...)
			if err != nil {
				r.Status, r.Detail = Fail, err.Error()
				return r
			}
			var id struct{ Account, Arn string }
			if err := json.Unmarshal([]byte(out), &id); err != nil {
				r.Status, r.Detail = Fail, "cannot read the STS reply"
				return r
			}
			r.Status, r.Detail = Pass, fmt.Sprintf("account %s, %s", id.Account, Role(id.Arn))
			return r
		}},
		{ID: "region", Group: "AWS account", Name: "Region", Run: func(ctx context.Context, in Input) Result {
			r := Result{ID: "region", Group: "AWS account", Name: "Region", Fix: "choose a Region with Amazon EKS and m8i instances"}
			out, err := run(ctx, in.Repo, "aws", awsArgs(in, "ec2", "describe-availability-zones",
				"--filters", "Name=state,Values=available", "--query", "AvailabilityZones[].ZoneName", "--output", "text")...)
			if err != nil {
				r.Status, r.Detail = Fail, err.Error()
				return r
			}
			r.Status, r.Detail = Pass, fmt.Sprintf("%s, %d Availability Zones", in.Region, len(strings.Fields(out)))
			return r
		}},
		{ID: "cluster", Group: "AWS account", Name: "Cluster name", Run: func(ctx context.Context, in Input) Result {
			r := Result{ID: "cluster", Group: "AWS account", Name: "Cluster name"}
			out, err := run(ctx, in.Repo, "aws", awsArgs(in, "eks", "describe-cluster", "--name", in.Cluster,
				"--query", "cluster.status", "--output", "text")...)
			switch {
			case err != nil && strings.Contains(err.Error(), "ResourceNotFound"):
				r.Status, r.Detail = Pass, in.Cluster+" is free; it will be created"
			case err != nil:
				r.Status, r.Detail = Fail, err.Error()
			default:
				r.Status, r.Detail = Warn, fmt.Sprintf("%s exists (%s); create-cluster.sh reuses it and adds what is missing", in.Cluster, strings.TrimSpace(out))
			}
			return r
		}},
		{ID: "nested-virt", Group: "Capacity", Name: "Nested virtualization", Run: func(ctx context.Context, in Input) Result {
			r := Result{ID: "nested-virt", Group: "Capacity", Name: "Nested virtualization", Fix: "pick a microVM instance type with nested virtualization (m8i, c8i, r8i)"}
			t := ""
			for _, g := range in.Groups {
				if g.Name == "microvm" {
					t = g.Type
				}
			}
			out, err := run(ctx, in.Repo, "aws", awsArgs(in, "ec2", "describe-instance-types", "--instance-types", t,
				"--query", "InstanceTypes[0].ProcessorInfo.SupportedFeatures", "--output", "text")...)
			switch {
			case err != nil:
				r.Status, r.Detail = Fail, err.Error()
			case strings.Contains(out, "nested-virtualization"):
				r.Status, r.Detail = Pass, t+" supports it (microVM node group)"
			default:
				r.Status, r.Detail = Fail, t+" does not support it"
			}
			return r
		}},
		{ID: "vcpu", Group: "Capacity", Name: "EC2 vCPU quota", Run: func(ctx context.Context, in Input) Result {
			return vcpu(ctx, run, in)
		}},
	}
}

func awsArgs(in Input, args ...string) []string {
	a := []string{"--region", in.Region}
	if in.Profile != "" {
		a = append(a, "--profile", in.Profile)
	}
	return append(a, args...)
}

// Role turns an STS ARN into "role Admin" or "user alice".
func Role(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 {
		return arn
	}
	res := strings.Split(parts[5], "/")
	switch res[0] {
	case "assumed-role":
		if len(res) > 1 {
			return "role " + res[1]
		}
	case "user":
		return "user " + res[len(res)-1]
	case "root":
		return "root user"
	}
	return parts[5]
}

var standardFamily = regexp.MustCompile(`^(a|c|d|h|i|m|r|t|z)[0-9]`)

// vcpu compares the Running On-Demand Standard instances quota
// (L-1216C47A) with what the node groups need and what already runs.
func vcpu(ctx context.Context, run Runner, in Input) Result {
	r := Result{ID: "vcpu", Group: "Capacity", Name: "EC2 vCPU quota", Fix: "request more Running On-Demand Standard instances vCPUs (L-1216C47A) in Service Quotas"}
	var types []string
	seen := map[string]bool{}
	for _, g := range in.Groups {
		if !seen[g.Type] {
			seen[g.Type] = true
			types = append(types, g.Type)
		}
	}
	out, err := run(ctx, in.Repo, "aws", awsArgs(in, append(append([]string{"ec2", "describe-instance-types", "--instance-types"}, types...),
		"--query", "InstanceTypes[].[InstanceType,VCpuInfo.DefaultVCpus]", "--output", "text")...)...)
	if err != nil {
		r.Status, r.Detail = Fail, err.Error()
		return r
	}
	per := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			per[f[0]], _ = strconv.Atoi(f[1])
		}
	}
	need, max := 0, 0
	for _, g := range in.Groups {
		need += g.Nodes * per[g.Type]
		max += g.Max * per[g.Type]
	}
	qout, err := run(ctx, in.Repo, "aws", awsArgs(in, "service-quotas", "get-service-quota", "--service-code", "ec2",
		"--quota-code", "L-1216C47A", "--query", "Quota.Value", "--output", "text")...)
	if err != nil {
		r.Status, r.Detail = Warn, fmt.Sprintf("needs %d vCPUs (up to %d); cannot read the quota: %s", need, max, err.Error())
		return r
	}
	quota, _ := strconv.ParseFloat(strings.TrimSpace(qout), 64)
	iout, err := run(ctx, in.Repo, "aws", awsArgs(in, "ec2", "describe-instances",
		"--filters", "Name=instance-state-name,Values=pending,running",
		"--query", "Reservations[].Instances[].[InstanceType,CpuOptions.CoreCount,CpuOptions.ThreadsPerCore]", "--output", "text")...)
	inUse := 0
	if err == nil {
		for _, l := range strings.Split(strings.TrimSpace(iout), "\n") {
			f := strings.Fields(l)
			if len(f) == 3 && standardFamily.MatchString(f[0]) {
				c, _ := strconv.Atoi(f[1])
				t, _ := strconv.Atoi(f[2])
				inUse += c * t
			}
		}
	}
	free := int(quota) - inUse
	r.Detail = fmt.Sprintf("needs %d vCPUs (up to %d), %d in use, quota %d", need, max, inUse, int(quota))
	switch {
	case need > free:
		r.Status = Fail
	case max > free:
		r.Status = Warn
		r.Detail += "; scaling to max would not fit"
	default:
		r.Status = Pass
	}
	return r
}

var verRe = regexp.MustCompile(`[0-9]+\.[0-9]+(\.[0-9]+)?`)

// version pulls the version out of a tool's version output.
func version(id, out string) string {
	out = strings.TrimSpace(out)
	switch id {
	case "kubectl":
		var v struct {
			ClientVersion struct{ GitVersion string } `json:"clientVersion"`
		}
		if json.Unmarshal([]byte(out), &v) == nil && v.ClientVersion.GitVersion != "" {
			return strings.TrimPrefix(v.ClientVersion.GitVersion, "v")
		}
	case "docker", "pyyaml":
		return firstLine(out)
	case "envsubst":
		l := firstLine(out)
		if !strings.Contains(l, "GNU") {
			return l + " (not GNU)"
		}
		return "GNU " + verRe.FindString(l)
	}
	return verRe.FindString(firstLine(out))
}

// atLeast compares dotted versions numerically.
func atLeast(have, want string) bool {
	h := strings.Split(verRe.FindString(have), ".")
	w := strings.Split(want, ".")
	for i := 0; i < len(w); i++ {
		hv := 0
		if i < len(h) {
			hv, _ = strconv.Atoi(h[i])
		}
		wv, _ := strconv.Atoi(w[i])
		if hv != wv {
			return hv > wv
		}
	}
	return true
}
