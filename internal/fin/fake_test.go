package fin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
)

// fakePlaid serves canned responses keyed by access token. Unset methods
// fail the call so a test notices an unexpected request.
type fakePlaid struct {
	mu    sync.Mutex
	calls []string

	errs        map[string]error // token -> error for every data call
	accounts    map[string][]plaid.Account
	syncPages   map[string]func(cursor string, call int) (*plaid.TransactionsSyncResponse, error)
	syncCalls   map[string]int
	holdings    map[string]*plaid.HoldingsResponse
	holdingsErr error
	invTxs      map[string][]plaid.InvestmentTransaction
	invMaxPage  int // the fake returns at most this many per page, like a short page from Plaid

	linkCreate   *plaid.LinkTokenCreateRequest
	linkGets     []*plaid.LinkTokenGetResponse
	exchanges    map[string]*plaid.ExchangeResponse // public token -> result
	itemErrors   map[string]*plaid.Error            // token -> item.error in /item/get
	itemProducts map[string][]string                // token -> item.products in /item/get
}

func newFakePlaid() *fakePlaid {
	return &fakePlaid{
		errs:         map[string]error{},
		accounts:     map[string][]plaid.Account{},
		syncPages:    map[string]func(string, int) (*plaid.TransactionsSyncResponse, error){},
		syncCalls:    map[string]int{},
		holdings:     map[string]*plaid.HoldingsResponse{},
		invTxs:       map[string][]plaid.InvestmentTransaction{},
		exchanges:    map[string]*plaid.ExchangeResponse{},
		itemErrors:   map[string]*plaid.Error{},
		itemProducts: map[string][]string{},
	}
}

func (f *fakePlaid) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakePlaid) VerifyCredentials(ctx context.Context) error { return nil }

func (f *fakePlaid) LinkTokenCreate(ctx context.Context, req plaid.LinkTokenCreateRequest) (*plaid.LinkTokenCreateResponse, error) {
	f.record("link_token_create")
	f.linkCreate = &req
	return &plaid.LinkTokenCreateResponse{LinkToken: "link-1", HostedLinkURL: "https://hosted.plaid.com/link/abc"}, nil
}

func (f *fakePlaid) LinkTokenGet(ctx context.Context, linkToken string) (*plaid.LinkTokenGetResponse, error) {
	f.record("link_token_get")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.linkGets) == 0 {
		return &plaid.LinkTokenGetResponse{}, nil
	}
	next := f.linkGets[0]
	if len(f.linkGets) > 1 {
		f.linkGets = f.linkGets[1:]
	}
	return next, nil
}

func (f *fakePlaid) ItemPublicTokenExchange(ctx context.Context, publicToken string) (*plaid.ExchangeResponse, error) {
	f.record("exchange %s", publicToken)
	ex, ok := f.exchanges[publicToken]
	if !ok {
		return nil, &plaid.Error{Code: "INVALID_PUBLIC_TOKEN"}
	}
	return ex, nil
}

func (f *fakePlaid) ItemGet(ctx context.Context, token string) (*plaid.ItemGetResponse, error) {
	f.record("item_get %s", token)
	if err := f.errs[token]; err != nil {
		return nil, err
	}
	inst := "ins_1"
	return &plaid.ItemGetResponse{Item: plaid.Item{InstitutionID: &inst, Error: f.itemErrors[token], Products: f.itemProducts[token]}}, nil
}

func (f *fakePlaid) InstitutionGetByID(ctx context.Context, id string) (*plaid.Institution, error) {
	return &plaid.Institution{InstitutionID: id, Name: "Chase"}, nil
}

func (f *fakePlaid) InstitutionsSearch(ctx context.Context, query string, products []string) ([]plaid.InstitutionDetail, error) {
	return nil, fmt.Errorf("not used")
}

func (f *fakePlaid) AccountsGet(ctx context.Context, token string) (*plaid.AccountsResponse, error) {
	f.record("accounts_get %s", token)
	if err := f.errs[token]; err != nil {
		return nil, err
	}
	return &plaid.AccountsResponse{Accounts: f.accounts[token]}, nil
}

func (f *fakePlaid) AccountsBalanceGet(ctx context.Context, token string) (*plaid.AccountsResponse, error) {
	return f.AccountsGet(ctx, token)
}

func (f *fakePlaid) TransactionsSync(ctx context.Context, token, cursor string, count int) (*plaid.TransactionsSyncResponse, error) {
	f.record("sync %s cursor=%q", token, cursor)
	if err := f.errs[token]; err != nil {
		return nil, err
	}
	f.mu.Lock()
	call := f.syncCalls[token]
	f.syncCalls[token]++
	f.mu.Unlock()
	return f.syncPages[token](cursor, call)
}

