package proxmox

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/aptrepo"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Storage usage thresholds, as for local filesystems.
const storageWarnPct, storageCritPct = 85, 95

// Services checked by health; failure of the first group makes the node
// unmanageable (CRIT), of the rest degrades it (WARN).
var (
	critServices = []string{"pve-cluster", "pvedaemon", "pveproxy"}
	warnServices = []string{"pvestatd", "pvescheduler"}
)

// Health implements registry.Module. API checks run concurrently (each
// pvesh call costs about a second) and are skipped when pmxcfs is down,
// because pvesh would only hang until its timeout.
func (m *Module) Health(ctx context.Context, env *registry.Env) []registry.Check {
	checks := []registry.Check{registry.VersionCheck(m.Version(ctx, env)), servicesCheck(ctx, env)}
	pmx := pmxcfsCheck()
	checks = append(checks, pmx)
	names := []string{"firewall", "cluster", "guests", "storage", "tasks", "updates"}
	if pmx.Status != registry.OK {
		for _, n := range names {
			checks = append(checks, registry.Check{Name: n, Status: registry.Unknown, Detail: "skipped: pmxcfs (/etc/pve) unavailable"})
		}
		return label(checks)
	}

	node, err := localNode()
	if err != nil {
		return label(append(checks, registry.Check{Name: "api", Status: registry.Unknown, Detail: err.Error()}))
	}
	var (
		wg        sync.WaitGroup
		res       []resource
		status    []clusterEntry
		tasks     []task
		ups       []aptUpdate
		repos     repoInfo
		repoErr   error
		resErr    error
		statusErr error
		taskErr   error
		upErr     error
	)
	since := strconv.FormatInt(time.Now().Add(-24*time.Hour).Unix(), 10)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { resErr = pvesh(ctx, env, &res, "/cluster/resources") })
	run(func() { statusErr = pvesh(ctx, env, &status, "/cluster/status") })
	run(func() {
		taskErr = pvesh(ctx, env, &tasks, "/nodes/"+node+"/tasks", "--errors", "1", "--since", since, "--limit", "200")
	})
	run(func() { upErr = pvesh(ctx, env, &ups, "/nodes/"+node+"/apt/update") })
	run(func() { repoErr = pvesh(ctx, env, &repos, "/nodes/"+node+"/apt/repositories") })
	wg.Wait()

	checks = append(checks,
		firewallState(ctx, env, node).check(),
		orUnknown("cluster", statusErr, func() registry.Check { return evalCluster(status, corosyncConfigured()) }),
		orUnknown("guests", resErr, func() registry.Check { return evalGuests(res) }),
		orUnknown("storage", resErr, func() registry.Check { return evalStorage(res) }),
		orUnknown("tasks", taskErr, func() registry.Check { return evalTasks(tasks) }),
		orUnknown("updates", upErr, func() registry.Check { return evalUpdates(ups, repos, repoErr) }),
	)
	return label(checks)
}

// Version implements registry.Versioned: pve-manager from the dpkg database
// (no Perl process is started).
func (*Module) Version(context.Context, *registry.Env) compat.Result {
	return compat.Assess(compat.PVE, compat.PackageVersion("pve-manager"))
}

func label(checks []registry.Check) []registry.Check {
	for i := range checks {
		checks[i].Module = Name
	}
	return checks
}

func orUnknown(name string, err error, eval func() registry.Check) registry.Check {
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		if strings.Contains(err.Error(), "TIMEOUT") {
			first = "API call timed out (pve-cluster/pvedaemon may be hung)"
		}
		return registry.Check{Name: name, Status: registry.Unknown, Detail: first}
	}
	c := eval()
	c.Name = name
	return c
}

func servicesCheck(ctx context.Context, env *registry.Env) registry.Check {
	c := registry.Check{Name: "services", Status: registry.OK}
	all := append(append([]string{}, critServices...), warnServices...)
	r := env.Exec.Run(ctx, "systemctl", append([]string{"is-active"}, all...)...)
	states := strings.Fields(r.Stdout)
	if r.NotFound || r.TimedOut || len(states) != len(all) {
		c.Status, c.Detail = registry.Unknown, "cannot query service states"
		return c
	}
	var bad []string
	for i, svc := range all {
		if states[i] == "active" {
			continue
		}
		bad = append(bad, svc+"="+states[i])
		if i < len(critServices) {
			c.Status = registry.Crit
		} else if c.Status == registry.OK {
			c.Status = registry.Warn
		}
	}
	if len(bad) == 0 {
		c.Detail = "active: " + strings.Join(all, ", ")
	} else {
		c.Detail = "not active: " + strings.Join(bad, ", ")
	}
	return c
}

// pmxcfsCheck verifies that the cluster filesystem is mounted in the host's
// mount namespace (PID 1; the service itself runs in a sandbox).
func pmxcfsCheck() registry.Check {
	c := registry.Check{Name: "pmxcfs"}
	data, err := os.ReadFile("/proc/1/mountinfo")
	if err != nil {
		c.Status, c.Detail = registry.Unknown, "cannot read /proc/1/mountinfo: "+err.Error()
		return c
	}
	if !pmxcfsMounted(string(data)) {
		c.Status, c.Detail = registry.Crit, "/etc/pve is not mounted: pve-cluster (pmxcfs) is down, configuration and API are unavailable"
		return c
	}
	c.Status, c.Detail = registry.OK, "/etc/pve mounted (pmxcfs)"
	return c
}

