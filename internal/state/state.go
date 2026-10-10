// Package state keeps fin's non-secret state in ~/.config/fin/state.json.
// Access tokens live in the Keychain, never here.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const fileMode = 0o600

// Item is one linked Plaid Item.
type Item struct {
	Name            string    `json:"name"`
	ItemID          string    `json:"item_id"`
	Env             string    `json:"env"`
	Kind            string    `json:"kind"`
	Products        []string  `json:"products"`
	InstitutionID   string    `json:"institution_id"`
	InstitutionName string    `json:"institution_name"`
	LinkedAt        time.Time `json:"linked_at"`
	// LastSync is when transactions were last synced into the local store,
	// which also keeps the /transactions/sync cursor.
	LastSync *time.Time `json:"last_sync,omitempty"`
}

// HasProduct reports whether the Item was linked with product.
func (it Item) HasProduct(product string) bool {
	for _, p := range it.Products {
		if p == product {
			return true
		}
	}
	return false
}

type State struct {
	Version int `json:"version"`
	// Env is the Plaid environment chosen with `fin env`. PLAID_ENV overrides it.
	Env string `json:"env,omitempty"`
	// Epoch is the first day, YYYY-MM-DD, of the finances fin reports on,
	// such as when a household started sharing money. Set with `fin epoch`.
	Epoch string `json:"epoch,omitempty"`
	Items []Item `json:"items"`
	// Sheets maps an environment to the spreadsheet fin sheet writes to.
	Sheets map[string]string `json:"sheets,omitempty"`
	// Retailers lists the connected retailer accounts, such as Amazon.com.
	Retailers []RetailerAccount `json:"retailers,omitempty"`
	// Unlinked lists the Items removed with fin unlink. Plaid still counts
	// them against the plan's Item limit.
	Unlinked []UnlinkedItem `json:"unlinked,omitempty"`
}

// UnlinkedItem is an Item removed with fin unlink.
type UnlinkedItem struct {
	Name            string    `json:"name"`
	ItemID          string    `json:"item_id"`
	Env             string    `json:"env"`
	InstitutionName string    `json:"institution_name"`
	LinkedAt        time.Time `json:"linked_at"`
	UnlinkedAt      time.Time `json:"unlinked_at"`
}

// RetailerAccount is one sign-in at a retailer, imported from a Chrome
// profile. The session itself is encrypted on disk with a key in the
// Keychain.
type RetailerAccount struct {
	Retailer    string     `json:"retailer"` // such as "amazon"
	Name        string     `json:"name"`
	Env         string     `json:"env"`
	Profile     string     `json:"profile"`      // Chrome's profile directory, such as "Default"
	ProfileName string     `json:"profile_name"` // the name Chrome shows for it
	ConnectedAt time.Time  `json:"connected_at"`
	LastSync    *time.Time `json:"last_sync,omitempty"`
}

func (a RetailerAccount) is(retailer, env, name string) bool {
	return a.Retailer == retailer && a.Env == env && strings.EqualFold(a.Name, name)
}

// RetailerAccounts returns the retailer's accounts connected in env.
func (s *State) RetailerAccounts(retailer, env string) []RetailerAccount {
	out := []RetailerAccount{}
	for _, a := range s.Retailers {
		if a.Retailer == retailer && a.Env == env {
			out = append(out, a)
		}
	}
	return out
}

// FindRetailer looks a retailer account up by name.
func (s *State) FindRetailer(retailer, env, name string) (RetailerAccount, bool) {
	for _, a := range s.Retailers {
		if a.is(retailer, env, name) {
			return a, true
		}
	}
	return RetailerAccount{}, false
}

// PutRetailer replaces the account with the same retailer and name, or
// appends it.
func (s *State) PutRetailer(acct RetailerAccount) {
	for i, a := range s.Retailers {
		if a.is(acct.Retailer, acct.Env, acct.Name) {
			s.Retailers[i] = acct
			return
		}
	}
	s.Retailers = append(s.Retailers, acct)
}

// RemoveRetailer forgets an account.
func (s *State) RemoveRetailer(retailer, env, name string) {
	out := s.Retailers[:0]
	for _, a := range s.Retailers {
		if !a.is(retailer, env, name) {
			out = append(out, a)
		}
	}
	s.Retailers = out
}

// DefaultPath is $FIN_CONFIG_DIR/state.json, or ~/.config/fin/state.json.
func DefaultPath() (string, error) {
	if dir := os.Getenv("FIN_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "state.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "fin", "state.json"), nil
}

// Load reads the state file. A missing file is an empty state.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{Version: 1, Items: []Item{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Items == nil {
		s.Items = []Item{}
	}
	return &s, nil
}

// Save writes the state atomically with mode 0600.
func (s *State) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ForEnv returns the Items linked in env, in link order.
func (s *State) ForEnv(env string) []Item {
	items := []Item{}
	for _, it := range s.Items {
		if it.Env == env {
			items = append(items, it)
		}
	}
	return items
}

// Find looks an Item up by name or item_id.
func (s *State) Find(env, key string) (Item, bool) {
	for _, it := range s.Items {
		if it.Env == env && (strings.EqualFold(it.Name, key) || it.ItemID == key) {
			return it, true
		}
	}
	return Item{}, false
}

// Put replaces the Item with the same item_id, or appends it.
func (s *State) Put(item Item) {
	for i, it := range s.Items {
		if it.ItemID == item.ItemID {
			s.Items[i] = item
			return
		}
	}
	s.Items = append(s.Items, item)
}

// Unlink moves the Item with itemID in env to Unlinked.
func (s *State) Unlink(env, itemID string, at time.Time) {
	for i, it := range s.Items {
		if it.Env == env && it.ItemID == itemID {
			s.Items = append(s.Items[:i], s.Items[i+1:]...)
			s.Unlinked = append(s.Unlinked, UnlinkedItem{Name: it.Name, ItemID: it.ItemID, Env: it.Env, InstitutionName: it.InstitutionName, LinkedAt: it.LinkedAt, UnlinkedAt: at.UTC()})
			return
		}
	}
}

// SlotsUsed counts the Items ever linked in env, unlinked ones included:
// the plan's limit counts every Item created.
func (s *State) SlotsUsed(env string) int {
	n := len(s.ForEnv(env))
	for _, u := range s.Unlinked {
		if u.Env == env {
			n++
		}
	}
	return n
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// UniqueName derives a short command-line name from an institution name,
// such as "american-express", adding -2, -3 when one is taken.
func (s *State) UniqueName(env, institution string) string {
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(institution), "-"), "-")
	if base == "" {
		base = "item"
	}
	name := base
	for n := 2; ; n++ {
		if _, taken := s.Find(env, name); !taken {
			return name
		}
		name = base + "-" + strconv.Itoa(n)
	}
}
