package fin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/amazon/amazontest"
	"github.com/kilianc/fin/internal/plaid"
)

const multiOrder = "111-0000001-0000001"

// amazonApp is a test app with one bank Item holding an Amazon charge and a
// coffee, a Chrome profile signed in to a fake amazon.com, and that charge's
// order.
func amazonApp(t *testing.T) (*testApp, *amazontest.Server) {
	t.Helper()
	ta := newTestApp(t, chase)
	charge := tx("amz", "card", "2026-09-03", 51.99)
	charge.Name, charge.MerchantName = "AMAZON MARKETPLACE NAMZN.COM/BILL", ptr("Amazon")
	charge.PersonalFinanceCategory = &plaid.PersonalFinanceCategory{Primary: "GENERAL_MERCHANDISE", Detailed: "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES"}
	coffee := tx("coffee", "card", "2026-09-04", 4.25)
	coffee.PersonalFinanceCategory = &plaid.PersonalFinanceCategory{Primary: "FOOD_AND_DRINK", Detailed: "FOOD_AND_DRINK_COFFEE"}
	ta.fake.syncPages["tok-item-chase"] = func(string, int) (*plaid.TransactionsSyncResponse, error) {
		return &plaid.TransactionsSyncResponse{
			Added: []plaid.Transaction{charge, coffee}, NextCursor: "c1", TransactionsUpdateStatus: "HISTORICAL_UPDATE_COMPLETE",
			Accounts: []plaid.Account{{AccountID: "card", Name: "Platinum", Mask: ptr("1002"), Type: "credit"}},
		}, nil
	}

	srv := amazontest.New(t)
	page, err := os.ReadFile("../amazon/testdata/order-multi.html")
	if err != nil {
		t.Fatal(err)
	}
	srv.Orders[multiOrder] = page
	srv.Rows = append(srv.Rows,
		amazontest.Payment("-$51.99", "Sep 2, 2026", multiOrder, "Platinum Card®", "AMZN Mktp US", "Charged"),
		amazontest.Payment("-$10.00", "Sep 2, 2026", multiOrder, "Amazon Gift Card", "", "Charged"),
		amazontest.Payment("-$9.99", "Aug 2, 2026", "D01-0000003-0000003", "Visa", "Kindle", "Charged"),
	)
	ta.AmazonBase = srv.URL
	ta.Chrome = amazon.Chrome{
		Dir:         amazontest.ChromeDir(t, "peanuts", map[string]string{"Default": "Pat", "Profile 1": "Sam"}, map[string]string{"Default": "good"}),
		SafeStorage: func() (string, error) { return "peanuts", nil },
	}
	return ta, srv
}

