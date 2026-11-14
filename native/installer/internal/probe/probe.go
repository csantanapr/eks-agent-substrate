// Package probe is the reinstall guard's read of a cluster: does it already
// run Substrate, and which version. It is substrate-gke's
// snapshot.CheckInstalled and ParseInstalled without the shell: the wizard
// runs the probe by re-executing itself (--probe-installed), which calls Run,
// and reads the one marker line Run prints with ParseInstalled. Every
// command in it is a kubectl `get` started from an argv.
package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Marker starts the line Run prints, as substrate-gke's installedMarker.
const Marker = "SUBSTRATE_EKS_INSTALLED "

// Result is what the probe found. Installed without Atelet means the
// ate-system namespace exists but the atelet DaemonSet does not: an
// interrupted or partially deleted install rather than a running one.
// Version is the tag of the image the API server runs, empty when it could
// not be read or names no version.
type Result struct {
	Installed bool
	Atelet    bool
	Version   string
	// Plugins are the plugins the cluster records as installed (the
	// plugin record, kube-system/substrate-eks-plugins), which
	// --remove-control-plane removes first. PluginsRead reports that the
	// record was read: without it the plugins are unknown, not none.
	Plugins     []string
	PluginsRead bool
}

// Partial reports the namespace-without-atelet state.
func (r Result) Partial() bool { return r.Installed && !r.Atelet }

// Badge is the picker's install-state label (issue #5 AC5.1).
func (r Result) Badge() string {
	switch {
	case r.Partial():
		return "partial install"
	case r.Installed && r.Version != "":
		return "installed " + r.Version
	case r.Installed:
		return "installed"
	}
	return "ready"
}

// Kubectl runs one kubectl command against the probed cluster and returns
// its stdout; a failed command is an error, never an empty answer.
type Kubectl func(ctx context.Context, args ...string) (string, error)

// Check reads the cluster through kubectl and returns the marker line. A
// failed namespace or DaemonSet read stops it with that error rather than
// reporting "absent": a cluster the probe cannot read has not been shown to
// be free of Substrate. The image read is best effort, as in substrate-gke.
func Check(ctx context.Context, kubectl Kubectl) (string, error) {
	ns, err := kubectl(ctx, "get", "namespace", "ate-system", "--ignore-not-found", "-o", "jsonpath={.metadata.name}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ns) == "" {
		return Marker + "false", nil
	}
	atelet, err := kubectl(ctx, "-n", "ate-system", "get", "daemonset", "atelet", "--ignore-not-found", "-o", "name")
	if err != nil {
		return "", err
	}
	line := Marker + "true"
	if strings.TrimSpace(atelet) != "" {
		line += " atelet"
		image, err := kubectl(ctx, "-n", "ate-system", "get", "deployment", "ate-api-server", "--ignore-not-found",
			"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="ate-api-server")].image}`)
		if image = strings.TrimSpace(image); err == nil && image != "" {
			line += " image=" + image
		}
	}
	// The plugins a removal removes first; best effort, like the image: a
	// failed read leaves them unknown.
	rec, err := kubectl(ctx, "-n", plugin.RecordNamespace, "get", "configmap", plugin.RecordName, "--ignore-not-found",
		"-o", "jsonpath={.data."+plugin.RecordKey+"}")
	if err == nil {
		line += " plugins=" + plugin.FormatRecord(plugin.ParseRecord(rec))
	}
	return line, nil
}

// ParseInstalled reads the line Check printed, as substrate-gke's.
func ParseInstalled(lines []string) (Result, error) {
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, Marker)
		if !ok {
			continue
		}
		rest, ok = strings.CutPrefix(rest, "true")
		if !ok {
			return Result{}, nil
		}
		r := Result{Installed: true}
		for _, f := range strings.Fields(rest) {
			switch k, v, _ := strings.Cut(f, "="); k {
			case "atelet":
				r.Atelet = true
			case "image":
				r.Version = imageTagVersion(v)
			case "plugins":
				r.Plugins, r.PluginsRead = plugin.ParseRecord(v), true
			}
		}
		return r, nil
	}
	return Result{}, fmt.Errorf("probe did not report installation status")
}

// imageTagVersion is the tag of an image reference: what follows the last
// colon of the last path element, before any digest. A registry port is not
// a tag, and latest names no version.
func imageTagVersion(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	name := ref[strings.LastIndex(ref, "/")+1:]
	_, tag, ok := strings.Cut(name, ":")
	if !ok || tag == "latest" {
		return ""
	}
	return tag
}

// Run probes cluster in region and prints the marker line to w. It writes the
// installer's kind of kubeconfig (awsx.WriteKubeconfig: mode 0600, the
// profile, no token) into a directory of its own and removes it on return: a
// probe only reads, and the user's ~/.kube/config is never read or written,
// because every kubectl is given --kubeconfig.
func Run(ctx context.Context, w io.Writer, profile, region, cluster string) error {
	c, err := awsx.New(ctx, profile, region)
	if err != nil {
		return err
	}
	cl, err := c.Describe(ctx, cluster)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "substrate-eks-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	kc := filepath.Join(dir, "config")
	if err := awsx.WriteKubeconfig(kc, cl, region, profile); err != nil {
		return err
	}
	line, err := Check(ctx, KubectlWith(kc))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, line)
	return err
}

// KubectlWith runs the kubectl on PATH against kubeconfig only, with a
// request timeout so an unreachable endpoint cannot hold the probe.
func KubectlWith(kubeconfig string) Kubectl {
	return func(ctx context.Context, args ...string) (string, error) {
		argv := append([]string{"--kubeconfig", kubeconfig, "--request-timeout", "15s"}, args...)
		c := exec.CommandContext(ctx, "kubectl", argv...)
		var stderr bytes.Buffer
		c.Stderr = &stderr
		out, err := c.Output()
		if err != nil {
			return "", fmt.Errorf("kubectl %s: %w: %s", strings.Join(args[:min(len(args), 4)], " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
}

// SimLine is the answer a --dry-run replays for a cluster, chosen by name as
// substrate-gke's dry run does, so the guard's whole story is on display: a
// name with "partial" reads as an interrupted install, one with "installed"
// as a running v1.0.0, and every other as clean.
func SimLine(cluster string) string {
	switch {
	case strings.Contains(cluster, "partial"):
		return Marker + "true"
	case strings.Contains(cluster, "installed"):
		return Marker + "true atelet image=ghcr.io/agent-substrate/substrate/ate-api-server:v1.0.0 plugins=efs,llm-gateway"
	}
	return Marker + "false"
}
