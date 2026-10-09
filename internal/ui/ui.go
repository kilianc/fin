// Package ui renders fin's human-facing output: tables, notices, a spinner
// while waiting on Plaid Link, and prompts. Agents get JSON instead and
// never see any of this.
package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

// Brand colors come from the mascot (mascot.png), softened so text in them
// is easy on the eyes:
//
//	Fin Blue  #81B1CF  the shark's body; the brand color
//	Fin Deep  #356B8D  the shark's shading; Fin Blue on light terminals
//	Fin Navy  #00184B  the badge
//	Fin Glow  #8CD9D6  the badge ring, for highlights on the web
//
// Green is reserved for meaning, not branding: money in, gains, healthy.
// Hex colors are downsampled to whatever the terminal supports.
var (
	accent = lipgloss.Color("#81B1CF")
	good   = lipgloss.Color("#3FB97A")
	yellow = lipgloss.Color("#E2A33B")
	red    = lipgloss.Color("#E5484D")
	gray   = lipgloss.Color("#7D8A99")
)

var (
	Bold   = lipgloss.NewStyle().Bold(true)
	Muted  = lipgloss.NewStyle().Foreground(gray)
	Accent = lipgloss.NewStyle().Foreground(accent)
	Brand  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	title  = lipgloss.NewStyle().Bold(true).Foreground(accent)
)

// SetDarkBackground picks brand shades for the terminal's background. Call
// it once at startup; the default assumes a dark terminal.
func SetDarkBackground(dark bool) {
	if dark {
		return
	}
	accent = lipgloss.Color("#356B8D")
	good = lipgloss.Color("#1F8A55")
	gray = lipgloss.Color("#5F6B78")
	Muted = lipgloss.NewStyle().Foreground(gray)
	Accent = lipgloss.NewStyle().Foreground(accent)
	Brand = lipgloss.NewStyle().Foreground(accent).Bold(true)
	title = lipgloss.NewStyle().Bold(true).Foreground(accent)
}

// Tone colors a table cell or a line by meaning rather than by color.
type Tone int

const (
	Plain Tone = iota
	Good
	Warn
	Bad
	Dim
)

func (t Tone) Style() lipgloss.Style {
	switch t {
	case Good:
		return lipgloss.NewStyle().Foreground(good)
	case Warn:
		return lipgloss.NewStyle().Foreground(yellow)
	case Bad:
		return lipgloss.NewStyle().Foreground(red)
	case Dim:
		return Muted
	}
	return lipgloss.NewStyle()
}

// Table is a titled table with right-aligned number columns and optional
// per-cell tones.
type Table struct {
	Title   string
	Headers []string
	Rows    [][]string
	Right   []int // columns to right-align
	Tone    func(row, col int) Tone
	Footer  string
}

func (t *Table) Render() string {
	var b strings.Builder
	if t.Title != "" {
		b.WriteString(title.Render(t.Title) + "\n")
	}
	if len(t.Rows) == 0 {
		b.WriteString(Muted.Render("Nothing to show.") + "\n")
	} else {
		right := map[int]bool{}
		for _, c := range t.Right {
			right[c] = true
		}
		tbl := table.New().
			Border(lipgloss.RoundedBorder()).
			BorderStyle(Muted).
			Headers(t.Headers...).
			Rows(t.Rows...).
			StyleFunc(func(row, col int) lipgloss.Style {
				s := lipgloss.NewStyle().Padding(0, 1)
				if right[col] {
					s = s.Align(lipgloss.Right)
				}
				if row == table.HeaderRow {
					return s.Bold(true).Foreground(accent)
				}
				if t.Tone != nil {
					if fg := t.Tone(row, col).Style().GetForeground(); fg != nil {
						s = s.Foreground(fg)
					}
				}
				return s
			})
		b.WriteString(tbl.Render() + "\n")
	}
	if t.Footer != "" {
		b.WriteString(Muted.Render(t.Footer) + "\n")
	}
	return b.String()
}

// Line renders a status line with a leading symbol.
func Line(tone Tone, msg string) string {
	symbol := map[Tone]string{Good: "✓", Warn: "!", Bad: "✗", Dim: "·", Plain: "›"}[tone]
	return tone.Style().Bold(true).Render(symbol) + " " + msg
}

// Link renders clickable text in terminals that support hyperlinks.
func Link(text, url string) string {
	return lipgloss.NewStyle().Foreground(accent).Underline(true).Hyperlink(url).Render(text)
}

// Box frames text, for example a prompt to copy.
func Box(text string, width int) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(gray).
		Padding(1, 2).
		Width(width).
		Render(text)
}

// Print writes styled text, dropping colors the writer cannot show.
func Print(w io.Writer, s string) {
	lipgloss.Fprint(w, s)
}

// ErrCancelled is returned when the user presses Ctrl-C in a spinner or prompt.
var ErrCancelled = errors.New("cancelled")

type doneMsg struct{ err error }

type waitModel struct {
	spinner spinner.Model
	label   string
	fn      func() error
	cancel  context.CancelFunc
	err     error
	done    bool
}

func (m waitModel) Init() tea.Cmd {
	run := func() tea.Msg { return doneMsg{m.fn()} }
	return tea.Batch(m.spinner.Tick, run)
}

func (m waitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case doneMsg:
		m.err, m.done = msg.err, true
		return m, tea.Quit
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.cancel()
			m.label = "Cancelling…"
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m waitModel) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	return tea.NewView(m.spinner.View() + " " + m.label + Muted.Render("  ctrl+c to cancel") + "\n")
}

// Wait shows a spinner with label on out while fn runs. Ctrl-C cancels the
// context passed to fn.
func Wait(ctx context.Context, in io.Reader, out io.Writer, label string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := waitModel{
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(Accent)),
		label:   label,
		fn:      func() error { return fn(ctx) },
		cancel:  cancel,
	}
	final, err := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		cancel()
		return fmt.Errorf("terminal: %w", err)
	}
	return final.(waitModel).err
}

type promptModel struct {
	input     textinput.Model
	label     string
	secret    bool
	submitted bool
	cancelled bool
}

func (m promptModel) Init() tea.Cmd { return m.input.Focus() }

func (m promptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "enter":
			m.submitted = true
			return m, tea.Quit
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m promptModel) View() tea.View {
	if m.submitted || m.cancelled {
		value := m.input.Value()
		if m.secret {
			value = strings.Repeat("•", min(len(value), 12))
		}
		if m.cancelled {
			value = Muted.Render("cancelled")
		}
		return tea.NewView(Bold.Render(m.label) + " " + value + "\n")
	}
	return tea.NewView(Bold.Render(m.label) + "\n" + m.input.View() + "\n")
}

// Prompt reads one line. With secret set, input is masked and never echoed.
func Prompt(in io.Reader, out io.Writer, label, placeholder string, secret bool) (string, error) {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.Placeholder = placeholder
	if secret {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '•'
	}
	final, err := tea.NewProgram(promptModel{input: ti, label: label, secret: secret}, tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return "", fmt.Errorf("terminal: %w", err)
	}
	m := final.(promptModel)
	if m.cancelled {
		return "", ErrCancelled
	}
	return m.input.Value(), nil
}
