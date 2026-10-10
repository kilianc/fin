package ui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Reporter is how a Flow's work tells the screen what is happening. Its
// methods are safe to call from any goroutine.
type Reporter interface {
	// Start marks step i as running, with an optional detail.
	Start(i int, detail string)
	// Done marks step i as finished, with an optional detail.
	Done(i int, detail string)
	// Link shows a URL the user should open; o opens it.
	Link(label, url string)
}

// FlowResult is what a Flow shows once its work succeeds.
type FlowResult struct {
	Message string
	URL     string // opened with o or enter
	OpenAs  string // what o opens, such as "the spreadsheet"
}

// FlowOptions describe a full-screen Flow.
type FlowOptions struct {
	Title    string
	Subtitle string
	Steps    []string
	Open     func(url string) error
}

type stepState int

const (
	stepPending stepState = iota
	stepRunning
	stepDone
	stepFailed
)

type flowStep struct {
	label, detail string
	state         stepState
}

type (
	stepMsg struct {
		i      int
		state  stepState
		detail string
	}
	linkMsg     struct{ label, url string }
	flowDoneMsg struct {
		res FlowResult
		err error
	}
)

type flowModel struct {
	opts      FlowOptions
	steps     []flowStep
	spinner   spinner.Model
	linkLabel string
	linkURL   string
	result    *FlowResult
	err       error
	finished  bool
	cancel    context.CancelFunc
	notice    string
	w, h      int
}

func (m flowModel) Init() tea.Cmd { return m.spinner.Tick }

