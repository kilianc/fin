package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kilianc/fin/internal/costco"
)

const costcoItemSource = `select 'costco' as retailer, i.account,
	i.account || '/' || i.barcode || '#' || i.line as item, i.item_number as product,
	i.barcode as order_id, i.line, r.date, i.title, i.quantity, i.cost, m.transaction_id
from costco_items i join costco_receipts r using (account, barcode)
left join costco_matches m using (account, barcode)`

var costcoSchema = `
create table if not exists costco_receipts (
	account varchar not null,
	barcode varchar not null,
	date date not null,
	transaction_time varchar,
	warehouse_number varchar,
	warehouse_name varchar,
	register_number varchar,
	transaction_number varchar,
	transaction_type varchar,
	subtotal decimal(18, 4) not null,
	tax decimal(18, 4) not null,
	total decimal(18, 4) not null,
	instant_savings decimal(18, 4) not null,
	card_last4 varchar,
	no_bank_charge boolean not null,
	split_tender boolean not null,
	raw json not null,
	parser integer not null,
	fetched_at timestamptz not null,
	primary key (account, barcode)
);
create table if not exists costco_items (
	account varchar not null,
	barcode varchar not null,
	line integer not null,
	item_number varchar,
	title varchar not null,
	quantity double not null,
	unit_price decimal(18, 4) not null,
	amount decimal(18, 4) not null,
	tax_flag varchar,
	department varchar,
	cost decimal(18, 4) not null,
	primary key (account, barcode, line)
);
` + "create or replace view costco_matches as " + retailerMatches(`
	select account || '/' || barcode as receipt_key, account, barcode, date, total, total as amount, card_last4,
		no_bank_charge as no_bank, not split_tender as matchable
	from costco_receipts`, "receipt_key", 3, "costco")

// ApplyCostcoReceipts saves one complete window in a transaction. The raw
// JSON stays with each receipt, and replacing lines leaves categories alone.
func (s *Store) ApplyCostcoReceipts(ctx context.Context, account, profile string, receipts []costco.Receipt, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range receipts {
		if err := writeCostcoReceipt(ctx, tx, account, r, at); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.SetRetailerStatus(ctx, "costco", account, profile, "syncing")
}

func writeCostcoReceipt(ctx context.Context, tx *sql.Tx, account string, r costco.Receipt, at time.Time) error {
	if !json.Valid(r.Raw) {
		return fmt.Errorf("store: costco receipt has no raw JSON")
	}
	if _, err := tx.ExecContext(ctx, `insert or replace into costco_receipts values (?, ?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::json, ?, ?)`,
		account, r.Barcode, r.Date, nullable(r.DateTime), nullable(r.Warehouse), nullable(r.WarehouseName), nullable(r.Register), nullable(r.Transaction), nullable(r.Type),
		r.Subtotal.String(), r.Tax.String(), r.Total.String(), r.Savings.String(), nullable(r.CardLast4), r.NoBankCharge, r.SplitTender, string(r.Raw), costco.ParserVersion, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from costco_items where account = ? and barcode = ?`, account, r.Barcode); err != nil {
		return err
	}
	for _, it := range r.Items {
		if _, err := tx.ExecContext(ctx, `insert into costco_items values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account, r.Barcode, it.Line, nullable(it.Number), it.Title, it.Quantity, it.UnitPrice.String(), it.Amount.String(), nullable(it.TaxFlag), nullable(it.Department), it.Cost.String()); err != nil {
			return err
		}
	}
	return nil
}

// ReparseCostcoReceipts updates receipts written by an older parser using
// only stored JSON. A parser error leaves the previous receipt intact.
func (s *Store) ReparseCostcoReceipts(ctx context.Context, account string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `select raw::varchar, fetched_at from costco_receipts where account = ? and parser < ?`, account, costco.ParserVersion)
	if err != nil {
		return 0, err
	}
	type kept struct {
		raw []byte
		at  time.Time
	}
	var receipts []kept
	for rows.Next() {
		var r kept
		if err := rows.Scan(&r.raw, &r.at); err != nil {
			rows.Close()
			return 0, err
		}
		receipts = append(receipts, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for i, r := range receipts {
		parsed, err := costco.ParseReceipt(r.raw)
		if err != nil {
			return i, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return i, err
		}
		if err := writeCostcoReceipt(ctx, tx, account, *parsed, r.at); err != nil {
			tx.Rollback()
			return i, err
		}
		if err := tx.Commit(); err != nil {
			return i, err
		}
	}
	return len(receipts), nil
}

// CostcoSummary counts receipts, items and matches stored for one account.
type CostcoSummary struct {
	Account       string     `json:"account"`
	Receipts      int        `json:"receipts"`
	Items         int        `json:"items"`
	Uncategorized int        `json:"uncategorized_items"`
	Matched       int        `json:"matched_receipts"`
	Unmatched     int        `json:"unmatched_receipts"`
	Ambiguous     int        `json:"ambiguous_receipts"`
	LastSync      *time.Time `json:"last_sync"`
	Status        string     `json:"status"`
}

func (s *Store) CostcoSummaries(ctx context.Context) (map[string]CostcoSummary, error) {
	rows, err := s.db.QueryContext(ctx, `select a.account, a.last_sync, coalesce(a.status, ''),
		(select count(*) from costco_receipts r where r.account = a.account),
		(select count(*) from costco_items i where i.account = a.account),
		(select count(*) from retailer_items i where i.retailer = 'costco' and i.account = a.account and i.category is null),
		(select count(*) from costco_matches m where m.account = a.account and m.match = 'exact'),
		(select count(*) from costco_matches m where m.account = a.account and m.match = 'unmatched'),
		(select count(*) from costco_matches m where m.account = a.account and m.match = 'ambiguous')
		from retailer_accounts a where a.retailer = 'costco'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CostcoSummary{}
	for rows.Next() {
		var r CostcoSummary
		if err := rows.Scan(&r.Account, &r.LastSync, &r.Status, &r.Receipts, &r.Items, &r.Uncategorized, &r.Matched, &r.Unmatched, &r.Ambiguous); err != nil {
			return nil, err
		}
		out[r.Account] = r
	}
	return out, rows.Err()
}

// DeleteCostcoAccount forgets an account's receipts and the shared rows
// that refer to their items, before removing the items themselves.
func (s *Store) DeleteCostcoAccount(ctx context.Context, account string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteRetailerAccount(ctx, tx, "costco", account); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from costco_items where account = ?`, account); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from costco_receipts where account = ?`, account); err != nil {
		return err
	}
	return tx.Commit()
}
