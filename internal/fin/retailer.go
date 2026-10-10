package fin

import (
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

	"github.com/kilianc/fin/internal/chrome"
	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/pace"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// retailer is one store fin itemizes, such as Amazon. What retailers share
// works from it: the sealed session, pacing and cooldown, errors, the sync
// screen, categories, logout and fin sync. A retailer brings its own client,
// tables and commands, and adds itself to retailers.
type retailer struct {
	ID   string // "amazon": its name in state, tables, files, the Keychain and error codes
	Name string // "Amazon"
	Site string // "amazon.com", where people sign in
	// SignIn is the error the retailer's client returns once the site no
	// longer accepts the saved sign-in.
	SignIn error
	// Sync reads what is new for one account, as fin sync does.
	Sync func(a *App, ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, progress syncProgress) (any, error)
	// Forget deletes an account's rows, the shared ones included.
	Forget func(ctx context.Context, s *store.Store, account string) error
}

// retailers are the retailers fin sync and fin sheet include. Each adds
// itself in an init function, once its Sync and Forget are set.
var retailers []retailer

func findRetailer(id string) retailer {
	for _, r := range retailers {
		if r.ID == id {
			return r
		}
	}
	panic("fin: unknown retailer " + id)
}

func (r retailer) code(suffix string) string { return strings.ToUpper(r.ID) + "_" + suffix }

// retailerCooldown is how long fin leaves a retailer alone after it says
// "too many requests". On 2026-10-10 an Amazon limit, hit after a morning of
// full reads, still held five minutes later and let only six pages through
// ninety minutes later; asking sooner only keeps it in place.
const retailerCooldown = 2 * time.Hour

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// pfcPrimary is Plaid's personal finance categories, the vocabulary
// categorize accepts so items total up with bank transactions.
var pfcPrimary = []string{
	"INCOME", "TRANSFER_IN", "TRANSFER_OUT", "LOAN_PAYMENTS", "BANK_FEES", "ENTERTAINMENT", "FOOD_AND_DRINK",
	"GENERAL_MERCHANDISE", "HOME_IMPROVEMENT", "MEDICAL", "PERSONAL_CARE", "GENERAL_SERVICES",
	"GOVERNMENT_AND_NON_PROFIT", "TRANSPORTATION", "TRAVEL", "RENT_AND_UTILITIES",
}

// --- files ---

func keyAccount(acct state.RetailerAccount) string {
	return acct.Retailer + "." + acct.Env + "." + acct.Name + ".key"
}

// retailerFile is one of an account's files in fin's data directory, such
// as its sealed session (".session") or its request log (".log").
func (a *App) retailerFile(acct state.RetailerAccount, ext string) string {
	return filepath.Join(a.DataDir, acct.Retailer, acct.Env+"-"+acct.Name+ext)
}

// saveSession seals v with AES-GCM in a file next to the database, with only
// its key in the Keychain: a session can be more than security(1) stores
// reliably.
func (a *App) saveSession(acct state.RetailerAccount, v any) error {
	keyHex, err := a.Secrets.Get(keyAccount(acct))
	if errors.Is(err, keychain.ErrNotFound) {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return err
		}
		keyHex = hex.EncodeToString(k)
		err = a.Secrets.Set(keyAccount(acct), keyHex)
	}
	if err != nil {
		return newErr("KEYCHAIN_ERROR", "%v", err)
	}
	gcm, err := sessionCipher(acct, keyHex)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, plain, []byte(acct.Env+"/"+acct.Name))
	path := a.retailerFile(acct, ".session")
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

// loadSession opens the account's sealed session into v. Anything missing
// or unreadable is the retailer's sign-in error: logging in again fixes it.
func (a *App) loadSession(acct state.RetailerAccount, v any) error {
	keyHex, err := a.Secrets.Get(keyAccount(acct))
	if errors.Is(err, keychain.ErrNotFound) {
		return signInErr(acct)
	}
	if err != nil {
		return newErr("KEYCHAIN_ERROR", "%v", err)
	}
	sealed, err := os.ReadFile(a.retailerFile(acct, ".session"))
	if errors.Is(err, os.ErrNotExist) {
		return signInErr(acct)
	}
	if err != nil {
		return err
	}
	gcm, err := sessionCipher(acct, keyHex)
	if err != nil {
		return err
	}
	if len(sealed) < gcm.NonceSize() {
		return signInErr(acct)
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte(acct.Env+"/"+acct.Name))
	if err != nil || json.Unmarshal(plain, v) != nil {
		return signInErr(acct)
	}
	return nil
}