func TestAmazonLoginSyncCategorizeAndSpending(t *testing.T) {
	ta, srv := amazonApp(t)

	code, body := ta.run(t, "amazon", "login", "pat", "--json")
	if code != exitOK {
		t.Fatalf("login exit %d: %s", code, ta.stderr)
	}
	if sync := body["sync"].(map[string]any); sync["new_payments"] != 3.0 || sync["orders_read"] != 1.0 {
		t.Errorf("login sync = %v", sync)
	}
	if !strings.Contains(ta.stderr.String(), "Chrome Safe Storage") {
		t.Errorf("login did not warn about the Keychain prompt: %s", ta.stderr)
	}
	sealed, err := os.ReadFile(filepath.Join(ta.DataDir, "amazon", "sandbox-pat.session"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("good")) || bytes.Contains(sealed, []byte("session-token")) {
		t.Error("the session file holds the cookie in plain text")
	}
	if _, err := ta.secrets.Get("amazon.sandbox.pat.key"); err != nil {
		t.Errorf("session key not in the Keychain: %v", err)
	}

	// fin sync pulls the bank transactions and checks Amazon for new payments.
	code, body = ta.run(t, "sync", "--json")
	if code != exitOK {
		t.Fatalf("sync exit %d: %s", code, ta.stderr)
	}
	if views := body["amazon"].([]any); len(views) != 1 || views[0].(map[string]any)["new_payments"] != 0.0 {
		t.Errorf("sync amazon = %v", body["amazon"])
	}

	code, body = ta.run(t, "sql", "--json", "select match, count(*) as n from amazon_matches group by 1 order by 1")
	if code != exitOK {
		t.Fatalf("sql exit %d: %s", code, ta.stderr)
	}
	matches := map[string]float64{}
	for _, r := range body["rows"].([]any) {
		row := r.(map[string]any)
		matches[row["match"].(string)] = row["n"].(float64)
	}
	if matches["exact"] != 1 || matches["no_bank_charge"] != 1 || matches["unmatched"] != 1 {
		t.Errorf("matches = %v", matches)
	}

	code, body = ta.run(t, "amazon", "categorize", "--json")
	if items := body["items"].([]any); code != exitOK || len(items) != 3 {
		t.Fatalf("uncategorized = %v (exit %d)", body, code)
	}
	if code, _ := ta.run(t, "amazon", "categorize", "--set", multiOrder+"#1", "home_improvement", "--product", "--json"); code != exitOK {
		t.Fatalf("set exit %d: %s", code, ta.stderr)
	}
	ta.Stdin = strings.NewReader(`[{"item": "` + multiOrder + `#2", "category": "GENERAL_MERCHANDISE", "detailed": "GENERAL_MERCHANDISE_ELECTRONICS"}]`)
	if code, _ := ta.run(t, "amazon", "categorize", "--set", "--json"); code != exitOK {
		t.Fatalf("set from stdin exit %d: %s", code, ta.stderr)
	}
	if code, _ := ta.run(t, "amazon", "categorize", "--set", multiOrder+"#3", "SHOPPING", "--json"); code != exitUsage {
		t.Errorf("an unknown category was accepted (exit %d)", code)
	}

	_, body = ta.run(t, "sql", "--json", "select category, sum(amount)::double as total from spending group by 1 order by 1")
	totals := map[string]float64{}
	for _, r := range body["rows"].([]any) {
		row := r.(map[string]any)
		totals[row["category"].(string)] = row["total"].(float64)
	}
	// 51.99 splits 34.66 (knife), 13.43 (cable) and 3.90 (notebook, still the bank's category).
	want := map[string]float64{"HOME_IMPROVEMENT": 34.66, "GENERAL_MERCHANDISE": 13.43 + 3.90, "FOOD_AND_DRINK": 4.25}
	for k, v := range want {
		if d := totals[k] - v; d > 0.001 || d < -0.001 {
			t.Errorf("spending %s = %v, want %v (all: %v)", k, totals[k], v, totals)
		}
	}

	code, body = ta.run(t, "amazon", "--json")
	acct := body["accounts"].([]any)[0].(map[string]any)
	if code != exitOK || acct["items"] != 3.0 || acct["uncategorized_items"] != 1.0 || acct["matched_payments"] != 1.0 || acct["profile"] != "Pat" {
		t.Errorf("amazon list = %v", acct)
	}

	// Amazon signs the session out: the bank still syncs, Amazon says what to run.
	srv.SignedIn = false
	code, body = ta.run(t, "sync", "--json")
	if code != exitPartial {
		t.Fatalf("sync after sign-out: exit %d, want %d", code, exitPartial)
	}
	errs := body["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["code"] != "AMAZON_SIGNIN_EXPIRED" || errs[0].(map[string]any)["action"] != "run fin amazon login pat" {
		t.Errorf("errors = %v", errs)
	}

	if code, _ := ta.run(t, "amazon", "logout", "pat", "--json"); code != exitOK {
		t.Fatalf("logout exit %d: %s", code, ta.stderr)
	}
	if _, err := os.Stat(filepath.Join(ta.DataDir, "amazon", "sandbox-pat.session")); !os.IsNotExist(err) {
		t.Error("session file left after logout")
	}
	if _, err := ta.secrets.Get("amazon.sandbox.pat.key"); err == nil {
		t.Error("session key left in the Keychain after logout")
	}
	_, body = ta.run(t, "sql", "--json", "select count(*) as n from amazon_items")
	if n := body["rows"].([]any)[0].(map[string]any)["n"]; n != 0.0 {
		t.Errorf("items left after logout = %v", n)
	}
}

func TestAmazonLoginNeedsAProfileSignedIn(t *testing.T) {
	ta, _ := amazonApp(t)
	ta.Chrome.Dir = amazontest.ChromeDir(t, "peanuts", map[string]string{"Default": "Pat", "Profile 1": "Sam"},
		map[string]string{"Default": "good", "Profile 1": "good"})
	code, _ := ta.run(t, "amazon", "login", "pat", "--json")
	if e := ta.stderrJSON(t); code != exitUsage || e["code"] != "PROFILE_REQUIRED" {
		t.Errorf("two signed-in profiles without --profile: exit %d, %v", code, e)
	}
	code, _ = ta.run(t, "amazon", "login", "pat", "--profile", "Sam", "--no-sync", "--json")
	if code != exitOK {
		t.Fatalf("--profile Sam: exit %d: %s", code, ta.stderr)
	}

	ta.Chrome.Dir = amazontest.ChromeDir(t, "peanuts", map[string]string{"Default": "Pat"}, nil)
	code, _ = ta.run(t, "amazon", "login", "home", "--json")
	if e := ta.stderrJSON(t); code != exitError || e["code"] != "AMAZON_NOT_SIGNED_IN" {
		t.Errorf("no profile signed in: exit %d, %v", code, e)
	}
	if code, _ := ta.run(t, "amazon", "login", "Not A Name", "--json"); code != exitUsage {
		t.Errorf("bad account name: exit %d", code)
	}
}

func TestAmazonRateLimitKeepsWhatWasRead(t *testing.T) {
	ta, srv := amazonApp(t)
	saved := amazon.Backoff
	amazon.Backoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { amazon.Backoff = saved })
	if code, _ := ta.run(t, "amazon", "login", "pat", "--no-sync", "--json"); code != exitOK {
		t.Fatalf("login exit %d: %s", code, ta.stderr)
	}
	srv.PagesBeforeLimit = 1
	code, body := ta.run(t, "amazon", "sync", "--json")
	if code != exitPartial {
		t.Fatalf("exit %d, want %d: %s", code, exitPartial, ta.stderr)
	}
	if e := body["errors"].([]any)[0].(map[string]any); e["code"] != "AMAZON_RATE_LIMITED" {
		t.Errorf("error = %v", e)
	}
	_, body = ta.run(t, "sql", "--json", "select count(*) as n from amazon_payments")
	if n := body["rows"].([]any)[0].(map[string]any)["n"]; n != 2.0 {
		t.Errorf("payments kept from the first page = %v, want 2", n)
	}
	srv.PagesBeforeLimit = 0
	if code, _ := ta.run(t, "amazon", "sync", "--json"); code != exitOK {
		t.Fatalf("second sync exit %d: %s", code, ta.stderr)
	}
	_, body = ta.run(t, "sql", "--json", "select count(*) as n from amazon_payments")
	if n := body["rows"].([]any)[0].(map[string]any)["n"]; n != 3.0 {
		t.Errorf("payments after finishing = %v, want 3", n)
	}
}

