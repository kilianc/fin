package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/amazon"
	"github.com/kilianc/fin/internal/money"
)

// amazonRetailer is Amazon's name in the shared retailer tables.
const amazonRetailer = "amazon"

// amazonItemSource is Amazon's part of the retailer_items view.
const amazonItemSource = `select 'amazon' as retailer, o.account, i.order_id || '#' || i.line as item, i.asin as product,
	i.order_id, i.line, o.date, i.title, i.quantity, i.cost,
	(select first(m.transaction_id order by m.date) from amazon_matches m
		where m.match = 'exact' and m.amount > 0 and list_contains(m.order_ids, i.order_id)) as transaction_id
from amazon_items i join amazon_orders o using (order_id)`

const amazonSchema = `
create table if not exists amazon_payments (
	payment_key       varchar primary key,
	account           varchar not null,
	date              date not null,
	amount            decimal(18, 4) not null,
	iso_currency_code varchar,
	payment_method    varchar,
	descriptor        varchar,
	status            varchar,
	order_ids         varchar[] not null,
	raw               json
);
create table if not exists amazon_orders (
	order_id       varchar primary key,
	account        varchar not null,
	date           date,
	kind           varchar not null,
	items_subtotal decimal(18, 4),
	shipping       decimal(18, 4),
	discounts      decimal(18, 4),
	tax            decimal(18, 4),
	gift_card      decimal(18, 4),
	total          decimal(18, 4),
	paid           decimal(18, 4),
	refund_total   decimal(18, 4),
	card_last4     varchar,
	summary        json,
	error          varchar,
	parser         integer not null,
	fetched_at     timestamptz not null
);
create table if not exists amazon_order_pages (
	order_id   varchar primary key,
	page       blob not null,
	fetched_at timestamptz not null
);
create table if not exists amazon_items (
	order_id      varchar not null,
	line          integer not null,
	asin          varchar,
	title         varchar not null,
	quantity      integer not null,
	unit_price    decimal(18, 4),
	seller        varchar,
	condition     varchar,
	return_status varchar,
	cost          decimal(18, 4) not null,
	primary key (order_id, line)
);
-- Each Amazon payment and the bank transaction it became: same amount, the
-- bank's date within four days, an Amazon-looking merchant, and the card's
-- last four digits when the order page showed them. Ambiguous cases are
-- reported, never guessed.
create or replace view amazon_matches as
with p as (
	select p.*, (
		select first(o.card_last4) from amazon_orders o where list_contains(p.order_ids, o.order_id)
	) as card_last4,
	lower(coalesce(p.payment_method, '')) like '%gift card%' or lower(coalesce(p.payment_method, '')) like '%points%' as no_bank
	from amazon_payments p
),
bank as (
	select t.transaction_id, t.amount, coalesce(t.authorized_date, t.date) as day, a.mask
	from transactions t left join accounts a using (account_id)
	where regexp_matches(lower(coalesce(t.merchant_name, '') || ' ' || t.name), 'amazon|amzn|audible|kindle|prime video')
),
cand as (
	select p.payment_key, b.transaction_id,
		count(*) over (partition by b.transaction_id) as per_transaction
	from p join bank b on b.amount = p.amount
		and b.day between p.date - 4 and p.date + 4
		and (p.card_last4 is null or b.mask is null or b.mask = p.card_last4)
	where not p.no_bank
),
agg as (
	select payment_key, count(*) as candidates, any_value(transaction_id) as transaction_id,
		max(per_transaction) as per_transaction
	from cand group by payment_key
)
select p.payment_key, p.account, p.date, p.amount, p.payment_method, p.descriptor, p.status, p.order_ids,
	p.card_last4,
	case
		when p.no_bank then 'no_bank_charge'
		when agg.candidates is null then 'unmatched'
		when agg.candidates = 1 and agg.per_transaction = 1 then 'exact'
		else 'ambiguous'
	end as match,
	case when agg.candidates = 1 and agg.per_transaction = 1 then agg.transaction_id end as transaction_id,
	coalesce(agg.candidates, 0) as candidates
from p left join agg using (payment_key);
`

