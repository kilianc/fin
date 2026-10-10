package fin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

var costcoRetailer = retailer{ID: "costco", Name: "Costco", SignIn: costco.ErrSignIn}

func init() {
	costcoRetailer.Sync = func(a *App, ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, p syncProgress) (any, error) {
		return a.syncCostco(ctx, s, st, acct, p)
	}
	costcoRetailer.Forget = func(ctx context.Context, s *store.Store, account string) error {
		return s.DeleteCostcoAccount(ctx, account)
	}
	retailers = append(retailers, costcoRetailer)
}

func (a *App) cmdCostco(ctx context.Context, args []string) (*result, error) {
	return a.cmdRetailer(ctx, costcoRetailer, args, a.cmdCostcoList, a.cmdCostcoLogin, a.cmdCostcoSync, nil)
}

func (a *App) costcoClient(acct state.RetailerAccount, sess *costco.Session) *costco.Client {
	client := costco.NewClient(sess, a.CostcoTokenURL, cmp.Or(a.CostcoOrdersURL, costco.OrdersURL))
	client.Gate = a.retailerGate(costcoRetailer, a.CostcoPause)
	client.Save = func(s *costco.Session) error { return a.saveSession(acct, s) }
	return client
}

// --- sync ---

type costcoSyncView struct {
	Account  string     `json:"account"`
	Since    string     `json:"since"`
	Receipts int        `json:"receipts_read"`
	Items    int        `json:"items_read"`
	LastSync *time.Time `json:"last_sync"`
}

func (v *costcoSyncView) summary() (string, int, string) {
	return v.Account, v.Receipts, fmt.Sprintf("%d receipts, %d items", v.Receipts, v.Items)
}

// syncCostco saves each date window before advancing coverage. An initial
// read cut short resumes just before CompleteSince. Once history is read,
// subsequent syncs reread the last 60 days for returns and late receipts.
func (a *App) syncCostco(ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, progress syncProgress) (*costcoSyncView, error) {
	if err := a.beginSync(ctx, s, acct); err != nil {
		return nil, err
	}
	var sess costco.Session
	if err := a.loadSession(acct, &sess); err != nil {
		return nil, err
	}
	client := a.costcoClient(acct, &sess)
	log, closeLog := a.requestLog(acct)
	defer closeLog()
	client.Log = log
	epoch, err := a.readSince(ctx, s, st)
	if err != nil {
		return nil, err
	}
	saved, err := s.RetailerAccountState(ctx, acct.Retailer, acct.Name)
	if err != nil {
		return nil, storeErr(err)
	}
	now := a.Now()
	windows, err := costcoWindows(epoch, now, saved)
	if err != nil {
		return nil, a.syncErr(ctx, s, acct, err)
	}
	since := epoch
	if len(windows) > 0 {
		since = windows[len(windows)-1].Start
	}
	view := &costcoSyncView{Account: acct.Name, Since: since}
	start := time.Now()
	complete := saved.CompleteSince
	for i, window := range windows {
		progress.step(0, countProgress(i, len(windows), time.Since(start))+" · "+longDate(window.Start))
		receipts, err := client.Receipts(ctx, window)
		if err != nil {
			return nil, a.syncErr(ctx, s, acct, err)
		}
		slices.SortFunc(receipts, func(a, b costco.Receipt) int {
			return cmp.Or(cmp.Compare(b.Date, a.Date), cmp.Compare(b.Barcode, a.Barcode))
		})
		if err := s.ApplyCostcoReceipts(ctx, acct.Name, acct.Profile, receipts, now.UTC()); err != nil {
			return nil, storeErr(err)
		}
		// A recent refresh must not move the history boundary forward.
		if complete == "" || window.Start < complete {
			complete = window.Start
		}
		if err := s.SetCompleteSince(ctx, acct.Retailer, acct.Name, complete); err != nil {
			return nil, storeErr(err)
		}
		// Keep the remaining windows too: a sync after a long absence can
		// have gaps newer than CompleteSince. LastSync changes only after
		// all windows finish, so even a lost checkpoint safely rereads them.
		resume := ""
		if i+1 < len(windows) {
			b, err := json.Marshal(costcoResume{Through: now.Format("2006-01-02"), Windows: windows[i+1:]})
			if err != nil {
				return nil, err
			}
			resume = string(b)
		}
		if err := s.SetResumeKey(ctx, acct.Retailer, acct.Name, resume); err != nil {
			return nil, storeErr(err)
		}
		view.Receipts += len(receipts)
		for _, r := range receipts {
			view.Items += len(r.Items)
			for _, it := range r.Items {
				progress.feed(feedLine(r.Date, it.Title, it.Cost.USD()))
			}
		}
		progress.step(0, countProgress(i+1, len(windows), time.Since(start)))
	}
	if _, err := s.ReparseCostcoReceipts(ctx, acct.Name); err != nil {
		return nil, a.syncErr(ctx, s, acct, err)
	}
	if err := s.SetRetailerSynced(ctx, acct.Retailer, acct.Name, acct.Profile, now.UTC()); err != nil {
		return nil, storeErr(err)
	}
	if err := a.endSync(ctx, s, st, acct, now.UTC()); err != nil {
		return nil, err
	}
	last := now.UTC()
	view.LastSync = &last
	return view, nil
}

