package linux

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// cronPaths are the cron locations read by linux_scheduled_jobs; a variable
// so tests can point them at a temporary directory.
var cronPaths = struct {
	crontab, cronD, anacrontab string
	periodic                   []string
	// userSpools: Debian keeps user crontabs in crontabs/, RHEL directly in
	// /var/spool/cron. Directories are skipped, so listing both is safe.
	userSpools []string
}{
	crontab:    "/etc/crontab",
	cronD:      "/etc/cron.d",
	anacrontab: "/etc/anacrontab",
	periodic:   []string{"/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly"},
	userSpools: []string{"/var/spool/cron/crontabs", "/var/spool/cron"},
}

// cronUnits are the cron daemon units of the supported distributions.
var cronUnits = []string{"cron.service", "crond.service"}

const maxCronFileBytes = 1 << 20

var (
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)
	safeName      = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
)

// cronFile is a system cron table with its entries (shown in full: these
// files are readable with linux_read_file anyway).
type cronFile struct {
	Path    string
	Entries []string
	Err     error
}

// userCrontab is a personal crontab: only the owner and the entry count
// are reported, never the content (commands may carry credentials).
type userCrontab struct {
	User    string
	Entries int
}

type timerJob struct {
	Timer, Unit, Result, State string
}

type schedule struct {
	CronDaemon       string // unit name of the cron daemon, "" if none installed
	CronActive       bool
	System           []cronFile
	Periodic         map[string][]string // directory -> script names
	Users            []userCrontab
	UserErr          []string
	Timers           []timerJob
	TimersQueryError string
}

func (s schedule) cronEntries() (system, user int) {
	for _, f := range s.System {
		system += len(f.Entries)
	}
	for _, d := range s.Periodic {
		system += len(d)
	}
	for _, u := range s.Users {
		user += u.Entries
	}
	return system, user
}

func (s schedule) failedTimers() []timerJob {
	var out []timerJob
	for _, t := range s.Timers {
		if t.Result != "" && t.Result != "success" {
			out = append(out, t)
		}
	}
	return out
}

// collectSchedule gathers timers, the cron daemon state and cron tables.
func collectSchedule(ctx context.Context, env *registry.Env) schedule {
	var s schedule
	s.Timers, s.TimersQueryError = timerJobs(ctx, env)

	props := showUnits(ctx, env, cronUnits, "Id,LoadState,ActiveState")
	for _, u := range cronUnits {
		if p := props[u]; p["LoadState"] == "loaded" {
			s.CronDaemon, s.CronActive = u, p["ActiveState"] == "active"
			break
		}
	}
	readCron(&s)
	return s
}

// readCron fills the cron tables of s from the files in cronPaths.
func readCron(s *schedule) {
	paths := []string{cronPaths.crontab}
	if entries, err := os.ReadDir(cronPaths.cronD); err == nil {
		for _, e := range entries {
			// cron skips dotfiles such as .placeholder.
			if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
				paths = append(paths, filepath.Join(cronPaths.cronD, e.Name()))
			}
		}
	}
	paths = append(paths, cronPaths.anacrontab)
	for _, p := range paths {
		lines, err := readCronFile(p)
		if os.IsNotExist(err) {
			continue
		}
		s.System = append(s.System, cronFile{Path: p, Entries: lines, Err: err})
	}

	s.Periodic = map[string][]string{}
	for _, dir := range cronPaths.periodic {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			// run-parts skips dotfiles such as .placeholder.
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				s.Periodic[dir] = append(s.Periodic[dir], e.Name())
			}
		}
	}

	seen := map[string]bool{}
	for _, dir := range cronPaths.userSpools {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				s.UserErr = append(s.UserErr, fmt.Sprintf("%s: %v", dir, err))
			}
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.Type().IsRegular() || seen[name] || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "tmp.") {
				continue
			}
			if !safeName.MatchString(name) {
				name = "(unusual file name)"
			}
			lines, err := readCronFile(filepath.Join(dir, e.Name()))
			if err != nil {
				s.UserErr = append(s.UserErr, fmt.Sprintf("crontab of %s: %v", name, err))
				continue
			}
			seen[e.Name()] = true
			s.Users = append(s.Users, userCrontab{User: name, Entries: len(lines)})
		}
	}
	sort.Slice(s.Users, func(i, j int) bool { return s.Users[i].User < s.Users[j].User })
}

// readCronFile returns the job lines of a crontab: no comments, blank lines
// or environment assignments. Symlinks and oversized files are refused.
func readCronFile(path string) ([]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCronFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCronFileBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxCronFileBytes)
	}
	return cronEntries(string(data)), nil
}

func cronEntries(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || envAssignment.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// timerJobs lists all timers with the unit each one triggers and the result
// of that unit's last run.
func timerJobs(ctx context.Context, env *registry.Env) ([]timerJob, string) {
	r := env.Exec.Run(ctx, "systemctl", "list-units", "--type=timer", "--all", "--no-legend", "--plain", "--no-pager")
	if r.NotFound || r.TimedOut || r.ExitCode != 0 {
		return nil, "cannot list timers"
	}
	var timers []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if f := strings.Fields(l); len(f) > 0 && strings.HasSuffix(f[0], ".timer") {
			timers = append(timers, f[0])
		}
	}
	if len(timers) == 0 {
		return nil, ""
	}
	tprops := showUnits(ctx, env, timers, "Id,Unit")
	var units []string
	for _, t := range timers {
		if u := tprops[t]["Unit"]; u != "" {
			units = append(units, u)
		}
	}
	uprops := showUnits(ctx, env, units, "Id,Result,ActiveState")
	jobs := make([]timerJob, 0, len(timers))
	for _, t := range timers {
		u := tprops[t]["Unit"]
		jobs = append(jobs, timerJob{Timer: t, Unit: u, Result: uprops[u]["Result"], State: uprops[u]["ActiveState"]})
	}
	return jobs, ""
}

