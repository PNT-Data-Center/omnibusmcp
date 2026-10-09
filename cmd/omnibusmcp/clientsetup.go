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

// printClientSetup prints copy-and-paste commands for a client host; the
// landing page at "/" serves the same text.
func printClientSetup(cfg *config.Config, host string) error {
	p := server.TLSPaths(cfg)
	kind, _, err := certs.Classify(p)
	if err != nil {
		return err
	}
	if kind != certs.Managed {
		return errors.New("the certificate is not issued by a local OmnibusMCP CA; run 'omnibusmcp tls generate --force' first")
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
		fmt.Println(ui.Yellow(fmt.Sprintf("uwaga: %s nie występuje w certyfikacie (%s), klienci go odrzucą. Użyj --host albo wygeneruj certyfikat z --hosts.",
			host, strings.Join(leaf.Hosts(), ", "))))
	}
	t := clientsetup.Target{Host: host, Port: port}

	fmt.Printf("%s %s\n\n", ui.Bold("OmnibusMCP: konfiguracja klienta dla"), ui.Cyan(t.MCPURL()))
	fmt.Print(clientsetup.Render(clientsetup.Steps(t, cfg.TokenFile), colorStyle))
	fmt.Printf("\n%s\n", ui.Dim("Ta sama instrukcja: curl -k "+t.BaseURL()+"/"))
	return nil
}

// colorStyle renders the steps for a terminal: headings stand out,
// explanations are dimmed and commands stay plain, so they copy cleanly.
func colorStyle(b clientsetup.Block) string {
	switch b.Kind {
	case clientsetup.Heading:
		return ui.Bold(ui.Cyan("## " + b.Text))
	case clientsetup.Subheading:
		return ui.Bold("### " + b.Text)
	case clientsetup.Text:
		return ui.Dim(b.Text)
	}
	return b.Text
}
