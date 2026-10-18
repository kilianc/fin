package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kilianc/fin/internal/plaid"
)

const investmentsSchema = `
create table if not exists securities (
	security_id        varchar primary key,
	ticker             varchar,
	name               varchar,
	type               varchar,
	is_cash_equivalent boolean,
	close_price        double,
	iso_currency_code  varchar,
	updated_at         timestamptz not null
);
-- One snapshot of an Item's positions per day: the last sync of the day
-- replaces that day's rows, and earlier days stay as history.
create table if not exists holdings (
	date              date not null,
	item_id           varchar not null,
	account_id        varchar not null,
	security_id       varchar not null,
	quantity          double not null,
	price             double not null,
	price_as_of       date,
	value             double not null,
	cost_basis        double,
	iso_currency_code varchar,
	tax_lots          json,
	primary key (date, account_id, security_id)
);
-- What Plaid sent, as it sent it. Plaid has no removed list for investment
-- transactions, so a row a later sync's window no longer has gets a
-- removed_at instead of being deleted; it clears if the row comes back.
create table if not exists plaid_investment_transactions (
	investment_transaction_id varchar primary key,
	item_id                   varchar not null,
	account_id                varchar not null,
	security_id               varchar,
	date                      date not null,
	name                      varchar not null,
	type                      varchar not null,
	subtype                   varchar,
	quantity                  double not null,
	price                     double not null,
	amount                    double not null,
	fees                      double,
	iso_currency_code         varchar,
	first_seen                timestamptz,
	last_seen                 timestamptz,
	removed_at                timestamptz
);
-- Every row of every Robinhood account activity report imported, with its
-- text as Robinhood wrote it and fin's reading of it beside, upserted by key.
create table if not exists robinhood_activity (
	key           varchar primary key,
	item_id       varchar not null,
	account_id    varchar not null,
	activity_date date not null,
	process_date  date,
	settle_date   date,
	instrument    varchar,
	description   varchar,
	trans_code    varchar,
	quantity_text varchar,
	price_text    varchar,
	amount_text   varchar,
	security_id   varchar,
	name          varchar not null,
	type          varchar not null,
	subtype       varchar,
	quantity      double not null,
	price         double not null,
	amount        double not null,
	first_imported timestamptz not null,
	last_imported  timestamptz not null
);
-- One row per report file imported: the dates its rows span are the dates
-- the report speaks for.
create table if not exists robinhood_reports (
	report_id   varchar not null,
	account_id  varchar not null,
	item_id     varchar not null,
	file        varchar,
	rows        integer not null,
	first_date  date,
	last_date   date,
	imported_at timestamptz not null,
	primary key (report_id, account_id)
);
`

// investmentsViews resolves overlapping sources when reading. A Robinhood
// report is the fuller record (one row per fill, and payouts Plaid leaves
// out), so it speaks for the dates its rows span; Plaid's rows on those dates
// stay in plaid_investment_transactions but are left out. Just past a
// report's last day, a Plaid row with the same amount as a report row within
// a week is the same activity dated differently, and is left out too.
const investmentsViews = `
create or replace view investment_matches as
with plaid as (
	select * from plaid_investment_transactions where removed_at is null
),
covered as (
	select p.investment_transaction_id, min(r.report_id) as report_id
	from plaid p join robinhood_reports r
		on r.account_id = p.account_id and p.date between r.first_date and r.last_date
	group by p.investment_transaction_id
),
pairs as (
	select p.investment_transaction_id, a.key, p.date, a.activity_date,
		row_number() over (partition by p.investment_transaction_id order by abs(p.date - a.activity_date), a.key) as by_plaid,
		row_number() over (partition by a.key order by abs(p.date - a.activity_date), p.investment_transaction_id) as by_report
	from plaid p join robinhood_activity a
		on a.account_id = p.account_id and round(a.amount, 2) = round(p.amount, 2)
		and a.activity_date between p.date - 7 and p.date + 7
	where p.investment_transaction_id not in (select investment_transaction_id from covered)
)
select p.investment_transaction_id as plaid_id, p.item_id, p.account_id, p.date, p.name, p.amount,
	case when c.report_id is not null then 'robinhood report covers this date' else 'same amount in a robinhood report' end as reason,
	c.report_id, m.key as robinhood_key
from plaid p
left join covered c using (investment_transaction_id)
left join (select * from pairs where by_plaid = 1 and by_report = 1) m using (investment_transaction_id)
where c.report_id is not null or m.key is not null;

create or replace view investment_transactions as
select investment_transaction_id, item_id, account_id, security_id, date, name, type, subtype,
	quantity, price, amount, fees, iso_currency_code, 'plaid' as source
from plaid_investment_transactions
where removed_at is null and investment_transaction_id not in (select plaid_id from investment_matches)
union all
select key, item_id, account_id, security_id, activity_date, name, type, subtype,
	quantity, price, amount, null, 'USD', 'robinhood'
from robinhood_activity;
`

