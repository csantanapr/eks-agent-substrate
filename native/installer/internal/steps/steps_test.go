package steps

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// setupAWSPhases reads setup-aws's Phases straight from its source (a separate
// Go module), so the checklist cannot drift from the lines bootstrap prints.
func setupAWSPhases(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../../../tools/setup-aws/cmd/bootstrap.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "Phases" {
			return true
		}
		for _, el := range vs.Values[0].(*ast.CompositeLit).Elts {
			s, err := strconv.Unquote(el.(*ast.BasicLit).Value)
			if err != nil {
				t.Fatal(err)
			}
			phases = append(phases, s)
		}
		return false
	})
	if len(phases) == 0 {
		t.Fatal("found no Phases in setup-aws bootstrap.go")
	}
	return phases
}

func TestBootstrapChecklistMatchesEverySetupAWSPhaseInOrder(t *testing.T) {
	phases := setupAWSPhases(t)
	items := Bootstrap()
	if len(items) != len(phases) {
		t.Fatalf("%d checklist rows for %d setup-aws phases", len(items), len(phases))
	}
	active := -1
	for i, p := range phases {
		line := `level=INFO msg="Step ` + strconv.Itoa(i+1) + `/` + strconv.Itoa(len(phases)) + `: ` + p + `"`
		active = Progress(items, active, line)
		if active != i {
			t.Errorf("phase %q lit row %d, want %d (%s)", p, active, i, items[i].Label)
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
