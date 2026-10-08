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
	for _, script := range []string{
		tg.SystemTrust("AA:BB"), tg.UserTrust("AA:BB"),
		tg.SystemTrust(""), tg.UserTrust(""),
	} {
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

// With a fingerprint the commands refuse a mismatch; without one (the
// landing page) they need neither the check nor openssl.
func TestFingerprintCheck(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	for _, script := range []string{tg.SystemTrust("AA:BB"), tg.UserTrust("AA:BB")} {
		if !strings.Contains(script, `!= "AA:BB"`) || !strings.Contains(script, "FINGERPRINT MISMATCH") {
			t.Errorf("checked script:\n%s", script)
		}
	}
	for _, script := range []string{tg.SystemTrust(""), tg.UserTrust("")} {
		if strings.Contains(script, "openssl") || strings.Contains(script, "FINGERPRINT") {
			t.Errorf("unchecked script:\n%s", script)
		}
	}
}

func TestLandingPage(t *testing.T) {
	tg := Target{Host: "192.0.2.10", Port: "8765"}
	want := `## OmnibusMCP
MCP endpoint: https://192.0.2.10:8765/mcp  (Streamable HTTP, header "Authorization: Bearer <token>")

## Install OmnibusMCP on the client
1. Trust the server's certificate https://192.0.2.10:8765/ca.pem
2. Token: ask the administrator (on the server: sudo cat /etc/omnibusmcp/token).
3. Add OmnibusMCP to your agent
`
	if got := LandingPage(tg, true, "/etc/omnibusmcp/token"); got != want {
		t.Errorf("page:\n%s\nwant:\n%s", got, want)
	}
	if p := LandingPage(tg, false, "/t"); strings.Contains(p, "ca.pem") || !strings.Contains(p, "administrator") {
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
