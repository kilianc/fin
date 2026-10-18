package fin

import (
	"context"
	"errors"
	"fmt"

	"github.com/kilianc/fin/internal/state"
	"github.com/kilianc/fin/internal/ui"
)

// syncScreen is fin sync on one full screen: a step for each Item's
// transactions, each Item's investments and each step of each retailer
// account, in the order fin sync works through them. Its methods do nothing
// on a nil *syncScreen, so the sync code calls them unconditionally.
type syncScreen struct {
	rep    ui.Reporter
	labels []string
	items  [2]int         // the first step of each itemPart
	shops  map[string]int // each retailer's first step, by ID
}

// itemPart is which of an Item's steps: its transactions or its investments.
type itemPart int

const (
	txsPart itemPart = iota
	invsPart
)

func newSyncScreen(a *App, st *state.State, items, invItems []state.Item) *syncScreen {
	p := &syncScreen{shops: map[string]int{}}
	p.items[txsPart] = len(p.labels)
	for _, it := range items {
		p.labels = append(p.labels, it.Name)
	}
	p.items[invsPart] = len(p.labels)
	for _, it := range invItems {
		p.labels = append(p.labels, it.Name+" investments")
	}
	for _, r := range retailers {
		accts := st.RetailerAccounts(r.ID, string(a.Env))
		if len(accts) == 0 {
			continue
		}
		p.shops[r.ID] = len(p.labels)
		for _, acct := range accts {
			for _, step := range r.Steps {
				p.labels = append(p.labels, r.ID+"/"+acct.Name+": "+step)
			}
		}
	}
	return p
}

// run shows the screen while work runs, and closes it when work ends: fin
// sync's table reports what it read.
func (p *syncScreen) run(ctx context.Context, a *App, work func(ctx context.Context) error) error {
	a.onScreen = true
	defer func() { a.onScreen = false }()
	_, err := ui.Flow(ctx, a.Stdin, a.Stderr, ui.FlowOptions{Title: "fin → sync", Steps: p.labels, CloseWhenDone: true},
		func(ctx context.Context, rep ui.Reporter) (ui.FlowResult, error) {
			p.rep = rep
			err := work(ctx)
			if err == nil {
				// q cancels Plaid's requests, which come back as Item errors.
				err = ctx.Err()
			}
			return ui.FlowResult{Message: "Done."}, err
		})
	if errors.Is(err, ui.ErrCancelled) {
		err = context.Canceled
	}
	return err
}

// start marks part of the i-th Item as running.
func (p *syncScreen) start(part itemPart, i int) {
	if p != nil {
		p.rep.Start(p.items[part]+i, "")
	}
}

// done marks part of the i-th Item as read.
func (p *syncScreen) done(part itemPart, i int, detail string) {
	if p != nil {
		p.rep.Done(p.items[part]+i, detail)
	}
}

// failItems marks part of each Item that failed.
func (p *syncScreen) failItems(part itemPart, items []state.Item, errs []ItemError) {
	if p == nil {
		return
	}
	for _, e := range errs {
		for i, it := range items {
			if it.ItemID == e.ItemID {
				p.rep.Fail(p.items[part]+i, e.Code)
			}
		}
	}
}

func changedDetail(changed, removed int) string {
	if removed == 0 {
		return fmt.Sprintf("%d changed", changed)
	}
	return fmt.Sprintf("%d changed, %d removed", changed, removed)
}
