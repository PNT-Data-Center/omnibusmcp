package clientsetup

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// The style and script in the page must be exactly what the CSP hashes.
func TestLandingHTMLMatchesCSP(t *testing.T) {
	page := string(LandingHTML(Target{Host: "192.0.2.10", Port: "8765"}, true, "/etc/omnibusmcp/token"))
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(page)
	script := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(page)
	if style == nil || script == nil {
		t.Fatalf("page without style or script:\n%s", page)
	}
	for _, part := range []string{style[1], script[1]} {
		if !strings.Contains(PageCSP, "'"+cspHash(part)+"'") {
			t.Errorf("CSP %q does not allow:\n%s", PageCSP, part)
		}
	}
	if strings.Count(page, "<script") != 1 || strings.Contains(page, "http://") || strings.Contains(page, "src=") {
		t.Error("page loads or runs something else")
	}
}

// The HTML page shows the same commands as the text versions, escaped.
func TestLandingHTMLSteps(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	page := string(LandingHTML(tg, true, "/etc/omnibusmcp/token"))
	steps := Steps(tg, "/etc/omnibusmcp/token")
	cmds := 0
	for _, b := range steps {
		if !strings.Contains(page, html.EscapeString(b.Text)) {
			t.Errorf("page lacks %q", b.Text)
		}
		if b.Kind == Command {
			cmds++
		}
	}
	if n := strings.Count(page, ">Kopiuj</button>"); n != cmds {
		t.Errorf("%d copy buttons for %d commands", n, cmds)
	}
	if strings.Contains(page, `"$HOME`) || !strings.Contains(page, "&#34;$HOME") {
		t.Error("commands are not HTML-escaped")
	}

	none := string(LandingHTML(tg, false, "/t"))
	if strings.Contains(none, "ca.pem") || strings.Contains(none, ">Kopiuj</button>") || !strings.Contains(none, "administratora") {
		t.Errorf("page without an OmnibusMCP CA:\n%s", none)
	}
}

// Every rendering lists all steps, in order.
func TestRenderPlain(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	text := Instructions(tg, "/etc/omnibusmcp/token")
	pos := 0
	for _, b := range Steps(tg, "/etc/omnibusmcp/token") {
		i := strings.Index(text[pos:], PlainStyle(b))
		if i < 0 {
			t.Fatalf("%q missing or out of order in:\n%s", PlainStyle(b), text)
		}
		pos += i
	}
	if !strings.HasPrefix(text, "## 1. Zaufanie") || !strings.Contains(text, "\n\n## 2. Token\n") {
		t.Errorf("layout:\n%s", text)
	}
}
