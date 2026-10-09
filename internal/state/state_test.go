package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveWritesPrivateFileAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fin", "state.json")
	s := &State{Version: 1, Items: []Item{{Name: "chase", ItemID: "i1", Env: "sandbox"}}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := got.Find("sandbox", "i1"); !ok || it.Name != "chase" {
		t.Errorf("Find by item_id = %+v, %v", it, ok)
	}
	if _, ok := got.Find("production", "chase"); ok {
		t.Error("Find crossed environments")
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(s.Items) != 0 {
		t.Fatalf("Load = %+v, %v", s, err)
	}
}

func TestUniqueName(t *testing.T) {
	s := &State{Items: []Item{{Name: "american-express", Env: "sandbox"}}}
	if got := s.UniqueName("sandbox", "American Express"); got != "american-express-2" {
		t.Errorf("got %q", got)
	}
	if got := s.UniqueName("production", "American Express"); got != "american-express" {
		t.Errorf("got %q", got)
	}
	if got := s.UniqueName("sandbox", "Charles Schwab & Co."); got != "charles-schwab-co" {
		t.Errorf("got %q", got)
	}
}