// costcoResume keeps pending windows and the day the read started. Coverage
// alone cannot describe a recent refresh interrupted after months offline.
type costcoResume struct {
	Through string          `json:"through"`
	Windows []costco.Window `json:"windows"`
}

func costcoWindows(epoch string, now time.Time, saved store.RetailerAccount) ([]costco.Window, error) {
	today := now.Format("2006-01-02")
	if saved.ResumeKey != "" {
		var resume costcoResume
		if err := json.Unmarshal([]byte(saved.ResumeKey), &resume); err != nil {
			return nil, fmt.Errorf("costco: invalid saved window checkpoint: %w", err)
		}
		through, err := time.Parse("2006-01-02", resume.Through)
		if err != nil {
			return nil, err
		}
		// Read newly arrived days first, then exactly where the last read
		// stopped, extending the tail if the epoch moved earlier.
		windows, err := costco.Windows(max(epoch, through.AddDate(0, 0, 1).Format("2006-01-02")), today)
		if err != nil {
			return nil, err
		}
		oldest := saved.CompleteSince
		for _, w := range resume.Windows {
			if oldest == "" || w.Start < oldest {
				oldest = w.Start
			}
			if w.End < epoch {
				continue
			}
			w.Start = max(w.Start, epoch)
			w.End = min(w.End, today)
			if w.Start <= w.End {
				windows = append(windows, w)
			}
		}
		if oldest > epoch {
			day, err := time.Parse("2006-01-02", oldest)
			if err != nil {
				return nil, err
			}
			older, err := costco.Windows(epoch, day.AddDate(0, 0, -1).Format("2006-01-02"))
			if err != nil {
				return nil, err
			}
			windows = append(windows, older...)
		}
		return windows, nil
	}
	since := epoch
	if saved.CompleteSince != "" && saved.LastSync != nil {
		// Include every day since the last successful sync, even if fin
		// has been closed longer than the usual 60-day return lookback.
		since = max(epoch, min(now.AddDate(0, 0, -59).Format("2006-01-02"), saved.LastSync.In(now.Location()).Format("2006-01-02")))
	}
	windows, err := costco.Windows(since, today)
	if err != nil {
		return nil, err
	}
	if saved.CompleteSince > epoch && saved.LastSync != nil && saved.CompleteSince < since {
		day, err := time.Parse("2006-01-02", saved.CompleteSince)
		if err != nil {
			return nil, err
		}
		older, err := costco.Windows(epoch, day.AddDate(0, 0, -1).Format("2006-01-02"))
		if err != nil {
			return nil, err
		}
		windows = append(windows, older...)
	} else if saved.CompleteSince > epoch && saved.LastSync != nil {
		// The epoch moved earlier and the recent read already reaches
		// the stored history: one continuous read is enough.
		return costco.Windows(epoch, today)
	}
	return windows, nil
}

