package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

func TestCronEntries(t *testing.T) {
	in := "# m h dom mon dow user command\nSHELL=/bin/sh\nPATH = /usr/bin\n\n17 * * * * root cd / && run-parts --report /etc/cron.hourly\n  @reboot root /usr/local/bin/x\n"
	got := cronEntries(in)
	if len(got) != 2 || !strings.HasPrefix(got[0], "17 * * * *") || got[1] != "@reboot root /usr/local/bin/x" {
		t.Fatalf("got %q", got)
	}
}

func TestParseShow(t *testing.T) {
	out := "Unit=logrotate.service\nId=logrotate.timer\n\nResult=exit-code\nId=logrotate.service\nActiveState=failed\n"
	m := parseShow(out)
	if m["logrotate.timer"]["Unit"] != "logrotate.service" || m["logrotate.service"]["Result"] != "exit-code" {
		t.Fatalf("got %v", m)
	}
}

// withCronFixture points cronPaths at a temporary tree for one test.
func withCronFixture(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	saved := cronPaths
	t.Cleanup(func() { cronPaths = saved })
	cronPaths.crontab = filepath.Join(d, "crontab")
	cronPaths.cronD = filepath.Join(d, "cron.d")
	cronPaths.anacrontab = filepath.Join(d, "anacrontab")
	cronPaths.periodic = []string{filepath.Join(d, "cron.daily")}
	cronPaths.userSpools = []string{filepath.Join(d, "spool", "crontabs"), filepath.Join(d, "spool")}
	for _, dir := range []string{"cron.d", "cron.daily", "spool/crontabs"} {
		os.MkdirAll(filepath.Join(d, dir), 0o755)
	}
	write := func(p, s string) { os.WriteFile(filepath.Join(d, p), []byte(s), 0o600) }
	write("crontab", "SHELL=/bin/sh\n25 6 * * * root run-parts /etc/cron.daily\n")
	write("cron.d/.placeholder", "# placeholder\n0 0 * * * root /ignored\n")
	write("cron.d/backup", "# nightly\n0 2 * * * root /usr/local/bin/backup.sh\n")
	write("cron.daily/logrotate", "#!/bin/sh\n")
	write("cron.daily/.placeholder", "")
	write("spool/crontabs/root", "# header\nMAILTO=x\n*/5 * * * * curl -u admin:SECRET https://example.com\n0 1 * * * /x\n") // gitleaks:allow (fake test data)
	write("spool/crontabs/www-data", "0 3 * * * /y\n")
	os.Symlink("/etc/shadow", filepath.Join(d, "spool/crontabs/evil"))
	return d
}

func TestReadCron(t *testing.T) {
	withCronFixture(t)
	var s schedule
	readCron(&s)
	if len(s.System) != 2 || len(s.System[0].Entries) != 1 || len(s.System[1].Entries) != 1 {
		t.Fatalf("system tables: %+v", s.System)
	}
	if got := s.Periodic[cronPaths.periodic[0]]; len(got) != 1 || got[0] != "logrotate" {
		t.Fatalf("periodic: %v", got)
	}
	if len(s.Users) != 2 || s.Users[0] != (userCrontab{"root", 2}) || s.Users[1] != (userCrontab{"www-data", 1}) {
		t.Fatalf("users: %+v", s.Users)
	}
	out := formatSchedule(s)
	for _, want := range []string{"root: 2 entries", "www-data: 1 entry", "/usr/local/bin/backup.sh", "cron.daily: logrotate", "content not shown"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// User crontab content and symlinked files never reach the output.
	for _, bad := range []string{"SECRET", "curl", "evil", "shadow", "placeholder", "/ignored"} {
		if strings.Contains(out, bad) {
			t.Errorf("output leaks %q:\n%s", bad, out)
		}
	}
}

func TestEvalScheduler(t *testing.T) {
	base := schedule{Timers: []timerJob{{Timer: "a.timer", Unit: "a.service", Result: "success"}, {Timer: "b.timer", Unit: "b.service", Result: "exit-code"}}}
	jobs := []cronFile{{Path: "/etc/crontab", Entries: []string{"x"}}}

	s := base
	s.CronDaemon, s.CronActive, s.System = "cron.service", true, jobs
	if c := evalScheduler(s); c.Status != registry.OK || !strings.Contains(c.Detail, "last run failed: b.service") || !strings.Contains(c.Detail, "cron.service active") {
		t.Errorf("active: %+v", c)
	}
	s.CronActive = false
	if c := evalScheduler(s); c.Status != registry.Warn || !strings.Contains(c.Detail, "cron.service is not active") {
		t.Errorf("inactive with jobs: %+v", c)
	}
	s = base
	s.Users = []userCrontab{{"root", 1}}
	if c := evalScheduler(s); c.Status != registry.Warn || !strings.Contains(c.Detail, "no cron daemon") {
		t.Errorf("no daemon with jobs: %+v", c)
	}
	s = base
	if c := evalScheduler(s); c.Status != registry.OK || !strings.Contains(c.Detail, "no cron daemon installed") {
		t.Errorf("no daemon, no jobs: %+v", c)
	}
	// Minimal Debian: package cron files, no daemon, timers do the work.
	s.System = jobs
	if c := evalScheduler(s); c.Status != registry.OK || !strings.Contains(c.Detail, "not executed") {
		t.Errorf("no daemon, package cron files only: %+v", c)
	}
}
