package openclaw

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// staleRef matches a comment that names a script or file this installer
// does not have: the manifests came from the always-on-agent repository,
// whose deploy script and neighbouring files did the work the plugin now does.
var staleRef = regexp.MustCompile(`deploy-demo\.sh|deploy script|\.\./gateway\.yaml|ingress\.yaml|manifests/eks/|build/actor/`)

// staleComments are the full-line comments of a manifest that match staleRef.
func staleComments(text string) []string {
	var out []string
	for i, l := range strings.Split(text, "\n") {
		if c := strings.TrimSpace(l); strings.HasPrefix(c, "#") && staleRef.MatchString(c) {
			out = append(out, fmt.Sprintf("line %d: %s", i+1, c))
		}
	}
	return out
}

// Review R14: the embedded manifests' comments describe what this installer
// does and name nothing that is not here. The check is proved both ways on
// a line of each kind first.
func TestManifestCommentsNameNothingRemoved(t *testing.T) {
	if got := staleComments("# after demo/eks/deploy-demo.sh substitutes\n"); len(got) != 1 {
		t.Fatalf("the check missed a stale comment: %q", got)
	}
	if got := staleComments("# after the openclaw plugin (openclaw.go) substitutes\nargs: [deploy-demo.sh]\n"); len(got) != 0 {
		t.Fatalf("the check matched a current comment or a non-comment line: %q", got)
	}
	files, err := filepath.Glob(filepath.Join("manifests", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range staleComments(string(b)) {
			t.Errorf("%s %s", f, c)
		}
	}
}
