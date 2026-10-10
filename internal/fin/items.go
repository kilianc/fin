package fin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/ui"
)

// reconnectCodes are Item errors that only the user can fix, by signing in
// again through update mode.
var reconnectCodes = map[string]bool{
	"ITEM_LOGIN_REQUIRED": true,
	"PENDING_EXPIRATION":  true,
	"PENDING_DISCONNECT":  true,
}

// itemIssue is a per-Item problem that is not a Plaid API error.
type itemIssue struct {
	code    string
	message string
}

func (e *itemIssue) Error() string { return e.code + ": " + e.message }

func reconnectAction(it state.Item) string { return "run fin reconnect " + it.Name }

func toItemError(it state.Item, err error) ItemError {
	ie := ItemError{Item: it.Name, ItemID: it.ItemID, Institution: it.InstitutionName}
	var perr *plaid.Error
	var issue *itemIssue
	switch {
	case errors.As(err, &perr):
		ie.Code = perr.Code
		ie.Message = perr.Message
		if perr.DisplayMessage != "" {
			ie.Message = perr.DisplayMessage
		}
		if reconnectCodes[perr.Code] {
			ie.Action = reconnectAction(it)
			ie.Message = fmt.Sprintf("%s needs you to sign in again; run `fin reconnect %s`", it.InstitutionName, it.Name)
		}
	case errors.As(err, &issue):
		ie.Code = issue.code
		ie.Message = issue.message
	case errors.Is(err, keychain.ErrNotFound):
		ie.Code = "ACCESS_TOKEN_MISSING"
		ie.Message = "no access token in the Keychain for this Item; link it again"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		ie.Code = "CANCELLED"
		ie.Message = err.Error()
	default:
		ie.Code = "REQUEST_FAILED"
		ie.Message = err.Error()
	}
	return ie
}

// forEachItem runs fn concurrently for every Item, giving it the Item's index
// and access token. One Item failing never stops the others; failures come
// back as ItemErrors in Item order.
func (a *App) forEachItem(ctx context.Context, items []state.Item, fn func(ctx context.Context, i int, token string) error) []ItemError {
	errs := make([]error, len(items))
	run := func(ctx context.Context) error {
		var wg sync.WaitGroup
		for i, it := range items {
			wg.Go(func() {
				token, err := a.Secrets.Get(tokenAccount(a.Env, it.ItemID))
				if err == nil {
					err = fn(ctx, i, token)
				}
				errs[i] = err
			})
		}
		wg.Wait()
		return nil
	}
	if a.showSpinner() {
		_ = ui.Wait(ctx, a.Stdin, a.Stderr, fmt.Sprintf("Asking Plaid about %d Items…", len(items)), run)
	} else {
		_ = run(ctx)
	}
	out := []ItemError{}
	for i, err := range errs {
		if err != nil {
			out = append(out, toItemError(items[i], err))
		}
	}
	return out
}

// itemsWith returns the Items in the current env linked with product, or all
// of them when product is empty.
func (a *App) itemsWith(st *state.State, product string) []state.Item {
	items := []state.Item{}
	for _, it := range st.ForEnv(string(a.Env)) {
		if product == "" || it.HasProduct(product) {
			items = append(items, it)
		}
	}
	return items
}

func (a *App) findItem(st *state.State, key string) (state.Item, error) {
	if it, ok := st.Find(string(a.Env), key); ok {
		return it, nil
	}
	names := []string{}
	for _, it := range st.ForEnv(string(a.Env)) {
		names = append(names, it.Name)
	}
	return state.Item{}, &CLIError{
		Code:    "ITEM_NOT_FOUND",
		Message: fmt.Sprintf("no %s Item named %q", a.Env, key),
		Details: map[string]any{"items": names},
		exit:    exitError,
	}
}

type itemView struct {
	Name                 string                `json:"name"`
	ItemID               string                `json:"item_id"`
	Institution          string                `json:"institution"`
	Kind                 string                `json:"kind"`
	Products             []string              `json:"products"`
	LinkedAt             time.Time             `json:"linked_at"`
	Health               string                `json:"health"`
	Error                *ItemError            `json:"error"`
	Action               string                `json:"action,omitempty"`
	ConsentExpiresAt     *time.Time            `json:"consent_expires_at"`
	LastSuccessfulUpdate map[string]*time.Time `json:"last_successful_update"`
	LastSync             *time.Time            `json:"last_sync"`
}

func newItemView(it state.Item) itemView {
	return itemView{
		Name:                 it.Name,
		ItemID:               it.ItemID,
		Institution:          it.InstitutionName,
		Kind:                 it.Kind,
		Products:             it.Products,
		LinkedAt:             it.LinkedAt,
		Health:               "unknown",
		LastSuccessfulUpdate: map[string]*time.Time{},
		LastSync:             it.LastSync,
	}
}

// applyItemGet fills health fields from /item/get.
func (v *itemView) applyItemGet(it state.Item, ig *plaid.ItemGetResponse) {
	v.ConsentExpiresAt = ig.Item.ConsentExpirationTime
	if s := ig.Status; s != nil {
		if s.Transactions != nil {
			v.LastSuccessfulUpdate["transactions"] = s.Transactions.LastSuccessfulUpdate
		}
		if s.Investments != nil {
			v.LastSuccessfulUpdate["investments"] = s.Investments.LastSuccessfulUpdate
		}
	}
	v.Health = "ok"
	if ig.Item.Error != nil {
		ie := toItemError(it, ig.Item.Error)
		v.Error = &ie
		v.Action = ie.Action
		v.Health = "error"
		if ie.Action != "" {
			v.Health = "needs_reconnect"
		}
	}
}