// migrateInvestmentTransactions moves a store whose investment_transactions
// is a table, from before the raw tables, into them: Plaid's rows (all of
// them when there is no source column) to plaid_investment_transactions, and
// imported Robinhood rows to robinhood_activity, without the report text it
// did not keep. Nothing is dropped until both copies are in.
func migrateInvestmentTransactions(ctx context.Context, db *sql.DB) error {
	var kind string
	err := db.QueryRowContext(ctx, `select table_type from information_schema.tables where table_name = 'investment_transactions'`).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) || kind != "BASE TABLE" {
		return nil
	}
	if err != nil {
		return err
	}
	var hasSource int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.columns
		where table_name = 'investment_transactions' and column_name = 'source'`).Scan(&hasSource); err != nil {
		return err
	}
	source := "'plaid'"
	if hasSource > 0 {
		source = "coalesce(source, 'plaid')"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		investmentsSchema,
		`insert into plaid_investment_transactions
			select investment_transaction_id, item_id, account_id, security_id, date, name, type, subtype,
				quantity, price, amount, fees, iso_currency_code, null, null, null
			from investment_transactions where ` + source + ` = 'plaid'
			on conflict do nothing`,
		`insert into robinhood_activity (key, item_id, account_id, activity_date, security_id, name, type, subtype,
				quantity, price, amount, first_imported, last_imported)
			select investment_transaction_id, item_id, account_id, date, security_id, name, type, subtype,
				quantity, price, amount, now(), now()
			from investment_transactions where ` + source + ` = 'robinhood'
			on conflict do nothing`,
		`drop table investment_transactions`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("store: move investment transactions to their raw tables: %w", err)
		}
	}
	return tx.Commit()
}

// InvestmentSync is one Item's holdings today and its investment
// transactions from From on, Plaid's whole answer for that window.
type InvestmentSync struct {
	ItemID      string
	Item        string
	Institution string
	Day         string // the holdings snapshot's date, YYYY-MM-DD
	SyncedAt    time.Time
	Accounts    []plaid.Account
	Securities  []plaid.Security
	Holdings    []plaid.Holding
	From        string // YYYY-MM-DD
	Txs         []plaid.InvestmentTransaction
}

// InvestmentsFrom returns the date of the last investment transaction Plaid
// sent for an Item, or "" when it has sent none.
func (s *Store) InvestmentsFrom(ctx context.Context, itemID string) (string, error) {
	var day sql.NullString
	err := s.db.QueryRowContext(ctx, `select max(date)::varchar from plaid_investment_transactions where item_id = ? and removed_at is null`, itemID).Scan(&day)
	return day.String, err
}

// ApplyInvestments writes an Item's holdings snapshot and investment
// transactions in one transaction.
func (s *Store) ApplyInvestments(ctx context.Context, d InvestmentSync) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertAccounts(ctx, tx, d.ItemID, d.Accounts, d.SyncedAt, d.Day); err != nil {
		return err
	}
	for _, sec := range d.Securities {
		if _, err := tx.ExecContext(ctx, `insert or replace into securities values (?, ?, ?, ?, ?, ?, ?, ?)`,
			sec.SecurityID, sec.TickerSymbol, sec.Name, sec.Type, sec.IsCashEquivalent, sec.ClosePrice, sec.IsoCurrencyCode, d.SyncedAt); err != nil {
			return fmt.Errorf("store: security %s: %w", sec.SecurityID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `delete from holdings where item_id = ? and date = ?::date`, d.ItemID, d.Day); err != nil {
		return err
	}
	for _, h := range d.Holdings {
		lots, _ := json.Marshal(h.TaxLots)
		if h.TaxLots == nil {
			lots = []byte("[]")
		}
		var asOf *string
		if h.InstitutionPriceAsOf != nil && len(*h.InstitutionPriceAsOf) >= 10 {
			day := (*h.InstitutionPriceAsOf)[:10]
			asOf = &day
		}
		if _, err := tx.ExecContext(ctx, `insert or replace into holdings values (?::date, ?, ?, ?, ?, ?, ?::date, ?, ?, ?, ?::json)`,
			d.Day, d.ItemID, h.AccountID, h.SecurityID, h.Quantity, h.InstitutionPrice, asOf, h.InstitutionValue,
			h.CostBasis, h.IsoCurrencyCode, string(lots)); err != nil {
			return fmt.Errorf("store: holding %s/%s: %w", h.AccountID, h.SecurityID, err)
		}
	}
	for _, t := range d.Txs {
		if _, err := tx.ExecContext(ctx, `insert into plaid_investment_transactions values (?, ?, ?, ?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, null)
			on conflict (investment_transaction_id) do update set item_id = excluded.item_id, account_id = excluded.account_id,
				security_id = excluded.security_id, date = excluded.date, name = excluded.name, type = excluded.type,
				subtype = excluded.subtype, quantity = excluded.quantity, price = excluded.price, amount = excluded.amount,
				fees = excluded.fees, iso_currency_code = excluded.iso_currency_code, last_seen = excluded.last_seen, removed_at = null`,
			t.InvestmentTransactionID, d.ItemID, t.AccountID, t.SecurityID, t.Date, t.Name, t.Type, t.Subtype,
			t.Quantity, t.Price, t.Amount, t.Fees, t.IsoCurrencyCode, d.SyncedAt, d.SyncedAt); err != nil {
			return fmt.Errorf("store: investment transaction %s: %w", t.InvestmentTransactionID, err)
		}
	}
	// Plaid has no removed list for investment transactions: one in the
	// window that this answer no longer has is marked removed, and kept.
	sent := make([]string, len(d.Txs))
	for i, t := range d.Txs {
		sent[i] = t.InvestmentTransactionID
	}
	if _, err := tx.ExecContext(ctx, `update plaid_investment_transactions set removed_at = ?
		where item_id = ? and date >= ?::date and removed_at is null
			and investment_transaction_id not in (select unnest(?::varchar[]))`,
		d.SyncedAt, d.ItemID, d.From, sent); err != nil {
		return err
	}
	// An Item linked for investments only has no transactions cursor; keep
	// one it has, and note when it last synced.
	if _, err := tx.ExecContext(ctx, `insert into items values (?, ?, ?, '', null, ?)
		on conflict (item_id) do update set item = excluded.item, institution = excluded.institution,
			last_sync = greatest(items.last_sync, excluded.last_sync)`,
		d.ItemID, d.Item, d.Institution, d.SyncedAt); err != nil {
		return fmt.Errorf("store: item %s: %w", d.ItemID, err)
	}
	return tx.Commit()
}

// Holding is one position from an Item's latest holdings snapshot.
type Holding struct {
	Date            string          `json:"date"`
	Item            string          `json:"item"`
	Institution     string          `json:"institution"`
	AccountID       string          `json:"account_id"`
	AccountName     string          `json:"account_name"`
	SecurityID      string          `json:"security_id"`
	Ticker          *string         `json:"ticker"`
	SecurityName    *string         `json:"security_name"`
	SecurityType    *string         `json:"security_type"`
	Quantity        float64         `json:"quantity"`
	Price           float64         `json:"price"`
	PriceAsOf       *string         `json:"price_as_of"`
	Value           float64         `json:"value"`
	CostBasis       *float64        `json:"cost_basis"`
	IsoCurrencyCode *string         `json:"iso_currency_code"`
	TaxLots         json.RawMessage `json:"tax_lots"`
}

// Holdings returns each Item's most recent snapshot, largest positions first.
func (s *Store) Holdings(ctx context.Context, itemIDs []string, account string) ([]Holding, error) {
	out := []Holding{}
	// A store last written by a fin older than investments has no holdings.
	if ok, err := s.hasTable(ctx, "holdings"); err != nil || !ok || len(itemIDs) == 0 {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `
		select h.date::varchar, i.item, coalesce(i.institution, ''), h.account_id, coalesce(a.name, ''), h.security_id,
			sec.ticker, sec.name, sec.type, h.quantity, h.price, h.price_as_of::varchar, h.value, h.cost_basis,
			h.iso_currency_code, coalesce(h.tax_lots::varchar, '[]')
		from holdings h
		join (select item_id, max(date) as date from holdings group by item_id) latest using (item_id, date)
		join items i using (item_id)
		left join accounts a using (account_id)
		left join securities sec using (security_id)
		where h.item_id in (select unnest(?::varchar[]))
			and (? = '' or h.account_id = ? or a.mask = ? or lower(a.name) = lower(?))
		order by i.item, a.name, h.value desc, h.security_id`, itemIDs, account, account, account, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h Holding
		var lots string
		if err := rows.Scan(&h.Date, &h.Item, &h.Institution, &h.AccountID, &h.AccountName, &h.SecurityID,
			&h.Ticker, &h.SecurityName, &h.SecurityType, &h.Quantity, &h.Price, &h.PriceAsOf, &h.Value, &h.CostBasis,
			&h.IsoCurrencyCode, &lots); err != nil {
			return nil, err
		}
		h.TaxLots = json.RawMessage(lots)
		out = append(out, h)
	}
	return out, rows.Err()
}

// InvestmentTransaction is one stored investment transaction.
type InvestmentTransaction struct {
	InvestmentTransactionID string   `json:"investment_transaction_id"`
	Item                    string   `json:"item"`
	Institution             string   `json:"institution"`
	AccountID               string   `json:"account_id"`
	AccountName             string   `json:"account_name"`
	Date                    string   `json:"date"`
	Name                    string   `json:"name"`
	Type                    string   `json:"type"`
	Subtype                 string   `json:"subtype"`
	SecurityID              *string  `json:"security_id"`
	Ticker                  *string  `json:"ticker"`
	SecurityName            *string  `json:"security_name"`
	Quantity                float64  `json:"quantity"`
	Price                   float64  `json:"price"`
	Amount                  float64  `json:"amount"`
	Fees                    *float64 `json:"fees"`
	IsoCurrencyCode         *string  `json:"iso_currency_code"`
	// Source is plaid, or the import the row came from, such as robinhood.
	Source string `json:"source"`
}

// InvestmentTransactions returns matching investment transactions, newest first.
func (s *Store) InvestmentTransactions(ctx context.Context, f Filter) ([]InvestmentTransaction, error) {
	out := []InvestmentTransaction{}
	if len(f.ItemIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		select t.investment_transaction_id, i.item, coalesce(i.institution, ''), t.account_id, coalesce(a.name, ''),
			t.date::varchar, t.name, t.type, coalesce(t.subtype, ''), t.security_id, sec.ticker, sec.name,
			t.quantity, t.price, t.amount, t.fees, t.iso_currency_code, t.source
		from investment_transactions t
		join items i using (item_id)
		left join accounts a using (account_id)
		left join securities sec using (security_id)
		where t.item_id in (select unnest(?::varchar[]))
			and t.date between ?::date and ?::date
			and (? = '' or t.account_id = ? or a.mask = ? or lower(a.name) = lower(?))
		order by t.date desc, i.item, t.investment_transaction_id`,
		f.ItemIDs, f.From, f.To, f.Account, f.Account, f.Account, f.Account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t InvestmentTransaction
		if err := rows.Scan(&t.InvestmentTransactionID, &t.Item, &t.Institution, &t.AccountID, &t.AccountName,
			&t.Date, &t.Name, &t.Type, &t.Subtype, &t.SecurityID, &t.Ticker, &t.SecurityName,
			&t.Quantity, &t.Price, &t.Amount, &t.Fees, &t.IsoCurrencyCode, &t.Source); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InvestmentAccount is an investment account fin has synced.
