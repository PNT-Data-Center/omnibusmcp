// Package pbs is the Proxmox Backup Server module: node, datastores (usage,
// garbage collection, backup groups), jobs, tasks, updates. All tools are
// read-only (tier 1).
package pbs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/aptrepo"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Name is the module identifier used in configuration.
const Name = "pbs"

// staleBackup is the age after which a datastore without any newer backup
// is reported.
const staleBackup = 48 * time.Hour

// Module implements registry.Module.
type Module struct{}

// New returns the PBS module.
func New() *Module { return &Module{} }

func (*Module) Name() string { return Name }

func (*Module) Description() string {
	return "Proxmox Backup Server: datastores, garbage collection, backups, jobs, tasks, updates"
}

// Detect reports a PBS host: its configuration directory and the debug CLI.
func (*Module) Detect(context.Context, *registry.Env) bool {
	st, err := os.Stat("/etc/proxmox-backup")
	if err != nil || !st.IsDir() {
		return false
	}
	if _, err := exec.LookPath(debugCLI); err == nil {
		return true
	}
	_, err = os.Stat("/usr/sbin/" + debugCLI)
	return err == nil
}

type noInput struct{}

type storeInput struct {
	Store string `json:"store" jsonschema:"datastore name (see pbs_datastores)"`
}

type tasksInput struct {
	ErrorsOnly bool   `json:"errors_only,omitempty" jsonschema:"only failed tasks"`
	Since      string `json:"since,omitempty" jsonschema:"only tasks started within this period: e.g. 24h, 7d"`
	Type       string `json:"type,omitempty" jsonschema:"task type, e.g. backup, prune, garbage_collection, verificationjob, syncjob, aptupdate"`
	Store      string `json:"store,omitempty" jsonschema:"only tasks of this datastore"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum number of tasks (default 30, max 200)"`
}

type taskLogInput struct {
	UPID  string `json:"upid" jsonschema:"task UPID as listed by pbs_tasks"`
	Lines int    `json:"lines,omitempty" jsonschema:"number of last log lines (default 100, max 1000)"`
}

