// Package plugin is the contract every optional part of an install follows:
// the LLM gateway, EFS and S3 Files volumes, Secrets Manager login rotation,
// the Secrets Manager credential provider, OpenClaw, and mode 3. Each lives in
// its own file (or subpackage) and registers itself with Register from an
// init function, so plugins are built in parallel without touching each
// other's files. It is the counterpart of substrate-gke's optional steps
// (Filestore CSI, autoscaling).
//
// A plugin has two halves:
//   - AWS resources, made by `setup-aws plugin <id>` (tools/setup-aws), which
//     also teardown and inventory know; Plugin.SetupAWS says it has one.
//   - Kubernetes objects and checks, made by Deploy and Verify here, run by Run
//     as `substrate-eks --plugins <id> --plugin-action deploy|verify|remove`.
//
// A plugin's manifests live in its own package directory and are compiled in
// with //go:embed, so the installer binary carries them and no path is
// searched at run time.
//
// What a plugin needs from the control plane install (a preview gate, a
// credential provider, ingress clients, another Substrate build) is declared
// in Needs and merged into the one `ate-setup deploy ate-system` the install
// runs, so choosing plugins in the wizard installs the control plane once.
// The set of installed plugins is recorded in the cluster (RecordName), so a
// later control plane deploy keeps every installed plugin's needs.
package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// Source replaces the Substrate release the control plane installs (mode 3
// installs a fork's build). Empty fields keep the default.
type Source struct {
	Repo, Commit, Tag, ImageRepo string
}

// UpstreamRepo is upstream Substrate's repository: the tree of every release
// no plugin replaces.
const UpstreamRepo = "https://github.com/agent-substrate/substrate.git"

// Release is the one Substrate release an install runs. The control plane,
// the demo's and every plugin's worker pools, and every image or binary
// built from the tree (the counter, the credential provider's probe,
// kubectl-ate) all use it: a worker image of another release can reject what
// the control plane gives it (the mode 3 fork's controller passes every
// preview gate it enables to every worker, and a v1.0.0 worker exits on
// NetworkWorkers). ReleaseOf decides it.
type Release struct {
	// Tag and ImageRepo name its images, <ImageRepo>/<image>:<Tag>.
	Tag, ImageRepo string
	// Repo checked out at Ref is its tree: Ref is a Source's pinned commit,
	// else the tag.
	Repo, Ref string
	// Tree is where the tree is cached with this install's EKS overlay
	// applied (Locate); empty until located. Every step that reads the tree
	// (the control plane deploy and delete, the demo, kubectl-ate and image
	// builds, the plugins) reads it here.
	Tree string
}

// Locate is r with Tree set: under root, the tree of r's tag with the overlay
// whose digest is overlay (eksoverlay.Digest) applied for region. The overlay
// is applied in place, so the name carries a short digest of the overlay,
// the Region and r's repository and ref: two checkouts with different
// deploy/eks, or two Regions, get two trees, and the same ones reuse one.
func (r Release) Locate(root, overlay, region string) Release {
	h := sha256.Sum256([]byte(strings.Join([]string{r.Repo, r.Ref, overlay, region}, "\x00")))
	r.Tree = filepath.Join(root, "substrate-"+r.Tag+"-"+hex.EncodeToString(h[:4]))
	return r
}

// Release is the release of an install with these needs: n's Source when a
// plugin set one, else tag from imageRepo on upstream's tree.
func (n Needs) Release(tag, imageRepo string) Release {
	r := Release{Tag: tag, ImageRepo: imageRepo, Repo: UpstreamRepo, Ref: tag}
	if s := n.Source; s != nil {
		if s.Repo != "" {
			r.Repo = s.Repo
		}
		if s.Tag != "" {
			r.Tag, r.Ref = s.Tag, s.Tag
		}
		if s.ImageRepo != "" {
			r.ImageRepo = s.ImageRepo
		}
		if s.Commit != "" {
			r.Ref = s.Commit
		}
	}
	return r
}

// ReleaseOf is the release of an install carrying ps, the chosen and the
// recorded plugins alike: the Source the plugins that set one share, else
// the --tag release (tag, imageRepo).
func ReleaseOf(ps []Plugin, tag, imageRepo string) (Release, error) {
	n, err := Merge(ps)
	if err != nil {
		return Release{}, err
	}
	return n.Release(tag, imageRepo), nil
}

