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
	(select first(m.transaction_id order by m.payment) from costco_matches m
		where m.account = i.account and m.order_id = i.barcode and m.match = 'exact') as transaction_id
from costco_items i join costco_receipts r using (account, barcode)
left join costco_items d on d.account = i.account and d.barcode = i.barcode and d.line = i.discount_for`

// costcoOrderItemSource is every costco.com order line.
const costcoOrderItemSource = `select 'costco' as retailer, i.account,
	i.account || '/' || i.order_number || '#' || i.line as item, i.item_number as product,
	i.order_number as order_id, i.line, o.date, i.title, i.quantity, i.cost,
	(select first(m.transaction_id order by m.payment) from costco_matches m
		where m.account = i.account and m.order_id = i.order_number and m.match = 'exact') as transaction_id
from costco_order_items i join costco_orders o using (account, order_number)`

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
create table if not exists costco_orders (
	account varchar not null,
	order_number varchar not null,
	date date,
	status varchar,
	merchandise decimal(18, 4),
	discount decimal(18, 4),
	shipping decimal(18, 4),
	fees decimal(18, 4),
	tax decimal(18, 4),
	total decimal(18, 4),
	error varchar,
	raw json not null,
	parser integer not null,
	fetched_at timestamptz not null,
	primary key (account, order_number)
);
create table if not exists costco_order_items (
	account varchar not null,
	order_number varchar not null,
	line integer not null,
	item_number varchar,
	title varchar not null,
	quantity double not null,
	unit_price decimal(18, 4) not null,
	amount decimal(18, 4) not null,
	cost decimal(18, 4) not null,
	primary key (account, order_number, line)
);
create table if not exists costco_order_payments (
	account varchar not null,
	order_number varchar not null,
	payment integer not null,
	type varchar,
	amount decimal(18, 4) not null,
	no_bank_charge boolean not null,
	primary key (account, order_number, payment)
);
` + "create or replace view costco_matches as " + retailerMatches(`
	select t.account || '/' || t.barcode || '#' || t.tender as payment_key, t.account, 'warehouse' as source,
		t.barcode as order_id, t.tender as payment, r.date, t.amount, t.type, t.card_last4, t.no_bank_charge as no_bank, 3 as days
	from costco_tenders t join costco_receipts r using (account, barcode)
	where t.amount <> 0
	union all
	-- A receipt that lists no payments is matched on its total.
	select r.account || '/' || r.barcode || '#0', r.account, 'warehouse', r.barcode, 0, r.date, r.total, null, null, r.total = 0, 3
	from costco_receipts r
	where r.error is null and not exists (select 1 from costco_tenders t where t.account = r.account and t.barcode = r.barcode)
	union all
	-- costco.com charges a card when an order ships, often days after it
	-- was placed.
	select p.account || '/' || p.order_number || '#' || p.payment, p.account, 'online', p.order_number, p.payment, o.date,
		p.amount, p.type, null, p.no_bank_charge, 10
	from costco_order_payments p join costco_orders o using (account, order_number)
	where p.amount <> 0`,
	"payment_key", "costco")

