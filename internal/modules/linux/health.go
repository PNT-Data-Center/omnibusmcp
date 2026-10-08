package linux

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Thresholds for health checks.
const (
	fsWarnPct, fsCritPct           = 85, 95
	memWarnPct, memCritPct         = 10, 5 // available memory
	loadWarnPerCPU, loadCritPerCPU = 1.5, 3.0
)

// Health implements registry.Module.
func (m *Module) Health(ctx context.Context, env *registry.Env) []registry.Check {
	checks := []registry.Check{
		registry.VersionCheck(m.Version(ctx, env)),
		hostCheck(),
		systemdCheck(ctx, env),
		filesystemCheck(ctx, env),
		memoryCheck(),
		loadCheck(),
		timeSyncCheck(ctx, env),
		schedulerCheck(ctx, env),
	}
	for i := range checks {
		checks[i].Module = Name
	}
	return checks
}

func hostCheck() registry.Check {
	host, _ := os.Hostname()
	osName := "unknown OS"
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				osName = strings.Trim(v, `"`)
			}
		}
	}
	kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	uptime := "?"
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(data)); len(f) > 0 {
			if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
				uptime = formatUptime(time.Duration(secs) * time.Second)
			}
		}
	}
	return registry.Check{Name: "host", Status: registry.OK,
		Detail: fmt.Sprintf("%s, %s, kernel %s, up %s", host, osName, strings.TrimSpace(string(kernel)), uptime)}
}

// Version implements registry.Versioned: the distribution release.
func (*Module) Version(context.Context, *registry.Env) compat.Result {
	p, v := compat.OSRelease()
	return compat.Assess(p, v)
}

// formatUptime renders e.g. "47d 19h 47m" instead of "1147h47m0s".
func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func systemdCheck(ctx context.Context, env *registry.Env) registry.Check {
	c := registry.Check{Name: "systemd"}
	r := env.Exec.Run(ctx, "systemctl", "is-system-running")
	state := strings.TrimSpace(r.Stdout)
	switch {
	case r.NotFound || r.TimedOut || state == "":
		c.Status, c.Detail = registry.Unknown, "cannot query systemd state"
		return c
	case state == "running":
		c.Status, c.Detail = registry.OK, "running, no failed units"
		return c
	case state == "starting" || state == "initializing" || state == "degraded" || state == "maintenance":
		c.Status = registry.Warn
	default:
		c.Status = registry.Crit
	}
	c.Detail = state
	if failed := failedUnits(ctx, env); len(failed) > 0 {
		c.Detail += fmt.Sprintf("; %d failed: %s", len(failed), strings.Join(limit(failed, 10), ", "))
	}
	return c
}

func failedUnits(ctx context.Context, env *registry.Env) []string {
	r := env.Exec.Run(ctx, "systemctl", "list-units", "--state=failed", "--no-legend", "--plain", "--no-pager")
	var units []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			units = append(units, f[0])
		}
	}
	return units
}

func filesystemCheck(ctx context.Context, env *registry.Env) registry.Check {
	c := registry.Check{Name: "filesystems"}
	args := append([]string{"--output=pcent,ipcent,fstype,target"}, dfExcludes()...)
	r := env.Exec.Run(ctx, "df", args...)
	if r.NotFound || r.TimedOut || r.Stdout == "" {
		c.Status = registry.Unknown
		c.Detail = "df failed"
		if r.TimedOut {
			c.Detail = "df timed out: a mount (e.g. NFS/CephFS) may be hung"
		}
		return c
	}
	type usage struct {
		mount string
		pct   int
		kind  string
	}
	var bad []usage
	count, maxPct, maxMount := 0, -1, ""
	sc := bufio.NewScanner(strings.NewReader(r.Stdout))
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		mount := strings.Join(f[3:], " ")
		if alwaysFull(f[2], mount) {
			continue
		}
		count++
		for i, kind := range []string{"space", "inodes"} {
			pct, err := strconv.Atoi(strings.TrimSuffix(f[i], "%"))
			if err != nil {
				continue // "-" for filesystems without inodes
			}
			if kind == "space" && pct > maxPct {
				maxPct, maxMount = pct, mount
			}
			if pct >= fsWarnPct {
				bad = append(bad, usage{mount, pct, kind})
			}
		}
	}
	if len(bad) == 0 {
		c.Status = registry.OK
		c.Detail = fmt.Sprintf("%d filesystems, highest usage %d%% (%s)", count, maxPct, maxMount)
		return c
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].pct > bad[j].pct })
	c.Status = registry.Warn
	var parts []string
	for _, u := range bad {
		if u.pct >= fsCritPct {
			c.Status = registry.Crit
		}
		parts = append(parts, fmt.Sprintf("%s %s %d%%", u.mount, u.kind, u.pct))
	}
	c.Detail = strings.Join(limit(parts, 10), ", ")
	return c
}

