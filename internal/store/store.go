// Package store keeps synced transactions in a local DuckDB file, one per
// Plaid environment, so reads are incremental and the data can be queried
// with SQL. Access tokens never go here; they stay in the Keychain.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/kilianc/fin/internal/plaid"
)

// ErrNotFound means no store has been created yet for this environment.
var ErrNotFound = errors.New("no local store yet")

// ErrBusy means another fin process holds the store open for writing.
var ErrBusy = errors.New("the local store is in use by another fin command")

// lockWait is how long Open waits for another fin process to release the file.
var lockWait = 10 * time.Second

const schema = `
create table if not exists items (
	item_id     varchar primary key,
	item        varchar not null,
	institution varchar,
	cursor      varchar not null,
	status      varchar,
	last_sync   timestamptz not null
);
create table if not exists accounts (
	account_id        varchar primary key,
	item_id           varchar not null,
	name              varchar not null,
	official_name     varchar,
	mask              varchar,
	type              varchar,
	subtype           varchar,
	current           decimal(18, 4),
	available         decimal(18, 4),
	"limit"           decimal(18, 4),
	iso_currency_code varchar,
	updated_at        timestamptz not null
);
create table if not exists transactions (
	transaction_id    varchar primary key,
	item_id           varchar not null,
	account_id        varchar not null,
	date              date not null,
	authorized_date   date,
	name              varchar not null,
	merchant_name     varchar,
	amount            decimal(18, 4) not null,
	iso_currency_code varchar,
	pending           boolean not null,
	category          varchar,
	category_detailed varchar,
	payment_channel   varchar
);
`

// Store is an open DuckDB file.
type Store struct {
	db *sql.DB
}

// Path is the store file for env inside fin's data directory.
func Path(dir, env string) string { return filepath.Join(dir, env+".duckdb") }

// DefaultDir is $FIN_DATA_DIR, else $XDG_DATA_HOME/fin, else
// ~/.local/share/fin. It is kept apart from ~/.config, which people often
// commit or sync as dotfiles.
func DefaultDir() (string, error) {
	if dir := os.Getenv("FIN_DATA_DIR"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "fin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "fin"), nil
}

// Open opens the store read-write, creating the file and schema if needed.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s, err := open(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing store for queries. It cannot write to the
// database or touch any other file, and the query cannot turn that back on.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return open(ctx, path+"?access_mode=READ_ONLY&enable_external_access=false&lock_configuration=true")
}