// Tools returns the module's tools.
func (m *Module) Tools(env *registry.Env) []registry.Tool {
	ro := tier.ReadOnly
	limit := func(s string) string { return truncate(s, env.Cfg.Limits.MaxOutputBytes) }
	return []registry.Tool{
		registry.NewTool("pbs_node_status", "PBS node status",
			"Proxmox Backup Server version, kernel, uptime, CPU, IO wait, load, memory, swap, root filesystem, boot mode, subscription status and the API/proxy TLS certificate (expiry, fingerprint clients pin).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				var st nodeStatus
				if err := api(ctx, env, &st, localNodeAPI+"/status"); err != nil {
					return "", err
				}
				var v version
				vErr := api(ctx, env, &v, "/version")
				var sub subscription
				subErr := api(ctx, env, &sub, localNodeAPI+"/subscription")
				var certs []certInfo
				certErr := api(ctx, env, &certs, localNodeAPI+"/certificates/info")
				return limit(renderNodeStatus(st, v, vErr, sub, subErr, certs, certErr, time.Now())), nil
			}),

		registry.NewTool("pbs_versions", "PBS package versions",
			"Versions of Proxmox Backup Server related packages (running version and kernel included).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				var pkgs []struct {
					Package    string `json:"Package"`
					OldVersion string `json:"OldVersion"`
					ExtraInfo  string `json:"ExtraInfo"`
				}
				if err := api(ctx, env, &pkgs, localNodeAPI+"/apt/versions"); err != nil {
					return "", err
				}
				var b strings.Builder
				tw := newTable(&b)
				fmt.Fprintln(tw, "PACKAGE\tVERSION\tINFO")
				for _, p := range pkgs {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Package, firstNonEmpty(p.OldVersion, "not installed"), p.ExtraInfo)
				}
				tw.Flush()
				return limit(b.String()), nil
			}),

		registry.NewTool("pbs_datastores", "PBS datastores",
			"All datastores: path, usage (used/total/available), estimated full date, mount status, maintenance mode, garbage collection (last run, state, pending space). Stores sharing one filesystem are marked.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				ds, err := loadDatastores(ctx, env)
				if err != nil {
					return "", err
				}
				return limit(renderDatastores(ds, time.Now())), nil
			}),

		registry.NewTool("pbs_datastore", "PBS datastore details",
			"One datastore: configuration, usage, garbage collection details (including bad chunks), and backup groups in all namespaces: counts by namespace and type, snapshots, newest/oldest backup, groups without a backup in the last 48 h.",
			ro, true, func(ctx context.Context, in storeInput) (string, error) {
				if err := validStore(in.Store); err != nil {
					return "", err
				}
				ds, err := loadDatastores(ctx, env)
				if err != nil {
					return "", err
				}
				var d *datastore
				for i := range ds {
					if ds[i].Config.Name == in.Store {
						d = &ds[i]
					}
				}
				if d == nil {
					return "", fmt.Errorf("no datastore %q", in.Store)
				}
				groups, gErr := loadGroups(ctx, env, in.Store)
				return limit(renderDatastore(*d, groups, gErr, time.Now())), nil
			}),

		registry.NewTool("pbs_jobs", "PBS jobs",
			"Scheduled prune, verify, sync and tape backup jobs with schedule, next run and the state of the last run.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				return limit(renderJobs(loadJobs(ctx, env), time.Now())), nil
			}),

		registry.NewTool("pbs_tasks", "PBS tasks",
			"Recent tasks (backup, prune, garbage collection, verification, sync, updates...) with status and UPID. Use errors_only to find failures, then pbs_task_log for details.",
			ro, true, func(ctx context.Context, in tasksInput) (string, error) {
				args, err := taskArgs(in)
				if err != nil {
					return "", err
				}
				var tasks []task
				if err := api(ctx, env, &tasks, localNodeAPI+"/tasks", args...); err != nil {
					return "", err
				}
				return limit(renderTasks(tasks)), nil
			}),

		registry.NewTool("pbs_task_log", "PBS task log",
			"Status, result and the last lines of the log of one task, identified by its UPID (from pbs_tasks).",
			ro, true, func(ctx context.Context, in taskLogInput) (string, error) {
				if !upidRe.MatchString(in.UPID) {
					return "", fmt.Errorf("invalid UPID %q", in.UPID)
				}
				lines := in.Lines
				switch {
				case lines == 0:
					lines = 100
				case lines < 1:
					lines = 1
				case lines > 1000:
					lines = 1000
				}
				base := localNodeAPI + "/tasks/" + in.UPID
				var st task
				if err := api(ctx, env, &st, base+"/status"); err != nil {
					return "", err
				}
				var log []logLine
				if err := api(ctx, env, &log, base+"/log", "--limit", "50000"); err != nil {
					return "", err
				}
				return limit(renderTaskLog(st, log, lines)), nil
			}),

		registry.NewTool("pbs_updates", "PBS updates and repositories",
			"Pending package updates (from the last apt update, not refreshed here) and APT repository configuration with warnings (e.g. no enabled Proxmox Backup Server repository).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				var ups []aptrepo.Update
				upErr := api(ctx, env, &ups, localNodeAPI+"/apt/update")
				var repos aptrepo.Repos
				repoErr := api(ctx, env, &repos, localNodeAPI+"/apt/repositories")
				return limit(aptrepo.Render(ups, upErr, repos, repoErr, "Proxmox Backup Server")), nil
			}),
	}
}

// datastore joins configuration, usage and GC state of one store.
type datastore struct {
	Config datastoreConfig
	Usage  *datastoreUsage
	GC     *gcStatus
	GCErr  error
	FSKey  string // identical for stores on one filesystem
}

