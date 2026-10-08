// Package install installs and removes OmnibusMCP as a systemd service.
package install

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
	"github.com/PNT-Data-Center/omnibusmcp/internal/ui"
)

// Installation paths.
const (
	BinPath  = "/usr/local/bin/omnibusmcp"
	UnitName = "omnibusmcp.service"
	UnitPath = "/etc/systemd/system/" + UnitName
	// The renewal timer runs "omnibusmcp tls renew" daily; without TLS it is a no-op.
	RenewName  = "omnibusmcp-tls-renew"
	RenewUnit  = "/etc/systemd/system/" + RenewName + ".service"
	RenewTimer = "/etc/systemd/system/" + RenewName + ".timer"
	ConfigDir  = "/etc/omnibusmcp"
	LogDir     = "/var/log/omnibusmcp"
)

// Options for Install.
type Options struct {
	Tier    int
	Modules []string
	Listen  string
	// Force overwrites an existing config file.
	Force bool
	Out   io.Writer
}

// Paths is a set of installation paths; tests override it.
type Paths struct {
	Bin, Unit, RenewUnit, RenewTimer, ConfigDir, LogDir string
}

// DefaultPaths are the production locations.
var DefaultPaths = Paths{Bin: BinPath, Unit: UnitPath, RenewUnit: RenewUnit, RenewTimer: RenewTimer, ConfigDir: ConfigDir, LogDir: LogDir}

// Install copies the binary, writes config, token and unit, then enables
// and (re)starts the service. It is idempotent.
func Install(o Options) error {
	if err := preflight(); err != nil {
		return err
	}
	p := DefaultPaths
	if err := Files(p, o); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", UnitName}, {"restart", UnitName}, {"enable", "--now", RenewName + ".timer"}} {
		if err := systemctl(args...); err != nil {
			return err
		}
	}
	fmt.Fprintf(o.Out, "\n%s (%s).\n", ui.Bold(ui.Green("OmnibusMCP installed and started")), UnitName)
	fmt.Fprintf(o.Out, "  %s%s\n  %s%s  %s\n  %s%s\n  %s%s\n  %s%s  %s\n",
		ui.Label("config", 9), filepath.Join(p.ConfigDir, "config.yaml"),
		ui.Label("token", 9), filepath.Join(p.ConfigDir, "token"), ui.Dim("(read with: sudo cat "+filepath.Join(p.ConfigDir, "token")+")"),
		ui.Label("audit", 9), filepath.Join(p.LogDir, "audit.log"),
		ui.Label("status", 9), ui.Cyan("omnibusmcp status"),
		ui.Label("tls", 9), ui.Cyan("omnibusmcp tls generate"), ui.Dim("(renewal: "+RenewName+".timer)"))
	return nil
}

// Files writes everything except systemd state changes. Options are
// validated before anything on disk is touched.
func Files(p Paths, o Options) error {
	cfgData, err := RenderConfig(o, p)
	if err != nil {
		return err
	}
	if err := copySelf(p.Bin); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("binary", 9), p.Bin, ui.Green("installed"))

	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(p.LogDir, 0o700); err != nil {
		return err
	}

	cfgPath := filepath.Join(p.ConfigDir, "config.yaml")
	switch _, statErr := os.Stat(cfgPath); {
	case statErr == nil && !o.Force:
		fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("config", 9), cfgPath, ui.Dim("exists, kept (use --force to overwrite; install flags were not applied)"))
	default:
		if err := writeAtomic(cfgPath, cfgData, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("config", 9), cfgPath, ui.Green("written"))
	}

	tokPath := filepath.Join(p.ConfigDir, "token")
	if _, err := os.Stat(tokPath); errors.Is(err, os.ErrNotExist) {
		tok, err := newToken()
		if err != nil {
			return err
		}
		if err := writeAtomic(tokPath, []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("token", 9), tokPath, ui.Green("generated"))
	} else {
		fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("token", 9), tokPath, ui.Dim("exists, kept"))
	}

	unit, err := RenderUnit(p)
	if err != nil {
		return err
	}
	if err := writeAtomic(p.Unit, unit, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("unit", 9), p.Unit, ui.Green("written"))

	renewSvc, renewTimer, err := RenderRenewUnits(p)
	if err != nil {
		return err
	}
	if err := writeAtomic(p.RenewUnit, renewSvc, 0o644); err != nil {
		return err
	}
	if err := writeAtomic(p.RenewTimer, renewTimer, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "%s%s %s\n", ui.Label("timer", 9), p.RenewTimer, ui.Green("written"))
	return nil
}

