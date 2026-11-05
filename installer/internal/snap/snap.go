// Package snap writes the installer's rendered screens to files: the ANSI
// text as the terminal gets it, and an HTML page that draws it in a
// terminal window at 1920x1080, ready for a headless browser screenshot.
package snap

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Writer saves screens into Dir.
type Writer struct {
	Dir   string
	Title string // the window title in the HTML page
}

// Save writes NAME.ansi and NAME.html.
func (w Writer) Save(name, view string) error {
	if w.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.Dir, name+".ansi"), []byte(view), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.Dir, name+".html"), []byte(Page(w.Title, view)), 0o644)
}

// RebuildHTML writes NAME.html again for every NAME.ansi in dir.
func RebuildHTML(dir, title string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.ansi"))
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(strings.TrimSuffix(f, ".ansi")+".html", []byte(Page(title, string(b))), 0o644); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}

var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)
var brailleRe = regexp.MustCompile(`[\x{2800}-\x{28FF}]`)
var otherEsc = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-ln-z]|\x1b\][^\x07]*\x07`)

type style struct {
	fg, bg                   string
	bold, faint, ital, under bool
	reverse                  bool
}

func (s style) css() string {
	fg, bg := s.fg, s.bg
	if s.reverse {
		fg, bg = bg, fg
		if fg == "" {
			fg = "var(--bg)"
		}
		if bg == "" {
			bg = "var(--fg)"
		}
	}
	var b []string
	if fg != "" {
		b = append(b, "color:"+fg)
	}
	if bg != "" {
		b = append(b, "background:"+bg)
	}
	if s.bold {
		b = append(b, "font-weight:bold")
	}
	if s.faint {
		b = append(b, "opacity:.6")
	}
	if s.ital {
		b = append(b, "font-style:italic")
	}
	if s.under {
		b = append(b, "text-decoration:underline")
	}
	return strings.Join(b, ";")
}

var basic = []string{
	"#1b1f24", "#e5534b", "#57ab5a", "#c69026", "#539bf5", "#b083f0", "#39c5cf", "#adbac7",
	"#636e7b", "#ff938a", "#6bc46d", "#daaa3f", "#6cb6ff", "#dcbdfb", "#56d4dd", "#cdd9e5",
}

func color256(n int) string {
	switch {
	case n < 16:
		return basic[n]
	case n < 232:
		n -= 16
		lv := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", lv(n/36), lv(n/6%6), lv(n%6))
	default:
		g := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	}
}

func (s *style) apply(params string) {
	if params == "" {
		*s = style{}
		return
	}
	p := strings.Split(params, ";")
	num := func(i int) int { v, _ := strconv.Atoi(p[i]); return v }
	for i := 0; i < len(p); i++ {
		n := num(i)
		switch {
		case n == 0:
			*s = style{}
		case n == 1:
			s.bold = true
		case n == 2:
			s.faint = true
		case n == 3:
			s.ital = true
		case n == 4:
			s.under = true
		case n == 7:
			s.reverse = true
		case n == 22:
			s.bold, s.faint = false, false
		case n == 23:
			s.ital = false
		case n == 24:
			s.under = false
		case n == 27:
			s.reverse = false
		case n >= 30 && n <= 37:
			s.fg = basic[n-30]
		case n >= 90 && n <= 97:
			s.fg = basic[n-90+8]
		case n == 39:
			s.fg = ""
		case n >= 40 && n <= 47:
			s.bg = basic[n-40]
		case n >= 100 && n <= 107:
			s.bg = basic[n-100+8]
		case n == 49:
			s.bg = ""
		case n == 38 || n == 48:
			var c string
			if i+1 < len(p) && p[i+1] == "5" && i+2 < len(p) {
				c = color256(num(i + 2))
				i += 2
			} else if i+1 < len(p) && p[i+1] == "2" && i+4 < len(p) {
				c = fmt.Sprintf("#%02x%02x%02x", num(i+2), num(i+3), num(i+4))
				i += 4
			}
			if n == 38 {
				s.fg = c
			} else {
				s.bg = c
			}
		}
	}
}

// HTML converts ANSI text (SGR colors and attributes) to HTML lines, one
// <div> per line.
func HTML(ansi string) string {
	ansi = otherEsc.ReplaceAllString(ansi, "")
	// The spinner's braille frames are not in DejaVu Sans Mono (nor any font
	// on a stock host); draw them as a half circle so the cell keeps its width.
	ansi = brailleRe.ReplaceAllString(ansi, "◐")
	var b strings.Builder
	var st style
	for _, line := range strings.Split(strings.TrimRight(ansi, "\n"), "\n") {
		b.WriteString(`<div class="l">`)
		rest := line
		for rest != "" {
			loc := sgrRe.FindStringSubmatchIndex(rest)
			text := rest
			if loc != nil {
				text = rest[:loc[0]]
			}
			if text != "" {
				if css := st.css(); css != "" {
					fmt.Fprintf(&b, `<span style="%s">%s</span>`, css, html.EscapeString(text))
				} else {
					b.WriteString(html.EscapeString(text))
				}
			}
			if loc == nil {
				break
			}
			st.apply(rest[loc[2]:loc[3]])
			rest = rest[loc[1]:]
		}
		// No newline between lines: inside <pre> it would draw an empty line.
		b.WriteString("</div>")
	}
	return b.String()
}

// Page is a full HTML document: the screen in a terminal window, centered on
// a 1920x1080 background.
func Page(title, ansi string) string {
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>` + html.EscapeString(title) + `</title>
<style>
:root { --bg: #0f1923; --fg: #d5dbdb; }
html, body { margin: 0; width: 1920px; height: 1080px; overflow: hidden;
  background: radial-gradient(ellipse at 30% 20%, #23344a 0%, #121c28 55%, #0b121a 100%); }
.win { position: absolute; left: 50%; top: 50%; transform: translate(-50%, -50%);
  background: var(--bg); border-radius: 14px; box-shadow: 0 30px 80px rgba(0,0,0,.55), 0 0 0 1px #2c3e50; overflow: hidden; }
.bar { height: 40px; background: #1b2735; display: flex; align-items: center; padding: 0 18px; gap: 9px;
  font: 15px "DejaVu Sans", sans-serif; color: #8d99a6; }
.dot { width: 13px; height: 13px; border-radius: 50%; }
.bar .t { flex: 1; text-align: center; margin-right: 60px; }
pre { margin: 0; padding: 18px 24px 22px; color: var(--fg);
  font: 21px/26px "DejaVu Sans Mono", monospace; white-space: pre; }
.l { height: 26px; }
</style></head><body><div class="win">
<div class="bar"><span class="dot" style="background:#ff5f57"></span><span class="dot" style="background:#febc2e"></span><span class="dot" style="background:#28c840"></span><span class="t">` + html.EscapeString(title) + `</span></div>
<pre>` + HTML(ansi) + `</pre></div></body></html>
`
}