type itemsBody struct {
	Env        string      `json:"env"`
	SlotsUsed  int         `json:"slots_used"`
	SlotsTotal int         `json:"slots_total"`
	Items      []itemView  `json:"items"`
	Errors     []ItemError `json:"errors"`
}

func (a *App) cmdItems(ctx context.Context, args []string) (*result, error) {
	if len(args) > 0 {
		return nil, usageErr("usage: fin items")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	items := a.itemsWith(st, "")
	views := make([]itemView, len(items))
	for i, it := range items {
		views[i] = newItemView(it)
	}
	var api Plaid
	var errs []ItemError
	if len(items) > 0 {
		if api, err = a.plaid(); err != nil {
			return nil, err
		}
		errs = a.forEachItem(ctx, items, func(ctx context.Context, i int, token string) error {
			ig, err := api.ItemGet(ctx, token)
			if err != nil {
				return err
			}
			views[i].applyItemGet(items[i], ig)
			return nil
		})
		for _, e := range errs {
			for i := range views {
				if views[i].ItemID == e.ItemID && e.Action != "" {
					views[i].Health, views[i].Action = "needs_reconnect", e.Action
				}
			}
		}
	}
	body := itemsBody{
		Env:        string(a.Env),
		SlotsUsed:  st.SlotsUsed(string(a.Env)),
		SlotsTotal: SlotsTotal,
		Items:      views,
		Errors:     nonNil(errs),
	}
	t := itemsTable(views)
	t.Title = fmt.Sprintf("Items · %s", a.Env)
	t.Footer = fmt.Sprintf("%d of %d slots used", st.SlotsUsed(string(a.Env)), SlotsTotal)
	if a.Env == plaid.Sandbox {
		t.Footer = fmt.Sprintf("%d sandbox Items", len(items))
	}
	return &result{body: body, table: t, errors: errs}, nil
}

func itemsTable(views []itemView) *ui.Table {
	t := &ui.Table{Headers: []string{"Name", "Institution", "Products", "Health", "Consent expires", "Last sync"}}
	for _, v := range views {
		health := strings.ReplaceAll(v.Health, "_", " ")
		if v.Action != "" {
			health += " → " + v.Action
		}
		products := slices.Sorted(slices.Values(v.Products))
		t.Rows = append(t.Rows, []string{v.Name, v.Institution, strings.Join(products, ", "), health, fmtDate(v.ConsentExpiresAt), fmtTime(v.LastSync)})
	}
	t.Tone = func(row, col int) ui.Tone {
		if col != 3 {
			return ui.Plain
		}
		return healthTone(views[row].Health)
	}
	return t
}

func healthTone(health string) ui.Tone {
	switch health {
	case "ok":
		return ui.Good
	case "needs_reconnect":
		return ui.Warn
	case "error":
		return ui.Bad
	}
	return ui.Dim
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("Jan 2, 2006")
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("Jan 2, 15:04")
}

func nonNil(errs []ItemError) []ItemError {
	if errs == nil {
		return []ItemError{}
	}
	return errs
}

type institutionView struct {
	InstitutionID string   `json:"institution_id"`
	Name          string   `json:"name"`
	URL           *string  `json:"url"`
	OAuth         bool     `json:"oauth"`
	Products      []string `json:"products"`
	Bank          bool     `json:"bank"`
	Brokerage     bool     `json:"brokerage"`
}

func (a *App) cmdInstitutions(ctx context.Context, args []string) (*result, error) {
	if len(args) == 0 {
		return nil, usageErr("usage: fin institutions <name>")
	}
	query := strings.Join(args, " ")
	api, err := a.plaid()
	if err != nil {
		return nil, err
	}
	found, err := api.InstitutionsSearch(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	views := []institutionView{}
	t := &ui.Table{
		Title:   fmt.Sprintf("Plaid institutions matching %q", query),
		Headers: []string{"Institution", "ID", "Bank", "Brokerage", "OAuth"},
		Footer:  "fin link covers both; use fin link brokerage only when Bank is no.",
		Tone: func(row, col int) ui.Tone {
			if col < 2 {
				return ui.Plain
			}
			if t := views[row]; (col == 2 && t.Bank) || (col == 3 && t.Brokerage) || (col == 4 && t.OAuth) {
				return ui.Good
			}
			return ui.Dim
		},
	}
	for _, inst := range found {
		v := institutionView{
			InstitutionID: inst.InstitutionID,
			Name:          inst.Name,
			URL:           inst.URL,
			OAuth:         inst.OAuth,
			Products:      inst.Products,
			Bank:          slices.Contains(inst.Products, "transactions"),
			Brokerage:     slices.Contains(inst.Products, "investments"),
		}
		views = append(views, v)
		t.Rows = append(t.Rows, []string{v.Name, v.InstitutionID, yesNo(v.Bank), yesNo(v.Brokerage), yesNo(v.OAuth)})
	}
	body := map[string]any{"env": a.Env, "query": query, "institutions": views}
	return &result{body: body, table: t}, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
