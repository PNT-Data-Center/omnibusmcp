package pbs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/aptrepo"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Thresholds.
const (
	storeWarnPct, storeCritPct = 85, 95
	fullSoon                   = 30 * 24 * time.Hour // estimated full date
	certWarn                   = 30 * 24 * time.Hour
)

var pbsServices = []string{"proxmox-backup", "proxmox-backup-proxy"}

// Health implements registry.Module. API calls run concurrently, bounded by
// the API slot limit.
func (m *Module) Health(ctx context.Context, env *registry.Env) []registry.Check {
	svc := servicesCheck(ctx, env)
	if svc.Status == registry.Crit && strings.Contains(svc.Detail, "proxmox-backup=") {
		// The API daemon is down: API calls would fail or hang.
		checks := []registry.Check{registry.VersionCheck(m.Version(ctx, env)), svc}
		for _, n := range []string{"datastores", "gc", "backups", "jobs", "tasks", "updates", "certificate"} {
			checks = append(checks, registry.Check{Name: n, Status: registry.Unknown, Detail: "skipped: proxmox-backup (API) is not running"})
		}
		return label(checks)
	}

	var (
		wg       sync.WaitGroup
		ds       []datastore
		dsErr    error
		js       jobSet
		tasks    []task
		taskErr  error
		ups      []aptrepo.Update
		upErr    error
		repos    aptrepo.Repos
		repoErr  error
		certs    []certInfo
		certErr  error
		groups   = map[string][]group{}
		groupErr = map[string]error{}
		groupMu  sync.Mutex
		since    = strconv.FormatInt(time.Now().Add(-24*time.Hour).Unix(), 10)
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() {
		ds, dsErr = loadDatastores(ctx, env)
		if dsErr != nil {
			return
		}
		var gw sync.WaitGroup
		for _, d := range ds {
			gw.Add(1)
			go func() {
				defer gw.Done()
				g, err := loadGroups(ctx, env, d.Config.Name)
				groupMu.Lock()
				groups[d.Config.Name], groupErr[d.Config.Name] = g, err
				groupMu.Unlock()
			}()
		}
		gw.Wait()
	})
	run(func() { js = loadJobs(ctx, env) })
	run(func() {
		taskErr = api(ctx, env, &tasks, localNodeAPI+"/tasks", "--errors", "1", "--since", since, "--limit", "200")
	})
	run(func() { upErr = api(ctx, env, &ups, localNodeAPI+"/apt/update") })
	run(func() { repoErr = api(ctx, env, &repos, localNodeAPI+"/apt/repositories") })
	run(func() { certErr = api(ctx, env, &certs, localNodeAPI+"/certificates/info") })
	wg.Wait()

	now := time.Now()
	checks := []registry.Check{registry.VersionCheck(m.Version(ctx, env)), svc}
	if dsErr != nil {
		for _, n := range []string{"datastores", "gc", "backups"} {
			checks = append(checks, unknown(n, dsErr))
		}
	} else {
		checks = append(checks, evalDatastores(ds, now), evalGC(ds), evalBackups(ds, groups, groupErr, now))
	}
	var storeNames []string
	for _, d := range ds {
		storeNames = append(storeNames, d.Config.Name)
	}
	checks = append(checks, evalJobs(js, storeNames, now))
	if taskErr != nil {
		checks = append(checks, unknown("tasks", taskErr))
	} else {
		checks = append(checks, evalTasks(tasks))
	}
	if upErr != nil {
		checks = append(checks, unknown("updates", upErr))
	} else {
		checks = append(checks, aptrepo.Eval(ups, repos, repoErr, "Proxmox Backup Server"))
	}
	if certErr != nil {
		checks = append(checks, unknown("certificate", certErr))
	} else {
		checks = append(checks, evalCerts(certs, now))
	}
	return label(checks)
}

// Version implements registry.Versioned: proxmox-backup-server from the dpkg
// database.
func (*Module) Version(context.Context, *registry.Env) compat.Result {
	return compat.Assess(compat.PBS, compat.PackageVersion("proxmox-backup-server"))
}

func label(checks []registry.Check) []registry.Check {
	for i := range checks {
		checks[i].Module = Name
	}
	return checks
}

func unknown(name string, err error) registry.Check {
	first, _, _ := strings.Cut(err.Error(), "\n")
	if strings.Contains(err.Error(), "TIMEOUT") {
		first = "API call timed out (proxmox-backup may be hung)"
	}
	return registry.Check{Name: name, Status: registry.Unknown, Detail: first}
}

func servicesCheck(ctx context.Context, env *registry.Env) registry.Check {
	c := registry.Check{Name: "services", Status: registry.OK}
	r := env.Exec.Run(ctx, "systemctl", append([]string{"is-active"}, pbsServices...)...)
	states := strings.Fields(r.Stdout)
	if r.NotFound || r.TimedOut || len(states) != len(pbsServices) {
		c.Status, c.Detail = registry.Unknown, "cannot query service states"
		return c
	}
	var bad []string
	for i, s := range pbsServices {
		if states[i] != "active" {
			bad = append(bad, s+"="+states[i])
		}
	}
	if len(bad) > 0 {
		c.Status, c.Detail = registry.Crit, "not active: "+strings.Join(bad, ", ")
		return c
	}
	c.Detail = "active: " + strings.Join(pbsServices, ", ")
	return c
}

