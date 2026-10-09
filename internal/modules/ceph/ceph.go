// Package ceph is the Ceph module. It works on every host of a cluster,
// whether installed with cephadm (containers) or packages (Proxmox VE
// pveceph, distribution packages):
//
//   - cluster view: health, monitors, OSDs, pools, PGs... queried with the
//     read-only key client.omnibusmcp (mon 'allow r' mgr 'allow r');
//   - local view, always: this host's daemons, OSD devices, unposted crash
//     reports and logs.
//
// A host without the key (or with ceph.cluster: false) offers the local
// view only. All tools are read-only (tier 1).
package ceph

import (
	"context"
	"os/exec"
	"regexp"
	"sort"
	"sync"

	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Name is the module identifier used in configuration.
const Name = "ceph"

// Module implements registry.Module.
type Module struct {
	once sync.Once
	s    site
}

// New returns the Ceph module.
func New() *Module { return &Module{} }

func (*Module) Name() string { return Name }

func (*Module) Description() string {
	return "Ceph (cephadm or packages, e.g. Proxmox VE): cluster health, monitors, OSDs, PGs; this host's daemons, devices, crashes"
}

// Detect reports a host with Ceph daemons or a Proxmox VE Ceph cluster
// configuration; client packages alone do not count.
func (*Module) Detect(context.Context, *registry.Env) bool { return installed() }

// site discovers the installation once: the tools offered depend on it,
// so a key added later takes effect after a restart.
func (m *Module) site(env *registry.Env) site {
	m.once.Do(func() {
		m.s = discover(env.Cfg.Ceph, func(name string) error { _, err := exec.LookPath(name); return err })
	})
	return m.s
}

type noInput struct{}

// Tools returns the module's tools: the cluster tools only when this host
// can query the cluster.
func (m *Module) Tools(env *registry.Env) []registry.Tool {
	ro := tier.ReadOnly
	limit := func(s string) string { return truncate(s, env.Cfg.Limits.MaxOutputBytes) }
	tools := []registry.Tool{
		registry.NewTool("ceph_local", "Ceph on this host",
			"This host's part of the Ceph cluster, without querying the cluster: installation (cephadm or packages), Ceph daemons and their systemd state (failed units, stale units of moved daemons), OSD to disk mapping, crash reports not yet sent to the cluster, and whether the cluster view is available.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				out, err := m.localTool(ctx, env)
				return limit(out), err
			}),
		registry.NewTool("ceph_logs", "Ceph logs on this host",
			"Logs kept on this host, from files (package installations) or the journal (cephadm): source cluster (the cluster log; only on hosts with a monitor), audit (commands run against the cluster; secret values masked), or a daemon of this host (osd.3, mon.host1...; list: ceph_local). Default level warn shows warnings and errors only; also error or all. Optional grep text, lines.",
			ro, true, func(ctx context.Context, in logsInput) (string, error) {
				out, err := m.logsTool(ctx, env, in)
				return limit(out), err
			}),
	}
	if !m.site(env).Cluster {
		return tools
	}
	return append(tools,
		registry.NewTool("ceph_status", "Ceph cluster status",
			"Cluster health with every active check and its details, monitors and quorum (members out of quorum named), managers, OSD counts (down/out), MDS, capacity, PG states, recovery and client IO, operations in progress. Start here when diagnosing Ceph.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				out, err := m.statusTool(ctx, env)
				return limit(out), err
			}),
		registry.NewTool("ceph_osds", "Ceph OSDs",
			"OSD summary (up/in, average usage, spread), the cluster's own fullness thresholds and unusual OSD flags (noout, norebalance...), usage per host, then OSDs with problems: down, out, nearfull/backfillfull/full by the cluster thresholds; and the OSDs with the highest latency. Filters: host, state (problems, all, down, out, nearfull), limit.",
			ro, true, func(ctx context.Context, in osdsInput) (string, error) {
				out, err := m.osdsTool(ctx, env, in)
				return limit(out), err
			}),
		registry.NewTool("ceph_pools", "Ceph pools",
			"Pools: replicated size/min_size or erasure profile, PG count (and autoscaler target), application, stored and used bytes, use percentage, available space, objects; notes on risky settings (min_size 1), quotas nearly used up and pools without free space. Filter: pool.",
			ro, true, func(ctx context.Context, in poolsInput) (string, error) {
				out, err := m.poolsTool(ctx, env, in)
				return limit(out), err
			}),
		registry.NewTool("ceph_pgs", "Ceph placement groups",
			"PG counts by state, stuck PGs (inactive, unclean, stale, undersized, degraded) with their up/acting OSDs and last clean time, and OSDs that block peering.",
			ro, true, func(ctx context.Context, in pgsInput) (string, error) {
				out, err := m.pgsTool(ctx, env, in)
				return limit(out), err
			}),
		registry.NewTool("ceph_daemons", "Ceph daemons",
			"Ceph daemon versions per type (mixed versions flagged) and daemon state. With the cephadm orchestrator: daemons not running (or all) with host, status, version, start time; filters type, host, state, limit. Without an orchestrator (e.g. Proxmox VE): monitors, managers, OSDs and MDS from the cluster maps; use ceph_local on a host for its daemons.",
			ro, true, func(ctx context.Context, in daemonsInput) (string, error) {
				out, err := m.daemonsTool(ctx, env, in)
				return limit(out), err
			}),
		registry.NewTool("ceph_crashes", "Ceph crashes",
			"Daemon crashes reported to the cluster (newest first, archived or not; new: only not archived), and crash reports this host never sent. With id: entity, version, assertion, stack signature and backtrace of one crash.",
			ro, true, func(ctx context.Context, in crashesInput) (string, error) {
				out, err := m.crashesTool(ctx, env, in)
				return limit(out), err
			}),
		registry.NewTool("ceph_cephfs", "CephFS",
			"CephFS file systems: pools, client count, MDS daemons with state (active, standby, replay...), rank, request rate, inodes and caps; no active MDS means the file system is unavailable.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				out, err := m.cephfsTool(ctx, env)
				return limit(out), err
			}),
	)
}

