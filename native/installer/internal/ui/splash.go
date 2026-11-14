package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/theme"
)

// The splash before Welcome, ported from eks-talk's first installer: the
// Agent Substrate mascot, a hooded octopus holding three robot puppets, in
// half-block pixels; the title; and four small robots (actors) landing on
// gVisor, microVM and AWS Nitro Sandbox nodes in an Amazon EKS cluster.
// About 3.6 seconds; any key skips it.

const (
	splashFrames = 36
	splashEvery  = 100 * time.Millisecond
)

type splashMsg struct{}

func splashTick() tea.Cmd {
	return tea.Tick(splashEvery, func(time.Time) tea.Msg { return splashMsg{} })
}

func (m Model) onSplash() (tea.Model, tea.Cmd) {
	if !m.splash {
		return m, nil // a tick from a splash that was skipped
	}
	m.splashFrame++
	if m.splashFrame >= splashFrames {
		return m, m.endSplash()
	}
	return m, splashTick()
}

// endSplash leaves the splash for Welcome.
func (m *Model) endSplash() tea.Cmd {
	m.splash = false
	return nil
}

// splashView is the splash at the terminal's size.
func (m Model) splashView() string { return splashArt(m.w, m.h, m.splashFrame) }

// ---------------------------------------------------------------- art

// The mascot, 48 x 38 pixels, one letter a pixel ('.' is see-through). The
// puppets and the tentacle curls are separate so they can move; the arms
// under the puppets reach two pixels into them, so a puppet that bobs up
// stays on its tentacle.
var mascotBase = []string{
	"...................kkkkkkkkk....................",
	"................kkkhhhhhhhhhkkk.................",
	"..............kkhhhhhhhhhhhhhhhkk...............",
	".............khhhhhhhhhhhhhhhhhhhk..............",
	"............khhhhhhhhhhhhhhhhhhhhhk.............",
	"...........khhhhhhhhhhhhhhhhhhhhhhhk............",
	"...........khhhhhhhhkkkkkkkhhhhhhhhk............",
	"..........khhhhhhhkkdddddddkkhhhhhhhk...........",
	"..........khhhhhhkdddddddddddkhhhhhhk...........",
	"..........khhhhhkdddgggggggdddkhhhhhk...........",
	"..........khhhhhkdgggggggggggdkhhhhhk...........",
	"..........khhhhkgggggggggggggggkhhhhk...........",
	"..........khhhhkgggggggggggggggkhhhhk...........",
	"..........khhhkggEEEgggggggEEEggkhhhk...........",
	"...kggk...khhhkgEEEWEgkkkgEEEWEgkhhhk...........",
	"...kglk...khhhkgEEEEEgkmkgEEEEEgkhhhk...........",
	"...kggk...khhkggEEEEEggkggEEEEEggkhhk...........",
	"..kglgk...khhkglgEEEgggggggEEEglgkhhk...........",
	"..kgggk...khhkglllggggggggggglllgkhhk...kggk....",
	"..kglgk...khhikgggggggggggggggggkihhk...kglk....",
	"..kgggk...khhiiikkgggggggggggkkiiihhk...kggk....",
	"..kgglk...khhiiiiiikkgggggkkiiiiiihhk...kkgk....",
	"...kggk.kkkhhkkkkkkkkkkkkkkkkkkkkkhhkkk...kk....",
	"....kkkkkhhhhhhhhhhhhkhhhkhhhhhhhhhhhhkkk.......",
	"....kkhhhhhhhhhhhhhhhhkkkhhhhhhhhhhhhhhhhk......",
	"....khhhhhhhhhhhhhhhfhhhhhfhhhhhhhhhhhhhk.......",
	".....kkhhhhhhfhhhhhhfhhfhhfhhhhhhfhhhhhkkkk.....",
	"......kkhhhhhfhhhhhfhhhfhhhfhhhhhfhhhhk.kgk.....",
	"........khhhfhhhhhhfhhhfhhhfhhhhhhfhhhkkglk.....",
	"........khhhfhhhhhfhhhhfhhhhfhhhhhfkkkgggk......",
	"........khhfhhhhhhfhhhhfhhhhfhhhhhhkglggk.......",
	".........khfhhhhhfhhhhhfhhhhhfhhhhhkggkk........",
	".........khhhhhhhfhhhhhfhhhhhfhhhhhhkk..........",
	"..........khhhhhfhhhhhhfhhhhhhfhhhhk............",
	"...........khhhhfhhhhhhfhhhhhhfhhhk.............",
	".............khhkhhhkhhhhhkhhhkhk...............",
	"..............kkkkkkkkkkkkkkkkkk................",
	"................................................",
}