func evalDatastores(ds []datastore, now time.Time) registry.Check {
	c := registry.Check{Name: "datastores", Status: registry.OK}
	raise := func(s registry.Status) {
		if s > c.Status {
			c.Status = s
		}
	}
	shared := sharedWith(ds)
	reported := map[string]bool{} // one usage verdict per filesystem
	var problems []string
	maxPct, maxName := -1.0, ""
	for _, d := range ds {
		name := d.Config.Name
		if d.Config.MaintenanceMode != "" {
			raise(registry.Warn)
			problems = append(problems, fmt.Sprintf("%s in maintenance mode (%s)", name, d.Config.MaintenanceMode))
		}
		u := d.Usage
		if u == nil {
			raise(registry.Warn)
			problems = append(problems, name+" has no usage data")
			continue
		}
		if u.MountStatus != "" && u.MountStatus != "nonremovable" && u.MountStatus != "mounted" {
			raise(registry.Crit)
			problems = append(problems, fmt.Sprintf("%s mount status %s", name, u.MountStatus))
			continue
		}
		if d.FSKey != "" {
			if reported[d.FSKey] {
				continue
			}
			reported[d.FSKey] = true
		}
		label := name
		if others := shared[name]; len(others) > 0 {
			label = name + " (+" + strings.Join(others, ", ") + ", shared filesystem)"
		}
		p := pct(u.Used, u.Total)
		if p > maxPct {
			maxPct, maxName = p, label
		}
		switch {
		case p >= storeCritPct:
			raise(registry.Crit)
			problems = append(problems, fmt.Sprintf("%s %.0f%% full", label, p))
		case p >= storeWarnPct:
			raise(registry.Warn)
			problems = append(problems, fmt.Sprintf("%s %.0f%% full", label, p))
		}
		if full := u.EstimatedFull; full > now.Unix() && time.Unix(full, 0).Sub(now) < fullSoon {
			raise(registry.Warn)
			problems = append(problems, fmt.Sprintf("%s estimated full on %s", label, estimatedFull(full, now)))
		}
	}
	switch {
	case len(problems) > 0:
		c.Detail = strings.Join(problems, "; ")
	case len(ds) == 0:
		c.Status, c.Detail = registry.Warn, "no datastores configured"
	default:
		c.Detail = fmt.Sprintf("%d datastore(s), highest usage %.0f%% (%s)", len(ds), maxPct, maxName)
	}
	return c
}

func evalGC(ds []datastore) registry.Check {
	c := registry.Check{Name: "gc", Status: registry.OK}
	var problems []string
	ran := 0
	for _, d := range ds {
		g := d.GC
		if g == nil {
			if d.GCErr != nil {
				problems = append(problems, d.Config.Name+" GC status unavailable")
				if c.Status < registry.Unknown {
					c.Status = registry.Unknown
				}
			}
			continue
		}
		if g.StillBad > 0 {
			c.Status = registry.Crit
			problems = append(problems, fmt.Sprintf("%s has %d bad chunk(s): run verify", d.Config.Name, g.StillBad))
		}
		switch st := strings.ToLower(g.LastRunState); {
		case st == "":
		case st == "ok":
			ran++
		default:
			if c.Status < registry.Warn {
				c.Status = registry.Warn
			}
			problems = append(problems, fmt.Sprintf("%s last GC: %s", d.Config.Name, g.LastRunState))
		}
	}
	if len(problems) > 0 {
		c.Detail = strings.Join(problems, "; ")
		return c
	}
	c.Detail = fmt.Sprintf("last garbage collection OK on %d of %d datastore(s), no bad chunks", ran, len(ds))
	return c
}

// evalBackups warns when a datastore holding backups got none recently; a
// few stale groups (removed guests) are normal and only counted.
func evalBackups(ds []datastore, groups map[string][]group, errs map[string]error, now time.Time) registry.Check {
	c := registry.Check{Name: "backups", Status: registry.OK}
	var problems, summary []string
	totalGroups, staleGroups := 0, 0
	for _, d := range ds {
		name := d.Config.Name
		if err := errs[name]; err != nil {
			problems = append(problems, name+" groups unavailable")
			if c.Status < registry.Unknown {
				c.Status = registry.Unknown
			}
			continue
		}
		gs := groups[name]
		if len(gs) == 0 {
			continue
		}
		var newest int64
		for _, g := range gs {
			totalGroups++
			if g.LastBackup > newest {
				newest = g.LastBackup
			}
			if now.Unix()-g.LastBackup > int64(staleBackup.Seconds()) {
				staleGroups++
			}
		}
		if now.Unix()-newest > int64(staleBackup.Seconds()) {
			c.Status = registry.Warn
			problems = append(problems, fmt.Sprintf("%s: no backup for %s", name, humanDuration(now.Unix()-newest)))
		} else {
			summary = append(summary, fmt.Sprintf("%s newest %s", name, ago(newest, now)))
		}
	}
	sort.Strings(summary)
	detail := fmt.Sprintf("%d group(s), %d without a backup in 48h", totalGroups, staleGroups)
	if len(summary) > 0 {
		detail += "; " + strings.Join(summary, ", ")
	}
	if len(problems) > 0 {
		detail = strings.Join(problems, "; ") + "; " + detail
	}
	c.Detail = detail
	return c
}

