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
create table if not exists investment_transactions (
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
	iso_currency_code         varchar
);
-- plaid, or the import a row came from, such as robinhood.
alter table investment_transactions add column if not exists source varchar default 'plaid';
`

// InvestmentSync is one Item's holdings today and its investment
// transactions from From on, which replace the stored ones in that window.
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

// InvestmentsFrom returns the date an Item's last stored investment
// transaction falls on, or "" when it has none.
func (s *Store) InvestmentsFrom(ctx context.Context, itemID string) (string, error) {
	var day sql.NullString
	err := s.db.QueryRowContext(ctx, `select max(date)::varchar from investment_transactions where item_id = ? and source = 'plaid'`, itemID).Scan(&day)
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
	// Plaid has no removed list for investment transactions, so the window
	// is replaced whole: one Plaid dropped disappears here too.
	if _, err := tx.ExecContext(ctx, `delete from investment_transactions where item_id = ? and date >= ?::date and source = 'plaid'`, d.ItemID, d.From); err != nil {
		return err
	}
	for _, t := range d.Txs {
		if _, err := tx.ExecContext(ctx, `insert or replace into investment_transactions values (?, ?, ?, ?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, 'plaid')`,
			t.InvestmentTransactionID, d.ItemID, t.AccountID, t.SecurityID, t.Date, t.Name, t.Type, t.Subtype,
			t.Quantity, t.Price, t.Amount, t.Fees, t.IsoCurrencyCode); err != nil {
			return fmt.Errorf("store: investment transaction %s: %w", t.InvestmentTransactionID, err)
		}
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

// ImportedInvestments is what ImportInvestments did.
type ImportedInvestments struct {
	Imported int    `json:"imported"`
	Covered  int    `json:"covered_by_plaid"` // rows from the day Plaid's history starts on, left to Plaid
	Since    string `json:"plaid_since"`      // that day, "" when Plaid has none for the account
}

// ImportInvestments replaces an account's rows from source with txs, keeping
// only those before the account's first Plaid row, so nothing is counted
// twice. securities adds any the rows need that fin does not have yet.
func (s *Store) ImportInvestments(ctx context.Context, acct InvestmentAccount, source string, txs []plaid.InvestmentTransaction, securities []plaid.Security, at time.Time) (ImportedInvestments, error) {
	var res ImportedInvestments
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	var since sql.NullString
	if err := tx.QueryRowContext(ctx, `select min(date)::varchar from investment_transactions where account_id = ? and source = 'plaid'`,
		acct.AccountID).Scan(&since); err != nil {
		return res, err
	}
	res.Since = since.String
	for _, sec := range securities {
		if _, err := tx.ExecContext(ctx, `insert into securities values (?, ?, ?, ?, ?, ?, ?, ?) on conflict do nothing`,
			sec.SecurityID, sec.TickerSymbol, sec.Name, sec.Type, sec.IsCashEquivalent, sec.ClosePrice, sec.IsoCurrencyCode, at); err != nil {
			return res, fmt.Errorf("store: security %s: %w", sec.SecurityID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `delete from investment_transactions where account_id = ? and source = ?`, acct.AccountID, source); err != nil {
		return res, err
	}
	for _, t := range txs {
		if res.Since != "" && t.Date >= res.Since {
			res.Covered++
			continue
		}
		if _, err := tx.ExecContext(ctx, `insert or replace into investment_transactions values (?, ?, ?, ?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.InvestmentTransactionID, acct.ItemID, acct.AccountID, t.SecurityID, t.Date, t.Name, t.Type, t.Subtype,
			t.Quantity, t.Price, t.Amount, t.Fees, t.IsoCurrencyCode, source); err != nil {
			return res, fmt.Errorf("store: imported row %s: %w", t.InvestmentTransactionID, err)
		}
		res.Imported++
	}
	return res, tx.Commit()
}
