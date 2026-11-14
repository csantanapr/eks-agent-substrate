package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Lines Run prints, for the wizard's checklist (steps.Plugins) and its
// dry-run replay. Each is followed by the plugin ID.
const (
	DeployLine  = "Deploying plugin "
	VerifyLine  = "Checking plugin "
	ReadyLine   = "Plugin ready: "
	RemoveLine  = "Removing plugin "
	RemovedLine = "Plugin removed: "
)

// Actions for Run (--plugin-action).
const (
	ActionDeploy = "deploy"
	ActionVerify = "verify"
	ActionRemove = "remove"
)

// Cmd is one child process: an argv, never shell text. A secret reaches a
// child only through Env or Stdin.
type Cmd struct {
	Dir   string
	Env   []string
	Stdin []byte
	// Stdout, when set, takes stdout instead of the writer.
	Stdout io.Writer
	Argv   []string
}

// Exec starts every child process a plugin makes. Tests replace it to see
// each argv, environment and stdin.
var Exec = func(ctx context.Context, w io.Writer, c Cmd) error {
	if len(c.Argv) == 0 {
		return errors.New("plugin: empty argv")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = c.Dir, w, w
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	cmd.Env = append(os.Environ(), c.Env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(c.Argv[:min(len(c.Argv), 3)], " "), err)
	}
	return nil
}

// Kubectl runs kubectl against o's cluster only: --kubeconfig is the
// installer's own, so an ambient context is never reached. stdin and stdout
// may be nil.
func Kubectl(ctx context.Context, w io.Writer, o Options, stdin []byte, stdout io.Writer, args ...string) error {
	if o.Kubeconfig == "" {
		return errors.New("plugin: no kubeconfig")
	}
	return Exec(ctx, w, Cmd{Stdin: stdin, Stdout: stdout, Argv: append([]string{"kubectl", "--kubeconfig", o.Kubeconfig}, args...)})
}

// SetupAWS runs `go -C <o.SetupAWS> run . <args>` for o's deployment, with
// the knobs that would change what it does pinned, as the wizard runs it.
func SetupAWS(ctx context.Context, w io.Writer, o Options, args ...string) error {
	if o.SetupAWS == "" {
		return errors.New("plugin: no setup-aws path")
	}
	env := []string{"CLUSTER_NAME=" + o.Cluster, "AWS_REGION=" + o.Region, "AWS_PROFILE=" + o.Profile,
		"PLAN=false", "KEEP_CLUSTER=false"}
	if o.Tags != "" {
		env = append(env, "TAGS="+o.Tags)
	}
	return Exec(ctx, w, Cmd{Env: env, Argv: append([]string{"go", "-C", o.SetupAWS, "run", "."}, args...)})
}

// ReadRecord returns the IDs the cluster records as installed (none when the
// record is missing).
func ReadRecord(ctx context.Context, w io.Writer, o Options) ([]string, error) {
	var out bytes.Buffer
	err := Kubectl(ctx, w, o, nil, &out, "-n", RecordNamespace, "get", "configmap", RecordName,
		"--ignore-not-found", "-o", "jsonpath={.data."+RecordKey+"}")
	if err != nil {
		return nil, fmt.Errorf("read the plugin record: %w", err)
	}
	return ParseRecord(out.String()), nil
}

