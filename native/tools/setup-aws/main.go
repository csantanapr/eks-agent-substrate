// setup-aws provisions the AWS resources Agent Substrate uses on Amazon EKS.
// It is the AWS counterpart of substrate-gke's tools/setup-gcp: a Go program
// with idempotent subcommands that call the AWS APIs through the AWS SDK for
// Go, with no shell scripts and no aws CLI.
package main

import (
	"os"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/cmd"
)

func main() {
	// cobra has printed the error ("Error: ..."); exit non-zero only.
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