// ChosenRelease is ReleaseOf the plugins ids resolve to, for the wizard,
// which shows it and passes the steps the --tag, --image-repo and --plugins
// that decide it. IDs that do not resolve keep the --tag release: the wizard
// offers only registered plugins, whose Sources are all one release.
func ChosenRelease(ids []string, tag, imageRepo string) Release {
	ps, err := Resolve(ids)
	if err == nil {
		if r, err := ReleaseOf(ps, tag, imageRepo); err == nil {
			return r
		}
	}
	return Needs{}.Release(tag, imageRepo)
}

// SourceOf is the first plugin (by Order) whose Source installs release tag,
// or "" when no registered plugin's does (or tag is empty).
func SourceOf(tag string) string {
	if ids := SourcesOf(tag); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// SourcesOf is every plugin whose Source installs release tag, by Order
// (mode3 and mode3-iso both install the fork).
func SourcesOf(tag string) []string {
	var ids []string
	for _, p := range All() {
		if s := p.Needs.Source; s != nil && tag != "" && s.Tag == tag {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// RunningRelease is the release o's control plane runs, located like o's,
// for removing what runs on it: o's own release when Running is its tag or
// is not known (""), else the Source of the plugin that installs Running, or
// Running's tag on upstream's tree from o's image repository. A removal
// never runs on --tag or SUBSTRATE_TAG when the cluster runs another release
// (review N1): that release's kubectl-ate and ate-setup would act on what
// another release made, and an overlay patch that does not apply to the
// --tag release would block the way out.
func (o Options) RunningRelease() Release {
	if o.Running == "" || o.Running == o.Tag {
		return o.Release
	}
	var n Needs
	if p, ok := Get(SourceOf(o.Running)); ok {
		n = p.Needs
	}
	return n.Release(o.Running, o.ImageRepo).Locate(o.TreeRoot, o.OverlayDigest, o.Region)
}

// ReleaseFix is how to change the release of an installed control plane,
// which --deploy-control-plane refuses: remove the control plane, then
// install again with or without plugin id, whose Source is one of the two.
func ReleaseFix(id string) string {
	return "run --remove-control-plane, then install again with or without --plugins " + id
}

// ReinstallFix is ReleaseFix for a control plane that runs release have
// where the install's release is want: with or without the plugins whose
// Source is one of the two (each one named, so an install with mode3-iso
// alone is not told about mode3 only), else at want's tag.
func ReinstallFix(have, want string) string {
	for _, tag := range []string{have, want} {
		if ids := SourcesOf(tag); len(ids) > 0 {
			return ReleaseFix(strings.Join(ids, " or "))
		}
	}
	return "run --remove-control-plane, then install again with --tag " + want
}

// Needs is what a plugin adds to `ate-setup deploy ate-system`.
type Needs struct {
	// Preview gates (ate-setup --preview), e.g. "ExternalVolumes".
	Preview []string
	// CredentialProviders are extra JSON objects for --credential-provider,
	// after the bundled {"name":"k8s.io"}.
	CredentialProviders []string
	// IngressIDs are SPIFFE IDs the ingress router admits, beyond
	// spiffe://cluster.local/ns/ate-system/sa/atenet-client.
	IngressIDs []string
	// Source, when set, replaces the release. Plugins installed together
	// must set the same one, or none (Merge).
	Source *Source
}

// Options is what Deploy, Verify and Remove get.
type Options struct {
	Cluster, Region, Profile string
	// Release is the install's one release (ReleaseOf), already decided and
	// located by the caller: a worker pool runs
	// <ImageRepo>/ateworker-<class>:<Tag>, and what is built from the tree is
	// built from Tree, fetched from Repo at Ref when missing.
	Release
	// TreeRoot is where the trees are cached, and other sources a plugin
	// fetches (the credential provider's) beside them.
	TreeRoot string
	// OverlayDigest is eksoverlay.Digest of deploy/eks, which Locate keys
	// the tree by.
	OverlayDigest string
	// Running is the release tag the installed control plane runs, as
	// --probe-installed reads it ("" when none is installed or its release
	// cannot be read). Run's remove runs each Remove on it (RunningRelease)
	// and compares it with Tag: a control plane still on another release
	// (its plugin removed, or --tag changed since the install) is left only
	// by a reinstall. Run refuses a NeedsRDS plugin while it is "".
	Running string
	// Kubeconfig is the installer's own kubeconfig for the cluster
	// (awsx.KubeconfigPath), already written by the caller; its current
	// context is the cluster. Use Kubectl, which passes it.
	Kubeconfig string
	// SetupAWS is native/tools/setup-aws, or a release's setup-aws binary
	// (SetupAWSArgv); Run runs `setup-aws plugin <id>` from it for a plugin
	// with SetupAWS set.
	SetupAWS string
	// Tags is TAGS: space-separated key=value pairs every AWS resource gets
	// besides the owner tag (OwnerTags adds it).
	Tags string
	// AutoMode is the cluster's EKS Auto Mode, read off the cluster.
	AutoMode bool
	// Pools are the ate.dev/pool values of the cluster's nodes (NodePools),
	// read by the caller: the node groups a plugin's Nodes are checked
	// against.
	Pools []string
	// BundledDatabase is a control plane on ate-setup's bundled PostgreSQL
	// (BundledDatabase), read by the caller: Run refuses a plugin with
	// NeedsRDS there.
	BundledDatabase bool
	// NitroCompute is NITRO_COMPUTE (--nitro-compute): where the AWS Nitro
	// sandbox class runs, NitroAutoMode (EKS Auto Mode's nodes, the default;
	// empty reads as it) or NitroNodeGroup (the nitro node group).
	NitroCompute string
	// BeforeFirstWrite, when set, is what Deploy calls (BeforeWrite) after
	// its read-only checks and just before its first write to the cluster.
	// Run's deploy sets it to record a plugin that follows NITRO_COMPUTE as
	// pending (Plugin.FollowsNitroCompute), so a Deploy that refuses after
	// its reads leaves the record as it was.
	BeforeFirstWrite func() error
}

// BeforeWrite runs o.BeforeFirstWrite when it is set. A Deploy that follows
// NITRO_COMPUTE calls it just before it creates or applies anything.
func (o Options) BeforeWrite() error {
	if o.BeforeFirstWrite == nil {
		return nil
	}
	return o.BeforeFirstWrite()
}

// NITRO_COMPUTE's values: the AWS Nitro sandbox class (mode3-iso) on nodes
// EKS Auto Mode launches, or on the nitro node group setup-aws makes from a
// Nitro Sandbox AMI, which needs no Auto Mode.
const (
	NitroAutoMode  = "auto-mode"
	NitroNodeGroup = "node-group"
)

// NitroComputes are NITRO_COMPUTE's values; NitroAutoMode is the default.
var NitroComputes = []string{NitroAutoMode, NitroNodeGroup}

// nitroCompute is o's NITRO_COMPUTE, NitroAutoMode when it is empty.
func (o Options) nitroCompute() string {
	if o.NitroCompute == "" {
		return NitroAutoMode
	}
	return o.NitroCompute
}

// CheckNitroCompute refuses a NITRO_COMPUTE that is not one of NitroComputes.
func CheckNitroCompute(v string) error {
	if !slices.Contains(NitroComputes, v) {
		return fmt.Errorf("NITRO_COMPUTE=%q is not %s", v, strings.Join(NitroComputes, " or "))
	}
	return nil
}

// PoolLabel is the label of the node groups' nodes: system, gvisor, microvm
// or nitro (and isolated, on the AWS Nitro sandbox class's Auto Mode nodes).
const PoolLabel = "ate.dev/pool"

// PoolsArgs are the kubectl arguments that list the PoolLabel values of the
// cluster's nodes, which ParsePools reads.
var PoolsArgs = []string{"get", "nodes", "-l", PoolLabel, "-o", `jsonpath={.items[*].metadata.labels.ate\.dev/pool}`}

// ParsePools is the sorted, distinct pools PoolsArgs printed.
func ParsePools(out string) []string {
	pools := strings.Fields(out)
	slices.Sort(pools)
	return slices.Compact(pools)
}

// MissingNodes are the groups of p's Nodes that pools lack.
func MissingNodes(p Plugin, pools []string) []string {
	var out []string
	for _, n := range p.Nodes {
		if !slices.Contains(pools, n) {
			out = append(out, n)
		}
	}
	return out
}

// ForClass are the registered plugins the sandbox class installs, by Order.
func ForClass(class string) []Plugin {
	var out []Plugin
	for _, p := range All() {
		if class != "" && p.Class == class {
			out = append(out, p)
		}
	}
	return out
}

// Plugin is one optional part of an install.
type Plugin struct {
	// ID is the --plugin value and the setup-aws subcommand name.
	ID, Title, Summary string
	// Order places it in lists and in the install (lower first). It must be
	// higher than the Order of every plugin it Requires (Check).
	Order int
	// Minutes is the rough time Deploy plus Verify take, for the Review screen.
	Minutes int
	// Default plugins are preselected in the wizard (the LLM gateway), but
	// not on a cluster the run creates when choosing one would add a node
	// group: the gateway's system group on Auto Mode with the bundled
	// PostgreSQL or the AWS Nitro sandbox alone.
	Default bool
	// Requires are plugin IDs that must be installed first (OpenClaw keeps
	// its gateway state on EFS).
	Requires []string
	// SetupAWS reports that `setup-aws plugin <ID>` makes AWS resources.
	SetupAWS bool
	// NeedsAutoMode, when set, is why the plugin needs EKS Auto Mode: the
	// wizard offers it only on an Auto Mode cluster, and Run refuses to
	// deploy it on any other (AutoModeProblem).
	NeedsAutoMode string
	// AutoModeOptional, when set, reports the options under which a plugin
	// with NeedsAutoMode runs without Auto Mode all the same (mode3-iso with
	// NITRO_COMPUTE=node-group, on the nitro node group).
	AutoModeOptional func(Options) bool
	// FollowsNitroCompute reports that Deploy, Verify and Remove act on
	// Options.NitroCompute (mode3-iso), so the record keeps the one a deploy
	// ran with. Run's deploy writes the plugin as pending, with its
	// NITRO_COMPUTE, just before the plugin's first write: before its AWS
	// half, or when Deploy calls Options.BeforeWrite after its read-only
	// checks, and at the latest once Deploy returns. It refuses another
	// NITRO_COMPUTE than the one recorded for it. Run's verify and remove run
	// Verify and Remove with the recorded one. Only such a plugin is ever
	// pending, so a deploy of any other writes the record as before.
	FollowsNitroCompute bool
	// NeedsRDS, when set, is why the plugin needs the RDS database (login
	// rotation rotates its logins): the wizard offers it only with RDS, and
	// Run refuses to deploy or check it, before any child, on a cluster
	// whose control plane uses the bundled PostgreSQL
	// (Options.BundledDatabase) or that has no control plane (Options.Running
	// empty).
	NeedsRDS string
	// NeedsDemo reports that Deploy builds on the counter demo (its actor
	// templates): choosing it in the wizard turns the demo on, and the demo
	// runs before the plugins. Deploy itself refuses when the demo is
	// missing, so --plugin-action deploy says so too.
	NeedsDemo bool
	// Nodes are the node groups its Pods are pinned to, by their
	// ate.dev/pool label (system, gvisor, microvm). The wizard offers it
	// only when the run makes those groups or the cluster has their nodes,
	// and Run refuses to deploy or check it on a cluster whose nodes
	// (Options.Pools) lack one. Choosing one with system makes the system
	// group on a cluster the run creates (nodegroups.For).
	Nodes []string
	// AddsNodes: choosing it makes its Nodes' gvisor and microvm groups on a
	// cluster the run creates, as their sandbox classes do (mode3 makes the
	// microvm group even without the microvm class).
	AddsNodes bool
	// Class is the sandbox class that installs it: such a plugin is not on
	// the wizard's Plugins screen, and choosing the class in Settings (or
	// --sandbox-classes) adds it (mode3-iso is the AWS Nitro sandbox class,
	// "nitro").
	Class string
	Needs Needs
	// HourlyUSD is the rough on-demand cost the plugin adds, for the Review
	// screen (0 when it is only Kubernetes objects).
	HourlyUSD float64
	// Check, when set, refuses settings Deploy and Verify cannot work with
	// (an OPENCLAW_*_IMAGE that is not pinned by digest), reading each with
	// lookup. It starts no child. CheckSettings runs it: Run before anything
	// else, the --plugin-action subcommand before it reads the cluster, and
	// the wizard when y on Review would start the install, with the config
	// files' values, so a bad setting stops a run before any AWS or kubectl
	// call.
	Check func(lookup func(key string) (string, bool)) error
	// Deploy makes the plugin's Kubernetes objects, Verify checks it works
	// (a quick check fit for the install), Remove deletes its Kubernetes
	// objects (Run then deletes its AWS resources with `setup-aws plugin <id>
	// --delete`). All must be idempotent, start every child from an argv
	// (Exec), and print no secret.
	Deploy, Verify, Remove func(ctx context.Context, w io.Writer, o Options) error
}

// AutoModeProblem is why p cannot run on a cluster with o's Auto Mode
// (o.AutoMode) and settings (AutoModeOptional reads them), or "": its
// NeedsAutoMode, on a cluster without Auto Mode, unless AutoModeOptional
// lets it run there.
func (p Plugin) AutoModeProblem(o Options) string {
	if p.NeedsAutoMode == "" || o.AutoMode || (p.AutoModeOptional != nil && p.AutoModeOptional(o)) {
		return ""
	}
	return p.NeedsAutoMode
}

// CheckSettings runs Check for each of ps (Resolve's, so what they require
// too) with lookup, and returns the first refusal, naming its plugin.
func CheckSettings(ps []Plugin, lookup func(string) (string, bool)) error {
	for _, p := range ps {
		if p.Check == nil {
			continue
		}
		if err := p.Check(lookup); err != nil {
			return fmt.Errorf("plugin %s: %w", p.ID, err)
		}
	}
	return nil
}

var registry = map[string]Plugin{}

// Register adds p. It panics on a duplicate or empty ID, which is a
// programming error caught by any test that imports the package.
func Register(p Plugin) {
	if p.ID == "" || strings.ContainsAny(p.ID, " ,=") {
		panic(fmt.Sprintf("plugin: bad ID %q", p.ID))
	}
	if _, dup := registry[p.ID]; dup {
		panic("plugin: duplicate ID " + p.ID)
	}
	registry[p.ID] = p
}

// All returns every registered plugin by Order, then ID.
func All() []Plugin {
	out := make([]Plugin, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Check reports a registry mistake: a required plugin that is not registered,
// or one whose Order is not below the plugin requiring it (Run removes in
// reverse Order, so a plugin must go before what it requires). A test that
// imports every plugin calls it.
func Check() error {
	for _, p := range All() {
		for _, r := range p.Requires {
			q, ok := registry[r]
			if !ok {
				return fmt.Errorf("plugin %s requires unknown plugin %q", p.ID, r)
			}
			if q.Order >= p.Order {
				return fmt.Errorf("plugin %s (order %d) requires %s (order %d): its Order must be higher", p.ID, p.Order, r, q.Order)
			}
		}
	}
	return nil
}

// OwnerTags is the owner tag plus Tags, for a plugin that tags what it makes
// through Kubernetes (a StorageClass's tagSpecification).
func (o Options) OwnerTags() (map[string]string, error) {
	m := map[string]string{awsx.OwnerTagKey: o.Cluster + "." + o.Region}
	for _, kv := range strings.Fields(o.Tags) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("TAGS: %q is not key=value", kv)
		}
		m[k] = v
	}
	return m, nil
}

// Get returns the plugin with id.
func Get(id string) (Plugin, bool) {
	p, ok := registry[id]
	return p, ok
}

// Resolve returns ids plus everything they require, in install order, or an
// error naming an unknown ID.
func Resolve(ids []string) ([]Plugin, error) {
	want := map[string]bool{}
	var add func(id string, chain []string) error
	add = func(id string, chain []string) error {
		if slices.Contains(chain, id) {
			return fmt.Errorf("plugin %s requires itself (%s)", id, strings.Join(append(chain, id), " -> "))
		}
		p, ok := registry[id]
		if !ok {
			return fmt.Errorf("unknown plugin %q", id)
		}
		want[id] = true
		for _, r := range p.Requires {
			if err := add(r, append(chain, id)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := add(id, nil); err != nil {
			return nil, err
		}
	}
	var out []Plugin
	for _, p := range All() {
		if want[p.ID] {
			out = append(out, p)
		}
	}
	return out, nil
}

// Merge is the union of ps' needs, each list sorted and without repeats.
// Plugins may set the same Source (mode3 and mode3-iso both install the
// fork); two different Sources are an error naming both plugins.
func Merge(ps []Plugin) (Needs, error) {
	var n Needs
	from := ""
	for _, p := range ps {
		n.Preview = append(n.Preview, p.Needs.Preview...)
		n.CredentialProviders = append(n.CredentialProviders, p.Needs.CredentialProviders...)
		n.IngressIDs = append(n.IngressIDs, p.Needs.IngressIDs...)
		switch s := p.Needs.Source; {
		case s == nil:
		case n.Source == nil:
			c := *s
			n.Source, from = &c, p.ID
		case *n.Source != *s:
			return Needs{}, fmt.Errorf("plugins %s and %s install two different Substrate releases (%s and %s); choose one",
				from, p.ID, n.Source.Tag, s.Tag)
		}
	}
	for _, l := range []*[]string{&n.Preview, &n.CredentialProviders, &n.IngressIDs} {
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
	return n, nil
}

// RecordNamespace/RecordName are the ConfigMap that records which plugins are
// installed, in kube-system so that removing ate-system keeps it. Its data
// keys:
//   - RecordKey holds a comma-separated, sorted list of the installed IDs.
//     The control plane deploy adds the plugins whose needs it now carries,
//     Run's deploy adds a plugin once it is deployed and checked, and Run's
//     remove drops it. Its format is the one older installers read.
//   - PendingKey lists, the same way, each plugin that follows NITRO_COMPUTE
//     (Plugin.FollowsNitroCompute) whose deploy has started and not
//     finished: Run's deploy writes it just before the plugin's first
//     write, only while the record is as it read it (markPending), and
//     AddToRecord moves it to RecordKey, after Run's deploy and
//     check, or in the control plane deploy's record. A plugin the control
//     plane deploy recorded first is in both keys while its own deploy
//     runs, and after it fails. A failed deploy leaves it there, and a
//     removal of every plugin removes it too.
//   - NitroComputeKey is the NITRO_COMPUTE such a plugin was (being)
//     deployed with, written with its pending entry. Its removal runs with
//     it, and drops it.
//
// The last two are written only when used, so a record without them is the
// one an older installer, or a deploy of other plugins, writes.
const (
	RecordNamespace = "kube-system"
	RecordName      = "substrate-eks-plugins"
	RecordKey       = "plugins"
	PendingKey      = "pending"
	NitroComputeKey = "nitro-compute"
)

// ParseRecord reads the record's data value into IDs.
func ParseRecord(v string) []string {
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// FormatRecord is the record's data value for ids.
func FormatRecord(ids []string) string {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	return strings.Join(slices.Compact(ids), ",")
}

// Record is what the record ConfigMap holds.
type Record struct {
	// Plugins are the installed plugins (RecordKey), Pending the ones whose
	// deploy has started and not finished (PendingKey), each sorted.
	Plugins, Pending []string
	// NitroCompute is NitroComputeKey: "" when the record has none (one an
	// older installer wrote, or with no plugin that follows NITRO_COMPUTE).
	NitroCompute string
	// ResourceVersion is the ConfigMap's metadata.resourceVersion as it was
	// read: "" when there is no ConfigMap (the API server gives every object
	// one). Only the pending write uses it, as its precondition
	// (markPending); Equal ignores it and Manifest does not write it.
	ResourceVersion string
	// Unknown are the record's data keys this installer does not know (a
	// newer installer's), sorted. Every write of the record would drop
	// them: the pending write's replace sends the whole ConfigMap, an apply
	// drops a key its last-applied configuration has and the manifest
	// lacks, and a delete drops all. So while there are some, nothing writes
	// the record (CheckKeys). Equal ignores it and Manifest does not write
	// them.
	Unknown []string
}

// CheckKeys refuses r while it holds data keys this installer does not
// know (Unknown): a newer installer wrote them, and a write of the record
// by this one would drop them. Every path that writes or deletes the
// record checks it before its first write, cluster or AWS.
func (r Record) CheckKeys() error {
	if len(r.Unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%s/%s holds data keys this installer does not know (%s), which a write of the record by this installer would drop: use the installer that wrote them (a newer one)",
		RecordNamespace, RecordName, strings.Join(r.Unknown, ", "))
}

// RecordArgs are the kubectl arguments that print the record ConfigMap as
// JSON, or nothing when it is missing (ParseRecordJSON).
var RecordArgs = []string{"-n", RecordNamespace, "get", "configmap", RecordName, "--ignore-not-found", "-o", "json"}

// ParseRecordJSON reads what RecordArgs printed: nothing is no record, and
// data keys it does not know go to Unknown, so a record a newer installer
// wrote stays readable. Anything else it cannot read is an error.
func ParseRecordJSON(js []byte) (Record, error) {
	if len(bytes.TrimSpace(js)) == 0 {
		return Record{}, nil
	}
	var cm struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(js, &cm); err != nil {
		return Record{}, fmt.Errorf("read ConfigMap %s/%s: %w", RecordNamespace, RecordName, err)
	}
	var unknown []string
	for k := range cm.Data {
		if k != RecordKey && k != PendingKey && k != NitroComputeKey {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	return Record{Plugins: ParseRecord(cm.Data[RecordKey]), Pending: ParseRecord(cm.Data[PendingKey]),
		NitroCompute: strings.TrimSpace(cm.Data[NitroComputeKey]), ResourceVersion: cm.Metadata.ResourceVersion, Unknown: unknown}, nil
}

// All are the installed and the pending plugins, sorted, once each: what a
// removal of every plugin removes.
func (r Record) All() []string {
	return ParseRecord(FormatRecord(append(slices.Clone(r.Plugins), r.Pending...)))
}

// Empty reports a record with no plugin installed or pending, which
// WriteRecord deletes.
func (r Record) Empty() bool { return len(r.Plugins) == 0 && len(r.Pending) == 0 }

// Equal reports that r and s hold the same, whatever their ResourceVersion.
func (r Record) Equal(s Record) bool {
	return slices.Equal(r.Plugins, s.Plugins) && slices.Equal(r.Pending, s.Pending) && r.NitroCompute == s.NitroCompute
}

// NitroComputeOf is the NITRO_COMPUTE r records for p: NitroCompute while p
// follows NITRO_COMPUTE and is installed or pending, else "".
func (r Record) NitroComputeOf(p Plugin) string {
	if !p.FollowsNitroCompute || !slices.Contains(r.All(), p.ID) {
		return ""
	}
	return r.NitroCompute
}

// Manifest is the ConfigMap that holds r. It always has RecordKey, and the
// other keys only when they hold something, so with only Plugins it is the
// ConfigMap older installers write, byte for byte. It has no
// resourceVersion, so a write of it has no precondition.
func (r Record) Manifest() []byte { return r.manifest("") }

// manifest is Manifest with metadata.resourceVersion rv when rv is not "":
// a replace of it then succeeds only while the ConfigMap is at rv.
func (r Record) manifest(rv string) []byte {
	data := fmt.Sprintf("  %s: %q\n", RecordKey, FormatRecord(r.Plugins))
	if len(r.Pending) > 0 {
		data += fmt.Sprintf("  %s: %q\n", PendingKey, FormatRecord(r.Pending))
	}
	if r.NitroCompute != "" {
		data += fmt.Sprintf("  %s: %q\n", NitroComputeKey, r.NitroCompute)
	}
	version := ""
	if rv != "" {
		version = fmt.Sprintf("  resourceVersion: %q\n", rv)
	}
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
%s  labels:
    app.kubernetes.io/managed-by: substrate-eks
data:
%s`, RecordName, RecordNamespace, version, data))
}