type InvestmentAccount struct {
	AccountID   string `json:"account_id"`
	ItemID      string `json:"item_id"`
	Item        string `json:"item"`
	Institution string `json:"institution"`
	Name        string `json:"name"`
	Mask        string `json:"mask"`
}

// InvestmentAccounts lists the stored investment accounts.
func (s *Store) InvestmentAccounts(ctx context.Context) ([]InvestmentAccount, error) {
	rows, err := s.db.QueryContext(ctx, `
		select a.account_id, a.item_id, i.item, coalesce(i.institution, ''), a.name, coalesce(a.mask, '')
		from accounts a join items i using (item_id)
		where a.type = 'investment' order by i.item, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InvestmentAccount{}
	for rows.Next() {
		var a InvestmentAccount
		if err := rows.Scan(&a.AccountID, &a.ItemID, &a.Item, &a.Institution, &a.Name, &a.Mask); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SecurityByTicker returns the security_id stored for a ticker, or "".
func (s *Store) SecurityByTicker(ctx context.Context, ticker string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `select security_id from securities where upper(ticker) = upper(?) order by updated_at desc limit 1`, ticker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RobinhoodReport is one account activity report file being imported.
type RobinhoodReport struct {
	ID    string // a hash of the file's bytes
	File  string
	Rows  int
	First string // the dates its rows span, YYYY-MM-DD
	Last  string
}

// RobinhoodRow is one report row: Robinhood's text, and fin's reading of it
// with Plaid's signs (a positive Amount is money out).
type RobinhoodRow struct {
	Key                                 string
	ActivityDate                        string
	ProcessDate, SettleDate             string // "" when the report leaves them blank
	Instrument, Description, TransCode  string
	QuantityText, PriceText, AmountText string
	SecurityID                          *string
	Name, Type, Subtype                 string
	Quantity, Price, Amount             float64
}

// RobinhoodImport is what ImportRobinhood did.
type RobinhoodImport struct {
	New   int `json:"new"`              // rows fin did not have
	Known int `json:"already_imported"` // rows an earlier import brought in, refreshed
	// PlaidReplaced is how many of the account's Plaid rows the account's
	// reports now speak for; they stay in plaid_investment_transactions.
	PlaidReplaced int `json:"plaid_rows_replaced"`
}

// ImportRobinhood upserts an account's report rows and records the reports.
// It never removes a row: a report that lacks one an earlier report had says
// nothing about it. securities adds any the rows need that fin lacks.
func (s *Store) ImportRobinhood(ctx context.Context, acct InvestmentAccount, reports []RobinhoodReport, rows []RobinhoodRow, securities []plaid.Security, at time.Time) (RobinhoodImport, error) {
	var res RobinhoodImport
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, sec := range securities {
		if _, err := tx.ExecContext(ctx, `insert into securities values (?, ?, ?, ?, ?, ?, ?, ?) on conflict do nothing`,
			sec.SecurityID, sec.TickerSymbol, sec.Name, sec.Type, sec.IsCashEquivalent, sec.ClosePrice, sec.IsoCurrencyCode, at); err != nil {
			return res, fmt.Errorf("store: security %s: %w", sec.SecurityID, err)
		}
	}
	for _, r := range reports {
		if _, err := tx.ExecContext(ctx, `insert or replace into robinhood_reports values (?, ?, ?, ?, ?, ?::date, ?::date, ?)`,
			r.ID, acct.AccountID, acct.ItemID, r.File, r.Rows, nullable(r.First), nullable(r.Last), at); err != nil {
			return res, fmt.Errorf("store: report %s: %w", r.File, err)
		}
	}
	for _, r := range rows {
		var known int
		if err := tx.QueryRowContext(ctx, `select count(*) from robinhood_activity where key = ?`, r.Key).Scan(&known); err != nil {
			return res, err
		}
		if _, err := tx.ExecContext(ctx, `insert into robinhood_activity values (?, ?, ?, ?::date, ?::date, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			on conflict (key) do update set process_date = excluded.process_date, settle_date = excluded.settle_date,
				instrument = excluded.instrument, description = excluded.description, trans_code = excluded.trans_code,
				quantity_text = excluded.quantity_text, price_text = excluded.price_text, amount_text = excluded.amount_text,
				security_id = excluded.security_id, name = excluded.name, type = excluded.type, subtype = excluded.subtype,
				quantity = excluded.quantity, price = excluded.price, amount = excluded.amount, last_imported = excluded.last_imported`,
			r.Key, acct.ItemID, acct.AccountID, r.ActivityDate, nullable(r.ProcessDate), nullable(r.SettleDate),
			r.Instrument, r.Description, r.TransCode, r.QuantityText, r.PriceText, r.AmountText,
			r.SecurityID, r.Name, r.Type, r.Subtype, r.Quantity, r.Price, r.Amount, at, at); err != nil {
			return res, fmt.Errorf("store: report row %s: %w", r.Key, err)
		}
		if known > 0 {
			res.Known++
		} else {
			res.New++
		}
	}
	if err := tx.QueryRowContext(ctx, `select count(*) from investment_matches where account_id = ?`, acct.AccountID).Scan(&res.PlaidReplaced); err != nil {
		return res, err
	}
	return res, tx.Commit()
}