// ApplyCostco saves one complete window of receipts and orders in a
// transaction. The raw JSON stays with each, and replacing lines leaves
// categories alone.
func (s *Store) ApplyCostco(ctx context.Context, account, profile string, receipts []costco.Receipt, orders []costco.Order, at time.Time) error {
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
	for _, o := range orders {
		if err := writeCostcoOrder(ctx, tx, account, o, at); err != nil {
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

func writeCostcoOrder(ctx context.Context, tx *sql.Tx, account string, o costco.Order, at time.Time) error {
	if !json.Valid(o.Raw) {
		return fmt.Errorf("store: costco order has no raw JSON")
	}
	amount := func(c money.Cents) any {
		if o.Error != "" {
			return nil
		}
		return c.String()
	}
	if _, err := tx.ExecContext(ctx, `insert or replace into costco_orders values (?, ?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?::json, ?, ?)`,
		account, o.Number, nullable(o.Date), nullable(o.Status), amount(o.Merchandise), amount(o.Discount), amount(o.Shipping), amount(o.Fees),
		amount(o.Tax), amount(o.Total), nullable(o.Error), string(o.Raw), costco.ParserVersion, at); err != nil {
		return err
	}
	for _, table := range []string{"costco_order_items", "costco_order_payments"} {
		if _, err := tx.ExecContext(ctx, `delete from `+table+` where account = ? and order_number = ?`, account, o.Number); err != nil {
			return err
		}
	}
	for _, l := range o.Lines {
		if _, err := tx.ExecContext(ctx, `insert into costco_order_items values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			account, o.Number, l.Line, nullable(l.Number), l.Title, l.Quantity, l.UnitPrice.String(), l.Amount.String(), l.Cost.String()); err != nil {
			return err
		}
	}
	for _, p := range o.Payments {
		if _, err := tx.ExecContext(ctx, `insert into costco_order_payments values (?, ?, ?, ?, ?, ?)`,
			account, o.Number, p.Tender, nullable(p.Type), p.Amount.String(), p.NoBankCharge); err != nil {
			return err
		}
	}
	return nil
}

// ReparseCostco reads receipts and orders written by an older parser
// again, from their stored JSON only. One it still cannot read stays
// unreadable. It returns how many it read.
func (s *Store) ReparseCostco(ctx context.Context, account string) (int, error) {
	type kept struct {
		key string
		raw []byte
		at  time.Time
	}
	read := func(query string) ([]kept, error) {
		rows, err := s.db.QueryContext(ctx, query, account, costco.ParserVersion)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []kept
		for rows.Next() {
			var k kept
			if err := rows.Scan(&k.key, &k.raw, &k.at); err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	}
	receipts, err := read(`select barcode, raw::varchar, fetched_at from costco_receipts where account = ? and parser < ?`)
	if err != nil {
		return 0, err
	}
	orders, err := read(`select order_number, raw::varchar, fetched_at from costco_orders where account = ? and parser < ?`)
	if err != nil {
		return 0, err
	}
	write := func(w func(tx *sql.Tx) error) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := w(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	for i, r := range receipts {
		parsed, err := costco.ParseReceipt(r.raw)
		if err != nil {
			u := costco.Unreadable(r.raw, err)
			parsed = &u
		}
		if err := write(func(tx *sql.Tx) error { return writeCostcoReceipt(ctx, tx, account, *parsed, r.at) }); err != nil {
			return i, err
		}
	}
	for i, o := range orders {
		parsed, err := costco.ParseOrder(o.raw)
		if err != nil {
			u := costco.UnreadableOrder(o.key, o.raw, err)
			parsed = &u
		}
		if err := write(func(tx *sql.Tx) error { return writeCostcoOrder(ctx, tx, account, *parsed, o.at) }); err != nil {
			return len(receipts) + i, err
		}
	}
	return len(receipts) + len(orders), nil
}

// CostcoSummary counts receipts, orders, items and payment matches for one account.
type CostcoSummary struct {
	Account       string     `json:"account"`
	Receipts      int        `json:"receipts"`
	Unreadable    int        `json:"unreadable_receipts"`
	Orders        int        `json:"online_orders"`
	UnreadableOrd int        `json:"unreadable_orders"`
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
		(select count(*) from costco_orders o where o.account = a.account),
		(select count(*) from costco_orders o where o.account = a.account and o.error is not null),
		(select count(*) from retailer_items i where i.retailer = 'costco' and i.account = a.account),
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
		if err := rows.Scan(&r.Account, &r.LastSync, &r.Status, &r.Receipts, &r.Unreadable, &r.Orders, &r.UnreadableOrd, &r.Items, &r.Uncategorized, &r.Matched, &r.Unmatched, &r.Ambiguous); err != nil {
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
	for _, table := range []string{"costco_items", "costco_tenders", "costco_receipts", "costco_order_items", "costco_order_payments", "costco_orders"} {
		if _, err := tx.ExecContext(ctx, `delete from `+table+` where account = ?`, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}