func sessionCipher(acct state.RetailerAccount, keyHex string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, newErr("KEYCHAIN_ERROR", "the %s session key in the Keychain is damaged; run fin %s login %s again",
			findRetailer(acct.Retailer).Name, acct.Retailer, acct.Name)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// requestLog appends one line per request to the account's log (time, kind,
// HTTP status), kept to learn the retailer's limits. close may be called
// even when the log could not be opened.
func (a *App) requestLog(acct state.RetailerAccount) (log func(kind string, status int), close func()) {
	f, err := os.OpenFile(a.retailerFile(acct, ".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, func() {}
	}
	return func(kind string, status int) {
		fmt.Fprintf(f, "%s %s %d\n", time.Now().UTC().Format(time.RFC3339), kind, status)
	}, func() { f.Close() }
}

// --- errors ---

func signInErr(acct state.RetailerAccount) *CLIError {
	r := findRetailer(acct.Retailer)
	action := fmt.Sprintf("run fin %s login %s", r.ID, acct.Name)
	return &CLIError{Code: r.code("SIGNIN_EXPIRED"),
		Message: fmt.Sprintf("%s no longer accepts the %s sign-in; sign in to %s in Chrome, then %s", r.Name, acct.Name, r.Name, action),
		Details: map[string]any{"action": action}, exit: exitError}
}

// limitedErr says the retailer refused requests and when fin will ask again.
func limitedErr(acct state.RetailerAccount, until time.Time) *CLIError {
	r := findRetailer(acct.Retailer)
	at := until.Local().Format("15:04")
	return &CLIError{Code: r.code("RATE_LIMITED"),
		Message: fmt.Sprintf("%s is limiting requests from %s; what was read so far is saved, and fin won't ask %s again before %s", r.Name, acct.Name, r.Name, at),
		Details: map[string]any{"action": fmt.Sprintf("run fin %s sync %s after %s", r.ID, acct.Name, at), "retry_at": until.UTC().Format(time.RFC3339)}, exit: exitError}
}

// syncErr turns what stopped a sync into a CLIError and records it on the
// account; a rate limit also starts the cooldown.
func (a *App) syncErr(ctx context.Context, s *store.Store, acct state.RetailerAccount, err error) error {
	r := findRetailer(acct.Retailer)
	status := func(code string) { _ = s.SetRetailerStatus(ctx, r.ID, acct.Name, acct.Profile, code) }
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, r.SignIn):
		status(r.code("SIGNIN_EXPIRED"))
		return signInErr(acct)
	case errors.Is(err, pace.ErrRateLimited):
		wait := retailerCooldown
		var rl *pace.RateLimited
		if errors.As(err, &rl) && rl.RetryAfter > wait {
			wait = rl.RetryAfter
		}
		until := a.Now().Add(wait)
		status(r.code("RATE_LIMITED"))
		_ = s.SetLimitedUntil(ctx, r.ID, acct.Name, acct.Profile, until)
		return limitedErr(acct, until)
	}
	var cerr *CLIError
	if errors.As(err, &cerr) {
		return err
	}
	status(r.code("ERROR"))
	return newErr(r.code("ERROR"), "%s: %v", acct.Name, err)
}

func (a *App) noAccountErr(st *state.State, r retailer, name string) error {
	names := []string{}
	for _, acct := range st.RetailerAccounts(r.ID, string(a.Env)) {
		names = append(names, acct.Name)
	}
	return &CLIError{Code: "NO_" + r.code("ACCOUNT"), Message: fmt.Sprintf("no %s account named %q", r.Name, name),
		Details: map[string]any{"accounts": names}, exit: exitError}
}

// --- sync ---

// beginSync refuses to start while the account's cooldown runs, so nothing
// at all is asked of the retailer until it is over.
func (a *App) beginSync(ctx context.Context, s *store.Store, acct state.RetailerAccount) error {
	st, err := s.RetailerAccountState(ctx, acct.Retailer, acct.Name)
	if err != nil {
		return storeErr(err)
	}
	if st.LimitedUntil.After(a.Now()) {
		return limitedErr(acct, st.LimitedUntil)
	}
	return nil
}