// pmxcfsMounted parses mountinfo: "... <mountpoint> ... - <fstype> <source> ...".
func pmxcfsMounted(mountinfo string) bool {
	for _, l := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 || f[4] != "/etc/pve" {
			continue
		}
		for i, x := range f {
			if x == "-" && i+1 < len(f) && strings.HasPrefix(f[i+1], "fuse") {
				return true
			}
		}
	}
	return false
}

func evalCluster(status []clusterEntry, clustered bool) registry.Check {
	c := registry.Check{Status: registry.OK}
	var cl *clusterEntry
	var offline []string
	nodes := 0
	for i := range status {
		switch status[i].Type {
		case "cluster":
			cl = &status[i]
		case "node":
			nodes++
			if status[i].Online != 1 {
				offline = append(offline, status[i].Name)
			}
		}
	}
	switch {
	case cl == nil && clustered:
		c.Status, c.Detail = registry.Crit, "corosync.conf present but the cluster is not visible in /cluster/status"
	case cl == nil:
		c.Detail = "standalone node (no cluster)"
		if len(offline) > 0 {
			c.Status, c.Detail = registry.Crit, "node reported offline"
		}
	case cl.Quorate != 1:
		c.Status, c.Detail = registry.Crit, fmt.Sprintf("cluster %q NOT quorate (%d of %d nodes online): /etc/pve is read-only, guests cannot be started", cl.Name, nodes-len(offline), nodes)
	case len(offline) > 0:
		c.Status, c.Detail = registry.Warn, fmt.Sprintf("cluster %q quorate, offline node(s): %s", cl.Name, strings.Join(offline, ", "))
	default:
		c.Detail = fmt.Sprintf("cluster %q quorate, %d/%d nodes online", cl.Name, nodes, nodes)
	}
	return c
}

func evalGuests(res []resource) registry.Check {
	c := registry.Check{Status: registry.OK}
	counts := map[string]int{}
	templates := 0
	var odd []string
	for _, r := range res {
		if r.Type != "qemu" && r.Type != "lxc" {
			continue
		}
		if r.Template == 1 {
			templates++
			continue
		}
		counts[r.Status]++
		if r.Status != "running" && r.Status != "stopped" {
			odd = append(odd, fmt.Sprintf("%d(%s)", r.VMID, r.Status))
		}
		if r.HAState == "error" || r.HAState == "fence" {
			odd = append(odd, fmt.Sprintf("%d(HA %s)", r.VMID, r.HAState))
		}
	}
	c.Detail = fmt.Sprintf("%d running, %d stopped, %d template(s)", counts["running"], counts["stopped"], templates)
	if len(odd) > 0 {
		sort.Strings(odd)
		c.Status = registry.Warn
		c.Detail += "; unusual state: " + strings.Join(odd, ", ")
	}
	return c
}

func evalStorage(res []resource) registry.Check {
	c := registry.Check{Status: registry.OK}
	var problems []string
	maxPct, maxName, n := -1.0, "", 0
	for _, r := range res {
		if r.Type != "storage" {
			continue
		}
		n++
		name := r.Storage
		if r.Node != "" {
			name += "@" + r.Node
		}
		if r.Status != "available" {
			c.Status = registry.Crit
			problems = append(problems, fmt.Sprintf("%s %s", name, firstNonEmpty(r.Status, "unknown")))
			continue
		}
		if r.MaxDisk <= 0 {
			continue
		}
		p := pct(r.Disk, r.MaxDisk)
		if p > maxPct {
			maxPct, maxName = p, name
		}
		switch {
		case p >= storageCritPct:
			c.Status = registry.Crit
			problems = append(problems, fmt.Sprintf("%s %.0f%%", name, p))
		case p >= storageWarnPct:
			if c.Status == registry.OK {
				c.Status = registry.Warn
			}
			problems = append(problems, fmt.Sprintf("%s %.0f%%", name, p))
		}
	}
	switch {
	case len(problems) > 0:
		c.Detail = strings.Join(problems, ", ")
	case n == 0:
		c.Detail = "no storage reported"
	default:
		c.Detail = fmt.Sprintf("%d storage(s) available, highest usage %.0f%% (%s)", n, maxPct, maxName)
	}
	return c
}

func evalTasks(tasks []task) registry.Check {
	c := registry.Check{Status: registry.OK}
	byType := map[string]int{}
	failed := 0
	for _, t := range tasks {
		if !t.failed() {
			continue
		}
		failed++
		byType[t.Type]++
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
	c.Detail = fmt.Sprintf("%d failed task(s) in the last 24h: %s (see proxmox_tasks errors_only)", failed, strings.Join(parts, ", "))
	return c
}

func evalUpdates(ups []aptUpdate, repos repoInfo, repoErr error) registry.Check {
	return aptrepo.Eval(ups, repos, repoErr, "Proxmox VE")
}
