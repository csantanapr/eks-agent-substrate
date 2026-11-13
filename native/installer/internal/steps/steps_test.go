package steps

import (
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

// Every setup-aws command's checklist lights each row, in order, on the exact
// line setup-aws logs for that phase (slog's text handler quotes msg).
func TestEveryPhaseListLightsEachRowInOrder(t *testing.T) {
	for name, list := range map[string][]string{
		"bootstrap": phases.Bootstrap, "cluster": phases.Cluster,
		"rds": phases.RDS, "teardown": phases.Teardown,
	} {
		items, active := FromPhases(list), -1
		for i := range list {
			line := `time=x level=INFO msg="` + phases.Line(list, i) + `"`
			active = Progress(items, active, line)
			if active != i {
				t.Errorf("%s: phase %d lit row %d", name, i, active)
			}
		}
	}
}

// "Step 1/9: ..." must not light a row of a 4-phase list, and a phase title
// inside an unrelated line ("would create ...") must not light its row.
func TestPhaseRowsMatchOnlyTheirOwnStepLine(t *testing.T) {
	items := FromPhases(phases.Bootstrap)
	for _, line := range []string{
		"Step 1/9: " + phases.Bootstrap[0],
		"plan: " + phases.Bootstrap[1],
	} {
		if got := Progress(items, -1, line); got != -1 {
			t.Errorf("%q lit row %d", line, got)
		}
	}
}

// The marker lines of a real install on sub-native (us-west-2, v1.0.0), in
// the order they came. Each must light exactly the next row: a row lit out of
// order ticks the rows it skipped, which is how the first live run showed
// CRDs done while the assets were still uploading.
func TestControlPlaneChecklistFollowsRealMarkerOrder(t *testing.T) {
	real := []string{
		"Fetching substrate v1.0.0 from https://github.com/agent-substrate/substrate.git",
		"substrate-eks EKS overlay: added deploy/eks to manifests/ate-install/base/kustomization.yaml",
		"Writing kubeconfig /home/u/.cache/substrate-eks/kubeconfig/sub-native.us-west-2.yaml",
		"Applying cluster prerequisites from /x/native/deploy/eks/prereqs",
		"[step]: prepare_microvm_deps",
		"Uploading assets to s3://sub-native-assets-111122223333-us-west-2/microvm-assets/ ...",
		"[step]: deploy_crds",
		"[step]: ensure_apiserver_prerequisites",
		"[step]: Waiting for ATE system components to be ready...",
	}
	want := []int{0, 1, 2, 3, 4, 4, 5, 6, 7}
	items, active := ControlPlane(), -1
	for i, line := range real {
		active = Progress(items, active, line)
		if active != want[i] {
			t.Fatalf("line %q lit row %d, want %d", line, active, want[i])
		}
	}
}

func TestChecklistRejectsUnrelatedLines(t *testing.T) {
	for _, items := range [][]ChecklistItem{Bootstrap(), ControlPlane()} {
		if got := Progress(items, -1, "some unrelated output"); got != -1 {
			t.Errorf("unrelated line lit row %d", got)
		}
	}
}

func TestProgressNeverMovesBack(t *testing.T) {
	items := Bootstrap()
	if got := Progress(items, 2, "Step 1/4: Checking the EKS cluster..."); got != 2 {
		t.Errorf("cursor moved back to %d", got)
	}
}
