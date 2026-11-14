// Package state holds the wizard's step machine and the answers collected so
// far. The Step/Machine half is substrate-gke's installer/internal/state
// unchanged except for the step list; Setup holds EKS answers instead of GCP
// ones. Keep the Machine code identical so upstream fixes apply as-is.
package state

import (
	"fmt"
	"os"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
)

// Step identifies one screen of the wizard.
type Step int

// The steps. EKS has no Images step yet (the install uses the published
// release images) and folds the sandbox choice into Settings. Teardown's
// steps come after Complete, as substrate-gke's upgrade steps do: their place
// on screen comes from the Machine's order, not from these values.
const (
	Welcome Step = iota
	CheckSetup
	Account // substrate-gke: Project
	Cluster
	Database
	// Settings edits config.env's knobs for the run, Plugins chooses the
	// optional parts, and Review shows every command, its time and the cost
	// before anything runs.
	Settings
	Plugins
	Review
	CreateCluster
	Provision
	RDS
	ControlPlane
	// InstallPlugins deploys and checks the chosen plugins.
	InstallPlugins
	Demo
	Complete

	TeardownScope
	RemoveControlPlane
	TeardownAWS
)

// Order is the linear flow of the wizard: connect an existing cluster, with
// the RDS database and no plugin. InstallOrder adds or drops the optional
// steps.
var Order = []Step{Welcome, CheckSetup, Account, Cluster, Database, Settings, Plugins, Review, Provision, RDS, ControlPlane, Complete}

// InstallOrder is the install flow for the answers in s, as substrate-gke's
// InstallOrder picks DemoOrder for a demo run. Every choice that changes the
// flow is made on the Cluster, Database, Settings and Plugins screens, before
// Review, and nothing runs before Review:
//   - CreateCluster runs `setup-aws create cluster` when the run creates the
//     cluster, before Provision, which needs it ACTIVE;
//   - RDS runs `setup-aws create rds` after Provision when the database is
//     RDS, before ControlPlane, which connects to it;
//   - Demo deploys and checks the counter demo on a demo run (--demo),
//     after ControlPlane;
//   - InstallPlugins deploys the chosen plugins last: ControlPlane already
//     carries their needs, and a plugin that builds on the counter demo
//     (plugin.NeedsDemo; the wizard turns the demo on for it) finds it.
func InstallOrder(s *Setup) []Step {
	order := []Step{Welcome, CheckSetup, Account, Cluster, Database, Settings, Plugins, Review}
	if s.ClusterIsNew {
		order = append(order, CreateCluster)
	}
	order = append(order, Provision)
	if s.Database != DatabaseBundled {
		order = append(order, RDS)
	}
	order = append(order, ControlPlane)
	if s.DemoMode {
		order = append(order, Demo)
	}
	if len(s.Plugins) > 0 {
		order = append(order, InstallPlugins)
	}
	return append(order, Complete)
}

// TeardownOrder is the flow that removes an install (--teardown, or [t] on
// the Complete screen): Substrate first, with ate-setup, so its volumes and
// claims are released, then the AWS resources, with `setup-aws teardown`.
var TeardownOrder = []Step{Welcome, CheckSetup, Account, TeardownScope, RemoveControlPlane, TeardownAWS, Complete}

// Title is a step's label in the step rail. The rail is 30 columns and a
// two-digit step's prefix takes seven of them, so a title is at most 23
// columns (TestStepTitlesFitTheRail).
func (s Step) Title() string {
	switch s {
	case Welcome:
		return "Welcome"
	case CheckSetup:
		return "Check your setup"
	case Account:
		return "Choose AWS account"
	case Cluster:
		return "Connect your cluster"
	case Database:
		return "Choose the database"
	case Settings:
		return "Settings"
	case Plugins:
		return "Choose plugins"
	case Review:
		return "Review the plan"
	case CreateCluster:
		return "Create the cluster"
	case Provision:
		return "Provision AWS resources"
	case RDS:
		return "Create the database"
	case ControlPlane:
		return "Turn on Substrate"
	case InstallPlugins:
		return "Install the plugins"
	case Demo:
		return "Deploy the demo"
	case Complete:
		return "Complete"
	case TeardownScope:
		return "Choose what to remove"
	case RemoveControlPlane:
		return "Remove Substrate"
	case TeardownAWS:
		return "Delete AWS resources"
	}
	return "?"
}

// Machine is the linear step machine with back-navigation history.
type Machine struct {
	order   []Step
	idx     int
	history []int
}

// NewMachine starts at Welcome on the install flow.
func NewMachine() *Machine { return &Machine{order: Order} }

// SetOrder switches the flow, which the welcome screen does when the user
// picks the upgrade track. Both flows start at Welcome, so switching there
// keeps the current step in place.
func (m *Machine) SetOrder(order []Step) {
	m.order = order
	m.idx = 0
	m.history = nil
}

// Order returns the steps of the active flow.
func (m *Machine) Order() []Step { return m.order }

