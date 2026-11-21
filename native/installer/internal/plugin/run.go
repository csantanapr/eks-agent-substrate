package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// Lines Run prints, for the wizard's checklist (steps.Plugins) and its
// dry-run replay. Each is followed by the plugin ID.
const (
	DeployLine  = "Deploying plugin "
	VerifyLine  = "Checking plugin "
	ReadyLine   = "Plugin ready: "
	RemoveLine  = "Removing plugin "
	RemovedLine = "Plugin removed: "
	// PlanLine comes before any removal, for each plugin with AWS resources:
	// what deleting them would do, and whether AWS refuses it.
	PlanLine = "Checking what removing plugin would delete in AWS: "
	// NeedsKeptLine follows RemovedLine for a plugin with Needs: the running
	// control plane still carries them (a preview gate, a credential
	// provider, ingress clients, another release) until it is deployed again.
	NeedsKeptLine = "The control plane still carries the needs of "
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

// SetupAWS runs setup-aws at o.SetupAWS (SetupAWSArgv) for o's deployment,
// with the knobs that would change what it does pinned, as the wizard runs
// it.
func SetupAWS(ctx context.Context, w io.Writer, o Options, args ...string) error {
	if o.SetupAWS == "" {
		return errors.New("plugin: no setup-aws path")
	}
	env := append([]string{"CLUSTER_NAME=" + o.Cluster, "AWS_REGION=" + o.Region},
		append(awsx.ProfileEnv(o.Profile), "PLAN=false", "KEEP_CLUSTER=false")...)
	if o.Tags != "" {
		env = append(env, "TAGS="+o.Tags)
	}
	return Exec(ctx, w, Cmd{Env: env, Argv: SetupAWSArgv(o.SetupAWS, args...)})
}

// IsSetupAWSBinary reports whether path is a file: the setup-aws binary a
// release archive carries beside substrate-eks, rather than the
// native/tools/setup-aws source directory.
func IsSetupAWSBinary(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// SetupAWSArgv is the argv that runs setup-aws at path with args: the
// binary itself, by its absolute path, when path is one (IsSetupAWSBinary),
// else `go -C <path> run .` on the source directory, as substrate-gke runs
// setup-gcp.
func SetupAWSArgv(path string, args ...string) []string {
	if IsSetupAWSBinary(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		return append([]string{path}, args...)
	}
	return append([]string{"go", "-C", path, "run", "."}, args...)
}

// NodePools are the PoolLabel values of o's cluster's nodes (Options.Pools).
func NodePools(ctx context.Context, w io.Writer, o Options) ([]string, error) {
	var out bytes.Buffer
	if err := Kubectl(ctx, w, o, nil, &out, PoolsArgs...); err != nil {
		return nil, fmt.Errorf("read the nodes' %s labels: %w", PoolLabel, err)
	}
	return ParsePools(out.String()), nil
}

// BundledDatabaseArgs are the kubectl arguments that name the StatefulSet
// of ate-setup's bundled PostgreSQL, ate-system/postgres, when it exists.
var BundledDatabaseArgs = []string{"-n", "ate-system", "get", "statefulset", "postgres", "--ignore-not-found", "-o", "name"}

// BundledDatabase reports whether o's control plane runs on the bundled
// PostgreSQL (Options.BundledDatabase): its StatefulSet exists. With RDS,
// or no control plane, it does not.
func BundledDatabase(ctx context.Context, w io.Writer, o Options) (bool, error) {
	var out bytes.Buffer
	if err := Kubectl(ctx, w, o, nil, &out, BundledDatabaseArgs...); err != nil {
		return false, fmt.Errorf("read whether %s runs the bundled PostgreSQL: %w", o.Cluster, err)
	}
	return strings.TrimSpace(out.String()) != "", nil
}

// LoadRecord reads the whole record: none when the ConfigMap is missing. A
// failed read, or one it cannot parse, is an error, never an empty record.
func LoadRecord(ctx context.Context, w io.Writer, o Options) (Record, error) {
	var out bytes.Buffer
	if err := Kubectl(ctx, w, o, nil, &out, RecordArgs...); err != nil {
		return Record{}, fmt.Errorf("read the plugin record: %w", err)
	}
	r, err := ParseRecordJSON(out.Bytes())
	if err != nil {
		return Record{}, fmt.Errorf("read the plugin record: %w", err)
	}
	return r, nil
}

// ReadRecord returns the IDs the cluster records as installed (none when the
// record is missing), not the pending ones.
func ReadRecord(ctx context.Context, w io.Writer, o Options) ([]string, error) {
	r, err := LoadRecord(ctx, w, o)
	return r.Plugins, err
}

// WriteRecord makes the record hold exactly r; with no plugin installed or
// pending it deletes it. A record with keys this installer does not know
// (Record.CheckKeys) is refused, since either write would drop them.
func WriteRecord(ctx context.Context, w io.Writer, o Options, r Record) error {
	if err := r.CheckKeys(); err != nil {
		return err
	}
	if r.Empty() {
		return Kubectl(ctx, w, o, nil, nil, "-n", RecordNamespace, "delete", "configmap", RecordName, "--ignore-not-found")
	}
	return Kubectl(ctx, w, o, r.Manifest(), nil, "apply", "-f", "-")
}

// ReadRelease is the release of the install on o's cluster (ReleaseOf): the
// plugins its record lists plus chosen, on o's release, which is the --tag
// one, located for o's tree root, overlay and Region.
func ReadRelease(ctx context.Context, w io.Writer, o Options, chosen []string) (Release, error) {
	rec, err := ReadRecord(ctx, w, o)
	if err != nil {
		return Release{}, err
	}
	ps, err := Resolve(append(rec, chosen...))
	if err != nil {
		return Release{}, err
	}
	r, err := ReleaseOf(ps, o.Tag, o.ImageRepo)
	if err != nil {
		return Release{}, err
	}
	return r.Locate(o.TreeRoot, o.OverlayDigest, o.Region), nil
}

// AddToRecord adds ids to the installed plugins and takes them out of the
// pending ones, keeping the recorded NITRO_COMPUTE. It writes nothing when
// that changes nothing.
func AddToRecord(ctx context.Context, w io.Writer, o Options, ids ...string) error {
	rec, err := LoadRecord(ctx, w, o)
	if err != nil {
		return err
	}
	next := rec
	next.Plugins = ParseRecord(FormatRecord(append(slices.Clone(rec.Plugins), ids...)))
	next.Pending = without(rec.Pending, ids...)
	if next.Equal(rec) {
		return nil
	}
	return WriteRecord(ctx, w, o, next)
}

// without is ids less drop.
func without(ids []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return slices.Contains(drop, id) })
}