func loadDatastores(ctx context.Context, env *registry.Env) ([]datastore, error) {
	var cfgs []datastoreConfig
	if err := api(ctx, env, &cfgs, "/config/datastore"); err != nil {
		return nil, err
	}
	var usages []datastoreUsage
	usageErr := api(ctx, env, &usages, "/status/datastore-usage")
	byName := map[string]*datastoreUsage{}
	for i := range usages {
		byName[usages[i].Store] = &usages[i]
	}
	ds := make([]datastore, len(cfgs))
	var wg sync.WaitGroup
	for i, c := range cfgs {
		ds[i] = datastore{Config: c, Usage: byName[c.Name], FSKey: fsKey(c.Path)}
		if usageErr != nil {
			ds[i].GCErr = usageErr
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var gc gcStatus
			if err := api(ctx, env, &gc, "/admin/datastore/"+c.Name+"/gc"); err != nil {
				ds[i].GCErr = err
				return
			}
			ds[i].GC = &gc
		}()
	}
	wg.Wait()
	sort.Slice(ds, func(i, j int) bool { return ds[i].Config.Name < ds[j].Config.Name })
	return ds, nil
}

// loadGroups lists backup groups in every namespace of a datastore (most
// PVE setups back up into namespaces; /groups alone shows only the root).
func loadGroups(ctx context.Context, env *registry.Env, store string) ([]group, error) {
	var nss []struct {
		NS string `json:"ns"`
	}
	if err := api(ctx, env, &nss, "/admin/datastore/"+store+"/namespace"); err != nil {
		return nil, err
	}
	if len(nss) == 0 {
		nss = append(nss, struct {
			NS string `json:"ns"`
		}{""})
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		all      []group
		firstErr error
	)
	for _, n := range nss {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var gs []group
			var args []string
			if n.NS != "" {
				args = []string{"--ns", n.NS}
			}
			err := api(ctx, env, &gs, "/admin/datastore/"+store+"/groups", args...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for i := range gs {
				gs[i].NS = n.NS
			}
			all = append(all, gs...)
		}()
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool {
		if all[i].NS != all[j].NS {
			return all[i].NS < all[j].NS
		}
		return all[i].BackupType+"/"+all[i].BackupID < all[j].BackupType+"/"+all[j].BackupID
	})
	return all, firstErr
}

// fsKey identifies the filesystem holding path (device number); empty if
// the path cannot be inspected.
func fsKey(path string) string {
	var st syscall.Stat_t
	if path == "" || syscall.Stat(path, &st) != nil {
		return ""
	}
	return strconv.FormatUint(uint64(st.Dev), 10)
}

// sharedWith returns, per store, the other stores on the same filesystem.
func sharedWith(ds []datastore) map[string][]string {
	byFS := map[string][]string{}
	for _, d := range ds {
		if d.FSKey != "" {
			byFS[d.FSKey] = append(byFS[d.FSKey], d.Config.Name)
		}
	}
	out := map[string][]string{}
	for _, names := range byFS {
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			for _, other := range names {
				if other != n {
					out[n] = append(out[n], other)
				}
			}
		}
	}
	return out
}

type jobSet struct {
	Prune, Verify, Sync, Tape          []job
	PruneErr, VerifyErr, SyncErr, TErr error
}

func loadJobs(ctx context.Context, env *registry.Env) jobSet {
	var js jobSet
	var wg sync.WaitGroup
	var pruneCfg []map[string]any
	wg.Add(1)
	go func() { defer wg.Done(); _ = api(ctx, env, &pruneCfg, "/config/prune") }()
	for _, j := range []struct {
		path string
		out  *[]job
		err  *error
	}{
		{"/admin/prune", &js.Prune, &js.PruneErr},
		{"/admin/verify", &js.Verify, &js.VerifyErr},
		{"/admin/sync", &js.Sync, &js.SyncErr},
		{"/config/tape-backup-job", &js.Tape, &js.TErr},
	} {
		wg.Add(1)
		go func() { defer wg.Done(); *j.err = api(ctx, env, j.out, j.path) }()
	}
	wg.Wait()
	retention := map[string]string{}
	for _, c := range pruneCfg {
		retention[fmt.Sprint(c["id"])] = retentionOf(c)
	}
	for i := range js.Prune {
		js.Prune[i].Retention = retention[js.Prune[i].ID]
	}
	return js
}

