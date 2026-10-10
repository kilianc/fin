package fin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/store"
	"github.com/kilianc/fin/internal/ui"
)

type unlinkBody struct {
	Env         string `json:"env"`
	Item        string `json:"item"`
	ItemID      string `json:"item_id"`
	Institution string `json:"institution"`
	// RemovedAtPlaid is false when fin had no access token to remove the
	// Item with; it then only forgets it locally.
	RemovedAtPlaid bool `json:"removed_at_plaid"`
	SlotsUsed      int  `json:"slots_used"`
	SlotsTotal     int  `json:"slots_total"`
}

// cmdUnlink removes a connection at Plaid and forgets its token and synced
// data. Plaid's limit counts the Item anyway, so its slot stays used.
func (a *App) cmdUnlink(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("unlink", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usageErr("usage: fin unlink <item> [--yes]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	it, err := a.findItem(st, pos[0])
	if err != nil {
		return nil, err
	}
	if !*yes {
		msg := fmt.Sprintf("Unlinking %s (%s) ends the connection at Plaid and deletes its transactions and balances from fin. It cannot be undone.", it.Name, it.InstitutionName)
		if a.Env == plaid.Production {
			msg += fmt.Sprintf(" Its slot stays used (%d of %d).", st.SlotsUsed(string(a.Env)), SlotsTotal)
		}
		if err := a.confirm(msg); err != nil {
			return nil, err
		}
	}
	// Open the local file first, so a busy file stops us before anything
	// irreversible happens at Plaid.
	var s *store.Store
	if _, err := os.Stat(a.storePath()); err == nil {
		if s, err = a.openStore(ctx); err != nil {
			return nil, err
		}
		defer s.Close()
	}
	body := unlinkBody{Env: string(a.Env), Item: it.Name, ItemID: it.ItemID, Institution: it.InstitutionName, SlotsTotal: SlotsTotal}
	account := tokenAccount(a.Env, it.ItemID)
	token, err := a.Secrets.Get(account)
	switch {
	case err == nil:
		api, err := a.plaid()
		if err != nil {
			return nil, err
		}
		if err := api.ItemRemove(ctx, token); err != nil && !plaid.IsCode(err, "ITEM_NOT_FOUND") && !plaid.IsCode(err, "INVALID_ACCESS_TOKEN") {
			e := toItemError(it, err)
			return nil, newErr("UNLINK_FAILED", "Plaid did not remove %s (%s: %s); nothing was changed", it.Name, e.Code, e.Message)
		}
		body.RemovedAtPlaid = true
	case !errors.Is(err, keychain.ErrNotFound):
		return nil, err
	}
	if s != nil {
		if err := s.DeleteItem(ctx, it.ItemID); err != nil {
			return nil, storeErr(err)
		}
	}
	if err := a.Secrets.Delete(account); err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return nil, err
	}
	st.Unlink(string(a.Env), it.ItemID, a.Now())
	if err := a.saveState(st); err != nil {
		return nil, err
	}
	body.SlotsUsed = st.SlotsUsed(string(a.Env))
	msg := ui.Line(ui.Good, fmt.Sprintf("Unlinked %s (%s)", it.Name, it.InstitutionName))
	if !body.RemovedAtPlaid {
		msg = ui.Line(ui.Warn, fmt.Sprintf("Forgot %s (%s); with no access token, fin could not end it at Plaid", it.Name, it.InstitutionName))
	}
	if a.Env == plaid.Production {
		msg += "\n" + ui.Muted.Render(fmt.Sprintf("%d of %d slots used; Plaid still counts this one", body.SlotsUsed, SlotsTotal))
	}
	return &result{body: body, message: msg}, nil
}
