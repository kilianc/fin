package fin

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kilianc/fin/internal/state"
)

type recordReporter struct{ events []string }

func (r *recordReporter) Start(i int, detail string) {
	r.events = append(r.events, fmt.Sprintf("start %d %s", i, detail))
}
func (r *recordReporter) Done(i int, detail string) {
	r.events = append(r.events, fmt.Sprintf("done %d %s", i, detail))
}
func (r *recordReporter) Fail(i int, detail string) {
	r.events = append(r.events, fmt.Sprintf("fail %d %s", i, detail))
}
func (r *recordReporter) Link(string, string) {}
func (r *recordReporter) Feed(string)         {}

// Retailer accounts on fin sync's screen start after the Items' steps, and
// one that fails marks the step it was on.
func TestOnScreenOffsetsAndFailure(t *testing.T) {
	rep := &recordReporter{}
	sync := func(acct state.RetailerAccount, p syncProgress) (any, error) {
		p.step(0, "")
		if acct.Name == "b" {
			p.step(1, "")
			return nil, errors.New("signed out")
		}
		return acct.Name, nil
	}
	shown, progress := onScreen(rep, 10, 2, sync, func(v any) []string { return []string{v.(string) + " 1", v.(string) + " 2"} })
	for k, name := range []string{"a", "b"} {
		_, _ = shown(state.RetailerAccount{Name: name}, progress(k))
	}
	want := []string{
		"start 10 ", "done 10 a 1", "done 11 a 2",
		"start 12 ", "done 12 ", "start 13 ", "fail 13 ",
	}
	if !reflect.DeepEqual(rep.events, want) {
		t.Errorf("events\n got %q\nwant %q", rep.events, want)
	}
}

func TestSyncScreenNilIsQuiet(t *testing.T) {
	var p *syncScreen
	p.start(txsPart, 0)
	p.done(invsPart, 1, "x")
	p.failItems(txsPart, []state.Item{{ItemID: "i"}}, []ItemError{{ItemID: "i"}})
}
