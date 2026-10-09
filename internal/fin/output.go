package fin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/ui"
)

const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitPartial = 3
)

// CLIError is a command failure, printed to stderr as {"error": {...}}.
type CLIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	exit    int
}

func (e *CLIError) Error() string { return e.Code + ": " + e.Message }

func newErr(code, format string, args ...any) *CLIError {
	return &CLIError{Code: code, Message: fmt.Sprintf(format, args...), exit: exitError}
}

func usageErr(format string, args ...any) *CLIError {
	return &CLIError{Code: "USAGE", Message: fmt.Sprintf(format, args...), exit: exitUsage}
}

// asCLIError maps any error to a CLIError with a stable code.
func asCLIError(err error) *CLIError {
	var cerr *CLIError
	if errors.As(err, &cerr) {
		return cerr
	}
	var perr *plaid.Error
	if errors.As(err, &perr) {
		return &CLIError{
			Code:    perr.Code,
			Message: perr.Message,
			Details: map[string]any{"plaid_error_type": perr.Type, "request_id": perr.RequestID},
			exit:    exitError,
		}
	}
	if errors.Is(err, context.Canceled) {
		return newErr("CANCELLED", "cancelled")
	}
	return newErr("INTERNAL", "%v", err)
}

// ItemError is one Item's failure inside an otherwise successful read.
type ItemError struct {
	Item        string `json:"item"`
	ItemID      string `json:"item_id"`
	Institution string `json:"institution"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Action      string `json:"action,omitempty"`
}

// result is a command's output: a JSON body for agents, a table or a line
// for humans, and the per-Item errors already embedded in the body.
type result struct {
	body    any
	table   *ui.Table
	message string
	errors  []ItemError
}

func (a *App) emit(res *result) int {
	switch {
	case a.human && res.table != nil:
		ui.Print(a.Stdout, res.table.Render())
	case a.human && res.message != "":
		ui.Print(a.Stdout, res.message+"\n")
	default:
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res.body); err != nil {
			return a.fail(err)
		}
	}
	if a.human {
		for _, e := range res.errors {
			msg := ui.Bold.Render(e.Item) + "  " + e.Message
			if e.Action != "" && !strings.Contains(e.Message, e.Action) {
				msg += ui.Muted.Render("  → " + e.Action)
			}
			ui.Print(a.Stderr, ui.Line(ui.Warn, msg)+"\n")
		}
	}
	if len(res.errors) > 0 {
		return exitPartial
	}
	return exitOK
}

func (a *App) fail(err error) int {
	cerr := asCLIError(err)
	if a.human {
		ui.Print(a.Stderr, ui.Line(ui.Bad, cerr.Message)+ui.Muted.Render("  "+cerr.Code)+"\n")
	} else {
		enc := json.NewEncoder(a.Stderr)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"error": cerr})
	}
	return cerr.exit
}

func fmtMoney(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
