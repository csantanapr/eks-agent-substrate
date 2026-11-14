package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

type deleteCall struct {
	dir  string
	env  []string
	argv []string
}

// deleteEnv gives Delete a cached tree, a private cache dir for the
// kubeconfig, a stub cluster read and a recording runner.
func deleteEnv(t *testing.T, describeErr error, runErr func(argv []string) error) (o Options, calls *[]deleteCall, kc string) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	o = opts()
	o.TreeRoot = t.TempDir()
	tree := TreeDir(o.TreeRoot, o.Tag)
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, completeMarker), []byte(o.Tag+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kc = awsx.KubeconfigPath(o.Cluster, o.Region)
	prevDescribe, prevRun := describeCluster, deleteRun
	prevRead, prevRemove, prevOverlay := readRecord, removePlugins, applyOverlay
	t.Cleanup(func() {
		describeCluster, deleteRun = prevDescribe, prevRun
		readRecord, removePlugins, applyOverlay = prevRead, prevRemove, prevOverlay
	})
	readRecord = func(context.Context, io.Writer, plugin.Options) ([]string, error) { return nil, nil }
	removePlugins = func(_ context.Context, _ io.Writer, action string, ids []string, _ plugin.Options) error {
		*calls = append(*calls, deleteCall{argv: append([]string{"plugins", action}, ids...)})
		return nil
	}
	applyOverlay = func(io.Writer, string, string, string) error { return nil }
	describeCluster = func(context.Context, string, string, string) (awsx.Cluster, error) {
		if describeErr != nil {
			return awsx.Cluster{}, describeErr
		}
		return awsx.Cluster{Name: "demo", Status: "ACTIVE", Endpoint: "https://example.invalid",
			CA: base64.StdEncoding.EncodeToString([]byte("ca"))}, nil
	}
	calls = &[]deleteCall{}
	deleteRun = func(_ context.Context, _ io.Writer, dir string, env []string, argv ...string) error {
		// The kubeconfig is written, private, before any child runs.
		if st, err := os.Stat(kc); err != nil || st.Mode().Perm() != 0o600 {
			return fmt.Errorf("kubeconfig not ready when %v ran: %v", argv, err)
		}
		*calls = append(*calls, deleteCall{dir, env, argv})
		if runErr != nil {
			return runErr(argv)
		}
		return nil
	}
	return o, calls, kc
}

func TestDeleteRunsAteSetupWaitsAndRemovesTheKubeconfig(t *testing.T) {
	o, calls, kc := deleteEnv(t, nil, nil)
	var out bytes.Buffer
	if err := Delete(context.Background(), &out, o); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 {
		t.Fatalf("want the plugin removal, ate-setup, then kubectl wait, got %v", *calls)
	}
	// Every recorded plugin is removed (no IDs: all of them) before ate-system.
	if want := []string{"plugins", plugin.ActionRemove}; !slices.Equal((*calls)[0].argv, want) {
		t.Errorf("first call = %v, want %v", (*calls)[0].argv, want)
	}
	del, wait := (*calls)[1], (*calls)[2]
	if want := []string{"go", "run", "./cmd/ate-setup", "delete", "ate-system", "--context", "demo"}; !slices.Equal(del.argv, want) {
		t.Errorf("delete argv = %v, want %v", del.argv, want)
	}
	if del.dir != TreeDir(o.TreeRoot, o.Tag) {
		t.Errorf("ate-setup ran in %q, not the cached tree", del.dir)
	}
	for _, want := range []string{"KUBECONFIG=" + kc, "KUBECTL_CONTEXT=", "NO_DEV_ENV=1", "ATE_INSTALL_STORAGE_BACKEND=s3", "AWS_PROFILE=p"} {
		if !slices.Contains(del.env, want) {
			t.Errorf("ate-setup env lacks %s", want)
		}
	}
	if want := []string{"kubectl", "--context", "demo", "wait", "--for=delete", "namespace/ate-system", "--timeout=10m"}; !slices.Equal(wait.argv, want) {
		t.Errorf("wait argv = %v, want %v", wait.argv, want)
	}
	if !slices.Contains(wait.env, "KUBECONFIG="+kc) {
		t.Errorf("kubectl wait env lacks the installer's kubeconfig: %v", wait.env)
	}
	for _, c := range *calls {
		if c.argv[0] == "bash" || c.argv[0] == "sh" || slices.Contains(c.argv, "-c") {
			t.Errorf("a shell ran: %v", c.argv)
		}
	}
	if _, err := os.Stat(kc); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("kubeconfig %s is still there (%v)", kc, err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), RemovedLine) {
		t.Errorf("output does not end with %q:\n%s", RemovedLine, out.String())
	}
}

// A failed delete keeps the kubeconfig for the retry and claims nothing.
func TestDeleteFailureStopsBeforeTheWait(t *testing.T) {
	o, calls, kc := deleteEnv(t, nil, func(argv []string) error {
		if argv[0] == "go" {
			return errors.New("exit status 1")
		}
		return nil
	})
	var out bytes.Buffer
	if err := Delete(context.Background(), &out, o); err == nil {
		t.Fatal("want the ate-setup error")
	}
	if len(*calls) != 2 {
		t.Errorf("want the plugin removal then the failed delete only, got %v", *calls)
	}
	if _, err := os.Stat(kc); err != nil {
		t.Errorf("kubeconfig removed after a failed delete: %v", err)
	}
	if strings.Contains(out.String(), RemovedLine) {
		t.Errorf("printed %q after a failure", RemovedLine)
	}
}

// Both directions: a cluster that is gone has nothing left to remove; a
// denied read is an error, never "gone".
func TestDeleteOfAClusterThatIsGone(t *testing.T) {
	gone := fmt.Errorf("describe demo: %w", &ekstypes.ResourceNotFoundException{Message: new(string)})
	o, calls, _ := deleteEnv(t, gone, nil)
	var out bytes.Buffer
	if err := Delete(context.Background(), &out, o); err != nil {
		t.Fatalf("gone cluster: %v", err)
	}
	if len(*calls) != 0 || !strings.Contains(out.String(), RemovedLine) {
		t.Errorf("gone cluster: calls %v, output %q", *calls, out.String())
	}

	o, calls, _ = deleteEnv(t, errors.New("describe demo: api error AccessDeniedException"), nil)
	out.Reset()
	if err := Delete(context.Background(), &out, o); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("denied read: want an error, got %v", err)
	}
	if len(*calls) != 0 || strings.Contains(out.String(), RemovedLine) {
		t.Errorf("denied read: calls %v, output %q", *calls, out.String())
	}
}
