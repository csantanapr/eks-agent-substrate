// Package state holds the wizard's step machine and the answers collected so
// far. The Step/Machine half is substrate-gke's installer/internal/state
// unchanged except for the step list; Setup holds EKS answers instead of GCP
// ones. Keep the Machine code identical so upstream fixes apply as-is.
package state

import "fmt"

// Step identifies one screen of the wizard.
type Step int

// The steps, in substrate-gke's order. EKS has no Images step yet (the POC
// installs the published release images) and folds the sandbox choice into
// ControlPlane (gvisor and microvm are both installed).
const (
	Welcome Step = iota
	CheckSetup
	Account // substrate-gke: Project
	Cluster
	Provision
	ControlPlane
	Complete
)

// Order is the linear flow of the wizard.
var Order = []Step{Welcome, CheckSetup, Account, Cluster, Provision, ControlPlane, Complete}

// Title is a step's label in the step rail.
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
	case Provision:
		return "Provision AWS resources"
	case ControlPlane:
		return "Turn on Substrate"
	case Complete:
		return "Complete"
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

// Prev rolls back to the most recently visited step.
func (m *Machine) Prev() (Step, bool) {
	if len(m.history) == 0 {
		return m.Current(), false
	}
	m.idx = m.history[len(m.history)-1]
	m.history = m.history[:len(m.history)-1]
	return m.Current(), true
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

	// SubstrateTag is the release the control plane installs, with its
	// published images from ImageRepo.
	SubstrateTag string
	ImageRepo    string
	// SandboxClasses ate-setup installs.
	SandboxClasses string
}

// NewSetup returns the defaults: the release eks-talk pins.
func NewSetup() *Setup {
	return &Setup{
		SubstrateTag:   "v1.0.0",
		ImageRepo:      "ghcr.io/agent-substrate/substrate",
		SandboxClasses: "gvisor,microvm",
	}
}

// Names are setup-aws's derived resource names (cmd.deriveNames).
func (s *Setup) SnapBucket() string {
	return fmt.Sprintf("%s-snap-%s-%s", s.ClusterName, s.Account, s.Region)
}

// AssetBucket is the microVM asset bucket.
func (s *Setup) AssetBucket() string {
	return fmt.Sprintf("%s-assets-%s-%s", s.ClusterName, s.Account, s.Region)
}
