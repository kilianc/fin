package fin

import (
	"bytes"
	"html/template"
	"strings"
)

// Example is a prompt from the gallery in the README and on the landing page.
// Its try page, docs/try/<slug>/, opens the prompt in Claude Code or Codex.
type Example struct {
	Slug   string
	Title  string
	Ask    string // the headline on the card
	Detail string // the line under it
	Image  string // the gallery card, relative to docs/
}

// Examples are the gallery's prompts, in order. docs/examples/index.html
// shows each Ask and Detail verbatim on its card.
var Examples = []Example{
	{"spending", "Spending report", "Build me a monthly spending review.", "Compare it to the month before.", "examples/1.png"},
	{"subscriptions", "Subscription audit", "Find the subscriptions worth reviewing.", "Find subscriptions, price increases, and anything I’m paying for twice.", "examples/2.png"},
	{"portfolio", "Portfolio review", "Put all my investments in one report.", "Show my allocation and unrealized gains across brokerages.", "examples/3.png"},
	{"net-worth", "Net worth", "Show how my net worth changed this year.", "Chart it month by month and explain what moved it.", "examples/4.png"},
}

// Prompt is what the person asks: the card's headline and the line under it.
func (e Example) Prompt() string { return e.Ask + " " + e.Detail }

// TryURL is the example's try page on GitHub Pages.
func (e Example) TryURL() string { return PagesURL + "try/" + e.Slug + "/" }

// agentPrompt tells the agent to answer with fin; the person's question
// follows verbatim.
func (e Example) agentPrompt() string {
	return "Use fin, my local command-line tool for my bank and brokerage data (run `fin help` to see its commands), to answer this with a short report: " + e.Prompt()
}