func (a *App) cmdCostcoSync(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("costco sync", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 {
		return nil, usageErr("usage: fin costco sync [name]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	name := ""
	if len(pos) == 1 {
		name = pos[0]
	}
	accts, err := a.connectedAccounts(st, costcoRetailer, name)
	if err != nil {
		return nil, err
	}
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	views, errs, err := a.runSync(ctx, st, costcoRetailer, accts, []string{"receipts"},
		func(acct state.RetailerAccount, p syncProgress) (any, error) {
			return a.syncCostco(ctx, s, st, acct, p)
		},
		func(v any) []string { _, _, detail := v.(*costcoSyncView).summary(); return []string{detail} })
	if err != nil {
		return nil, err
	}
	t := &ui.Table{Title: fmt.Sprintf("Costco · %d synced", len(views)), Headers: []string{"Account", "Receipts read", "Items read"}, Right: []int{1, 2},
		Footer: "Stored in " + a.storePath() + ". Categorize new items with fin costco categorize."}
	for _, v := range views {
		v := v.(*costcoSyncView)
		t.Rows = append(t.Rows, []string{v.Account, strconv.Itoa(v.Receipts), strconv.Itoa(v.Items)})
	}
	return &result{body: map[string]any{"env": a.Env, "store": a.storePath(), "costco": views, "errors": errs}, table: t, errors: errs}, nil
}

// --- login ---

func (a *App) cmdCostcoLogin(ctx context.Context, args []string) (*result, error) {
	st, acct, profile, noSync, err := a.loginAccount(costcoRetailer, args, func() ([]chromeProfile, error) { return a.chromeProfiles(costcoRetailer, costco.SignedIn) })
	if err != nil {
		return nil, err
	}
	// Logging in again must not bypass a retailer's request cooldown.
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := a.beginSync(ctx, s, acct); err != nil {
		return nil, err
	}
	steps := []string{"Read your Costco sign-in from Chrome", "Check it with Costco"}
	err = a.runLogin(ctx, costcoRetailer, "Experimental. fin reads your warehouse receipts using your costco.com sign-in. Costco's website endpoints can change or stop working at any time.", steps,
		func(ctx context.Context, r ui.Reporter) (ui.FlowResult, error) {
			r.Start(0, "macOS may ask to allow Chrome Safe Storage")
			sess, err := costco.ChromeSession(a.Chrome, profile.Dir)
			if errors.Is(err, costco.ErrSignIn) {
				return ui.FlowResult{}, newErr("COSTCO_NOT_SIGNED_IN", "sign in at costco.com in Chrome profile %q, then run this again", profile.Name)
			}
			if err != nil {
				return ui.FlowResult{}, newErr("CHROME_ERROR", "%v", err)
			}
			r.Done(0, profile.Name)
			r.Start(1, "")
			client := a.costcoClient(acct, sess)
			if err := client.Check(ctx); err != nil {
				return ui.FlowResult{}, a.syncErr(ctx, s, acct, err)
			}
			st.PutRetailer(acct)
			if err := a.saveState(st); err != nil {
				return ui.FlowResult{}, err
			}
			if err := s.SetRetailerStatus(ctx, acct.Retailer, acct.Name, acct.Profile, "ok"); err != nil {
				return ui.FlowResult{}, storeErr(err)
			}
			r.Done(1, "signed in")
			return ui.FlowResult{Message: "Connected. Run fin costco sync to read warehouse receipts."}, nil
		})
	if err != nil {
		return nil, err
	}
	if !noSync {
		s.Close()
		return a.cmdCostcoSync(ctx, []string{acct.Name})
	}
	return &result{body: map[string]any{"env": a.Env, "account": acct.Name, "profile": profile},
		message: ui.Line(ui.Good, fmt.Sprintf("Connected Costco account %s from Chrome profile %s", acct.Name, profile.Name))}, nil
}

// --- list ---

type costcoAccountView struct {
	store.CostcoSummary
	Profile     string    `json:"profile"`
	ConnectedAt time.Time `json:"connected_at"`
}

func (a *App) cmdCostcoList(ctx context.Context, args []string) (*result, error) {
	return listRetailer(a, ctx, costcoRetailer, args, (*store.Store).CostcoSummaries,
		[]string{"Account", "Chrome profile", "Receipts", "Items", "To categorize", "Matched", "Last sync", "Status"}, []int{2, 3, 4, 5},
		func(acct state.RetailerAccount, sum store.CostcoSummary) (any, []string) {
			sum.Account = acct.Name
			if sum.LastSync == nil {
				sum.LastSync = acct.LastSync
			}
			view := costcoAccountView{CostcoSummary: sum, Profile: acct.ProfileName, ConnectedAt: acct.ConnectedAt}
			matched := fmt.Sprintf("%d of %d", sum.Matched, sum.Matched+sum.Unmatched+sum.Ambiguous)
			return view, []string{acct.Name, acct.ProfileName, strconv.Itoa(sum.Receipts), strconv.Itoa(sum.Items),
				strconv.Itoa(sum.Uncategorized), matched, fmtTime(sum.LastSync), sum.Status}
		})
}
