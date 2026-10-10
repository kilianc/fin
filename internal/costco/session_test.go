package costco_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kilianc/fin/internal/chrome"
	"github.com/kilianc/fin/internal/chrome/chrometest"
	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/costco/costcotest"
)

func TestChromeSessionPlainAndEncrypted(t *testing.T) {
	secret := `{"secret":"synthetic-refresh","credentialType":"RefreshToken","clientId":"` + costcotest.ClientID + `","environment":"signin.costco.com"}`
	for _, encrypted := range []bool{false, true} {
		value, cookie := secret, ""
		if encrypted {
			value, cookie = encryptedCache(t, secret)
		}
		cookies := map[string][]*http.Cookie{"Default": {
			{Domain: ".costco.com", Name: "unrelated", Value: "not-session-material"},
			{Domain: ".example.com", Name: "msal.cache.encryption", Value: "wrong-origin"},
		}}
		if encrypted {
			cookies["Default"] = append(cookies["Default"], &http.Cookie{Domain: ".costco.com", Name: "msal.cache.encryption", Value: cookie})
		}
		dir := chrometest.Dir(t, "peanuts", map[string]string{"Default": "Pat"}, cookies)
		key := "synthetic-signin.costco.com-refreshtoken-" + costcotest.ClientID + "---"
		chrometest.LocalStorage(t, dir, "Default", costco.Origin, map[string]string{key: value, "other-client-refreshtoken-0000": "not a Costco token"})
		chrometest.LocalStorage(t, dir, "Default", "https://unrelated.example", map[string]string{key: "wrong origin"})
		asked := 0
		c := chrome.Chrome{Dir: dir, SafeStorage: func() (string, error) { asked++; return "peanuts", nil }}
		if signed, err := costco.SignedIn(c, "Default"); err != nil || !signed || asked != 0 {
			t.Fatalf("signed=%v asked=%d err=%v", signed, asked, err)
		}
		s, err := costco.ChromeSession(c, "Default")
		if err != nil || s.RefreshToken != "synthetic-refresh" || s.Profile != "Default" {
			t.Fatalf("import: %v", err)
		}
		want := 0
		if encrypted {
			want = 1
		}
		if asked != want {
			t.Errorf("Keychain requests=%d; want %d", asked, want)
		}
		if _, err := costco.ChromeSession(c, "../other"); err == nil {
			t.Error("accepted profile traversal")
		}
	}
}

// The fixture derives HKDF with HMAC directly, independently of hkdf.Key
// used by the importer, and wraps the cache key in a Chrome-encrypted cookie.
func encryptedCache(t *testing.T, plain string) (string, string) {
	t.Helper()
	base, nonce := bytes.Repeat([]byte{0x5a}, 32), bytes.Repeat([]byte{0x3c}, 16)
	extract := hmac.New(sha256.New, nonce)
	extract.Write(base)
	expand := hmac.New(sha256.New, extract.Sum(nil))
	expand.Write([]byte(costcotest.ClientID))
	expand.Write([]byte{1})
	block, err := aes.NewCipher(expand.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	data := gcm.Seal(nil, make([]byte, 12), []byte(plain), nil)
	entry, _ := json.Marshal(map[string]string{"id": "synthetic-cache-key", "nonce": encode(nonce), "data": encode(data)})
	cookie, _ := json.Marshal(map[string]string{"id": "synthetic-cache-key", "key": encode(base)})
	return string(entry), url.QueryEscape(string(cookie))
}

func TestChromeSessionRejectsAmbiguousAndDamagedCache(t *testing.T) {
	secret := `{"secret":"synthetic-refresh","credentialType":"RefreshToken","clientId":"` + costcotest.ClientID + `","environment":"signin.costco.com"}`
	for _, encrypted := range []bool{false, true} {
		entries := map[string]string{}
		cookies := map[string][]*http.Cookie{}
		key := "synthetic-signin.costco.com-refreshtoken-" + costcotest.ClientID + "---"
		if encrypted {
			value, cookie := encryptedCache(t, secret)
			entries[key] = strings.Replace(value, "synthetic-cache-key", "old-cache-key", 1)
			cookies["Default"] = []*http.Cookie{{Domain: ".costco.com", Name: "msal.cache.encryption", Value: cookie}}
		} else {
			entries[key] = secret
			entries["another-"+key] = strings.Replace(secret, "synthetic-refresh", "other-synthetic-refresh", 1)
		}
		dir := chrometest.Dir(t, "peanuts", map[string]string{"Default": "Pat"}, cookies)
		chrometest.LocalStorage(t, dir, "Default", costco.Origin, entries)
		_, err := costco.ChromeSession(chrome.Chrome{Dir: dir, SafeStorage: func() (string, error) { return "peanuts", nil }}, "Default")
		if err == nil || strings.Contains(err.Error(), "synthetic-refresh") {
			t.Errorf("unsafe cache accepted or exposed: %v", err)
		}
	}
}

func TestChromeSessionUsesActiveAccount(t *testing.T) {
	for _, tc := range []struct {
		name, active, secondAccount, want string
	}{
		{"current", `{"homeAccountId":"current-account","localAccountId":"current-local"}`, "older-account", "current-refresh"},
		{"other current", `{"homeAccountId":"older-account"}`, "older-account", "older-refresh"},
		{"missing token", `{"homeAccountId":"missing-account"}`, "older-account", ""},
		{"ambiguous token", `{"homeAccountId":"current-account"}`, "current-account", ""},
		{"damaged filter", `{`, "older-account", ""},
		{"empty filter", `{}`, "older-account", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := map[string]string{"msal." + costcotest.ClientID + ".active-account-filters": tc.active}
			for i, acct := range []string{"current-account", tc.secondAccount} {
				secret := []string{"current-refresh", "older-refresh"}[i]
				token, _ := json.Marshal(map[string]string{"secret": secret, "credentialType": "RefreshToken", "clientId": costcotest.ClientID, "environment": "signin.costco.com", "homeAccountId": acct})
				entries[secret+"-signin.costco.com-refreshtoken-"+costcotest.ClientID+"---"] = string(token)
			}
			dir := chrometest.Dir(t, "peanuts", map[string]string{"Default": "Pat"}, nil)
			chrometest.LocalStorage(t, dir, "Default", costco.Origin, entries)
			sess, err := costco.ChromeSession(chrome.Chrome{Dir: dir}, "Default")
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted missing or ambiguous active account")
				}
			} else if err != nil || sess.RefreshToken != tc.want {
				t.Fatalf("active account was not selected: %v", err)
			}
		})
	}
}

func TestChromeSessionUsesSitesCurrentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy, issuer, account string
		encrypted                     bool
		want                          bool
	}{
		{"new policy with stale MSAL marker", "B2C_1A_SSO_WCS_signup_signin_900", "https://signin.costco.com/e0714dd4-784d-46d6-a278-3e29553483eb/v2.0/", "current-account", false, true},
		{"encrypted", "B2C_1A_SSO_WCS_signup_signin_900", "https://signin.costco.com/e0714dd4-784d-46d6-a278-3e29553483eb/v2.0/", "current-account", true, true},
		{"no matching cached identity", "B2C_1A_SSO_WCS_signup_signin_900", "https://signin.costco.com/e0714dd4-784d-46d6-a278-3e29553483eb/v2.0/", "missing-account", false, false},
		{"other authority", "B2C_1A_SSO_WCS_signup_signin_900", "https://other.example/", "current-account", false, false},
		{"invalid policy", "../other", "https://signin.costco.com/e0714dd4-784d-46d6-a278-3e29553483eb/v2.0/", "current-account", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, _ := json.Marshal(map[string]string{"iss": tc.issuer, "aud": costcotest.ClientID, "acr": tc.policy})
			idToken := "synthetic." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			entries := map[string]string{"idToken": idToken, "msal." + costcotest.ClientID + ".active-account-filters": `{"homeAccountId":"old-account"}`}
			cookies := map[string][]*http.Cookie{}
			for _, token := range []struct{ kind, account, secret string }{
				{"IdToken", tc.account, idToken},
				{"RefreshToken", "current-account", "current-refresh"},
				{"RefreshToken", "old-account", "old-refresh"},
			} {
				b, _ := json.Marshal(map[string]string{"secret": token.secret, "credentialType": token.kind, "clientId": costcotest.ClientID, "environment": "signin.costco.com", "homeAccountId": token.account})
				value := string(b)
				if tc.encrypted {
					var cookie string
					value, cookie = encryptedCache(t, value)
					cookies["Default"] = []*http.Cookie{{Domain: ".costco.com", Name: "msal.cache.encryption", Value: cookie}}
				}
				entries[token.account+"-signin.costco.com-"+strings.ToLower(token.kind)+"-"+costcotest.ClientID+"---"] = value
			}
			dir := chrometest.Dir(t, "peanuts", map[string]string{"Default": "Pat"}, cookies)
			chrometest.LocalStorage(t, dir, "Default", costco.Origin, entries)
			sess, err := costco.ChromeSession(chrome.Chrome{Dir: dir, SafeStorage: func() (string, error) { return "peanuts", nil }}, "Default")
			if !tc.want {
				if err == nil {
					t.Fatal("accepted invalid current identity or policy")
				}
			} else if err != nil || sess.RefreshToken != "current-refresh" || sess.Policy != "b2c_1a_sso_wcs_signup_signin_900" {
				t.Fatalf("current site session was not selected: %v", err)
			}
		})
	}
}
