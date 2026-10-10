// Package chrometest builds Chrome profiles using synthetic sessions only.
package chrometest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
)

// Dir makes profiles with encrypted cookies and no real browser data.
func Dir(t *testing.T, password string, profiles map[string]string, cookies map[string][]*http.Cookie) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skip("needs /usr/bin/sqlite3")
	}
	dir := t.TempDir()
	cache := map[string]any{}
	for d, name := range profiles {
		cache[d] = map[string]string{"name": name}
	}
	ls, _ := json.Marshal(map[string]any{"profile": map[string]any{"info_cache": cache}})
	os.WriteFile(filepath.Join(dir, "Local State"), ls, 0o600)
	os.WriteFile(filepath.Join(dir, "Last Version"), []byte("154.0.8037.98"), 0o600)
	key, _ := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), 1003, 16)
	block, _ := aes.NewCipher(key)
	for d := range profiles {
		pdir := filepath.Join(dir, d)
		os.MkdirAll(pdir, 0o700)
		sql := `create table meta(key text, value text); insert into meta values ('version', '24');
create table cookies(host_key text, name text, path text, value text, encrypted_value blob, is_secure int, is_httponly int, expires_utc int);
insert into cookies values ('.example.com', 'other', '/', 'nope', x'', 0, 0, 0);`
		for _, ck := range cookies[d] {
			host, value := ck.Domain, ck.Value
			sum := sha256.Sum256([]byte(host))
			pt := append(sum[:], value...)
			pad := aes.BlockSize - len(pt)%aes.BlockSize
			for range pad {
				pt = append(pt, byte(pad))
			}
			ct := make([]byte, len(pt))
			cipher.NewCBCEncrypter(block, []byte(strings.Repeat(" ", 16))).CryptBlocks(ct, pt)
			sql += fmt.Sprintf("\ninsert into cookies values ('%s', '%s', '/', '', x'%s', 1, 1, 13500000000000000);",
				host, ck.Name, hex.EncodeToString(append([]byte("v10"), ct...)))
		}
		if out, err := exec.Command("/usr/bin/sqlite3", filepath.Join(pdir, "Cookies"), sql).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v: %s", err, out)
		}
	}
	return dir
}

// LocalStorage writes origin's entries to a synthetic LevelDB. The caller
// can add other origins to exercise the reader's origin filtering.
func LocalStorage(t *testing.T, dir, profile, origin string, entries map[string]string) {
	t.Helper()
	path := filepath.Join(dir, profile, "Local Storage", "leveldb")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for key, value := range entries {
		if err := db.Put([]byte("_"+origin+"\x00\x01"+key), append([]byte{1}, []byte(value)...), nil); err != nil {
			t.Fatal(err)
		}
	}
}