// alwaysFull reports read-only image filesystems that are 100% used by
// design (ISO images, AppImage and snap mounts).
func alwaysFull(fstype, mount string) bool {
	switch fstype {
	case "iso9660", "udf", "squashfs":
		return true
	}
	return strings.HasPrefix(mount, "/tmp/.mount_") || strings.HasPrefix(mount, "/snap/")
}

func memoryCheck() registry.Check {
	c := registry.Check{Name: "memory"}
	info, err := meminfo()
	if err != nil || info["MemTotal"] == 0 {
		c.Status, c.Detail = registry.Unknown, "cannot read /proc/meminfo"
		return c
	}
	total, avail := info["MemTotal"], info["MemAvailable"]
	pct := avail * 100 / total
	c.Detail = fmt.Sprintf("available %d%% (%d of %d MiB)", pct, avail/1024, total/1024)
	if st := info["SwapTotal"]; st > 0 {
		c.Detail += fmt.Sprintf(", swap used %d%%", (st-info["SwapFree"])*100/st)
	}
	switch {
	case pct < memCritPct:
		c.Status = registry.Crit
	case pct < memWarnPct:
		c.Status = registry.Warn
	default:
		c.Status = registry.OK
	}
	return c
}

func meminfo() (map[string]int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			out[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	return out, nil
}

func loadCheck() registry.Check {
	c := registry.Check{Name: "load"}
	data, err := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(data))
	if err != nil || len(f) < 3 {
		c.Status, c.Detail = registry.Unknown, "cannot read /proc/loadavg"
		return c
	}
	cpus := runtime.NumCPU()
	load5, _ := strconv.ParseFloat(f[1], 64)
	perCPU := load5 / float64(cpus)
	c.Detail = fmt.Sprintf("load %s %s %s on %d CPUs (5m per CPU %.2f)", f[0], f[1], f[2], cpus, perCPU)
	switch {
	case perCPU >= loadCritPerCPU:
		c.Status = registry.Crit
	case perCPU >= loadWarnPerCPU:
		c.Status = registry.Warn
	default:
		c.Status = registry.OK
	}
	return c
}

func timeSyncCheck(ctx context.Context, env *registry.Env) registry.Check {
	c := registry.Check{Name: "time_sync"}
	r := env.Exec.Run(ctx, "timedatectl", "show", "--property=NTPSynchronized", "--value")
	svc := timeService(env.Exec.Run(ctx, "systemctl", append([]string{"is-active"}, timeServices...)...).Stdout)
	switch strings.TrimSpace(r.Stdout) {
	case "yes":
		c.Status, c.Detail = registry.OK, "clock synchronised (NTP via "+svc+")"
	case "no":
		c.Status, c.Detail = registry.Warn, "clock NOT synchronised (time service: "+svc+"); clustered services (Ceph, corosync) are sensitive to clock skew"
	default:
		c.Status, c.Detail = registry.Unknown, "cannot query timedatectl"
	}
	return c
}

// timeServices are the NTP daemons checked, in systemctl is-active order.
// chronyd is the RHEL name of chrony (Debian also installs it as an alias).
var timeServices = []string{"chrony", "chronyd", "systemd-timesyncd", "ntpsec", "ntp", "ntpd", "openntpd"}

// timeService names the first active NTP daemon from "systemctl is-active"
// output (one state per line, in timeServices order).
func timeService(isActive string) string {
	states := strings.Fields(isActive)
	for i, st := range states {
		if i < len(timeServices) && st == "active" {
			if timeServices[i] == "chronyd" {
				return "chrony"
			}
			return timeServices[i]
		}
	}
	return "no known NTP service active"
}

func limit(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("... +%d more", len(s)-n))
	}
	return s
}
