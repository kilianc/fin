package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// retailerSchema holds what every retailer shares: each connected account's
// sync state, and the categories the agent sets on items and products.
const retailerSchema = `
create table if not exists retailer_accounts (
	retailer       varchar not null,
	account        varchar not null,
	profile        varchar,
	last_sync      timestamptz,
	status         varchar,
	-- history is stored without gaps back to this date
	complete_since date,
	-- where a history read the retailer cut short goes on from
	resume_key     varchar,
	-- after "too many requests", fin asks nothing of the retailer before this
	limited_until  timestamptz,
	primary key (retailer, account)
);
create table if not exists item_categories (
	retailer          varchar not null,
	item              varchar not null,
	category          varchar not null,
	category_detailed varchar,
	set_at            timestamptz not null,
	primary key (retailer, item)
);
create table if not exists product_categories (
	retailer          varchar not null,
	product           varchar not null,
	category          varchar not null,
	category_detailed varchar,
	set_at            timestamptz not null,
	primary key (retailer, product)
);
`

// retailerItemSources select each retailer's items in one shape: retailer,
// account, item (the handle categories use), product (the retailer's product
// ID, for categories reused on later purchases), order_id, line (its place
// in the order), date, title,
// quantity, cost (with its share of tax, shipping and discounts) and
// transaction_id (a bank transaction that paid for the order, if one
// matched). A new retailer adds its select here.
var retailerItemSources = []string{amazonItemSource, costcoItemSource}

// retailerItemsView is every retailer's items with their category: the
// item's own, else its product's.
func retailerItemsView() string {
	return `create or replace view retailer_items as
with items as (` + strings.Join(retailerItemSources, "\nunion all\n") + `)
select items.*,
	coalesce(ic.category, pc.category) as category,
	coalesce(ic.category_detailed, pc.category_detailed) as category_detailed,
	case when ic.category is not null then 'item' when pc.category is not null then 'product' else 'none' end as category_source
from items
left join item_categories ic using (retailer, item)
left join product_categories pc using (retailer, product);
`
}

// RetailerAccount is what the store keeps about syncing one account.
type RetailerAccount struct {
	LastSync      *time.Time
	Status        string
	CompleteSince string    // "" until a read has gone back far enough
	ResumeKey     string    // "" unless a history read was cut short
	LimitedUntil  time.Time // zero unless the retailer limited requests
}

// RetailerAccountState reads an account's sync state; an account never
// synced has the zero state.
func (s *Store) RetailerAccountState(ctx context.Context, retailer, account string) (RetailerAccount, error) {
	var a RetailerAccount
	var status, since, key sql.NullString
	var until sql.NullTime
	err := s.db.QueryRowContext(ctx, `select last_sync, status, complete_since::varchar, resume_key, limited_until
		from retailer_accounts where retailer = ? and account = ?`, retailer, account).Scan(&a.LastSync, &status, &since, &key, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	a.Status, a.CompleteSince, a.ResumeKey, a.LimitedUntil = status.String, since.String, key.String, until.Time
	return a, err
}

// setRetailerAccount upserts one column of an account's state.
func (s *Store) setRetailerAccount(ctx context.Context, retailer, account, profile, column string, value any) error {
	_, err := s.db.ExecContext(ctx, `insert into retailer_accounts (retailer, account, profile, `+column+`) values (?, ?, ?, ?)
		on conflict (retailer, account) do update set `+column+` = excluded.`+column, retailer, account, profile, value)
	return err
}

// SetRetailerStatus records how an account's last sync went, such as "ok"
// or an error code.
func (s *Store) SetRetailerStatus(ctx context.Context, retailer, account, profile, status string) error {
	return s.setRetailerAccount(ctx, retailer, account, profile, "status", status)
}

// SetRetailerSynced records a sync that read something, now.
func (s *Store) SetRetailerSynced(ctx context.Context, retailer, account, profile string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `insert into retailer_accounts (retailer, account, profile, last_sync, status) values (?, ?, ?, ?, 'ok')
		on conflict (retailer, account) do update set profile = excluded.profile, last_sync = excluded.last_sync, status = 'ok'`,
		retailer, account, profile, at)
	return err
}

// SetLimitedUntil records when fin may ask the retailer again; the zero
// time clears it.
func (s *Store) SetLimitedUntil(ctx context.Context, retailer, account, profile string, until time.Time) error {
	var v any
	if !until.IsZero() {
		v = until.UTC()
	}
	return s.setRetailerAccount(ctx, retailer, account, profile, "limited_until", v)
}

// SetResumeKey saves where an unfinished history read goes on from; ""
// clears it.
func (s *Store) SetResumeKey(ctx context.Context, retailer, account, key string) error {
	_, err := s.db.ExecContext(ctx, `update retailer_accounts set resume_key = nullif(?, '') where retailer = ? and account = ?`, key, retailer, account)
	return err
}

// SetCompleteSince records that history is stored without gaps back to
// since, and clears any resume key.
func (s *Store) SetCompleteSince(ctx context.Context, retailer, account, since string) error {
	return setCompleteSince(ctx, s.db, retailer, account, since)
}

