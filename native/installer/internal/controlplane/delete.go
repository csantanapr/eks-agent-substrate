package controlplane

import (
	"context"
	"errors"
	"io"
)

// RemovedLine is what Delete prints once ate-system is gone.
const RemovedLine = "Substrate control plane removed"

// ErrDeleteNotImplemented is returned by the stub.
var ErrDeleteNotImplemented = errors.New("controlplane: Delete not implemented yet")

// Delete removes Substrate from the cluster, keeping the cluster: it runs
// `ate-setup delete ate-system` from the cached tree with the installer's own
// kubeconfig, waits for the ate-system namespace to be gone (so the bundled
// PostgreSQL volume and every claim are released before the AWS teardown
// runs), and removes the installer's kubeconfig.
//
// CONTRACT STUB (issue #5, T3): the wizard's teardown step calls this through
// main.go --remove-control-plane. Replace the body; keep the signature.
func Delete(ctx context.Context, w io.Writer, o Options) error { return ErrDeleteNotImplemented }
