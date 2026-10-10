// Package costcotest serves synthetic Costco receipts and sign-ins only.
package costcotest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/chrome/chrometest"
	"github.com/kilianc/fin/internal/costco"
)

const ClientID = "a3a5186b-7c89-4b4c-93a8-dd604e930757"

// Receipt makes a warehouse sale with a separate instant-savings line.
func Receipt(barcode, day string) map[string]any {
	return map[string]any{
		"transactionBarcode": barcode, "transactionDate": day, "transactionDateTime": day + "T12:34:56",
		"warehouseNumber": "0123", "warehouseName": "Example Warehouse", "registerNumber": 7, "transactionNumber": "42", "transactionType": "Sale",
		"subTotal": 13.0, "taxes": 1.04, "total": 14.04, "instantSavings": 2.0,
		"itemArray": []any{
			map[string]any{"itemNumber": "101", "itemDescription01": "SYNTHETIC SOAP", "itemDescription02": "", "unit": 2, "itemUnitPriceAmount": 5.0, "amount": 10.0, "taxFlag": "A", "itemDepartmentNumber": 12},
			map[string]any{"itemNumber": "202", "itemDescription01": "SYNTHETIC APPLES", "unit": 1.25, "itemUnitPriceAmount": 4.0, "amount": 5.0, "taxFlag": "N", "itemDepartmentNumber": 13},
			map[string]any{"itemNumber": "101", "itemDescription01": "INSTANT SAVINGS", "unit": 1, "itemUnitPriceAmount": -2.0, "amount": -2.0, "taxFlag": "A", "itemDepartmentNumber": 12},
		},
		"tenderArray": []any{map[string]any{"tenderTypeName": "Visa", "tenderDescription": "Visa", "amountTender": 14.04, "displayAccountNumber": "********4242"}},
	}
}

func Raw(t *testing.T, r map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ChromeDir has just one fake Costco session in Local Storage.
func ChromeDir(t *testing.T) string {
	t.Helper()
	dir := chrometest.Dir(t, "peanuts", map[string]string{"Default": "Pat"}, nil)
	chrometest.LocalStorage(t, dir, "Default", costco.Origin, map[string]string{
		"synthetic-signin.costco.com-refreshtoken-" + ClientID + "---": `{"secret":"synthetic-refresh","credentialType":"RefreshToken","clientId":"` + ClientID + `","environment":"signin.costco.com"}`,
	})
	return dir
}

// Server accepts only synthetic tokens, records date windows, and can stop
// one window to exercise interrupted syncs. Fields are set between calls.
type Server struct {
	*httptest.Server
	mu                sync.Mutex
	Rows              []json.RawMessage
	FailureAt, Status int
	RefreshStatus     int
	windows           []costco.Window
	refreshes         int
}

func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{Status: 429}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if r.URL.Path == "/token" {
			s.refreshes++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_id") != ClientID || r.Form.Get("grant_type") != "refresh_token" {
				t.Error("wrong refresh grant")
			}
			if s.RefreshStatus != 0 {
				w.WriteHeader(s.RefreshStatus)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"synthetic-token-must-not-leak"}`)
				return
			}
			fmt.Fprintf(w, `{"id_token":"synthetic-id","refresh_token":"synthetic-rotated-%d","id_token_expires_in":3600,"refresh_token_expires_in":86400}`, s.refreshes)
			return
		}
		if r.URL.Path != "/receipts" {
			t.Errorf("path = %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("costco-x-authorization") != "Bearer synthetic-id" || r.Header.Get("Origin") != costco.Origin || r.Header.Get("client-identifier") == "" {
			t.Error("missing Costco headers")
		}
		var request struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Variables["documentType"] != "warehouse" {
			t.Error("asked for more than warehouse receipts")
		}
		day := func(key string) string {
			d, err := time.Parse("1/02/2006", request.Variables[key])
			if err != nil {
				t.Error(err)
			}
			return d.Format("2006-01-02")
		}
		window := costco.Window{Start: day("startDate"), End: day("endDate")}
		s.windows = append(s.windows, window)
		if s.FailureAt > 0 && len(s.windows) == s.FailureAt {
			w.WriteHeader(s.Status)
			return
		}
		rows := []json.RawMessage{}
		for _, raw := range s.Rows {
			var receipt struct {
				Day string `json:"transactionDate"`
			}
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Error(err)
			}
			if receipt.Day >= window.Start && receipt.Day <= window.End {
				rows = append(rows, raw)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"receiptsWithCounts": map[string]any{"receipts": rows}}})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Windows() []costco.Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]costco.Window(nil), s.windows...)
}
func (s *Server) Refreshes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.refreshes }
