// Package sheetstest is an in-memory Google token endpoint and Sheets API
// for tests.
package sheetstest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kilianc/fin/internal/sheets"
)

// Sheet is one fake spreadsheet.
type Sheet struct {
	Title string
	Tabs  map[string]int
	// Values holds the last values written to each tab, by title.
	Values map[string][][]any
	// Options holds each write's valueInputOption, by tab title.
	Options map[string]string
	// Widths holds fixed column widths in pixels, by "sheetId:column".
	Widths map[string]int
}

// Server fakes Google's token endpoint and the parts of the Sheets API fin uses.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	Sheets  map[string]*Sheet
	nextID  int
	Refresh string // the refresh token the token endpoint accepts
	Code    string // the authorization code the token endpoint accepts
}

// New starts a fake that accepts refresh token "rt" and code "code".
func New(t *testing.T) *Server {
	f := &Server{Sheets: map[string]*Sheet{}, Refresh: "rt", Code: "code"}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Client returns an OAuthClient pointing at the fake token endpoint.
func (f *Server) Client() sheets.OAuthClient {
	return sheets.OAuthClient{ID: "client", Secret: "secret", AuthURL: f.URL + "/auth", TokenURL: f.URL + "/token"}
}

// Delete removes a spreadsheet, as if the user deleted it.
func (f *Server) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Sheets, id)
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		r.ParseForm()
		ok := (r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == f.Refresh) ||
			(r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") == f.Code && r.Form.Get("code_verifier") != "")
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error": "invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "refresh_token": f.Refresh})
		return
	}
	if r.Header.Get("Authorization") != "Bearer at" {
		http.Error(w, `{"error": {"message": "unauthenticated"}}`, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v4/spreadsheets")
	switch {
	case r.Method == http.MethodPost && path == "":
		var body struct {
			Properties struct{ Title string }
			Sheets     []struct{ Properties struct{ Title string } }
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		id := fmt.Sprintf("sheet-%d", f.nextID)
		sh := &Sheet{Title: body.Properties.Title, Tabs: map[string]int{}, Values: map[string][][]any{}, Options: map[string]string{}, Widths: map[string]int{}}
		for i, s := range body.Sheets {
			sh.Tabs[s.Properties.Title] = i + 1
		}
		f.Sheets[id] = sh
		f.writeSpreadsheet(w, id)
	case r.Method == http.MethodGet && !strings.Contains(path, "/values/"):
		id := strings.TrimPrefix(path, "/")
		if f.Sheets[id] == nil {
			http.Error(w, `{"error": {"message": "not found"}}`, http.StatusNotFound)
			return
		}
		f.writeSpreadsheet(w, id)
	case r.Method == http.MethodPost && strings.HasSuffix(path, ":batchUpdate"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), ":batchUpdate")
		sh := f.Sheets[id]
		if sh == nil {
			http.Error(w, `{"error": {"message": "not found"}}`, http.StatusNotFound)
			return
		}
		var body struct {
			Requests []map[string]json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&body)
		replies := []any{}
		for _, req := range body.Requests {
			if raw, ok := req["addSheet"]; ok {
				var add struct{ Properties struct{ Title string } }
				json.Unmarshal(raw, &add)
				sid := len(sh.Tabs) + 1
				sh.Tabs[add.Properties.Title] = sid
				replies = append(replies, map[string]any{"addSheet": map[string]any{"properties": map[string]any{"sheetId": sid}}})
				continue
			}
			if raw, ok := req["updateDimensionProperties"]; ok {
				var u struct {
					Range struct {
						SheetID    int `json:"sheetId"`
						StartIndex int `json:"startIndex"`
					}
					Properties struct {
						PixelSize int `json:"pixelSize"`
					}
				}
				json.Unmarshal(raw, &u)
				sh.Widths[fmt.Sprintf("%d:%d", u.Range.SheetID, u.Range.StartIndex)] = u.Properties.PixelSize
			}
			replies = append(replies, map[string]any{})
		}
		json.NewEncoder(w).Encode(map[string]any{"replies": replies})
	case r.Method == http.MethodPut && strings.Contains(path, "/values/"):
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/values/", 2)
		sh := f.Sheets[parts[0]]
		if sh == nil {
			http.Error(w, `{"error": {"message": "not found"}}`, http.StatusNotFound)
			return
		}
		tab := strings.TrimSuffix(strings.TrimPrefix(parts[1], "'"), "'!A1")
		var body struct{ Values [][]any }
		json.NewDecoder(r.Body).Decode(&body)
		sh.Values[tab] = body.Values
		sh.Options[tab] = r.URL.Query().Get("valueInputOption")
		fmt.Fprint(w, `{}`)
	default:
		http.Error(w, `{"error": {"message": "unexpected request"}}`, http.StatusBadRequest)
	}
}

func (f *Server) writeSpreadsheet(w http.ResponseWriter, id string) {
	sh := f.Sheets[id]
	sheets := []any{}
	for title, sid := range sh.Tabs {
		sheets = append(sheets, map[string]any{"properties": map[string]any{"sheetId": sid, "title": title}})
	}
	json.NewEncoder(w).Encode(map[string]any{
		"spreadsheetId": id, "spreadsheetUrl": "https://docs.google.com/spreadsheets/d/" + id, "sheets": sheets,
	})
}
