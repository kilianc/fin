package chrome

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// LocalStorage reads only origin's live entries from a private copy. LevelDB
// follows the manifest and deletion records; scanning old table files could
// bring back a session the person has already signed out of.
func (c Chrome) LocalStorage(profile, origin string) (map[string]string, error) {
	if profile == "" || strings.ContainsAny(profile, `/\`) || profile == "." || profile == ".." {
		return nil, fmt.Errorf("chrome: bad profile %q", profile)
	}
	src := filepath.Join(c.Dir, profile, "Local Storage", "leveldb")
	files, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "fin-chrome-storage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	for _, f := range files {
		name := f.Name()
		if !(name == "CURRENT" || strings.HasPrefix(name, "MANIFEST-") || strings.HasSuffix(name, ".ldb") || strings.HasSuffix(name, ".sst") || strings.HasSuffix(name, ".log")) {
			continue
		}
		if !f.Type().IsRegular() {
			return nil, errors.New("chrome: Local Storage contains a non-file")
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return nil, fmt.Errorf("chrome: copy Local Storage; close Chrome and try again: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o600); err != nil {
			return nil, err
		}
	}
	db, err := leveldb.OpenFile(tmp, &opt.Options{ReadOnly: true, ErrorIfMissing: true})
	if err != nil {
		return nil, fmt.Errorf("chrome: read Local Storage; close Chrome and try again: %w", err)
	}
	defer db.Close()
	prefix := []byte("_" + origin + "\x00")
	it := db.NewIterator(util.BytesPrefix(prefix), nil)
	defer it.Release()
	out := map[string]string{}
	for it.Next() {
		key, err := storageString(it.Key()[len(prefix):])
		if err != nil {
			return nil, err
		}
		value, err := storageString(it.Value())
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, it.Error()
}

// Chrome prefixes strings with 0 for UTF-16LE, 1 for Latin-1.
func storageString(b []byte) (string, error) {
	if len(b) == 0 {
		return "", errors.New("chrome: empty Local Storage value")
	}
	switch b[0] {
	case 1:
		runes := make([]rune, len(b)-1)
		for i, v := range b[1:] {
			runes[i] = rune(v)
		}
		return string(runes), nil
	case 0:
		if len(b)%2 != 1 {
			return "", errors.New("chrome: invalid UTF-16 Local Storage value")
		}
		units := make([]uint16, (len(b)-1)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(b[1+i*2:])
		}
		return string(utf16.Decode(units)), nil
	}
	return "", errors.New("chrome: unknown Local Storage encoding")
}
