// Package costco reads a person's warehouse receipts from costco.com's
// undocumented website endpoints with their own Chrome sign-in.
package costco

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/chrome"
)

const (
	Origin     = "https://www.costco.com"
	spaID      = "a3a5186b-7c89-4b4c-93a8-dd604e930757"
	signInBase = "https://signin.costco.com/e0714dd4-784d-46d6-a278-3e29553483eb/"
	TokenURL   = signInBase + "b2c_1a_sso_wcs_signup_signin_214/oauth2/v2.0/token"
	OrdersURL  = "https://ecom-api.costco.com/ebusiness/order/v1/orders/graphql"
)

// ErrSignIn means Costco wants the person to sign in again.
var ErrSignIn = errors.New("Costco no longer accepts this sign-in")

// Session is Costco's sign-in, sealed by fin with its key in the Keychain.
// Only login reads Chrome. Every successful refresh keeps the rotated token.
type Session struct {
	Policy         string    `json:"policy"`
	RefreshToken   string    `json:"refresh_token"`
	IDToken        string    `json:"id_token"`
	IDExpires      time.Time `json:"id_expires"`
	RefreshExpires time.Time `json:"refresh_expires"`
	UserAgent      string    `json:"user_agent"`
	Profile        string    `json:"profile"`
	SavedAt        time.Time `json:"saved_at"`
}

func refreshKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "-refreshtoken-") && strings.Contains(key, spaID)
}

// SignedIn reports whether Chrome holds a Costco refresh-token entry. It
// does not decrypt it or ask for Keychain access.
func SignedIn(c chrome.Chrome, profile string) (bool, error) {
	entries, err := c.LocalStorage(profile, Origin)
	if err != nil {
		return false, err
	}
	for key := range entries {
		if refreshKey(key) {
			return true, nil
		}
	}
	return false, nil
}

