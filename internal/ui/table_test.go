package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestTableFitsTheTerminal(t *testing.T) {
	long := strings.Repeat("Baby Bottles, Anti Colic, 4 oz, ", 8)
	tbl := &Table{Headers: []string{"date", "title", "cost"}, Rows: [][]string{{"2026-08-10", long, "32.88"}, {"2026-07-21", "Diapers", "9.97"}}, Right: []int{2}, Width: 80}
	out := tbl.Render()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("line is %d wide, want at most 80:\n%s", w, out)
		}
	}
	if !strings.Contains(out, "…") || !strings.Contains(out, "32.88") || !strings.Contains(out, "2026-08-10") {
		t.Errorf("long text not cut, or short columns lost:\n%s", out)
	}
	tbl.Width = 0
	if !strings.Contains(tbl.Render(), long) {
		t.Error("with no width, text should be whole")
	}
}
