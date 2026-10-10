package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kilianc/fin/internal/amazon"
)

const amazonSchema = `
-- fin's settings that queries may want, such as the epoch set with fin epoch.
create table if not exists settings (
	key   varchar primary key,
	value varchar
);
create table if not exists amazon_accounts (
	account   varchar primary key,
	profile   varchar,
	last_sync timestamptz,
	status    varchar
);
-- complete_since: payments are stored without gaps back to this date.
alter table amazon_accounts add column if not exists complete_since date;
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
	allocated     decimal(18, 4) not null,
	primary key (order_id, line)
);
create table if not exists amazon_item_categories (
	order_id          varchar not null,
	line              integer not null,
	category          varchar not null,
	category_detailed varchar,
	set_at            timestamptz not null,
	primary key (order_id, line)
);
create table if not exists amazon_asin_categories (
	asin              varchar primary key,
	category          varchar not null,
	category_detailed varchar,
	set_at            timestamptz not null
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

-- Each matched bank transaction split across the items it paid for, in
-- proportion to their allocated amounts. A refund goes to the item whose
-- share it equals, else to the items Amazon marks returned, else to the whole
-- order. The splits of a transaction add up to its amount exactly; the
-- rounding cent goes to the largest item.
create or replace view amazon_splits as
with m as (
	select transaction_id, payment_key, amount as charge, unnest(order_ids) as order_id
	from amazon_matches where match = 'exact'
),
lines as (
	select m.transaction_id, m.payment_key, m.charge, i.order_id, i.line, i.asin, i.title, i.quantity, i.allocated,
		coalesce(ic.category, ac.category) as category,
		coalesce(ic.category_detailed, ac.category_detailed) as category_detailed,
		abs(i.allocated + m.charge) < 0.005 as equals_refund,
		regexp_matches(lower(coalesce(i.return_status, '')), 'refund (has been )?issued|return is complete|returned|refunded') as returned
	from m join amazon_items i using (order_id)
	left join amazon_item_categories ic on ic.order_id = i.order_id and ic.line = i.line
	left join amazon_asin_categories ac on ac.asin = i.asin
),
ranked as (
	select *,
		count(*) filter (where equals_refund) over w as n_equal,
		count(*) filter (where returned) over w as n_returned,
		row_number() over (partition by payment_key, equals_refund order by order_id, line) as nth_equal
	from lines window w as (partition by payment_key)
),
kept as (
	select *, sum(allocated) over (partition by payment_key) as base
	from ranked
	where charge >= 0
		or (n_equal > 0 and equals_refund and nth_equal = 1)
		or (n_equal = 0 and n_returned > 0 and returned)
		or (n_equal = 0 and n_returned = 0)
),
shares as (
	select *,
		case when base = 0 then 0 else round(charge * allocated / base, 2) end::decimal(18, 4) as share,
		row_number() over (partition by payment_key order by allocated desc, order_id, line) as rank
	from kept
)
select transaction_id, payment_key, order_id, line, asin, title, quantity, category, category_detailed,
	(share + case when rank = 1 then charge - sum(share) over (partition by payment_key) else 0 end)::decimal(18, 4) as amount
from shares;

-- Every transaction once, except Amazon charges matched to an order, which
-- appear as one row per item with the item's category.
create or replace view spending as
select t.transaction_id, t.item_id, t.account_id, t.date, t.authorized_date, t.name, t.merchant_name,
	t.amount, t.iso_currency_code, t.pending, t.category, t.category_detailed,
	null::varchar as amazon_order_id, null::integer as amazon_line, null::varchar as amazon_item
from transactions t
where t.transaction_id not in (select transaction_id from amazon_splits)
union all
select t.transaction_id, t.item_id, t.account_id, t.date, t.authorized_date, t.name, t.merchant_name,
	s.amount, t.iso_currency_code, t.pending,
	coalesce(s.category, t.category), coalesce(s.category_detailed, t.category_detailed),
	s.order_id, s.line, s.title
from amazon_splits s join transactions t using (transaction_id);
`

// SetSetting saves one setting; an empty value removes it.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `delete from settings where key = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `insert or replace into settings values (?, ?)`, key, value)
	return err
}

// EarliestTransaction is the date of the oldest stored bank transaction, or
// "" when there is none.
func (s *Store) EarliestTransaction(ctx context.Context) (string, error) {
	var d sql.NullString
	err := s.db.QueryRowContext(ctx, `select min(date)::varchar from transactions`).Scan(&d)
	return d.String, err
}

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
	if _, err := tx.ExecContext(ctx, `insert into amazon_accounts (account, profile, last_sync, status) values (?, ?, ?, 'ok')
		on conflict (account) do update set profile = excluded.profile, last_sync = excluded.last_sync, status = 'ok'`,
		account, profile, at); err != nil {
		return err
	}
	return tx.Commit()
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
	if _, err := tx.ExecContext(ctx, `update amazon_accounts set complete_since = ?::date where account = ?`, since, account); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAmazonAccountStatus records why an account's last sync failed.
func (s *Store) SetAmazonAccountStatus(ctx context.Context, account, profile, status string) error {
	_, err := s.db.ExecContext(ctx, `insert into amazon_accounts (account, profile, status) values (?, ?, ?)
		on conflict (account) do update set status = excluded.status`, account, profile, status)
	return err
}

