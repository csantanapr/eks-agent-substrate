package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Colors: AWS orange on a dark "squid ink" background.
var (
	cOrange = lipgloss.Color("#FF9900")
	cInk    = lipgloss.Color("#232F3E")
	cText   = lipgloss.Color("#E6EBEF")
	cMuted  = lipgloss.Color("#8D99A6")
	cDim    = lipgloss.Color("#5A6878")
	cGreen  = lipgloss.Color("#3FB950")
	cRed    = lipgloss.Color("#FF6B6B")
	cYellow = lipgloss.Color("#F2CC60")
	cBlue   = lipgloss.Color("#58A6FF")
	cCyan   = lipgloss.Color("#56D4DD")
	cLine   = lipgloss.Color("#344456")
)

var (
	sTitle   = lipgloss.NewStyle().Foreground(cOrange).Bold(true)
	sText    = lipgloss.NewStyle().Foreground(cText)
	sBold    = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sMuted   = lipgloss.NewStyle().Foreground(cMuted)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sGreen   = lipgloss.NewStyle().Foreground(cGreen)
	sRed     = lipgloss.NewStyle().Foreground(cRed)
	sYellow  = lipgloss.NewStyle().Foreground(cYellow)
	sBlue    = lipgloss.NewStyle().Foreground(cBlue)
	sCyan    = lipgloss.NewStyle().Foreground(cCyan)
	sOrange  = lipgloss.NewStyle().Foreground(cOrange)
	sKey     = lipgloss.NewStyle().Foreground(cInk).Background(cOrange).Bold(true).Padding(0, 1)
	sSection = lipgloss.NewStyle().Foreground(cOrange).Bold(true)
	sSel     = lipgloss.NewStyle().Foreground(cInk).Background(cOrange).Bold(true)
	sCode    = lipgloss.NewStyle().Foreground(cCyan)
)

const (
	iconDone    = "✓"
	iconFail    = "✗"
	iconWarn    = "!"
	iconPending = "·"
	iconSkip    = "–"
	iconArrow   = "›"
)

// pad cuts or pads s (which may hold color codes) to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := lipgloss.Width(s)
	if n > w {
		return ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-n)
}

// cut shortens plain text to w cells with an ellipsis.
func cut(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// fit makes block exactly w x h cells: lines cut or padded, extra lines
// dropped, missing lines added.
func fit(block string, w, h int) string {
	lines := strings.Split(block, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = pad(l, w)
	}
	return strings.Join(lines, "\n")
}

// hjoin puts blocks side by side with gap spaces between them.
func hjoin(gap int, blocks ...string) string {
	parts := []string{}
	for i, b := range blocks {
		if i > 0 {
			parts = append(parts, strings.Repeat(" ", gap))
		}
		parts = append(parts, b)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func secs(n int) string { return dur(time.Duration(n) * time.Second) }

var et *time.Location

func init() {
	var err error
	if et, err = time.LoadLocation("America/New_York"); err != nil {
		et = time.Local
	}
}

// clock is a wall clock time in 12-hour form, Eastern Time.
func clock(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(et).Format("3:04 PM") + " ET"
}

func clockSec(t time.Time) string {
	return t.In(et).Format("3:04:05 PM") + " ET"
}

// rule is a horizontal line w cells wide.
func rule(w int) string {
	return lipgloss.NewStyle().Foreground(cLine).Render(strings.Repeat("─", w))
}

// keyHelp renders "k action" pairs for the footer.
func keyHelp(pairs ...string) string {
	var b []string
	for i := 0; i+1 < len(pairs); i += 2 {
		b = append(b, sOrange.Render(pairs[i])+" "+sMuted.Render(pairs[i+1]))
	}
	return strings.Join(b, sDim.Render("  ·  "))
}
