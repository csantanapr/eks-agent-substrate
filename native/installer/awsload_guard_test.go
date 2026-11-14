package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loaderAllowed are the files that may call config.LoadDefaultConfig
// themselves: awsload, which every step's clients load through so a step
// outlives its credentials' session (issue #5 BUG5), and awsx, whose short
// wizard reads fail fast on an expired session on purpose (a picker that
// waits 30 minutes behind a spinner is worse than an error Enter retries).
var loaderAllowed = map[string]bool{
	"tools/setup-aws/awsload/awsload.go": true,
	"installer/internal/awsx/awsx.go":    true,
}

// rawLoaders returns "file:line" for every call of a function named
// LoadDefaultConfig in the non-test Go files under root, outside allowed.
// It parses the code, so a comment or a string naming the function is not a
// call.
func rawLoaders(t *testing.T, root string, allowed map[string]bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Never skip root itself: it is ".." when run from installer/,
			// and a dot-prefixed root would skip the whole tree.
			if n := d.Name(); path != root && (n == "vendor" || n == "testdata" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[filepath.ToSlash(rel)] {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if name == "LoadDefaultConfig" {
				out = append(out, filepath.ToSlash(rel)+":"+itoa(fset.Position(call.Pos()).Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// Every AWS client in native/ loads through awsload (or is one of the
// allowed fail-fast reads), so no new loader brings BUG5 back.
func TestEveryAWSLoaderGoesThroughAwsload(t *testing.T) {
	if got := rawLoaders(t, "..", loaderAllowed); len(got) > 0 {
		t.Errorf("these call config.LoadDefaultConfig directly; load through awsload.Config instead (a step must outlive its session):\n  %s",
			strings.Join(got, "\n  "))
	}
}

// Both ways for the guard itself: a direct call is found; a comment, a
// string, an allowed file and a test file are not.
func TestRawLoadersBothWays(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/bad.go", "package a\n\nfunc f() { awsconfig.LoadDefaultConfig(nil) }\n")
	write("a/comment.go", "package a\n\n// awsconfig.LoadDefaultConfig(ctx) is not called here.\nvar s = \"LoadDefaultConfig(\"\n")
	write("a/bad_test.go", "package a\n\nfunc g() { awsconfig.LoadDefaultConfig(nil) }\n")
	write("b/ok.go", "package b\n\nfunc h() { awsconfig.LoadDefaultConfig(nil) }\n")
	got := rawLoaders(t, root, map[string]bool{"b/ok.go": true})
	if len(got) != 1 || got[0] != "a/bad.go:3" {
		t.Errorf("found %q, want only a/bad.go:3", got)
	}
	// The real test walks "..": a root whose name starts with a dot must
	// still be walked (a first version skipped it and passed on anything).
	sub := filepath.Join(root, "a")
	t.Chdir(sub)
	if got := rawLoaders(t, "..", map[string]bool{"b/ok.go": true}); len(got) != 1 || got[0] != "a/bad.go:3" {
		t.Errorf("walking \"..\": found %q, want only a/bad.go:3", got)
	}
}