func (m flowModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case stepMsg:
		m.steps[msg.i].state = msg.state
		if msg.detail != "" || msg.state == stepRunning {
			m.steps[msg.i].detail = msg.detail
		}
		if msg.state == stepDone {
			// A link belongs to the step that asked for it.
			m.linkLabel, m.linkURL = "", ""
		}
	case linkMsg:
		m.linkLabel, m.linkURL = msg.label, msg.url
	case flowDoneMsg:
		m.finished = true
		m.linkLabel, m.linkURL = "", ""
		if msg.err != nil {
			m.err = msg.err
			for i := range m.steps {
				if m.steps[i].state == stepRunning {
					m.steps[i].state = stepFailed
				}
			}
		} else {
			m.result = &msg.res
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			if !m.finished {
				m.cancel()
				m.notice = "Cancelling…"
				return m, nil
			}
			return m, tea.Quit
		case "o", "enter":
			url := m.linkURL
			if m.finished && m.result != nil {
				url = m.result.URL
			}
			if url != "" && m.opts.Open != nil {
				if err := m.opts.Open(url); err != nil {
					m.notice = "Could not open the browser: " + err.Error()
				}
			}
			if m.finished && msg.String() == "enter" {
				return m, tea.Quit
			}
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m flowModel) View() tea.View {
	width := min(max(m.w-4, 40), 72)
	var b strings.Builder
	b.WriteString(Brand.Render(m.opts.Title) + "\n")
	if m.opts.Subtitle != "" {
		b.WriteString(lipgloss.NewStyle().Width(width).Render(Muted.Render(m.opts.Subtitle)) + "\n")
	}
	b.WriteString("\n")
	for _, s := range m.steps {
		var mark string
		label := s.label
		switch s.state {
		case stepPending:
			mark, label = Muted.Render("○"), Muted.Render(label)
		case stepRunning:
			mark, label = m.spinner.View(), Bold.Render(label)
		case stepDone:
			mark = Good.Style().Bold(true).Render("✓")
		case stepFailed:
			mark = Bad.Style().Bold(true).Render("✗")
		}
		line := mark + " " + label
		if s.detail != "" {
			line += Muted.Render("  " + s.detail)
		}
		b.WriteString("  " + line + "\n")
	}
	if m.linkURL != "" {
		b.WriteString("\n" + Box(Bold.Render(m.linkLabel)+"\n"+Link(m.linkURL, m.linkURL)+"\n\n"+
			Muted.Render("The browser opened it. Press o to open it again."), width) + "\n")
	}
	if m.err != nil {
		b.WriteString("\n" + Line(Bad, wrap(m.err.Error(), width-2)) + "\n")
	}
	if m.result != nil {
		b.WriteString("\n" + Line(Good, wrap(m.result.Message, width-2)) + "\n")
		if m.result.URL != "" {
			b.WriteString("  " + Link(m.result.URL, m.result.URL) + "\n")
		}
	}
	if m.notice != "" {
		b.WriteString("\n" + Muted.Render(m.notice) + "\n")
	}
	b.WriteString("\n" + Muted.Render(m.keys()))

	return fullScreen(m.w, m.h, width, b.String())
}

// fullScreen centers body on the screen, with the mascot above it when there
// is room to draw it legibly. Shrunk much below 32 columns the artwork turns
// to noise, so smaller screens get the text alone.
func fullScreen(w, h, width int, body string) tea.View {
	body = lipgloss.NewStyle().Width(width).Render(body)
	content := body
	if rows := h - lipgloss.Height(body) - 4; w > 0 && h > 0 {
		if size := min(rows*2, 48, w-4) &^ 1; size >= 32 {
			mascot := lipgloss.PlaceHorizontal(width, lipgloss.Center, renderFinColor(size))
			content = mascot + "\n\n" + body
		}
		content = lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m flowModel) keys() string {
	switch {
	case m.finished && m.result != nil && m.result.URL != "":
		return fmt.Sprintf("enter open %s and quit · o open · q quit", m.result.OpenAs)
	case m.finished:
		return "q quit"
	case m.linkURL != "":
		return "o open in browser · q cancel"
	}
	return "q cancel"
}

func wrap(s string, width int) string {
	return lipgloss.NewStyle().Width(max(width, 20)).Render(s)
}

type flowReporter struct{ p *tea.Program }

func (r flowReporter) Start(i int, detail string) { r.p.Send(stepMsg{i, stepRunning, detail}) }
func (r flowReporter) Done(i int, detail string)  { r.p.Send(stepMsg{i, stepDone, detail}) }
func (r flowReporter) Link(label, url string)     { r.p.Send(linkMsg{label, url}) }

// Flow runs fn on a full-screen view that shows each step's progress, any
// URL the user needs to open, and the result, then waits for a key so the
// user can read it. q cancels the context passed to fn while it runs.
func Flow(ctx context.Context, in io.Reader, out io.Writer, opts FlowOptions, fn func(ctx context.Context, r Reporter) (FlowResult, error)) (FlowResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := flowModel{
		opts:    opts,
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(Accent)),
		cancel:  cancel,
	}
	for _, s := range opts.Steps {
		m.steps = append(m.steps, flowStep{label: s})
	}
	p := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out))
	go func() {
		res, err := fn(ctx, flowReporter{p})
		p.Send(flowDoneMsg{res, err})
	}()
	final, err := p.Run()
	if err != nil {
		cancel()
		return FlowResult{}, fmt.Errorf("terminal: %w", err)
	}
	fm := final.(flowModel)
	if !fm.finished {
		return FlowResult{}, ErrCancelled
	}
	if fm.err != nil {
		return FlowResult{}, fm.err
	}
	return *fm.result, nil
}

// PlainReporter prints each step as a line, for terminals that should not
// switch to a full screen and for logs.
type PlainReporter struct {
	W     io.Writer
	Steps []string
}

func (r PlainReporter) Start(i int, detail string) {}

func (r PlainReporter) Done(i int, detail string) {
	msg := r.Steps[i]
	if detail != "" {
		msg += Muted.Render("  " + detail)
	}
	Print(r.W, Line(Good, msg)+"\n")
}

func (r PlainReporter) Link(label, url string) {
	Print(r.W, label+"\n"+url+"\n")
}
