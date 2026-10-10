// Package costcotest serves synthetic Costco receipts and sign-ins only.
package costcotest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/chrome/chrometest"
	"github.com/kilianc/fin/internal/costco"
)

const ClientID = "a3a5186b-7c89-4b4c-93a8-dd604e930757"

// Receipt makes a warehouse sale shaped like a real one: a taxed item (Y),
// an untaxed one (N), and an instant-savings line with no tax flag whose
// title names the item it discounts.
func Receipt(barcode, day string) map[string]any {
	return map[string]any{
		"transactionBarcode": barcode, "transactionDate": day, "transactionDateTime": day + "T12:34:56",
		"warehouseNumber": "0123", "warehouseName": "Example Warehouse", "registerNumber": 7, "transactionNumber": "42", "transactionType": "Sale",
		"subTotal": 13.0, "taxes": 1.04, "total": 14.04, "instantSavings": 2.0,
		"itemArray": []any{
			map[string]any{"itemNumber": "101", "itemDescription01": "SYNTHETIC SOAP", "itemDescription02": "", "unit": 2, "itemUnitPriceAmount": 5.0, "amount": 10.0, "taxFlag": "Y", "itemDepartmentNumber": 12},
			map[string]any{"itemNumber": "202", "itemDescription01": "SYNTHETIC APPLES", "unit": 1.25, "itemUnitPriceAmount": 4.0, "amount": 5.0, "taxFlag": "N", "itemDepartmentNumber": 13},
			map[string]any{"itemNumber": "9001", "itemDescription01": "/  101", "unit": 1, "itemUnitPriceAmount": -2.0, "amount": -2.0, "taxFlag": nil, "itemDepartmentNumber": 12},
		},
		"tenderArray": []any{map[string]any{"tenderTypeName": "Visa", "tenderDescription": "Visa", "amountTender": 14.04, "displayAccountNumber": "********4242"}},
	}
}

// Order makes a costco.com order's details shaped like a real one: lines
// listed last first, an order-wide coupon, tax, and the card charge with
// the coupon beside it as a payment.
func Order(number, day string) map[string]any {
	return map[string]any{
		"orderNumber": number, "orderPlacedDate": day + "T12:40:12.51", "status": "Shipped",
		"merchandiseTotal": 45.97, "discountAmount": 4.0, "shippingAndHandling": 0.0, "retailDeliveryFee": 0.0, "grocerySurcharge": 0.0,
		"frozenSurchargeFee": 0.0, "nonMemberSurchargeAmount": 0.0, "uSTaxTotal1": 3.84, "orderTotal": 45.81,
		"shopCardAppliedAmount": 0.0, "walletShopCardAppliedAmount": 0.0,
		"orderPayment": []any{map[string]any{"paymentType": "Visa", "totalCharged": 45.81}, map[string]any{"paymentType": "Coupon", "totalCharged": 4.0}},
		"shipToAddress": []any{map[string]any{"orderLineItems": []any{
			map[string]any{"itemNumber": "1998930", "itemDescription": "SYNTHETIC TEE", "price": 11.99, "quantity": 2, "merchandiseTotalAmount": 23.98, "lineNumber": 2, "isFeeItem": false},
			map[string]any{"itemNumber": "2005928", "itemDescription": "SYNTHETIC CREWNECK", "price": 21.99, "quantity": 1, "merchandiseTotalAmount": 21.99, "lineNumber": 1, "isFeeItem": false},
		}}},
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

// Server accepts only synthetic tokens, records receipt date windows, and
// can stop one window to exercise interrupted syncs. It serves Rows as
// receipts and Orders as costco.com orders, a page of OrdersPage at a time.
// Fields are set between calls.
type Server struct {
	*httptest.Server
	mu                sync.Mutex
	Rows              []json.RawMessage
	Orders            []json.RawMessage
	OrderRequests     int
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
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		day := func(layout, key string) string {
			v, _ := request.Variables[key].(string)
			d, err := time.Parse(layout, v)
			if err != nil {
				t.Error(err)
			}
			return d.Format("2006-01-02")
		}
		placed := func(raw json.RawMessage) (string, string) {
			var o struct {
				Number string `json:"orderNumber"`
				Placed string `json:"orderPlacedDate"`
			}
			if err := json.Unmarshal(raw, &o); err != nil {
				t.Error(err)
			}
			return o.Number, o.Placed[:10]
		}
		switch {
		case strings.HasPrefix(request.Query, "query getOnlineOrders"):
			s.OrderRequests++
			start, end := day("2006-1-02", "startDate"), day("2006-1-02", "endDate")
			if request.Variables["warehouseNumber"] != "847" {
				t.Error("orders asked of the wrong warehouse")
			}
			var in []map[string]any
			for _, raw := range s.Orders {
				n, d := placed(raw)
				if d >= start && d <= end {
					in = append(in, map[string]any{"orderNumber": n, "orderPlacedDate": d})
				}
			}
			page, size := int(request.Variables["pageNumber"].(float64)), int(request.Variables["pageSize"].(float64))
			from, to := min((page-1)*size, len(in)), min(page*size, len(in))
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"getOnlineOrders": []any{map[string]any{
				"pageNumber": page, "pageSize": size, "totalNumberOfRecords": len(in), "bcOrders": in[from:to]}}}})
			return
		case strings.HasPrefix(request.Query, "query getOrderDetails"):
			s.OrderRequests++
			want := request.Variables["orderNumbers"].([]any)[0]
			for _, raw := range s.Orders {
				if n, _ := placed(raw); n == want {
					fmt.Fprintf(w, `{"data":{"getOrderDetails":%s}}`, raw)
					return
				}
			}
			fmt.Fprint(w, `{"data":{"getOrderDetails":null}}`)
			return
		}
		if request.Variables["documentType"] != "all" || request.Variables["documentSubType"] != "all" {
			t.Error("did not ask for every warehouse, gas and car wash receipt")
		}
		window := costco.Window{Start: day("1/02/2006", "startDate"), End: day("1/02/2006", "endDate")}
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
