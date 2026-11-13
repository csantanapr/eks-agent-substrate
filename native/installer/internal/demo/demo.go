// Package demo deploys the counter demo and checks it, the native form of
// eks-talk's demos/counter/deploy.sh and verify.sh, and the EKS counterpart of
// substrate-gke's Demo step (make run-demo).
//
// CONTRACT STUB (issue #5, T4b): the signatures and line constants below are
// what the wizard (main.go --deploy-demo / --verify) calls. The implementation
// replaces the bodies; it must not change the signatures without telling the
// lead.
package demo

import (
	"context"
	"errors"
	"io"
)

// Lines Deploy and Verify print for the wizard's checklist.
const (
	DeployLine     = "Deploying the counter demo on "
	VerifyLine     = "Checking suspend and resume on "
	VerifyPassLine = "PASS: "
)

// Options are the install answers the demo needs.
type Options struct {
	Cluster, Region, Profile string
	// Tag is the Substrate release; its tree (TreeRoot) holds demos/counter.
	Tag, TreeRoot string
	// ImageRepo is where the release worker images come from.
	ImageRepo string
	// SandboxClasses are the classes to deploy and check, e.g. "gvisor,microvm".
	SandboxClasses string
}

// ErrNotImplemented is returned by the stubs.
var ErrNotImplemented = errors.New("demo: not implemented yet")

// Deploy deploys the counter demo on every class in o.SandboxClasses.
func Deploy(ctx context.Context, w io.Writer, o Options) error { return ErrNotImplemented }

// Verify proves, on every class, that suspend and resume continue the
// memory, file and rootfs counters and that a snapshot exists in S3.
func Verify(ctx context.Context, w io.Writer, o Options) error { return ErrNotImplemented }
