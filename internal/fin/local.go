package fin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

// storePath is the DuckDB file for the current environment.
func (a *App) storePath() string {
	return store.Path(a.DataDir, string(a.Env))
}

func (a *App) openStore(ctx context.Context) (*store.Store, error) {
	s, err := store.Open(ctx, a.storePath())
	if err != nil {
		return nil, storeErr(err)
	}
	// Keep the epoch in the database too, so queries can use it.
	if st, err := a.loadState(); err == nil {
		if err := s.SetSetting(ctx, "epoch", st.Epoch); err != nil {
			s.Close()
			return nil, storeErr(err)
		}
	}
	return s, nil
}

// --- epoch ---

func (a *App) cmdEpoch(ctx context.Context, args []string) (*result, error) {
	if len(args) > 1 {
		return nil, usageErr("usage: fin epoch [YYYY-MM-DD|none]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	if len(args) == 1 {
		if args[0] == "none" {
			st.Epoch = ""
		} else if st.Epoch, err = parseDate("epoch", args[0]); err != nil {
			return nil, err
		}
		if err := a.saveState(st); err != nil {
			return nil, err
		}
		if _, err := os.Stat(a.storePath()); err == nil {
			s, err := a.openStore(ctx)
			if err != nil {
				return nil, err
			}
			s.Close()
		}
	}
	body := map[string]any{"epoch": nil}
	msg := "No epoch: fin reports on everything it has. Set one with fin epoch YYYY-MM-DD."
	if st.Epoch != "" {
		body["epoch"] = st.Epoch
		msg = "Epoch: " + ui.Bold.Render(st.Epoch) + ui.Muted.Render("  fin sheet and fin amazon start here; older data stays in the database")
	}
	return &result{body: body, message: msg}, nil
}

func storeErr(err error) error {
	switch {
	case errors.Is(err, store.ErrBusy):
		return newErr("STORE_BUSY", "%v; wait for it to finish and try again", err)
	case errors.Is(err, store.ErrNotFound):
		return newErr("NO_LOCAL_DATA", "nothing has been synced in this environment yet; run fin sync first")
	case errors.Is(err, context.Canceled):
		return err
	}
	return newErr("STORE_ERROR", "%v", err)
}

type syncView struct {
	Item                     string     `json:"item"`
	TransactionsUpdateStatus string     `json:"transactions_update_status"`
	Changed                  int        `json:"changed"`
	Removed                  int        `json:"removed"`
	LastSync                 *time.Time `json:"last_sync"`
}

// syncStore pulls each Item's /transactions/sync delta since the cursor saved
// in the store, then writes the deltas one Item at a time. Fetches run in
// parallel; writes do not, so DuckDB never sees two writers.
func (a *App) syncStore(ctx context.Context, s *store.Store, st *state.State, items []state.Item, api Plaid) ([]syncView, []ItemError, error) {
	cursors := make([]string, len(items))
	for i, it := range items {
		c, err := s.Cursor(ctx, it.ItemID)
		if err != nil {
			return nil, nil, storeErr(err)
		}
		cursors[i] = c
	}
	results := make([]*syncResult, len(items))
	errs := a.forEachItem(ctx, items, func(ctx context.Context, i int, token string) error {
		res, err := syncTransactions(ctx, api, token, cursors[i])
		if err != nil {
			return err
		}
		if res.status == "NOT_READY" {
			return &itemIssue{"TRANSACTIONS_NOT_READY", "Plaid is still pulling this Item's transactions; try again in a few minutes"}
		}
		results[i] = res
		return nil
	})

	syncs := []syncView{}
	for i, res := range results {
		if res == nil {
			continue
		}
		it := items[i]
		now := a.Now().UTC()
		d := store.ItemSync{
			ItemID: it.ItemID, Item: it.Name, Institution: it.InstitutionName,
			Cursor: res.cursor, Status: res.status, SyncedAt: now,
		}
		for _, acc := range res.accounts {
			d.Accounts = append(d.Accounts, acc)
		}
		for _, t := range res.transactions {
			d.Upserts = append(d.Upserts, t)
		}
		for id := range res.removed {
			d.Removed = append(d.Removed, id)
		}
		if err := s.Apply(ctx, d); err != nil {
			return nil, nil, storeErr(err)
		}
		it.LastSync = &now
		st.Put(it)
		syncs = append(syncs, syncView{
			Item: it.Name, TransactionsUpdateStatus: res.status,
			Changed: len(d.Upserts), Removed: len(d.Removed), LastSync: &now,
		})
	}
	if len(syncs) > 0 {
		if err := a.saveState(st); err != nil {
			return nil, nil, err
		}
	}
	return syncs, errs, nil
}

// --- sync ---

func (a *App) cmdSync(ctx context.Context, args []string) (*result, error) {
	if len(args) > 0 {
		return nil, usageErr("usage: fin sync")
	}
	st, items, api, err := a.readSetup("transactions")
	if err != nil {
		return nil, err
	}
	invItems := a.itemsWith(st, "investments")
	if api == nil && len(invItems) > 0 {
		if api, err = a.plaid(); err != nil {
			return nil, err
		}
	}
	syncs, invs, errs, shops := []syncView{}, []investmentsView{}, []ItemError{}, map[string][]any{}
	if len(items) > 0 || len(invItems) > 0 || len(st.Retailers) > 0 {
		s, err := a.openStore(ctx)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if len(items) > 0 {
			if syncs, errs, err = a.syncStore(ctx, s, st, items, api); err != nil {
				return nil, err
			}
		}
		if len(invItems) > 0 {
			var invErrs []ItemError
			if invs, invErrs, err = a.syncInvestments(ctx, s, invItems, api); err != nil {
				return nil, err
			}
			errs = append(errs, invErrs...)
		}
		var shopErrs []ItemError
		if shops, shopErrs, err = a.syncRetailers(ctx, s, st); err != nil {
			return nil, err
		}
		errs = append(errs, shopErrs...)
	}
	t := &ui.Table{
		Title:   fmt.Sprintf("Synced · %d of %d Items", len(syncs), len(items)),
		Headers: []string{"Item", "Changed", "Removed", "Status"},
		Right:   []int{1, 2},
		Footer:  "Stored in " + a.storePath() + ". Query it with fin sql.",
		Tone: func(row, col int) ui.Tone {
			if col == 3 {
				return ui.Dim
			}
			return ui.Plain
		},
	}
	for _, v := range syncs {
		status := strings.ToLower(strings.ReplaceAll(v.TransactionsUpdateStatus, "_", " "))
		t.Rows = append(t.Rows, []string{v.Item, strconv.Itoa(v.Changed), strconv.Itoa(v.Removed), status})
	}
	for _, v := range invs {
		t.Rows = append(t.Rows, []string{v.Item + " investments", strconv.Itoa(v.Transactions), "0", fmt.Sprintf("%d holdings", v.Holdings)})
	}
	body := map[string]any{"env": a.Env, "store": a.storePath(), "sync": syncs, "investments": invs, "errors": errs}
	for _, r := range retailers {
		for _, v := range shops[r.ID] {
			account, changed, status := v.(retailerSyncView).summary()
			t.Rows = append(t.Rows, []string{r.ID + "/" + account, strconv.Itoa(changed), "0", status})
		}
		if views, ok := shops[r.ID]; ok {
			body[r.ID] = views
		}
	}
	return &result{body: body, table: t, errors: errs}, nil
}

// --- paths ---

func (a *App) cmdPaths(ctx context.Context, args []string) (*result, error) {
	if len(args) > 1 || (len(args) == 1 && args[0] != "database") {
		return nil, usageErr("usage: fin paths [database]")
	}
	db := a.storePath()
	if len(args) == 1 {
		return &result{raw: db}, nil
	}
	_, err := os.Stat(db)
	exists := err == nil
	body := map[string]any{
		"env": a.Env, "state": a.StatePath, "data_dir": a.DataDir,
		"database": db, "database_exists": exists, "keychain_service": "fin",
	}
	dbLine := db
	if !exists {
		dbLine += ui.Muted.Render("  (not created yet; fin sync creates it)")
	}
	msg := fmt.Sprintf("%-9s %s\n%-9s %s\n%-9s %s",
		"state", a.StatePath, "database", dbLine, "keychain", ui.Muted.Render(`service "fin" in your login Keychain`))
	return &result{body: body, message: msg}, nil
}

// --- sql ---

func (a *App) cmdSQL(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("sql", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		return nil, usageErr(`usage: fin sql "select ..."`)
	}
	s, err := store.OpenReadOnly(ctx, a.storePath())
	if err != nil {
		return nil, storeErr(err)
	}
	defer s.Close()
	res, err := s.Query(ctx, query)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, newErr("SQL_ERROR", "%v", err)
	}
	synced, err := s.Syncs(ctx)
	if err != nil {
		return nil, storeErr(err)
	}

	columns := uniqueNames(res.Columns)
	rows := make([]map[string]any, len(res.Rows))
	t := &ui.Table{
		Title:   fmt.Sprintf("%d rows", len(res.Rows)),
		Headers: columns,
		Footer:  "From " + a.storePath() + lastSyncNote(synced),
	}
	if len(res.Rows) == 1 {
		t.Title = "1 row"
	}
	for i, typ := range res.Types {
		if numericType(typ) {
			t.Right = append(t.Right, i)
		}
	}
	for r, vals := range res.Rows {
		rows[r] = make(map[string]any, len(columns))
		cells := make([]string, len(vals))
		for c, v := range vals {
			rows[r][columns[c]] = v
			cells[c] = cellText(v)
		}
		t.Rows = append(t.Rows, cells)
	}
	body := map[string]any{"env": a.Env, "columns": columns, "rows": rows, "synced": synced}
	return &result{body: body, table: t}, nil
}

