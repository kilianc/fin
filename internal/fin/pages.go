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
const shortPrompt = "Help me set up fin (https://github.com/kilianc/fin), a local, read-only command-line tool for my own bank and brokerage data. If `fin` isn't installed, install it with `mkdir -p ~/.local/bin && curl -fsSL https://github.com/kilianc/fin/releases/latest/download/fin-darwin-universal.tar.gz | tar -xz -C ~/.local/bin` and make sure ~/.local/bin is on my PATH. Then run `fin init --json` and follow the setup steps in its \"prompt\" field, using my web browser for the clicking."

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

var setupPage = pageTemplate("setup", `<!doctype html>
<html lang="en">
<head>
{{template "head" .}}
<title>fin · set up with {{.Name}}</title>
</head>
<body>
{{template "header" .}}
<main>
<section class="section first wrap">
<p class="kicker">// setup · {{.Name}}</p>
<h1 class="display">Opening {{.Name}}…</h1>
<p class="lede">Your browser asks to open {{.Name}} with <span class="fin">fin</span>'s setup prompt filled in. Read the prompt, then press Enter; your agent does the clicking and leaves you only the parts that need you.</p>
<div class="buttons"><a href="{{.Link}}"><img src="{{.Root}}setup-with-{{.Slug}}.svg?v=flat2" alt="Set up with {{.Name}}"></a></div>
<p class="note">{{.Help}}</p>
</section>
<section class="section tight wrap">
<div class="shell">
<div class="shell-bar"><span class="dots" aria-hidden="true"><i></i><i></i><i></i></span><span class="label">the setup prompt · paste it into any agent that can use your browser</span><button class="copy" id="copy" type="button">copy</button></div>
<pre class="prose" id="prompt">{{.Prompt}}</pre>
</div>
</section>
</main>
{{template "footer" .}}
<script>
location.href = {{.Link}};
document.getElementById("copy").onclick = function () {
  navigator.clipboard.writeText(document.getElementById("prompt").textContent);
  this.textContent = "copied";
};
</script>
</body>
</html>
`)

// SetupPage renders docs/<slug>/index.html, the page behind a README
// button: it opens the agent app through its deep link and shows the prompt
// for anyone without the app.
func SetupPage(a Agent) string {
	var b bytes.Buffer
	err := setupPage.Execute(&b, map[string]any{
		"Root":   "../",
		"Slug":   a.Slug,
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
