package snap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTML(t *testing.T) {
	in := "\x1b[1;38;2;255;153;0mTitle\x1b[0m <a&b>\n\x1b[32mok\x1b[39m plain \x1b[38;5;196mred\x1b[0m ⣾"
	got := HTML(in)
	for _, want := range []string{
		`<span style="color:#ff9900;font-weight:bold">Title</span> &lt;a&amp;b&gt;`,
		`<span style="color:#57ab5a">ok</span> plain `,
		`<span style="color:#ff0000">red</span> ◐`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("HTML missing %q in\n%s", want, got)
		}
	}
	if strings.Count(got, `<div class="l">`) != 2 || strings.Contains(got, "</div>\n") {
		t.Errorf("one <div> per line and no newline between them (it draws an empty line in <pre>):\n%s", got)
	}
}

func TestSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shots")
	w := Writer{Dir: dir, Title: "t"}
	if err := w.Save("01-welcome", "\x1b[1mhi\x1b[0m"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"01-welcome.ansi", "01-welcome.html"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "01-welcome.html"), []byte("old"), 0o644)
	if n, err := RebuildHTML(dir, "t"); err != nil || n != 1 {
		t.Fatalf("RebuildHTML: %d %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "01-welcome.html")); !strings.Contains(string(b), "font-weight:bold") {
		t.Error("RebuildHTML did not rewrite the page")
	}
	if err := (Writer{}).Save("x", "y"); err != nil {
		t.Error("an empty Dir saves nothing and is not an error")
	}
}