// PendingLine is printed when Run's deploy records a plugin as pending,
// just before the plugin's first write, and only when that changes the
// record. It is followed by the plugin ID and the NITRO_COMPUTE recorded
// with it.
const PendingLine = "Recording as pending before this deploy writes: "

// pendingTries is how many times markPending writes the pending entry
// while other runs change the record between its read and its write.
const pendingTries = 3

// RecordChangedLine is printed when the pending write is refused because
// the record changed since it was read: markPending reads it again. It is
// followed by the plugin ID.
const RecordChangedLine = "The plugin record changed since it was read; reading it again to record as pending: "

// errRecordChanged is the pending write's refusal: the record is not the one
// it was written from (kubectl create found one, or kubectl replace another
// resourceVersion).
var errRecordChanged = errors.New("the plugin record changed since it was read")

// markPending records p as pending with o's NITRO_COMPUTE (a plugin that
// follows it, Plugin.FollowsNitroCompute), so a deploy that fails leaves
// it where a removal finds it, and the removal runs with the NITRO_COMPUTE
// the deploy ran with. Any other plugin is not recorded until it is checked.
// It writes, and says so, only when that changes the record, and only while
// the record is the one it read (writePending): when another run changed it
// in between, it reads it again, checks it again and writes again, at most
// pendingTries times. So of two deploys with opposite NITRO_COMPUTE at the
// same time, the second to write reads the first one's entry and is refused,
// unless another run's plain apply of the record lands in between and drops
// that entry: runs at the same time stay unsupported.
func markPending(ctx context.Context, w io.Writer, o Options, p Plugin) error {
	if !p.FollowsNitroCompute {
		return nil
	}
	for try := 1; ; try++ {
		rec, err := LoadRecord(ctx, w, o)
		if err != nil {
			return err
		}
		// The replace below sends the whole ConfigMap, so keys it does not
		// write would be gone: a newer installer's record is left to it.
		if err := rec.CheckKeys(); err != nil {
			return err
		}
		// Checked again on this fresh read: another run may have recorded p
		// with the other NITRO_COMPUTE since this deploy's checks.
		if err := checkRecordedNitroCompute(rec, p, o); err != nil {
			return fmt.Errorf("another run recorded %s meanwhile: %w", p.ID, err)
		}
		next := rec
		next.Pending = ParseRecord(FormatRecord(append(slices.Clone(rec.Pending), p.ID)))
		next.NitroCompute = o.nitroCompute()
		if next.Equal(rec) {
			return nil
		}
		fmt.Fprintf(w, "%s%s with NITRO_COMPUTE=%s (%s/%s)\n", PendingLine, p.ID, o.nitroCompute(), RecordNamespace, RecordName)
		err = writePending(ctx, w, o, next)
		if !errors.Is(err, errRecordChanged) {
			return err
		}
		if try == pendingTries {
			return fmt.Errorf("%s/%s changed between the read and the write on each of %d tries to record %s as pending: "+
				"another run is writing it; let it finish, then deploy again: %w", RecordNamespace, RecordName, pendingTries, p.ID, err)
		}
		fmt.Fprintln(w, RecordChangedLine+p.ID)
	}
}

