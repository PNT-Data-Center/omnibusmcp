// Command omnibusmcp is a diagnostic MCP server for Linux hosts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/audit"
	"github.com/PNT-Data-Center/omnibusmcp/internal/certs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/executor"
	"github.com/PNT-Data-Center/omnibusmcp/internal/install"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/server"
	"github.com/PNT-Data-Center/omnibusmcp/internal/ui"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0". When
// empty, it comes from the Go build info (see buildVersion).
var version = ""

// author is shown by "omnibusmcp version" and in the usage text.
const author = "Kamil Kobak"

// exitConfig (EX_CONFIG from sysexits.h) marks errors a restart cannot fix;
// the systemd unit uses RestartPreventExitStatus to stop restarting on it.
const exitConfig = 78

// configError wraps errors caused by configuration, token or audit setup.
type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

const usage = `OmnibusMCP - diagnostic MCP server for Linux hosts
Author: ` + author + `

Usage:
  omnibusmcp serve     [--config PATH]      run the MCP server
  omnibusmcp install   [--tier N] [--modules auto|m1,m2] [--listen ADDR] [--force] [--allow-insecure-remote]
                                            install and start the systemd service
                                            (a non-loopback --listen gets a TLS certificate)
  omnibusmcp uninstall [--purge]            stop and remove the service
  omnibusmcp status    [--config PATH]      service state and MCP endpoint (exit 0 running, 3 stopped, 4 not installed)
  omnibusmcp detect    [--config PATH]      show detected and enabled modules
  omnibusmcp tools     [--config PATH]      list tools exposed at the configured tier
  omnibusmcp tls       generate|renew|status|disable
                                            manage the self-signed HTTPS certificate
  omnibusmcp upgrade   [--check] [--version vX.Y.Z] [--force] [--yes]
                                            download a release from GitHub and replace the binary
  omnibusmcp version
`

// buildVersion derives a version from the build info: the module version
// for "go install ...@v0.1.0", otherwise "dev+<commit>[-dirty]".
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return "dev+" + rev
}

func main() {
	if version == "" {
		version = buildVersion()
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "install":
		err = runInstall(args)
	case "uninstall":
		err = runUninstall(args)
	case "status":
		os.Exit(runStatus(args))
	case "detect":
		err = runDetect(args)
	case "tools":
		err = runTools(args)
	case "tls":
		err = runTLS(args)
	case "upgrade":
		err = runUpgrade(args)
	case "version", "--version", "-v":
		fmt.Println(ui.Bold("omnibusmcp"), ui.Cyan(version))
		fmt.Println(ui.Dim("Author: " + author))
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		prefix := "error:"
		if ui.StderrEnabled() {
			prefix = "\033[1;31merror:\033[0m"
		}
		fmt.Fprintln(os.Stderr, prefix, err)
		var ce configError
		if errors.As(err, &ce) {
			os.Exit(exitConfig)
		}
		os.Exit(1)
	}
}

// setup loads config and resolves modules, shared by serve/detect/tools.
func setup(ctx context.Context, fs *flag.FlagSet, args []string) (*registry.Env, []modules.Detection, bool, error) {
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, false, err
	}
	cfg, found, err := config.Load(*cfgPath)
	if err != nil {
		return nil, nil, false, err
	}
	env := &registry.Env{Cfg: cfg, Exec: executor.New(cfg.Limits.CommandTimeout, cfg.Limits.MaxOutputBytes)}
	ds, err := modules.Resolve(ctx, env, cfg)
	return env, ds, found, err
}

