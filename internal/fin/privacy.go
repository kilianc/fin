package fin

import "bytes"

var privacyPage = pageTemplate("privacy", `<!doctype html>
<html lang="en">
<head>
{{template "head" .}}
<title>fin · privacy</title>
<meta name="description" content="fin collects nothing. It runs on your machine and talks only to Plaid.">
</head>
<body>
{{template "header" .}}
<main>
<section class="section first wrap">
<p class="kicker">// privacy</p>
<h1 class="display">fin collects nothing.</h1>
<p class="lede"><b><span class="fin">fin</span></b> is a command-line tool that runs on your own computer. There is no fin server, no account and no analytics. Its author never sees your data.</p>
<dl class="help">
<dt>what it reads</dt><dd>Balances, transactions, holdings and investment activity for the institutions you connect, from <a href="https://plaid.com/legal/#end-user-privacy-policy">Plaid</a>, using your own Plaid account. It never asks Plaid for account numbers or personal details, and it cannot move money.</dd>
<dt>where it goes</dt><dd>Straight from Plaid to your computer. <span class="fin">fin</span> prints it to your terminal, or to the AI agent you run it from. That agent and its provider, such as Anthropic or OpenAI, handle it under their own terms.</dd>
<dt>what it stores</dt><dd>Your Plaid keys and access tokens in the macOS Keychain, and connection names and sync times in <code>~/.config/fin/state.json</code>, readable only by you. Nothing else is written to disk.</dd>
<dt>removing it</dt><dd>Delete the Keychain entries for the service <code>fin</code> and the state file, and nothing <span class="fin">fin</span> kept is left on your computer. Your connections live in your own Plaid account; you can review or revoke them at <a href="https://my.plaid.com">my.plaid.com</a>.</dd>
<dt>this website</dt><dd>Hosted on GitHub Pages, which may log visits under <a href="https://docs.github.com/en/site-policy/privacy-policies/github-general-privacy-statement">GitHub's privacy statement</a>. It sets no cookies and runs no analytics.</dd>
<dt>questions</dt><dd>Open an issue at <a href="https://github.com/kilianc/fin/issues">github.com/kilianc/fin</a>.</dd>
</dl>
</section>
</main>
{{template "footer" .}}
</body>
</html>
`)

// PrivacyPage renders docs/privacy/index.html, the privacy policy the
// plugin directory links to.
func PrivacyPage() string {
	var b bytes.Buffer
	if err := privacyPage.Execute(&b, map[string]any{"Root": "../"}); err != nil {
		panic(err)
	}
	return b.String()
}
