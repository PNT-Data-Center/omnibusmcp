package clientsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
)

// The page has no external resources; its style and script are allowed by
// hash in the Content-Security-Policy, so nothing else can run.
const (
	pageStyle = `
:root { color-scheme: light dark; --fg: #1f2328; --muted: #59636e; --bg: #ffffff; --code: #f6f8fa; --line: #d1d9e0; --accent: #0969da; }
@media (prefers-color-scheme: dark) { :root { --fg: #e6edf3; --muted: #9198a1; --bg: #0d1117; --code: #161b22; --line: #3d444d; --accent: #4493f8; } }
body { margin: 0; background: var(--bg); color: var(--fg); font: 15px/1.5 system-ui, sans-serif; }
main { max-width: 860px; margin: 0 auto; padding: 24px 16px 48px; }
h1 { font-size: 26px; margin: 0 0 4px; }
h2 { font-size: 20px; margin: 32px 0 8px; padding-bottom: 4px; border-bottom: 1px solid var(--line); color: var(--accent); }
h3 { font-size: 16px; margin: 20px 0 6px; }
p { margin: 8px 0 4px; }
.muted { color: var(--muted); }
code { font: 13px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace; }
.cmd { position: relative; margin: 4px 0 8px; }
.cmd pre { margin: 0; padding: 10px 84px 10px 12px; background: var(--code); border: 1px solid var(--line); border-radius: 6px; white-space: pre-wrap; overflow-wrap: anywhere; }
.cmd button { position: absolute; top: 6px; right: 6px; font: 12px system-ui, sans-serif; padding: 3px 10px; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--fg); cursor: pointer; }
.cmd button:hover { border-color: var(--accent); }
`
	pageScript = `
document.querySelectorAll(".cmd button").forEach(function (b) {
  b.addEventListener("click", function () {
    var text = b.parentNode.querySelector("pre").innerText;
    var done = function () { b.textContent = "Skopiowano"; setTimeout(function () { b.textContent = "Kopiuj"; }, 1500); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done);
      return;
    }
    var ta = document.createElement("textarea");
    ta.value = text; document.body.appendChild(ta); ta.select();
    try { document.execCommand("copy"); done(); } finally { document.body.removeChild(ta); }
  });
});
`
)

// PageCSP is the Content-Security-Policy of the HTML landing page.
var PageCSP = "default-src 'none'; style-src '" + cspHash(pageStyle) + "'; script-src '" + cspHash(pageScript) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func cspHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256-" + base64.StdEncoding.EncodeToString(h[:])
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="pl">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OmnibusMCP {{.Host}}</title>
<style>{{.Style}}</style>
</head>
<body>
<main>
<h1>OmnibusMCP</h1>
<p class="muted">Endpoint MCP: <code>{{.MCPURL}}</code> (Streamable HTTP, nagłówek <code>Authorization: Bearer &lt;token&gt;</code>)</p>
{{if .Steps}}{{range .Steps}}{{if eq .Kind 0}}<h2>{{.Text}}</h2>
{{else if eq .Kind 1}}<h3>{{.Text}}</h3>
{{else if eq .Kind 2}}<p>{{.Text}}</p>
{{else}}<div class="cmd"><pre><code>{{.Text}}</code></pre><button type="button">Kopiuj</button></div>
{{end}}{{end}}{{else}}<p>Ten serwer nie używa CA OmnibusMCP; zapytaj administratora, jak zaufać jego certyfikatowi.</p>
{{end}}</main>
<script>{{.Script}}</script>
</body>
</html>
`))

// LandingHTML is the landing page for browsers: the same steps as
// LandingPage, with headings, code blocks and copy buttons.
func LandingHTML(t Target, hasCA bool, serverTokenFile string) []byte {
	data := map[string]any{
		"Host":   t.Host,
		"MCPURL": t.MCPURL(),
		"Style":  template.CSS(pageStyle),
		"Script": template.JS(pageScript),
	}
	if hasCA {
		data["Steps"] = Steps(t, serverTokenFile)
	}
	var b bytes.Buffer
	if err := pageTmpl.Execute(&b, data); err != nil {
		panic(err) // the template and its data are fixed
	}
	return b.Bytes()
}