// evalJobs reports failed last runs, jobs that never ran although their
// schedule is long past (the scheduler is not running them), jobs pointing
// at a missing datastore, and prune jobs without retention options.
func evalJobs(js jobSet, stores []string, now time.Time) registry.Check {
	c := registry.Check{Name: "jobs", Status: registry.OK}
	known := map[string]bool{}
	for _, s := range stores {
		known[s] = true
	}
	warn := func(msg string) {
		if c.Status < registry.Warn {
			c.Status = registry.Warn
		}
		c.Detail += "; " + msg
	}
	var counts []string
	var neverRan, noRetention []string
	for _, set := range []struct {
		kind string
		jobs []job
		err  error
	}{{"prune", js.Prune, js.PruneErr}, {"verify", js.Verify, js.VerifyErr}, {"sync", js.Sync, js.SyncErr}} {
		if set.err != nil {
			if c.Status < registry.Unknown {
				c.Status = registry.Unknown
			}
			c.Detail += "; " + set.kind + " jobs unavailable"
			continue
		}
		counts = append(counts, fmt.Sprintf("%d %s", len(set.jobs), set.kind))
		for _, j := range set.jobs {
			if st := j.LastRunState; st != "" && !strings.EqualFold(st, "ok") {
				warn(fmt.Sprintf("%s job %s (%s) last run: %s", set.kind, j.ID, j.Store, st))
			}
			if len(stores) > 0 && j.Store != "" && !known[j.Store] {
				warn(fmt.Sprintf("%s job %s references missing datastore %q", set.kind, j.ID, j.Store))
			}
			if j.LastRunEnd == 0 && j.overdue(now) {
				neverRan = append(neverRan, fmt.Sprintf("%s %s (due %s)", set.kind, j.ID, epoch(j.NextRun)))
			}
			if set.kind == "prune" && j.Retention == "none" {
				noRetention = append(noRetention, j.ID)
			}
		}
	}
	if len(neverRan) > 0 {
		warn(fmt.Sprintf("%d job(s) never ran although overdue: %s", len(neverRan), strings.Join(limitList(neverRan, 5), ", ")))
	}
	if len(noRetention) > 0 {
		warn(fmt.Sprintf("%d prune job(s) without keep-* retention options (they remove nothing)", len(noRetention)))
	}
	summary := strings.Join(counts, ", ") + " job(s)"
	if c.Detail == "" {
		c.Detail = summary + ", no problems"
	} else {
		c.Detail = strings.TrimPrefix(c.Detail, "; ") + "; " + summary
	}
	return c
}

func limitList(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("+%d more", len(s)-n))
	}
	return s
}

func evalTasks(tasks []task) registry.Check {
	c := registry.Check{Name: "tasks", Status: registry.OK}
	byType := map[string]int{}
	failed := 0
	for _, t := range tasks {
		if t.failed() {
			failed++
			byType[t.WorkerType]++
		}
	}
	if failed == 0 {
		c.Detail = "no failed tasks in the last 24h"
		return c
	}
	var parts []string
	for t, n := range byType {
		parts = append(parts, fmt.Sprintf("%s x%d", t, n))
	}
	sort.Strings(parts)
	c.Status = registry.Warn
	c.Detail = fmt.Sprintf("%d failed task(s) in the last 24h: %s (see pbs_tasks errors_only)", failed, strings.Join(parts, ", "))
	return c
}

func evalCerts(certs []certInfo, now time.Time) registry.Check {
	c := registry.Check{Name: "certificate", Status: registry.OK}
	if len(certs) == 0 {
		c.Status, c.Detail = registry.Unknown, "no certificate information"
		return c
	}
	var parts []string
	for _, ct := range certs {
		left := time.Unix(ct.NotAfter, 0).Sub(now)
		switch {
		case left <= 0:
			c.Status = registry.Crit
		case left < certWarn && c.Status < registry.Warn:
			c.Status = registry.Warn
		}
		parts = append(parts, fmt.Sprintf("%s valid until %s (%s)", ct.Filename, epoch(ct.NotAfter), certLeft(ct.NotAfter, now)))
	}
	c.Detail = strings.Join(parts, "; ")
	return c
}
