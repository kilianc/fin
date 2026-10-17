package store

import (
	"context"
	"database/sql"
)

// Reads for fin's local pages (fin ui). They work on a read-only store, so
// a table an older fin never created reads as empty instead of failing.

// DayValue is one key's value on one day: an account's balance, or the
// value of an Item's holdings.
type DayValue struct {
	Date  string  `json:"date"`
	Key   string  `json:"key"`
	Value float64 `json:"value"`
}

// BalanceHistory lists the daily balances kept for the given Items' accounts,
// oldest first. Credit and loan balances are what is owed, so they come out
// negative: each value is what the account adds to net worth. A day Plaid
// reported no current balance comes out NaN, so the account stops counting
// from that day, as it does in the accounts table.
func (s *Store) BalanceHistory(ctx context.Context, itemIDs []string) ([]DayValue, error) {
	if ok, err := s.hasTable(ctx, "balances"); err != nil || !ok || len(itemIDs) == 0 {
		return []DayValue{}, err
	}
	return s.dayValues(ctx, `
		select b.date::varchar, b.account_id,
			coalesce(case when a.type in ('credit', 'loan') then -b.current else b.current end::double, 'nan'::double)
		from balances b left join accounts a using (account_id)
		where b.item_id in (select unnest(?::varchar[]))
		order by b.date, b.account_id`, itemIDs)
}

// HoldingsHistory is the value of each Item's holdings on every day a
// snapshot was kept, oldest first.
func (s *Store) HoldingsHistory(ctx context.Context, itemIDs []string, account string) ([]DayValue, error) {
	if ok, err := s.hasTable(ctx, "holdings"); err != nil || !ok || len(itemIDs) == 0 {
		return []DayValue{}, err
	}
	return s.dayValues(ctx, `
		select h.date::varchar, h.item_id, sum(h.value)
		from holdings h left join accounts a using (account_id)
		where h.item_id in (select unnest(?::varchar[]))
			and (? = '' or h.account_id = ? or a.mask = ? or lower(a.name) = lower(?))
		group by all
		order by 1, 2`, itemIDs, account, account, account, account)
}

func (s *Store) dayValues(ctx context.Context, query string, args ...any) ([]DayValue, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayValue{}
	for rows.Next() {
		var v DayValue
		if err := rows.Scan(&v.Date, &v.Key, &v.Value); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CashEquivalents reports which securities the institution calls cash
// equivalents, such as money market funds.
func (s *Store) CashEquivalents(ctx context.Context) (map[string]bool, error) {
	if ok, err := s.hasTable(ctx, "securities"); err != nil || !ok {
		return map[string]bool{}, err
	}
	rows, err := s.db.QueryContext(ctx, `select security_id from securities where is_cash_equivalent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PaidItem is one retailer item a bank transaction paid for.
type PaidItem struct {
	TransactionID string  `json:"-"`
	Retailer      string  `json:"retailer"`
	OrderID       string  `json:"order_id"`
	Title         string  `json:"title"`
	Quantity      float64 `json:"quantity"`
	Cost          float64 `json:"cost"`
	Category      *string `json:"category"`
}

// ItemsPaidBy lists the retailer items each of the given bank transactions
// paid for, in order.
func (s *Store) ItemsPaidBy(ctx context.Context, transactionIDs []string) ([]PaidItem, error) {
	if ok, err := s.hasTable(ctx, "retailer_items"); err != nil || !ok || len(transactionIDs) == 0 {
		return []PaidItem{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		select transaction_id, retailer, order_id, title, quantity, cost::double, category
		from retailer_items
		where transaction_id in (select unnest(?::varchar[]))
		order by transaction_id, order_id, line`, transactionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaidItem{}
	for rows.Next() {
		var it PaidItem
		if err := rows.Scan(&it.TransactionID, &it.Retailer, &it.OrderID, &it.Title, &it.Quantity, &it.Cost, &it.Category); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// hasTable reports whether the store has a table or view by that name.
func (s *Store) hasTable(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from information_schema.tables where table_name = ?`, name).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return n > 0, err
}
