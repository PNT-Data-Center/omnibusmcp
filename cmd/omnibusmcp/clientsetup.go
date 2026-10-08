package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/certs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/clientsetup"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/server"
	"github.com/PNT-Data-Center/omnibusmcp/internal/ui"
)

func tlsClientSetup(args []string) error {
	fs := flag.NewFlagSet("tls client-setup", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	host := fs.String("host", "", "name or IP clients use to reach this server (default: from listen or the certificate)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadInstalledConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.TLS.Enabled() {
		return errors.New("TLS is not configured; run 'omnibusmcp tls generate' first")
	}
	return printClientSetup(cfg, *host)
}

// printClientSetup prints copy-and-paste commands for a client host. The
// commands download the CA from the server without trusting it yet, so they
// embed its fingerprint and refuse a mismatch: whoever copies them from this
// (SSH-authenticated) console gets an end-to-end check.
func printClientSetup(cfg *config.Config, host string) error {
	p := server.TLSPaths(cfg)
	kind, _, err := certs.Classify(p)
	if err != nil {
		return err
	}
	if kind != certs.Managed {
		return errors.New("the certificate is not issued by a local OmnibusMCP CA; run 'omnibusmcp tls generate --force' first")
	}
	ca, err := certs.LoadCA(p)
	if err != nil {
		return err
	}
	leaf, err := certs.Load(p.Cert)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	if host == "" {
		host = clientsetup.ClientHost(cfg.Listen, leaf.Hosts())
	}
	if !contains(leaf.Hosts(), host) {
		fmt.Println(ui.Yellow(fmt.Sprintf("warning: %s is not in the certificate (%s); clients will reject it. Use --host or regenerate with --hosts.",
			host, strings.Join(leaf.Hosts(), ", "))))
	}
	t := clientsetup.Target{Host: host, Port: port}

	fmt.Printf("%s %s\n\n", ui.Bold("OmnibusMCP client setup for"), ui.Cyan(t.MCPURL()))
	fmt.Println(ui.Label("CA download", 13) + t.CAURL())
	fmt.Println(ui.Label("CA sha256", 13) + ca.Fingerprint)
	fmt.Println(ui.Dim("The commands check this fingerprint; copy them only from this server's console."))

	fmt.Printf("\n%s\n%s\n\n", ui.Bold("1. Trust the CA on the client"),
		ui.Dim("   Linux (Debian/Ubuntu, RHEL/AlmaLinux); needs curl, openssl and root or sudo. Works for Claude Code, curl and most tools:"))
	fmt.Print(t.SystemTrust(ca.Fingerprint))
	fmt.Printf("\n%s\n\n", ui.Dim("   Without sudo, for Node.js-based clients only (e.g. Gemini CLI), per user:"))
	fmt.Print(t.UserTrust(ca.Fingerprint))

	fmt.Printf("\n%s\n", ui.Bold("2. Token"))
	fmt.Printf("   On this server: %s\n", ui.Cyan("sudo cat "+cfg.TokenFile))
	fmt.Printf("   On the client keep it in %s (mode 0600) and add to your shell profile:\n   %s\n", t.TokenFile(), ui.Cyan(t.ExportToken()))

	fmt.Printf("\n%s %s\n", ui.Bold("3. Claude Code"), ui.Dim("(single quotes: the token stays a variable, it is not written to the config)"))
	fmt.Printf("   %s\n", ui.Cyan(t.ClaudeAdd()))
	fmt.Printf("   %s\n", ui.Dim("check: claude mcp list"))
	fmt.Printf("\n%s\n", ui.Dim("Anyone can also open "+t.BaseURL()+"/ with curl -k or a browser for these instructions (without the fingerprint guarantee)."))
	return nil
}