// writePending writes r, the record with a pending entry, only while the
// ConfigMap is the one r was read from: with no ConfigMap read (no
// ResourceVersion) a kubectl create, which fails with AlreadyExists when
// one was made since; else a kubectl replace of a manifest that carries
// r.ResourceVersion, which fails with Conflict when it was written since,
// or NotFound when it was deleted since. Each refusal, as kubectl says it
// on stderr, is errRecordChanged; any
// other error is returned as it is. --save-config keeps the
// last-applied-configuration annotation that the record's later writes
// (kubectl apply, WriteRecord) merge with, so they still drop the pending
// entry.
func writePending(ctx context.Context, w io.Writer, o Options, r Record) error {
	verb, refusals := "create", []string{"(AlreadyExists)"}
	if r.ResourceVersion != "" {
		verb, refusals = "replace", []string{"(Conflict)", "(NotFound)"}
	}
	var said bytes.Buffer
	err := Kubectl(ctx, io.MultiWriter(w, &said), o, r.manifest(r.ResourceVersion), nil, verb, "--save-config", "-f", "-")
	if err != nil && slices.ContainsFunc(refusals, func(s string) bool { return strings.Contains(said.String(), s) }) {
		return fmt.Errorf("%w: %w", errRecordChanged, err)
	}
	return err
}

// checkRecordedNitroCompute refuses to deploy p, a plugin that follows
// NITRO_COMPUTE, with another NITRO_COMPUTE than the one rec holds for it:
// the objects of the first would stay behind, and the record could name
// only one of them for the removal. The second way out is the whole
// sequence: a plugin with control plane needs (mode3-iso, the fork's
// release) is refused once the removal took it out of the record, until a
// control plane deploy records it again.
func checkRecordedNitroCompute(rec Record, p Plugin, o Options) error {
	m := rec.NitroComputeOf(p)
	if m == "" || m == o.nitroCompute() {
		return nil
	}
	if err := checkRecorded(p, m); err != nil {
		return err
	}
	again := ","
	if hasNeeds(p) {
		again = fmt.Sprintf(", run --deploy-control-plane --plugins %s --nitro-compute %s again (or rerun the wizard, whose control plane step does it),",
			p.ID, o.nitroCompute())
	}
	return fmt.Errorf("plugin %s was deployed with NITRO_COMPUTE=%s (recorded in %s/%s), not %s: deploy it with --nitro-compute %s, "+
		"or remove it first with --plugin-action remove --plugins %s (which removes it with NITRO_COMPUTE=%s)%s then deploy it with --nitro-compute %s",
		p.ID, m, RecordNamespace, RecordName, o.nitroCompute(), m, p.ID, m, again, o.nitroCompute())
}

