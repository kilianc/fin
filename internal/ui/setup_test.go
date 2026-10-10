package ui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func press(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		var cmd tea.Cmd
		m, cmd = m.Update(msg)
		// Run the verify command synchronously, as the program would.
		for cmd != nil {
			out := cmd()
			if v, ok := out.(verifiedMsg); ok {
				m, cmd = m.Update(v)
				continue
			}
			cmd = nil
		}
	}
	return m
}

func TestSetupVerifiesThenSaves(t *testing.T) {
	var saved []string
	tries := 0
	opts := SetupOptions{
		Env: "sandbox", KeysURL: "https://example.com/keys",
		Verify: func(_ context.Context, env, id, secret string) error {
			tries++
			if secret != "good" {
				return errors.New("invalid secret")
			}
			return nil
		},
		Save: func(env, id, secret string) error { saved = []string{env, id, secret}; return nil },
	}
	m := tea.Model(newSetupModel(context.Background(), opts))
	m = press(m, "c", "i", "d", "enter", "b", "a", "d", "enter")
	sm := m.(setupModel)
	if sm.stage != stageSecret || sm.err == nil || saved != nil {
		t.Fatalf("after a bad secret: stage %v, err %v, saved %v", sm.stage, sm.err, saved)
	}
	if sm.secret.Value() != "" {
		t.Errorf("the rejected secret should be cleared")
	}
	m = press(m, "g", "o", "o", "d", "enter")
	sm = m.(setupModel)
	if sm.stage != stageDone || tries != 2 {
		t.Fatalf("stage %v after %d tries", sm.stage, tries)
	}
	if saved[0] != "sandbox" || saved[1] != "cid" || saved[2] != "good" {
		t.Errorf("saved = %v", saved)
	}
}