// Uninstall stops and removes the service and binary; purge also removes
// configuration, token and audit logs.
func Uninstall(purge bool, out io.Writer) error {
	if err := preflight(); err != nil {
		return err
	}
	p := DefaultPaths
	// The units may already be gone; stopping is best effort.
	_ = systemctl("disable", "--now", RenewName+".timer")
	_ = systemctl("disable", "--now", UnitName)
	for _, f := range []string{p.RenewTimer, p.RenewUnit, p.Unit, p.Bin} {
		if err := removeReport(out, f, os.Remove); err != nil {
			return err
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if purge {
		for _, d := range []string{p.ConfigDir, p.LogDir} {
			if err := removeReport(out, d, os.RemoveAll); err != nil {
				return err
			}
		}
	} else {
		fmt.Fprintf(out, "%s%s and %s %s\n", ui.Label("kept", 9), p.ConfigDir, p.LogDir, ui.Dim("(use --purge to remove)"))
	}
	fmt.Fprintln(out, ui.Bold(ui.Yellow("OmnibusMCP uninstalled.")))
	return nil
}

// removeReport removes path and reports whether it existed.
func removeReport(out io.Writer, path string, remove func(string) error) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "%s%s\n", ui.Dim(fmt.Sprintf("%-9s", "absent:")), ui.Dim(path))
		return nil
	}
	if err := remove(path); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s%s\n", ui.Yellow(fmt.Sprintf("%-9s", "removed:")), path)
	return nil
}

// ErrNotInstalled means the systemd service is not installed on this host.
var ErrNotInstalled = errors.New("omnibusmcp.service is not installed")

// RestartService restarts the installed service so it picks up a new
// configuration or certificate.
func RestartService() error {
	if _, err := os.Stat(UnitPath); err != nil {
		return ErrNotInstalled
	}
	return systemctl("restart", UnitName)
}

func preflight() error {
	if os.Geteuid() != 0 {
		return errors.New("must be run as root")
	}
	if st, err := os.Stat("/run/systemd/system"); err != nil || !st.IsDir() {
		return errors.New("systemd is not running on this host")
	}
	return nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func copySelf(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(dst); err == nil && real == self {
		return nil // already running the installed binary
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeAtomic(dst, data, 0o755)
}

// writeAtomic writes via a temp file and rename, so a running binary or a
// config being read is never seen half-written.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var configTmpl = template.Must(template.New("config").Parse(`# OmnibusMCP configuration. Changes take effect after:
#   systemctl restart omnibusmcp

# Bind address and port of the HTTP endpoint (host:port).
#   127.0.0.1:8765      loopback only; connect through an SSH tunnel (default, recommended)
#   [::1]:8765          IPv6 loopback
#   192.0.2.10:8765   one specific interface; requires tls below or allow_insecure_remote
#   0.0.0.0:8765        all IPv4 interfaces; same requirement
listen: {{.Listen}}

# Permission tier: 1 = read-only, 2 = read-only + service restarts.
tier: {{.Tier}}

# Modules: [auto] enables every detected module; or list them, e.g. [linux, containers].
# The base "linux" module is always enabled.
modules: [{{.Modules}}]

# File with the Bearer token clients must send (Authorization: Bearer <token>).
token_file: {{.TokenFile}}

# HTTPS. Empty = plain HTTP. "omnibusmcp tls generate" creates a self-signed
# certificate and fills these in; omnibusmcp-tls-renew.timer renews it.
tls:
  cert_file: ""
  key_file: ""

# Allow a non-loopback listener without TLS (not recommended).
allow_insecure_remote: false

# JSON-lines audit log of every tool call; "-" logs to the journal.
audit_log: {{.AuditLog}}

# Limits applied to every command run by a tool.
limits:
  command_timeout: 15s
  health_timeout: 30s
  max_output_bytes: 65536
`))

// RenderConfig produces the config file for o and validates it.
func RenderConfig(o Options, p Paths) ([]byte, error) {
	d := config.Default()
	if o.Listen != "" {
		d.Listen = o.Listen
	}
	if o.Tier != 0 {
		d.Tier = tier.Tier(o.Tier)
	}
	if len(o.Modules) > 0 {
		d.Modules = o.Modules
	}
	d.TokenFile = filepath.Join(p.ConfigDir, "token")
	d.AuditLog = filepath.Join(p.LogDir, "audit.log")
	if err := d.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err := configTmpl.Execute(&buf, map[string]any{
		"Listen": d.Listen, "Tier": int(d.Tier), "Modules": strings.Join(d.Modules, ", "),
		"TokenFile": d.TokenFile, "AuditLog": d.AuditLog,
	})
	return buf.Bytes(), err
}

var unitTmpl = template.Must(template.New("unit").Parse(`# Managed by "omnibusmcp install"; changes are overwritten on reinstall.
[Unit]
Description=OmnibusMCP diagnostic MCP server
Documentation=https://github.com/PNT-Data-Center/omnibusmcp
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.Bin}} serve --config {{.Config}}
Restart=on-failure
RestartSec=5s
# Exit code 78 = configuration error: restarting would not help.
RestartPreventExitStatus=78
User=root

