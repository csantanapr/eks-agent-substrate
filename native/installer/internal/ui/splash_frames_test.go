package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/snap"
)

// SPLASH_FRAMES_DIR=DIR saves every splash frame at 126x33 under DIR/126x33
// as snapshot pages, for installer/docs/splash-gif.sh.
func TestSplashFrames(t *testing.T) {
	dir := os.Getenv("SPLASH_FRAMES_DIR")
	if dir == "" {
		t.Skip("SPLASH_FRAMES_DIR is not set")
	}
	// 0 is termenv.TrueColor: the art needs 256 colors or more, and naming
	// the constant would make termenv a direct dependency of this module.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	w := snap.Writer{Dir: filepath.Join(dir, "126x33"), Title: "splash"}
	for f := 0; f <= splashFrames; f++ {
		v := splashArt(126, 33, f)
		// The art paints cell backgrounds; the plain title does not.
		if !strings.Contains(v, "48;2;") {
			t.Fatalf("frame %d is the plain title, not the art", f)
		}
		if _, err := w.Save(fmt.Sprintf("frame-%02d", f), v); err != nil {
			t.Fatal(err)
		}
	}
}