var versionRe = regexp.MustCompile(`ceph version (\d+\.\d+\.\d+)`)

// Version implements registry.Versioned: the version most daemons of the
// cluster run, or the installed packages in the local view.
func (m *Module) Version(ctx context.Context, env *registry.Env) compat.Result {
	s := m.site(env)
	if s.Cluster {
		var vs struct {
			Overall map[string]int `json:"overall"`
		}
		if query(ctx, env, s, &vs, "versions") == nil {
			if v := dominantVersion(vs.Overall); v != "" {
				return compat.Assess(compat.Ceph, v)
			}
		}
	}
	for _, pkg := range []string{"ceph-base", "ceph-common"} {
		if v := compat.PackageVersion(pkg); v != "" {
			return compat.Assess(compat.Ceph, v)
		}
	}
	r := env.Exec.Run(ctx, "ceph", "--version")
	if mm := versionRe.FindStringSubmatch(r.Stdout); mm != nil {
		return compat.Assess(compat.Ceph, mm[1])
	}
	return compat.Assess(compat.Ceph, "")
}

// dominantVersion picks the version run by most daemons.
func dominantVersion(overall map[string]int) string {
	keys := sortedKeys(overall)
	sort.SliceStable(keys, func(i, j int) bool { return overall[keys[i]] > overall[keys[j]] })
	for _, k := range keys {
		if mm := versionRe.FindStringSubmatch(k); mm != nil {
			return mm[1]
		}
	}
	return ""
}

// truncate bounds tool output to max bytes on a line boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := lastNewline(cut); i > 0 {
		cut = cut[:i+1]
	}
	return cut + "... [output truncated: use filters or a lower limit]\n"
}

func lastNewline(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}
