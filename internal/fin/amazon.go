package fin

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// amazonCooldown is how long fin leaves Amazon alone after it says "too
// many requests". On 2026-10-10 a limit hit after a morning of full reads
// still held five minutes later, and let only six pages through ninety
// minutes later; asking sooner only keeps it in place.
const amazonCooldown = 2 * time.Hour

var amazonName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// pfcPrimary is Plaid's personal finance categories, the vocabulary fin
// amazon categorize accepts so Amazon items total up with bank transactions.
var pfcPrimary = []string{
	"INCOME", "TRANSFER_IN", "TRANSFER_OUT", "LOAN_PAYMENTS", "BANK_FEES", "ENTERTAINMENT", "FOOD_AND_DRINK",
	"GENERAL_MERCHANDISE", "HOME_IMPROVEMENT", "MEDICAL", "PERSONAL_CARE", "GENERAL_SERVICES",
	"GOVERNMENT_AND_NON_PROFIT", "TRANSPORTATION", "TRAVEL", "RENT_AND_UTILITIES",
}

func (a *App) cmdAmazon(ctx context.Context, args []string) (*result, error) {
	sub, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "", "list":
		return a.cmdAmazonList(ctx, rest)
	case "login":
		return a.cmdAmazonLogin(ctx, rest)
	case "logout":
		return a.cmdAmazonLogout(ctx, rest)
	case "sync":
		return a.cmdAmazonSync(ctx, rest)
	case "categorize":
		return a.cmdAmazonCategorize(ctx, rest)
	case "profiles":
		return a.cmdAmazonProfiles(ctx, rest)
	}
	return nil, usageErr("unknown subcommand %q; run fin help amazon", sub)
}

// --- the encrypted session file ---

func amazonKeyAccount(acct state.AmazonAccount) string {
	return "amazon." + acct.Env + "." + acct.Name + ".key"
}

func (a *App) amazonSessionPath(acct state.AmazonAccount) string {
	return filepath.Join(a.DataDir, "amazon", acct.Env+"-"+acct.Name+".session")
}

// The session is about 5 KB of cookies, more than security(1) stores
// reliably, so it is sealed with AES-GCM in a file next to the database and
// only its key goes in the Keychain.
func (a *App) saveAmazonSession(acct state.AmazonAccount, s *amazon.Session) error {
	keyHex, err := a.Secrets.Get(amazonKeyAccount(acct))
	if errors.Is(err, keychain.ErrNotFound) {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return err
		}
		keyHex = hex.EncodeToString(k)
		err = a.Secrets.Set(amazonKeyAccount(acct), keyHex)
	}
	if err != nil {
		return newErr("KEYCHAIN_ERROR", "%v", err)
	}
	gcm, err := sessionCipher(keyHex)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(s)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, plain, []byte(acct.Env+"/"+acct.Name))
	path := a.amazonSessionPath(acct)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(sealed); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (a *App) loadAmazonSession(acct state.AmazonAccount) (*amazon.Session, error) {
	keyHex, err := a.Secrets.Get(amazonKeyAccount(acct))
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, amazonSignInErr(acct)
	}
	if err != nil {
		return nil, newErr("KEYCHAIN_ERROR", "%v", err)
	}
	sealed, err := os.ReadFile(a.amazonSessionPath(acct))
	if errors.Is(err, os.ErrNotExist) {
		return nil, amazonSignInErr(acct)
	}
	if err != nil {
		return nil, err
	}
	gcm, err := sessionCipher(keyHex)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, amazonSignInErr(acct)
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte(acct.Env+"/"+acct.Name))
	if err != nil {
		return nil, amazonSignInErr(acct)
	}
	var s amazon.Session
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, amazonSignInErr(acct)
	}
	return &s, nil
}

