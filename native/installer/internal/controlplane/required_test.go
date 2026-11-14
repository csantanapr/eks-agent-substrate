package controlplane

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// An empty asset bucket is refused before anything runs; with it set, the
// same deploy reaches ate-setup.
func TestDeployRequiresTheAssetBucket(t *testing.T) {
	f := &deployFakes{}
	o := deployEnv(t, f)
	o.AssetBucket = ""
	describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
		f.calls = append(f.calls, "describe")
		return awsx.Cluster{}, errors.New("describe")
	}
	if err := Deploy(context.Background(), io.Discard, o); err == nil || !strings.Contains(err.Error(), "asset bucket") {
		t.Fatalf("want an asset bucket refusal, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %q without an asset bucket", f.calls)
	}

	f = &deployFakes{}
	o = deployEnv(t, f)
	if err := Deploy(context.Background(), io.Discard, o); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls); n == 0 || !strings.HasPrefix(f.calls[n-1], "ate-setup ") || !strings.Contains(f.calls[n-1], "--storage-bucket-name") {
		t.Errorf("calls %q, want ate-setup last", f.calls)
	}
}
