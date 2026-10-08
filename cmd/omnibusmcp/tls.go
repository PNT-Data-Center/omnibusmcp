package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/audit"
	"github.com/PNT-Data-Center/omnibusmcp/internal/certs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/install"
	"github.com/PNT-Data-Center/omnibusmcp/internal/ui"
)

const tlsUsage = `Usage:
  omnibusmcp tls generate [--config PATH] [--days 243] [--hosts h1,h2] [--force]
      create a single-use local CA (its key is never stored) and a server
      certificate signed by it (default 5 years), enable TLS in the config,
      restart the service; clients trust the CA once
  omnibusmcp tls renew    [--config PATH] [--before 30d] [--days 243] [--force]
      replace the certificate (and its CA: clients trust the new one) if it
      expires within --before (or always with --force); certificates from
      another CA are never replaced
  omnibusmcp tls status   [--config PATH]
      show validity, hosts and fingerprints of the certificate and the CA
  omnibusmcp tls client-setup [--config PATH] [--host NAME]
      print commands that make a client trust this server (CA download with
      fingerprint check) and register it in Claude Code
  omnibusmcp tls disable  [--config PATH] [--allow-insecure-remote]
      remove TLS from the config and restart the service (plain HTTP)
`

func runTLS(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, tlsUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "generate":
		return tlsGenerate(args[1:])
	case "renew":
		return tlsRenew(args[1:])
	case "status":
		return tlsStatus(args[1:])
	case "client-setup":
		return tlsClientSetup(args[1:])
	case "disable":
		return tlsDisable(args[1:])
	case "help", "-h", "--help":
		fmt.Print(tlsUsage)
		return nil
	}
	fmt.Fprintf(os.Stderr, "unknown tls command %q\n\n%s", args[0], tlsUsage)
	os.Exit(2)
	return nil
}

// loadInstalledConfig loads a config that must exist: TLS commands edit it.
func loadInstalledConfig(path string) (*config.Config, error) {
	cfg, found, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s not found; run 'omnibusmcp install' first", path)
	}
	return cfg, nil
}

