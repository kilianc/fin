package ui

import (
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// Fin is a detailed, uncolored Unicode rendering of the glasses-only shark.
const Fin = finMonochrome

const finCompact = `              /|
             / |
        ____/  |___
      ╱             ╲
     /  ╭───╮ ╭───╮  \
    │   │ • ├─┤ • │   │
    │ ≋ ╰───╯ ╰───╯ ≋ │
    │      ╰─v─╯      │
  ╭─┴─╮             ╭─┴─╮
 ╱    ╰─────────────╯    ╲
│          F I N          │
 ╲                       ╱
   ╲                   ╱
     ╰───────────────╯`

// Mascot uses Unicode half-blocks for two vertical color samples per cell.
// Monochrome terminals get braille line art instead of unreadable solid blocks.
// No image protocol, special font, or runtime image file is needed.
func Mascot(width int, out io.Writer) string {
	if width >= 40 && colorprofile.Detect(out, os.Environ()) >= colorprofile.ANSI256 {
		return renderFinColor(min(width, 64))
	}
	if width >= lipgloss.Width(Fin) {
		return Fin
	}
	if width < lipgloss.Width(finCompact) {
		return Accent.Bold(true).Render("FIN")
	}
	return Accent.Render(finCompact)
}

func renderFinColor(width int) string {
	var b strings.Builder
	// The source is square; terminal cells are approximately twice as tall
	// as they are wide. Sample a square raster into width/2 text rows.
	height := width + width%2
	pixel := func(x, y int) byte {
		return finPixels[y*len(finPixels)/height][x*len(finPixels[0])/width]
	}
	for y := 0; y < height; y += 2 {
		if y > 0 {
			b.WriteByte('\n')
		}
		for x := 0; x < width; x++ {
			top, bottom := pixel(x, y), pixel(x, y+1)
			style := lipgloss.NewStyle()
			switch {
			case top == ' ' && bottom == ' ':
				b.WriteByte(' ')
			case top == ' ':
				b.WriteString(style.Foreground(lipgloss.Color(finPalette[bottom-'A'])).Render("▄"))
			case bottom == ' ':
				b.WriteString(style.Foreground(lipgloss.Color(finPalette[top-'A'])).Render("▀"))
			default:
				b.WriteString(style.Foreground(lipgloss.Color(finPalette[top-'A'])).
					Background(lipgloss.Color(finPalette[bottom-'A'])).Render("▀"))
			}
		}
	}
	return b.String()
}