// uniqueNames suffixes repeated column names (a, a_2) so every value has a
// key in the JSON row objects.
func uniqueNames(names []string) []string {
	out := make([]string, len(names))
	seen := map[string]int{}
	for i, n := range names {
		seen[n]++
		out[i] = n
		for k := seen[n]; k > 1; k++ {
			candidate := n + "_" + strconv.Itoa(k)
			if !slices.Contains(names, candidate) && !slices.Contains(out[:i], candidate) {
				out[i] = candidate
				break
			}
		}
	}
	return out
}

func numericType(t string) bool {
	switch {
	case strings.HasPrefix(t, "DECIMAL"), strings.HasSuffix(t, "INT"), t == "BIGINT", t == "HUGEINT",
		t == "DOUBLE", t == "FLOAT", t == "INTEGER", t == "SMALLINT", t == "TINYINT":
		return true
	}
	return false
}

func cellText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	}
	return fmt.Sprint(v)
}

func lastSyncNote(synced []store.SyncInfo) string {
	var oldest time.Time
	for _, si := range synced {
		if oldest.IsZero() || si.LastSync.Before(oldest) {
			oldest = si.LastSync
		}
	}
	if oldest.IsZero() {
		return ""
	}
	return ", synced " + fmtTime(&oldest) + ". Run fin sync to refresh."
}
