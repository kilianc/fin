package fin

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/chrome"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// amazonRetailer is Amazon in what retailers share. Its Sync and Forget are
// set in init: they lead back to retailers, which would be a cycle here.
var amazonRetailer = retailer{ID: "amazon", Name: "Amazon", Site: "amazon.com", Orders: "https://www.amazon.com/your-orders/orders", SignIn: amazon.ErrSignIn}

func init() {
	amazonRetailer.Sync = func(a *App, ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, p syncProgress) (any, error) {
		return a.syncAmazon(ctx, s, st, acct, amazonSync{}, p)
	}
	amazonRetailer.Forget = func(ctx context.Context, s *store.Store, account string) error {
		return s.DeleteAmazonAccount(ctx, account)
	}
	retailers = append(retailers, amazonRetailer)
}

func (a *App) cmdAmazon(ctx context.Context, args []string) (*result, error) {
	return a.cmdRetailer(ctx, amazonRetailer, args, a.cmdAmazonList, a.cmdAmazonLogin, a.cmdAmazonSync, a.cmdAmazonProfiles)
}

func (a *App) amazonBase() string {
	if a.AmazonBase != "" {
		return a.AmazonBase
	}
	return amazon.Origin
}

// --- sync ---

type amazonSyncView struct {
	Account    string     `json:"account"`
	Since      string     `json:"since"`
	Payments   int        `json:"new_payments"`
	Orders     int        `json:"orders_read"`
	Unreadable int        `json:"orders_unreadable"`
	Failed     int        `json:"orders_failed"`
	LastSync   *time.Time `json:"last_sync"`
}

func (v *amazonSyncView) summary() (string, int, string) {
	return v.Account, v.Payments + v.Orders, fmt.Sprintf("%d payments, %d orders", v.Payments, v.Orders)
}

// amazonSync says what a sync reads: by default new payments and the orders
// they point to. Full re-reads every payment; Orders reads just those order
// pages and no payments.
type amazonSync struct {
	Full   bool
	Orders []string
}

// syncAmazon reads payments and the order pages they point to, as opts
// says. progress hears what it is doing.
func (a *App) syncAmazon(ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, opts amazonSync, progress syncProgress) (*amazonSyncView, error) {
	if err := a.beginSync(ctx, s, acct); err != nil {
		return nil, err
	}
	var sess amazon.Session
	if err := a.loadSession(acct, &sess); err != nil {
		return nil, err
	}
	client := amazon.NewClient(&sess, a.amazonBase())
	client.Gate = a.retailerGate(amazonRetailer, a.AmazonPause)
	log, closeLog := a.requestLog(acct)
	defer closeLog()
	client.Log = log
	now := a.Now().UTC()
	if len(opts.Orders) > 0 {
		return a.readAmazonOrders(ctx, s, st, acct, client, opts.Orders, &amazonSyncView{Account: acct.Name}, now, progress)
	}
	full := opts.Full
	known, err := s.AmazonPaymentKeys(ctx, acct.Name)
	if err != nil {
		return nil, storeErr(err)
	}
	epoch, err := a.readSince(ctx, s, st)
	if err != nil {
		return nil, err
	}
	state, err := s.RetailerAccountState(ctx, acct.Retailer, acct.Name)
	if err != nil {
		return nil, storeErr(err)
	}
	// Until one read has gone back to the epoch without a gap, read all of it,
	// going on from where Amazon last cut that read short.
	history := state.CompleteSince == "" || state.CompleteSince > epoch
	resume := ""
	if history && !full {
		resume = state.ResumeKey
	}
	full = full || history

	// Each page is saved as it arrives, so a sync cut short by Amazon keeps
	// what it read; a full re-read prunes rows Amazon no longer lists only
	// once it reaches the end.
	progress.step(0, "")
	seen := map[string]bool{}
	fresh := []string{}
	added := 0
	var saveErr error
	_, err = client.Payments(ctx, acct.Name, resume, func(all []amazon.Payment, next string) bool {
		// The list is newest first: past the epoch, nothing more is needed.
		page, past := []amazon.Payment{}, false
		for _, p := range all {
			if p.Date < epoch {
				past = true
				continue
			}
			page = append(page, p)
		}
		allKnown := true
		for _, p := range page {
			seen[p.Key] = true
			if !known[p.Key] {
				allKnown = false
				added++
				fresh = append(fresh, p.OrderIDs...)
			}
		}
		if saveErr = s.ApplyAmazonPayments(ctx, acct.Name, acct.Profile, page, now); saveErr != nil {
			return true
		}
		if history && !past {
			if saveErr = s.SetResumeKey(ctx, acct.Retailer, acct.Name, next); saveErr != nil {
				return true
			}
		}
		for _, p := range page {
			progress.feed(feedLine(p.Date, cmp.Or(p.Descriptor, p.Method), p.Amount.USD()))
		}
		if len(page) > 0 {
			progress.step(0, fmt.Sprintf("%d so far, back to %s", len(seen), longDate(page[len(page)-1].Date)))
		}
		return past || (!full && allKnown)
	})
	if saveErr != nil {
		return nil, storeErr(saveErr)
	}
	if err != nil {
		return nil, a.syncErr(ctx, s, acct, err)
	}
	switch {
	case resume != "":
		// Only part of the list was read this time: nothing to prune.
		if err := s.SetCompleteSince(ctx, acct.Retailer, acct.Name, epoch); err != nil {
			return nil, storeErr(err)
		}
	case full:
		if err := s.PruneAmazonPayments(ctx, acct.Name, epoch, seen); err != nil {
			return nil, storeErr(err)
		}
	}

	ids, err := s.AmazonOrdersToFetch(ctx, acct.Name, fresh, now.AddDate(0, 0, -60), now.Add(-12*time.Hour))
	if err != nil {
		return nil, storeErr(err)
	}
	return a.readAmazonOrders(ctx, s, st, acct, client, ids, &amazonSyncView{Account: acct.Name, Since: epoch, Payments: added}, now, progress)
}