func TestAmazonReadsBackOnlyToTheEpoch(t *testing.T) {
	ta, srv := amazonApp(t)
	srv.Rows = append(srv.Rows, amazontest.Payment("-$5.00", "Jan 2, 2020", "114-0000005-0000005", "Visa", "AMZN Mktp US", "Charged"))
	if code, body := ta.run(t, "epoch", "2026-09-08", "--json"); code != exitOK || body["epoch"] != "2026-09-08" {
		t.Fatalf("fin epoch: exit %d, %v", code, body)
	}
	code, body := ta.run(t, "amazon", "login", "pat", "--json")
	if code != exitOK {
		t.Fatalf("login exit %d: %s", code, ta.stderr)
	}
	// The epoch is in the database for queries, and Amazon is read from a week before it.
	_, q := ta.run(t, "sql", "--json", "select value from settings where key = 'epoch'")
	if v := q["rows"].([]any)[0].(map[string]any)["value"]; v != "2026-09-08" {
		t.Errorf("settings epoch = %v", v)
	}
	if sync := body["sync"].(map[string]any); sync["since"] != "2026-09-01" || sync["new_payments"] != 2.0 {
		t.Errorf("sync = %v; payments before the epoch were read", sync)
	}
	if n := srv.Count("/payments-portal/data/iris/live/v1/data/manage/get-transactions"); n != 2 {
		t.Errorf("payments pages read = %d, want 2: it should stop at the first page past the epoch", n)
	}

	if code, body := ta.run(t, "epoch", "none", "--json"); code != exitOK || body["epoch"] != nil {
		t.Errorf("clearing the epoch: exit %d, %v", code, body)
	}

	// Without an epoch, Amazon is read back to a week before the bank history.
	ta2, _ := amazonApp(t)
	if code, _ := ta2.run(t, "sync", "--json"); code != exitOK {
		t.Fatalf("bank sync exit %d: %s", code, ta2.stderr)
	}
	_, body = ta2.run(t, "amazon", "login", "pat", "--json")
	if since := body["sync"].(map[string]any)["since"]; since != "2026-08-27" {
		t.Errorf("default epoch = %v, want a week before the oldest bank transaction (2026-09-03)", since)
	}
}