const mascotW, mascotH = 48, 38

// part is a moving piece of the mascot at x, y in its pixels.
type part struct {
	x, y int
	px   []string
}

var (
	puppetBlue = part{0, 2, []string{
		"...kkk...",
		"...kbk...",
		"...kkk...",
		"....k....",
		"kkkkkkkkk",
		"kbbbbbbbk",
		"kbkbbbkbk",
		"kbbkkkbbk",
		"kbbbbbbbk",
		"kkkkkkkkk",
		".kbbbbbk.",
		".kbbbbbk.",
		".kbbbbbk.",
		".kkkkkkk.",
	}}
	puppetYellow = part{38, 6, []string{
		"kkk....kkk",
		"kyk....kyk",
		"kkk....kkk",
		".k......k.",
		"kkkkkkkkkk",
		"kyyyyyyyyk",
		"kykkyykkyk",
		"kyyykkyyyk",
		"kyyyyyyyyk",
		"kkkkkkkkkk",
		".kyyyyyyk.",
		".kyyyyyyk.",
		".kyyyyyyk.",
		".kkkkkkkk.",
	}}
	puppetPink = part{38, 20, []string{
		".......kkk",
		"......kkpk",
		"..kkkkkkk.",
		".kpppppk..",
		"kkpkppkpk.",
		"kppppppk..",
		"kkkkkkkk..",
		"kpkpkpk...",
		"kkkkkkk...",
	}}
	curlLeft = part{0, 26, []string{
		".kkkkk..",
		"kgglggk.",
		"kgkkkggk",
		"kgkgkgk.",
		"kgkkggk.",
		".kgggkkk",
		"..kkk...",
	}}
	curlLowLeft = part{9, 31, []string{
		"..kkkk..",
		".kglggk.",
		"kgkkkggk",
		"kgkgkgk.",
		"kggkggk.",
		".kgggk..",
		"..kkk...",
	}}
	curlLowRight = part{30, 31, []string{
		"..kkkk..",
		".kgglgk.",
		"kggkkkgk",
		".kgkgkgk",
		".kggkggk",
		"..kgggk.",
		"...kkk..",
	}}
	// eyelids drawn over each eye (at x 16 and 26, y 13) for a blink
	eyelid = []string{
		"ggggg",
		"ggggg",
		"kgggk",
		"gkkkg",
		"ggggg",
	}
)

// Actors: the puppets' small look-alikes that land on the nodes.
var actors = []struct {
	px  []string
	col uint32
}{
	{[]string{"..c..", "ccccc", "ckckc", "ccccc"}, 0x62B2DA},
	{[]string{"c...c", "ccccc", "ckckc", "ccccc"}, 0xF0EA6E},
	{[]string{"...c.", "ccccc", "ckckc", "ccccc"}, 0xE678B0},
	{[]string{"..c..", "ccccc", "ckckc", "ccccc"}, 0x5FCF65},
}

var mascotColors = map[byte]uint32{
	'k': 0x0C0C10, // outline
	'h': 0x3A3640, // hood
	'f': 0x28252D, // folds in the cloak
	'i': 0x1A191E, // inside the hood
	'g': 0x5FCF65, // skin
	'l': 0x8EEC8C, // spots and cheeks
	'd': 0x3C8C42, // skin in the hood's shadow
	'E': 0x161618, // eyes
	'W': 0xFFFFFF, // eye highlights
	'm': 0xE274AC, // tongue
	'b': 0x62B2DA,
	'y': 0xF0EA6E,
	'p': 0xE678B0,
}

// Squid ink, the splash background, and the other colors as 0xRRGGBB.
const (
	inkBG     uint32 = 0x232F3E
	inkOrange uint32 = 0xFF9900
	inkText   uint32 = 0xE6EBEF
	inkMuted  uint32 = 0x8D99A6
	inkDim    uint32 = 0x5A6878
	inkEyes   uint32 = 0x0C0C10
)

// ---------------------------------------------------------------- canvas

