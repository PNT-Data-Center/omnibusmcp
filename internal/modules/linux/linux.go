// Package linux is the base module, enabled on every host: systemd, journal,
// disks, network, load, memory and host identity.
package linux

import (
	"context"
	"fmt"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Name is the module identifier used in configuration.
const Name = "linux"

const (
	maxJournalLines = 2000
	maxDirEntries   = 500
)

// Module implements registry.Module.
type Module struct{}

// New returns the base Linux module.
func New() *Module { return &Module{} }

func (*Module) Name() string { return Name }

func (*Module) Description() string {
	return "base Linux host: systemd, journal, disks, network, load, memory, scheduled jobs, host identity"
}

// Detect always succeeds: every supported host is a Linux host.
func (*Module) Detect(context.Context, *registry.Env) bool { return true }

type noInput struct{}

type resourcesInput struct {
	Top int `json:"top,omitempty" jsonschema:"number of top processes by CPU and by memory (default 10, max 50)"`
}

type unitInput struct {
	Unit  string `json:"unit" jsonschema:"systemd unit name, e.g. ssh.service or docker"`
	Lines int    `json:"lines,omitempty" jsonschema:"number of recent log lines to include (default 20, max 200)"`
}

type servicesInput struct {
	State string `json:"state,omitempty" jsonschema:"filter by state: running, failed, active, inactive, exited or all (default all loaded)"`
}

type journalInput struct {
	Unit     string `json:"unit,omitempty" jsonschema:"only entries of this systemd unit"`
	Since    string `json:"since,omitempty" jsonschema:"start time: relative (30m, 2h, 1d, 1w), today, yesterday or absolute (2026-09-28 10:00)"`
	Until    string `json:"until,omitempty" jsonschema:"end time, same formats as since"`
	Priority string `json:"priority,omitempty" jsonschema:"maximum priority: emerg, alert, crit, err, warning, notice, info, debug or 0-7; err shows errors and worse"`
	Grep     string `json:"grep,omitempty" jsonschema:"regular expression the message must match (case-insensitive if lowercase)"`
	Lines    int    `json:"lines,omitempty" jsonschema:"maximum number of most recent entries (default 100, max 2000)"`
	Kernel   bool   `json:"kernel,omitempty" jsonschema:"only kernel messages (like dmesg)"`
	Boot     *int   `json:"boot,omitempty" jsonschema:"boot offset: 0 current boot, -1 previous boot (useful after a crash), down to -20"`
}

type fileInput struct {
	Path string `json:"path" jsonschema:"absolute path under /etc, /var/log, /proc, /sys, /run, /usr/lib, /usr/share, /lib or /opt"`
	Tail bool   `json:"tail,omitempty" jsonschema:"return the end of the file instead of the beginning (for logs)"`
}

type dirInput struct {
	Path string `json:"path" jsonschema:"absolute directory path under the same roots as linux_read_file"`
}

var serviceStates = map[string]bool{"running": true, "failed": true, "active": true, "inactive": true, "exited": true, "all": true}

// Tools returns the module's tools; all of them are read-only (tier 1).
func (m *Module) Tools(env *registry.Env) []registry.Tool {
	ro := tier.ReadOnly
	files := newFilePolicy(env.Cfg)
	return []registry.Tool{
		registry.NewTool("linux_host_overview", "Host overview",
			"Host identity and state: hostname, OS, kernel, virtualization, uptime, time synchronisation.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				return runAll(ctx, env,
					[]string{"hostnamectl"},
					[]string{"uptime"},
					[]string{"timedatectl"},
				), nil
			}),

		registry.NewTool("linux_resources", "CPU, memory and top processes",
			"Load average, CPU count, memory and swap usage, and the top processes by CPU and by memory.",
			ro, true, func(ctx context.Context, in resourcesInput) (string, error) {
				top := clamp(in.Top, 10, 50)
				psCols := "pid,user,pcpu,pmem,rss,etime,stat,comm"
				var b strings.Builder
				b.WriteString(runAll(ctx, env, []string{"cat", "/proc/loadavg"}, []string{"nproc"}, []string{"free", "-m"}))
				for _, sort := range []string{"-pcpu", "-rss"} {
					r := env.Exec.Run(ctx, "ps", "-eo", psCols, "--sort="+sort)
					r.Stdout = headLines(r.Stdout, top+1)
					b.WriteString("\n" + r.Format())
				}
				return b.String(), nil
			}),

		registry.NewTool("linux_disks", "Disks and filesystems",
			"Filesystem space and inode usage, block devices, and the host's real mounts with their options (look for ro on filesystems that should be rw).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				return runAll(ctx, env,
					append([]string{"df", "-hT"}, dfExcludes()...),
					append([]string{"df", "-ih"}, dfExcludes()...),
					// The service runs in its own mount namespace (systemd sandbox), so
					// mount points are read from PID 1 to show the host's view.
					[]string{"lsblk", "-o", "NAME,SIZE,TYPE,FSTYPE,MODEL,ROTA"},
					[]string{"findmnt", "--task", "1", "--real", "--list", "-o", "TARGET,SOURCE,FSTYPE,OPTIONS"},
				), nil
			}),

		registry.NewTool("linux_network", "Network",
			"Interfaces and addresses, routes, listening sockets with owning processes, DNS resolver configuration.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				out := runAll(ctx, env,
					[]string{"ip", "-brief", "address"},
					[]string{"ip", "route"},
					[]string{"ss", "-tulpn"},
				)
				resolv, err := files.readFile("/etc/resolv.conf", env.Cfg.Limits.MaxOutputBytes, false)
				if err != nil {
					resolv = "[/etc/resolv.conf: " + err.Error() + "]"
				}
				return out + "\n" + resolv, nil
			}),

		registry.NewTool("linux_systemd_overview", "systemd overview",
			"Overall systemd state (running/degraded) and the list of failed units.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				return runAll(ctx, env,
					[]string{"systemctl", "is-system-running"},
					[]string{"systemctl", "list-units", "--failed", "--no-pager", "--plain"},
				), nil
			}),

		registry.NewTool("linux_service_status", "Service status",
			"Detailed status of one systemd unit including its most recent log lines.",
			ro, true, func(ctx context.Context, in unitInput) (string, error) {
				if err := validUnit(in.Unit); err != nil {
					return "", err
				}
				lines := clamp(in.Lines, 20, 200)
				return env.Exec.RunTail(ctx, "systemctl", "status", "--no-pager", "--full", "--lines="+itoa(lines), "--", in.Unit).Format(), nil
			}),

		registry.NewTool("linux_list_services", "List services",
			"List systemd service units with their load, active and sub state.",
			ro, true, func(ctx context.Context, in servicesInput) (string, error) {
				args := []string{"systemctl", "list-units", "--type=service", "--no-pager", "--plain"}
				switch {
				case in.State == "" || in.State == "all":
					if in.State == "all" {
						args = append(args, "--all")
					}
				case serviceStates[in.State]:
					args = append(args, "--state="+in.State)
				default:
					return "", fmt.Errorf("invalid state %q", in.State)
				}
				return runAll(ctx, env, args), nil
			}),

		registry.NewTool("linux_scheduled_jobs", "Scheduled jobs (timers and cron)",
			"systemd timers (next and last run, result of the last run), the cron daemon state, system cron tables "+
				"(/etc/crontab, /etc/cron.d, anacron, cron.hourly/daily/weekly/monthly scripts) and which users have a "+
				"personal crontab with its number of entries. The content of user crontabs is never shown.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				timers := env.Exec.Run(ctx, "systemctl", "list-timers", "--all", "--no-pager").Format()
				return timers + "\n" + formatSchedule(collectSchedule(ctx, env)), nil
			}),

		registry.NewTool("linux_journal", "Read the system journal",
			"Read systemd journal entries with filters (unit, time range, priority, regex, kernel only, previous boot). "+
				"Always narrow the query: prefer a unit, a since value or a priority.",
			ro, true, func(ctx context.Context, in journalInput) (string, error) {
				args, err := journalArgs(in)
				if err != nil {
					return "", err
				}
				// Newest entries come last: keep the tail when output is too big.
				return env.Exec.RunTail(ctx, args[0], args[1:]...).Format(), nil
			}),

		registry.NewTool("linux_read_file", "Read a file",
			"Read a text file (configuration, log, /proc or /sys entry). Output is size-limited; use tail for logs.",
			ro, true, func(_ context.Context, in fileInput) (string, error) {
				return files.readFile(in.Path, env.Cfg.Limits.MaxOutputBytes, in.Tail)
			}),

		registry.NewTool("linux_list_dir", "List a directory",
			"List directory entries with mode, size and modification time.",
			ro, true, func(_ context.Context, in dirInput) (string, error) {
				return files.listDir(in.Path, maxDirEntries)
			}),
	}
}