// AmazonPaymentKeys returns the keys of the payments stored for account.
func (s *Store) AmazonPaymentKeys(ctx context.Context, account string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `select payment_key from amazon_payments where account = ?`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// ApplyAmazonPayments adds or updates payments read from Amazon.
func (s *Store) ApplyAmazonPayments(ctx context.Context, account, profile string, payments []amazon.Payment, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `insert or replace into amazon_payments values (?, ?, ?::date, ?, ?, ?, ?, ?, ?::varchar[], ?::json)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range payments {
		if _, err := stmt.ExecContext(ctx, p.Key, account, p.Date, p.Amount.String(), p.Currency, p.Method,
			p.Descriptor, p.Status, p.OrderIDs, string(p.Raw)); err != nil {
			return fmt.Errorf("store: amazon payment: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.SetRetailerSynced(ctx, amazonRetailer, account, profile, at)
}

// PruneAmazonPayments records that a read went back to since without gaps,
// and deletes the account's payments it did not see.
func (s *Store) PruneAmazonPayments(ctx context.Context, account, since string, keep map[string]bool) error {
	keys := make([]string, 0, len(keep))
	for k := range keep {
		keys = append(keys, k)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `delete from amazon_payments where account = ? and not list_contains(?::varchar[], payment_key)`, account, keys); err != nil {
		return err
	}
	if err := setCompleteSince(ctx, tx, amazonRetailer, account, since); err != nil {
		return err
	}
	return tx.Commit()
}

// AmazonOrdersToFetch lists the account's physical orders whose page should
// be read: ones never read, ones in fresh (new payments point at them), and
// ones recent enough to still change that were last read before stale.
func (s *Store) AmazonOrdersToFetch(ctx context.Context, account string, fresh []string, since time.Time, stale time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		with ids as (
			select distinct unnest(order_ids) as order_id from amazon_payments where account = ?
		)
		select distinct ids.order_id from ids left join amazon_orders o using (order_id)
		where regexp_matches(ids.order_id, '^[0-9]{3}-')
			and (o.order_id is null
				or list_contains(?::varchar[], ids.order_id)
				or (o.date >= ?::date and o.fetched_at < ?))
		order by 1 desc`, account, fresh, since.Format("2006-01-02"), stale)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AmazonOrderAccount is the Amazon account whose payments named the order,
// or "" when none did.
func (s *Store) AmazonOrderAccount(ctx context.Context, id string) (string, error) {
	var account sql.NullString
	err := s.db.QueryRowContext(ctx, `select first(account) from amazon_payments where list_contains(order_ids, ?)`, id).Scan(&account)
	return account.String, err
}

// ApplyAmazonOrder stores an order page and what was read from it. A nil
// order records a page fin could not read, with why.
func (s *Store) ApplyAmazonOrder(ctx context.Context, account, id string, page []byte, o *amazon.Order, readErr error, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if page != nil {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(page)
		zw.Close()
		if _, err := tx.ExecContext(ctx, `insert or replace into amazon_order_pages values (?, ?, ?)`, id, buf.Bytes(), at); err != nil {
			return err
		}
	}
	if err := writeOrder(ctx, tx, account, id, o, readErr, at); err != nil {
		return err
	}
	return tx.Commit()
}

func writeOrder(ctx context.Context, tx *sql.Tx, account, id string, o *amazon.Order, readErr error, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `delete from amazon_items where order_id = ?`, id); err != nil {
		return err
	}
	if o == nil {
		msg := "unreadable"
		if readErr != nil {
			msg = readErr.Error()
		}
		_, err := tx.ExecContext(ctx, `insert or replace into amazon_orders (order_id, account, kind, error, parser, fetched_at)
			values (?, ?, 'unreadable', ?, ?, ?)`, id, account, msg, amazon.ParserVersion, at)
		return err
	}
	summary, _ := json.Marshal(centsMap(o.Summary))
	var last4 *string
	if o.CardLast4 != "" {
		last4 = &o.CardLast4
	}
	if _, err := tx.ExecContext(ctx, `insert or replace into amazon_orders values
		(?, ?, ?::date, 'physical', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::json, null, ?, ?)`,
		id, account, o.Date, o.Subtotal.String(), o.Shipping.String(), o.Discounts.String(), o.Tax.String(),
		o.GiftCard.String(), o.Total.String(), o.Paid.String(), o.RefundTotal.String(), last4, string(summary),
		amazon.ParserVersion, at); err != nil {
		return fmt.Errorf("store: amazon order %s: %w", id, err)
	}
	for _, it := range o.Items {
		if _, err := tx.ExecContext(ctx, `insert into amazon_items values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, it.Line, nullable(it.ASIN), it.Title, it.Quantity, it.UnitPrice.String(), nullable(it.Seller),
			nullable(it.Condition), nullable(it.Return), it.Cost.String()); err != nil {
			return fmt.Errorf("store: amazon item %s/%d: %w", id, it.Line, err)
		}
	}
	return nil
}

// ReparseAmazonOrders reads the kept pages of orders parsed by an older
// parser again, without asking Amazon. It returns how many it read.
func (s *Store) ReparseAmazonOrders(ctx context.Context, parse func([]byte) (*amazon.Order, error)) (int, error) {
	rows, err := s.db.QueryContext(ctx, `select o.order_id, o.account, p.page, p.fetched_at from amazon_orders o
		join amazon_order_pages p using (order_id) where o.parser < ?`, amazon.ParserVersion)
	if err != nil {
		return 0, err
	}
	type kept struct {
		id, account string
		page        []byte
		at          time.Time
	}
	var pages []kept
	for rows.Next() {
		var k kept
		if err := rows.Scan(&k.id, &k.account, &k.page, &k.at); err != nil {
			rows.Close()
			return 0, err
		}
		pages = append(pages, k)
	}
	rows.Close()
	for _, k := range pages {
		zr, err := gzip.NewReader(bytes.NewReader(k.page))
		if err != nil {
			return 0, err
		}
		page, err := io.ReadAll(zr)
		if err != nil {
			return 0, err
		}
		o, perr := parse(page)
		if perr != nil {
			o = nil
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		if err := writeOrder(ctx, tx, k.account, k.id, o, perr, k.at); err != nil {
			tx.Rollback()
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	return len(pages), nil
}

// AmazonSummary counts what is stored for one account.
type AmazonSummary struct {
	Account       string     `json:"account"`
	Payments      int        `json:"payments"`
	Orders        int        `json:"orders"`
	Unreadable    int        `json:"unreadable_orders"`
	Items         int        `json:"items"`
	Uncategorized int        `json:"uncategorized_items"`
	Matched       int        `json:"matched_payments"`
	Unmatched     int        `json:"unmatched_payments"`
	Ambiguous     int        `json:"ambiguous_payments"`
	LastSync      *time.Time `json:"last_sync"`
	Status        string     `json:"status"`
}

// AmazonSummaries counts payments, orders and items for each account.
func (s *Store) AmazonSummaries(ctx context.Context) (map[string]AmazonSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		select a.account, a.last_sync, coalesce(a.status, ''),
			(select count(*) from amazon_payments p where p.account = a.account),
			(select count(*) from amazon_orders o where o.account = a.account),
			(select count(*) from amazon_orders o where o.account = a.account and o.kind = 'unreadable'),
			(select count(*) from amazon_items i join amazon_orders o using (order_id) where o.account = a.account),
			(select count(*) from retailer_items i where i.retailer = 'amazon' and i.account = a.account and i.category is null),
			(select count(*) from amazon_matches m where m.account = a.account and m.match = 'exact'),
			(select count(*) from amazon_matches m where m.account = a.account and m.match = 'unmatched'),
			(select count(*) from amazon_matches m where m.account = a.account and m.match = 'ambiguous')
		from retailer_accounts a where a.retailer = 'amazon'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AmazonSummary{}
	for rows.Next() {
		var a AmazonSummary
		if err := rows.Scan(&a.Account, &a.LastSync, &a.Status, &a.Payments, &a.Orders, &a.Unreadable, &a.Items,
			&a.Uncategorized, &a.Matched, &a.Unmatched, &a.Ambiguous); err != nil {
			return nil, err
		}
		out[a.Account] = a
	}
	return out, rows.Err()
}

// DeleteAmazonAccount forgets everything stored for account.
func (s *Store) DeleteAmazonAccount(ctx context.Context, account string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteRetailerAccount(ctx, tx, amazonRetailer, account); err != nil {
		return err
	}
	for _, q := range []string{
		`delete from amazon_items where order_id in (select order_id from amazon_orders where account = ?)`,
		`delete from amazon_order_pages where order_id in (select order_id from amazon_orders where account = ?)`,
		`delete from amazon_orders where account = ?`,
		`delete from amazon_payments where account = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func centsMap(m map[string]money.Cents) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[strings.TrimSuffix(k, ":")] = v.Dollars()
	}
	return out
}