// canvas is the splash screen: w x h cells, each two pixels, one above the
// other, drawn as "▀" in two colors, and text, which pixels cover.
type canvas struct {
	w, h int
	px   []uint32 // w x 2h; 0 is the background, else 0x1RRGGBB
	ch   []rune   // w x h; 0 is no text
	fg   []uint32
	bold []bool
}

const opaque = 1 << 24

func newCanvas(w, h int) *canvas {
	return &canvas{w: w, h: h, px: make([]uint32, w*2*h), ch: make([]rune, w*h), fg: make([]uint32, w*h), bold: make([]bool, w*h)}
}

func (c *canvas) dot(x, y int, col uint32) {
	if x >= 0 && x < c.w && y >= 0 && y < 2*c.h {
		c.px[y*c.w+x] = col | opaque
	}
}

// art draws a bitmap at pixel x, y, each pixel s x s.
func (c *canvas) art(x, y, s int, rows []string, color func(byte) uint32) {
	for j, row := range rows {
		for i := 0; i < len(row); i++ {
			if row[i] == '.' {
				continue
			}
			col := color(row[i])
			for dy := 0; dy < s; dy++ {
				for dx := 0; dx < s; dx++ {
					c.dot(x+i*s+dx, y+j*s+dy, col)
				}
			}
		}
	}
}

// text writes s at cell x, y.
func (c *canvas) text(x, y int, s string, fg uint32, bold bool) int {
	for _, r := range s {
		if x >= 0 && x < c.w && y >= 0 && y < c.h {
			i := y*c.w + x
			c.ch[i], c.fg[i], c.bold[i] = r, fg, bold
		}
		x++
	}
	return x
}

func hex(c uint32) string { return fmt.Sprintf("#%06X", c&0xFFFFFF) }