// AmazonCompleteSince is the date back to which the account's payments are
// stored without gaps, or "" when no read has gone back far enough yet.
func (s *Store) AmazonCompleteSince(ctx context.Context, account string) (string, error) {
	var since sql.NullString
	err := s.db.QueryRowContext(ctx, `select complete_since::varchar from amazon_accounts where account = ?`, account).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return since.String, err
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
			nullable(it.Condition), nullable(it.Return), it.Allocated.String()); err != nil {
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

// AmazonItem is an order line for categorizing.
type AmazonItem struct {
	Item             string  `json:"item"` // order_id#line, the handle fin amazon categorize takes
	OrderID          string  `json:"order_id"`
	Line             int     `json:"line"`
	Date             string  `json:"date"`
	ASIN             *string `json:"asin"`
	Title            string  `json:"title"`
	Quantity         int     `json:"quantity"`
	Allocated        float64 `json:"allocated"`
	Category         *string `json:"category"`
	CategoryDetailed *string `json:"category_detailed"`
	Source           string  `json:"category_source"` // item, asin, or none
}

// AmazonItems lists order lines, newest first; with all false, only those
// without a category.
func (s *Store) AmazonItems(ctx context.Context, all bool) ([]AmazonItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		select i.order_id || '#' || i.line, i.order_id, i.line, coalesce(o.date::varchar, ''), i.asin, i.title, i.quantity,
			i.allocated::double, coalesce(ic.category, ac.category), coalesce(ic.category_detailed, ac.category_detailed),
			case when ic.category is not null then 'item' when ac.category is not null then 'asin' else 'none' end
		from amazon_items i join amazon_orders o using (order_id)
		left join amazon_item_categories ic on ic.order_id = i.order_id and ic.line = i.line
		left join amazon_asin_categories ac on ac.asin = i.asin
		where ? or (ic.category is null and ac.category is null)
		order by o.date desc, i.order_id, i.line`, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AmazonItem{}
	for rows.Next() {
		var it AmazonItem
		if err := rows.Scan(&it.Item, &it.OrderID, &it.Line, &it.Date, &it.ASIN, &it.Title, &it.Quantity, &it.Allocated,
			&it.Category, &it.CategoryDetailed, &it.Source); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CategoryChange sets one item's category, and optionally its product's.
type CategoryChange struct {
	OrderID          string
	Line             int
	Category         string
	CategoryDetailed string
	ASINDefault      bool
}

// ErrNoSuchItem means a category was set for an item fin does not have.
var ErrNoSuchItem = errors.New("no such Amazon item")

// SetAmazonCategories saves categories in one transaction.
func (s *Store) SetAmazonCategories(ctx context.Context, changes []CategoryChange, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		var asin sql.NullString
		err := tx.QueryRowContext(ctx, `select asin from amazon_items where order_id = ? and line = ?`, c.OrderID, c.Line).Scan(&asin)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s#%d", ErrNoSuchItem, c.OrderID, c.Line)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert or replace into amazon_item_categories values (?, ?, ?, ?, ?)`,
			c.OrderID, c.Line, c.Category, nullable(c.CategoryDetailed), at); err != nil {
			return err
		}
		if c.ASINDefault && asin.Valid {
			if _, err := tx.ExecContext(ctx, `insert or replace into amazon_asin_categories values (?, ?, ?, ?)`,
				asin.String, c.Category, nullable(c.CategoryDetailed), at); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
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
			(select count(*) from amazon_items i join amazon_orders o using (order_id)
				left join amazon_item_categories ic on ic.order_id = i.order_id and ic.line = i.line
				left join amazon_asin_categories ac on ac.asin = i.asin
				where o.account = a.account and ic.category is null and ac.category is null),
			(select count(*) from amazon_matches m where m.account = a.account and m.match = 'exact'),
			(select count(*) from amazon_matches m where m.account = a.account and m.match = 'unmatched'),
			(select count(*) from amazon_matches m where m.account = a.account and m.match = 'ambiguous')
		from amazon_accounts a`)
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
	for _, q := range []string{
		`delete from amazon_item_categories where order_id in (select order_id from amazon_orders where account = ?)`,
		`delete from amazon_items where order_id in (select order_id from amazon_orders where account = ?)`,
		`delete from amazon_order_pages where order_id in (select order_id from amazon_orders where account = ?)`,
		`delete from amazon_orders where account = ?`,
		`delete from amazon_payments where account = ?`,
		`delete from amazon_accounts where account = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AmazonItemRow is one item with its order and match, for the spreadsheet.
type AmazonItemRow struct {
	Date        string
	OrderID     string
	Title       string
	Quantity    int
	Category    *string
	Allocated   float64
	Account     string
	Transaction *string // the matched bank transaction's name and date
}

// AmazonItemRows lists the items of orders placed on or after since (all
// when empty), newest first, with the bank transaction its order's charge
// matched, if any.
func (s *Store) AmazonItemRows(ctx context.Context, since string) ([]AmazonItemRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		select coalesce(o.date::varchar, ''), i.order_id, i.title, i.quantity, coalesce(ic.category, ac.category),
			i.allocated::double, o.account,
			(select first(t.name || ' · ' || t.date::varchar order by t.date) from amazon_matches m join transactions t using (transaction_id)
				where m.match = 'exact' and list_contains(m.order_ids, i.order_id) and m.amount > 0)
		from amazon_items i join amazon_orders o using (order_id)
		left join amazon_item_categories ic on ic.order_id = i.order_id and ic.line = i.line
		left join amazon_asin_categories ac on ac.asin = i.asin
		where ? = '' or o.date >= ?::date
		order by o.date desc, i.order_id, i.line`, since, cmpDate(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AmazonItemRow{}
	for rows.Next() {
		var r AmazonItemRow
		if err := rows.Scan(&r.Date, &r.OrderID, &r.Title, &r.Quantity, &r.Category, &r.Allocated, &r.Account, &r.Transaction); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func centsMap(m map[string]amazon.Cents) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[strings.TrimSuffix(k, ":")] = v.Dollars()
	}
	return out
}

func cmpDate(d string) string {
	if d == "" {
		return "0001-01-01"
	}
	return d
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
