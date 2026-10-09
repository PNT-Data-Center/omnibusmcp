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

// NodeTrust keeps the CA in the user's config directory and rebuilds one
// bundle for all servers, since NODE_EXTRA_CA_CERTS takes a single file.
// No sudo needed; it serves Node.js-based clients only.
func (t Target) NodeTrust() string {
	return fmt.Sprintf(`d="$HOME/.config/omnibusmcp"; mkdir -p "$d"
curl -fsSk %s -o "$d/ca-%s.pem"
cat "$d"/ca-*.pem > "$d/ca-bundle.pem"
`, t.CAURL(), t.FileName())
}

// DebianTrust adds the CA to the system store on Debian/Ubuntu. curl -f
// writes nothing on an error, and "&&" then skips the store update.
func (t Target) DebianTrust() string {
	return fmt.Sprintf("sudo curl -fsSk %s -o /usr/local/share/ca-certificates/omnibusmcp-%s.crt && sudo update-ca-certificates\n", t.CAURL(), t.FileName())
}

// RHELTrust adds the CA to the system store on RHEL/AlmaLinux.
func (t Target) RHELTrust() string {
	return fmt.Sprintf("sudo curl -fsSk %s -o /etc/pki/ca-trust/source/anchors/omnibusmcp-%s.pem && sudo update-ca-trust\n", t.CAURL(), t.FileName())
}

// NodeEnv is the one profile line Node.js clients need for every server.
const NodeEnv = `export NODE_EXTRA_CA_CERTS="$HOME/.config/omnibusmcp/ca-bundle.pem"`

// Kind is the role of a Block in the instructions.
type Kind int

const (
	Heading    Kind = iota // "1. Zaufanie do certyfikatu serwera"
	Subheading             // "Gemini CLI i inne klienty Node.js (bez sudo)"
	Text                   // explanation
	Command                // lines to copy into a shell, as they are
)

// Block is one element of the instructions.
type Block struct {
	Kind Kind
	Text string
}

// Steps are the client setup steps shared by "omnibusmcp tls
// client-setup", the landing page and the documentation, each rendering
// them in its own format. The CA is downloaded without verification
// (trust on first use).
func Steps(t Target, serverTokenFile string) []Block {
	cmd := func(s string) Block { return Block{Command, strings.TrimSuffix(s, "\n")} }
	return []Block{
		{Heading, "1. Zaufanie do certyfikatu serwera"},
		{Subheading, "Gemini CLI i inne klienty Node.js (bez sudo)"},
		cmd(t.NodeTrust()),
		{Text, "W ~/.bashrc wystarczy raz dodać (jedna linia dla wszystkich serwerów):"},
		cmd(NodeEnv),
		{Subheading, "Claude Code, curl i reszta systemu (sudo)"},
		{Text, "Debian/Ubuntu:"},
		cmd(t.DebianTrust()),
		{Text, "RHEL/AlmaLinux:"},
		cmd(t.RHELTrust()),
		{Text, "Po odnowieniu certyfikatu na serwerze powtórz te same polecenia."},
		{Heading, "2. Token"},
		{Text, "Na serwerze:"},
		cmd("sudo cat " + serverTokenFile),
		{Text, "Na kliencie zapisz go w " + t.TokenFile() + " (chmod 600) i dodaj do ~/.bashrc:"},
		cmd(t.ExportToken()),
		{Heading, "3. Claude Code"},
		cmd(t.ClaudeAdd()),
		{Text, "Sprawdzenie:"},
		cmd("claude mcp list"),
	}
}

// Style formats one block of the text rendering.
type Style func(Block) string

// PlainStyle marks headings Markdown-style and leaves the rest as it is.
func PlainStyle(b Block) string {
	switch b.Kind {
	case Heading:
		return "## " + b.Text
	case Subheading:
		return "### " + b.Text
	}
	return b.Text
}

// Render lays the steps out as text, one block per line group, with a
// blank line before every heading.
func Render(steps []Block, style Style) string {
	var sb strings.Builder
	for i, b := range steps {
		if i > 0 && (b.Kind == Heading || b.Kind == Subheading) {
			sb.WriteString("\n")
		}
		sb.WriteString(style(b) + "\n")
	}
	return sb.String()
}

// Instructions are the steps as plain text.
func Instructions(t Target, serverTokenFile string) string {
	return Render(Steps(t, serverTokenFile), PlainStyle)
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

// LandingPage is the page served at "/" over HTTPS to anyone (e.g. "curl
// -k https://host:8765/"): the MCP endpoint and the client setup steps.
func LandingPage(t Target, hasCA bool, serverTokenFile string) string {
	head := fmt.Sprintf("# OmnibusMCP\nEndpoint MCP: %s  (Streamable HTTP, nagłówek \"Authorization: Bearer <token>\")\n", t.MCPURL())
	if !hasCA {
		return head + "\nTen serwer nie używa CA OmnibusMCP; zapytaj administratora, jak zaufać jego certyfikatowi.\n"
	}
	return head + "\n" + Instructions(t, serverTokenFile)
}
