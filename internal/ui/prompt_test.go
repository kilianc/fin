package ui

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestPromptTakesWhatIsTyped(t *testing.T) {
	done := make(chan struct{})
	var got string
	var err error
	go func() {
		got, err = Prompt(strings.NewReader("2\r"), io.Discard, "Which one? [1-2]", "", false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return")
	}
	if err != nil || got != "2" {
		t.Fatalf("Prompt = %q, %v", got, err)
	}
}
