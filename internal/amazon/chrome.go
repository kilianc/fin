package amazon

import (
	"fmt"
	"time"

	"github.com/kilianc/fin/internal/chrome"
)

// Chrome imports Amazon's session from a Chrome profile.
type Chrome chrome.Chrome

// Profile is one Chrome profile and whether it holds Amazon cookies.
type Profile struct {
	Dir    string `json:"dir"`
	Name   string `json:"name"`
	Amazon bool   `json:"signed_in_to_amazon"`
}

var ErrNoChrome = chrome.ErrNoChrome
var amazonHosts = []string{".amazon.com", "www.amazon.com", "amazon.com"}

func DefaultChromeDir() string                 { return chrome.DefaultChromeDir() }
func SafeStorageFromKeychain() (string, error) { return chrome.SafeStorageFromKeychain() }

// Profiles lists profiles without reading cookie values.
func (c Chrome) Profiles() ([]Profile, error) {
	browser := chrome.Chrome(c)
	profiles, err := browser.Profiles()
	if err != nil {
		return nil, err
	}
	out := make([]Profile, len(profiles))
	for i, p := range profiles {
		signed, _ := browser.HasCookies(p.Dir, amazonHosts)
		out[i] = Profile{Dir: p.Dir, Name: p.Name, Amazon: signed}
	}
	return out, nil
}

func (c Chrome) Find(key string) (Profile, error) {
	profile, err := chrome.Chrome(c).Find(key)
	if err != nil {
		return Profile{}, err
	}
	signed, _ := chrome.Chrome(c).HasCookies(profile.Dir, amazonHosts)
	return Profile{Dir: profile.Dir, Name: profile.Name, Amazon: signed}, nil
}

// Session reads only Amazon cookies into Amazon's own session.
func (c Chrome) Session(profile string) (*Session, error) {
	browser := chrome.Chrome(c)
	cookies, err := browser.Cookies(profile, amazonHosts, nil)
	if err != nil {
		return nil, err
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("Chrome profile %q is not signed in to Amazon", profile)
	}
	return &Session{Cookies: cookies, UserAgent: browser.Agent(), Profile: profile, SavedAt: time.Now().UTC()}, nil
}
