package efs

// Helpers the efs plugin shares with s3files and openclaw, which require it:
// reads through kubectl and the AWS CLI, kubectl-ate, Pod Identity restarts
// and StorageClass replacement. Every child goes through plugin.Exec, from
// an argv, against the installer's own kubeconfig.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Wait paces every poll of the efs, s3files and openclaw plugins; tests set
// it to 0.
var Wait = 2 * time.Second

// Out runs kubectl args against o's cluster and returns its stdout. Its
// stderr goes into the error, not to the screen, so a read that finds
// nothing prints nothing.
func Out(ctx context.Context, o plugin.Options, args ...string) (string, error) {
	var out, errs bytes.Buffer
	if err := plugin.Kubectl(ctx, &errs, o, nil, &out, args...); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errs.String()))
	}
	return out.String(), nil
}

// Run runs kubectl args against o's cluster, streaming its output to w.
func Run(ctx context.Context, w io.Writer, o plugin.Options, args ...string) error {
	return plugin.Kubectl(ctx, w, o, nil, nil, args...)
}

// Apply applies manifest from stdin.
func Apply(ctx context.Context, w io.Writer, o plugin.Options, manifest []byte) error {
	return plugin.Kubectl(ctx, w, o, manifest, nil, "apply", "-f", "-")
}

// AWS runs the AWS CLI for o's Region and profile and returns its stdout.
// The installer module has no EFS or S3 Files SDK, and the CLI is one of its
// prerequisites (kubectl's token plugin is `aws eks get-token`).
func AWS(ctx context.Context, o plugin.Options, args ...string) (string, error) {
	argv := append([]string{"aws"}, args...)
	argv = append(argv, "--region", o.Region)
	if o.Profile != "" {
		argv = append(argv, "--profile", o.Profile)
	}
	var out, errs bytes.Buffer
	if err := plugin.Exec(ctx, &errs, plugin.Cmd{Stdout: &out, Argv: argv}); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errs.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// FileSystemID is this deployment's EFS file system (`setup-aws plugin
// efs` made it, creation token <cluster>-actors).
func FileSystemID(ctx context.Context, o plugin.Options) (string, error) {
	id, err := AWS(ctx, o, "efs", "describe-file-systems", "--creation-token", o.Cluster+"-actors",
		"--query", "FileSystems[0].FileSystemId", "--output", "text")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(id, "fs-") {
		return "", fmt.Errorf("no EFS file system %s-actors: run `setup-aws plugin efs`", o.Cluster)
	}
	return id, nil
}

// EnsurePodIdentity restarts kind/name in ns until each of its pods has the
// Pod Identity credentials: EKS adds them to a pod only when it is created,
// and a new association takes a few seconds to apply.
func EnsurePodIdentity(ctx context.Context, w io.Writer, o plugin.Options, ns, obj string) error {
	sel, err := Out(ctx, o, "-n", ns, "get", obj, "-o", "jsonpath={.spec.selector.matchLabels}")
	if err != nil {
		return err
	}
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(sel), &labels); err != nil || len(labels) == 0 {
		return fmt.Errorf("%s/%s: no selector (%q)", ns, obj, sel)
	}
	var parts []string
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	for attempt := 1; ; attempt++ {
		missing, err := podsWithoutPodIdentity(ctx, o, ns, strings.Join(parts, ","))
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			fmt.Fprintf(w, "%s/%s has Pod Identity credentials\n", ns, obj)
			return nil
		}
		if attempt == 4 {
			return fmt.Errorf("%s/%s pods still lack Pod Identity credentials: %s", ns, obj, strings.Join(missing, " "))
		}
		fmt.Fprintf(w, "%s/%s started before its Pod Identity association applied; restarting\n", ns, obj)
		sleep(ctx, 5*Wait)
		if err := Run(ctx, w, o, "-n", ns, "rollout", "restart", obj); err != nil {
			return err
		}
		if err := Run(ctx, w, o, "-n", ns, "rollout", "status", obj, "--timeout=5m"); err != nil {
			return err
		}
	}
}

// podCredentialsEnv is what EKS Pod Identity adds to a pod's containers.
const podCredentialsEnv = "AWS_CONTAINER_CREDENTIALS_FULL_URI"

func podsWithoutPodIdentity(ctx context.Context, o plugin.Options, ns, selector string) ([]string, error) {
	out, err := Out(ctx, o, "-n", ns, "get", "pods", "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Env []struct {
						Name string `json:"name"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return nil, fmt.Errorf("read pods of %s: %w", selector, err)
	}
	var missing []string
	for _, p := range pods.Items {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		has := false
		for _, c := range p.Spec.Containers {
			for _, e := range c.Env {
				has = has || e.Name == podCredentialsEnv
			}
		}
		if !has {
			missing = append(missing, p.Metadata.Name)
		}
	}
	return missing, nil
}

// The label StorageTmpl puts on every StorageClass the plugins make: the
// installer replaces or deletes only a class that carries it.
const (
	managedByKey = "app.kubernetes.io/managed-by"
	managedBy    = "substrate-eks"
)

// storageClass reads StorageClass name: whether it exists, whether it
// carries the installer's managed-by label, and its parameters.
func storageClass(ctx context.Context, o plugin.Options, name string) (exists, ours bool, params map[string]string, err error) {
	have, err := Out(ctx, o, "get", "storageclass", name, "--ignore-not-found", "-o", "json")
	if err != nil || strings.TrimSpace(have) == "" {
		return false, false, nil, err
	}
	var sc struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Parameters map[string]string `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(have), &sc); err != nil {
		return false, false, nil, fmt.Errorf("read StorageClass %s: %w", name, err)
	}
	return true, sc.Metadata.Labels[managedByKey] == managedBy, sc.Parameters, nil
}

