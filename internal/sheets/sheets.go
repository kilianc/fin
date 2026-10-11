package sheets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

// Service calls the Google Sheets API as the signed-in user.
type Service struct {
	http *http.Client
	base string
}

// New returns a Service that refreshes its access token from refreshToken.
// base is the API root; empty means Google's.
func New(ctx context.Context, c OAuthClient, refreshToken, base string) *Service {
	if base == "" {
		base = "https://sheets.googleapis.com"
	}
	client := c.config("").Client(ctx, &oauth2.Token{RefreshToken: refreshToken})
	return &Service{http: client, base: base}
}

// Error is a failed Sheets API call.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("google sheets: %d %s", e.Status, e.Message) }

// IsGone reports whether the spreadsheet no longer exists or fin lost access
// to it, so a new one should be created.
func IsGone(err error) bool {
	e, ok := err.(*Error)
	return ok && (e.Status == http.StatusNotFound || e.Status == http.StatusForbidden)
}

func (s *Service) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		return &Error{Status: resp.StatusCode, Message: e.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Spreadsheet is the part of a spreadsheet fin needs.
type Spreadsheet struct {
	ID   string
	URL  string
	Tabs map[string]int // title -> sheetId
}

type spreadsheetJSON struct {
	SpreadsheetID  string `json:"spreadsheetId"`
	SpreadsheetURL string `json:"spreadsheetUrl"`
	Sheets         []struct {
		Properties struct {
			SheetID int    `json:"sheetId"`
			Title   string `json:"title"`
		} `json:"properties"`
	} `json:"sheets"`
}

func (j spreadsheetJSON) spreadsheet() *Spreadsheet {
	sp := &Spreadsheet{ID: j.SpreadsheetID, URL: j.SpreadsheetURL, Tabs: map[string]int{}}
	for _, sh := range j.Sheets {
		sp.Tabs[sh.Properties.Title] = sh.Properties.SheetID
	}
	return sp
}

// Create makes a new spreadsheet with the given tabs.
func (s *Service) Create(ctx context.Context, title string, tabs []string) (*Spreadsheet, error) {
	sheets := []map[string]any{}
	for _, t := range tabs {
		sheets = append(sheets, map[string]any{"properties": map[string]any{"title": t}})
	}
	body := map[string]any{"properties": map[string]any{"title": title}, "sheets": sheets}
	var out spreadsheetJSON
	if err := s.call(ctx, http.MethodPost, "/v4/spreadsheets", body, &out); err != nil {
		return nil, err
	}
	return out.spreadsheet(), nil
}

// Get reads a spreadsheet's URL and tabs.
func (s *Service) Get(ctx context.Context, id string) (*Spreadsheet, error) {
	var out spreadsheetJSON
	path := "/v4/spreadsheets/" + url.PathEscape(id) + "?fields=" + url.QueryEscape("spreadsheetId,spreadsheetUrl,sheets.properties(sheetId,title)")
	if err := s.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.spreadsheet(), nil
}

// Kind decides how a column's values are written and formatted.
type Kind int

const (
	Text Kind = iota
	Number
	Money
	Date
	Bool
)

type Column struct {
	Name string
	Kind Kind
	// Width, in pixels, fixes the width of a column of long text, which is
	// then clipped, so one long bank description does not push the rest of
	// the sheet off screen. Other columns fit their contents.
	Width int
}

// Tab is one sheet's full contents. Date cells are "YYYY-MM-DD" strings or nil.
type Tab struct {
	Title   string
	Columns []Column
	Rows    [][]any
}

// Write replaces a tab's contents: it adds the tab if the user deleted it,
// clears it, writes the header and rows as raw values (so text from a bank
// can never run as a formula), and formats the columns.
func (s *Service) Write(ctx context.Context, sp *Spreadsheet, tab Tab) error {
	sheetID, ok := sp.Tabs[tab.Title]
	if !ok {
		var out struct {
			Replies []struct {
				AddSheet struct {
					Properties struct {
						SheetID int `json:"sheetId"`
					} `json:"properties"`
				} `json:"addSheet"`
			} `json:"replies"`
		}
		req := map[string]any{"requests": []any{map[string]any{"addSheet": map[string]any{"properties": map[string]any{"title": tab.Title}}}}}
		if err := s.call(ctx, http.MethodPost, "/v4/spreadsheets/"+url.PathEscape(sp.ID)+":batchUpdate", req, &out); err != nil {
			return err
		}
		sheetID = out.Replies[0].AddSheet.Properties.SheetID
		sp.Tabs[tab.Title] = sheetID
	}

	rows := len(tab.Rows) + 1
	values := make([][]any, 0, rows)
	header := make([]any, len(tab.Columns))
	for i, c := range tab.Columns {
		header[i] = c.Name
	}
	values = append(values, header)
	for _, r := range tab.Rows {
		out := make([]any, len(r))
		for i, v := range r {
			out[i] = cell(tab.Columns[i].Kind, v)
		}
		values = append(values, out)
	}

	// Size the grid to the data first: clearing keeps the old row count, and
	// a write past the grid's edge fails.
	requests := []any{
		map[string]any{"updateSheetProperties": map[string]any{
			"properties": map[string]any{"sheetId": sheetID, "gridProperties": map[string]any{
				"rowCount": max(rows, 2), "columnCount": len(tab.Columns), "frozenRowCount": 1,
			}},
			"fields": "gridProperties(rowCount,columnCount,frozenRowCount)",
		}},
		map[string]any{"updateCells": map[string]any{
			"range":  map[string]any{"sheetId": sheetID},
			"fields": "userEnteredValue",
		}},
	}
	if err := s.batchUpdate(ctx, sp.ID, requests); err != nil {
		return err
	}
	rng := url.PathEscape(quoteTab(tab.Title) + "!A1")
	put := map[string]any{"majorDimension": "ROWS", "values": values}
	if err := s.call(ctx, http.MethodPut, "/v4/spreadsheets/"+url.PathEscape(sp.ID)+"/values/"+rng+"?valueInputOption=RAW", put, nil); err != nil {
		return err
	}

	formats := []any{
		map[string]any{"repeatCell": map[string]any{
			"range":  map[string]any{"sheetId": sheetID, "startRowIndex": 0, "endRowIndex": 1},
			"cell":   map[string]any{"userEnteredFormat": map[string]any{"textFormat": map[string]any{"bold": true}}},
			"fields": "userEnteredFormat.textFormat.bold",
		}},
	}
	for i, c := range tab.Columns {
		var pattern, typ string
		switch c.Kind {
		case Money:
			typ, pattern = "NUMBER", "#,##0.00"
		case Date:
			typ, pattern = "DATE", "yyyy-mm-dd"
		default:
			continue
		}
		formats = append(formats, map[string]any{"repeatCell": map[string]any{
			"range":  map[string]any{"sheetId": sheetID, "startRowIndex": 1, "startColumnIndex": i, "endColumnIndex": i + 1},
			"cell":   map[string]any{"userEnteredFormat": map[string]any{"numberFormat": map[string]any{"type": typ, "pattern": pattern}}},
			"fields": "userEnteredFormat.numberFormat",
		}})
	}
	formats = append(formats, map[string]any{"autoResizeDimensions": map[string]any{
		"dimensions": map[string]any{"sheetId": sheetID, "dimension": "COLUMNS", "startIndex": 0, "endIndex": len(tab.Columns)},
	}})
	for i, c := range tab.Columns {
		if c.Width == 0 {
			continue
		}
		formats = append(formats,
			map[string]any{"updateDimensionProperties": map[string]any{
				"range":      map[string]any{"sheetId": sheetID, "dimension": "COLUMNS", "startIndex": i, "endIndex": i + 1},
				"properties": map[string]any{"pixelSize": c.Width},
				"fields":     "pixelSize",
			}},
			map[string]any{"repeatCell": map[string]any{
				"range":  map[string]any{"sheetId": sheetID, "startRowIndex": 1, "startColumnIndex": i, "endColumnIndex": i + 1},
				"cell":   map[string]any{"userEnteredFormat": map[string]any{"wrapStrategy": "CLIP"}},
				"fields": "userEnteredFormat.wrapStrategy",
			}})
	}
	return s.batchUpdate(ctx, sp.ID, formats)
}

func (s *Service) batchUpdate(ctx context.Context, id string, requests []any) error {
	return s.call(ctx, http.MethodPost, "/v4/spreadsheets/"+url.PathEscape(id)+":batchUpdate", map[string]any{"requests": requests}, nil)
}

func quoteTab(title string) string {
	return "'" + string(bytes.ReplaceAll([]byte(title), []byte("'"), []byte("''"))) + "'"
}

// sheetsEpoch is day zero of a spreadsheet date serial.
var sheetsEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// cell converts a value for a RAW write: dates become serial numbers that the
// column's date format displays, and everything else passes through.
func cell(kind Kind, v any) any {
	switch x := v.(type) {
	case nil:
		return ""
	case *string:
		if x == nil {
			return ""
		}
		v = *x
	case *float64:
		if x == nil {
			return ""
		}
		v = *x
	}
	if s, ok := v.(string); ok && kind == Date {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return int(t.Sub(sheetsEpoch).Hours() / 24)
		}
	}
	return v
}