func open(ctx context.Context, dsn string) (*Store, error) {
	deadline := time.Now().Add(lockWait)
	for {
		db, err := sql.Open("duckdb", dsn)
		if err == nil {
			err = db.PingContext(ctx)
			if err == nil {
				return &Store{db: db}, nil
			}
			db.Close()
		}
		if !strings.Contains(err.Error(), "Could not set lock") {
			return nil, fmt.Errorf("store: open: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Store) Close() error { return s.db.Close() }

// Cursor returns the /transactions/sync cursor saved for an Item, or "" when
// the Item has never been synced into this store.
func (s *Store) Cursor(ctx context.Context, itemID string) (string, error) {
	var cursor string
	err := s.db.QueryRowContext(ctx, `select cursor from items where item_id = ?`, itemID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

// ItemSync is one Item's /transactions/sync delta.
type ItemSync struct {
	ItemID      string
	Item        string
	Institution string
	Cursor      string
	Status      string
	SyncedAt    time.Time
	Accounts    []plaid.Account
	Upserts     []plaid.Transaction
	Removed     []string
}

// Apply writes a delta and its cursor in one transaction, so a failed write
// leaves the old cursor in place and the next sync replays the same delta.
func (s *Store) Apply(ctx context.Context, d ItemSync) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	accStmt, err := tx.PrepareContext(ctx, `insert or replace into accounts values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer accStmt.Close()
	for _, a := range d.Accounts {
		b := a.Balances
		if _, err := accStmt.ExecContext(ctx, a.AccountID, d.ItemID, a.Name, a.OfficialName, a.Mask, a.Type, a.Subtype,
			b.Current, b.Available, b.Limit, b.IsoCurrencyCode, d.SyncedAt); err != nil {
			return fmt.Errorf("store: account %s: %w", a.AccountID, err)
		}
	}

	delStmt, err := tx.PrepareContext(ctx, `delete from transactions where transaction_id = ?`)
	if err != nil {
		return err
	}
	defer delStmt.Close()
	for _, id := range d.Removed {
		if _, err := delStmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("store: remove %s: %w", id, err)
		}
	}

	txStmt, err := tx.PrepareContext(ctx, `insert or replace into transactions values
		(?, ?, ?, ?::date, ?::date, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer txStmt.Close()
	for _, t := range d.Upserts {
		var category, detailed *string
		if c := t.PersonalFinanceCategory; c != nil {
			category, detailed = &c.Primary, &c.Detailed
		}
		if _, err := txStmt.ExecContext(ctx, t.TransactionID, d.ItemID, t.AccountID, t.Date, t.AuthorizedDate,
			t.Name, t.MerchantName, t.Amount, t.IsoCurrencyCode, t.Pending, category, detailed, t.PaymentChannel); err != nil {
			return fmt.Errorf("store: transaction %s: %w", t.TransactionID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `insert or replace into items values (?, ?, ?, ?, ?, ?)`,
		d.ItemID, d.Item, d.Institution, d.Cursor, d.Status, d.SyncedAt); err != nil {
		return fmt.Errorf("store: item %s: %w", d.ItemID, err)
	}
	return tx.Commit()
}

// Transaction is one stored transaction joined with its account and Item.
type Transaction struct {
	TransactionID    string  `json:"transaction_id"`
	Item             string  `json:"item"`
	Institution      string  `json:"institution"`
	AccountID        string  `json:"account_id"`
	AccountName      string  `json:"account_name"`
	AccountMask      *string `json:"account_mask"`
	Date             string  `json:"date"`
	AuthorizedDate   *string `json:"authorized_date"`
	Name             string  `json:"name"`
	MerchantName     *string `json:"merchant_name"`
	Amount           float64 `json:"amount"`
	IsoCurrencyCode  *string `json:"iso_currency_code"`
	Pending          bool    `json:"pending"`
	Category         *string `json:"category"`
	CategoryDetailed *string `json:"category_detailed"`
	PaymentChannel   string  `json:"payment_channel"`
}

// Filter selects transactions. Account matches an account_id, mask or name.
type Filter struct {
	ItemIDs  []string
	From, To string
	Account  string
}

// Transactions returns matching transactions, newest first.
func (s *Store) Transactions(ctx context.Context, f Filter) ([]Transaction, error) {
	out := []Transaction{}
	if len(f.ItemIDs) == 0 {
		return out, nil
	}
	q := `
		select t.transaction_id, i.item, coalesce(i.institution, ''), t.account_id,
			coalesce(a.name, ''), a.mask, t.date::varchar, t.authorized_date::varchar,
			t.name, t.merchant_name, t.amount::double, t.iso_currency_code, t.pending,
			t.category, t.category_detailed, coalesce(t.payment_channel, '')
		from transactions t
		join items i using (item_id)
		left join accounts a using (account_id)
		where t.item_id in (select unnest(?::varchar[]))
			and t.date between ?::date and ?::date
			and (? = '' or t.account_id = ? or a.mask = ? or lower(a.name) = lower(?))
		order by t.date desc, i.item, t.transaction_id`
	rows, err := s.db.QueryContext(ctx, q, f.ItemIDs, f.From, f.To, f.Account, f.Account, f.Account, f.Account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.TransactionID, &t.Item, &t.Institution, &t.AccountID, &t.AccountName, &t.AccountMask,
			&t.Date, &t.AuthorizedDate, &t.Name, &t.MerchantName, &t.Amount, &t.IsoCurrencyCode, &t.Pending,
			&t.Category, &t.CategoryDetailed, &t.PaymentChannel); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SyncInfo is when an Item was last synced into the store.
type SyncInfo struct {
	Item     string    `json:"item"`
	ItemID   string    `json:"item_id"`
	Status   string    `json:"transactions_update_status"`
	LastSync time.Time `json:"last_sync"`
}

// Syncs lists every Item in the store with its last sync time.
func (s *Store) Syncs(ctx context.Context) ([]SyncInfo, error) {
	rows, err := s.db.QueryContext(ctx, `select item, item_id, coalesce(status, ''), last_sync from items order by item`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SyncInfo{}
	for rows.Next() {
		var si SyncInfo
		if err := rows.Scan(&si.Item, &si.ItemID, &si.Status, &si.LastSync); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// QueryResult is the output of an ad hoc query: column names in order, and
// each row as values ready for JSON.
type QueryResult struct {
	Columns []string
	Types   []string
	Rows    [][]any
}

// Query runs one SQL statement and converts the values for JSON output.
func (s *Store) Query(ctx context.Context, query string) (*QueryResult, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Columns: []string{}, Types: []string{}, Rows: [][]any{}}
	for _, c := range cols {
		res.Columns = append(res.Columns, c.Name())
		res.Types = append(res.Types, c.DatabaseTypeName())
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			vals[i] = jsonValue(v, res.Types[i])
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// jsonValue turns driver values into plain JSON values: dates as
// YYYY-MM-DD, decimals as numbers, huge integers as numbers when they fit.
func jsonValue(v any, dbType string) any {
	switch x := v.(type) {
	case time.Time:
		if dbType == "DATE" {
			return x.Format("2006-01-02")
		}
		return x.Format(time.RFC3339Nano)
	case duckdb.Decimal:
		return x.Float64()
	case *big.Int:
		if x.IsInt64() {
			return x.Int64()
		}
		return x.String()
	case duckdb.UUID:
		return x.String()
	case []byte:
		return string(x)
	case []any:
		for i := range x {
			x[i] = jsonValue(x[i], "")
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = jsonValue(x[k], "")
		}
		return x
	}
	return v
}