var tryPage = template.Must(template.New("try").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>fin · try it: {{.Title}}</title>
<meta name="description" content="{{.Prompt}}">
<meta property="og:title" content="fin · {{.Title}}">
<meta property="og:description" content="{{.Prompt}}">
<meta property="og:image" content="` + PagesURL + `{{.Image}}">
<meta name="twitter:card" content="summary_large_image">
<link rel="icon" href="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Geist:wght@400;500;600&family=Geist+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
body { margin: 0; background: #F6F7F9; color: #111A26; font: 17px/1.6 "Geist", -apple-system, sans-serif; }
a { color: #2C5F80; } a:hover { color: #173F59; }
.wrap { max-width: 1040px; margin: 0 auto; padding: 0 24px; }
.mono { font-family: "Geist Mono", ui-monospace, monospace; }
header { display: flex; align-items: center; gap: 10px; padding-top: 24px; font-size: 14px; }
header img { width: 30px; height: 30px; }
header b { font-weight: 600; }
header .dim { color: #7D8794; }
header .sp { flex: 1; }
header a { color: #3A4656; text-decoration: none; }
.kicker { font-size: 13px; color: #7D8794; margin: 72px 0 14px; }
h1 { font-size: 48px; line-height: 1.08; letter-spacing: -0.035em; font-weight: 600; margin: 0 0 14px; max-width: 820px; }
.detail { font-size: 21px; color: #4A5666; margin: 0 0 32px; max-width: 720px; }
.buttons { display: flex; flex-wrap: wrap; gap: 12px; }
.buttons img { height: 52px; display: block; }
.shell { margin-top: 48px; background: #131B26; border-radius: 12px; overflow: hidden; box-shadow: 0 1px 2px rgba(17,26,38,.06), 0 12px 32px -12px rgba(17,26,38,.18); }
.bar { display: flex; align-items: center; gap: 8px; padding: 10px 12px 10px 16px; border-bottom: 1px solid #243041; font-size: 12px; color: #6F7E90; }
.bar i { width: 10px; height: 10px; border-radius: 50%; background: #2D394A; }
.bar span { margin-left: 8px; flex: 1; }
.bar button { font: 12px "Geist Mono", ui-monospace, monospace; color: #D8E0E8; background: none; border: 1px solid #2D394A; border-radius: 6px; padding: 4px 10px; cursor: pointer; }
.bar button:hover { border-color: #81B1CF; color: #FFFFFF; }
.shell pre { margin: 0; padding: 20px 22px 24px; font: 14px/1.7 "Geist Mono", ui-monospace, monospace; color: #D8E0E8; white-space: pre-wrap; background: #1C2633; }
.shell pre .you { color: #81B1CF; font-weight: 600; }
.shell pre .ask { color: #FFFFFF; }
.note { font-size: 15px; color: #4A5666; margin: 14px 0 0; }
h2 { font-size: 13px; font-weight: 400; color: #7D8794; margin: 72px 0 14px; }
.example { display: block; width: 100%; height: auto; border: 1px solid #E1E4E8; border-radius: 8px; background: #FFFFFF; }
footer { margin-top: 96px; padding: 20px 24px 48px; border-top: 1px solid #E1E4E8; display: flex; gap: 12px; font-size: 13px; color: #7D8794; }
footer .sp { flex: 1; }
footer a { color: #3A4656; }
@media (max-width: 640px) { h1 { font-size: 36px; } .detail { font-size: 18px; } .kicker { margin-top: 48px; } header .dim { display: none; } }
</style>
</head>
<body>
<header class="wrap mono">
<img src="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png" alt=""><b>fin</b><span class="dim">~ try it</span><span class="sp"></span><a href="../../#examples">← all examples</a>
</header>
<main class="wrap">
<p class="kicker mono">// example {{.N}} of {{.Of}} · {{.Title}}</p>
<h1>“{{.Ask}}”</h1>
<p class="detail">{{.Detail}}</p>
<div class="buttons">
<a href="{{.Claude}}"><img src="../../try-in-claude.svg" alt="Try in Claude Code"></a>
<a href="{{.Codex}}"><img src="../../try-in-codex.svg" alt="Try in Codex"></a>
</div>
<div class="shell">
<div class="bar mono"><i></i><i></i><i></i><span>what your agent receives</span><button id="copy" type="button">copy</button></div>
<pre id="prompt"><span class="you">you ›</span> {{.Lead}}<span class="ask">{{.Prompt}}</span></pre>
</div>
<p class="note">The app opens with this prompt filled in; read it, then press Enter. It needs fin installed and connected to your accounts, so <a href="../../">set it up first</a> if you haven't.</p>
<h2 class="mono">// what you might get · made-up numbers</h2>
<img class="example" src="../../{{.Image}}" alt="{{.Title}}, with made-up numbers">
</main>
<footer class="wrap mono"><span>fin · MIT</span><span class="sp"></span><a href="https://github.com/kilianc/fin">github.com/kilianc/fin</a></footer>
<script>
document.getElementById("copy").onclick = function () {
  var t = document.getElementById("prompt").textContent.replace(/^you › /, "");
  navigator.clipboard.writeText(t);
  this.textContent = "copied";
};
</script>
</body>
</html>
`))

// TryPage renders docs/try/<slug>/index.html.
func TryPage(e Example) string {
	p := e.agentPrompt()
	var b bytes.Buffer
	n := 0
	for i, x := range Examples {
		if x.Slug == e.Slug {
			n = i + 1
		}
	}
	err := tryPage.Execute(&b, map[string]any{
		"N":      n,
		"Of":     len(Examples),
		"Title":  e.Title,
		"Ask":    e.Ask,
		"Detail": e.Detail,
		"Lead":   strings.TrimSuffix(p, e.Prompt()),
		"Prompt": e.Prompt(),
		"Image":  e.Image,
		"Claude": template.URL("claude-cli://open?q=" + queryEscape(p)),
		"Codex":  template.URL("codex://new?prompt=" + queryEscape(p)),
	})
	if err != nil {
		panic(err)
	}
	return b.String()
}
