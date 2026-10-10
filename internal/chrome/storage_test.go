package chrome

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/syndtr/goleveldb/leveldb"
)

func TestLocalStorageReadsLiveOriginOnlyWhileOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Default", "Local Storage", "leveldb")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := []byte("_https://www.costco.com\x00\x01refresh")
	for _, s := range []string{"old", "new"} {
		if err := db.Put(key, []byte("\x01"+s), nil); err != nil {
			t.Fatal(err)
		}
	}
	deleted := []byte("_https://www.costco.com\x00\x01deleted")
	if err := db.Put(deleted, []byte("\x01signed-out"), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(deleted, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("_https://other.example\x00\x01secret"), []byte("\x01private"), nil); err != nil {
		t.Fatal(err)
	}
	c := Chrome{Dir: dir}
	entries, err := c.LocalStorage("Default", "https://www.costco.com")
	if err != nil || len(entries) != 1 || entries["refresh"] != "new" {
		t.Fatalf("live entries=%v err=%v", entries, err)
	}
	if _, err := c.LocalStorage("../Default", "https://www.costco.com"); err == nil {
		t.Error("accepted path traversal")
	}
}

func TestStorageStringEncodings(t *testing.T) {
	want := "Café 🛒"
	b := []byte{0}
	for _, unit := range utf16.Encode([]rune(want)) {
		b = binary.LittleEndian.AppendUint16(b, unit)
	}
	if got, err := storageString(b); err != nil || got != want {
		t.Errorf("UTF16=%q %v", got, err)
	}
	if got, err := storageString([]byte{1, 'C', 'a', 'f', 0xe9}); err != nil || got != "Café" {
		t.Errorf("Latin1=%q %v", got, err)
	}
	for _, b := range [][]byte{nil, {0, 1}, {2, 1}} {
		if _, err := storageString(b); err == nil {
			t.Error("accepted bad encoding")
		}
	}
}
