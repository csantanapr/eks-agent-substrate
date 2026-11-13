// setup-aws provisions the AWS resources Agent Substrate uses on Amazon EKS.
// It is the AWS counterpart of substrate-gke's tools/setup-gcp: a Go program
// with idempotent subcommands that call the AWS APIs through the AWS SDK for
// Go, with no shell scripts and no aws CLI.
package main

import (
	"fmt"
	"os"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
