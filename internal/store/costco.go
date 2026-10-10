package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kilianc/fin/internal/costco"
	"github.com/kilianc/fin/internal/money"
)

// costcoItemSource gives a discount line its item's number as product, so
// a category set for the product covers its discounts too.
const costcoItemSource = `select 'costco' as retailer, i.account,
	i.account || '/' || i.barcode || '#' || i.line as item, coalesce(d.item_number, i.item_number) as product,
	i.barcode as order_id, i.line, r.date, i.title, i.quantity, i.cost,
	(select first(m.transaction_id order by m.tender) from costco_matches m
		where m.account = i.account and m.barcode = i.barcode and m.match = 'exact') as transaction_id
from costco_items i join costco_receipts r using (account, barcode)
left join costco_items d on d.account = i.account and d.barcode = i.barcode and d.line = i.discount_for`

var costcoSchema = `
create table if not exists costco_receipts (
	account varchar not null,
	barcode varchar not null,
	date date,
	transaction_time varchar,
	warehouse_number varchar,
	warehouse_name varchar,
	register_number varchar,
	transaction_number varchar,
	transaction_type varchar,
	subtotal decimal(18, 4),
	tax decimal(18, 4),
	total decimal(18, 4),
	instant_savings decimal(18, 4),
	error varchar,
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
	discount_for integer,
	cost decimal(18, 4) not null,
	primary key (account, barcode, line)
);
create table if not exists costco_tenders (
	account varchar not null,
	barcode varchar not null,
	tender integer not null,
	type varchar,
	description varchar,
	card_last4 varchar,
	amount decimal(18, 4) not null,
	no_bank_charge boolean not null,
	primary key (account, barcode, tender)
);
` + "create or replace view costco_matches as " + retailerMatches(`
	select t.account || '/' || t.barcode || '#' || t.tender as payment_key, t.account, t.barcode, t.tender, r.date,
		t.amount, t.type, t.card_last4, t.no_bank_charge as no_bank
	from costco_tenders t join costco_receipts r using (account, barcode)
	where t.amount <> 0
	union all
	-- A receipt that lists no payments is matched on its total.
	select r.account || '/' || r.barcode || '#0', r.account, r.barcode, 0, r.date, r.total, null, null, r.total = 0
	from costco_receipts r
	where r.error is null and not exists (select 1 from costco_tenders t where t.account = r.account and t.barcode = r.barcode)`,
	"payment_key", 3, "costco")

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
	amount := func(c money.Cents) any {
		if r.Error != "" {
			return nil
		}
		return c.String()
	}
	if _, err := tx.ExecContext(ctx, `insert or replace into costco_receipts values (?, ?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::json, ?, ?)`,
		account, r.Barcode, nullable(r.Date), nullable(r.DateTime), nullable(r.Warehouse), nullable(r.WarehouseName), nullable(r.Register), nullable(r.Transaction), nullable(r.Type),
		amount(r.Subtotal), amount(r.Tax), amount(r.Total), amount(r.Savings), nullable(r.Error), string(r.Raw), costco.ParserVersion, at); err != nil {
		return err
	}
	for _, table := range []string{"costco_items", "costco_tenders"} {
		if _, err := tx.ExecContext(ctx, `delete from `+table+` where account = ? and barcode = ?`, account, r.Barcode); err != nil {
			return err
		}
	}
	for _, it := range r.Items {
		var discountFor *int
		if it.DiscountFor > 0 {
			discountFor = &it.DiscountFor
		}
		if _, err := tx.ExecContext(ctx, `insert into costco_items values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account, r.Barcode, it.Line, nullable(it.Number), it.Title, it.Quantity, it.UnitPrice.String(), it.Amount.String(), nullable(it.TaxFlag), nullable(it.Department), discountFor, it.Cost.String()); err != nil {
			return err
		}
	}
	for _, t := range r.Tenders {
		if _, err := tx.ExecContext(ctx, `insert into costco_tenders values (?, ?, ?, ?, ?, ?, ?, ?)`,
			account, r.Barcode, t.Tender, nullable(t.Type), nullable(t.Description), nullable(t.Last4), t.Amount.String(), t.NoBankCharge); err != nil {
			return err
		}
	}
	return nil
}

// ReparseCostcoReceipts reads receipts written by an older parser again,
// from their stored JSON only. One it still cannot read stays unreadable.
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
			u := costco.Unreadable(r.raw, err)
			parsed = &u
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

// CostcoSummary counts receipts, items and payment matches for one account.
type CostcoSummary struct {
	Account       string     `json:"account"`
	Receipts      int        `json:"receipts"`
	Unreadable    int        `json:"unreadable_receipts"`
	Items         int        `json:"items"`
	Uncategorized int        `json:"uncategorized_items"`
	Matched       int        `json:"matched_payments"`
	Unmatched     int        `json:"unmatched_payments"`
	Ambiguous     int        `json:"ambiguous_payments"`
	LastSync      *time.Time `json:"last_sync"`
	Status        string     `json:"status"`
}

func (s *Store) CostcoSummaries(ctx context.Context) (map[string]CostcoSummary, error) {
	rows, err := s.db.QueryContext(ctx, `select a.account, a.last_sync, coalesce(a.status, ''),
		(select count(*) from costco_receipts r where r.account = a.account),
		(select count(*) from costco_receipts r where r.account = a.account and r.error is not null),
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
		if err := rows.Scan(&r.Account, &r.LastSync, &r.Status, &r.Receipts, &r.Unreadable, &r.Items, &r.Uncategorized, &r.Matched, &r.Unmatched, &r.Ambiguous); err != nil {
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
	for _, table := range []string{"costco_items", "costco_tenders", "costco_receipts"} {
		if _, err := tx.ExecContext(ctx, `delete from `+table+` where account = ?`, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}