// CheckNitroRecord is the control plane deploy's check of ids, the plugins
// it is asked to carry: one that follows NITRO_COMPUTE, with another
// NITRO_COMPUTE than the one the record holds for it, is refused as its own
// deploy would refuse it (checkRecordedNitroCompute), before the control
// plane is installed again for nothing. It reads the record only when one
// of ids follows NITRO_COMPUTE, so a deploy without one reads nothing more.
func CheckNitroRecord(ctx context.Context, w io.Writer, o Options, ids []string) error {
	ps, err := Resolve(ids)
	if err != nil {
		return err
	}
	if !anyFollowsNitroCompute(ps) {
		return nil
	}
	rec, err := LoadRecord(ctx, w, o)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if err := checkRecordedNitroCompute(rec, p, o); err != nil {
			return err
		}
	}
	return nil
}

// checkRecorded refuses m, the NITRO_COMPUTE the record holds for p, when
// this installer does not know it (a newer installer wrote it).
func checkRecorded(p Plugin, m string) error {
	if err := CheckNitroCompute(m); err != nil {
		return fmt.Errorf("plugin %s: %s/%s records %s=%q, which is not %s: this installer cannot deploy, check or remove it",
			p.ID, RecordNamespace, RecordName, NitroComputeKey, m, strings.Join(NitroComputes, " or "))
	}
	return nil
}

// hasNeeds reports whether p changes the control plane install.
func hasNeeds(p Plugin) bool {
	n := p.Needs
	return len(n.Preview)+len(n.CredentialProviders)+len(n.IngressIDs) > 0 || n.Source != nil
}

// recordedNeeds is what the control plane carries: the merged needs of the
// recorded plugins (and what they require). A record this installer cannot
// resolve gives none, so only a recorded ID counts as carried.
func recordedNeeds(rec []string) Needs {
	ps, err := Resolve(rec)
	if err != nil {
		return Needs{}
	}
	n, err := Merge(ps)
	if err != nil {
		return Needs{}
	}
	return n
}

// carries reports whether n includes all of m: m's Source, when it sets
// one, is n's, and each of m's gates, providers and ingress IDs is in n.
func (n Needs) carries(m Needs) bool {
	if m.Source != nil && (n.Source == nil || *n.Source != *m.Source) {
		return false
	}
	for _, l := range [][2][]string{{n.Preview, m.Preview}, {n.CredentialProviders, m.CredentialProviders}, {n.IngressIDs, m.IngressIDs}} {
		for _, v := range l[1] {
			if !slices.Contains(l[0], v) {
				return false
			}
		}
	}
	return true
}

