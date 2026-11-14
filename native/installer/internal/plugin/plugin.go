// Package plugin is the contract every optional part of an install follows:
// the LLM gateway, EFS and S3 Files volumes, Secrets Manager login rotation,
// the Secrets Manager credential provider, OpenClaw, and mode 3. Each lives in
// its own file (or subpackage) and registers itself with Register from an
// init function, so plugins are built in parallel without touching each
// other's files. It is the native form of eks-talk's demos/*/deploy.sh, and of
// substrate-gke's optional steps (Filestore CSI, autoscaling).
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
	"context"
	"fmt"
	"io"
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
	// Source, when set, replaces the release. At most one plugin may set it.
	Source *Source
}

// Options is what Deploy, Verify and Remove get.
type Options struct {
	Cluster, Region, Profile string
	// Tag is the Substrate release; TreeRoot holds its cached tree.
	Tag, TreeRoot string
	// Kubeconfig is the installer's own kubeconfig for the cluster
	// (awsx.KubeconfigPath), already written by the caller; its current
	// context is the cluster. Use Kubectl, which passes it.
	Kubeconfig string
	// SetupAWS is native/tools/setup-aws; Run runs `setup-aws plugin <id>`
	// from it for a plugin with SetupAWS set.
	SetupAWS string
	// Tags is TAGS: space-separated key=value pairs every AWS resource gets
	// besides the owner tag (OwnerTags adds it).
	Tags string
	// AutoMode is the cluster's EKS Auto Mode, read off the cluster.
	AutoMode bool
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
	// Default plugins are preselected in the wizard (the LLM gateway, which
	// eks-talk's deploy.sh always installed).
	Default bool
	// Requires are plugin IDs that must be installed first (OpenClaw keeps
	// its gateway state on EFS).
	Requires []string
	// SetupAWS reports that `setup-aws plugin <ID>` makes AWS resources.
	SetupAWS bool
	Needs    Needs
	// HourlyUSD is the rough on-demand cost the plugin adds, for the Review
	// screen (0 when it is only Kubernetes objects).
	HourlyUSD float64
	// Deploy makes the plugin's Kubernetes objects, Verify checks it works
	// (a quick check fit for the install), Remove deletes its Kubernetes
	// objects (Run then deletes its AWS resources with `setup-aws plugin <id>
	// --delete`). All must be idempotent, start every child from an argv
	// (Exec), and print no secret.
	Deploy, Verify, Remove func(ctx context.Context, w io.Writer, o Options) error
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

// Merge is the union of ps' needs, each list sorted and without repeats. Two
// plugins that set a Source are an error.
func Merge(ps []Plugin) (Needs, error) {
	var n Needs
	for _, p := range ps {
		n.Preview = append(n.Preview, p.Needs.Preview...)
		n.CredentialProviders = append(n.CredentialProviders, p.Needs.CredentialProviders...)
		n.IngressIDs = append(n.IngressIDs, p.Needs.IngressIDs...)
		if p.Needs.Source != nil {
			if n.Source != nil {
				return Needs{}, fmt.Errorf("two plugins replace the Substrate release; choose one (%s)", p.ID)
			}
			s := *p.Needs.Source
			n.Source = &s
		}
	}
	for _, l := range []*[]string{&n.Preview, &n.CredentialProviders, &n.IngressIDs} {
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
	return n, nil
}

// RecordNamespace/RecordName are the ConfigMap that records which plugins are
// installed, in kube-system so that removing ate-system keeps it, and its data
// key holds a comma-separated, sorted list of IDs. The control plane deploy
// adds the plugins whose needs it now carries, Run's deploy adds a plugin once
// it is deployed and checked, and Run's remove drops it.
const (
	RecordNamespace = "kube-system"
	RecordName      = "substrate-eks-plugins"
	RecordKey       = "plugins"
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
