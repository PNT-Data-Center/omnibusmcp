// Package proxmox is the Proxmox VE module: node, cluster, guests, storage,
// tasks, updates and backups, read through the local API (pvesh). All tools
// are read-only (tier 1).
package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/aptrepo"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Name is the module identifier used in configuration.
const Name = "proxmox"

// Module implements registry.Module.
type Module struct{}

// New returns the Proxmox VE module.
func New() *Module { return &Module{} }

func (*Module) Name() string { return Name }

func (*Module) Description() string {
	return "Proxmox VE: node, cluster/quorum, VMs and containers, storage, tasks, updates, backups"
}

// Detect reports a Proxmox VE host: the pmxcfs mount point and pvesh exist.
func (*Module) Detect(context.Context, *registry.Env) bool {
	st, err := os.Stat("/etc/pve")
	if err != nil || !st.IsDir() {
		return false
	}
	_, err = exec.LookPath("pvesh")
	if err != nil {
		_, err = os.Stat("/usr/bin/pvesh")
	}
	return err == nil
}

type noInput struct{}

type guestsInput struct {
	Type   string `json:"type,omitempty" jsonschema:"filter: qemu (VMs), lxc (containers) or all (default)"`
	Status string `json:"status,omitempty" jsonschema:"filter: running, stopped or all (default)"`
}

type guestInput struct {
	VMID int `json:"vmid" jsonschema:"VM or container ID, e.g. 100"`
}

type tasksInput struct {
	ErrorsOnly bool   `json:"errors_only,omitempty" jsonschema:"only failed tasks"`
	Since      string `json:"since,omitempty" jsonschema:"only tasks started within this period: e.g. 24h, 7d (default: no limit)"`
	Type       string `json:"type,omitempty" jsonschema:"task type filter, e.g. vzdump, qmstart, qmshutdown, aptupdate"`
	VMID       int    `json:"vmid,omitempty" jsonschema:"only tasks of this VM/container"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum number of tasks (default 30, max 200)"`
}

type taskLogInput struct {
	UPID  string `json:"upid" jsonschema:"task UPID as listed by proxmox_tasks"`
	Lines int    `json:"lines,omitempty" jsonschema:"number of last log lines (default 100, max 1000)"`
}

var (
	upidRe      = regexp.MustCompile(`^UPID:[A-Za-z0-9.-]+:[0-9A-Fa-f]{8}:[0-9A-Fa-f]{8,16}:[0-9A-Fa-f]{8}:[A-Za-z0-9_-]+:[^\s:/]*:[^\s:/]+:$`)
	taskTypeRe  = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	sinceRe     = regexp.MustCompile(`^(\d{1,4})(h|d)$`)
	secretKeyRe = regexp.MustCompile(`(?i)pass|secret|token|key$`)
)

