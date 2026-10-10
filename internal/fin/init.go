package fin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/kilianc/fin/internal/keychain"
	"github.com/kilianc/fin/internal/plaid"
	"github.com/kilianc/fin/internal/ui"
)

// agentPrompt is what a person pastes into an AI agent that can drive their
// browser, so the agent does the clicking and the person only does what needs
// them: passwords, codes, terms and secrets.
const agentPrompt = `Help me set up fin, a local command-line tool that reads my own bank and brokerage data through Plaid. fin is already installed; run ` + "`fin help`" + ` to see its commands. Use my web browser to do the navigating, clicking and form-filling for me, and leave me only the parts that need me personally.

Ground rules:
- Never type my passwords, verification codes or API secrets, and never accept terms, agreements or attestations for me. When one is needed, stop, tell me exactly what to click or paste and where, and wait.
- Never put my Plaid secret in this chat, in a file, or anywhere except the hidden field of ` + "`fin setup`" + ` in my terminal.
- Each institution I connect permanently uses one of my 10 free Plaid slots. Ask me before every ` + "`fin link`" + `.

Steps:
1. Open https://dashboard.plaid.com/signup. Fill in the non-secret fields (ask me for anything you don't know). When asked how I'll use Plaid, choose personal use (building something for myself). Let me set the password, accept the terms and verify my email.
2. Open https://dashboard.plaid.com/trial-plan and fill in the free Trial plan application. Describe the use as: "Personal, read-only access to my own bank and brokerage accounts with a local command-line tool." Let me review and submit it. It is usually approved right away and allows 10 connected institutions. Do NOT apply for full Production access: once that is submitted, the Trial plan is no longer available.
3. Open https://dashboard.plaid.com/developers/keys. Run ` + "`fin env production`" + `. Then have me run ` + "`fin setup`" + ` in my terminal (it needs my keyboard): it opens a full-screen form where I paste the Client ID and the Production secret. fin checks them with Plaid and saves them only in my macOS Keychain.
4. Brand Plaid Link with fin's look, so the consent screen I see when connecting shows fin's mascot and colors. Download https://raw.githubusercontent.com/kilianc/fin/main/mascot-plaid.png, the 1024 x 1024 square logo with an opaque background. Open https://dashboard.plaid.com/link and edit the customization named "default" (fin uses the default). In its settings (gear icon, upper right), set language English and countries United States only, or Plaid will ignore it. On the Consent pane choose co-branded, upload mascot-plaid.png as the logo (ask me to pick the file if you can't upload it yourself), set the brand color to #81B1CF and the background color to #203A52, then click Publish.
5. Ask me which banks, credit cards and brokerages I want to connect. For each, run ` + "`fin institutions <name>`" + ` to confirm Plaid supports it, and tell me which slot count that leaves.
6. For each one I approve, run ` + "`fin link --yes --open`" + ` in the background. It opens Plaid Link in my browser, where I pick the institution and sign in; fin collects transactions and investments, whichever the institution supports. Only if ` + "`fin institutions`" + ` shows an institution as brokerage-only, use ` + "`fin link brokerage --yes --open`" + ` instead.
7. Run ` + "`fin sync`" + ` to save my transactions on this Mac, then finish with ` + "`fin items`" + ` and ` + "`fin accounts`" + ` and summarize what is connected and healthy.
8. Tell me I can now ask you about my money in plain words, or query it myself with ` + "`fin sql`" + `. Ask whether I'd also like my transactions in a Google Sheet; if so, run ` + "`fin sheet --open`" + ` in the background and let me sign in to Google in the browser.
9. Ask whether I shop on Amazon and want each order broken into its items (experimental). If so, ask me for a name for the account and run ` + "`fin amazon login <name>`" + `; macOS will ask me to allow Chrome Safe Storage.
10. Ask whether I shop at Costco and want my receipts and costco.com orders broken into their items (experimental). If so, ask me for a name for the account and run ` + "`fin costco login <name>`" + `; macOS may ask me to allow Chrome Safe Storage.`

type initStatus struct {
	Env         string          `json:"env"`
	Credentials map[string]bool `json:"credentials"`
	Items       int             `json:"items"`
	Next        string          `json:"next"`
}