// Run deploys, checks or removes plugins on o's cluster. A deploy or verify
// first runs every resolved plugin's Check on the environment, and a
// refusal stops it before any child starts.
//
//   - deploy: ids and what they require, in order. Each plugin's AWS half
//     (`setup-aws plugin <id>`), then Deploy, then Verify; then it is
//     recorded. A plugin with Needs is refused unless the control plane
//     already carries them (its ID is in the record, or the recorded plugins'
//     needs include its own, as mode3's do mode3-iso's), since its objects
//     would not work: run --deploy-control-plane --plugins <id> first, or,
//     for one that replaces the release, remove the control plane and install
//     again. A plugin that follows NITRO_COMPUTE (Plugin.FollowsNitroCompute)
//     is recorded as pending, with its NITRO_COMPUTE, just before its first
//     write (Options.BeforeWrite), and is refused with another
//     NITRO_COMPUTE than the one recorded for it, there too: the pending
//     write succeeds only while the record is as it was read (markPending).
//   - verify: Verify for ids and what they require. A plugin that follows
//     NITRO_COMPUTE is checked with the recorded one when there is one.
//   - remove: ids (every recorded plugin, installed or pending, when ids is
//     empty), in reverse order: Remove, then `setup-aws plugin <id>
//     --delete`, then dropped from the record, read again first so that
//     what another run recorded meanwhile stays. A plugin that follows
//     NITRO_COMPUTE is removed with the recorded one when there is one. A
//     recorded plugin that requires one being removed stops it, and so does
//     an AWS refusal, which `setup-aws plugin <id> --delete --plan` finds for
//     every plugin before anything is removed.
//
// A record with data keys this installer does not know (a newer
// installer's, Record.CheckKeys) stops a deploy or a removal before its
// first write: every write of the record would drop them. Verify writes
// nothing, so it runs.
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
		// Verify checks a plugin that follows NITRO_COMPUTE with the value
		// its deploy ran with, as removal does: the record is read first
		// (a read only), so the Auto Mode check below judges that value,
		// not the run's.
		var rec Record
		if action == ActionVerify && anyFollowsNitroCompute(ps) {
			if rec, err = LoadRecord(ctx, w, o); err != nil {
				return err
			}
		}
		for _, p := range ps {
			po := o
			if m := rec.NitroComputeOf(p); m != "" && checkRecorded(p, m) == nil {
				po.NitroCompute = m
			}
			if why := p.AutoModeProblem(po); why != "" {
				return fmt.Errorf("plugin %s %s; %s has EKS Auto Mode off", p.ID, why, o.Cluster)
			}
			// Before its AWS half, which would otherwise be made for an
			// install it cannot work on, or for none.
			if p.NeedsRDS != "" && o.BundledDatabase {
				return fmt.Errorf("plugin %s %s; %s's control plane uses the bundled PostgreSQL: install it with --database rds to use the plugin",
					p.ID, p.NeedsRDS, o.Cluster)
			}
			if p.NeedsRDS != "" && o.Running == "" {
				return fmt.Errorf("plugin %s %s; no control plane is installed on %s (or the release it runs cannot be read): run --deploy-control-plane --database rds first",
					p.ID, p.NeedsRDS, o.Cluster)
			}
		}
		// Before the record is read and before any plugin's AWS half or
		// hooks: a setting one of them cannot work with stops the run with
		// nothing done (review of 86ba13f, finding 3).
		if err := CheckSettings(ps, os.LookupEnv); err != nil {
			return err
		}
		// So do missing nodes: its Pods would stay Pending on nodes that do
		// not exist.
		for _, p := range ps {
			if miss := MissingNodes(p, o.Pools); len(miss) > 0 {
				return fmt.Errorf("plugin %s runs on the %s node group, and %s has no node labeled %s=%s: a cluster gets its node groups from the sandbox classes it is created with",
					p.ID, strings.Join(miss, " and "), o.Cluster, PoolLabel, strings.Join(miss, ","))
			}
		}
		if action == ActionVerify {
			// rec (read above, only for a plugin that follows NITRO_COMPUTE):
			// each is checked with the value its deploy ran with.
			for _, p := range ps {
				fmt.Fprintln(w, VerifyLine+p.ID)
				po, err := withRecordedNitroCompute(w, rec, p, o, "checked")
				if err != nil {
					return err
				}
				if p.Verify != nil {
					if err := p.Verify(ctx, w, po); err != nil {
						return fmt.Errorf("plugin %s: %w", p.ID, err)
					}
				}
				fmt.Fprintln(w, ReadyLine+p.ID)
			}
			return nil
		}
		rec, err = LoadRecord(ctx, w, o)
		if err != nil {
			return err
		}
		// Before any write, cluster or AWS: the record writes below would
		// drop keys a newer installer wrote.
		if err := rec.CheckKeys(); err != nil {
			return err
		}
		// Only the installed plugins carry needs: a pending one's deploy
		// did not finish.
		carried := recordedNeeds(rec.Plugins)
		for _, p := range ps {
			if err := checkRecordedNitroCompute(rec, p, o); err != nil {
				return err
			}
			if !hasNeeds(p) || slices.Contains(rec.Plugins, p.ID) || carried.carries(p.Needs) {
				continue
			}
			if p.Needs.Source != nil {
				// Its release replaces the one installed, which a deploy of
				// the control plane refuses to change.
				return fmt.Errorf("plugin %s installs another Substrate release than the control plane runs: %s", p.ID, ReleaseFix(p.ID))
			}
			return fmt.Errorf("plugin %s changes the control plane install, which does not carry it yet: run --deploy-control-plane --plugins %s first", p.ID, p.ID)
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

// deployOne is p's AWS half, Deploy and Verify. A plugin that follows
// NITRO_COMPUTE is recorded as pending once, just before its first write:
// before its AWS half when it has one, else when Deploy calls
// o.BeforeWrite after its read-only checks, so a Deploy that refuses after
// its reads leaves the record as it was. A Deploy that returns without
// calling it is recorded then, before Verify, so its NITRO_COMPUTE is
// recorded all the same.
func deployOne(ctx context.Context, w io.Writer, p Plugin, o Options) error {
	fmt.Fprintln(w, DeployLine+p.ID)
	marked := false
	o.BeforeFirstWrite = func() error {
		if marked {
			return nil
		}
		if err := markPending(ctx, w, o, p); err != nil {
			return err
		}
		marked = true
		return nil
	}
	if p.SetupAWS {
		if err := o.BeforeWrite(); err != nil {
			return err
		}
		if err := SetupAWS(ctx, w, o, "plugin", p.ID); err != nil {
			return err
		}
	}
	if p.Deploy != nil {
		if err := p.Deploy(ctx, w, o); err != nil {
			return err
		}
	}
	if err := o.BeforeWrite(); err != nil {
		return err
	}
	fmt.Fprintln(w, VerifyLine+p.ID)
	if p.Verify != nil {
		return p.Verify(ctx, w, o)
	}
	return nil
}

// RecordedNitroComputeLine starts the line Run's verify and remove print
// when they check or remove a plugin with the NITRO_COMPUTE the record
// holds for it, and the run's is another. It is followed by the plugin ID.
const RecordedNitroComputeLine = "NITRO_COMPUTE recorded at deploy: "

// anyFollowsNitroCompute reports whether one of ps follows NITRO_COMPUTE
// (mode3-iso): only then does a run need the record's nitro-compute.
func anyFollowsNitroCompute(ps []Plugin) bool {
	return slices.ContainsFunc(ps, func(p Plugin) bool { return p.FollowsNitroCompute })
}

// withRecordedNitroCompute is o with the NITRO_COMPUTE rec holds for p, a
// plugin that follows it, saying so when the run's is another (what is
// "checked" or "removed"); o itself when rec holds none (a record an older
// installer wrote). One this installer does not know is refused.
func withRecordedNitroCompute(w io.Writer, rec Record, p Plugin, o Options, what string) (Options, error) {
	m := rec.NitroComputeOf(p)
	if m == "" {
		return o, nil
	}
	if err := checkRecorded(p, m); err != nil {
		return o, err
	}
	if m != o.nitroCompute() {
		fmt.Fprintf(w, "%s%s was deployed with NITRO_COMPUTE=%s, as %s/%s records, so it is %s with %s, not the run's %s\n",
			RecordedNitroComputeLine, p.ID, m, RecordNamespace, RecordName, what, m, o.nitroCompute())
	}
	o.NitroCompute = m
	return o, nil
}

func remove(ctx context.Context, w io.Writer, ids []string, o Options) error {
	rec, err := LoadRecord(ctx, w, o)
	if err != nil {
		return err
	}
	// Before anything is removed: each removal ends in a write of the
	// record, which would drop keys a newer installer wrote.
	if err := rec.CheckKeys(); err != nil {
		return err
	}
	// The pending plugins too: a deploy that failed may have made some of
	// their objects.
	all := rec.All()
	if len(ids) == 0 {
		ids = all
	}
	var ps []Plugin
	for _, id := range ids {
		p, ok := Get(id)
		if !ok {
			// A record written by a newer installer: say so and leave it.
			return fmt.Errorf("unknown plugin %q (recorded in %s/%s): this installer cannot remove it", id, RecordNamespace, RecordName)
		}
		// So is a NITRO_COMPUTE this installer does not know.
		if m := rec.NitroComputeOf(p); m != "" {
			if err := checkRecorded(p, m); err != nil {
				return err
			}
		}
		ps = append(ps, p)
	}
	// Refuse to remove what a plugin that stays still requires.
	for _, keep := range all {
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
	// Ask AWS first, for every plugin: `--delete --plan` finds each refusal
	// (a same-named resource with another owner) and deletes nothing, so a
	// refusal stops the removal before any Kubernetes object is gone, rather
	// than leaving a plugin half removed (review R5).
	for _, p := range ps {
		if !p.SetupAWS {
			continue
		}
		fmt.Fprintln(w, PlanLine+p.ID)
		if err := SetupAWS(ctx, w, o, "plugin", p.ID, "--delete", "--plan"); err != nil {
			return fmt.Errorf("plugin %s: its AWS resources cannot be deleted, so nothing was removed: %w", p.ID, err)
		}
	}
	// Each Remove runs on the release the control plane runs (review N1),
	// which is another than o's when a fork's plugin was removed earlier or
	// --tag changed since the install. The advice after each removal still
	// compares it with o's: the release a control plane deploy would install.
	on := o
	if on.Release = o.RunningRelease(); len(ps) > 0 && on.Tag != o.Tag {
		fmt.Fprintf(w, "Removing on Substrate %s, the release %s runs (not %s)\n", on.Tag, o.Cluster, o.Tag)
	}
	// cur is the record as this removal last wrote it (main's behavior).
	// reread: the record had the keys a plugin that follows NITRO_COMPUTE
	// brings, or one is removed; then each write starts from a fresh read.
	cur := rec
	reread := len(rec.Pending) > 0 || rec.NitroCompute != "" || anyFollowsNitroCompute(ps)
	for _, p := range ps {
		fmt.Fprintln(w, RemoveLine+p.ID)
		// A plugin that follows NITRO_COMPUTE is removed with the one its
		// deploy ran with, when the record holds it; else (a record an
		// older installer wrote) with the run's.
		pon, err := withRecordedNitroCompute(w, rec, p, on, "removed")
		if err != nil {
			return err
		}
		if p.Remove != nil {
			if err := p.Remove(ctx, w, pon); err != nil {
				return fmt.Errorf("plugin %s: %w", p.ID, err)
			}
		}
		if p.SetupAWS {
			if err := SetupAWS(ctx, w, o, "plugin", p.ID, "--delete"); err != nil {
				return fmt.Errorf("plugin %s: %w", p.ID, err)
			}
		}
		// For a plugin that follows NITRO_COMPUTE, or a record with the
		// keys it brings, the record as it is now, read again: such a Remove
		// can take minutes (mode3-iso waits for its nodes), and what another
		// run recorded meanwhile stays. Any other removal writes from the
		// record it read, as before, with no extra read.
		left := cur
		if reread {
			if left, err = LoadRecord(ctx, w, o); err != nil {
				return fmt.Errorf("plugin %s was removed, but the record still lists it: %w", p.ID, err)
			}
		}
		left.Plugins, left.Pending = without(left.Plugins, p.ID), without(left.Pending, p.ID)
		if p.FollowsNitroCompute {
			left.NitroCompute = ""
		}
		if err := WriteRecord(ctx, w, o, left); err != nil {
			return err
		}
		cur = left
		fmt.Fprintln(w, RemovedLine+p.ID)
		switch {
		case p.Needs.Source != nil:
			// The release stays: a control plane deploy refuses to change
			// it, so the fix is a reinstall.
			fmt.Fprintf(w, NeedsKeptLine+"%s, its Substrate release %s among them, and so does every worker pool; to leave that release, %s (the record no longer lists %s)\n",
				p.ID, p.Needs.Source.Tag, ReleaseFix(p.ID), p.ID)
		case hasNeeds(p) && o.Running != "" && o.Running != o.Tag:
			// The control plane still runs another release (a plugin that
			// replaced it was removed), so a control plane deploy, which
			// would drop these needs, is refused: the fix is the reinstall.
			fmt.Fprintf(w, NeedsKeptLine+"%s; it runs Substrate %s, not the install's %s, so a control plane deploy is refused: to drop them, %s (the record no longer lists %s)\n",
				p.ID, o.Running, o.Tag, ReinstallFix(o.Running, o.Tag), p.ID)
		case hasNeeds(p):
			// Run has no buckets or database to reinstall the control plane
			// with, so it says what still runs and how to drop it.
			fmt.Fprintf(w, NeedsKeptLine+"%s; run --deploy-control-plane again to drop them (the record no longer lists %s)\n", p.ID, p.ID)
		}
	}
	return nil
}