func runServe(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env, ds, found, err := setup(ctx, flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return configError{err}
	}
	if !found {
		log.Warn("config file not found, using defaults")
	}
	cfg := env.Cfg
	token, err := server.ReadToken(cfg.TokenFile)
	if err != nil {
		return configError{err}
	}
	aud, err := audit.Open(cfg.AuditLog)
	if err != nil {
		return configError{fmt.Errorf("audit log: %w", err)}
	}
	defer aud.Close()

	mods := modules.Enabled(ds)
	opts := server.Options{Env: env, Modules: mods, Audit: aud, Version: version}
	s, err := server.NewMCPServer(opts)
	if err != nil {
		return configError{err}
	}
	tools, _ := server.Tools(opts)
	names := moduleNames(mods)
	aud.Event("server_start", "version", version, "listen", cfg.Listen, "tier", int(cfg.Tier), "modules", names, "tools", len(tools))
	log.Info("OmnibusMCP started", "version", version, "listen", cfg.Listen, "tls", cfg.TLS.Enabled(),
		"tier", cfg.Tier.String(), "modules", strings.Join(names, ","), "tools", len(tools))

	tlsConf, err := server.LoadTLS(cfg)
	if err != nil {
		return configError{err}
	}
	var pub *server.Public
	if cfg.TLS.Enabled() {
		pub = &server.Public{CAFile: cfg.TLS.CAFile(), TokenFile: cfg.TokenFile, Listen: cfg.Listen}
	}
	err = server.Serve(ctx, cfg, tlsConf, server.Handler(s, token, aud, pub))
	aud.Event("server_stop", "error", fmt.Sprint(err))
	return err
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	tierN := fs.Int("tier", 1, "permission tier (1 = read-only, 2 = + service restarts)")
	mods := fs.String("modules", config.ModulesAuto, "comma-separated modules or auto")
	listen := fs.String("listen", config.DefaultListen, "listen address; a non-loopback address gets a TLS certificate")
	force := fs.Bool("force", false, "overwrite an existing config file")
	insecure := fs.Bool("allow-insecure-remote", false, "serve a non-loopback address over plain HTTP (no certificate)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var list []string
	for _, m := range strings.Split(*mods, ",") {
		if m = strings.TrimSpace(m); m != "" {
			list = append(list, m)
		}
	}
	for _, m := range list {
		if m != config.ModulesAuto && !contains(modules.Names(), m) {
			return fmt.Errorf("unknown module %q (available: %s)", m, strings.Join(modules.Names(), ", "))
		}
	}

	p := install.DefaultPaths
	cfgPath := install.ConfigPath(p)
	if _, err := os.Stat(cfgPath); err == nil && !*force {
		// An existing config is kept; flags meant for it would be silently lost.
		var set []string
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "force" {
				set = append(set, "--"+f.Name)
			}
		})
		if len(set) > 0 {
			return fmt.Errorf("%s already exists, so %s would not be applied; add --force to rewrite it (the token is kept)", cfgPath, strings.Join(set, " "))
		}
	}

	o := install.Options{Tier: *tierN, Modules: list, Listen: *listen, Force: *force, AllowInsecureRemote: *insecure, Out: os.Stdout}
	probe := config.Default()
	probe.Listen = *listen
	o.TLS = !probe.ListenIsLoopback() && !*insecure
	var tlsCfg *config.Config
	if o.TLS {
		// A network listener serves HTTPS from the start: the certificate is
		// created (or an existing one reused) before the service starts.
		o.BeforeStart = func() error {
			cfg, err := loadInstalledConfig(cfgPath)
			if err != nil {
				return err
			}
			tlsCfg = cfg
			cp := server.TLSPaths(cfg)
			if _, err := os.Stat(cp.Cert); err == nil {
				fmt.Printf("%s%s %s\n", ui.Label("tls", 9), cp.Cert, ui.Dim("exists, kept"))
				return nil
			}
			info, err := certs.Generate(cp, certs.DetectHosts(cfg.Listen), certs.DefaultDays, time.Now())
			if err != nil {
				return fmt.Errorf("tls certificate: %w", err)
			}
			auditTLS(cfg, "tls_generate", info)
			fmt.Printf("%s%s %s %s\n", ui.Label("tls", 9), cp.Cert, ui.Green("generated"), ui.Dim("(hosts: "+strings.Join(info.Hosts(), ", ")+")"))
			return nil
		}
	}
	if err := install.Install(o); err != nil {
		return err
	}
	if tlsCfg != nil {
		fmt.Println()
		return printClientSetup(tlsCfg, "")
	}
	return nil
}

func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also remove configuration, token and audit logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return install.Uninstall(*purge, os.Stdout)
}

func runDetect(args []string) error {
	ctx := context.Background()
	env, ds, _, err := setup(ctx, flag.NewFlagSet("detect", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	rows := [][]string{{"MODULE", "DETECTED", "ENABLED", "VERSION", "DESCRIPTION"}}
	var notes []string
	for _, d := range ds {
		name := d.Module.Name()
		if d.Enabled {
			name = ui.Cyan(name)
		}
		ver := ui.Dim("-")
		if v, ok := d.Module.(registry.Versioned); ok && d.Detected {
			r := v.Version(ctx, env)
			switch {
			case r.Version == "":
				ver = ui.Dim("unknown")
			case r.Tested:
				ver = ui.Green(compat.Name(r.Product) + " " + r.Version)
			default:
				ver = compat.Name(r.Product) + " " + r.Version + ui.Dim(" *")
				notes = append(notes, fmt.Sprintf("* %s %s: %s", compat.Name(r.Product), r.Version, r.Note))
			}
		}
		rows = append(rows, []string{name, yesNoColor(d.Detected, ui.Good), yesNoColor(d.Enabled, ui.Good), ver, ui.Dim(d.Module.Description())})
	}
	fmt.Print(ui.Table(rows))
	for _, n := range notes {
		fmt.Println(ui.Dim(n))
	}
	return nil
}

func runTools(args []string) error {
	env, ds, _, err := setup(context.Background(), flag.NewFlagSet("tools", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	tools, err := server.Tools(server.Options{Env: env, Modules: modules.Enabled(ds)})
	if err != nil {
		return err
	}
	fmt.Printf("%s %s, %s tools\n\n", ui.Bold("tier"), env.Cfg.Tier, ui.Bold(strconv.Itoa(len(tools))))
	rows := [][]string{{"TOOL", "MODULE", "MIN TIER", "READ-ONLY"}}
	for _, t := range tools {
		mod := t.Module
		if mod == "" {
			mod = "server"
		}
		// A tool that can change state is highlighted, read-only is the norm.
		rows = append(rows, []string{t.Name, ui.Cyan(mod), strconv.Itoa(int(t.MinTier)), yesNoColor(t.ReadOnly, ui.Good, ui.Warn)})
	}
	fmt.Print(ui.Table(rows))
	return nil
}

// yesNoColor renders yes in the given level, no dimmed (or in noLevel).
func yesNoColor(b bool, yes ui.Level, noLevel ...ui.Level) string {
	if b {
		return ui.Paint(yes, "yes")
	}
	if len(noLevel) > 0 {
		return ui.Paint(noLevel[0], "no")
	}
	return ui.Dim("no")
}

func moduleNames(ms []registry.Module) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name()
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
