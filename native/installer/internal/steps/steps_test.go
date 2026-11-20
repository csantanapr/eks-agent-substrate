package steps

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/demo"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Every setup-aws command's checklist lights each row, in order, on the exact
// line setup-aws logs for that phase (slog's text handler quotes msg).
func TestEveryPhaseListLightsEachRowInOrder(t *testing.T) {
	for name, list := range map[string][]string{
		"bootstrap": phases.Bootstrap, "cluster": phases.Cluster, "cluster self-managed": phases.ClusterSelfManaged,
		"cluster Karpenter": phases.ClusterFor("managed", true), "cluster self-managed Karpenter": phases.ClusterFor("self-managed", true),
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

// The create cluster checklist names the run's node group type in its node
// group rows, and the others are the same.
func TestClusterChecklistFollowsTheNodeGroupType(t *testing.T) {
	m, sm := labels(Cluster("managed", false)), labels(Cluster("self-managed", false))
	if !slices.Equal(m, labels(Cluster("", false))) || !slices.Equal(m[:7], sm[:7]) || len(m) != len(sm) {
		t.Fatalf("managed %q, self-managed %q", m, sm)
	}
	if m[7] != "Creating the managed node groups" || sm[7] != "Creating the self-managed node groups" ||
		sm[8] != "Waiting for the self-managed nodes to be Ready" {
		t.Errorf("rows 8 and 9: managed %q, self-managed %q", m[7:], sm[7:])
	}
}

// With KARPENTER=on the create cluster checklist is the node group type's
// with Karpenter's two rows after it; without, it is the type's alone, as
// before (phases.Cluster and phases.ClusterSelfManaged).
func TestClusterChecklistFollowsKarpenter(t *testing.T) {
	for typ, list := range map[string][]string{"": phases.Cluster, "managed": phases.Cluster, "self-managed": phases.ClusterSelfManaged} {
		off, on := labels(Cluster(typ, false)), labels(Cluster(typ, true))
		if !slices.Equal(off, labels(FromPhases(list))) || len(off) != 9 {
			t.Errorf("%q without Karpenter: %q", typ, off)
		}
		if len(on) != 11 || !slices.Equal(on[:9], off) || on[9] != "Creating Karpenter's IAM role and interruption queue" || on[10] != "Installing Karpenter and its NodePools" {
			t.Errorf("%q with Karpenter: %q", typ, on)
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
// the order Deploy prints them since it reads the plugin record before the
// fetch (kubeconfig first). Each must light exactly the next row: a row lit
// out of order ticks the rows it skipped, which is how the first live run
// showed CRDs done while the assets were still uploading.
func TestControlPlaneChecklistFollowsRealMarkerOrder(t *testing.T) {
	real := []string{
		"Writing kubeconfig /home/u/.cache/substrate-eks/kubeconfig/sub-native.us-west-2.yaml",
		"Control plane carries plugins: efs",
		"Fetching substrate v1.0.0 from https://github.com/agent-substrate/substrate.git",
		"substrate-eks EKS overlay: added deploy/eks to manifests/ate-install/base/kustomization.yaml",
		"Applying cluster prerequisites: StorageClass substrate-eks-auto-ebs (ebs.csi.eks.amazonaws.com, encrypted gp3, 3 volume tags)",
		"[step]: prepare_microvm_deps",
		"Uploading assets to s3://sub-native-assets-111122223333-us-west-2/microvm-assets/ ...",
		"[step]: deploy_crds",
		"[step]: ensure_apiserver_prerequisites",
		"[step]: Waiting for ATE system components to be ready...",
	}
	want := []int{0, 0, 1, 2, 3, 4, 4, 5, 6, 7}
	items, active := ControlPlane(true), -1
	for i, line := range real {
		active = Progress(items, active, line)
		if active != want[i] {
			t.Fatalf("line %q lit row %d, want %d", line, active, want[i])
		}
	}
}

// Both ways: without ate-setup's microvm class (only the AWS Nitro sandbox
// or gVisor) there is no microVM assets row to tick as done, and the lines
// that run light every other row in order; with it the row is there.
func TestControlPlaneHasTheMicroVMRowOnlyWhenItStages(t *testing.T) {
	has := func(items []ChecklistItem) bool {
		return slices.ContainsFunc(items, func(i ChecklistItem) bool { return i.Label == "Stage microVM assets in S3" })
	}
	if !has(ControlPlane(true)) || has(ControlPlane(false)) || len(ControlPlane(false)) != len(ControlPlane(true))-1 {
		t.Fatalf("rows: with %d, without %d", len(ControlPlane(true)), len(ControlPlane(false)))
	}
	items, active := ControlPlane(false), -1
	for i, line := range []string{controlplane.KubeconfLine + "/k", controlplane.CachedLine + "v1", eksoverlay.LinePrefix + "x",
		controlplane.PrereqLine + "StorageClass x", "[step]: deploy_crds", "[step]: ensure_apiserver_prerequisites",
		"[step]: Waiting for ATE system components to be ready..."} {
		if active = Progress(items, active, line); active != i {
			t.Fatalf("line %q lit row %d, want %d", line, active, i)
		}
	}
}

// The database row lights for each database path, in place, and for the
// RDS path it lights on the RDS line rather than staying dark until a later
// row ticks it as if it had run.
func TestDatabaseRowLightsForEveryPath(t *testing.T) {
	for _, line := range []string{
		controlplane.DatabaseLine + "sub-nt1-pg (sub-nt1-pg.x.us-west-2.rds.amazonaws.com:5432)",
		controlplane.PrereqLine + "StorageClass substrate-eks-ebs (ebs.csi.aws.com, encrypted gp3, 3 volume tags)",
	} {
		items := ControlPlane(true)
		got := Progress(items, 2, line)
		if got != 3 || items[got].Label != "Prepare the database" {
			t.Errorf("%q lit row %d", line, got)
		}
	}
}

func TestChecklistRejectsUnrelatedLines(t *testing.T) {
	for _, items := range [][]ChecklistItem{Bootstrap(), ControlPlane(true), ControlPlane(false)} {
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

// --deploy-demo --verify deploys on every class, then checks every class; each
// line lights its own row.
func TestDemoChecklistFollowsDeployThenVerify(t *testing.T) {
	lines := []string{
		demo.DeployLine + "gvisor", demo.DeployLine + "microvm",
		demo.VerifyLine + "gvisor", demo.VerifyPassLine + "gvisor", demo.VerifyLine + "microvm",
	}
	want := []int{0, 1, 2, 2, 3}
	items, active := Demo([]string{"gvisor", "microvm"}, []string{"gvisor", "microvm"}), -1
	for i, line := range lines {
		active = Progress(items, active, line)
		if active != want[i] {
			t.Fatalf("line %q lit row %d, want %d", line, active, want[i])
		}
	}
}

// Both ways: a class this step deploys is checked under its own name; the
// AWS Nitro sandbox, checked but deployed by the Set up Nitro sandboxes
// step, says so, and its row lights on the same verify line.
func TestDemoNamesTheStepThatDeployedAClass(t *testing.T) {
	items := Demo([]string{"gvisor"}, []string{"gvisor", "nitro"})
	want := []string{
		"Deploy the counter demo on gvisor",
		"Check suspend and resume on gvisor",
		"Check suspend and resume on nitro (deployed by Set up Nitro sandboxes)",
	}
	if got := labels(items); !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q", got, want)
	}
	if got := Progress(items, 1, demo.VerifyLine+"nitro"); got != 2 {
		t.Errorf("%q lit row %d, want 2", demo.VerifyLine+"nitro", got)
	}
}

// Plugins for the removal tests: st-s3 requires st-efs, as S3 Files does EFS
// in a real install, so the removal order is the reverse of the install's.
func init() {
	for _, p := range []plugin.Plugin{
		{ID: "st-gw", Title: "LLM gateway", Order: 10},
		{ID: "st-efs", Title: "EFS volumes", Order: 20, SetupAWS: true},
		{ID: "st-s3", Title: "S3 Files volumes", Order: 30, SetupAWS: true, Requires: []string{"st-efs"}},
	} {
		plugin.Register(p)
	}
}

func labels(items []ChecklistItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

// removalLines is what --remove-control-plane prints for a cluster recording
// rec: Delete's RemovePluginsLine, every line plugin.Run's remove prints (run
// for real, with a fake Exec that answers the record read), then Delete's
// ate-setup command line and RemovedLine (TestDeletePrintsTheChecklistLinesInOrder
// checks Delete prints them in that order).
func removalLines(t *testing.T, rec string) []string {
	t.Helper()
	prev := plugin.Exec
	t.Cleanup(func() { plugin.Exec = prev })
	plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
		if c.Stdout != nil && slices.Contains(c.Argv, plugin.RecordName) && slices.Contains(c.Argv, "get") {
			io.WriteString(c.Stdout, rec)
		}
		return nil
	}
	var out bytes.Buffer
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/k", SetupAWS: "/setup-aws"}
	if err := plugin.Run(context.Background(), &out, plugin.ActionRemove, nil, o); err != nil {
		t.Fatal(err)
	}
	lines := []string{controlplane.RemovePluginsLine}
	lines = append(lines, strings.Split(strings.TrimSpace(out.String()), "\n")...)
	return append(lines, controlplane.DeleteLine+" --context demo", `namespace "ate-system" deleted`, controlplane.RemovedLine)
}

// With the record read, the checklist has a row per recorded plugin in the
// order the removal takes them, each lit by its own "Removing plugin" line,
// and every one is lit, so ticked, before the ate-setup row lights.
func TestRemoveControlPlaneHasARowPerRecordedPlugin(t *testing.T) {
	rec := []string{"st-efs", "st-gw", "st-s3"}
	items := RemoveControlPlane(rec, true)
	want := []string{"Remove S3 Files volumes", "Remove EFS volumes", "Remove LLM gateway",
		"Delete ate-system with ate-setup", "Wait for the ate-system namespace to be gone"}
	if !slices.Equal(labels(items), want) {
		t.Fatalf("rows %q, want %q", labels(items), want)
	}
	lightsOn := []string{plugin.RemoveLine + "st-s3", plugin.RemoveLine + "st-efs", plugin.RemoveLine + "st-gw",
		controlplane.DeleteLine + " --context demo", controlplane.RemovedLine}
	active := -1
	for _, line := range removalLines(t, plugin.FormatRecord(rec)) {
		next := Progress(items, active, line)
		if next != active {
			if next != active+1 || line != lightsOn[next] {
				t.Fatalf("%q lit row %d (%s) after row %d", line, next, items[next].Label, active)
			}
			active = next
		}
	}
	if active != len(items)-1 {
		t.Errorf("the removal ended on row %d of %d", active, len(items))
	}
}

// Both ways: a record read and listing no plugin gives no plugin row, and
// the ate-setup row lights on the ate-setup line, not on the line before
// the (empty) plugin removal; a record not read gives the one row for all.
func TestRemoveControlPlaneWithNoRecordedPluginHasNoPluginRow(t *testing.T) {
	items := RemoveControlPlane(nil, true)
	if want := []string{"Delete ate-system with ate-setup", "Wait for the ate-system namespace to be gone"}; !slices.Equal(labels(items), want) {
		t.Fatalf("rows %q, want %q", labels(items), want)
	}
	lines := removalLines(t, "")
	want := map[string]int{controlplane.DeleteLine + " --context demo": 0, controlplane.RemovedLine: 1}
	active := -1
	for _, line := range lines {
		if next := Progress(items, active, line); next != active {
			if w, ok := want[line]; !ok || next != w {
				t.Fatalf("%q lit row %d", line, next)
			}
			active = next
		}
	}
	if active != 1 {
		t.Errorf("ended on row %d", active)
	}
}

// Unread (the teardown flow), or naming a plugin this installer does not
// know, the plugins get one row, lit as their removal starts; the plugins'
// own lines then light nothing past it.
func TestRemoveControlPlaneWithTheRecordUnreadHasOneRow(t *testing.T) {
	for name, items := range map[string][]ChecklistItem{
		"unread":  RemoveControlPlane(nil, false),
		"unknown": RemoveControlPlane([]string{"st-efs", "not-registered"}, true),
	} {
		if len(items) != 3 || items[0].Label != "Remove the recorded plugins" {
			t.Fatalf("%s: rows %q", name, labels(items))
		}
		active := -1
		for _, line := range []string{controlplane.KubeconfLine + "/k", controlplane.RemovePluginsLine, plugin.RemoveLine + "st-efs", plugin.RemovedLine + "st-efs"} {
			active = Progress(items, active, line)
		}
		if active != 0 {
			t.Errorf("%s: the plugins' lines left row %d lit, want 0", name, active)
		}
		if got := Progress(items, active, controlplane.DeleteLine+" --context demo"); got != 1 {
			t.Errorf("%s: the ate-setup line lit row %d, want 1", name, got)
		}
	}
}

// Each plugin's rows light on its own lines only: "efs" is not lit by a
// plugin whose ID starts with it.
func TestPluginsChecklist(t *testing.T) {
	items := Plugins([]plugin.Plugin{{ID: "efs", Title: "EFS"}, {ID: "efs-x", Title: "EFS X"}})
	if len(items) != 4 || items[0].Label != "Deploy EFS" || items[3].Label != "Check EFS X" {
		t.Fatalf("items = %+v", items)
	}
	steps := []struct {
		line string
		want int
	}{
		{plugin.DeployLine + "efs-x", 2},
		{plugin.DeployLine + "efs", 2},
		{plugin.VerifyLine + "efs-x", 3},
	}
	active := -1
	for _, s := range steps {
		active = Progress(items, active, s.line)
		if active != s.want {
			t.Fatalf("%q lit row %d, want %d", s.line, active, s.want)
		}
	}
	if got := Progress(items, -1, plugin.DeployLine+"efs"); got != 0 {
		t.Errorf("efs deploy line lit row %d, want 0", got)
	}
}