# Hardening (ADR-021). Diagnostics need root, so limit what root can do here.
# Capabilities: read any file (DAC_READ_SEARCH), see other processes' sockets
# and fds for ss -p (SYS_PTRACE), bind a port below 1024 if configured.
CapabilityBoundingSet=CAP_DAC_READ_SEARCH CAP_SYS_PTRACE CAP_NET_BIND_SERVICE
AmbientCapabilities=
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths={{.LogDir}}
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
# Return EPERM instead of killing, so a blocked call shows up as a readable
# error in the tool output rather than a dead command.
SystemCallErrorNumber=EPERM
UMask=0077
MemoryMax=512M
TasksMax=256
LimitNOFILE=4096

[Install]
WantedBy=multi-user.target
`))

var renewServiceTmpl = template.Must(template.New("renew").Parse(`# Managed by "omnibusmcp install"; changes are overwritten on reinstall.
[Unit]
Description=OmnibusMCP TLS certificate renewal
Documentation=https://github.com/PNT-Data-Center/omnibusmcp
After=omnibusmcp.service

[Service]
Type=oneshot
# Renews only when the certificate expires within 30 days, then restarts
# omnibusmcp.service. Without TLS configured it does nothing.
ExecStart={{.Bin}} tls renew --config {{.Config}} --before 30d
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths={{.ConfigDir}} {{.LogDir}}
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
UMask=0077
`))

var renewTimerTmpl = template.Must(template.New("timer").Parse(`# Managed by "omnibusmcp install"; changes are overwritten on reinstall.
[Unit]
Description=Daily OmnibusMCP TLS certificate renewal check

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
`))

// RenderRenewUnits produces the renewal service and timer.
func RenderRenewUnits(p Paths) (service, timer []byte, err error) {
	var s, t bytes.Buffer
	data := map[string]string{"Bin": p.Bin, "Config": filepath.Join(p.ConfigDir, "config.yaml"), "ConfigDir": p.ConfigDir, "LogDir": p.LogDir}
	if err := renewServiceTmpl.Execute(&s, data); err != nil {
		return nil, nil, err
	}
	if err := renewTimerTmpl.Execute(&t, data); err != nil {
		return nil, nil, err
	}
	return s.Bytes(), t.Bytes(), nil
}

// RenderUnit produces the systemd unit.
func RenderUnit(p Paths) ([]byte, error) {
	var buf bytes.Buffer
	err := unitTmpl.Execute(&buf, map[string]string{
		"Bin": p.Bin, "Config": filepath.Join(p.ConfigDir, "config.yaml"), "LogDir": p.LogDir,
	})
	return buf.Bytes(), err
}
