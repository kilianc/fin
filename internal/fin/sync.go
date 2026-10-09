package fin

import (
	"context"
	"fmt"

	"github.com/kilianc/fin/internal/plaid"
)

const (
	syncPageSize = 500
	// maxSyncRestarts bounds how often a pagination loop restarts after
	// Plaid reports that data changed underneath it.
	maxSyncRestarts = 3
)

type syncResult struct {
	transactions map[string]plaid.Transaction
	accounts     map[string]plaid.Account
	cursor       string
	status       string
}

// syncTransactions pages /transactions/sync from cursor, applying added,
// modified and removed in order. If Plaid reports that data changed during
// pagination, the whole loop restarts from the original cursor and discards
// the partial pages, as Plaid requires.
func syncTransactions(ctx context.Context, api Plaid, token, cursor string) (*syncResult, error) {
	for restarts := 0; ; restarts++ {
		res, err := syncFrom(ctx, api, token, cursor)
		if plaid.IsCode(err, "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION") && restarts < maxSyncRestarts {
			continue
		}
		return res, err
	}
}

func syncFrom(ctx context.Context, api Plaid, token, cursor string) (*syncResult, error) {
	res := &syncResult{
		transactions: map[string]plaid.Transaction{},
		accounts:     map[string]plaid.Account{},
		cursor:       cursor,
	}
	for {
		page, err := api.TransactionsSync(ctx, token, res.cursor, syncPageSize)
		if err != nil {
			return nil, err
		}
		for _, t := range page.Added {
			res.transactions[t.TransactionID] = t
		}
		for _, t := range page.Modified {
			res.transactions[t.TransactionID] = t
		}
		for _, r := range page.Removed {
			delete(res.transactions, r.TransactionID)
		}
		for _, acc := range page.Accounts {
			res.accounts[acc.AccountID] = acc
		}
		res.status = page.TransactionsUpdateStatus
		if !page.HasMore {
			if page.NextCursor != "" {
				res.cursor = page.NextCursor
			}
			return res, nil
		}
		if page.NextCursor == "" || page.NextCursor == res.cursor {
			return nil, fmt.Errorf("transactions/sync: has_more without a new cursor")
		}
		res.cursor = page.NextCursor
	}
}