// readAmazonOrders reads and stores the given order pages, then saves the
// renewed session and the account's last sync.
func (a *App) readAmazonOrders(ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, client *amazon.Client, ids []string, view *amazonSyncView, now time.Time, progress syncProgress) (*amazonSyncView, error) {
	progress.step(1, fmt.Sprintf("0 of %d", len(ids)))
	start := time.Now()
	// One page at a time: the client spaces requests at a person's pace.
	var stop error
	for i, id := range ids {
		page, err := client.OrderPage(ctx, id)
		progress.step(1, countProgress(i+1, len(ids), time.Since(start)))
		switch {
		case errors.Is(err, amazon.ErrSignIn), errors.Is(err, amazon.ErrRateLimited), errors.Is(err, context.Canceled):
			stop = err
		case err != nil:
			// Not found or a server hiccup: try again on the next sync.
			view.Failed++
			continue
		}
		if stop != nil {
			break
		}
		o, perr := amazon.ParseOrder(page)
		if errors.Is(perr, amazon.ErrSignIn) {
			stop = perr
			break
		}
		if perr != nil {
			o = nil
			view.Unreadable++
		} else {
			for _, it := range o.Items {
				title := it.Title
				if it.Quantity > 1 {
					title += fmt.Sprintf(" ×%d", it.Quantity)
				}
				progress.feed(feedLine(o.Date, title, it.Cost.USD()))
			}
		}
		if err := s.ApplyAmazonOrder(ctx, acct.Name, id, page, o, perr, now); err != nil {
			stop = storeErr(err)
			break
		}
		view.Orders++
	}
	// Keep cookies Amazon renewed, even when the sync stopped part way.
	if err := a.saveSession(acct, client.Session(acct.Profile)); err != nil {
		return nil, err
	}
	if stop != nil {
		return nil, a.syncErr(ctx, s, acct, stop)
	}
	if _, err := s.ReparseAmazonOrders(ctx, amazon.ParseOrder); err != nil {
		return nil, storeErr(err)
	}
	if err := a.endSync(ctx, s, st, acct, now); err != nil {
		return nil, err
	}
	view.LastSync = &now
	return view, nil
}