// Tools returns the module's tools; all of them are read-only.
func (m *Module) Tools(env *registry.Env) []registry.Tool {
	ro := tier.ReadOnly
	limit := func(s string) string { return truncate(s, env.Cfg.Limits.MaxOutputBytes) }
	return []registry.Tool{
		registry.NewTool("proxmox_node_status", "Proxmox node status",
			"Proxmox VE version, kernel, uptime, CPU model and usage, IO wait, load, memory, swap, root filesystem, KSM, boot mode and PVE firewall state (enabled/disabled, enforced) of this node.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				node, err := localNode()
				if err != nil {
					return "", err
				}
				var st nodeStatus
				if err := pvesh(ctx, env, &st, "/nodes/"+node+"/status"); err != nil {
					return "", err
				}
				return limit(renderNodeStatus(node, st, firewallState(ctx, env, node).check().Detail)), nil
			}),

		registry.NewTool("proxmox_versions", "Proxmox package versions",
			"Versions of all Proxmox VE related packages (pveversion -v): pve-manager, kernel, qemu, lxc, ceph, corosync, zfs...",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				return limit(env.Exec.Run(ctx, "pveversion", "-v").Format()), nil
			}),

		registry.NewTool("proxmox_cluster", "Proxmox cluster and quorum",
			"Cluster membership and quorum: standalone node or cluster, nodes online, quorate, HA manager state; corosync details (pvecm status) when clustered.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				var status []clusterEntry
				if err := pvesh(ctx, env, &status, "/cluster/status"); err != nil {
					return "", err
				}
				var ha []haEntry
				haErr := pvesh(ctx, env, &ha, "/cluster/ha/status/current")
				out := renderCluster(status, ha, haErr, corosyncConfigured())
				if corosyncConfigured() {
					out += "\n" + env.Exec.Run(ctx, "pvecm", "status").Format()
				}
				return limit(out), nil
			}),

		registry.NewTool("proxmox_guests", "Proxmox VMs and containers",
			"All VMs (qemu) and containers (lxc) in the cluster: ID, name, node, status, CPU and memory usage, disk size, uptime, HA state, template flag, tags.",
			ro, true, func(ctx context.Context, in guestsInput) (string, error) {
				if !oneOf(in.Type, "", "all", "qemu", "lxc") {
					return "", fmt.Errorf("invalid type %q (qemu, lxc or all)", in.Type)
				}
				if !oneOf(in.Status, "", "all", "running", "stopped") {
					return "", fmt.Errorf("invalid status %q (running, stopped or all)", in.Status)
				}
				var res []resource
				if err := pvesh(ctx, env, &res, "/cluster/resources", "--type", "vm"); err != nil {
					return "", err
				}
				return limit(renderGuests(res, in.Type, in.Status)), nil
			}),

		registry.NewTool("proxmox_guest", "Proxmox VM/container details",
			"Configuration and current runtime status of one VM or container. Password-like values are redacted, SSH keys are only counted.",
			ro, true, func(ctx context.Context, in guestInput) (string, error) {
				if in.VMID < 1 || in.VMID > 999999999 {
					return "", fmt.Errorf("invalid vmid %d", in.VMID)
				}
				var res []resource
				if err := pvesh(ctx, env, &res, "/cluster/resources", "--type", "vm"); err != nil {
					return "", err
				}
				var g *resource
				for i := range res {
					if res[i].VMID == in.VMID {
						g = &res[i]
					}
				}
				if g == nil {
					return "", fmt.Errorf("no VM or container with ID %d in the cluster", in.VMID)
				}
				base := fmt.Sprintf("/nodes/%s/%s/%d", g.Node, g.Type, g.VMID)
				var cfg map[string]any
				if err := pvesh(ctx, env, &cfg, base+"/config"); err != nil {
					return "", err
				}
				var cur map[string]any
				curErr := pvesh(ctx, env, &cur, base+"/status/current")
				return limit(renderGuest(*g, cfg, cur, curErr)), nil
			}),

		registry.NewTool("proxmox_storage", "Proxmox storage",
			"All storages with type, status, shared flag, content types and usage (including LVM-thin and ZFS pools, as reported by pvestatd).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				var res []resource
				if err := pvesh(ctx, env, &res, "/cluster/resources", "--type", "storage"); err != nil {
					return "", err
				}
				return limit(renderStorage(res)), nil
			}),

		registry.NewTool("proxmox_tasks", "Proxmox tasks",
			"Recent tasks of this node (start/stop, backups, migrations, updates...) with status and UPID. Use errors_only to find failures, then proxmox_task_log for details.",
			ro, true, func(ctx context.Context, in tasksInput) (string, error) {
				node, err := localNode()
				if err != nil {
					return "", err
				}
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
						return "", err
					}
					args = append(args, "--since", strconv.FormatInt(time.Now().Add(-d).Unix(), 10))
				}
				if in.Type != "" {
					if !taskTypeRe.MatchString(in.Type) {
						return "", fmt.Errorf("invalid task type %q", in.Type)
					}
					args = append(args, "--typefilter", in.Type)
				}
				if in.VMID != 0 {
					if in.VMID < 1 || in.VMID > 999999999 {
						return "", fmt.Errorf("invalid vmid %d", in.VMID)
					}
					args = append(args, "--vmid", strconv.Itoa(in.VMID))
				}
				var tasks []task
				if err := pvesh(ctx, env, &tasks, "/nodes/"+node+"/tasks", args...); err != nil {
					return "", err
				}
				return limit(renderTasks(tasks)), nil
			}),

		registry.NewTool("proxmox_task_log", "Proxmox task log",
			"Status and the last lines of the log of one task, identified by its UPID (from proxmox_tasks).",
			ro, true, func(ctx context.Context, in taskLogInput) (string, error) {
				if !upidRe.MatchString(in.UPID) {
					return "", fmt.Errorf("invalid UPID %q", in.UPID)
				}
				node := strings.Split(in.UPID, ":")[1]
				lines := in.Lines
				switch {
				case lines == 0:
					lines = 100
				case lines < 1:
					lines = 1
				case lines > 1000:
					lines = 1000
				}
				base := "/nodes/" + node + "/tasks/" + in.UPID
				var st task
				if err := pvesh(ctx, env, &st, base+"/status"); err != nil {
					return "", err
				}
				var log []logLine
				if err := pvesh(ctx, env, &log, base+"/log", "--limit", "50000"); err != nil {
					return "", err
				}
				return limit(renderTaskLog(st, log, lines)), nil
			}),

		registry.NewTool("proxmox_updates", "Proxmox updates and repositories",
			"Pending package updates (from the last apt update, not refreshed here) and APT repository configuration with Proxmox warnings (e.g. enterprise repository without subscription).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				node, err := localNode()
				if err != nil {
					return "", err
				}
				var ups []aptUpdate
				upErr := pvesh(ctx, env, &ups, "/nodes/"+node+"/apt/update")
				var repos repoInfo
				repoErr := pvesh(ctx, env, &repos, "/nodes/"+node+"/apt/repositories")
				return limit(renderUpdates(ups, upErr, repos, repoErr)), nil
			}),

		registry.NewTool("proxmox_backups", "Proxmox backup jobs",
			"Scheduled backup jobs, guests not covered by any job, and the latest backup (vzdump) tasks of this node.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				node, err := localNode()
				if err != nil {
					return "", err
				}
				var jobs []map[string]any
				jobsErr := pvesh(ctx, env, &jobs, "/cluster/backup")
				var uncovered []resource
				uncErr := pvesh(ctx, env, &uncovered, "/cluster/backup-info/not-backed-up")
				var tasks []task
				taskErr := pvesh(ctx, env, &tasks, "/nodes/"+node+"/tasks", "--typefilter", "vzdump", "--limit", "10")
				return limit(renderBackups(jobs, jobsErr, uncovered, uncErr, tasks, taskErr)), nil
			}),
	}
}