// Position returns the 1-based position of s among the numbered steps of the
// active flow, and false for Welcome, Complete, and steps not in the flow.
func (m *Machine) Position(s Step) (int, bool) {
	if s == Welcome || s == Complete {
		return 0, false
	}
	for i, o := range m.order {
		if o == s {
			return i, true
		}
	}
	return 0, false
}

// NumberedSteps is how many numbered steps the active flow has.
func (m *Machine) NumberedSteps() int { return len(m.order) - 2 }

// Current returns the active step.
func (m *Machine) Current() Step { return m.order[m.idx] }

// Next advances to the following step and records history for Prev.
func (m *Machine) Next() Step {
	if m.idx < len(m.order)-1 {
		m.history = append(m.history, m.idx)
		m.idx++
	}
	return m.Current()
}

// Prev rolls back to the most recently visited step. The second return is
// false when there is nothing to go back to.
func (m *Machine) Prev() (Step, bool) {
	if len(m.history) == 0 {
		return m.Current(), false
	}
	m.idx = m.history[len(m.history)-1]
	m.history = m.history[:len(m.history)-1]
	return m.Current(), true
}

// Restart returns to Welcome and clears history.
func (m *Machine) Restart() Step {
	m.idx = 0
	m.history = nil
	return m.Current()
}

// Database choices, the values --deploy-control-plane takes as --database.
const (
	DatabaseRDS     = controlplane.DatabaseRDS
	DatabaseBundled = controlplane.DatabaseBundled
)

// ClusterConfig shapes a cluster the run creates: config.env's create-cluster
// knobs, which `setup-aws create cluster` reads from its environment. They are
// ignored for a cluster the run connects to.
type ClusterConfig struct {
	KubernetesVersion string
	// AutoMode is "on" for EKS Auto Mode, empty for none (config.env's AUTO_MODE).
	AutoMode                                                    string
	SystemInstanceType, GVisorInstanceType, MicroVMInstanceType string
	SystemNodes, GVisorNodes, MicroVMNodes                      string
	PublicAccessCIDRs                                           string
}

// clusterKnobs are the environment variables of ClusterConfig with
// config.env's defaults, in the order Env lists them.
var clusterKnobs = []struct {
	env, def string
	field    func(*ClusterConfig) *string
}{
	{"KUBERNETES_VERSION", "1.37", func(c *ClusterConfig) *string { return &c.KubernetesVersion }},
	{"AUTO_MODE", "on", func(c *ClusterConfig) *string { return &c.AutoMode }},
	{"SYSTEM_INSTANCE_TYPE", "m8i.xlarge", func(c *ClusterConfig) *string { return &c.SystemInstanceType }},
	{"GVISOR_INSTANCE_TYPE", "m8i.2xlarge", func(c *ClusterConfig) *string { return &c.GVisorInstanceType }},
	{"MICROVM_INSTANCE_TYPE", "m8i.2xlarge", func(c *ClusterConfig) *string { return &c.MicroVMInstanceType }},
	{"SYSTEM_NODES", "2", func(c *ClusterConfig) *string { return &c.SystemNodes }},
	{"GVISOR_NODES", "2", func(c *ClusterConfig) *string { return &c.GVisorNodes }},
	{"MICROVM_NODES", "2", func(c *ClusterConfig) *string { return &c.MicroVMNodes }},
	{"EKS_PUBLIC_ACCESS_CIDRS", "auto", func(c *ClusterConfig) *string { return &c.PublicAccessCIDRs }}, // auto: setup-aws allows this machine's address, a /32
}

// ClusterConfigFromEnv reads the knobs with lookup (os.LookupEnv), falling
// back to config.env's defaults. A variable set to empty stays empty: an
// exported AUTO_MODE= turns Auto Mode off, as it does for create-cluster.sh.
func ClusterConfigFromEnv(lookup func(string) (string, bool)) ClusterConfig {
	var c ClusterConfig
	for _, k := range clusterKnobs {
		v, ok := lookup(k.env)
		if !ok {
			v = k.def
		}
		*k.field(&c) = v
	}
	return c
}

// Env is the knobs as KEY=VALUE for setup-aws's environment.
func (c ClusterConfig) Env() []string {
	out := make([]string, 0, len(clusterKnobs))
	for _, k := range clusterKnobs {
		out = append(out, k.env+"="+*k.field(&c))
	}
	return out
}

// Summary is the review line the create-cluster panel and step show.
func (c ClusterConfig) Summary() string {
	auto := "Auto Mode off"
	if c.AutoMode == "on" {
		auto = "Auto Mode on"
	}
	return fmt.Sprintf("EKS %s · %s · system %s×%s · gvisor %s×%s · microvm %s×%s · API from %s",
		c.KubernetesVersion, auto, c.SystemNodes, c.SystemInstanceType, c.GVisorNodes, c.GVisorInstanceType,
		c.MicroVMNodes, c.MicroVMInstanceType, c.PublicAccessCIDRs)
}