// retentionOf summarises keep-* options of a prune job ("none" if unset:
// such a job removes nothing).
func retentionOf(cfg map[string]any) string {
	var parts []string
	for _, k := range []string{"keep-last", "keep-hourly", "keep-daily", "keep-weekly", "keep-monthly", "keep-yearly"} {
		if v, ok := cfg[k]; ok {
			parts = append(parts, strings.TrimPrefix(k, "keep-")+"="+fmt.Sprint(v))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// overdue reports a job whose scheduled run is more than an hour late.
func (j job) overdue(now time.Time) bool {
	return j.NextRun > 0 && j.NextRun < now.Add(-time.Hour).Unix()
}

func nsName(ns string) string {
	if ns == "" {
		return "<root>"
	}
	return ns
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func taskArgs(in tasksInput) ([]string, error) {
	n := in.Limit
	switch {
	case n == 0:
		n = 30
	case n < 1:
		n = 1
	case n > 200:
		n = 200
	}
	args := []string{"--limit", strconv.Itoa(n)}
	if in.ErrorsOnly {
		args = append(args, "--errors", "1")
	}
	if in.Since != "" {
		d, err := parseSince(in.Since)
		if err != nil {
			return nil, err
		}
		args = append(args, "--since", strconv.FormatInt(time.Now().Add(-d).Unix(), 10))
	}
	if in.Type != "" {
		if !taskTypeRe.MatchString(in.Type) {
			return nil, fmt.Errorf("invalid task type %q", in.Type)
		}
		args = append(args, "--typefilter", in.Type)
	}
	if in.Store != "" {
		if err := validStore(in.Store); err != nil {
			return nil, err
		}
		args = append(args, "--store", in.Store)
	}
	return args, nil
}

func parseSince(s string) (time.Duration, error) {
	m := sinceRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid since %q (use e.g. 24h or 7d)", s)
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "d" {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.Duration(n) * time.Hour, nil
}

func newTable(b *strings.Builder) *tabwriter.Writer { return tabwriter.NewWriter(b, 0, 4, 2, ' ', 0) }

func renderNodeStatus(st nodeStatus, v version, vErr error, sub subscription, subErr error, certs []certInfo, certErr error, now time.Time) string {
	var b strings.Builder
	ver := "unavailable"
	if vErr == nil {
		ver = fmt.Sprintf("%s.%s (repo %s)", v.Version, v.Release, shortRepo(v.RepoID))
	}
	load := make([]string, len(st.LoadAvg))
	for i, l := range st.LoadAvg {
		load[i] = fmt.Sprintf("%.2f", l)
	}
	fmt.Fprintf(&b, "Version:      Proxmox Backup Server %s\n", ver)
	fmt.Fprintf(&b, "Kernel:       %s\n", firstNonEmpty(st.Kernel.Release, st.KVersion))
	fmt.Fprintf(&b, "Uptime:       %s\n", humanDuration(st.Uptime))
	fmt.Fprintf(&b, "Boot:         %s (secure boot: %v)\n", firstNonEmpty(st.BootInfo.Mode, "?"), aptrepo.Truthy(st.BootInfo.SecureBoot))
	fmt.Fprintf(&b, "CPU:          %s, %d socket(s), %d threads\n", st.CPUInfo.Model, st.CPUInfo.Sockets, st.CPUInfo.CPUs)
	fmt.Fprintf(&b, "CPU usage:    %.1f%%, IO wait %.1f%%, load %s\n", st.CPU*100, st.Wait*100, strings.Join(load, " "))
	fmt.Fprintf(&b, "Memory:       %s used of %s (%.1f%%)\n", humanBytes(st.Memory.Used), humanBytes(st.Memory.Total), pct(st.Memory.Used, st.Memory.Total))
	fmt.Fprintf(&b, "Swap:         %s used of %s\n", humanBytes(st.Swap.Used), humanBytes(st.Swap.Total))
	fmt.Fprintf(&b, "Root FS:      %s used of %s (%.1f%%)\n", humanBytes(st.Root.Used), humanBytes(st.Root.Total), pct(st.Root.Used, st.Root.Total))
	if subErr != nil {
		b.WriteString("Subscription: unavailable\n")
	} else {
		fmt.Fprintf(&b, "Subscription: %s\n", firstNonEmpty(sub.Status, "?"))
	}
	if certErr != nil {
		fmt.Fprintf(&b, "Certificate:  unavailable: %v\n", certErr)
	}
	for _, c := range certs {
		fmt.Fprintf(&b, "Certificate:  %s valid until %s (%s), issuer %s\n", c.Filename, epoch(c.NotAfter), certLeft(c.NotAfter, now), c.Issuer)
		fmt.Fprintf(&b, "              fingerprint %s (PVE clients pin it in storage.cfg)\n", c.Fingerprint)
	}
	return b.String()
}

func certLeft(notAfter int64, now time.Time) string {
	days := (notAfter - now.Unix()) / 86400
	if notAfter <= now.Unix() {
		return "EXPIRED"
	}
	return fmt.Sprintf("%d days left", days)
}

func shortRepo(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func renderDatastores(ds []datastore, now time.Time) string {
	var b strings.Builder
	shared := sharedWith(ds)
	tw := newTable(&b)
	fmt.Fprintln(tw, "STORE\tPATH\tUSED\tTOTAL\tUSE%\tAVAIL\tEST. FULL\tMOUNT\tMAINTENANCE\tLAST GC\tGC STATE\tGC PENDING")
	for _, d := range ds {
		used, total, use, avail, full, mount := "-", "-", "-", "-", "-", "-"
		if u := d.Usage; u != nil {
			used, total, avail = humanBytes(u.Used), humanBytes(u.Total), humanBytes(u.Avail)
			use = fmt.Sprintf("%.1f%%", pct(u.Used, u.Total))
			full = firstNonEmpty(estimatedFull(u.EstimatedFull, now), "not growing")
			mount = firstNonEmpty(u.MountStatus, "-")
		}
		lastGC, gcState, pending := "-", "-", "-"
		if g := d.GC; g != nil {
			lastGC = ago(g.LastRunEnd, now)
			gcState = firstNonEmpty(g.LastRunState, "never run")
			pending = humanBytes(g.PendingBytes)
		} else if d.GCErr != nil {
			gcState = "unavailable"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Config.Name, d.Config.Path, used, total, use, avail, full, mount,
			firstNonEmpty(d.Config.MaintenanceMode, "-"), lastGC, gcState, pending)
	}
	tw.Flush()
	var notes []string
	for _, d := range ds {
		if others := shared[d.Config.Name]; len(others) > 0 && d.Config.Name < others[0] {
			notes = append(notes, fmt.Sprintf("%s shares one filesystem with %s: usage figures are for the whole filesystem", d.Config.Name, strings.Join(others, ", ")))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range notes {
			b.WriteString("  - " + n + "\n")
		}
	}
	return b.String()
}

func renderDatastore(d datastore, groups []group, gErr error, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Datastore:    %s\n", d.Config.Name)
	fmt.Fprintf(&b, "Path:         %s\n", d.Config.Path)
	if d.Config.Comment != "" {
		fmt.Fprintf(&b, "Comment:      %s\n", d.Config.Comment)
	}
	fmt.Fprintf(&b, "Maintenance:  %s\n", firstNonEmpty(d.Config.MaintenanceMode, "none"))
	if u := d.Usage; u != nil {
		fmt.Fprintf(&b, "Usage:        %s used of %s (%.1f%%), %s available, mount %s\n", humanBytes(u.Used), humanBytes(u.Total), pct(u.Used, u.Total), humanBytes(u.Avail), firstNonEmpty(u.MountStatus, "-"))
		fmt.Fprintf(&b, "Est. full:    %s\n", firstNonEmpty(estimatedFull(u.EstimatedFull, now), "no estimate (usage not growing)"))
	}
	switch g := d.GC; {
	case g != nil:
		fmt.Fprintf(&b, "\nGarbage collection (schedule %s):\n", firstNonEmpty(g.Schedule, d.Config.GCSchedule, "none"))
		fmt.Fprintf(&b, "  last run:   %s, state %s, duration %s\n", ago(g.LastRunEnd, now), firstNonEmpty(g.LastRunState, "never run"), humanDuration(g.Duration))
		fmt.Fprintf(&b, "  next run:   %s\n", epoch(g.NextRun))
		fmt.Fprintf(&b, "  on disk:    %s (deduplicated) for %s of index data", humanBytes(g.DiskBytes), humanBytes(g.IndexDataBytes))
		if g.DiskBytes > 0 && g.IndexDataBytes > 0 {
			fmt.Fprintf(&b, ", deduplication factor %.1f", float64(g.IndexDataBytes)/float64(g.DiskBytes))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "  pending:    %s (freed by the next run), removed last run %s\n", humanBytes(g.PendingBytes), humanBytes(g.RemovedBytes))
		if g.StillBad > 0 || g.RemovedBad > 0 {
			fmt.Fprintf(&b, "  BAD CHUNKS: %d still bad, %d removed: run a verify job\n", g.StillBad, g.RemovedBad)
		}
	case d.GCErr != nil:
		fmt.Fprintf(&b, "\nGarbage collection: unavailable: %v\n", d.GCErr)
	}

	b.WriteString("\nBackup groups:\n")
	if gErr != nil {
		fmt.Fprintf(&b, "  unavailable: %v\n", gErr)
		return b.String()
	}
	if len(groups) == 0 {
		b.WriteString("  none in any namespace (empty datastore)\n")
		return b.String()
	}
	types := map[string]int{}
	byNS := map[string]int{}
	var snapshots, newest int64
	oldest := int64(1<<62 - 1)
	var stale []group
	for _, g := range groups {
		types[g.BackupType]++
		byNS[nsName(g.NS)]++
		snapshots += g.BackupCount
		if g.LastBackup > newest {
			newest = g.LastBackup
		}
		if g.LastBackup < oldest {
			oldest = g.LastBackup
		}
		if now.Unix()-g.LastBackup > int64(staleBackup.Seconds()) {
			stale = append(stale, g)
		}
	}
	var typeParts []string
	for _, t := range []string{"vm", "ct", "host"} {
		if types[t] > 0 {
			typeParts = append(typeParts, fmt.Sprintf("%d %s", types[t], t))
		}
	}
	var nsParts []string
	for _, n := range sortedKeys(byNS) {
		nsParts = append(nsParts, fmt.Sprintf("%s: %d", n, byNS[n]))
	}
	fmt.Fprintf(&b, "  %d group(s) (%s), %d snapshot(s)\n", len(groups), strings.Join(typeParts, ", "), snapshots)
	fmt.Fprintf(&b, "  by namespace: %s\n", strings.Join(nsParts, ", "))
	fmt.Fprintf(&b, "  newest backup %s, oldest group's last backup %s\n", ago(newest, now), ago(oldest, now))
	if len(stale) == 0 {
		b.WriteString("  all groups have a backup from the last 48h\n")
		return b.String()
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].LastBackup < stale[j].LastBackup })
	fmt.Fprintf(&b, "  %d group(s) without a backup in the last 48h (removed guests or failing jobs):\n", len(stale))
	for i, g := range stale {
		if i == 20 {
			fmt.Fprintf(&b, "    ... %d more\n", len(stale)-20)
			break
		}
		fmt.Fprintf(&b, "    %s %s/%s: last backup %s (%s), %d snapshot(s)\n", nsName(g.NS), g.BackupType, g.BackupID, epoch(g.LastBackup), ago(g.LastBackup, now), g.BackupCount)
	}
	return b.String()
}

func renderJobs(js jobSet, now time.Time) string {
	var b strings.Builder
	section := func(title string, jobs []job, err error, remote bool) {
		fmt.Fprintf(&b, "%s:\n", title)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "  unavailable: %v\n", err)
			return
		case len(jobs) == 0:
			b.WriteString("  none configured\n")
			return
		}
		tw := newTable(&b)
		head := "  ID\tSTORE\tSCHEDULE\tLAST RUN\tLAST STATE\tNEXT RUN"
		if title == "Prune jobs" {
			head += "\tRETENTION"
		}
		if remote {
			head += "\tREMOTE"
		}
		fmt.Fprintln(tw, head)
		for _, j := range jobs {
			next := epoch(j.NextRun)
			if j.overdue(now) {
				next += " (OVERDUE)"
			}
			line := fmt.Sprintf("  %s\t%s\t%s\t%s\t%s\t%s", j.ID, j.Store, firstNonEmpty(j.Schedule, "-"), ago(j.LastRunEnd, now), firstNonEmpty(j.LastRunState, "-"), next)
			if title == "Prune jobs" {
				line += "\t" + firstNonEmpty(j.Retention, "?")
			}
			if remote {
				remoteCol := "-"
				if j.Remote != "" {
					remoteCol = j.Remote + ":" + j.RemoteStore
				}
				line += "\t" + remoteCol
			}
			fmt.Fprintln(tw, line)
		}
		tw.Flush()
	}
	section("Prune jobs", js.Prune, js.PruneErr, false)
	section("Verify jobs", js.Verify, js.VerifyErr, false)
	section("Sync jobs", js.Sync, js.SyncErr, true)
	section("Tape backup jobs", js.Tape, js.TErr, false)
	return b.String()
}

func renderTasks(tasks []task) string {
	if len(tasks) == 0 {
		return "No tasks match the filter.\n"
	}
	var b strings.Builder
	tw := newTable(&b)
	fmt.Fprintln(tw, "START\tDURATION\tTYPE\tID\tUSER\tSTATUS\tUPID")
	for _, t := range tasks {
		dur, status := "running", firstNonEmpty(t.Status, "running")
		if t.EndTime > 0 {
			dur = humanDuration(t.EndTime - t.StartTime)
			if t.EndTime == t.StartTime {
				dur = "<1s"
			}
		}
		id := "-"
		if t.WorkerID != nil && *t.WorkerID != "" {
			id = *t.WorkerID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", epoch(t.StartTime), dur, t.WorkerType, id, t.User, status, t.UPID)
	}
	tw.Flush()
	return fmt.Sprintf("%d task(s), newest first:\n\n%s", len(tasks), b.String())
}

func renderTaskLog(st task, log []logLine, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task:     %s %s by %s\n", st.Type, st.ID, st.User)
	fmt.Fprintf(&b, "Started:  %s\n", epoch(st.StartTime))
	status := firstNonEmpty(st.Status, "?")
	if st.ExitStatus != "" {
		status += ", result: " + st.ExitStatus
	}
	fmt.Fprintf(&b, "Status:   %s\n", status)
	start := 0
	if len(log) > lines {
		start = len(log) - lines
		fmt.Fprintf(&b, "\n[showing the last %d of %d log lines]\n", lines, len(log))
	}
	b.WriteString("\n")
	for _, l := range log[start:] {
		b.WriteString(l.T + "\n")
	}
	return b.String()
}
