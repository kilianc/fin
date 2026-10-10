package ui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// SetupOptions drive the full-screen fin setup.
type SetupOptions struct {
	Env      string   // the environment the keys are for
	Envs     []string // environments ctrl+e cycles through
	ClientID string   // the stored client ID, prefilled
	KeysURL  string   // where the keys are, opened with ctrl+o
	// Verify checks the keys with Plaid; Save stores them. Both run off the
	// UI goroutine.
	Verify func(ctx context.Context, env, clientID, secret string) error
	Save   func(env, clientID, secret string) error
	// SwitchEnv saves a new environment chosen with ctrl+e.
	SwitchEnv func(env string) error
	// Import, when set, reads keys from Plaid's own CLI (ctrl+p).
	Import func(env string) (clientID, secret string, err error)
	Open   func(url string) error
	Next   string // what to run once the keys are saved
}

// SetupResult is what the user saved.
type SetupResult struct {
	Env      string
	Imported bool
}

type setupStage int

const (
	stageClientID setupStage = iota
	stageSecret
	stageVerifying
	stageDone
)

type verifiedMsg struct {
	err      error
	imported bool
}

type setupModel struct {
	opts     SetupOptions
	ctx      context.Context
	stage    setupStage
	clientID textinput.Model
	secret   textinput.Model
	spinner  spinner.Model
	env      string
	err      error
	notice   string
	imported bool
	quit     bool
	w, h     int
}

func newSetupModel(ctx context.Context, opts SetupOptions) setupModel {
	id := textinput.New()
	id.Prompt = ""
	id.Placeholder = "paste the client_id"
	id.SetValue(opts.ClientID)
	id.CursorEnd()
	// Focus here: Init gets a copy of the model, so focusing there is lost.
	id.Focus()
	secret := textinput.New()
	secret.Prompt = ""
	secret.Placeholder = "paste the secret; it stays hidden"
	secret.EchoMode = textinput.EchoPassword
	secret.EchoCharacter = '•'
	return setupModel{
		opts: opts, ctx: ctx, env: opts.Env, clientID: id, secret: secret,
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(Accent)),
	}
}

func (m setupModel) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.spinner.Tick) }

func (m setupModel) verify(clientID, secret string, imported bool) tea.Cmd {
	env, opts, ctx := m.env, m.opts, m.ctx
	return func() tea.Msg {
		err := opts.Verify(ctx, env, clientID, secret)
		if err == nil {
			err = opts.Save(env, clientID, secret)
		}
		return verifiedMsg{err: err, imported: imported}
	}
}

func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case verifiedMsg:
		if msg.err != nil {
			m.err, m.stage = msg.err, stageSecret
			m.secret.SetValue("")
			return m, m.secret.Focus()
		}
		m.stage, m.imported = stageDone, msg.imported
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	var cmd tea.Cmd
	switch m.stage {
	case stageClientID:
		m.clientID, cmd = m.clientID.Update(msg)
	case stageSecret:
		m.secret, cmd = m.secret.Update(msg)
	}
	return m, cmd
}

func (m setupModel) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.notice = ""
	switch k.String() {
	case "ctrl+c", "esc":
		m.quit = true
		return m, tea.Quit
	}
	if m.stage == stageDone {
		if k.String() == "enter" || k.String() == "q" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.stage == stageVerifying {
		return m, nil
	}
	switch k.String() {
	case "ctrl+o":
		if m.opts.Open != nil && m.opts.KeysURL != "" {
			if err := m.opts.Open(m.opts.KeysURL); err != nil {
				m.notice = "Could not open the browser: " + err.Error()
			}
		}
		return m, nil
	case "ctrl+e":
		if len(m.opts.Envs) > 1 {
			next := m.opts.Envs[0]
			for i, e := range m.opts.Envs {
				if e == m.env {
					next = m.opts.Envs[(i+1)%len(m.opts.Envs)]
				}
			}
			if m.opts.SwitchEnv != nil {
				if err := m.opts.SwitchEnv(next); err != nil {
					m.notice = err.Error()
					return m, nil
				}
			}
			m.env, m.err = next, nil
			m.secret.SetValue("")
		}
		return m, nil
	case "ctrl+p":
		if m.opts.Import == nil {
			return m, nil
		}
		id, secret, err := m.opts.Import(m.env)
		if err != nil {
			m.err = err
			return m, nil
		}
		m.clientID.SetValue(id)
		m.err, m.stage = nil, stageVerifying
		return m, m.verify(id, secret, true)
	case "enter":
		switch m.stage {
		case stageClientID:
			if strings.TrimSpace(m.clientID.Value()) == "" {
				m.notice = "The client ID is required."
				return m, nil
			}
			m.clientID.Blur()
			m.stage = stageSecret
			return m, m.secret.Focus()
		case stageSecret:
			if strings.TrimSpace(m.secret.Value()) == "" {
				m.notice = "The secret is required."
				return m, nil
			}
			m.secret.Blur()
			m.err, m.stage = nil, stageVerifying
			return m, m.verify(strings.TrimSpace(m.clientID.Value()), strings.TrimSpace(m.secret.Value()), false)
		}
	case "shift+tab", "up":
		if m.stage == stageSecret {
			m.secret.Blur()
			m.stage = stageClientID
			return m, m.clientID.Focus()
		}
	case "tab", "down":
		if m.stage == stageClientID && strings.TrimSpace(m.clientID.Value()) != "" {
			m.clientID.Blur()
			m.stage = stageSecret
			return m, m.secret.Focus()
		}
	}
	var cmd tea.Cmd
	switch m.stage {
	case stageClientID:
		m.clientID, cmd = m.clientID.Update(k)
	case stageSecret:
		m.secret, cmd = m.secret.Update(k)
	}
	return m, cmd
}

