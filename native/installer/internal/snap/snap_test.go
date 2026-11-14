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

// Save writes the three files private, and the text one has no escapes.
func TestSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shots")
	path, err := Writer{Dir: dir, Title: "t"}.Save("snap-01-welcome", "\x1b[1mhi\x1b[0m  \n\x1b]8;;x\x07link\x1b[2K")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "snap-01-welcome.txt") {
		t.Errorf("path %s", path)
	}
	for _, f := range []string{"snap-01-welcome.ansi", "snap-01-welcome.txt", "snap-01-welcome.html"} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, mode %v", f, err, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != "hi\nlink\n" {
		t.Errorf("plain text %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "snap-01-welcome.ansi")); !strings.Contains(string(b), "\x1b[1m") {
		t.Error("the ANSI file lost its colors")
	}
	if _, err := (Writer{}).Save("x", "y"); err == nil {
		t.Error("a Writer with no Dir reported success")
	}
}

// Half blocks are drawn as halves of the cell, a run of them in one span.
func TestHTMLHalfBlocks(t *testing.T) {
	got := HTML("\x1b[38;2;255;0;0;48;2;0;0;255m▀▀\x1b[0mx\x1b[38;2;0;255;0m▄█\x1b[0m")
	for _, want := range []string{
		`<span class="b" style="background:linear-gradient(#ff0000 50%,#0000ff 50%)">  </span>x`,
		`background:linear-gradient(var(--bg) 50%,#00ff00 50%)"> </span>`,
		`<span class="b" style="background:#00ff00"> </span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("HTML missing %q in\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "▀▄█") {
		t.Errorf("block elements left as text:\n%s", got)
	}
}
