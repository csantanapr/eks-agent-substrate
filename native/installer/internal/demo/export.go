package demo

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Thin exports for the plugins that run a counter pool of their own (mode 3
// and mode 3 on isolated sandboxes): each one calls the unexported code the
// demo's own Deploy and Verify use, on the same env, so the plugins build,
// push, wait and check exactly as the demo does. o.Tag and o.TreeRoot name the
// tree and the kubectl-ate binary; o.SandboxClasses is not read.

// exportEnv is the env the exports run on; tests replace it with the fake.
var exportEnv = newReal

// Pool is a counter pool a plugin made: its atespace, its ActorTemplate, and
// the actor RoundTrip suspends and resumes.
type Pool struct{ Atespace, Template, Actor string }

func (p Pool) class() class {
	return class{name: p.Template, demo: p.Template, atespace: p.Atespace, template: p.Template, actor: p.Actor}
}

// kubectlFor is the kubectl argv prefix Deploy uses: the installer's own
// kubeconfig and the cluster's context.
func kubectlFor(kc, cluster string) func(...string) []string {
	return func(args ...string) []string {
		return append([]string{"kubectl", "--kubeconfig", kc, "--context", cluster}, args...)
	}
}

// CounterImage builds the counter from tree for the architecture of the nodes
// labeled ate.dev/pool=<pool>, on the base image tree's .ko.yaml gives it,
// pushes it to <cluster>/counter:<tag> in the caller's ECR registry, and
// returns its reference by digest: the first half of Deploy's demoImages.
// The demo pushes <cluster>/counter:<o.Tag>, so a plugin on the same release
// passes a tag of its own, which its removal can delete.
func CounterImage(ctx context.Context, w io.Writer, o Options, tree, pool, tag string) (string, error) {
	e := exportEnv(o)
	kc, err := e.kubeconfig(ctx)
	if err != nil {
		return "", err
	}
	arch, err := e.nodeArch(ctx, kubectlFor(kc, o.Cluster), []class{{name: pool}})
	if err != nil {
		return "", err
	}
	base, err := counterBase(tree)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "substrate-eks-counter-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	fmt.Fprintf(w, "Building the counter image for linux/%s on %s\n", arch, base)
	bin := filepath.Join(dir, "counter")
	buildEnv := append(append([]string{}, goEnv...), "GOOS=linux", "GOARCH="+arch)
	if err := e.run(ctx, w, tree, buildEnv, "go", "build", "-trimpath", "-o", bin, "./demos/counter"); err != nil {
		return "", fmt.Errorf("build the counter: %w", err)
	}
	ref, err := e.pushCounter(ctx, bin, base, arch, o.Cluster+"/counter", tag)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(w, "Pushed "+ref)
	return ref, nil
}

// Pin is a public image's tagged reference with its digest appended, read
// anonymously, as Deploy pins the worker images.
func Pin(ctx context.Context, o Options, tagged string) (string, error) {
	return exportEnv(o).pin(ctx, tagged)
}

// KubectlAte builds kubectl-ate from tree, as Verify does, and returns its path.
func KubectlAte(ctx context.Context, w io.Writer, o Options, tree string) (string, error) {
	e := exportEnv(o)
	if err := e.run(ctx, w, tree, goEnv, "go", "build", "-o", e.kubectlAte, "./cmd/kubectl-ate"); err != nil {
		return "", fmt.Errorf("build kubectl-ate: %w", err)
	}
	return e.kubectlAte, nil
}

// exportAte is the kubectl-ate runner for o: the binary KubectlAte built,
// the installer's kubeconfig and the cluster's context.
func exportAte(ctx context.Context, o Options) (*env, ate, error) {
	e := exportEnv(o)
	kc, err := e.kubeconfig(ctx)
	if err != nil {
		return nil, ate{}, err
	}
	return e, ate{e: e, bin: e.kubectlAte, kubeconfig: kc, context: o.Cluster}, nil
}

// Ate runs kubectl-ate with args and returns its stdout (stderr is in the
// error), as Deploy and Verify run it. Build it first with KubectlAte.
func Ate(ctx context.Context, o Options, args ...string) ([]byte, error) {
	_, k, err := exportAte(ctx, o)
	if err != nil {
		return nil, err
	}
	return k.do(ctx, args...)
}

// TemplateImage is the first container image of an ActorTemplate as
// `kubectl ate get actor-template -o json` prints it.
func TemplateImage(out []byte) (string, error) { return templateImage(out) }

// WaitGolden waits for p's golden snapshot, as Deploy does (15 minutes).
func WaitGolden(ctx context.Context, w io.Writer, o Options, p Pool) error {
	e, k, err := exportAte(ctx, o)
	if err != nil {
		return err
	}
	return e.waitGolden(ctx, w, k, p.class())
}

// RoundTrip is Verify's check on p: it suspends and resumes p.Actor (made
// from p.Template if missing, reverted if crashed) through kubectl ate proxy,
// checks the memory, file and rootfs counters continue, and waits for the new
// snapshot in the deployment's S3 bucket. It returns the PASS summary.
func RoundTrip(ctx context.Context, w io.Writer, o Options, p Pool) (string, error) {
	e, k, err := exportAte(ctx, o)
	if err != nil {
		return "", err
	}
	account, err := e.account(ctx)
	if err != nil {
		return "", err
	}
	return e.roundtrip(ctx, w, k, p.class(), SnapBucket(o.Cluster, account, o.Region))
}