func (m setupModel) View() tea.View {
	width := min(max(m.w-4, 44), 72)
	var b strings.Builder
	envTag := lipgloss.NewStyle().Foreground(lipgloss.Color("#00184B")).Background(accent).Padding(0, 1).Render(m.env)
	b.WriteString(Brand.Render("Set up fin") + "  " + envTag + "\n")
	b.WriteString(wrap(Muted.Render("Save your Plaid keys in the macOS Keychain. fin checks them with Plaid first, and they never leave this Mac."), width) + "\n\n")

	field := func(n int, label string, stage setupStage, input textinput.Model, value string) {
		active := m.stage == stage
		num := Muted.Render(fmt.Sprintf("%d", n))
		name := Muted.Render(label)
		if active {
			num, name = Accent.Bold(true).Render(fmt.Sprintf("%d", n)), Bold.Render(label)
		}
		b.WriteString("  " + num + "  " + name + "\n")
		box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(gray).Padding(0, 1).Width(width - 4)
		if active {
			box = box.BorderForeground(accent)
			b.WriteString(box.Render(input.View()) + "\n")
		} else {
			b.WriteString(box.Render(value) + "\n")
		}
	}
	field(1, "Client ID", stageClientID, m.clientID, m.clientID.Value())
	masked := ""
	if n := len(m.secret.Value()); n > 0 {
		masked = strings.Repeat("•", min(n, 24))
	}
	field(2, fmt.Sprintf("%s secret", strings.ToUpper(m.env[:1])+m.env[1:]), stageSecret, m.secret, masked)

	b.WriteString("\n")
	switch {
	case m.stage == stageVerifying:
		b.WriteString("  " + m.spinner.View() + " Checking the keys with Plaid…\n")
	case m.stage == stageDone:
		how := "Plaid accepted your " + m.env + " keys"
		if m.imported {
			how = "Imported your " + m.env + " keys from Plaid's CLI; Plaid accepted them"
		}
		b.WriteString(Line(Good, wrap(how+". They are in your macOS Keychain, nowhere else.", width-2)) + "\n")
		if m.opts.Next != "" {
			b.WriteString("\n  Next: " + Accent.Bold(true).Render(m.opts.Next) + "\n")
		}
	case m.err != nil:
		b.WriteString(Line(Bad, wrap(m.err.Error(), width-2)) + "\n")
	default:
		b.WriteString("  " + Muted.Render("Find both at ") + Link(m.opts.KeysURL, m.opts.KeysURL) + "\n")
	}
	if m.opts.Import != nil && m.stage != stageDone && m.stage != stageVerifying {
		b.WriteString("  " + Muted.Render("Signed in to Plaid's CLI? ctrl+p imports its keys instead.") + "\n")
	}
	if m.notice != "" {
		b.WriteString("\n  " + Line(Warn, m.notice) + "\n")
	}
	b.WriteString("\n" + Muted.Render(m.keys()))

	return fullScreen(m.w, m.h, width, b.String())
}

func (m setupModel) keys() string {
	switch m.stage {
	case stageDone:
		return "enter done"
	case stageVerifying:
		return "esc quit"
	}
	keys := []string{"enter next", "ctrl+o keys page"}
	if len(m.opts.Envs) > 1 {
		keys = append(keys, "ctrl+e environment")
	}
	return strings.Join(append(keys, "esc quit"), " · ")
}

// Setup runs the full-screen key setup. It returns ErrCancelled if the user
// leaves before Plaid accepts a pair of keys.
func Setup(ctx context.Context, in io.Reader, out io.Writer, opts SetupOptions) (SetupResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	final, err := tea.NewProgram(newSetupModel(ctx, opts), tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return SetupResult{}, fmt.Errorf("terminal: %w", err)
	}
	m := final.(setupModel)
	if m.stage != stageDone {
		return SetupResult{}, ErrCancelled
	}
	return SetupResult{Env: m.env, Imported: m.imported}, nil
}
