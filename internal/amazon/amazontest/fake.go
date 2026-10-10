// Package amazontest fakes the parts of amazon.com and Chrome that fin
// amazon uses, for tests. It does not import package amazon, so that
// package's own tests can use it.
package amazontest

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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Server is a fake amazon.com: the payments page, the payments API in pages
// of two, and order pages.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	Rows     []json.RawMessage
	Orders   map[string][]byte // order ID -> page
	SignedIn bool
	Token    string // the session-token cookie value it accepts
	Requests map[string]int
	// Limited answers the next this many requests with 429 Too Many Requests.
	Limited int
	// PagesBeforeLimit, when set, answers 429 to every payments API call
	// after this many.
	PagesBeforeLimit int
}

// New starts a signed-in fake that accepts session-token "good".
func New(t *testing.T) *Server {
	f := &Server{Orders: map[string][]byte{}, SignedIn: true, Token: "good", Requests: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Payment builds one row of the payments list.
func Payment(amount, date, order, method, descriptor, status string) json.RawMessage {
	row := map[string]any{
		"formattedAmount": amount, "formattedDate": date,
		"orderData":                      []map[string]string{{"orderDisplayString": "Order #" + order, "orderDetailsUrl": "https://www.amazon.com/gp/css/summary/edit.html?orderID=" + order}},
		"statementDescriptor":            descriptor,
		"statusInfo":                     map[string]string{"status": "Approved", "label": status},
		"paymentMethodDisplayStringData": map[string]string{"paymentMethodName": method},
	}
	b, _ := json.Marshal(row)
	return b
}

// Count is how many requests a path got.
func (f *Server) Count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Requests[path]
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests[r.URL.Path]++
	if f.Limited > 0 {
		f.Limited--
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if c, err := r.Cookie("session-token"); err != nil || c.Value != f.Token || !f.SignedIn {
		http.Redirect(w, r, "/ap/signin", http.StatusFound)
		return
	}
	switch r.URL.Path {
	case "/cpe/yourpayments/transactions":
		http.SetCookie(w, &http.Cookie{Name: "session-token", Value: f.Token, Path: "/", MaxAge: 3600})
		fmt.Fprint(w, `<html><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"token":"tok","manageWalletRequest":{"requestContext":{"surfaceInfo":{"a":1},"localeInfo":{"locale":"en_US"}}}}}}</script></html>`)
	case "/payments-portal/data/iris/live/v1/data/manage/get-transactions":
		if f.PagesBeforeLimit > 0 && f.Requests[r.URL.Path] > f.PagesBeforeLimit {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("X-Amzn-Upx-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Start string `json:"exclusiveStartKey"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		start := 0
		fmt.Sscanf(body.Start, "page-%d", &start)
		end, next := min(start+2, len(f.Rows)), ""
		if end < len(f.Rows) {
			next = fmt.Sprintf("page-%d", end)
		}
		rows := f.Rows[start:end]
		if rows == nil {
			rows = []json.RawMessage{}
		}
		json.NewEncoder(w).Encode(map[string]any{"displayResponse": map[string]any{"transactionsList": rows, "lastEvaluatedKey": next}})
	case "/gp/css/summary/print.html":
		if page, ok := f.Orders[r.URL.Query().Get("orderID")]; ok {
			w.Write(page)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ChromeDir makes a Chrome data directory with the given profiles; each
// profile named in amazon holds an amazon.com session-token cookie with that
// value, encrypted with password the way Chrome 130 and later do.
func ChromeDir(t *testing.T, password string, profiles map[string]string, amazon map[string]string) string {
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
		if value, ok := amazon[d]; ok {
			host := ".amazon.com"
			sum := sha256.Sum256([]byte(host))
			pt := append(sum[:], value...)
			pad := aes.BlockSize - len(pt)%aes.BlockSize
			for range pad {
				pt = append(pt, byte(pad))
			}
			ct := make([]byte, len(pt))
			cipher.NewCBCEncrypter(block, []byte(strings.Repeat(" ", 16))).CryptBlocks(ct, pt)
			sql += fmt.Sprintf("\ninsert into cookies values ('%s', 'session-token', '/', '', x'%s', 1, 1, 13500000000000000);",
				host, hex.EncodeToString(append([]byte("v10"), ct...)))
		}
		if out, err := exec.Command("/usr/bin/sqlite3", filepath.Join(pdir, "Cookies"), sql).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v: %s", err, out)
		}
	}
	return dir
}