func (a *App) cmdAmazonSync(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("amazon sync", flag.ContinueOnError)
	full := fs.Bool("full", false, "re-read every payment, not just new ones")
	var orders []string
	fs.Func("order", "read just this order's page again; repeatable", func(id string) error {
		if !amazon.ValidOrderID(id) || !amazon.RetailOrderID(id) {
			return fmt.Errorf("%q is not an amazon.com order ID like 111-1234567-1234567", id)
		}
		orders = append(orders, id)
		return nil
	})
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 || (*full && len(orders) > 0) {
		return nil, usageErr("usage: fin amazon sync [name] [--full | --order ID ...]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	name := ""
	if len(pos) == 1 {
		name = pos[0]
	}
	accts, err := a.connectedAccounts(st, amazonRetailer, name)
	if err != nil {
		return nil, err
	}
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if len(orders) > 0 && len(accts) > 1 {
		// The orders belong to the account whose payments named them.
		owner, err := s.AmazonOrderAccount(ctx, orders[0])
		if err != nil {
			return nil, storeErr(err)
		}
		acct, ok := st.FindRetailer(amazonRetailer.ID, string(a.Env), owner)
		if !ok {
			return nil, usageErr("fin does not know which Amazon account placed %s; name it: fin amazon sync <name> --order %s", orders[0], orders[0])
		}
		accts = []state.RetailerAccount{acct}
	}
	opts := amazonSync{Full: *full, Orders: orders}
	views, errs, err := a.runSync(ctx, st, amazonRetailer, accts, []string{"payments", "orders"},
		func(acct state.RetailerAccount, p syncProgress) (any, error) {
			return a.syncAmazon(ctx, s, st, acct, opts, p)
		},
		func(v any) []string {
			view := v.(*amazonSyncView)
			return []string{fmt.Sprintf("%d new", view.Payments), fmt.Sprintf("%d read", view.Orders)}
		})
	if err != nil {
		return nil, err
	}
	t := &ui.Table{
		Title:   fmt.Sprintf("Amazon · %d synced", len(views)),
		Headers: []string{"Account", "New payments", "Orders read", "Unreadable", "Failed"},
		Right:   []int{1, 2, 3, 4},
		Footer:  "Stored in " + a.storePath() + ". Categorize new items with fin amazon categorize.",
	}
	for _, v := range views {
		v := v.(*amazonSyncView)
		t.Rows = append(t.Rows, []string{v.Account, strconv.Itoa(v.Payments), strconv.Itoa(v.Orders), strconv.Itoa(v.Unreadable), strconv.Itoa(v.Failed)})
	}
	body := map[string]any{"env": a.Env, "store": a.storePath(), "amazon": views, "errors": errs}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- login ---

func (a *App) cmdAmazonProfiles(ctx context.Context, args []string) (*result, error) {
	return a.cmdProfiles(amazonRetailer, a.amazonProfiles, args)
}

func (a *App) amazonProfiles() ([]chromeProfile, error) {
	return a.chromeProfiles(amazonRetailer, func(c chrome.Chrome, profile string) (bool, error) {
		// A profile without cookies is simply signed out.
		ok, _ := c.HasCookies(profile, []string{".amazon.com", "www.amazon.com", "amazon.com"})
		return ok, nil
	})
}

var amazonLoginSteps = []string{
	"Read your Amazon sign-in from Chrome",
	"Check it with Amazon",
	"Read your payments",
	"Read your orders",
}

func (a *App) cmdAmazonLogin(ctx context.Context, args []string) (*result, error) {
	st, acct, profile, noSync, err := a.loginAccount(amazonRetailer, args, a.amazonProfiles)
	if err != nil {
		return nil, err
	}
	name := acct.Name
	// Logging in again must not skip a cooldown Amazon started.
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	err = a.beginSync(ctx, s, acct)
	s.Close()
	if err != nil {
		return nil, err
	}

	var view *amazonSyncView
	work := func(ctx context.Context, r ui.Reporter) (ui.FlowResult, error) {
		r.Start(0, "macOS asks to allow Chrome Safe Storage: click Allow")
		if !a.onScreen {
			fmt.Fprintln(a.Stderr, "macOS will ask to allow access to \"Chrome Safe Storage\"; click Allow (not Always Allow).")
		}
		sess, err := amazon.Chrome(a.Chrome).Session(profile.Dir)
		if err != nil {
			return ui.FlowResult{}, newErr("CHROME_ERROR", "%v", err)
		}
		r.Done(0, fmt.Sprintf("%d cookies from %s", len(sess.Cookies), profile.Name))
		r.Start(1, "")
		client := amazon.NewClient(sess, a.amazonBase())
		client.Gate = a.retailerGate(amazonRetailer, a.AmazonPause)
		if err := client.Check(ctx); err != nil {
			return ui.FlowResult{}, a.loginErr(ctx, nil, amazonRetailer, acct, profile.Name, err)
		}
		if err := a.saveSession(acct, client.Session(profile.Dir)); err != nil {
			return ui.FlowResult{}, err
		}
		st.PutRetailer(acct)
		if err := a.saveState(st); err != nil {
			return ui.FlowResult{}, err
		}
		r.Done(1, "signed in")
		if noSync {
			return ui.FlowResult{Message: "Connected. Run fin amazon sync to read your payments and orders."}, nil
		}
		s, err := a.openStore(ctx)
		if err != nil {
			return ui.FlowResult{}, err
		}
		defer s.Close()
		view, err = a.syncAmazon(ctx, s, st, acct, amazonSync{Full: true}, syncProgress{
			Step: func(i int, detail string) {
				if i == 1 {
					r.Done(2, "")
				}
				r.Start(i+2, detail)
			},
			Feed: r.Feed,
		})
		if err != nil {
			return ui.FlowResult{}, err
		}
		r.Done(2, fmt.Sprintf("%d payments", view.Payments))
		r.Done(3, fmt.Sprintf("%d orders", view.Orders))
		if err := a.saveState(st); err != nil {
			return ui.FlowResult{}, err
		}
		return ui.FlowResult{Message: fmt.Sprintf("Connected %s and read %d orders. Next: fin amazon categorize.", name, view.Orders)}, nil
	}

	err = a.runLogin(ctx, amazonRetailer, "Experimental. fin copies your Amazon sign-in from Chrome, then reads your payments and order pages from amazon.com, the way your browser does. Amazon does not offer this officially; it can stop working at any time.", amazonLoginSteps, work)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"env": a.Env, "account": name, "profile": profile, "sync": view}
	msg := ui.Line(ui.Good, fmt.Sprintf("Connected Amazon account %s from Chrome profile %s", name, profile.Name))
	if view != nil {
		msg += fmt.Sprintf("\n  %d payments and %d orders read", view.Payments, view.Orders)
	}
	return &result{body: body, message: msg}, nil
}

// --- list ---

type amazonAccountView struct {
	store.AmazonSummary
	Profile     string    `json:"profile"`
	ConnectedAt time.Time `json:"connected_at"`
}

func (a *App) cmdAmazonList(ctx context.Context, args []string) (*result, error) {
	return listRetailer(a, ctx, amazonRetailer, args, (*store.Store).AmazonSummaries,
		[]string{"Account", "Chrome profile", "Payments", "Orders", "Items", "To categorize", "Matched", "Last sync"}, []int{2, 3, 4, 5, 6},
		func(acct state.RetailerAccount, sum store.AmazonSummary) (any, []string) {
			sum.Account = acct.Name
			if sum.LastSync == nil {
				sum.LastSync = acct.LastSync
			}
			view := amazonAccountView{AmazonSummary: sum, Profile: acct.ProfileName, ConnectedAt: acct.ConnectedAt}
			matched := fmt.Sprintf("%d of %d", sum.Matched, sum.Matched+sum.Unmatched+sum.Ambiguous)
			return view, []string{acct.Name, acct.ProfileName, strconv.Itoa(sum.Payments), strconv.Itoa(sum.Orders),
				strconv.Itoa(sum.Items), strconv.Itoa(sum.Uncategorized), matched, fmtTime(sum.LastSync)}
		})
}