func (a *App) cmdInit(ctx context.Context, args []string) (*result, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	copyPrompt := fs.Bool("copy", false, "copy the agent prompt to the clipboard")
	if pos, err := parseArgs(fs, args); err != nil {
		return nil, err
	} else if len(pos) > 0 {
		return nil, usageErr("usage: fin init [--copy]")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, err
	}
	status := initStatus{
		Env:         string(a.Env),
		Credentials: map[string]bool{},
		Items:       len(st.ForEnv(string(plaid.Production))),
	}
	for _, env := range []plaid.Env{plaid.Sandbox, plaid.Production} {
		has, err := a.hasCredentials(env)
		if err != nil {
			return nil, err
		}
		status.Credentials[string(env)] = has
	}
	switch {
	case !status.Credentials["production"]:
		status.Next = "Sign up for Plaid and its free Trial plan, then run fin env production and fin setup."
	case a.Env != plaid.Production:
		status.Next = "Run fin env production."
	case status.Items == 0:
		status.Next = "Check an institution with fin institutions <name>, then run fin link --open."
	default:
		status.Next = "You're set up. Try fin accounts or fin transactions --since 2026-01-01."
	}
	copied := false
	if *copyPrompt {
		if a.Copy == nil {
			return nil, newErr("NO_CLIPBOARD", "no clipboard available")
		}
		if err := a.Copy(agentPrompt); err != nil {
			return nil, newErr("NO_CLIPBOARD", "copy to clipboard: %v", err)
		}
		copied = true
	}
	openIn := map[string]string{}
	for _, agent := range Agents() {
		openIn[agent.Slug] = agent.Link
	}
	body := map[string]any{
		"tagline": Tagline, "status": status, "prompt": agentPrompt, "copied": copied,
		"open_in": openIn, "skill": SkillInstall,
	}
	msg := a.renderInit(status, copied)
	if latest := a.availableUpdate(ctx); latest != "" {
		body["update"] = map[string]string{"current": a.Version, "latest": latest, "command": "fin update"}
		msg += "\n\n" + ui.Line(ui.Warn, updateNotice(latest))
	}
	return &result{body: body, message: msg}, nil
}

// SkillInstall teaches an agent to use fin: Claude Code installs the plugin
// from this repository's marketplace, and Codex reads the same skill from
// ~/.agents/skills.
var SkillInstall = map[string][]string{
	"claude": {"/plugin marketplace add kilianc/fin", "/plugin install fin@fin"},
	"codex":  {"curl -fsSL --create-dirs -o ~/.agents/skills/fin/SKILL.md https://raw.githubusercontent.com/kilianc/fin/main/plugins/fin/skills/fin/SKILL.md"},
}

func (a *App) hasCredentials(env plaid.Env) (bool, error) {
	for _, account := range []string{accountClientID, secretAccount(env)} {
		if _, err := a.Secrets.Get(account); errors.Is(err, keychain.ErrNotFound) {
			return false, nil
		} else if err != nil {
			return false, newErr("KEYCHAIN_ERROR", "%v", err)
		}
	}
	return true, nil
}

func (a *App) renderInit(s initStatus, copied bool) string {
	width := min(a.width(), 96)
	var b strings.Builder
	b.WriteString(ui.Mascot(min(width, 44), a.Stdout) + "\n\n")
	b.WriteString(ui.Brand.Render("fin") + "  " + ui.Muted.Render("bank and brokerage data, on your machine") + "\n\n")
	b.WriteString(wrapText(Tagline, width) + "\n\n")

	check := func(done bool, label string) {
		if done {
			b.WriteString(ui.Line(ui.Good, label) + "\n")
		} else {
			b.WriteString(ui.Line(ui.Dim, ui.Muted.Render(label)) + "\n")
		}
	}
	check(s.Credentials["production"], "Plaid account with the free Trial plan, keys saved with fin setup")
	check(s.Env == "production", "Environment set to production (fin env production)")
	check(s.Items > 0, fmt.Sprintf("Institutions connected (%d of %d slots used)", s.Items, SlotsTotal))
	b.WriteString("\n" + ui.Bold.Render("Next: ") + s.Next + "\n\n")

	b.WriteString(ui.Bold.Render("Let your AI agent do the clicking.") + "\n")
	var links []string
	for _, agent := range Agents() {
		links = append(links, ui.Link(agent.Name, agent.Link))
	}
	b.WriteString("Open it prefilled: " + strings.Join(links, ui.Muted.Render(" · ")) + "\n")
	b.WriteString("Or paste this into any agent that can use your browser:\n")
	b.WriteString(ui.Box(agentPrompt, width) + "\n")
	if copied {
		b.WriteString(ui.Line(ui.Good, "Copied to your clipboard.") + "\n")
	} else {
		b.WriteString(ui.Muted.Render("Run fin init --copy to put it on your clipboard.") + "\n")
	}

	b.WriteString("\n" + ui.Bold.Render("Teach your agent fin.") + "\n")
	b.WriteString("In Claude Code: " + strings.Join(SkillInstall["claude"], ui.Muted.Render(" then ")) + "\n")
	b.WriteString("In Codex: " + SkillInstall["codex"][0] + "\n")
	return b.String()
}

// wrapText wraps plain text to width without styling it.
func wrapText(s string, width int) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		if line != "" && len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	return strings.Join(append(lines, line), "\n")
}