func sessionCipher(keyHex string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, newErr("KEYCHAIN_ERROR", "the Amazon session key in the Keychain is damaged; run fin amazon login again")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func amazonSignInErr(acct state.AmazonAccount) *CLIError {
	action := "run fin amazon login " + acct.Name
	return &CLIError{Code: "AMAZON_SIGNIN_EXPIRED",
		Message: fmt.Sprintf("Amazon no longer accepts the %s sign-in; sign in to amazon.com in Chrome, then %s", acct.Name, action),
		Details: map[string]any{"action": action}, exit: exitError}
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

// amazonSync says what a sync reads: by default new payments and the orders
// they point to. Full re-reads every payment; Orders reads just those order
// pages and no payments.
type amazonSync struct {
	Full   bool
	Orders []string
}

// syncAmazon reads payments and the order pages they point to, as opts
// says. progress hears what it is doing.
func (a *App) syncAmazon(ctx context.Context, s *store.Store, st *state.State, acct state.AmazonAccount, opts amazonSync, progress amazonProgress) (*amazonSyncView, error) {
	until, err := s.AmazonLimitedUntil(ctx, acct.Name)
	if err != nil {
		return nil, storeErr(err)
	}
	if until.After(a.Now()) {
		return nil, amazonLimitedErr(acct, until)
	}
	sess, err := a.loadAmazonSession(acct)
	if err != nil {
		return nil, err
	}
	client := amazon.NewClient(sess, a.amazonBase())
	client.Wait = a.AmazonPause
	if log, err := a.openAmazonLog(acct); err == nil {
		defer log.Close()
		client.Log = func(kind string, status int) {
			fmt.Fprintf(log, "%s %s %d\n", time.Now().UTC().Format(time.RFC3339), kind, status)
		}
	}
	now := a.Now().UTC()
	if len(opts.Orders) > 0 {
		return a.readAmazonOrders(ctx, s, st, acct, client, opts.Orders, &amazonSyncView{Account: acct.Name}, now, progress)
	}
	full := opts.Full
	known, err := s.AmazonPaymentKeys(ctx, acct.Name)
	if err != nil {
		return nil, storeErr(err)
	}
	epoch, err := a.amazonEpoch(ctx, s, st)
	if err != nil {
		return nil, err
	}
	complete, err := s.AmazonCompleteSince(ctx, acct.Name)
	if err != nil {
		return nil, storeErr(err)
	}
	// Until one read has gone back to the epoch without a gap, read all of it,
	// going on from where Amazon last cut that read short.
	history := complete == "" || complete > epoch
	resume := ""
	if history && !full {
		if resume, err = s.AmazonResumeKey(ctx, acct.Name); err != nil {
			return nil, storeErr(err)
		}
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
			if saveErr = s.SetAmazonResumeKey(ctx, acct.Name, next); saveErr != nil {
				return true
			}
		}
		for _, p := range page {
			progress.feed(fmt.Sprintf("%s  %-40s %9s", shortDate(p.Date), truncate(cmp.Or(p.Descriptor, p.Method), 40), p.Amount.USD()))
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
		return nil, a.amazonErr(ctx, s, acct, err)
	}
	switch {
	case resume != "":
		// Only part of the list was read this time: nothing to prune.
		if err := s.SetAmazonCompleteSince(ctx, acct.Name, epoch); err != nil {
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
func (a *App) readAmazonOrders(ctx context.Context, s *store.Store, st *state.State, acct state.AmazonAccount, client *amazon.Client, ids []string, view *amazonSyncView, now time.Time, progress amazonProgress) (*amazonSyncView, error) {
	progress.step(1, fmt.Sprintf("0 of %d", len(ids)))
	start := time.Now()
	// One page at a time: the client spaces requests at a person's pace.
	var stop error
	for i, id := range ids {
		page, err := client.OrderPage(ctx, id)
		progress.step(1, orderProgress(i+1, len(ids), time.Since(start)))
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
				progress.feed(fmt.Sprintf("%s  %-40s %9s", shortDate(o.Date), truncate(title, 40), it.Cost.USD()))
			}
		}
		if err := s.ApplyAmazonOrder(ctx, acct.Name, id, page, o, perr, now); err != nil {
			stop = storeErr(err)
			break
		}
		view.Orders++
	}
	// Keep cookies Amazon renewed, even when the sync stopped part way.
	if err := a.saveAmazonSession(acct, client.Session(acct.Profile)); err != nil {
		return nil, err
	}
	if stop != nil {
		return nil, a.amazonErr(ctx, s, acct, stop)
	}
	if _, err := s.ReparseAmazonOrders(ctx, amazon.ParseOrder); err != nil {
		return nil, storeErr(err)
	}
	if err := s.SetAmazonLimitedUntil(ctx, acct.Name, acct.Profile, time.Time{}); err != nil {
		return nil, storeErr(err)
	}
	acct.LastSync = &now
	view.LastSync = &now
	st.PutAmazon(acct)
	return view, nil
}

// amazonEpoch is the first day of Amazon payments to read: a week before
// the epoch set with fin epoch, else a week before the oldest bank
// transaction stored, since nothing older can match one, else two years
// back. The week covers Amazon dating a charge days before the bank does.
func (a *App) amazonEpoch(ctx context.Context, s *store.Store, st *state.State) (string, error) {
	first := st.Epoch
	if first == "" {
		var err error
		if first, err = s.EarliestTransaction(ctx); err != nil {
			return "", storeErr(err)
		}
	}
	if day, err := time.Parse("2006-01-02", first); err == nil {
		return day.AddDate(0, 0, -7).Format("2006-01-02"), nil
	}
	return a.Now().AddDate(-2, 0, 0).Format("2006-01-02"), nil
}

// amazonProgress hears what a sync is doing: Step how far each step is,
// Feed each thing it finds. Either may be nil.
type amazonProgress struct {
	Step func(step int, detail string)
	Feed func(line string)
}

func (p amazonProgress) step(i int, detail string) {
	if p.Step != nil {
		p.Step(i, detail)
	}
}

func (p amazonProgress) feed(line string) {
	if p.Feed != nil {
		p.Feed(line)
	}
}

// orderProgress is "34/104", a bar, and the time left at the pace so far.
func orderProgress(done, total int, took time.Duration) string {
	s := fmt.Sprintf("%d/%d  %s", done, total, ui.Bar(done, total, 12))
	if done < 3 || done >= total {
		return s
	}
	switch left := took / time.Duration(done) * time.Duration(total-done); {
	case left < time.Minute:
		return s + "  <1 min"
	default:
		return s + fmt.Sprintf("  ~%d min", int(left.Round(time.Minute)/time.Minute))
	}
}

func shortDate(day string) string {
	if t, err := time.Parse("2006-01-02", day); err == nil {
		return t.Format("Jan _2")
	}
	return day
}

func longDate(day string) string {
	if t, err := time.Parse("2006-01-02", day); err == nil {
		return t.Format("Jan 2, 2006")
	}
	return day
}

// amazonLimitedErr says Amazon refused requests and when fin will ask again.
func amazonLimitedErr(acct state.AmazonAccount, until time.Time) *CLIError {
	at := until.Local().Format("15:04")
	return &CLIError{Code: "AMAZON_RATE_LIMITED",
		Message: fmt.Sprintf("Amazon is limiting requests from %s; what was read so far is saved, and fin won't ask Amazon again before %s", acct.Name, at),
		Details: map[string]any{"action": fmt.Sprintf("run fin amazon sync %s after %s", acct.Name, at), "retry_at": until.UTC().Format(time.RFC3339)}, exit: exitError}
}

// openAmazonLog opens the account's request log, one line per request to
// Amazon (time, kind, HTTP status), kept to learn Amazon's limits.
func (a *App) openAmazonLog(acct state.AmazonAccount) (*os.File, error) {
	path := strings.TrimSuffix(a.amazonSessionPath(acct), ".session") + ".log"
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func (a *App) amazonErr(ctx context.Context, s *store.Store, acct state.AmazonAccount, err error) error {
	switch {
	case errors.Is(err, amazon.ErrSignIn):
		_ = s.SetAmazonAccountStatus(ctx, acct.Name, acct.Profile, "AMAZON_SIGNIN_EXPIRED")
		return amazonSignInErr(acct)
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, amazon.ErrRateLimited):
		wait := amazonCooldown
		var rl *amazon.RateLimited
		if errors.As(err, &rl) && rl.RetryAfter > wait {
			wait = rl.RetryAfter
		}
		until := a.Now().Add(wait)
		_ = s.SetAmazonAccountStatus(ctx, acct.Name, acct.Profile, "AMAZON_RATE_LIMITED")
		_ = s.SetAmazonLimitedUntil(ctx, acct.Name, acct.Profile, until)
		return amazonLimitedErr(acct, until)
	}
	var cerr *CLIError
	if errors.As(err, &cerr) {
		return err
	}
	_ = s.SetAmazonAccountStatus(ctx, acct.Name, acct.Profile, "AMAZON_ERROR")
	return newErr("AMAZON_ERROR", "%s: %v", acct.Name, err)
}

// syncAmazonAll syncs every Amazon account in the environment, turning each
// account's failure into an ItemError so the others still sync.
// progress, if set, gives each account's progress by its index.
func (a *App) syncAmazonAll(ctx context.Context, s *store.Store, st *state.State, accts []state.AmazonAccount, opts amazonSync, progress func(k int) amazonProgress) ([]amazonSyncView, []ItemError, error) {
	views, errs := []amazonSyncView{}, []ItemError{}
	for k, acct := range accts {
		var p amazonProgress
		if progress != nil {
			p = progress(k)
		}
		v, err := a.syncAmazon(ctx, s, st, acct, opts, p)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, nil, err
			}
			cerr := asCLIError(err)
			action, _ := cerr.Details["action"].(string)
			errs = append(errs, ItemError{Item: "amazon/" + acct.Name, Institution: "Amazon", Code: cerr.Code, Message: cerr.Message, Action: action})
			continue
		}
		views = append(views, *v)
	}
	if len(accts) > 0 {
		if err := a.saveState(st); err != nil {
			return nil, nil, err
		}
	}
	return views, errs, nil
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
	accts := st.AmazonForEnv(string(a.Env))
	if len(pos) == 1 {
		acct, ok := st.FindAmazon(string(a.Env), pos[0])
		if !ok {
			return nil, a.noAmazonAccount(st, pos[0])
		}
		accts = []state.AmazonAccount{acct}
	}
	if len(accts) == 0 {
		return nil, newErr("NO_AMAZON_ACCOUNT", "no Amazon account is connected; run fin amazon login <name>")
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
		acct, ok := st.FindAmazon(string(a.Env), owner)
		if !ok {
			return nil, usageErr("fin does not know which Amazon account placed %s; name it: fin amazon sync <name> --order %s", orders[0], orders[0])
		}
		accts = []state.AmazonAccount{acct}
	}
	var views []amazonSyncView
	var errs []ItemError
	opts := amazonSync{Full: *full, Orders: orders}
	if a.showSpinner() {
		// Each account has a payments step and an orders step.
		var steps []string
		for _, acct := range accts {
			steps = append(steps, acct.Name+": payments", acct.Name+": orders")
		}
		a.onScreen = true
		_, err = ui.Flow(ctx, a.Stdin, a.Stderr, ui.FlowOptions{Title: "fin → Amazon", Steps: steps}, func(ctx context.Context, r ui.Reporter) (ui.FlowResult, error) {
			var err error
			views, errs, err = a.syncAmazonAll(ctx, s, st, accts, opts, func(k int) amazonProgress {
				return amazonProgress{Step: func(i int, detail string) {
					if i == 1 {
						r.Done(2*k, "")
					}
					r.Start(2*k+i, detail)
				}, Feed: r.Feed}
			})
			if err != nil {
				return ui.FlowResult{}, err
			}
			for k, v := range views {
				r.Done(2*k, fmt.Sprintf("%d new", v.Payments))
				r.Done(2*k+1, fmt.Sprintf("%d read", v.Orders))
			}
			msg := "Done."
			if len(errs) > 0 {
				msg = errs[0].Message
			}
			return ui.FlowResult{Message: msg}, nil
		})
		a.onScreen = false
		if errors.Is(err, ui.ErrCancelled) {
			err = context.Canceled
		}
	} else {
		views, errs, err = a.syncAmazonAll(ctx, s, st, accts, opts, nil)
	}
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
		t.Rows = append(t.Rows, []string{v.Account, strconv.Itoa(v.Payments), strconv.Itoa(v.Orders), strconv.Itoa(v.Unreadable), strconv.Itoa(v.Failed)})
	}
	body := map[string]any{"env": a.Env, "store": a.storePath(), "amazon": views, "errors": errs}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- login ---

func (a *App) cmdAmazonProfiles(ctx context.Context, args []string) (*result, error) {
	if len(args) > 0 {
		return nil, usageErr("usage: fin amazon profiles")
	}
	profiles, err := a.chromeProfiles()
	if err != nil {
		return nil, err
	}
	t := &ui.Table{Title: "Chrome profiles", Headers: []string{"Profile", "Name", "Amazon"}}
	for _, p := range profiles {
		signed := ""
		if p.Amazon {
			signed = "signed in"
		}
		t.Rows = append(t.Rows, []string{p.Dir, p.Name, signed})
	}
	return &result{body: map[string]any{"profiles": profiles}, table: t}, nil
}

func (a *App) chromeProfiles() ([]amazon.Profile, error) {
	profiles, err := a.Chrome.Profiles()
	if errors.Is(err, amazon.ErrNoChrome) {
		return nil, newErr("NO_CHROME", "fin amazon reads your Amazon sign-in from Google Chrome, which is not set up on this Mac")
	}
	if err != nil {
		return nil, newErr("CHROME_ERROR", "%v", err)
	}
	return profiles, nil
}

var amazonLoginSteps = []string{
	"Read your Amazon sign-in from Chrome",
	"Check it with Amazon",
	"Read your payments",
	"Read your orders",
}

func (a *App) cmdAmazonLogin(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("amazon login", flag.ContinueOnError)
	profileFlag := fs.String("profile", "", "the Chrome profile, by directory or name")
	noSync := fs.Bool("no-sync", false, "connect without reading payments and orders yet")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usageErr("usage: fin amazon login <name> [--profile P]")
	}
	name := strings.ToLower(pos[0])
	if !amazonName.MatchString(name) {
		return nil, usageErr("an Amazon account name is lowercase letters, digits and dashes, such as kilian or home")
	}
	profiles, err := a.chromeProfiles()
	if err != nil {
		return nil, err
	}
	profile, err := a.pickProfile(profiles, *profileFlag)
	if err != nil {
		return nil, err
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	acct, exists := st.FindAmazon(string(a.Env), name)
	if !exists {
		acct = state.AmazonAccount{Name: name, Env: string(a.Env), ConnectedAt: a.Now().UTC()}
	}
	acct.Profile, acct.ProfileName = profile.Dir, profile.Name

	var view *amazonSyncView
	work := func(ctx context.Context, r ui.Reporter) (ui.FlowResult, error) {
		r.Start(0, "macOS asks to allow Chrome Safe Storage: click Allow")
		if !a.onScreen {
			fmt.Fprintln(a.Stderr, "macOS will ask to allow access to \"Chrome Safe Storage\"; click Allow (not Always Allow).")
		}
		sess, err := a.Chrome.Session(profile.Dir)
		if err != nil {
			return ui.FlowResult{}, newErr("CHROME_ERROR", "%v", err)
		}
		r.Done(0, fmt.Sprintf("%d cookies from %s", len(sess.Cookies), profile.Name))
		r.Start(1, "")
		client := amazon.NewClient(sess, a.amazonBase())
		if err := client.Check(ctx); err != nil {
			if errors.Is(err, amazon.ErrSignIn) {
				return ui.FlowResult{}, newErr("AMAZON_NOT_SIGNED_IN",
					"Amazon did not accept the sign-in from Chrome profile %q; sign in at amazon.com in that profile and run this again", profile.Name)
			}
			return ui.FlowResult{}, newErr("AMAZON_ERROR", "%v", err)
		}
		if err := a.saveAmazonSession(acct, client.Session(profile.Dir)); err != nil {
			return ui.FlowResult{}, err
		}
		st.PutAmazon(acct)
		if err := a.saveState(st); err != nil {
			return ui.FlowResult{}, err
		}
		r.Done(1, "signed in")
		if *noSync {
			return ui.FlowResult{Message: "Connected. Run fin amazon sync to read your payments and orders."}, nil
		}
		s, err := a.openStore(ctx)
		if err != nil {
			return ui.FlowResult{}, err
		}
		defer s.Close()
		view, err = a.syncAmazon(ctx, s, st, acct, amazonSync{Full: true}, amazonProgress{
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

	if a.showSpinner() {
		a.onScreen = true
		_, err = ui.Flow(ctx, a.Stdin, a.Stderr, ui.FlowOptions{
			Title:    "fin → Amazon",
			Subtitle: "Experimental. fin copies your Amazon sign-in from Chrome, then reads your payments and order pages from amazon.com, the way your browser does. Amazon does not offer this officially; it can stop working at any time.",
			Steps:    amazonLoginSteps,
		}, work)
		a.onScreen = false
		if errors.Is(err, ui.ErrCancelled) || errors.Is(err, context.Canceled) {
			return nil, newErr("CANCELLED", "cancelled")
		}
	} else {
		var r ui.Reporter = ui.PlainReporter{W: a.Stderr, Steps: amazonLoginSteps}
		if !a.human {
			r = linkOnlyReporter{a}
		}
		_, err = work(ctx, r)
	}
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

// pickProfile chooses the Chrome profile to import from: the one named, the
// only one signed in to Amazon, or one the person picks in a terminal.
func (a *App) pickProfile(profiles []amazon.Profile, want string) (amazon.Profile, error) {
	if want != "" {
		for _, p := range profiles {
			if p.Dir == want || strings.EqualFold(p.Name, want) {
				if !p.Amazon {
					return p, newErr("AMAZON_NOT_SIGNED_IN", "Chrome profile %q is not signed in to Amazon; sign in at amazon.com in that profile, then run this again", p.Name)
				}
				return p, nil
			}
		}
		return amazon.Profile{}, &CLIError{Code: "NO_SUCH_PROFILE", Message: fmt.Sprintf("Chrome has no profile %q", want),
			Details: map[string]any{"profiles": profiles}, exit: exitError}
	}
	signed := []amazon.Profile{}
	for _, p := range profiles {
		if p.Amazon {
			signed = append(signed, p)
		}
	}
	switch {
	case len(signed) == 0:
		return amazon.Profile{}, newErr("AMAZON_NOT_SIGNED_IN", "no Chrome profile is signed in to Amazon; sign in at amazon.com in Chrome, then run this again")
	case len(signed) == 1:
		return signed[0], nil
	case a.IsTerminal == nil || !a.IsTerminal() || a.ReadLine == nil:
		return amazon.Profile{}, &CLIError{Code: "PROFILE_REQUIRED",
			Message: "several Chrome profiles are signed in to Amazon; pick one with --profile",
			Details: map[string]any{"profiles": signed}, exit: exitUsage}
	}
	ui.Print(a.Stderr, "Chrome profiles signed in to Amazon:\n")
	for i, p := range signed {
		ui.Print(a.Stderr, fmt.Sprintf("  %d  %s %s\n", i+1, p.Name, ui.Muted.Render("("+p.Dir+")")))
	}
	line, err := a.ReadLine(fmt.Sprintf("Which one? [1-%d] ", len(signed)))
	if err != nil {
		return amazon.Profile{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(signed) {
		return amazon.Profile{}, usageErr("pick a number from 1 to %d", len(signed))
	}
	return signed[n-1], nil
}

func (a *App) noAmazonAccount(st *state.State, name string) error {
	names := []string{}
	for _, acct := range st.AmazonForEnv(string(a.Env)) {
		names = append(names, acct.Name)
	}
	return &CLIError{Code: "NO_AMAZON_ACCOUNT", Message: fmt.Sprintf("no Amazon account named %q", name),
		Details: map[string]any{"accounts": names}, exit: exitError}
}

// --- logout ---

func (a *App) cmdAmazonLogout(ctx context.Context, args []string) (*result, error) {
	if len(args) != 1 {
		return nil, usageErr("usage: fin amazon logout <name>")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	acct, ok := st.FindAmazon(string(a.Env), args[0])
	if !ok {
		return nil, a.noAmazonAccount(st, args[0])
	}
	for _, path := range []string{a.amazonSessionPath(acct), strings.TrimSuffix(a.amazonSessionPath(acct), ".session") + ".log"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := a.Secrets.Delete(amazonKeyAccount(acct)); err != nil {
		return nil, newErr("KEYCHAIN_ERROR", "%v", err)
	}
	if _, err := os.Stat(a.storePath()); err == nil {
		s, err := a.openStore(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if err := s.DeleteAmazonAccount(ctx, acct.Name); err != nil {
			return nil, storeErr(err)
		}
	}
	st.RemoveAmazon(acct.Env, acct.Name)
	if err := a.saveState(st); err != nil {
		return nil, err
	}
	msg := ui.Line(ui.Good, "Removed "+acct.Name+", its saved sign-in, and its payments, orders and categories") +
		ui.Muted.Render("\n  Your Chrome stays signed in to Amazon.")
	return &result{body: map[string]any{"removed": acct.Name}, message: msg}, nil
}

// --- list ---

type amazonAccountView struct {
	store.AmazonSummary
	Profile     string    `json:"profile"`
	ConnectedAt time.Time `json:"connected_at"`
}

func (a *App) cmdAmazonList(ctx context.Context, args []string) (*result, error) {
	if len(args) > 0 {
		return nil, usageErr("usage: fin amazon")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	accts := st.AmazonForEnv(string(a.Env))
	sums := map[string]store.AmazonSummary{}
	if s, err := store.OpenReadOnly(ctx, a.storePath()); err == nil {
		sums, err = s.AmazonSummaries(ctx)
		s.Close()
		if err != nil {
			return nil, storeErr(err)
		}
	}
	views := []amazonAccountView{}
	t := &ui.Table{
		Title:   fmt.Sprintf("Amazon · %d accounts · experimental", len(accts)),
		Headers: []string{"Account", "Chrome profile", "Payments", "Orders", "Items", "To categorize", "Matched", "Last sync"},
		Right:   []int{2, 3, 4, 5, 6},
		Footer:  "Connect another with fin amazon login <name>.",
	}
	for _, acct := range accts {
		sum := sums[acct.Name]
		sum.Account = acct.Name
		if sum.LastSync == nil {
			sum.LastSync = acct.LastSync
		}
		views = append(views, amazonAccountView{AmazonSummary: sum, Profile: acct.ProfileName, ConnectedAt: acct.ConnectedAt})
		matched := fmt.Sprintf("%d of %d", sum.Matched, sum.Matched+sum.Unmatched+sum.Ambiguous)
		t.Rows = append(t.Rows, []string{acct.Name, acct.ProfileName, strconv.Itoa(sum.Payments), strconv.Itoa(sum.Orders),
			strconv.Itoa(sum.Items), strconv.Itoa(sum.Uncategorized), matched, fmtTime(sum.LastSync)})
	}
	body := map[string]any{"env": a.Env, "accounts": views}
	if len(accts) == 0 {
		return &result{body: body, message: "No Amazon account is connected. Connect one with fin amazon login <name>."}, nil
	}
	return &result{body: body, table: t}, nil
}

// --- categorize ---

type categoryInput struct {
	Item        string `json:"item"`
	Category    string `json:"category"`
	Detailed    string `json:"detailed"`
	ASINDefault bool   `json:"asin_default"`
}

func (a *App) cmdAmazonCategorize(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("amazon categorize", flag.ContinueOnError)
	set := fs.Bool("set", false, "save categories: ITEM CATEGORY [DETAILED], or a JSON list on stdin")
	all := fs.Bool("all", false, "list categorized items too")
	product := fs.Bool("product", false, "with --set ITEM CATEGORY, also use it for later purchases of the same product")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if !*set {
		if len(pos) > 0 {
			return nil, usageErr("usage: fin amazon categorize [--all] | --set ITEM CATEGORY [DETAILED]")
		}
		s, err := store.OpenReadOnly(ctx, a.storePath())
		if err != nil {
			return nil, storeErr(err)
		}
		defer s.Close()
		items, err := s.AmazonItems(ctx, *all)
		if err != nil {
			return nil, storeErr(err)
		}
		t := &ui.Table{
			Title:   fmt.Sprintf("Amazon items · %d", len(items)),
			Headers: []string{"Item", "Date", "Title", "Qty", "Cost", "Category"},
			Right:   []int{3, 4},
			Footer:  "Set one with fin amazon categorize --set ITEM CATEGORY [--product].",
		}
		for _, it := range items {
			t.Rows = append(t.Rows, []string{it.Item, it.Date, truncate(it.Title, 48), strconv.Itoa(it.Quantity),
				strconv.FormatFloat(it.Cost, 'f', 2, 64), deref(it.Category)})
		}
		return &result{body: map[string]any{"items": items, "categories": pfcPrimary}, table: t}, nil
	}

	var inputs []categoryInput
	switch len(pos) {
	case 0:
		raw, err := io.ReadAll(io.LimitReader(a.Stdin, 8<<20))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &inputs); err != nil {
			return nil, usageErr(`--set reads a JSON list on stdin: [{"item": "111-…#1", "category": "HOME_IMPROVEMENT", "detailed": "…", "asin_default": true}]`)
		}
	case 2, 3:
		in := categoryInput{Item: pos[0], Category: pos[1], ASINDefault: *product}
		if len(pos) == 3 {
			in.Detailed = pos[2]
		}
		inputs = []categoryInput{in}
	default:
		return nil, usageErr("usage: fin amazon categorize --set ITEM CATEGORY [DETAILED]")
	}
	changes := []store.CategoryChange{}
	for _, in := range inputs {
		id, lineStr, ok := strings.Cut(in.Item, "#")
		line, err := strconv.Atoi(lineStr)
		if !ok || err != nil || !amazon.ValidOrderID(id) {
			return nil, usageErr("%q is not an item; items look like 111-1234567-1234567#1", in.Item)
		}
		cat, det := strings.ToUpper(strings.TrimSpace(in.Category)), strings.ToUpper(strings.TrimSpace(in.Detailed))
		if !slices.Contains(pfcPrimary, cat) {
			return nil, &CLIError{Code: "UNKNOWN_CATEGORY", Message: fmt.Sprintf("%q is not one of Plaid's categories", in.Category),
				Details: map[string]any{"categories": pfcPrimary}, exit: exitUsage}
		}
		if det != "" && !strings.HasPrefix(det, cat+"_") {
			return nil, usageErr("detailed category %q must start with %s_", in.Detailed, cat)
		}
		changes = append(changes, store.CategoryChange{OrderID: id, Line: line, Category: cat, CategoryDetailed: det, ASINDefault: in.ASINDefault})
	}
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := s.SetAmazonCategories(ctx, changes, a.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNoSuchItem) {
			return nil, newErr("NO_SUCH_ITEM", "%v; list items with fin amazon categorize", err)
		}
		return nil, storeErr(err)
	}
	left, err := s.AmazonItems(ctx, false)
	if err != nil {
		return nil, storeErr(err)
	}
	msg := ui.Line(ui.Good, fmt.Sprintf("Saved %d categories", len(changes))) + ui.Muted.Render(fmt.Sprintf("  %d items left to categorize", len(left)))
	return &result{body: map[string]any{"saved": len(changes), "uncategorized": len(left)}, message: msg}, nil
}