// ApplyStorageClass applies manifest, a StorageClass named name whose
// fileSystemId is fs, first deleting one of that name that has another
// fileSystemId or uid (StorageClass parameters cannot change). A class of
// that name without the installer's label is someone else's: it is refused,
// by name, and left as it is (review R10).
func ApplyStorageClass(ctx context.Context, w io.Writer, o plugin.Options, name, fs, uid string, manifest []byte) error {
	exists, ours, p, err := storageClass(ctx, o, name)
	if err != nil {
		return err
	}
	if exists && !ours {
		return fmt.Errorf("StorageClass %s exists without the label %s=%s, so the installer did not make it and will not replace it: "+
			"delete or rename it, then deploy the plugin again", name, managedByKey, managedBy)
	}
	if exists && (p["fileSystemId"] != fs || (uid != "" && p["uid"] != uid)) {
		fmt.Fprintf(w, "Replacing StorageClass %s (it was for %s %s)\n", name, p["fileSystemId"], p["uid"])
		if err := Run(ctx, w, o, "delete", "storageclass", name); err != nil {
			return err
		}
	}
	return Apply(ctx, w, o, manifest)
}

// DeleteStorageClass deletes StorageClass name when it carries the
// installer's label; one someone else made is left alone, and said so.
func DeleteStorageClass(ctx context.Context, w io.Writer, o plugin.Options, name string) error {
	exists, ours, _, err := storageClass(ctx, o, name)
	if err != nil || !exists {
		return err
	}
	if !ours {
		fmt.Fprintf(w, "StorageClass %s has no %s=%s label: the installer did not make it, so it is left alone\n", name, managedByKey, managedBy)
		return nil
	}
	return Run(ctx, w, o, "delete", "storageclass", name, "--ignore-not-found")
}

// OwnerTagsArg is the EFS CSI driver's --tags value: space-separated
// key:value pairs, the driver's own efs.csi.aws.com/cluster tag (named, so it
// is kept whether the flag adds to it or replaces it) plus the owner tag and
// TAGS.
func OwnerTagsArg(o plugin.Options) (string, error) {
	tags, err := o.OwnerTags()
	if err != nil {
		return "", err
	}
	pairs := []string{"efs.csi.aws.com/cluster:true"}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.ContainsAny(k+tags[k], ": \"") {
			return "", fmt.Errorf("tag %s=%s cannot be passed to the EFS CSI driver (no ':', space or quote)", k, tags[k])
		}
		pairs = append(pairs, k+":"+tags[k])
	}
	return strings.Join(pairs, " "), nil
}

// ---- kubectl-ate ---------------------------------------------------------

// Ate runs kubectl-ate against the installer's kubeconfig.
type Ate struct {
	Bin, Kubeconfig string
}

func (k Ate) argv(args ...string) []string {
	return append(append([]string{k.Bin}, args...), "--kubeconfig", k.Kubeconfig)
}

// Do runs kubectl-ate args and returns its stdout; stderr goes into the error.
func (k Ate) Do(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	var out, errs bytes.Buffer
	err := plugin.Exec(ctx, &errs, plugin.Cmd{Env: []string{"KUBECONFIG=" + k.Kubeconfig}, Stdin: stdin, Stdout: &out, Argv: k.argv(args...)})
	if err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errs.String()))
	}
	return out.Bytes(), nil
}

// Start runs kubectl-ate args in the background (a proxy) and returns how to
// stop it and what it printed.
func (k Ate) Start(ctx context.Context, args ...string) (stop func(), logs func() string) {
	return Start(ctx, plugin.Cmd{Env: []string{"KUBECONFIG=" + k.Kubeconfig}, Argv: k.argv(args...)})
}

// Start runs c in the background (a proxy or port-forward) and returns how
// to stop it, which waits for it to exit, and the tail of what it printed.
func Start(ctx context.Context, c plugin.Cmd) (stop func(), logs func() string) {
	ctx, cancel := context.WithCancel(ctx)
	var out tail
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = plugin.Exec(ctx, &out, c)
	}()
	return func() { cancel(); <-done }, out.String
}

// KubectlAtePath is where kubectl-ate is built, the installer's cache beside
// its trees, one binary per release; the counter demo builds the same file.
func KubectlAtePath(tag string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "substrate-eks", "bin", "kubectl-ate-"+tag)
}

// BuildAte builds kubectl-ate from the release's cached tree, o.Tree (the
// control plane deploy fetched it); Go's build cache makes a rebuild quick.
func BuildAte(ctx context.Context, w io.Writer, o plugin.Options) (Ate, error) {
	tree := o.Tree
	if _, err := os.Stat(filepath.Join(tree, "go.mod")); err != nil {
		return Ate{}, fmt.Errorf("no Substrate %s tree at %s: run --deploy-control-plane first", o.Tag, tree)
	}
	bin := KubectlAtePath(o.Tag)
	err := plugin.Exec(ctx, w, plugin.Cmd{Dir: tree, Env: []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=vendor", "CGO_ENABLED=0"},
		Argv: []string{"go", "build", "-o", bin, "./cmd/kubectl-ate"}})
	if err != nil {
		return Ate{}, fmt.Errorf("build kubectl-ate: %w", err)
	}
	return Ate{Bin: bin, Kubeconfig: o.Kubeconfig}, nil
}

// tail keeps the last few KB a child wrote, for an error message.
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Poll calls f every Wait until it reports done, up to tries times.
func Poll(ctx context.Context, tries int, f func() bool) bool {
	for i := 0; i < tries; i++ {
		if f() {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		sleep(ctx, Wait)
	}
	return false
}
