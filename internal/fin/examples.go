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

var tryPage = pageTemplate("try", `<!doctype html>
<html lang="en">
<head>
{{template "head" .}}
<title>fin · try it: {{.Ex.Title}}</title>
<meta name="description" content="{{.Prompt}}">
<meta property="og:title" content="fin · {{.Ex.Title}}">
<meta property="og:description" content="{{.Prompt}}">
<meta property="og:image" content="`+PagesURL+`{{.Ex.Image}}">
<meta name="twitter:card" content="summary_large_image">
</head>
<body>
{{template "header" .}}
<main>
<section class="section first wrap">
<ul class="ex-nav" aria-label="Examples">
{{- range $i, $e := .All}}
<li><a href="{{$.Root}}try/{{$e.Slug}}/"{{if eq $e.Slug $.Ex.Slug}} aria-current="page"{{end}}><b>0{{inc $i}}</b>{{$e.Title}}</a></li>
{{- end}}
</ul>
<h1 class="display">“{{.Ex.Ask}}”</h1>
<p class="lede">{{.Ex.Detail}}</p>
<div class="buttons">
<a href="{{.Claude}}"><img src="{{.Root}}try-in-claude.svg" alt="Try in Claude Code"></a>
<a href="{{.Codex}}"><img src="{{.Root}}try-in-codex.svg" alt="Try in Codex"></a>
</div>
</section>
<section class="section tight wrap">
<div class="shell">
<div class="shell-bar"><span class="dots" aria-hidden="true"><i></i><i></i><i></i></span><span class="label">what your agent receives</span><button class="copy" id="copy" type="button">copy</button></div>
<pre class="prose" id="prompt"><span class="you">you ›</span> {{.Lead}}<span class="hi">{{.Prompt}}</span></pre>
</div>
<p class="note">The app opens with this prompt filled in; read it, then press Enter. It needs fin installed and connected to your accounts, so <a href="{{.Root}}">set it up first</a> if you haven't.</p>
</section>
<section class="section wrap">
<p class="kicker">// what you might get · made-up numbers</p>
<img class="report" src="{{.Root}}{{.Ex.Image}}" alt="{{.Ex.Title}}, with made-up numbers">
<nav class="pager" aria-label="More examples">
{{- with .Prev}}
<a class="prev" href="{{$.Root}}try/{{.Slug}}/"><small>← previous</small><span>“{{.Ask}}”</span></a>
{{- end}}
{{- with .Next}}
<a class="next" href="{{$.Root}}try/{{.Slug}}/"><small>next →</small><span>“{{.Ask}}”</span></a>
{{- end}}
</nav>
</section>
</main>
{{template "footer" .}}
<script>
document.getElementById("copy").onclick = function () {
  navigator.clipboard.writeText(document.getElementById("prompt").textContent.replace(/^you › /, ""));
  this.textContent = "copied";
};
</script>
</body>
</html>
`)

// TryPage renders docs/try/<slug>/index.html.
func TryPage(e Example) string {
	p := e.agentPrompt()
	data := map[string]any{
		"Root":   "../../",
		"Ex":     e,
		"All":    Examples,
		"Lead":   strings.TrimSuffix(p, e.Prompt()),
		"Prompt": e.Prompt(),
		"Claude": template.URL("claude-cli://open?q=" + queryEscape(p)),
		"Codex":  template.URL("codex://new?prompt=" + queryEscape(p)),
	}
	for i, x := range Examples {
		if x.Slug != e.Slug {
			continue
		}
		if i > 0 {
			data["Prev"] = Examples[i-1]
		}
		if i < len(Examples)-1 {
			data["Next"] = Examples[i+1]
		}
	}
	var b bytes.Buffer
	if err := tryPage.Execute(&b, data); err != nil {
		panic(err)
	}
	return b.String()
}