// WriteRecord makes the record hold exactly ids; with none it deletes it.
func WriteRecord(ctx context.Context, w io.Writer, o Options, ids []string) error {
	if len(ids) == 0 {
		return Kubectl(ctx, w, o, nil, nil, "-n", RecordNamespace, "delete", "configmap", RecordName, "--ignore-not-found")
	}
	cm := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: substrate-eks
data:
  %s: %q
`, RecordName, RecordNamespace, RecordKey, FormatRecord(ids))
	return Kubectl(ctx, w, o, []byte(cm), nil, "apply", "-f", "-")
}

// AddToRecord adds ids to the record.
func AddToRecord(ctx context.Context, w io.Writer, o Options, ids ...string) error {
	rec, err := ReadRecord(ctx, w, o)
	if err != nil {
		return err
	}
	all := append(rec, ids...)
	if slices.Equal(ParseRecord(FormatRecord(all)), rec) {
		return nil
	}
	return WriteRecord(ctx, w, o, all)
}

// hasNeeds reports whether p changes the control plane install.
func hasNeeds(p Plugin) bool {
	n := p.Needs
	return len(n.Preview)+len(n.CredentialProviders)+len(n.IngressIDs) > 0 || n.Source != nil
}

// Run deploys, checks or removes plugins on o's cluster.
//
//   - deploy: ids and what they require, in order. Each plugin's AWS half
//     (`setup-aws plugin <id>`), then Deploy, then Verify; then it is
//     recorded. A plugin with Needs is refused unless the control plane
//     already carries them (its ID is in the record), since its objects would
//     not work: run --deploy-control-plane --plugins <id> first.
//   - verify: Verify for ids and what they require.
//   - remove: ids (every recorded plugin when ids is empty), in reverse
//     order: Remove, then `setup-aws plugin <id> --delete`, then dropped from
//     the record. A recorded plugin that requires one being removed stops it.
func Run(ctx context.Context, w io.Writer, action string, ids []string, o Options) error {
	if o.Cluster == "" || o.Region == "" {
		return errors.New("plugin: cluster and region are required")
	}
	switch action {
	case ActionDeploy, ActionVerify:
		ps, err := Resolve(ids)
		if err != nil {
			return err
		}
		if action == ActionVerify {
			for _, p := range ps {
				fmt.Fprintln(w, VerifyLine+p.ID)
				if p.Verify != nil {
					if err := p.Verify(ctx, w, o); err != nil {
						return fmt.Errorf("plugin %s: %w", p.ID, err)
					}
				}
				fmt.Fprintln(w, ReadyLine+p.ID)
			}
			return nil
		}
		rec, err := ReadRecord(ctx, w, o)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.NeedsAutoMode != "" && !o.AutoMode {
				return fmt.Errorf("plugin %s %s; %s has EKS Auto Mode off", p.ID, p.NeedsAutoMode, o.Cluster)
			}
			if hasNeeds(p) && !slices.Contains(rec, p.ID) {
				return fmt.Errorf("plugin %s changes the control plane install, which does not carry it yet: run --deploy-control-plane --plugins %s first", p.ID, p.ID)
			}
		}
		for _, p := range ps {
			if err := deployOne(ctx, w, p, o); err != nil {
				return fmt.Errorf("plugin %s: %w", p.ID, err)
			}
			if err := AddToRecord(ctx, w, o, p.ID); err != nil {
				return err
			}
			fmt.Fprintln(w, ReadyLine+p.ID)
		}
		return nil
	case ActionRemove:
		return remove(ctx, w, ids, o)
	}
	return fmt.Errorf("plugin action %q is not %s, %s or %s", action, ActionDeploy, ActionVerify, ActionRemove)
}

func deployOne(ctx context.Context, w io.Writer, p Plugin, o Options) error {
	fmt.Fprintln(w, DeployLine+p.ID)
	if p.SetupAWS {
		if err := SetupAWS(ctx, w, o, "plugin", p.ID); err != nil {
			return err
		}
	}
	if p.Deploy != nil {
		if err := p.Deploy(ctx, w, o); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, VerifyLine+p.ID)
	if p.Verify != nil {
		return p.Verify(ctx, w, o)
	}
	return nil
}

func remove(ctx context.Context, w io.Writer, ids []string, o Options) error {
	rec, err := ReadRecord(ctx, w, o)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		ids = rec
	}
	var ps []Plugin
	for _, id := range ids {
		p, ok := Get(id)
		if !ok {
			// A record written by a newer installer: say so and leave it.
			return fmt.Errorf("unknown plugin %q (recorded in %s/%s): this installer cannot remove it", id, RecordNamespace, RecordName)
		}
		ps = append(ps, p)
	}
	// Refuse to remove what a plugin that stays still requires.
	for _, keep := range rec {
		if slices.Contains(ids, keep) {
			continue
		}
		if k, ok := Get(keep); ok {
			for _, r := range k.Requires {
				if slices.Contains(ids, r) {
					return fmt.Errorf("plugin %s requires %s: remove it too", keep, r)
				}
			}
		}
	}
	// Reverse install order: a plugin before what it requires.
	slices.SortFunc(ps, func(a, b Plugin) int {
		if a.Order != b.Order {
			return b.Order - a.Order
		}
		return strings.Compare(b.ID, a.ID)
	})
	left := slices.Clone(rec)
	for _, p := range ps {
		fmt.Fprintln(w, RemoveLine+p.ID)
		if p.Remove != nil {
			if err := p.Remove(ctx, w, o); err != nil {
				return fmt.Errorf("plugin %s: %w", p.ID, err)
			}
		}
		if p.SetupAWS {
			if err := SetupAWS(ctx, w, o, "plugin", p.ID, "--delete"); err != nil {
				return fmt.Errorf("plugin %s: %w", p.ID, err)
			}
		}
		left = slices.DeleteFunc(left, func(id string) bool { return id == p.ID })
		if err := WriteRecord(ctx, w, o, left); err != nil {
			return err
		}
		fmt.Fprintln(w, RemovedLine+p.ID)
	}
	return nil
}