// showUnits runs "systemctl show -p <props> -- <units>" and returns the
// properties of each unit keyed by its Id.
func showUnits(ctx context.Context, env *registry.Env, units []string, props string) map[string]map[string]string {
	out := map[string]map[string]string{}
	if len(units) == 0 {
		return out
	}
	args := append([]string{"show", "-p", props, "--"}, units...)
	r := env.Exec.Run(ctx, "systemctl", args...)
	return parseShow(r.Stdout)
}

// parseShow splits "systemctl show" output into blocks (blank-line
// separated, one per unit) keyed by the Id property.
func parseShow(s string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, block := range strings.Split(strings.TrimSpace(s), "\n\n") {
		m := map[string]string{}
		for _, l := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		if id := m["Id"]; id != "" {
			out[id] = m
		}
	}
	return out
}

// formatSchedule renders the collected schedule for linux_scheduled_jobs.
func formatSchedule(s schedule) string {
	var b strings.Builder
	b.WriteString("== systemd timers: last run result ==\n")
	switch {
	case s.TimersQueryError != "":
		b.WriteString(s.TimersQueryError + "\n")
	case len(s.Timers) == 0:
		b.WriteString("no timers\n")
	default:
		for _, t := range s.Timers {
			res := t.Result
			if res == "" {
				res = "unknown"
			}
			mark := ""
			if res != "success" && res != "unknown" {
				mark = "  <- last run FAILED"
			}
			fmt.Fprintf(&b, "%s -> %s: %s (%s)%s\n", t.Timer, t.Unit, res, t.State, mark)
		}
	}

	b.WriteString("\n== cron daemon ==\n")
	switch {
	case s.CronDaemon == "":
		b.WriteString("no cron daemon installed (" + strings.Join(cronUnits, ", ") + " not found)\n")
	case s.CronActive:
		b.WriteString(s.CronDaemon + ": active\n")
	default:
		b.WriteString(s.CronDaemon + ": NOT active (cron jobs will not run)\n")
	}

	b.WriteString("\n== system cron tables ==\n")
	if len(s.System) == 0 {
		b.WriteString("none\n")
	}
	for _, f := range s.System {
		switch {
		case f.Err != nil:
			fmt.Fprintf(&b, "%s: [%v]\n", f.Path, f.Err)
		case len(f.Entries) == 0:
			fmt.Fprintf(&b, "%s: no entries\n", f.Path)
		default:
			fmt.Fprintf(&b, "%s (%s):\n", f.Path, entries(len(f.Entries)))
			for _, e := range f.Entries {
				b.WriteString("  " + truncate(e, 300) + "\n")
			}
		}
	}
	for _, dir := range cronPaths.periodic {
		if names := s.Periodic[dir]; len(names) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", dir, strings.Join(names, ", "))
		}
	}

	b.WriteString("\n== user crontabs (owner and entry count only, content not shown) ==\n")
	if len(s.Users) == 0 && len(s.UserErr) == 0 {
		b.WriteString("none\n")
	}
	for _, u := range s.Users {
		fmt.Fprintf(&b, "%s: %s\n", u.User, entries(u.Entries))
	}
	for _, e := range s.UserErr {
		b.WriteString("[" + e + "]\n")
	}
	return b.String()
}

// schedulerCheck warns when cron jobs exist but nothing runs them. Without
// a cron daemon only user crontabs count: minimal Debian systems carry
// package cron files (e2scrub, apt, dpkg) whose work systemd timers do.
// Failed timer jobs are listed for context only: their units are already
// failed units, reported by the systemd check.
func schedulerCheck(ctx context.Context, env *registry.Env) registry.Check {
	return evalScheduler(collectSchedule(ctx, env))
}

func evalScheduler(s schedule) registry.Check {
	sys, usr := s.cronEntries()
	c := registry.Check{Name: "scheduler", Status: registry.OK}

	parts := []string{fmt.Sprintf("%d timer(s)", len(s.Timers))}
	if s.TimersQueryError != "" {
		parts[0] = s.TimersQueryError
	}
	if failed := s.failedTimers(); len(failed) > 0 {
		names := make([]string, len(failed))
		for i, t := range failed {
			names[i] = t.Unit
		}
		parts[0] += fmt.Sprintf(" (last run failed: %s)", strings.Join(limit(names, 5), ", "))
	}
	cron := fmt.Sprintf("cron: %d system + %d user entries", sys, usr)
	switch {
	case s.CronDaemon == "" && usr > 0:
		c.Status = registry.Warn
		cron += ", but no cron daemon is installed: user crontabs do not run"
	case s.CronDaemon == "" && sys > 0:
		cron += ", no cron daemon installed (package cron files are not executed; their work is usually done by systemd timers)"
	case s.CronDaemon == "":
		cron = "no cron daemon installed"
	case !s.CronActive && sys+usr > 0:
		c.Status = registry.Warn
		cron += ", but " + s.CronDaemon + " is not active"
	case s.CronActive:
		cron += ", " + s.CronDaemon + " active"
	default:
		cron += ", " + s.CronDaemon + " inactive"
	}
	c.Detail = strings.Join(append(parts, cron), "; ")
	return c
}

func entries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
