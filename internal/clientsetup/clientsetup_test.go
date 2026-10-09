package clientsetup

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTargetNames(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	if tg.MCPName() != "192-0-2-10" || tg.TokenVar() != "OMNIBUS_TOKEN_192_0_2_10" || tg.FileName() != "192.0.2.10" {
		t.Fatalf("%s %s %s", tg.MCPName(), tg.TokenVar(), tg.FileName())
	}
	v6 := Target{Host: "::1", Port: "8765"}
	if v6.MCPURL() != "https://[::1]:8765/mcp" || v6.TokenVar() != "OMNIBUS_TOKEN___1" {
		t.Fatalf("%s %s", v6.MCPURL(), v6.TokenVar())
	}
	if got := (Target{Host: "backup.example.com"}).TokenVar(); got != "OMNIBUS_TOKEN_BACKUP_EXAMPLE_COM" {
		t.Fatal(got)
	}
	// The header must stay a variable in Claude Code's config.
	if add := tg.ClaudeAdd(); !strings.Contains(add, `'Authorization: Bearer ${OMNIBUS_TOKEN_192_0_2_10}'`) {
		t.Fatal(add)
	}
}

// Generated commands must parse in the shells clients use.
func TestScriptsParse(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	for _, script := range []string{tg.NodeTrust(), tg.DebianTrust(), tg.RHELTrust(), NodeEnv + "\n", tg.ExportToken() + "\n", tg.ClaudeAdd() + "\n"} {
		for _, sh := range []string{"sh", "bash"} {
			if _, err := exec.LookPath(sh); err != nil {
				continue
			}
			cmd := exec.Command(sh, "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s -n: %v %s\n%s", sh, err, out, script)
			}
		}
	}
}

func TestTrustCommands(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	want := map[string]string{
		tg.NodeTrust(): `d="$HOME/.config/omnibusmcp"; mkdir -p "$d"
curl -fsSk https://192.0.2.10:8765/ca.pem -o "$d/ca-192.0.2.10.pem"
cat "$d"/ca-*.pem > "$d/ca-bundle.pem"
`,
		tg.DebianTrust(): "sudo curl -fsSk https://192.0.2.10:8765/ca.pem -o /usr/local/share/ca-certificates/omnibusmcp-192.0.2.10.crt && sudo update-ca-certificates\n",
		tg.RHELTrust():   "sudo curl -fsSk https://192.0.2.10:8765/ca.pem -o /etc/pki/ca-trust/source/anchors/omnibusmcp-192.0.2.10.pem && sudo update-ca-trust\n",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got:\n%s\nwant:\n%s", got, w)
		}
	}
}

// The CLI and the landing page show the same steps.
func TestLandingPage(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	page := LandingPage(tg, true, "/etc/omnibusmcp/token")
	if !strings.HasPrefix(page, "# OmnibusMCP\nEndpoint MCP: https://192.0.2.10:8765/mcp") ||
		!strings.HasSuffix(page, Instructions(tg, "/etc/omnibusmcp/token")) {
		t.Errorf("page:\n%s", page)
	}
	for _, want := range []string{tg.NodeTrust(), NodeEnv, tg.DebianTrust(), tg.RHELTrust(), "sudo cat /etc/omnibusmcp/token", tg.ExportToken(), tg.ClaudeAdd()} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "openssl") {
		t.Error("page still checks a fingerprint")
	}
	if p := LandingPage(tg, false, "/t"); strings.Contains(p, "ca.pem") || !strings.Contains(p, "administratora") {
		t.Errorf("page without an OmnibusMCP CA:\n%s", p)
	}
}

func TestClientHost(t *testing.T) {
	if h := ClientHost("192.0.2.10:8765", nil); h != "192.0.2.10" {
		t.Error(h)
	}
	if h := ClientHost("0.0.0.0:8765", []string{"localhost", "127.0.0.1", "vm.example", "192.0.2.10"}); h != "vm.example" {
		t.Error(h)
	}
}
