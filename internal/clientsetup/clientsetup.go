// Package clientsetup builds the commands that make a client trust an
// OmnibusMCP server's CA and register the server in an MCP client. The CLI
// ("omnibusmcp tls client-setup") and the server's landing page share them.
package clientsetup

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// CAPath is where the server publishes its CA certificate.
const CAPath = "/ca.pem"

var (
	fileSafe    = regexp.MustCompile(`[^A-Za-z0-9.-]`)
	mcpNameSafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	envSafe     = regexp.MustCompile(`[^A-Z0-9_]`)
)

// Target is one server as clients reach it.
type Target struct {
	Host string // name or IP clients use
	Port string
}

// BaseURL is https://host:port.
func (t Target) BaseURL() string { return "https://" + net.JoinHostPort(t.Host, t.Port) }

// MCPURL is the MCP endpoint.
func (t Target) MCPURL() string { return t.BaseURL() + "/mcp" }

// CAURL is the CA download address.
func (t Target) CAURL() string { return t.BaseURL() + CAPath }

// FileName is the host made safe for file names.
func (t Target) FileName() string { return fileSafe.ReplaceAllString(t.Host, "_") }

// MCPName is the server name for MCP clients: the host with every
// character other than letters, digits, "-" and "_" (the only ones Claude
// Code allows) replaced by "-".
func (t Target) MCPName() string { return mcpNameSafe.ReplaceAllString(t.Host, "-") }

// TokenVar is the environment variable holding this server's token. Every
// server has its own token, so the name includes the host.
func (t Target) TokenVar() string {
	return "OMNIBUS_TOKEN_" + envSafe.ReplaceAllString(strings.ToUpper(t.Host), "_")
}

// TokenFile is where the client keeps the token (mode 0600).
func (t Target) TokenFile() string { return "~/.config/omnibusmcp/" + t.FileName() + ".token" }

// ClaudeAdd registers the server in Claude Code. The header is in single
// quotes so the token stays a variable instead of being written to the config.
func (t Target) ClaudeAdd() string {
	return fmt.Sprintf("claude mcp add --transport http --scope user %s %s --header 'Authorization: Bearer ${%s}'",
		t.MCPName(), t.MCPURL(), t.TokenVar())
}

// ExportToken is the profile line that loads the token from its file.
func (t Target) ExportToken() string {
	return fmt.Sprintf(`export %s="$(cat %s)"`, t.TokenVar(), t.TokenFile())
}

// SystemTrust adds the CA to the system trust store. With a fingerprint it
// first compares the download with it and refuses a mismatch; without one
// it trusts whatever the server sends (trust on first use). It runs in a
// subshell, so "set -e" and "exit" never close the user's terminal.
func (t Target) SystemTrust(fingerprint string) string {
	return fmt.Sprintf(`( set -e
  f=$(mktemp); trap 'rm -f "$f"' EXIT
  curl -fsSk %[1]s -o "$f"
%[2]s  s=sudo; [ "$(id -u)" -eq 0 ] && s=
  if [ -d /usr/local/share/ca-certificates ]; then
    $s install -m 0644 "$f" /usr/local/share/ca-certificates/omnibusmcp-%[3]s.crt && $s update-ca-certificates
  else
    $s install -m 0644 "$f" /etc/pki/ca-trust/source/anchors/omnibusmcp-%[3]s.pem && $s update-ca-trust
  fi
  echo "OK: OmnibusMCP CA of %[4]s is trusted" )
`, t.CAURL(), fingerprintCheck(fingerprint, `"$f"`, ""), t.FileName(), t.Host)
}

// fingerprintCheck is the script line refusing a CA whose SHA-256 differs
// from fingerprint ("" when there is nothing to compare with).
func fingerprintCheck(fingerprint, file, cleanup string) string {
	if fingerprint == "" {
		return ""
	}
	return fmt.Sprintf(`  fp=$(openssl x509 -in %[1]s -noout -fingerprint -sha256 | cut -d= -f2)
  if [ "$fp" != "%[2]s" ]; then %[3]secho "FINGERPRINT MISMATCH ($fp): CA NOT trusted" >&2; exit 1; fi
`, file, fingerprint, cleanup)
}

// UserTrust keeps the CA in the user's config directory and rebuilds one
// bundle file, since NODE_EXTRA_CA_CERTS takes a single file. The
// fingerprint works as in SystemTrust.
func (t Target) UserTrust(fingerprint string) string {
	return fmt.Sprintf(`( set -e
  d="$HOME/.config/omnibusmcp"; mkdir -p "$d"; f="$d/ca-%[3]s.pem"
  curl -fsSk %[1]s -o "$f.new"
%[2]s  mv "$f.new" "$f"; cat "$d"/ca-*.pem > "$d/ca-bundle.pem"
  echo "OK. Add to your shell profile: export NODE_EXTRA_CA_CERTS=$d/ca-bundle.pem" )
`, t.CAURL(), fingerprintCheck(fingerprint, `"$f.new"`, `rm -f "$f.new"; `), t.FileName())
}

// ClientHost picks the address clients most likely use: the listen address
// unless it is a wildcard, else the first non-loopback certificate host.
func ClientHost(listen string, sans []string) string {
	if h, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(h); h != "" && (ip == nil || !ip.IsUnspecified()) {
			return h
		}
	}
	for _, h := range sans {
		ip := net.ParseIP(h)
		if h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return h
		}
	}
	return "localhost"
}

// LandingPage is the short Markdown page served at "/" over HTTPS to
// anyone (e.g. "curl -k https://host:8765/"): where the MCP endpoint and
// the CA are, and the steps a client needs. Ready-made commands are printed
// by "omnibusmcp tls client-setup" on the server.
func LandingPage(t Target, hasCA bool, serverTokenFile string) string {
	head := fmt.Sprintf("## OmnibusMCP\nMCP endpoint: %s  (Streamable HTTP, header \"Authorization: Bearer <token>\")\n", t.MCPURL())
	if !hasCA {
		return head + "\nThis server does not use an OmnibusMCP CA; ask its administrator how to trust its certificate.\n"
	}
	return head + fmt.Sprintf(`
## Install OmnibusMCP on the client
1. Trust the server's certificate %s
2. Token: ask the administrator (on the server: sudo cat %s).
3. Add OmnibusMCP to your agent
`, t.CAURL(), serverTokenFile)
}