// corosyncConfigured reports whether this node belongs to a cluster.
func corosyncConfigured() bool {
	_, err := os.Stat("/etc/pve/corosync.conf")
	return err == nil
}

func oneOf(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
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

func newTable(b *strings.Builder) *tabwriter.Writer {
	return tabwriter.NewWriter(b, 0, 4, 2, ' ', 0)
}

func renderNodeStatus(node string, st nodeStatus, firewall string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Node:        %s\n", node)
	fmt.Fprintf(&b, "Version:     %s\n", st.PVEVersion)
	fmt.Fprintf(&b, "Kernel:      %s\n", firstNonEmpty(st.Kernel.Release, st.KVersion))
	fmt.Fprintf(&b, "Uptime:      %s\n", humanDuration(st.Uptime))
	fmt.Fprintf(&b, "Boot:        %s (secure boot: %v)\n", firstNonEmpty(st.BootInfo.Mode, "?"), st.BootInfo.SecureBoot == 1)
	fmt.Fprintf(&b, "CPU:         %s, %d socket(s) x %d cores, %d threads\n", st.CPUInfo.Model, st.CPUInfo.Sockets, st.CPUInfo.Cores, st.CPUInfo.CPUs)
	fmt.Fprintf(&b, "CPU usage:   %.1f%%, IO wait %.1f%%, load %s\n", st.CPU*100, st.Wait*100, strings.Join(st.LoadAvg, " "))
	fmt.Fprintf(&b, "Memory:      %s used of %s (%.1f%%)\n", humanBytes(st.Memory.Used), humanBytes(st.Memory.Total), pct(st.Memory.Used, st.Memory.Total))
	fmt.Fprintf(&b, "Swap:        %s used of %s\n", humanBytes(st.Swap.Used), humanBytes(st.Swap.Total))
	fmt.Fprintf(&b, "Root FS:     %s used of %s (%.1f%%)\n", humanBytes(st.RootFS.Used), humanBytes(st.RootFS.Total), pct(st.RootFS.Used, st.RootFS.Total))
	fmt.Fprintf(&b, "KSM shared:  %s\n", humanBytes(st.KSM.Shared))
	fmt.Fprintf(&b, "Firewall:    %s\n", firewall)
	return b.String()
}

func renderCluster(status []clusterEntry, ha []haEntry, haErr error, clustered bool) string {
	var b strings.Builder
	var cl *clusterEntry
	var nodes []clusterEntry
	for i := range status {
		switch status[i].Type {
		case "cluster":
			cl = &status[i]
		case "node":
			nodes = append(nodes, status[i])
		}
	}
	switch {
	case cl != nil:
		fmt.Fprintf(&b, "Mode:     cluster %q, %d node(s), config version %d, quorate: %v\n", cl.Name, cl.Nodes, cl.Version, cl.Quorate == 1)
	case clustered:
		b.WriteString("Mode:     corosync.conf present but no cluster entry in /cluster/status (check pve-cluster/corosync)\n")
	default:
		b.WriteString("Mode:     standalone node (no cluster, no corosync.conf); quorum is not applicable\n")
	}
	online := 0
	for _, n := range nodes {
		if n.Online == 1 {
			online++
		}
	}
	fmt.Fprintf(&b, "Nodes:    %d online of %d\n\n", online, len(nodes))
	tw := newTable(&b)
	fmt.Fprintln(tw, "NODE\tID\tONLINE\tLOCAL\tIP")
	for _, n := range nodes {
		fmt.Fprintf(tw, "%s\t%d\t%v\t%v\t%s\n", n.Name, n.NodeID, n.Online == 1, n.Local == 1, n.IP)
	}
	tw.Flush()
	b.WriteString("\nHA manager:\n")
	if haErr != nil {
		fmt.Fprintf(&b, "  unavailable: %v\n", haErr)
	} else {
		for _, h := range ha {
			line := fmt.Sprintf("  %s: %s", h.ID, h.Status)
			if h.ArmedState != "" {
				line += " (armed-state: " + h.ArmedState + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func guestSort(res []resource) {
	sort.Slice(res, func(i, j int) bool { return res[i].VMID < res[j].VMID })
}

func renderGuests(res []resource, typ, status string) string {
	guestSort(res)
	var b strings.Builder
	counts := map[string]int{}
	tw := newTable(&b)
	fmt.Fprintln(tw, "VMID\tTYPE\tNAME\tNODE\tSTATUS\tCPU\tMEM USED/MAX\tDISK\tUPTIME\tHA\tTEMPLATE\tTAGS")
	shown := 0
	for _, r := range res {
		if r.Type != "qemu" && r.Type != "lxc" {
			continue
		}
		counts[r.Type+"/"+r.Status]++
		if typ != "" && typ != "all" && r.Type != typ {
			continue
		}
		if status != "" && status != "all" && r.Status != status {
			continue
		}
		shown++
		cpu := "-"
		if r.Status == "running" {
			cpu = fmt.Sprintf("%.1f%% of %g", r.CPU*100, r.MaxCPU)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s / %s\t%s\t%s\t%s\t%s\t%s\n",
			r.VMID, r.Type, r.Name, r.Node, r.Status, cpu, humanBytes(r.Mem), humanBytes(r.MaxMem),
			humanBytes(r.MaxDisk), humanDuration(r.Uptime), firstNonEmpty(r.HAState, "-"), yesNo(r.Template == 1), firstNonEmpty(strings.TrimSpace(r.Tags), "-"))
	}
	tw.Flush()
	var keys []string
	for k := range counts {
		keys = append(keys, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	sort.Strings(keys)
	return fmt.Sprintf("Guests: %d shown; totals by type/status: %s\n\n%s", shown, strings.Join(keys, ", "), b.String())
}

func renderGuest(g resource, cfg, cur map[string]any, curErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d %q on node %s: %s\n", g.Type, g.VMID, g.Name, g.Node, g.Status)
	if curErr != nil {
		fmt.Fprintf(&b, "runtime status unavailable: %v\n", curErr)
	} else if cur != nil {
		b.WriteString("\nRuntime:\n")
		for _, k := range []string{"status", "qmpstatus", "uptime", "cpus", "cpu", "mem", "maxmem", "maxdisk", "pid", "running-qemu", "running-machine", "agent", "ha"} {
			v, ok := cur[k]
			if !ok {
				continue
			}
			switch k {
			case "uptime":
				v = humanDuration(int64(num(v)))
			case "mem", "maxmem", "maxdisk":
				v = humanBytes(int64(num(v)))
			case "cpu":
				v = fmt.Sprintf("%.1f%%", num(v)*100)
			}
			fmt.Fprintf(&b, "  %-16s %v\n", k+":", v)
		}
	}
	b.WriteString("\nConfiguration:\n")
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		if k != "digest" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.TrimRight(configValue(k, cfg[k]), "\n")
		fmt.Fprintf(&b, "  %s: %s\n", k, strings.ReplaceAll(v, "\n", "\n      | "))
	}
	return b.String()
}

// configValue redacts secrets and condenses SSH keys.
func configValue(key string, v any) string {
	s := fmt.Sprint(v)
	switch {
	case key == "sshkeys":
		dec, err := url.QueryUnescape(s)
		if err != nil {
			dec = s
		}
		n := 0
		for _, l := range strings.Split(dec, "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		return fmt.Sprintf("(%d public key(s), not shown)", n)
	case secretKeyRe.MatchString(key):
		return "*** (redacted)"
	}
	return s
}

func renderStorage(res []resource) string {
	sort.Slice(res, func(i, j int) bool {
		if res[i].Storage != res[j].Storage {
			return res[i].Storage < res[j].Storage
		}
		return res[i].Node < res[j].Node
	})
	var b strings.Builder
	tw := newTable(&b)
	fmt.Fprintln(tw, "STORAGE\tNODE\tTYPE\tSTATUS\tSHARED\tUSED\tTOTAL\tUSE%\tCONTENT")
	for _, r := range res {
		if r.Type != "storage" {
			continue
		}
		use := "-"
		if r.MaxDisk > 0 {
			use = fmt.Sprintf("%.1f%%", pct(r.Disk, r.MaxDisk))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Storage, r.Node, r.PluginType, r.Status,
			yesNo(r.Shared == 1), humanBytes(r.Disk), humanBytes(r.MaxDisk), use, r.Content)
	}
	tw.Flush()
	b.WriteString("\nUsage is reported by pvestatd (for LVM-thin: data usage of the thin pool). Storages not in storage.cfg are not listed.\n")
	return b.String()
}

func renderTasks(tasks []task) string {
	var b strings.Builder
	if len(tasks) == 0 {
		return "No tasks match the filter.\n"
	}
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", epoch(t.StartTime), dur, t.Type, firstNonEmpty(t.ID, "-"), t.User, status, t.UPID)
	}
	tw.Flush()
	return fmt.Sprintf("%d task(s), newest first:\n\n%s", len(tasks), b.String())
}

func renderTaskLog(st task, log []logLine, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task:     %s %s by %s on %s\n", st.Type, firstNonEmpty(st.ID, ""), st.User, st.Node)
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

// APT update/repository handling is shared with the PBS module.
type repoInfo = aptrepo.Repos

func renderUpdates(ups []aptUpdate, upErr error, repos repoInfo, repoErr error) string {
	return aptrepo.Render(ups, upErr, repos, repoErr, "Proxmox VE")
}

func renderBackups(jobs []map[string]any, jobsErr error, uncovered []resource, uncErr error, tasks []task, taskErr error) string {
	var b strings.Builder
	b.WriteString("Backup jobs:\n")
	switch {
	case jobsErr != nil:
		fmt.Fprintf(&b, "  unavailable: %v\n", jobsErr)
	case len(jobs) == 0:
		b.WriteString("  none configured\n")
	default:
		for _, j := range jobs {
			fmt.Fprintf(&b, "  %v: enabled=%v schedule=%v storage=%v mode=%v guests=%v%v\n",
				j["id"], truthy(j["enabled"]), j["schedule"], j["storage"], firstNonNil(j["mode"], "-"),
				firstNonNil(j["vmid"], map[bool]string{true: "all", false: "-"}[truthy(j["all"])]), optional(" comment=", j["comment"]))
		}
	}
	b.WriteString("\nGuests not covered by any backup job:\n")
	switch {
	case uncErr != nil:
		fmt.Fprintf(&b, "  unavailable: %v\n", uncErr)
	case len(uncovered) == 0:
		b.WriteString("  none\n")
	default:
		guestSort(uncovered)
		for _, g := range uncovered {
			fmt.Fprintf(&b, "  %d %s (%s)\n", g.VMID, g.Name, g.Type)
		}
	}
	b.WriteString("\nLatest backup (vzdump) tasks on this node:\n")
	switch {
	case taskErr != nil:
		fmt.Fprintf(&b, "  unavailable: %v\n", taskErr)
	case len(tasks) == 0:
		b.WriteString("  none\n")
	default:
		b.WriteString(renderTasks(tasks))
	}
	return b.String()
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	}
	return false
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonNil(v any, def string) any {
	if v == nil || v == "" {
		return def
	}
	return v
}

func optional(prefix string, v any) string {
	if v == nil || v == "" {
		return ""
	}
	return prefix + fmt.Sprint(v)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
