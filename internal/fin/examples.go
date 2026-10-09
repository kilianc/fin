package fin

import (
	"bytes"
	"html/template"
)

// Example is a prompt from the gallery in the README and on the landing page.
// Its try page, docs/try/<slug>/, opens the prompt in Claude Code or Codex.
type Example struct {
	Slug   string
	Title  string
	Prompt string
	Image  string // the gallery card, relative to docs/
}

// Examples are the gallery's prompts, in order. docs/examples/index.html
// shows each prompt verbatim on its card.
var Examples = []Example{
	{"spending", "Spending report", "Where did my money go last month? Compare it to the month before.", "examples/1.png"},
	{"subscriptions", "Subscription audit", "Find every subscription I'm paying for. Flag price increases, trials that turned paid, and anything I'm paying for twice.", "examples/2.png"},
	{"portfolio", "Portfolio review", "How is my money invested across my brokerages? What's my unrealized gain?", "examples/3.png"},
	{"net-worth", "Net worth", "Chart my net worth this year and tell me what moved it.", "examples/4.png"},
}

// TryURL is the example's try page on GitHub Pages.
func (e Example) TryURL() string { return PagesURL + "try/" + e.Slug + "/" }

// agentPrompt tells the agent to answer with fin; the person's question
// follows verbatim.
func (e Example) agentPrompt() string {
	return "Use fin, my local command-line tool for my bank and brokerage data (run `fin help` to see its commands), to answer this with a short report: " + e.Prompt
}

var tryPage = template.Must(template.New("try").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Try with fin: {{.Title}}</title>
<meta property="og:image" content="` + PagesURL + `{{.Image}}">
<link rel="icon" href="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Geist:wght@400;500;600;700&family=Geist+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
:root { color-scheme: light; }
* { box-sizing: border-box; }
body { margin: 0; background: #F4F5F7; color: #0E1B2E; font: 16px/1.55 "Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; }
main { max-width: 860px; margin: 0 auto; padding: 40px 16px 56px; }
.top { display: flex; align-items: center; gap: 10px; margin-bottom: 28px; }
.top img { width: 28px; height: 28px; }
.top b { font-size: 17px; }
.top .sp { flex: 1; }
a { color: #356B8D; text-decoration: none; }
a:hover { text-decoration: underline; }
.card { background: #FFFFFF; border: 1px solid #E4E7EB; border-radius: 12px; padding: 32px; }
.label { font-size: 12px; font-weight: 600; letter-spacing: .1em; text-transform: uppercase; color: #356B8D; }
.prompt { margin: 10px 0 24px; padding: 18px 22px; background: #EEF4F8; border-left: 4px solid #356B8D; border-radius: 0 10px 10px 0; font-size: 24px; line-height: 1.4; font-weight: 600; letter-spacing: -.01em; }
.actions { display: flex; flex-wrap: wrap; gap: 10px; }
.btn { display: inline-block; padding: 11px 20px; border-radius: 8px; font-weight: 600; font-size: 15px; border: 1px solid #0E1B2E; cursor: pointer; font-family: inherit; }
.btn.primary { background: #0E1B2E; color: #FFFFFF; }
.btn.primary:hover { background: #356B8D; border-color: #356B8D; text-decoration: none; }
.btn.secondary { background: #FFFFFF; color: #0E1B2E; }
.btn.secondary:hover { border-color: #356B8D; color: #356B8D; text-decoration: none; }
.note { color: #6A7686; font-size: 14px; margin: 16px 0 0; }
h2 { font-size: 13px; font-weight: 600; letter-spacing: .1em; text-transform: uppercase; color: #6A7686; margin: 36px 0 12px; }
.example { display: block; width: 100%; height: auto; border: 1px solid #E4E7EB; border-radius: 10px; background: #FFFFFF; }
</style>
</head>
<body>
<main>
<div class="top"><img src="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png" alt=""><b>fin</b><span class="sp"></span><a href="../../">kilianc.github.io/fin</a></div>
<div class="card">
<div class="label">Try this prompt</div>
<div class="prompt" id="prompt">{{.Prompt}}</div>
<div class="actions">
<a class="btn primary" href="{{.Claude}}">Open in Claude Code</a>
<a class="btn secondary" href="{{.Codex}}">Open in Codex</a>
<button class="btn secondary" id="copy" type="button">Copy prompt</button>
</div>
<p class="note">The app opens with the prompt filled in; read it, then press Enter. Needs fin installed and connected to your accounts: <a href="../../">set it up</a> first.</p>
</div>
<h2>What you might get</h2>
<img class="example" src="../../{{.Image}}" alt="{{.Title}}, with made-up numbers">
</main>
<script>
document.getElementById("copy").onclick = function () {
  navigator.clipboard.writeText(document.getElementById("prompt").textContent);
  this.textContent = "Copied";
};
</script>
</body>
</html>
`))

// TryPage renders docs/try/<slug>/index.html.
func TryPage(e Example) string {
	p := e.agentPrompt()
	var b bytes.Buffer
	err := tryPage.Execute(&b, map[string]any{
		"Title":  e.Title,
		"Prompt": e.Prompt,
		"Image":  e.Image,
		"Claude": template.URL("claude-cli://open?q=" + queryEscape(p)),
		"Codex":  template.URL("codex://new?prompt=" + queryEscape(p)),
	})
	if err != nil {
		panic(err)
	}
	return b.String()
}