func (f *fakePlaid) InvestmentsHoldingsGet(ctx context.Context, token string) (*plaid.HoldingsResponse, error) {
	f.record("holdings %s", token)
	if err := f.errs[token]; err != nil {
		return nil, err
	}
	if f.holdingsErr != nil {
		return nil, f.holdingsErr
	}
	return f.holdings[token], nil
}

func (f *fakePlaid) InvestmentsTransactionsGet(ctx context.Context, token, start, end string, offset, count int) (*plaid.InvestmentTransactionsResponse, error) {
	f.record("inv_txs %s offset=%d", token, offset)
	if err := f.errs[token]; err != nil {
		return nil, err
	}
	all := f.invTxs[token]
	n := min(count, f.invMaxPage)
	if n == 0 {
		n = count
	}
	stop := min(offset+n, len(all))
	return &plaid.InvestmentTransactionsResponse{
		InvestmentTransactions:      all[offset:stop],
		TotalInvestmentTransactions: len(all),
		Accounts:                    []plaid.Account{{AccountID: "acc-ira", Name: "IRA"}},
	}, nil
}

func (f *fakePlaid) SandboxPublicTokenCreate(ctx context.Context, institutionID string, products []string) (string, error) {
	return "", fmt.Errorf("not used")
}

func (f *fakePlaid) SandboxItemResetLogin(ctx context.Context, token string) error {
	return fmt.Errorf("not used")
}

// memSecrets is an in-memory Keychain.
type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Get(account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[account]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return v, nil
}

func (s *memSecrets) Set(account, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[account] = value
	return nil
}

type testApp struct {
	*App
	fake    *fakePlaid
	secrets *memSecrets
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	tty     bool
}

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// newTestApp builds a sandbox App with the given Items linked. Item i's
// access token is "tok-<item_id>".
func newTestApp(t *testing.T, items ...state.Item) *testApp {
	t.Helper()
	ta := &testApp{
		fake:    newFakePlaid(),
		secrets: &memSecrets{m: map[string]string{accountClientID: "client", secretAccount(plaid.Sandbox): "secret"}},
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
	}
	ta.App = &App{
		Env:          plaid.Sandbox,
		StatePath:    filepath.Join(t.TempDir(), "state.json"),
		Secrets:      ta.secrets,
		NewPlaid:     func(plaid.Env, string, string) Plaid { return ta.fake },
		Stdin:        &bytes.Buffer{},
		Stdout:       ta.stdout,
		Stderr:       ta.stderr,
		Now:          func() time.Time { return testNow },
		IsTerminal:   func() bool { return ta.tty },
		OpenURL:      func(string) error { return nil },
		PollInterval: time.Millisecond,
	}
	st := &state.State{Version: 1}
	for _, it := range items {
		if it.Env == "" {
			it.Env = string(plaid.Sandbox)
		}
		st.Items = append(st.Items, it)
		ta.secrets.m[tokenAccount(plaid.Env(it.Env), it.ItemID)] = "tok-" + it.ItemID
	}
	if err := st.Save(ta.StatePath); err != nil {
		t.Fatal(err)
	}
	return ta
}

func (ta *testApp) run(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	ta.stdout.Reset()
	ta.stderr.Reset()
	code := ta.Run(context.Background(), args)
	var body map[string]any
	if ta.stdout.Len() > 0 {
		if err := json.Unmarshal(ta.stdout.Bytes(), &body); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, ta.stdout)
		}
	}
	return code, body
}

func (ta *testApp) stderrJSON(t *testing.T) map[string]any {
	t.Helper()
	// Progress lines such as the Hosted Link URL may precede the error object.
	raw := ta.stderr.String()
	if i := strings.LastIndex(raw, "{\n  \"error\""); i >= 0 {
		raw = raw[i:]
	}
	var out struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, ta.stderr)
	}
	return out.Error
}

var (
	chase = state.Item{Name: "chase", ItemID: "item-chase", Kind: "bank", Products: []string{"transactions"}, InstitutionName: "Chase"}
	citi  = state.Item{Name: "citi", ItemID: "item-citi", Kind: "bank", Products: []string{"transactions"}, InstitutionName: "Citi"}
	fido  = state.Item{Name: "fidelity", ItemID: "item-fido", Kind: "brokerage", Products: []string{"investments"}, InstitutionName: "Fidelity"}
)

func ptr[T any](v T) *T { return &v }

func tx(id, account, date string, amount float64) plaid.Transaction {
	return plaid.Transaction{TransactionID: id, AccountID: account, Date: date, Amount: amount, Name: "tx " + id}
}