// ChromeSession imports only Costco's refresh token. MSAL may encrypt cache
// entries with a key in the site's msal.cache.encryption cookie. Costco's
// own idToken selects the account and sign-in policy: older MSAL active
// account markers can survive a policy change.
func ChromeSession(c chrome.Chrome, profile string) (*Session, error) {
	entries, err := c.LocalStorage(profile, Origin)
	if err != nil {
		return nil, err
	}
	type credential struct{ Secret, CredentialType, ClientID, Environment, HomeAccountID string }
	var cached []credential
	var cookie string
	for key, value := range entries {
		lower := strings.ToLower(key)
		if !refreshKey(key) && !(strings.Contains(lower, "-idtoken-") && strings.Contains(lower, spaID)) {
			continue
		}
		var entry struct{ ID, Nonce, Data string }
		if json.Unmarshal([]byte(value), &entry) != nil {
			continue
		}
		if entry.Nonce != "" || entry.Data != "" {
			if cookie == "" {
				cookies, err := c.Cookies(profile, []string{".costco.com", "www.costco.com", "costco.com"}, []string{"msal.cache.encryption"})
				if err != nil {
					return nil, err
				}
				for _, ck := range cookies {
					if !ck.Expires.IsZero() && ck.Expires.Before(time.Now()) {
						continue
					}
					if cookie != "" && cookie != ck.Value {
						return nil, errors.New("costco: several cache keys in Chrome; sign in again")
					}
					cookie = ck.Value
				}
			}
			value, err = decryptCache(value, cookie)
			if err != nil {
				return nil, err
			}
		}
		var token credential
		if json.Unmarshal([]byte(value), &token) != nil || token.Secret == "" {
			continue
		}
		if !strings.EqualFold(token.ClientID, spaID) || !strings.EqualFold(token.Environment, "signin.costco.com") {
			continue
		}
		cached = append(cached, token)
	}
	var active struct{ HomeAccountID string }
	var policy string
	if idToken := entries["idToken"]; idToken != "" {
		var quoted string
		if json.Unmarshal([]byte(idToken), &quoted) == nil {
			idToken = quoted
		}
		policy, err = idTokenPolicy(idToken)
		if err != nil {
			return nil, err
		}
		for _, token := range cached {
			if !strings.EqualFold(token.CredentialType, "IdToken") || token.Secret != idToken {
				continue
			}
			if active.HomeAccountID != "" && active.HomeAccountID != token.HomeAccountID {
				return nil, ErrSignIn
			}
			active.HomeAccountID = token.HomeAccountID
		}
		if active.HomeAccountID == "" {
			return nil, ErrSignIn
		}
	} else if value, ok := entries["msal."+spaID+".active-account-filters"]; ok {
		if json.Unmarshal([]byte(value), &active) != nil || active.HomeAccountID == "" {
			return nil, ErrSignIn
		}
	}
	var tokens []credential
	for _, token := range cached {
		if !strings.EqualFold(token.CredentialType, "RefreshToken") {
			continue
		}
		if active.HomeAccountID != "" && token.HomeAccountID != active.HomeAccountID {
			continue
		}
		if len(tokens) == 0 || tokens[0].Secret != token.Secret {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		return nil, ErrSignIn
	}
	if len(tokens) != 1 {
		return nil, errors.New("costco: several sign-ins in this Chrome profile; sign out of Costco and sign in to the intended account")
	}
	if policy == "" {
		// B2C cache account IDs include the policy after the user's ID.
		home := strings.ToLower(tokens[0].HomeAccountID)
		if i := strings.Index(home, "b2c_"); i >= 0 {
			candidate, _, _ := strings.Cut(home[i:], ".")
			if signInPolicy.MatchString(candidate) {
				policy = candidate
			}
		}
	}
	return &Session{Policy: policy, RefreshToken: tokens[0].Secret, UserAgent: c.Agent(), Profile: profile, SavedAt: time.Now().UTC()}, nil
}

var signInPolicy = regexp.MustCompile(`(?i)^b2c_1a_sso_wcs_signup_signin_[0-9]+$`)

// idTokenPolicy reads routing information, not proof of sign-in. The token
// endpoint verifies the refresh token. Restrict policies to Costco's known
// sign-in family and always send them to the fixed Costco authority.
func idTokenPolicy(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrSignIn
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrSignIn
	}
	var claims struct{ Iss, Aud, Acr, Tfp string }
	if json.Unmarshal(data, &claims) != nil || claims.Iss != signInBase+"v2.0/" || claims.Aud != spaID {
		return "", ErrSignIn
	}
	policy := claims.Acr
	if policy == "" {
		policy = claims.Tfp
	}
	if !signInPolicy.MatchString(policy) {
		return "", ErrSignIn
	}
	return strings.ToLower(policy), nil
}

// decryptCache follows MSAL's HKDF-SHA256/AES-256-GCM cache format. A new
// key is derived per entry from its nonce, with the SPA ID as context.
// https://github.com/AzureAD/microsoft-authentication-library-for-js/blob/dev/lib/msal-browser/src/crypto/BrowserCrypto.ts
func decryptCache(value, cookie string) (string, error) {
	if decoded, err := url.PathUnescape(cookie); err == nil {
		cookie = decoded
	}
	var k struct{ ID, Key string }
	var e struct{ ID, Nonce, Data string }
	bad := errors.New("costco: Chrome's Costco cache cannot be decrypted; sign in at costco.com again")
	if json.Unmarshal([]byte(cookie), &k) != nil || json.Unmarshal([]byte(value), &e) != nil || k.ID == "" || e.ID != k.ID {
		return "", bad
	}
	decode := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")) }
	base, err := decode(k.Key)
	if err != nil || len(base) != 32 {
		return "", bad
	}
	nonce, err := decode(e.Nonce)
	if err != nil || len(nonce) != 16 {
		return "", bad
	}
	data, err := decode(e.Data)
	if err != nil {
		return "", bad
	}
	key, err := hkdf.Key(sha256.New, base, nonce, spaID, 32)
	if err != nil {
		return "", bad
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", bad
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", bad
	}
	plain, err := gcm.Open(nil, make([]byte, gcm.NonceSize()), data, nil)
	if err != nil {
		return "", bad
	}
	return string(plain), nil
}