func setCompleteSince(ctx context.Context, db execer, retailer, account, since string) error {
	_, err := db.ExecContext(ctx, `update retailer_accounts set complete_since = ?::date, resume_key = null where retailer = ? and account = ?`, since, retailer, account)
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// deleteRetailerAccount forgets an account's shared rows: its items'
// categories and its sync state. Call it before deleting the items.
func deleteRetailerAccount(ctx context.Context, tx *sql.Tx, retailer, account string) error {
	if _, err := tx.ExecContext(ctx, `delete from item_categories where retailer = ?
		and item in (select item from retailer_items where retailer = ? and account = ?)`, retailer, retailer, account); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `delete from retailer_accounts where retailer = ? and account = ?`, retailer, account)
	return err
}

// RetailerItem is one item bought at a retailer, with its category.
type RetailerItem struct {
	Item             string  `json:"item"` // the handle categorize takes
	OrderID          string  `json:"order_id"`
	Date             string  `json:"date"`
	Product          *string `json:"product"`
	Title            string  `json:"title"`
	Quantity         float64 `json:"quantity"`
	Cost             float64 `json:"cost"`
	Category         *string `json:"category"`
	CategoryDetailed *string `json:"category_detailed"`
	Source           string  `json:"category_source"` // item, product, or none
	Account          string  `json:"account"`
	Transaction      *string `json:"bank_transaction"` // the matched bank transaction's name and date
}

// ItemFilter narrows RetailerItems.
type ItemFilter struct {
	Uncategorized bool   // only items without a category
	Since         string // only orders on or after this date, YYYY-MM-DD
}

// RetailerItems lists a retailer's items, newest first.
func (s *Store) RetailerItems(ctx context.Context, retailer string, f ItemFilter) ([]RetailerItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		select i.item, i.order_id, coalesce(i.date::varchar, ''), i.product, i.title, i.quantity, i.cost::double,
			i.category, i.category_detailed, i.category_source, i.account,
			(select t.name || ' · ' || t.date::varchar from transactions t where t.transaction_id = i.transaction_id)
		from retailer_items i
		where i.retailer = ? and (not ? or i.category is null) and (? = '' or i.date >= ?::date)
		order by i.date desc, i.order_id, i.line`, retailer, f.Uncategorized, f.Since, cmpDate(f.Since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RetailerItem{}
	for rows.Next() {
		var it RetailerItem
		if err := rows.Scan(&it.Item, &it.OrderID, &it.Date, &it.Product, &it.Title, &it.Quantity, &it.Cost,
			&it.Category, &it.CategoryDetailed, &it.Source, &it.Account, &it.Transaction); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CategoryChange sets one item's category, and with Product, the category
// of later purchases of the same product.
type CategoryChange struct {
	Item             string
	Category         string
	CategoryDetailed string
	Product          bool
}

// ErrNoSuchItem means a category was set for an item fin does not have.
var ErrNoSuchItem = errors.New("no such item")

// SetCategories saves a retailer's item categories in one transaction.
func (s *Store) SetCategories(ctx context.Context, retailer string, changes []CategoryChange, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		var product sql.NullString
		err := tx.QueryRowContext(ctx, `select product from retailer_items where retailer = ? and item = ?`, retailer, c.Item).Scan(&product)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNoSuchItem, c.Item)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert or replace into item_categories values (?, ?, ?, ?, ?)`,
			retailer, c.Item, c.Category, nullable(c.CategoryDetailed), at); err != nil {
			return err
		}
		if c.Product && product.Valid {
			if _, err := tx.ExecContext(ctx, `insert or replace into product_categories values (?, ?, ?, ?, ?)`,
				retailer, product.String, c.Category, nullable(c.CategoryDetailed), at); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

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

// retailerMatches matches whole retailer payments on basic facts only.
// Both sides must be unique: two receipts competing for one transaction
// are as ambiguous as one receipt with two possible bank transactions.
// source, key and merchant are trusted SQL from each retailer's schema.
func retailerMatches(source, key string, days int, merchant string) string {
	return fmt.Sprintf(`with p as (%s),
bank as (
	select t.transaction_id, t.amount, coalesce(t.authorized_date, t.date) as day, nullif(a.mask, '') as mask
	from transactions t left join accounts a using (account_id)
	where regexp_matches(lower(coalesce(t.merchant_name, '') || ' ' || coalesce(t.name, '')), '%s')
),
cand as (
	select p.%s, b.transaction_id, count(*) over (partition by b.transaction_id) as per_transaction
	from p join bank b on b.amount = p.amount and b.day between p.date - %d and p.date + %d
		and (p.card_last4 is null or b.mask is null or b.mask = p.card_last4)
	where not p.no_bank
),
agg as (
	select %s, count(*) as candidates, any_value(transaction_id) as transaction_id,
		max(per_transaction) as per_transaction
	from cand group by %s
)
select p.* exclude (no_bank),
	case when p.no_bank then 'no_bank_charge'
		when agg.candidates is null then 'unmatched'
		when agg.candidates = 1 and agg.per_transaction = 1 then 'exact'
		else 'ambiguous' end as match,
	case when agg.candidates = 1 and agg.per_transaction = 1 then agg.transaction_id end as transaction_id,
	coalesce(agg.candidates, 0) as candidates
from p left join agg using (%s);`, source, merchant, key, days, days, key, key, key)
}