// certPaths returns the configured files, or defaults next to the config,
// with the local CA in the same directory.
func certPaths(cfg *config.Config, cfgPath string) certs.Paths {
	t := cfg.TLS
	if !t.Enabled() {
		dir := filepath.Join(filepath.Dir(cfgPath), "tls")
		t = config.TLS{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
	}
	return certs.Paths{Cert: t.CertFile, Key: t.KeyFile, CA: t.CAFile()}
}

func tlsGenerate(args []string) error {
	fs := flag.NewFlagSet("tls generate", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	days := fs.Int("days", certs.DefaultDays, "validity in days (default 5 years)")
	hostsFlag := fs.String("hosts", "", "comma-separated DNS names and IPs for the certificate (default: detected)")
	force := fs.Bool("force", false, "replace an existing certificate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadInstalledConfig(*cfgPath)
	if err != nil {
		return err
	}
	p := certPaths(cfg, *cfgPath)
	if _, err := os.Stat(p.Cert); err == nil && !*force {
		return fmt.Errorf("%s already exists; use 'tls renew' or --force to replace it", p.Cert)
	}
	hosts := certs.DetectHosts(cfg.Listen)
	if *hostsFlag != "" {
		if hosts, err = certs.ParseHosts(*hostsFlag); err != nil {
			return err
		}
	}
	info, err := certs.Generate(p, hosts, *days, time.Now())
	if err != nil {
		return err
	}
	if err := config.Update(*cfgPath, map[string]any{"tls.cert_file": p.Cert, "tls.key_file": p.Key}); err != nil {
		return err
	}
	auditTLS(cfg, "tls_generate", info)
	fmt.Print(ui.Label("certificate", 13) + p.Cert + "\n" + ui.Label("key", 13) + p.Key + "\n" + certText(info))
	if err := restartAfterTLS(); err != nil {
		return err
	}
	fmt.Println(ui.Yellow("New local CA: every client must trust it once (commands below)."))
	cfg.TLS = config.TLS{CertFile: p.Cert, KeyFile: p.Key}
	return printClientSetup(cfg, "")
}

func tlsRenew(args []string) error {
	fs := flag.NewFlagSet("tls renew", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	beforeFlag := fs.String("before", "30d", "renew when the certificate expires within this time (e.g. 30d, 720h)")
	days := fs.Int("days", certs.DefaultDays, "validity of the new certificate in days")
	force := fs.Bool("force", false, "renew regardless of the remaining validity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	before, err := certs.ParseDays(*beforeFlag)
	if err != nil {
		return err
	}
	cfg, err := loadInstalledConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.TLS.Enabled() {
		fmt.Println(ui.Dim("TLS is not configured; nothing to renew."))
		return nil
	}
	p := certPaths(cfg, *cfgPath)
	now := time.Now()
	kind, _, classifyErr := certs.Classify(p)
	old, err := certs.Load(p.Cert)
	switch {
	case err != nil || classifyErr != nil:
		fmt.Println(ui.Yellow(fmt.Sprintf("current certificate unreadable (%v); generating a new CA and certificate for detected hosts", errors.Join(err, classifyErr))))
	case kind == certs.External:
		fmt.Println(ui.Dim(fmt.Sprintf("certificate issued by %q, not by OmnibusMCP: not renewed here (valid until %s, %d days left).",
			old.Issuer, old.NotAfter.Format("2006-01-02"), old.DaysLeft(now))))
		return nil
	case !*force && old.Remaining(now) > before:
		fmt.Println(ui.Paint(daysLevel(old.DaysLeft(now)), fmt.Sprintf("certificate valid until %s (%d days left)", old.NotAfter.Format("2006-01-02"), old.DaysLeft(now))) +
			ui.Dim(fmt.Sprintf(", renewal threshold %s; nothing to do.", *beforeFlag)))
		return nil
	}

	if old != nil && kind == certs.Legacy {
		fmt.Println(ui.Yellow("legacy self-signed certificate: switching to a single-use local CA (same host names)"))
	}
	hosts := certs.DetectHosts(cfg.Listen)
	if old != nil {
		hosts = old.Hosts() // keep the SANs clients already use
	}
	info, err := certs.Generate(p, hosts, *days, now)
	if err != nil {
		return err
	}
	auditTLS(cfg, "tls_renew", info)
	fmt.Print(ui.Green("certificate renewed: ") + p.Cert + "\n" + certText(info))
	if old != nil {
		fmt.Println(ui.Dim("previous sha256: " + old.Fingerprint))
	}
	if err := restartAfterTLS(); err != nil {
		return err
	}
	fmt.Println(ui.Yellow("New local CA: every client must trust it again; see 'omnibusmcp tls client-setup'."))
	return nil
}

func tlsStatus(args []string) error {
	fs := flag.NewFlagSet("tls status", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadInstalledConfig(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.TLS.Enabled() {
		lvl := ui.Info
		if !cfg.ListenIsLoopback() {
			lvl = ui.Warn
		}
		fmt.Println(ui.Paint(lvl, "TLS disabled: plain HTTP on "+cfg.Listen))
		return nil
	}
	p := certPaths(cfg, *cfgPath)
	info, err := certs.Load(p.Cert)
	if err != nil {
		return err
	}
	fmt.Print(ui.Green("TLS enabled") + " on " + ui.Cyan(cfg.Listen) + "\n" + ui.Label("certificate", 13) + p.Cert + "\n" + ui.Label("key", 13) + p.Key + "\n" + certText(info))
	switch kind, _, _ := certs.Classify(p); kind {
	case certs.Managed:
		if ca, err := certs.LoadCA(p); err == nil {
			fmt.Print("\n" + ui.Label("local CA", 13) + p.CA + "\n" + certText(ca))
		}
	case certs.Legacy:
		fmt.Println(ui.Yellow("\nLegacy self-signed certificate: some clients (native Claude Code) reject it.") +
			ui.Dim(" Switch to a local CA: omnibusmcp tls generate --force"))
	case certs.External:
		fmt.Println(ui.Dim("\nIssued by " + info.Issuer + ": renewed outside OmnibusMCP."))
	}
	if info.Remaining(time.Now()) <= 0 {
		return errors.New("certificate has EXPIRED; run 'omnibusmcp tls renew --force'")
	}
	return nil
}

func tlsDisable(args []string) error {
	fs := flag.NewFlagSet("tls disable", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	insecure := fs.Bool("allow-insecure-remote", false, "also set allow_insecure_remote: true (needed for a non-loopback listen address)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadInstalledConfig(*cfgPath)
	if err != nil {
		return err
	}
	set := map[string]any{"tls.cert_file": "", "tls.key_file": ""}
	if *insecure {
		set["allow_insecure_remote"] = true
	}
	if err := config.Update(*cfgPath, set); err != nil {
		if !cfg.ListenIsLoopback() && !*insecure {
			return fmt.Errorf("%w\nlisten %s is not loopback: add --allow-insecure-remote to serve plain HTTP on it", err, cfg.Listen)
		}
		return err
	}
	if a, err := audit.Open(cfg.AuditLog); err == nil {
		a.Event("tls_disable", "listen", cfg.Listen, "allow_insecure_remote", *insecure || cfg.AllowInsecureRemote)
		a.Close()
	}
	fmt.Println(ui.Yellow("TLS disabled") + " in " + *cfgPath + ui.Dim(" (certificate files kept in place)."))
	return restartAfterTLS()
}

func auditTLS(cfg *config.Config, event string, info *certs.Info) {
	a, err := audit.Open(cfg.AuditLog)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: audit log:", err)
		return
	}
	defer a.Close()
	a.Event(event, "not_after", info.NotAfter.Format(time.RFC3339), "hosts", info.Hosts(), "sha256", info.Fingerprint)
}

func restartAfterTLS() error {
	err := install.RestartService()
	switch {
	case errors.Is(err, install.ErrNotInstalled):
		fmt.Println(ui.Yellow("Service not installed; start 'omnibusmcp serve' to use the new settings."))
		return nil
	case err != nil:
		return fmt.Errorf("restart failed (check 'journalctl -u omnibusmcp'): %w", err)
	}
	fmt.Println(ui.Green("omnibusmcp.service restarted."))
	return nil
}

// certText renders certificate details with the validity colored.
func certText(i *certs.Info) string {
	days := i.DaysLeft(time.Now())
	return ui.Label("subject", 13) + i.Subject + "\n" +
		ui.Label("valid from", 13) + i.NotBefore.Format(time.RFC3339) + "\n" +
		ui.Label("valid until", 13) + ui.Paint(daysLevel(days), fmt.Sprintf("%s (%d days left)", i.NotAfter.Format(time.RFC3339), days)) + "\n" +
		hostsLine(i) +
		ui.Label("sha256", 13) + ui.Dim(i.Fingerprint) + "\n"
}

// hostsLine lists the SANs, or the issuer for a CA (which has none).
func hostsLine(i *certs.Info) string {
	if len(i.Hosts()) == 0 {
		return ""
	}
	s := ui.Label("hosts (SAN)", 13) + strings.Join(i.Hosts(), ", ") + "\n"
	if i.Issuer != "" && i.Issuer != i.Subject {
		s += ui.Label("issued by", 13) + i.Issuer + "\n"
	}
	return s
}
