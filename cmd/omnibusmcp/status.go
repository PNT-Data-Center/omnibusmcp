package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/certs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/install"
	"github.com/PNT-Data-Center/omnibusmcp/internal/server"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
	"github.com/PNT-Data-Center/omnibusmcp/internal/ui"
)

// Exit codes of "omnibusmcp status" follow the LSB init script convention.
const (
	statusRunning      = 0
	statusNotRunning   = 3
	statusNotInstalled = 4
)

// runStatus prints service state and endpoint and returns the exit code.
func runStatus(args []string) int {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := flags.String("config", config.DefaultPath, "configuration file")
	if err := flags.Parse(args); err != nil {
		return statusNotInstalled
	}

	fmt.Printf("%s %s\n\n", ui.Bold("OmnibusMCP"), ui.Cyan(version))
	svc, svcErr := install.QueryUnit(install.UnitName)
	code := statusNotRunning

	// Service
	switch {
	case svcErr != nil:
		line("Service", ui.Yellow(fmt.Sprintf("unknown (systemctl: %v)", svcErr)))
	case !svc.Installed:
		line("Service", ui.Red(install.UnitName+" NOT installed")+ui.Dim(" (install with: omnibusmcp install)"))
		code = statusNotInstalled
	default:
		enabled := firstNonEmpty(svc.UnitFileState, "?")
		line("Service", install.UnitName+" installed, "+ui.Paint(map[bool]ui.Level{true: ui.Good, false: ui.Warn}[enabled == "enabled"], enabled))
		state := svc.ActiveState
		if svc.SubState != "" {
			state += " (" + svc.SubState + ")"
		}
		lvl := ui.Warn
		switch svc.ActiveState {
		case "active":
			lvl = ui.Good
		case "failed":
			lvl = ui.Bad
		}
		since := ""
		if svc.Since != "" && svc.Since != "n/a" {
			since = ui.Dim(" since " + svc.Since)
		}
		line("State", ui.Paint(lvl, state)+since)
		if svc.Running() {
			code = statusRunning
			line("PID", strconv.Itoa(svc.MainPID))
			if rv := install.RunningVersion(); rv != "" {
				if rv == version {
					line("Running", "version "+ui.Green(rv))
				} else {
					line("Running", "version "+ui.Yellow(rv)+ui.Yellow(fmt.Sprintf("  (binary on disk: %s; restart to update: systemctl restart %s)", version, install.UnitName)))
				}
			}
		} else if svc.ActiveState == "failed" || svc.Result != "success" && svc.Result != "" {
			exit := fmt.Sprintf("status %d, result %s", svc.ExitStatus, svc.Result)
			if svc.ExitStatus == exitConfig {
				exit += " (configuration error, see: journalctl -u " + install.UnitName + " -n 20)"
			}
			line("Last exit", ui.Red(exit))
		}
	}

	line("Update", updateStatus())

	// Configuration and endpoint
	cfg, found, err := config.Load(*cfgPath)
	switch {
	case errors.Is(err, os.ErrPermission):
		fmt.Println()
		line("Config", *cfgPath+ui.Yellow(" not readable (run as root to see the endpoint)"))
		return code
	case err != nil:
		fmt.Println()
		line("Config", *cfgPath+" "+ui.Red("INVALID: "+err.Error()))
		return code
	}
	ep := install.ResolveEndpoint(cfg.Listen, cfg.TLS.Enabled())
	fmt.Println()
	if code == statusRunning {
		line("Endpoint", ui.Bold(ui.Cyan(ep.URL)))
		if ep.Listening {
			line("Listening", ui.Green("yes")+ui.Dim(" ("+ep.Probe+" accepts connections)"))
		} else {
			line("Listening", ui.Red("NO: service is active but "+ep.Probe+" refuses connections (still starting or failed to bind)"))
		}
	} else {
		line("Endpoint", ui.Cyan(ep.URL)+ui.Dim("  (after start)"))
		if ep.Listening {
			line("Warning", ui.Yellow(ep.Probe+" is already in use by another process"))
		}
	}
	cfgNote := ""
	if !found {
		cfgNote = ui.Yellow(" (not found, defaults shown)")
	}
	line("Config", *cfgPath+cfgNote)
	line("Tier", ui.Paint(map[bool]ui.Level{true: ui.Good, false: ui.Warn}[cfg.Tier == tier.ReadOnly], cfg.Tier.String()))
	line("Modules", strings.Join(cfg.Modules, ", ")+ui.Dim("  (enabled at start; check with: omnibusmcp detect)"))
	if cfg.TLS.Enabled() {
		if info, err := certs.Load(cfg.TLS.CertFile); err != nil {
			line("TLS", ui.Green("enabled")+", "+ui.Red("certificate unreadable: "+err.Error()))
		} else {
			days := info.DaysLeft(time.Now())
			line("TLS", ui.Green("enabled")+", certificate valid until "+ui.Paint(daysLevel(days), fmt.Sprintf("%s (%d days)", info.NotAfter.Format("2006-01-02"), days)))
			switch kind, _, _ := certs.Classify(server.TLSPaths(cfg)); kind {
			case certs.Managed:
				line("", ui.Dim("signed by the local CA; client setup: omnibusmcp tls client-setup"))
			case certs.Legacy:
				line("", ui.Yellow("legacy self-signed certificate, rejected by native Claude Code: omnibusmcp tls generate --force"))
			}
		}
	} else {
		lvl := ui.Info
		if !cfg.ListenIsLoopback() {
			lvl = ui.Warn // token travels unencrypted over the network
		}
		line("TLS", ui.Paint(lvl, "disabled (plain HTTP)"))
	}
	if t, err := install.QueryUnit(install.RenewName + ".timer"); err == nil && t.Installed {
		next := ""
		if t.NextElapse != "" && t.NextElapse != "n/a" {
			next = ui.Dim(", next run " + t.NextElapse)
		}
		line("TLS renewal", install.RenewName+".timer "+ui.Paint(map[bool]ui.Level{true: ui.Good, false: ui.Warn}[t.ActiveState == "active"], t.ActiveState)+next)
	}
	return code
}

// line prints one "Label:  value" row of the status report.
func line(label, value string) {
	if label == "" {
		fmt.Println(strings.Repeat(" ", 13) + value) // continuation of the row above
		return
	}
	fmt.Println(ui.Label(label, 13) + value)
}

// daysLevel colors certificate validity: expired, under 30 days, fine.
func daysLevel(days int) ui.Level {
	switch {
	case days <= 0:
		return ui.Bad
	case days < 30:
		return ui.Warn
	}
	return ui.Good
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
