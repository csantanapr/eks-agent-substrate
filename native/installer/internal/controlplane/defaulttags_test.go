package controlplane_test

import (
	"os"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// The control plane reads TAGS from the environment the wizard passes on,
// with this default when it is unset: the same as the wizard's, so the
// volume carries the tags the rest of the deployment does.
func TestDefaultTagsMatchTheWizard(t *testing.T) {
	t.Setenv("TAGS", "") // restores TAGS after the test
	if err := os.Unsetenv("TAGS"); err != nil {
		t.Fatal(err)
	}
	if got := state.NewSetup().Tags; got != controlplane.DefaultTags {
		t.Errorf("wizard TAGS default %q, control plane %q", got, controlplane.DefaultTags)
	}
}
