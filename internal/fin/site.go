package fin

import (
	"bytes"
	"html/template"
)

// siteParts are the head tags, header and footer every page on the site
// shares: the hand-written landing page, the setup pages and the try pages.
// Root is the relative path back to docs/ ("./", "../" or "../../"), so the
// site works from any folder it is served from. All styles live in
// docs/site.css.
const siteParts = `{{define "head"}}<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Geist:wght@400;500;600&family=Geist+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<link rel="stylesheet" href="{{.Root}}site.css?v={{cssVersion}}">{{end}}
{{define "header"}}<header class="site-header wrap">
<a class="brand" href="{{.Root}}"><img src="https://raw.githubusercontent.com/kilianc/fin/main/mascot.png" alt=""><b>fin</b></a><span class="where">~ local</span>
<span class="sp"></span>
<nav><a href="{{.Root}}#examples">examples</a><a href="{{.Root}}#security">security</a><a href="https://github.com/kilianc/fin">github ↗</a></nav>
</header>{{end}}
{{define "footer"}}<footer class="site-footer wrap"><span>fin · MIT</span><span class="sp"></span><a href="https://github.com/kilianc/fin">github.com/kilianc/fin</a></footer>{{end}}`

// SiteCSSVersion is a short hash of docs/site.css. Pages link the
// stylesheet with it so browsers fetch a fresh copy whenever it changes; the
// tests set it from the file before checking or regenerating docs/.
var SiteCSSVersion string

var siteTemplates = template.Must(template.New("site").Funcs(template.FuncMap{
	"inc":        func(i int) int { return i + 1 },
	"cssVersion": func() string { return SiteCSSVersion },
}).Parse(siteParts))

// pageTemplate parses a page that uses the shared parts.
func pageTemplate(name, text string) *template.Template {
	return template.Must(template.Must(siteTemplates.Clone()).New(name).Parse(text))
}

// sitePart renders one shared part for a page at root; the tests check the
// landing page carries the same markup as the generated pages.
func sitePart(name, root string) string {
	var b bytes.Buffer
	if err := siteTemplates.ExecuteTemplate(&b, name, map[string]string{"Root": root}); err != nil {
		panic(err)
	}
	return b.String()
}