// render draws the canvas in the terminal's colors, through lipgloss (the
// first installer called termenv itself, which this module does not import).
func (c *canvas) render() string {
	type cell struct {
		r      rune
		fg, bg uint32
		bold   bool
	}
	var b strings.Builder
	for y := 0; y < c.h; y++ {
		var run []rune
		var cur cell
		flush := func() {
			if len(run) == 0 {
				return
			}
			st := lipgloss.NewStyle().Foreground(lipgloss.Color(hex(cur.fg))).Background(lipgloss.Color(hex(cur.bg))).Bold(cur.bold)
			b.WriteString(st.Render(string(run)))
			run = run[:0]
		}
		for x := 0; x < c.w; x++ {
			top, bot := c.px[2*y*c.w+x], c.px[(2*y+1)*c.w+x]
			k := cell{r: ' ', fg: inkBG, bg: inkBG}
			switch {
			case top == 0 && bot == 0 && c.ch[y*c.w+x] != 0:
				i := y*c.w + x
				k = cell{r: c.ch[i], fg: c.fg[i], bg: inkBG, bold: c.bold[i]}
			case top == 0 && bot == 0:
			case top == bot:
				k = cell{r: ' ', fg: top &^ opaque, bg: top &^ opaque}
			default:
				t, u := top&^opaque, bot&^opaque
				if top == 0 {
					t = inkBG
				}
				if bot == 0 {
					u = inkBG
				}
				k = cell{r: '▀', fg: t, bg: u}
			}
			// A space shows only its background, so it joins any run with
			// that background.
			if k.r == ' ' && len(run) > 0 && cur.bg == k.bg {
				run = append(run, ' ')
				continue
			}
			if len(run) > 0 && (k.fg != cur.fg || k.bg != cur.bg || k.bold != cur.bold) {
				flush()
			}
			cur = k
			run = append(run, k.r)
		}
		flush()
		if y < c.h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// blend fades c toward the background; a is 0 (background) to 1 (c).
func blend(c uint32, a float64) uint32 {
	if a >= 1 {
		return c
	}
	mix := func(sh uint) uint32 {
		x, bg := float64(c>>sh&0xFF), float64(inkBG>>sh&0xFF)
		return uint32(math.Round(bg+(x-bg)*a)) << sh
	}
	return mix(16) | mix(8) | mix(0)
}

// ---------------------------------------------------------------- frames

// wave is 0 or -1, a two-step bob with the given period and phase.
func wave(f, period, phase int) int {
	if (f+phase)/(period/2)%2 == 1 {
		return -1
	}
	return 0
}

// drawMascot draws the mascot for frame f at pixel x, y, scale s: it fades
// in, the puppets bob, the curls wave and the eyes blink twice.
func drawMascot(c *canvas, x, y, s, f int) {
	a := math.Min(1, float64(f+1)/6)
	color := func(ch byte) uint32 { return blend(mascotColors[ch], a) }
	at := func(p part, dx, dy int) { c.art(x+(p.x+dx)*s, y+(p.y+dy)*s, s, p.px, color) }
	c.art(x, y, s, mascotBase, color)
	if f == 9 || f == 10 || f == 25 || f == 26 {
		at(part{16, 13, eyelid}, 0, 0)
		at(part{26, 13, eyelid}, 0, 0)
	}
	at(curlLeft, 0, wave(f, 10, 0))
	at(curlLowLeft, wave(f, 10, 3), 0)
	at(curlLowRight, -wave(f, 10, 6), 0)
	at(puppetBlue, 0, wave(f, 8, 0))
	at(puppetYellow, 0, wave(f, 8, 3))
	at(puppetPink, 0, wave(f, 8, 6))
}

// swoosh is a smile that rises to an arrowhead on the right, drawn under
// "on Amazon EKS".
var swoosh = []string{
	"#............###",
	".#..........####",
	"..###.....###.##",
	"....######......",
}

// drawSwoosh draws the first cols columns of the swoosh at pixel x, y.
func drawSwoosh(c *canvas, x, y, cols int) {
	for j, row := range swoosh {
		for i := 0; i < min(len(row), cols); i++ {
			if row[i] == '#' {
				c.dot(x+i, y+j, inkOrange)
			}
		}
	}
}

// drawTitle writes the title typed up to frame f, then "on Amazon EKS" with
// the swoosh drawn under it, in four rows from cell x, y.
func drawTitle(c *canvas, x, y, f int) {
	const name, ver = "Agent Substrate", " 1.0.0"
	n := min(len(name+ver), max(0, (f-3)*2))
	end := c.text(x, y, name[:min(n, len(name))], inkText, true)
	if n > len(name) {
		end = c.text(end, y, ver[:n-len(name)], inkOrange, true)
	}
	if n > 0 && n < len(name+ver) {
		c.text(end, y, "▌", inkOrange, false)
	}
	if f >= 12 {
		e := c.text(x, y+1, "on ", inkMuted, false)
		c.text(e, y+1, "Amazon EKS", inkText, true)
		drawSwoosh(c, x, 2*(y+2), len(swoosh[0])*(f-11)/7)
	}
}

// clusterRows is the height of the Amazon EKS frame.
const clusterRows = 13

// drawCluster draws the Amazon EKS frame, pw cells wide and clusterRows
// high, at cell x, y: a gVisor node and a microVM node side by side, an
// AWS Nitro Sandbox node under them, and the actors dropping onto them.
func drawCluster(c *canvas, x, y, pw, f int) {
	if f < 16 {
		return
	}
	// The nodes, side by side with a space before, between and (when it
	// fits) after them; gVisor holds two actors, microVM one. The
	// AWS Nitro Sandbox node below spans both, after a blank row.
	avail := pw - 5
	if avail < 24 {
		avail = pw - 4
	}
	bw1 := max(13, avail-avail/2)
	bw2 := avail - bw1
	// The labels are on the bottom borders, out of the way of the drops.
	label := " Amazon EKS "
	bottom := y + clusterRows - 1
	c.text(x, y, "╭"+strings.Repeat("─", pw-2)+"╮", inkDim, false)
	for r := y + 1; r < bottom; r++ {
		c.text(x, r, "│", inkDim, false)
		c.text(x+pw-1, r, "│", inkDim, false)
	}
	e := c.text(x, bottom, "╰─", inkDim, false)
	e = c.text(e, bottom, label, inkOrange, true)
	c.text(e, bottom, strings.Repeat("─", pw-3-len(label))+"╯", inkDim, false)

	// The boxes, each w x h cells at x, y with its name on the bottom
	// border; slots is how many actors land in it.
	type box struct {
		name       string
		x, y, w, h int
		slots      int
	}
	boxes := []box{
		{name: "gVisor", x: x + 2, y: y + 1, w: bw1, h: 4, slots: 2},
		{name: "microVM", x: x + 3 + bw1, y: y + 1, w: bw2, h: 4, slots: 1},
		{name: "AWS Nitro Sandbox", x: x + 2, y: y + 6, w: avail + 1, h: 6, slots: 1},
	}
	// Actor i drops from three rows above its box between frames start
	// and start+4, then that box's border lights up for two frames.
	type drop struct {
		actor, box, slot, start int
	}
	drops := []drop{{0, 0, 0, 19}, {1, 1, 0, 21}, {2, 0, 1, 23}, {3, 2, 0, 25}}
	for i, b := range boxes {
		border := inkDim
		for _, d := range drops {
			if d.box == i && f >= d.start+4 && f < d.start+6 {
				border = inkOrange
			}
		}
		name := " " + b.name + " "
		c.text(b.x, b.y, "┌"+strings.Repeat("─", b.w-2)+"┐", border, false)
		for r := b.y + 1; r < b.y+b.h-1; r++ {
			c.text(b.x, r, "│", border, false)
			c.text(b.x+b.w-1, r, "│", border, false)
		}
		e := c.text(b.x, b.y+b.h-1, "└", border, false)
		e = c.text(e, b.y+b.h-1, name, inkText, false)
		c.text(e, b.y+b.h-1, strings.Repeat("─", b.w-2-len(name))+"┘", border, false)
	}
	for _, d := range drops {
		if f < d.start {
			continue
		}
		b := boxes[d.box]
		in := b.w - 2
		ax := b.x + 1 + (in-(6*b.slots-1))/2 + 6*d.slot
		y1 := 2 * (b.y + 1)
		y0 := y1 - 6
		t := math.Min(1, float64(f-d.start)/4)
		ay := y0 + int(math.Round(float64(y1-y0)*t*t))
		a := actors[d.actor]
		c.art(ax, ay, 1, a.px, func(ch byte) uint32 {
			if ch == 'k' {
				return inkEyes
			}
			return a.col
		})
	}
}

// richColor reports a terminal that draws 256 colors or more, which the art
// needs: lipgloss then writes 38;5 or 38;2 color codes. NO_COLOR, a 16-color
// terminal and a test have neither.
func richColor() bool {
	s := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF9900")).Render("x")
	return strings.Contains(s, "38;2;") || strings.Contains(s, "38;5;")
}

// splashArt draws frame f at w x h cells.
func splashArt(w, h, f int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if !richColor() {
		return splashTitle(w, h)
	}
	c := newCanvas(w, h)
	hint := "press any key"
	// The title and the cluster are 5+clusterRows rows and at least 28
	// cells wide.
	side := func(s int) {
		aw, ah := s*mascotW, s*mascotH/2
		pw := min(44, w-aw-4)
		gap := min(3, w-aw-pw-2)
		x0 := (w - (aw + gap + pw)) / 2
		y0 := (h - 1 - ah) / 2
		drawMascot(c, x0, 2*y0, s, f)
		px, py := x0+aw+gap, y0+(ah-5-clusterRows)/2
		drawTitle(c, px, py, f)
		drawCluster(c, px, py+5, pw, f)
	}
	switch {
	case w >= 2*mascotW+34+5 && h >= mascotH+2:
		side(2) // the mascot at twice the size
	case w >= mascotW+28+4 && h >= mascotH/2+2:
		side(1)
	case w >= mascotW+2 && h >= mascotH/2+5:
		// Narrow: the mascot, then the title under it.
		y0 := (h - 1 - (mascotH/2 + 4)) / 2
		drawMascot(c, (w-mascotW)/2, 2*y0, 1, f)
		drawTitle(c, (w-len("Agent Substrate 1.0.0"))/2, y0+mascotH/2, f)
	default:
		return splashTitle(w, h)
	}
	if f >= 2 {
		c.text((w-len(hint))/2, h-1, hint, inkDim, false)
	}
	return c.render()
}

// splashTitle is the splash without the art: for a small window, or a
// terminal without color (NO_COLOR).
func splashTitle(w, h int) string {
	lines := []string{
		theme.Title.Render("Agent Substrate 1.0.0"),
		theme.Subtle.Render("on ") + lipgloss.NewStyle().Bold(true).Render("Amazon EKS"),
		"",
		theme.Fainted.Render("press any key"),
	}
	out := make([]string, max(0, (h-len(lines))/2))
	for _, l := range lines {
		out = append(out, strings.Repeat(" ", max(0, (w-lipgloss.Width(l))/2))+l)
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(out, "\n"))
}
