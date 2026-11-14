package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// RemovedLine is what Delete prints once ate-system is gone.
const RemovedLine = "Substrate control plane removed"

// namespaceWait bounds the wait for ate-system to finish deleting.
const namespaceWait = "10m"

// Seams replaced in tests: the cluster read, and the child processes.
var (
	describeCluster = func(ctx context.Context, profile, region, name string) (awsx.Cluster, error) {
		c, err := awsx.New(ctx, profile, region)
		if err != nil {
			return awsx.Cluster{}, err
		}
		return c.Describe(ctx, name)
	}
	deleteRun = run
)

// Delete removes Substrate from the cluster, keeping the cluster: it runs
// `ate-setup delete ate-system` from the cached tree with the installer's own
// kubeconfig, waits for the ate-system namespace to be gone (so the bundled
// PostgreSQL volume and every claim are released before the AWS teardown
// runs), and removes the installer's kubeconfig.
//
// It is substrate-gke's snapshot.Builder.DeleteAteSystem with each line of
// its `bash -c` text as a Go call: every child starts from an argv. Like
// Deploy, the kubeconfig is the installer's own and KUBECTL_CONTEXT is
// blanked (AteSetupEnv), so the delete can only reach the cluster it names,
// never an ambient context. A cluster that no longer exists has no control
// plane left, which is done, not an error; any other failed read is an error.
func Delete(ctx context.Context, w io.Writer, o Options) error {
	if o.Cluster == "" || o.Region == "" || o.Tag == "" {
		return errors.New("cluster, region and Substrate tag are required")
	}
	kc := awsx.KubeconfigPath(o.Cluster, o.Region)
	cl, err := describeCluster(ctx, o.Profile, o.Region, o.Cluster)
	var nf *ekstypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		fmt.Fprintf(w, "cluster %s not found in %s; no control plane to remove\n", o.Cluster, o.Region)
		if err := removeKubeconfig(kc); err != nil {
			return err
		}
		fmt.Fprintln(w, RemovedLine)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(w, KubeconfLine+kc)
	if err := writeKubeconfig(kc, cl, o.Region, o.Profile); err != nil {
		return err
	}
	// The release the install ran (a recorded plugin may have replaced it),
	// read before the plugins are removed and their record with them.
	po := o.pluginOptions(kc, cl.AutoMode)
	o.Plugins = nil
	if _, err := resolvePlugins(ctx, w, &o, po); err != nil {
		return err
	}
	// Every recorded plugin goes first, Kubernetes objects then AWS
	// resources: their objects run on Substrate, and a volume they hold
	// would keep the VPC's subnets.
	if err := removePlugins(ctx, w, plugin.ActionRemove, nil, po); err != nil {
		return err
	}

	// The tree the install ran from, fetched again if the cache is gone.
	repo := o.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	tree := TreeDir(o.TreeRoot, o.Tag)
	if err := fetchTree(ctx, w, tree, repo, o.Tag); err != nil {
		return err
	}
	if err := applyOverlay(w, tree, o.Overlay, o.Region); err != nil {
		return err
	}

	del := []string{"go", "run", "./cmd/ate-setup", "delete", "ate-system", "--context", cl.Name}
	fmt.Fprintln(w, "$ "+strings.Join(del, " "))
	if err := deleteRun(ctx, w, tree, AteSetupEnv(o, kc), del...); err != nil {
		return err
	}
	// ate-setup can return while the namespace is still terminating; the AWS
	// teardown must not start until its volumes are released.
	wait := []string{"kubectl", "--context", cl.Name, "wait", "--for=delete", "namespace/ate-system", "--timeout=" + namespaceWait}
	fmt.Fprintln(w, "$ "+strings.Join(wait, " "))
	if err := deleteRun(ctx, w, "", []string{"KUBECONFIG=" + kc}, wait...); err != nil {
		return err
	}
	if err := removeKubeconfig(kc); err != nil {
		return err
	}
	fmt.Fprintln(w, RemovedLine)
	return nil
}

// removeKubeconfig removes the installer's kubeconfig: the install left it,
// and a later install writes a fresh one.
func removeKubeconfig(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