func journalArgs(in journalInput) ([]string, error) {
	lines := clamp(in.Lines, 100, maxJournalLines)
	args := []string{"journalctl", "--no-pager", "--quiet", "--output=short-iso", "--lines=" + itoa(lines)}
	if in.Unit != "" {
		if err := validUnit(in.Unit); err != nil {
			return nil, err
		}
		args = append(args, "--unit="+in.Unit)
	}
	for _, t := range []struct{ flag, val string }{{"--since=", in.Since}, {"--until=", in.Until}} {
		if t.val == "" {
			continue
		}
		spec, err := journalTime(t.val)
		if err != nil {
			return nil, err
		}
		args = append(args, t.flag+spec)
	}
	if in.Priority != "" {
		if err := validPriority(in.Priority); err != nil {
			return nil, err
		}
		args = append(args, "--priority="+in.Priority)
	}
	if in.Grep != "" {
		if len(in.Grep) > 200 {
			return nil, fmt.Errorf("grep pattern too long (max 200 characters)")
		}
		args = append(args, "--grep="+in.Grep)
	}
	if in.Kernel {
		args = append(args, "--dmesg")
	}
	if in.Boot != nil {
		if *in.Boot > 0 || *in.Boot < -20 {
			return nil, fmt.Errorf("boot must be between -20 and 0")
		}
		args = append(args, "--boot="+itoa(*in.Boot))
	}
	return args, nil
}

// runAll runs commands sequentially and concatenates their formatted output.
func runAll(ctx context.Context, env *registry.Env, cmds ...[]string) string {
	parts := make([]string, 0, len(cmds))
	for _, c := range cmds {
		parts = append(parts, env.Exec.Run(ctx, c[0], c[1:]...).Format())
	}
	return strings.Join(parts, "\n")
}

func dfExcludes() []string {
	var out []string
	for _, t := range []string{"tmpfs", "devtmpfs", "overlay", "squashfs", "efivarfs", "nsfs"} {
		out = append(out, "-x", t)
	}
	return out
}

func headLines(s string, n int) string {
	lines := strings.SplitAfterN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "")
}
