package ui

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

func TestColorMascotFitsTerminal(t *testing.T) {
	for _, width := range []int{40, 41, 63, 64} {
		art := renderFinColor(width)
		if !utf8.ValidString(art) {
			t.Fatal("mascot is not valid UTF-8")
		}
		for _, line := range strings.Split(art, "\n") {
			if got := lipgloss.Width(line); got != width {
				t.Fatalf("width %d: rendered %d columns", width, got)
			}
		}
	}
}

func TestMascotWithoutColorAndAtNarrowWidths(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	art := Mascot(80, &out)
	if strings.Contains(art, "\x1b") || !strings.ContainsAny(art, "⣀⡀⠁") {
		t.Fatal("NO_COLOR should use unstyled braille line art")
	}
	for _, width := range []int{10, 26, 40, 63} {
		if got := lipgloss.Width(Mascot(width, &out)); got > width {
			t.Fatalf("mascot width %d exceeds terminal width %d", got, width)
		}
	}
}
