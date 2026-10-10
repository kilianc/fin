package fin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/chrome"
	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/costco/costcotest"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/sheets/sheetstest"
	"github.com/kilianc/fin/internal/state"
)

func costcoApp(t *testing.T) (*testApp, *costcotest.Server) {
	t.Helper()
	ta := newTestApp(t, chase)
	ta.Now = func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
	ta.Chrome = chrome.Chrome{Dir: costcotest.ChromeDir(t), SafeStorage: func() (string, error) { t.Error("plaintext cache asked for Keychain access"); return "peanuts", nil }}
	srv := costcotest.New(t)
	srv.Rows = []json.RawMessage{costcotest.Raw(t, costcotest.Receipt("synthetic-recent", "2026-09-02")), costcotest.Raw(t, costcotest.Receipt("synthetic-old", "2026-02-03"))}
	ta.CostcoTokenURL, ta.CostcoOrdersURL = srv.URL+"/token", srv.URL+"/receipts"
	ta.fake.syncPages["tok-item-chase"] = func(string, int) (*plaid.TransactionsSyncResponse, error) {
		charge := tx("costco-charge", "visa", "2026-09-03", 14.04)
		charge.Name = "COSTCO WHSE #0123"
		return &plaid.TransactionsSyncResponse{Added: []plaid.Transaction{charge}, Accounts: []plaid.Account{{AccountID: "visa", Name: "Visa", Mask: ptr("4242"), Type: "credit"}}, NextCursor: "c1", TransactionsUpdateStatus: "HISTORICAL_UPDATE_COMPLETE"}, nil
	}
	if code, _ := ta.run(t, "epoch", "2026-01-01", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	return ta, srv
}

func TestCostcoLoginSyncCategorizeSheetAndLogout(t *testing.T) {
	ta, srv := costcoApp(t)
	if code, _ := ta.run(t, "costco", "login", "home", "--profile", "Default", "--no-sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	if srv.Refreshes() != 1 || len(srv.Windows()) != 0 {
		t.Error("login did more than one verification")
	}
	sealed, err := os.ReadFile(filepath.Join(ta.DataDir, "costco", "sandbox-home.session"))
	if err != nil || bytes.Contains(sealed, []byte("synthetic")) {
		t.Fatal("session is missing or plaintext")
	}
	code, body := ta.run(t, "sync", "--json")
	if code != exitOK || len(body["costco"].([]any)) != 1 {
		t.Fatalf("sync=%v stderr=%s", body, ta.stderr)
	}
	code, body = ta.run(t, "sql", "--json", `select barcode, match from costco_matches order by barcode`)
	if code != exitOK || body["rows"].([]any)[1].(map[string]any)["match"] != "exact" {
		t.Fatalf("matches=%v", body)
	}
	code, body = ta.run(t, "costco", "categorize", "--json")
	if code != exitOK || len(body["items"].([]any)) != 6 {
		t.Fatalf("categorize=%v", body)
	}
	if code, _ := ta.run(t, "costco", "categorize", "--set", "home/synthetic-recent#1", "PERSONAL_CARE", "--product", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	if code, _ := ta.run(t, "costco", "sync", "home", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	code, body = ta.run(t, "costco", "list", "--json")
	acct := body["accounts"].([]any)[0].(map[string]any)
	if code != exitOK || acct["items"] != 6.0 || acct["uncategorized_items"] != 2.0 || acct["status"] != "ok" {
		t.Fatalf("account=%v", acct)
	}
	f := sheetstest.New(t)
	ta.Google, ta.SheetsBase = f.Client(), f.URL
	ta.secrets.m[googleTokenAccount] = f.Refresh
	code, body = ta.run(t, "sheet", "--json")
	if code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	sh := f.Sheets[body["spreadsheet_id"].(string)]
	if len(sh.Values["Costco items"]) != 7 {
		t.Errorf("sheet rows=%v", sh.Values["Costco items"])
	}
	if code, _ := ta.run(t, "costco", "logout", "home", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(ta.DataDir, "costco", "sandbox-home.session")); !os.IsNotExist(err) {
		t.Error("logout kept session")
	}
	if _, err := ta.secrets.Get("costco.sandbox.home.key"); err == nil {
		t.Error("logout kept key")
	}
	_, body = ta.run(t, "sql", "--json", `select (select count(*) from costco_items) + (select count(*) from costco_receipts) + (select count(*) from item_categories where retailer='costco') as n`)
	if body["rows"].([]any)[0].(map[string]any)["n"] != 0.0 {
		t.Errorf("logout kept rows: %v", body)
	}
}

func TestCostcoInterruptedSyncResumesAndRecentRefreshPreservesHistory(t *testing.T) {
	ta, srv := costcoApp(t)
	if code, _ := ta.run(t, "costco", "login", "home", "--no-sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	srv.FailureAt = 2
	code, body := ta.run(t, "costco", "sync", "--json")
	if code != exitPartial || body["errors"].([]any)[0].(map[string]any)["code"] != "COSTCO_RATE_LIMITED" {
		t.Fatalf("limited: %v stderr=%s", body, ta.stderr)
	}
	s, err := ta.openStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.RetailerAccountState(context.Background(), "costco", "home")
	s.Close()
	if err != nil || saved.CompleteSince != "2026-07-13" {
		t.Fatalf("coverage=%+v err=%v", saved, err)
	}
	st, _ := state.Load(ta.StatePath)
	acct, _ := st.FindRetailer("costco", "sandbox", "home")
	var sess costco.Session
	if err := ta.loadSession(acct, &sess); err != nil || sess.RefreshToken != "synthetic-rotated-1" {
		t.Fatalf("rotation was lost: %v", err)
	}
	before := len(srv.Windows())
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitPartial || len(srv.Windows()) != before {
		t.Error("cooldown sent a request")
	}
	if code, _ := ta.run(t, "costco", "login", "home", "--no-sync", "--json"); code != exitError || srv.Refreshes() != 1 {
		t.Error("login bypassed cooldown")
	}
	ta.Now = func() time.Time { return time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC) }
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	windows := srv.Windows()
	if windows[before] != (costco.Window{Start: "2026-04-14", End: "2026-07-12"}) {
		t.Fatalf("resume reread saved windows: %v", windows)
	}
	s, err = ta.openStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved, _ = s.RetailerAccountState(context.Background(), "costco", "home")
	s.Close()
	if saved.CompleteSince != "2025-12-25" {
		t.Errorf("completed coverage=%s", saved.CompleteSince)
	}
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	windows = srv.Windows()
	if windows[len(windows)-1] != (costco.Window{Start: "2026-08-12", End: "2026-10-10"}) {
		t.Fatalf("recent window=%v", windows[len(windows)-1])
	}
	s, err = ta.openStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved, _ = s.RetailerAccountState(context.Background(), "costco", "home")
	s.Close()
	if saved.CompleteSince != "2025-12-25" {
		t.Error("recent sync moved coverage forward")
	}
	if log, err := os.ReadFile(filepath.Join(ta.DataDir, "costco", "sandbox-home.log")); err != nil || !bytes.Contains(log, []byte(" receipts 429\n")) || bytes.Contains(log, []byte("synthetic")) {
		t.Error("request log missing or contains session/receipt content")
	}
}

func TestCostcoLoginDefaultsToSyncAndExpiredSessionIsActionable(t *testing.T) {
	ta, srv := costcoApp(t)
	if code, _ := ta.run(t, "costco", "login", "home", "--json"); code != exitOK || len(srv.Windows()) != 4 {
		t.Fatalf("login exit=%d windows=%v stderr=%s", code, srv.Windows(), ta.stderr)
	}
	st, _ := state.Load(ta.StatePath)
	acct, _ := st.FindRetailer("costco", "sandbox", "home")
	var sess costco.Session
	if err := ta.loadSession(acct, &sess); err != nil {
		t.Fatal(err)
	}
	sess.IDExpires = time.Time{}
	if err := ta.saveSession(acct, &sess); err != nil {
		t.Fatal(err)
	}
	srv.RefreshStatus = 400
	code, body := ta.run(t, "sync", "--json")
	if code != exitPartial {
		t.Fatalf("expired sync=%d %v", code, body)
	}
	e := body["errors"].([]any)[0].(map[string]any)
	if e["code"] != "COSTCO_SIGNIN_EXPIRED" || e["action"] != "run fin costco login home" {
		t.Errorf("error=%v", e)
	}
	if strings.Contains(ta.stdout.String()+ta.stderr.String(), "must-not-leak") {
		t.Error("token endpoint body leaked")
	}
}

func TestCostcoEmptyWindowsAdvanceAndBadArgumentsMakeNoRequests(t *testing.T) {
	ta, srv := costcoApp(t)
	srv.Rows = nil
	for _, args := range [][]string{{"costco", "login", "bad name"}, {"costco", "sync", "one", "two"}, {"costco", "login", "home", "--profile", "missing"}, {"costco", "surprise"}} {
		if code, _ := ta.run(t, append(args, "--json")...); code == exitOK {
			t.Errorf("accepted %v", args)
		}
	}
	if srv.Refreshes() != 0 || len(srv.Windows()) != 0 {
		t.Error("invalid command sent requests")
	}
	if code, _ := ta.run(t, "costco", "login", "home", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	s, err := ta.openStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	saved, err := s.RetailerAccountState(context.Background(), "costco", "home")
	if err != nil || saved.CompleteSince != "2025-12-25" {
		t.Fatalf("empty coverage=%+v err=%v", saved, err)
	}
}

func TestCostcoResumesAcrossDaysAndCatchesLongAbsence(t *testing.T) {
	ta, srv := costcoApp(t)
	if code, _ := ta.run(t, "costco", "login", "home", "--no-sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	srv.FailureAt = 2
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitPartial {
		t.Fatal("expected partial sync")
	}
	ta.Now = func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }
	before := len(srv.Windows())
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	if windows := srv.Windows(); windows[before] != (costco.Window{Start: "2026-10-11", End: "2026-10-12"}) || windows[before+1].End != "2026-07-12" {
		t.Fatalf("cross-day resume=%v", windows)
	}
	// Returning after more than 60 days must not silently skip the gap.
	ta.Now = func() time.Time { return time.Date(2027, 5, 1, 12, 0, 0, 0, time.UTC) }
	before = len(srv.Windows())
	srv.FailureAt = before + 2
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitPartial {
		t.Fatal("expected long-gap interruption")
	}
	ta.Now = func() time.Time { return time.Date(2027, 5, 1, 15, 0, 0, 0, time.UTC) }
	before = len(srv.Windows())
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	windows := srv.Windows()
	if windows[before].End != "2027-01-31" || windows[len(windows)-1].Start != "2026-10-12" {
		t.Fatalf("gap was skipped: %v", windows)
	}
}

func TestCostcoEarlierEpochExtendsPendingHistory(t *testing.T) {
	ta, srv := costcoApp(t)
	if code, _ := ta.run(t, "costco", "login", "home", "--no-sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	srv.FailureAt = 2
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitPartial {
		t.Fatal("expected partial sync")
	}
	if code, _ := ta.run(t, "epoch", "2025-10-01", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	ta.Now = func() time.Time { return time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC) }
	if code, _ := ta.run(t, "costco", "sync", "--json"); code != exitOK {
		t.Fatal(ta.stderr.String())
	}
	windows := srv.Windows()
	if windows[len(windows)-1].Start != "2025-09-24" {
		t.Fatalf("earlier epoch not read: %v", windows)
	}
}
