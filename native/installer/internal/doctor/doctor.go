// Package doctor is the preflight check, substrate-gke's installer/internal/doctor
// for AWS: each Check probes one prerequisite and names a fix.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// Status of one check.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

// Result is a check's outcome.
type Result struct {
	Status Status
	Detail string
	Fix    string
}

// Check is one prerequisite.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

func tool(name, fix string, args ...string) Check {
	return Check{Name: name, Run: func(ctx context.Context) Result {
		path, err := exec.LookPath(name)
		if err != nil {
			return Result{Fail, "not found on PATH", fix}
		}
		c := exec.CommandContext(ctx, path, args...)
		// The install runs go with GOTOOLCHAIN=local, so probe it that way: an
		// automatic toolchain download is not what the install will get.
		c.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		out, err := c.CombinedOutput()
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if err != nil {
			return Result{Fail, line, fix}
		}
		if strings.Contains(line, "downloading") {
			return Result{Warn, line, "a toolchain download was attempted; " + fix}
		}
		return Result{OK, line, ""}
	}}
}

// Checks are the preflight checks for profile and region.
//
// aws is still needed at run time, but only by kubectl's exec plugin (`aws eks
// get-token`) and by ate-setup's microVM staging (the EKS code patch); the
// installer and setup-aws reach AWS through the SDK.
func Checks(profile, region string) []Check {
	return []Check{
		{Name: "AWS credentials", Run: func(ctx context.Context) Result {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			c, err := awsx.New(ctx, profile, region)
			if err != nil {
				return Result{Fail, err.Error(), "configure a profile: aws configure sso, or pass --profile"}
			}
			_, arn, err := c.Identity(ctx)
			if err != nil {
				return Result{Fail, err.Error(), "refresh credentials for profile " + profile}
			}
			return Result{OK, MaskArn(arn), ""}
		}},
		tool("go", "install Go 1.27 or later: https://go.dev/doc/install", "version"),
		tool("git", "install git", "--version"),
		tool("kubectl", "install kubectl: https://kubernetes.io/docs/tasks/tools/", "version", "--client=true"),
		tool("aws", "install AWS CLI v2 (kubectl's token plugin and microVM staging use it): https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html", "--version"),
	}
}

// MaskArn hides all but the last four digits of the account in an ARN, so a
// screenshot of the doctor or the wizard never carries an account id.
func MaskArn(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) > 4 && len(parts[4]) > 4 {
		parts[4] = strings.Repeat("•", len(parts[4])-4) + parts[4][len(parts[4])-4:]
	}
	return strings.Join(parts, ":")
}

// RunCLI prints every check for --doctor and returns how many failed.
func RunCLI(ctx context.Context, checks []Check) int {
	fatal := 0
	for _, c := range checks {
		r := c.Run(ctx)
		mark := "✓"
		switch r.Status {
		case Warn:
			mark = "!"
		case Fail:
			mark = "✗"
			fatal++
		}
		fmt.Printf("  %s %-16s %s\n", mark, c.Name, r.Detail)
		if r.Fix != "" {
			fmt.Printf("      fix: %s\n", r.Fix)
		}
	}
	return fatal
}
