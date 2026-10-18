package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestFlowCloseWhenDone(t *testing.T) {
	for _, close := range []bool{false, true} {
		m := flowModel{opts: FlowOptions{CloseWhenDone: close}, steps: []flowStep{{label: "payments"}}}
		next, cmd := m.Update(flowDoneMsg{res: FlowResult{Message: "Done."}})
		if !next.(flowModel).finished {
			t.Fatalf("close %v: the flow should be finished", close)
		}
		quit := false
		if cmd != nil {
			_, quit = cmd().(tea.QuitMsg)
		}
		if quit != close {
			t.Errorf("close %v: quit right away = %v", close, quit)
		}
	}
}
