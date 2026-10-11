package fin

import (
	"context"
	"time"

	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/store"
)

const (
	// investmentsHistoryDays is how far back the first sync reads investment
	// transactions: Plaid keeps 24 months.
	investmentsHistoryDays = 730
	// investmentsOverlapDays is how much of the stored history later syncs
	// read again, since a brokerage can post or correct a trade late.
	investmentsOverlapDays = 30
)

type investmentsView struct {
	Item         string     `json:"item"`
	Holdings     int        `json:"holdings"`
	Transactions int        `json:"investment_transactions"`
	LastSync     *time.Time `json:"last_sync"`
}

// syncInvestments stores each Item's holdings snapshot and recent investment
// transactions. Fetches run in parallel; writes do not.
func (a *App) syncInvestments(ctx context.Context, s *store.Store, items []state.Item, api Plaid) ([]investmentsView, []ItemError, error) {
	today := a.Now()
	from := make([]string, len(items))
	for i, it := range items {
		last, err := s.InvestmentsFrom(ctx, it.ItemID)
		if err != nil {
			return nil, nil, storeErr(err)
		}
		start := today.AddDate(0, 0, -investmentsHistoryDays)
		if t, err := time.Parse(dateLayout, last); err == nil && t.AddDate(0, 0, -investmentsOverlapDays).After(start) {
			start = t.AddDate(0, 0, -investmentsOverlapDays)
		}
		from[i] = start.Format(dateLayout)
	}
	syncs := make([]*store.InvestmentSync, len(items))
	errs := a.forEachItem(ctx, items, func(ctx context.Context, i int, token string) error {
		h, err := api.InvestmentsHoldingsGet(ctx, token)
		if err != nil {
			return err
		}
		d := &store.InvestmentSync{
			ItemID: items[i].ItemID, Item: items[i].Name, Institution: items[i].InstitutionName,
			Day: today.Format(dateLayout), Accounts: h.Accounts, Securities: h.Securities, Holdings: h.Holdings, From: from[i],
		}
		securities := map[string]bool{}
		for _, sec := range h.Securities {
			securities[sec.SecurityID] = true
		}
		for {
			page, err := api.InvestmentsTransactionsGet(ctx, token, d.From, today.Format(dateLayout), len(d.Txs), invPageSize)
			if err != nil {
				return err
			}
			d.Txs = append(d.Txs, page.InvestmentTransactions...)
			for _, sec := range page.Securities {
				if !securities[sec.SecurityID] {
					securities[sec.SecurityID] = true
					d.Securities = append(d.Securities, sec)
				}
			}
			if len(page.InvestmentTransactions) == 0 || len(d.Txs) >= page.TotalInvestmentTransactions {
				break
			}
		}
		syncs[i] = d
		return nil
	})
	views := []investmentsView{}
	for _, d := range syncs {
		if d == nil {
			continue
		}
		now := a.Now().UTC()
		d.SyncedAt = now
		if err := s.ApplyInvestments(ctx, *d); err != nil {
			return nil, nil, storeErr(err)
		}
		views = append(views, investmentsView{Item: d.Item, Holdings: len(d.Holdings), Transactions: len(d.Txs), LastSync: &now})
	}
	return views, errs, nil
}

// readInvestments syncs the investment Items into the local file and hands
// it to read, for fin holdings and fin investments.
func (a *App) readInvestments(ctx context.Context, read func(s *store.Store, ids []string) error) ([]investmentsView, []ItemError, error) {
	_, items, api, err := a.readSetup("investments")
	if err != nil || len(items) == 0 {
		return []investmentsView{}, []ItemError{}, err
	}
	s, err := a.openStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer s.Close()
	views, errs, err := a.syncInvestments(ctx, s, items, api)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ItemID
	}
	if err := read(s, ids); err != nil {
		return nil, nil, storeErr(err)
	}
	return views, errs, nil
}
