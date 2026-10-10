package amazon

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Chrome reads Amazon cookies out of a Chrome profile's cookie store.
type Chrome struct {
	// Dir is Chrome's user data directory, holding Local State and one
	// directory per profile.
	Dir string
	// SafeStorage returns the password Chrome encrypts cookies with, kept in
	// the login Keychain as "Chrome Safe Storage". macOS asks the person
	// before handing it out.
	SafeStorage func() (string, error)
	// UserAgent is sent with requests made with the imported cookies.
	UserAgent string
}

// Profile is one Chrome profile.
type Profile struct {
	Dir    string `json:"dir"`  // such as "Default" or "Profile 2"
	Name   string `json:"name"` // the name shown in Chrome
	Amazon bool   `json:"signed_in_to_amazon"`
}

// ErrNoChrome means Chrome's data directory is missing.
var ErrNoChrome = errors.New("Google Chrome is not set up on this Mac")

const sqliteBin = "/usr/bin/sqlite3"

var amazonHosts = []string{".amazon.com", "www.amazon.com", "amazon.com"}

// DefaultChromeDir is where Chrome keeps its profiles on macOS.
func DefaultChromeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
}

// SafeStorageFromKeychain reads Chrome's cookie password with security(1).
func SafeStorageFromKeychain() (string, error) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-w", "-s", "Chrome Safe Storage").Output()
	if err != nil {
		return "", errors.New("macOS did not allow access to Chrome Safe Storage")
	}
	return strings.TrimSpace(string(out)), nil
}

// Profiles lists Chrome's profiles and whether each holds Amazon cookies.
// It reads no cookie values, so it needs no Keychain access.
func (c Chrome) Profiles() ([]Profile, error) {
	raw, err := os.ReadFile(filepath.Join(c.Dir, "Local State"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoChrome
	}
	if err != nil {
		return nil, err
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name string `json:"name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("chrome: Local State: %w", err)
	}
	out := []Profile{}
	for dir, info := range ls.Profile.InfoCache {
		p := Profile{Dir: dir, Name: info.Name}
		rows, err := c.query(dir, "select count(*) as n from cookies where host_key in ("+hostList()+")")
		if err == nil && len(rows) == 1 {
			n, _ := rows[0]["n"].(float64)
			p.Amazon = n > 0
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir == "Default" || out[j].Dir == "Default" {
			return out[i].Dir == "Default"
		}
		return out[i].Dir < out[j].Dir
	})
	return out, nil
}

// Find returns the profile whose directory or name is key.
func (c Chrome) Find(key string) (Profile, error) {
	profiles, err := c.Profiles()
	if err != nil {
		return Profile{}, err
	}
	for _, p := range profiles {
		if p.Dir == key || strings.EqualFold(p.Name, key) {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("Chrome has no profile named %q", key)
}

// Session decrypts the profile's amazon.com cookies.
func (c Chrome) Session(profile string) (*Session, error) {
	meta, err := c.query(profile, "select value from meta where key = 'version'")
	if err != nil {
		return nil, err
	}
	version := 0
	if len(meta) == 1 {
		version, _ = strconv.Atoi(fmt.Sprint(meta[0]["value"]))
	}
	rows, err := c.query(profile, `select host_key, name, path, hex(encrypted_value) as enc, value,
		is_secure, is_httponly, expires_utc from cookies where host_key in (`+hostList()+`)`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("Chrome profile %q is not signed in to Amazon", profile)
	}
	password, err := c.SafeStorage()
	if err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), 1003, 16)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = c.userAgent()
	}
	s := &Session{UserAgent: ua, Profile: profile, SavedAt: time.Now().UTC()}
	for _, r := range rows {
		host, _ := r["host_key"].(string)
		value, _ := r["value"].(string)
		enc, _ := hex.DecodeString(fmt.Sprint(r["enc"]))
		if len(enc) > 0 {
			v, err := decryptCookie(block, enc, host, version)
			if err != nil {
				return nil, fmt.Errorf("chrome: cookie %v: %w", r["name"], err)
			}
			value = v
		}
		ck := &http.Cookie{
			Name: fmt.Sprint(r["name"]), Value: strings.Trim(value, `"`), Path: fmt.Sprint(r["path"]), Domain: host,
			Secure: fmt.Sprint(r["is_secure"]) == "1", HttpOnly: fmt.Sprint(r["is_httponly"]) == "1",
		}
		// Chrome counts microseconds since 1601.
		if us, ok := r["expires_utc"].(float64); ok && us > 0 {
			ck.Expires = time.UnixMicro(int64(us) - 11644473600000000).UTC()
		}
		if ck.Valid() == nil {
			s.Cookies = append(s.Cookies, ck)
		}
	}
	return s, nil
}

// decryptCookie undoes Chrome's macOS cookie encryption: "v10", then
// AES-128-CBC with a fixed IV. Cookie stores from version 24 on put the
// SHA-256 of the cookie's host before the value.
func decryptCookie(block cipher.Block, enc []byte, host string, version int) (string, error) {
	if !bytes.HasPrefix(enc, []byte("v10")) {
		return "", errors.New("unknown encryption")
	}
	ct := enc[3:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errors.New("bad length")
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(pt) {
		return "", errors.New("wrong key")
	}
	pt = pt[:len(pt)-pad]
	if version >= 24 {
		sum := sha256.Sum256([]byte(host))
		if len(pt) < len(sum) || !bytes.Equal(pt[:len(sum)], sum[:]) {
			return "", errors.New("wrong key")
		}
		pt = pt[len(sum):]
	}
	return string(pt), nil
}

// query runs SQL on a copy of the profile's cookie store; Chrome keeps the
// original locked while it runs.
func (c Chrome) query(profile, sql string) ([]map[string]any, error) {
	if profile == "" || strings.ContainsAny(profile, `/\`) || profile == "." || profile == ".." {
		return nil, fmt.Errorf("chrome: bad profile %q", profile)
	}
	src := filepath.Join(c.Dir, profile, "Cookies")
	if _, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("Chrome profile %q has no cookie store", profile)
	}
	tmp, err := os.MkdirTemp("", "fin-chrome-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		b, err := os.ReadFile(src + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(tmp, "Cookies"+suffix), b, 0o600); err != nil {
			return nil, err
		}
	}
	out, err := exec.Command(sqliteBin, "-json", "file:"+filepath.Join(tmp, "Cookies")+"?mode=ro", sql).Output()
	if err != nil {
		return nil, fmt.Errorf("chrome: read cookie store: %w", err)
	}
	rows := []map[string]any{}
	if len(bytes.TrimSpace(out)) == 0 {
		return rows, nil
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("chrome: read cookie store: %w", err)
	}
	return rows, nil
}

// userAgent is what this Chrome sends, so Amazon sees the same browser.
func (c Chrome) userAgent() string {
	major := "140"
	if b, err := os.ReadFile(filepath.Join(c.Dir, "Last Version")); err == nil {
		if v := strings.SplitN(strings.TrimSpace(string(b)), ".", 2)[0]; v != "" {
			major = v
		}
	}
	return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Safari/537.36"
}

func hostList() string {
	q := make([]string, len(amazonHosts))
	for i, h := range amazonHosts {
		q[i] = "'" + h + "'"
	}
	return strings.Join(q, ",")
}