// Setup is everything the install steps need.
type Setup struct {
	Profile string // shared config profile, empty for the default chain
	Region  string
	Account string // from sts:GetCallerIdentity, never typed
	Arn     string

	ClusterName    string
	ClusterVersion string
	AutoMode       bool
	// ClusterIsNew is a cluster the run creates with NewClusterConfig.
	ClusterIsNew     bool
	NewClusterConfig ClusterConfig
	// ClusterOwned is a cluster this installer created (it carries
	// awsx.CreatedByTagKey and this deployment's owner tag), or one the run
	// creates. Only an owned cluster is ever offered for deletion, or for
	// removing an install found on it.
	ClusterOwned bool

	// Tags are config.env's TAGS, on every resource setup-aws creates.
	Tags string

	// Database is DatabaseRDS (the default) or DatabaseBundled.
	Database string
	// DemoMode is a demo run (--demo): the flow ends with the Demo step.
	DemoMode bool
	// Plugins are the chosen plugin IDs (plugin.Resolve adds what they
	// require): the control plane deploy carries their needs, then the
	// Plugins step deploys them.
	Plugins []string

	// Teardown is the teardown flow (TeardownOrder); KeepCluster is its
	// answer: remove Substrate and its AWS data but keep the cluster.
	Teardown    bool
	KeepCluster bool
	// KeepClusterOnly is --keep-cluster (make teardown KEEP_CLUSTER=true):
	// the teardown never offers to delete the cluster.
	KeepClusterOnly bool

	// SubstrateTag is the release the control plane installs, with its
	// published images from ImageRepo (config.env's SUBSTRATE_TAG and
	// SUBSTRATE_IMAGE_REPO); the control plane and demo steps get both.
	SubstrateTag string
	ImageRepo    string
	// SandboxClasses ate-setup installs, comma-separated: gvisor, microvm or
	// both (AllSandboxClasses).
	SandboxClasses string
}

// Defaults the installer passes on, and the flag defaults of its
// subcommands: a child given no --image-repo or --sandbox-classes uses these.
const (
	DefaultTag            = "v1.0.0"
	DefaultImageRepo      = "ghcr.io/agent-substrate/substrate"
	AllSandboxClasses     = "gvisor,microvm"
	DefaultSandboxClasses = AllSandboxClasses
	DefaultTags           = "project=agent-substrate auto-delete=no"
)

// NewSetup returns the defaults from the environment alone; NewSetupFrom
// from any lookup, such as Config.Lookup over config.env.
func NewSetup() *Setup { return NewSetupFrom(os.LookupEnv) }

// NewSetupFrom returns the defaults: the release eks-talk pins, RDS, and
// config.env's cluster knobs, tags and release unless lookup sets them.
func NewSetupFrom(lookup func(string) (string, bool)) *Setup {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && (v != "" || key == "TAGS") {
			return v
		}
		return def
	}
	return &Setup{
		SubstrateTag:     get("SUBSTRATE_TAG", DefaultTag),
		ImageRepo:        get("SUBSTRATE_IMAGE_REPO", DefaultImageRepo),
		SandboxClasses:   DefaultSandboxClasses,
		Database:         DatabaseRDS,
		Tags:             get("TAGS", DefaultTags),
		NewClusterConfig: ClusterConfigFromEnv(lookup),
	}
}

// ValidSandboxClasses reports why classes cannot be installed: it must name
// gvisor, microvm or both, each once, comma-separated.
func ValidSandboxClasses(classes string) error {
	seen := map[string]bool{}
	for _, c := range strings.Split(classes, ",") {
		if c != "gvisor" && c != "microvm" {
			return fmt.Errorf("sandbox classes %q: want gvisor, microvm or gvisor,microvm", classes)
		}
		if seen[c] {
			return fmt.Errorf("sandbox classes %q name %s twice", classes, c)
		}
		seen[c] = true
	}
	return nil
}

// SelectCluster records an existing cluster as the run's target, and
// NewCluster a cluster the run will create. Every Cluster* field is written
// by both, as in substrate-gke: a field one of them forgot would carry an
// abandoned selection's value into the other path.
func (s *Setup) SelectCluster(c awsx.Cluster) {
	s.ClusterName = c.Name
	s.ClusterVersion = c.Version
	s.AutoMode = c.AutoMode
	s.ClusterIsNew = false
	s.ClusterOwned = c.CreatedByInstaller(s.Region)
}

// NewCluster records a cluster the run will create, named name.
func (s *Setup) NewCluster(name string) {
	s.ClusterName = name
	s.ClusterVersion = s.NewClusterConfig.KubernetesVersion
	s.AutoMode = s.NewClusterConfig.AutoMode == "on"
	s.ClusterIsNew = true
	s.ClusterOwned = true
}

// Names are setup-aws's derived resource names (cmd.deriveNames).
func (s *Setup) SnapBucket() string {
	return fmt.Sprintf("%s-snap-%s-%s", s.ClusterName, s.Account, s.Region)
}

// AssetBucket is the microVM asset bucket.
func (s *Setup) AssetBucket() string {
	return fmt.Sprintf("%s-assets-%s-%s", s.ClusterName, s.Account, s.Region)
}
