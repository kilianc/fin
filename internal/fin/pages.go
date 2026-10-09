package fin

import (
	"bytes"
	"html/template"
	"net/url"
	"strings"
)

// PagesURL is where GitHub Pages serves docs/. The README's setup buttons
// link there, because GitHub strips claude-cli:// and codex:// links.
const PagesURL = "https://kilianc.github.io/fin/"

// Agent is a local AI agent app that can take the setup prompt from a link.
type Agent struct {
	Slug string // docs/<slug>/ and the JSON key
	Name string
	Link string        // deep link that opens the app with the prompt filled in
	Help template.HTML // what to do if the link doesn't open
}

// shortPrompt bootstraps the full agent prompt from fin itself. Claude Code
// types a deep link's prompt into the terminal as a command line, and a long
// one stalls there, so its link carries this instead.
const shortPrompt = "Help me set up fin (https://github.com/kilianc/fin), a local, read-only command-line tool for my own bank and brokerage data. If `fin` isn't installed, install it with `go install github.com/kilianc/fin/cmd/fin@latest`. Then run `fin init --json` and follow the setup steps in its \"prompt\" field, using my web browser for the clicking."

func queryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// Agents lists the apps fin can hand its setup prompt to. Neither sends the
// prompt on its own: the person reads it and presses Enter.
func Agents() []Agent {
	return []Agent{{
		Slug: "claude",
		Name: "Claude Code",
		Link: "claude-cli://open?q=" + queryEscape(shortPrompt),
		Help: `Nothing opened? Run <code>claude</code> in a terminal and send it any message once; that registers the link. For the browser steps, connect <a href="https://claude.com/chrome">Claude in Chrome</a> with <code>/chrome</code>.`,
	}, {
		Slug: "codex",
		Name: "Codex",
		Link: "codex://new?prompt=" + queryEscape(agentPrompt),
		Help: `Nothing opened? Codex runs in the <a href="https://chatgpt.com/download">ChatGPT desktop app</a>; install it and try again.`,
	}}
}

var setupPage = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Set up fin with {{.Name}}</title>
<link rel="icon" href="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png">
<style>
:root { color-scheme: light; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #F4F5F7; color: #0E1B2E; font: 16px/1.55 -apple-system, BlinkMacSystemFont, "Inter", "Segoe UI", Helvetica, Arial, sans-serif; }
main { box-sizing: border-box; width: 100%; max-width: 640px; margin: 48px 16px; padding: 40px 36px; background: #FFFFFF; border: 1px solid #E4E7EB; border-radius: 12px; text-align: center; }
img { width: 72px; height: 72px; }
h1 { font-size: 26px; letter-spacing: -.02em; margin: 14px 0 8px; }
p { color: #3B4757; margin: 8px 0; }
p.small { color: #6A7686; font-size: 14px; }
a { color: #356B8D; }
code { font: 13.5px ui-monospace, SFMono-Regular, Menlo, monospace; color: #0E1B2E; background: #F4F5F7; border: 1px solid #E4E7EB; border-radius: 5px; padding: 1px 6px; }
.open { display: inline-block; margin: 22px 0 14px; padding: 11px 26px; border-radius: 8px; background: #0E1B2E; color: #FFFFFF; font-weight: 600; text-decoration: none; }
.open:hover { background: #356B8D; }
details { margin-top: 24px; padding-top: 20px; border-top: 1px solid #E4E7EB; text-align: left; }
summary { cursor: pointer; color: #3B4757; text-align: center; }
pre { white-space: pre-wrap; background: #F4F5F7; border: 1px solid #E4E7EB; border-radius: 8px; padding: 16px; font: 12.5px/1.55 ui-monospace, SFMono-Regular, Menlo, monospace; color: #0E1B2E; }
button { font: 600 13px -apple-system, BlinkMacSystemFont, sans-serif; padding: 7px 14px; border-radius: 7px; border: 1px solid #E4E7EB; background: #F4F5F7; color: #3B4757; cursor: pointer; }
</style>
</head>
<body>
<main>
<img src="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png" alt="fin">
<h1>Opening {{.Name}}…</h1>
<p>Your browser asks to open {{.Name}} with fin's setup prompt filled in. Read the prompt, then press Enter.</p>
<a class="open" href="{{.Link}}">Open {{.Name}}</a>
<p class="small">{{.Help}}</p>
<details>
<summary>Or copy the prompt into another agent</summary>
<pre id="prompt">{{.Prompt}}</pre>
<button id="copy" type="button">Copy</button>
</details>
<p class="small"><a href="https://kilianc.github.io/fin/">kilianc.github.io/fin</a></p>
</main>
<script>
location.href = {{.Link}};
document.getElementById("copy").onclick = function () {
  navigator.clipboard.writeText(document.getElementById("prompt").textContent);
  this.textContent = "Copied";
};
</script>
</body>
</html>
`))

// SetupPage renders docs/<slug>/index.html, the page behind a README
// button: it opens the agent app through its deep link and shows the prompt
// for anyone without the app.
func SetupPage(a Agent) string {
	var b bytes.Buffer
	err := setupPage.Execute(&b, map[string]any{
		"Name":   a.Name,
		"Link":   template.URL(a.Link),
		"Help":   a.Help,
		"Prompt": agentPrompt,
	})
	if err != nil {
		panic(err)
	}
	return b.String()
}