// endSync records a sync that finished: the cooldown is over, and the
// account's last sync is now.
func (a *App) endSync(ctx context.Context, s *store.Store, st *state.State, acct state.RetailerAccount, now time.Time) error {
	if err := s.SetLimitedUntil(ctx, acct.Retailer, acct.Name, acct.Profile, time.Time{}); err != nil {
		return storeErr(err)
	}
	acct.LastSync = &now
	st.PutRetailer(acct)
	return nil
}

// syncAccounts syncs accts one after another with sync, turning each
// account's failure into an ItemError so the others still sync, then saves
// the state. progress, if set, gives each account's progress by its index.
func (a *App) syncAccounts(ctx context.Context, st *state.State, r retailer, accts []state.RetailerAccount,
	sync func(acct state.RetailerAccount, p syncProgress) (any, error), progress func(k int) syncProgress) ([]any, []ItemError, error) {
	views, errs := []any{}, []ItemError{}
	for k, acct := range accts {
		var p syncProgress
		if progress != nil {
			p = progress(k)
		}
		v, err := sync(acct, p)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, nil, err
			}
			cerr := asCLIError(err)
			action, _ := cerr.Details["action"].(string)
			errs = append(errs, ItemError{Item: r.ID + "/" + acct.Name, Institution: r.Name, Code: cerr.Code, Message: cerr.Message, Action: action})
			continue
		}
		views = append(views, v)
	}
	if len(accts) > 0 {
		if err := a.saveState(st); err != nil {
			return nil, nil, err
		}
	}
	return views, errs, nil
}

// runSync syncs accts with sync. In a terminal it shows the sync screen:
// each account's steps, how far each is, and a feed of what it finds;
// summarize says what each step of a finished account read.
func (a *App) runSync(ctx context.Context, st *state.State, r retailer, accts []state.RetailerAccount, steps []string,
	sync func(acct state.RetailerAccount, p syncProgress) (any, error), summarize func(view any) []string) ([]any, []ItemError, error) {
	if !a.showSpinner() {
		return a.syncAccounts(ctx, st, r, accts, sync, nil)
	}
	var labels []string
	for _, acct := range accts {
		for _, step := range steps {
			labels = append(labels, acct.Name+": "+step)
		}
	}
	var views []any
	var errs []ItemError
	a.onScreen = true
	_, err := ui.Flow(ctx, a.Stdin, a.Stderr, ui.FlowOptions{Title: "fin → " + r.Name, Steps: labels}, func(ctx context.Context, rep ui.Reporter) (ui.FlowResult, error) {
		k := 0
		var err error
		views, errs, err = a.syncAccounts(ctx, st, r, accts, func(acct state.RetailerAccount, p syncProgress) (any, error) {
			v, err := sync(acct, p)
			if err == nil {
				for i, detail := range summarize(v) {
					rep.Done(k*len(steps)+i, detail)
				}
			}
			k++
			return v, err
		}, func(k int) syncProgress {
			at := 0
			return syncProgress{Step: func(i int, detail string) {
				for ; at < i; at++ {
					rep.Done(k*len(steps)+at, "")
				}
				rep.Start(k*len(steps)+i, detail)
			}, Feed: rep.Feed}
		})
		if err != nil {
			return ui.FlowResult{}, err
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
	return views, errs, err
}

// syncRetailers syncs every connected retailer account, as fin sync does.
// The views are keyed by retailer ID.
func (a *App) syncRetailers(ctx context.Context, s *store.Store, st *state.State) (map[string][]any, []ItemError, error) {
	out, errs := map[string][]any{}, []ItemError{}
	for _, r := range retailers {
		accts := st.RetailerAccounts(r.ID, string(a.Env))
		if len(accts) == 0 {
			continue
		}
		views, e, err := a.syncAccounts(ctx, st, r, accts, func(acct state.RetailerAccount, p syncProgress) (any, error) {
			return r.Sync(a, ctx, s, st, acct, p)
		}, nil)
		if err != nil {
			return nil, nil, err
		}
		out[r.ID], errs = views, append(errs, e...)
	}
	return out, errs, nil
}

// readSince is the first day to read from a retailer: a week before the
// epoch set with fin epoch, else a week before the oldest bank transaction
// stored, since nothing older can match one, else two years back. The week
// covers a retailer dating a charge days before the bank does.
func (a *App) readSince(ctx context.Context, s *store.Store, st *state.State) (string, error) {
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

// retailerSyncView is what a retailer's Sync returns for one account.
type retailerSyncView interface {
	// summary is the account, how many things the sync read, and a few words
	// on them, for fin sync's table.
	summary() (account string, read int, detail string)
}

// syncProgress hears what a sync is doing: Step how far each step is, Feed
// each thing it finds. Either may be nil.
type syncProgress struct {
	Step func(step int, detail string)
	Feed func(line string)
}

func (p syncProgress) step(i int, detail string) {
	if p.Step != nil {
		p.Step(i, detail)
	}
}

func (p syncProgress) feed(line string) {
	if p.Feed != nil {
		p.Feed(line)
	}
}

// countProgress is "34/104", a bar, and the time left at the pace so far.
func countProgress(done, total int, took time.Duration) string {
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

// feedLine is one line of the sync screen's feed: date, what, and amount.
func feedLine(day, what, amount string) string {
	return fmt.Sprintf("%s  %-40s %9s", shortDate(day), truncate(what, 40), amount)
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

// --- login ---

// chromeProfile is a Chrome profile, and whether it holds a sign-in at the
// retailer being connected.
type chromeProfile struct {
	Dir      string `json:"dir"`
	Name     string `json:"name"`
	SignedIn bool   `json:"signed_in"`
}

// pickProfile chooses the Chrome profile to import from: the one named, the
// only one signed in to the retailer, or one the person picks in a terminal.
func (a *App) pickProfile(r retailer, profiles []chromeProfile, want string) (chromeProfile, error) {
	notSignedIn := r.code("NOT_SIGNED_IN")
	if want != "" {
		for _, p := range profiles {
			if p.Dir == want || strings.EqualFold(p.Name, want) {
				if !p.SignedIn {
					return p, newErr(notSignedIn, "Chrome profile %q is not signed in to %s; sign in there in that profile, then run this again", p.Name, r.Name)
				}
				return p, nil
			}
		}
		return chromeProfile{}, &CLIError{Code: "NO_SUCH_PROFILE", Message: fmt.Sprintf("Chrome has no profile %q", want),
			Details: map[string]any{"profiles": profiles}, exit: exitError}
	}
	signed := []chromeProfile{}
	for _, p := range profiles {
		if p.SignedIn {
			signed = append(signed, p)
		}
	}
	switch {
	case len(signed) == 0:
		return chromeProfile{}, newErr(notSignedIn, "no Chrome profile is signed in to %s; sign in there in Chrome, then run this again", r.Name)
	case len(signed) == 1:
		return signed[0], nil
	case a.IsTerminal == nil || !a.IsTerminal() || a.ReadLine == nil:
		return chromeProfile{}, &CLIError{Code: "PROFILE_REQUIRED",
			Message: fmt.Sprintf("several Chrome profiles are signed in to %s; pick one with --profile", r.Name),
			Details: map[string]any{"profiles": signed}, exit: exitUsage}
	}
	ui.Print(a.Stderr, fmt.Sprintf("Chrome profiles signed in to %s:\n", r.Name))
	for i, p := range signed {
		ui.Print(a.Stderr, fmt.Sprintf("  %d  %s %s\n", i+1, p.Name, ui.Muted.Render("("+p.Dir+")")))
	}
	line, err := a.ReadLine(fmt.Sprintf("Which one? [1-%d] ", len(signed)))
	if err != nil {
		return chromeProfile{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(signed) {
		return chromeProfile{}, usageErr("pick a number from 1 to %d", len(signed))
	}
	return signed[n-1], nil
}

// --- logout ---

// cmdLogout forgets an account: its sealed session, request log, Keychain
// key, stored rows and state. The browser stays signed in.
func (a *App) cmdLogout(ctx context.Context, r retailer, args []string) (*result, error) {
	if len(args) != 1 {
		return nil, usageErr("usage: fin %s logout <name>", r.ID)
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	acct, ok := st.FindRetailer(r.ID, string(a.Env), args[0])
	if !ok {
		return nil, a.noAccountErr(st, r, args[0])
	}
	for _, path := range []string{a.retailerFile(acct, ".session"), a.retailerFile(acct, ".log")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := a.Secrets.Delete(keyAccount(acct)); err != nil {
		return nil, newErr("KEYCHAIN_ERROR", "%v", err)
	}
	if _, err := os.Stat(a.storePath()); err == nil {
		s, err := a.openStore(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if err := r.Forget(ctx, s, acct.Name); err != nil {
			return nil, storeErr(err)
		}
	}
	st.RemoveRetailer(r.ID, acct.Env, acct.Name)
	if err := a.saveState(st); err != nil {
		return nil, err
	}
	msg := ui.Line(ui.Good, "Removed "+acct.Name+", its saved sign-in, and everything read from "+r.Name) +
		ui.Muted.Render("\n  Your Chrome stays signed in to "+r.Name+".")
	return &result{body: map[string]any{"removed": acct.Name}, message: msg}, nil
}

// --- categorize ---

type categoryInput struct {
	Item     string `json:"item"`
	Category string `json:"category"`
	Detailed string `json:"detailed"`
	Product  bool   `json:"product"`
}

// cmdCategorize lists a retailer's items without a category, or saves
// categories for them: one from the arguments, or a JSON list on stdin.
func (a *App) cmdCategorize(ctx context.Context, r retailer, args []string) (*result, error) {
	fs := flag.NewFlagSet(r.ID+" categorize", flag.ContinueOnError)
	set := fs.Bool("set", false, "save categories: ITEM CATEGORY [DETAILED], or a JSON list on stdin")
	all := fs.Bool("all", false, "list categorized items too")
	product := fs.Bool("product", false, "with --set ITEM CATEGORY, also use it for later purchases of the same product")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	usage := fmt.Sprintf("usage: fin %s categorize [--all] | --set ITEM CATEGORY [DETAILED] [--product]", r.ID)
	if !*set {
		if len(pos) > 0 {
			return nil, usageErr("%s", usage)
		}
		// Opened for writing so a database from an older fin gets the views
		// this one reads.
		s, err := a.openStore(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		items, err := s.RetailerItems(ctx, r.ID, store.ItemFilter{Uncategorized: !*all})
		if err != nil {
			return nil, storeErr(err)
		}
		t := &ui.Table{
			Title:   fmt.Sprintf("%s items · %d", r.Name, len(items)),
			Headers: []string{"Item", "Date", "Title", "Qty", "Cost", "Category"},
			Right:   []int{3, 4},
			Footer:  fmt.Sprintf("Set one with fin %s categorize --set ITEM CATEGORY [--product].", r.ID),
		}
		for _, it := range items {
			t.Rows = append(t.Rows, []string{it.Item, it.Date, truncate(it.Title, 48), strconv.FormatFloat(it.Quantity, 'f', -1, 64),
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
			return nil, usageErr(`--set reads a JSON list on stdin: [{"item": "…", "category": "HOME_IMPROVEMENT", "detailed": "…", "product": true}]`)
		}
	case 2, 3:
		in := categoryInput{Item: pos[0], Category: pos[1], Product: *product}
		if len(pos) == 3 {
			in.Detailed = pos[2]
		}
		inputs = []categoryInput{in}
	default:
		return nil, usageErr("%s", usage)
	}
	changes := []store.CategoryChange{}
	for _, in := range inputs {
		cat, det := strings.ToUpper(strings.TrimSpace(in.Category)), strings.ToUpper(strings.TrimSpace(in.Detailed))
		if !slices.Contains(pfcPrimary, cat) {
			return nil, &CLIError{Code: "UNKNOWN_CATEGORY", Message: fmt.Sprintf("%q is not one of Plaid's categories", in.Category),
				Details: map[string]any{"categories": pfcPrimary}, exit: exitUsage}
		}
		if det != "" && !strings.HasPrefix(det, cat+"_") {
			return nil, usageErr("detailed category %q must start with %s_", in.Detailed, cat)
		}
		changes = append(changes, store.CategoryChange{Item: strings.TrimSpace(in.Item), Category: cat, CategoryDetailed: det, Product: in.Product})
	}
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := s.SetCategories(ctx, r.ID, changes, a.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNoSuchItem) {
			return nil, newErr("NO_SUCH_ITEM", "%v; list items with fin %s categorize", err, r.ID)
		}
		return nil, storeErr(err)
	}
	left, err := s.RetailerItems(ctx, r.ID, store.ItemFilter{Uncategorized: true})
	if err != nil {
		return nil, storeErr(err)
	}
	msg := ui.Line(ui.Good, fmt.Sprintf("Saved %d categories", len(changes))) + ui.Muted.Render(fmt.Sprintf("  %d items left to categorize", len(left)))
	return &result{body: map[string]any{"saved": len(changes), "uncategorized": len(left)}, message: msg}, nil
}

// cmdRetailer dispatches the commands every retailer shares.
func (a *App) cmdRetailer(ctx context.Context, r retailer, args []string, list, login, sync, profiles command) (*result, error) {
	sub, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "", "list":
		return list(ctx, rest)
	case "login":
		return login(ctx, rest)
	case "sync":
		return sync(ctx, rest)
	case "categorize":
		return a.cmdCategorize(ctx, r, rest)
	case "logout":
		return a.cmdLogout(ctx, r, rest)
	case "profiles":
		if profiles != nil {
			return profiles(ctx, rest)
		}
	}
	return nil, usageErr("unknown subcommand %q; run fin help %s", sub, r.ID)
}

// connectedAccounts selects one named account, or all of a retailer's.
func (a *App) connectedAccounts(st *state.State, r retailer, name string) ([]state.RetailerAccount, error) {
	accts := st.RetailerAccounts(r.ID, string(a.Env))
	if name != "" {
		acct, ok := st.FindRetailer(r.ID, string(a.Env), name)
		if !ok {
			return nil, a.noAccountErr(st, r, name)
		}
		accts = []state.RetailerAccount{acct}
	}
	if len(accts) == 0 {
		return nil, newErr("NO_"+r.code("ACCOUNT"), "no %s account is connected; run fin %s login <name>", r.Name, r.ID)
	}
	return accts, nil
}

// chromeProfiles lists profile names and asks the retailer whether each has
// a sign-in. It does not decrypt credentials or contact the website.
func (a *App) chromeProfiles(r retailer, signed func(chrome.Chrome, string) (bool, error)) ([]chromeProfile, error) {
	profiles, err := a.Chrome.Profiles()
	if errors.Is(err, chrome.ErrNoChrome) {
		return nil, newErr("NO_CHROME", "fin %s reads your %s sign-in from Google Chrome, which is not set up on this Mac", r.ID, r.Name)
	}
	if err != nil {
		return nil, newErr("CHROME_ERROR", "%v", err)
	}
	out := make([]chromeProfile, len(profiles))
	for i, p := range profiles {
		ok, err := signed(a.Chrome, p.Dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, newErr("CHROME_ERROR", "%v", err)
		}
		out[i] = chromeProfile{Dir: p.Dir, Name: p.Name, SignedIn: ok}
	}
	return out, nil
}

// loginAccount parses a retailer login and selects its local Chrome profile.
// The caller verifies the session before saving this account or its state.
func (a *App) loginAccount(r retailer, args []string, profiles func() ([]chromeProfile, error)) (*state.State, state.RetailerAccount, chromeProfile, bool, error) {
	var acct state.RetailerAccount
	var profile chromeProfile
	fs := flag.NewFlagSet(r.ID+" login", flag.ContinueOnError)
	want := fs.String("profile", "", "the Chrome profile, by directory or name")
	noSync := fs.Bool("no-sync", false, "connect without reading history yet")
	pos, err := parseArgs(fs, args)
	if err == nil && len(pos) != 1 {
		err = usageErr("usage: fin %s login <name> [--profile P]", r.ID)
	}
	if err != nil {
		return nil, acct, profile, false, err
	}
	name := strings.ToLower(pos[0])
	if !accountName.MatchString(name) {
		return nil, acct, profile, false, usageErr("a %s account name is lowercase letters, digits and dashes, such as home", r.Name)
	}
	available, err := profiles()
	if err != nil {
		return nil, acct, profile, false, err
	}
	profile, err = a.pickProfile(r, available, *want)
	if err != nil {
		return nil, acct, profile, false, err
	}
	st, err := a.loadState()
	if err != nil {
		return nil, acct, profile, false, err
	}
	acct, exists := st.FindRetailer(r.ID, string(a.Env), name)
	if !exists {
		acct = state.RetailerAccount{Retailer: r.ID, Name: name, Env: string(a.Env), ConnectedAt: a.Now().UTC()}
	}
	acct.Profile, acct.ProfileName = profile.Dir, profile.Name
	return st, acct, profile, *noSync, nil
}

// loginErr explains why a retailer turned down a sign-in at login. A rate
// limit starts the cooldown, as in a sync, so logging in again cannot skip
// it; nothing else is recorded for an account that is not saved yet. s may
// be nil.
func (a *App) loginErr(ctx context.Context, s *store.Store, r retailer, acct state.RetailerAccount, profile string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, r.SignIn):
		return newErr(r.code("NOT_SIGNED_IN"), "%s did not accept the sign-in from Chrome profile %q; sign in at %s in that profile and run this again", r.Name, profile, r.Site)
	case errors.Is(err, pace.ErrRateLimited):
		if s == nil {
			opened, oerr := a.openStore(ctx)
			if oerr != nil {
				return oerr
			}
			defer opened.Close()
			s = opened
		}
		return a.syncErr(ctx, s, acct, err)
	}
	return newErr(r.code("ERROR"), "%v", err)
}

// cmdProfiles lists Chrome's profiles and which are signed in to r.
func (a *App) cmdProfiles(r retailer, profiles func() ([]chromeProfile, error), args []string) (*result, error) {
	if len(args) > 0 {
		return nil, usageErr("usage: fin %s profiles", r.ID)
	}
	list, err := profiles()
	if err != nil {
		return nil, err
	}
	t := &ui.Table{Title: "Chrome profiles", Headers: []string{"Profile", "Name", r.Name}}
	for _, p := range list {
		signed := ""
		if p.SignedIn {
			signed = "signed in"
		}
		t.Rows = append(t.Rows, []string{p.Dir, p.Name, signed})
	}
	return &result{body: map[string]any{"profiles": list}, table: t}, nil
}

// runLogin presents a retailer's sign-in steps in the terminal or plain
// output, translating cancellation in either form the same way.
func (a *App) runLogin(ctx context.Context, r retailer, subtitle string, steps []string, work func(context.Context, ui.Reporter) (ui.FlowResult, error)) error {
	var err error
	if a.showSpinner() {
		a.onScreen = true
		_, err = ui.Flow(ctx, a.Stdin, a.Stderr, ui.FlowOptions{Title: "fin → " + r.Name, Subtitle: subtitle, Steps: steps}, work)
		a.onScreen = false
	} else {
		var rep ui.Reporter = ui.PlainReporter{W: a.Stderr, Steps: steps}
		if !a.human {
			rep = linkOnlyReporter{a}
		}
		_, err = work(ctx, rep)
	}
	if errors.Is(err, ui.ErrCancelled) || errors.Is(err, context.Canceled) {
		return newErr("CANCELLED", "cancelled")
	}
	return err
}

// retailerGate keeps the pace across login, syncs and named accounts in
// this process, so a fresh client does not skip the next wait.
func (a *App) retailerGate(r retailer, wait time.Duration) *pace.Gate {
	if a.retailerGates == nil {
		a.retailerGates = map[string]*pace.Gate{}
	}
	if g := a.retailerGates[r.ID]; g != nil {
		return g
	}
	g := &pace.Gate{Wait: wait}
	a.retailerGates[r.ID] = g
	return g
}

// listRetailer reads a retailer's own summaries and presents the shared
// account metadata. row supplies only the retailer-specific counts.
func listRetailer[T any](a *App, ctx context.Context, r retailer, args []string,
	read func(*store.Store, context.Context) (map[string]T, error),
	headers []string, right []int, row func(state.RetailerAccount, T) (any, []string)) (*result, error) {
	if len(args) > 0 {
		return nil, usageErr("usage: fin %s", r.ID)
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	accts := st.RetailerAccounts(r.ID, string(a.Env))
	sums := map[string]T{}
	if len(accts) > 0 {
		// Writing updates tables and views in databases from older fin builds.
		s, err := a.openStore(ctx)
		if err != nil {
			return nil, err
		}
		sums, err = read(s, ctx)
		s.Close()
		if err != nil {
			return nil, storeErr(err)
		}
	}
	views := []any{}
	t := &ui.Table{Title: fmt.Sprintf("%s · %d accounts · experimental", r.Name, len(accts)), Headers: headers, Right: right,
		Footer: fmt.Sprintf("Connect another with fin %s login <name>.", r.ID)}
	for _, acct := range accts {
		view, cells := row(acct, sums[acct.Name])
		views = append(views, view)
		t.Rows = append(t.Rows, cells)
	}
	body := map[string]any{"env": a.Env, "accounts": views}
	if len(accts) == 0 {
		return &result{body: body, message: fmt.Sprintf("No %s account is connected. Connect one with fin %s login <name>.", r.Name, r.ID)}, nil
	}
	return &result{body: body, table: t}, nil
}
