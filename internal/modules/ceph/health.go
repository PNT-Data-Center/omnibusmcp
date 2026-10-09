package ceph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// clusterChecks are skipped together when the cluster cannot be queried.
var clusterChecks = []string{"health", "quorum", "osds", "pgs"}

// Health implements registry.Module. One "ceph status" serves as the
// connection check: when it fails, the other cluster checks are skipped at
// once instead of each waiting for the monitors.
func (m *Module) Health(ctx context.Context, env *registry.Env) []registry.Check {
	s := m.site(env)
	checks := []registry.Check{registry.VersionCheck(m.Version(ctx, env))}

	var orch []orchDaemon
	switch {
	case !s.Cluster:
		checks = append(checks, registry.Check{Name: "cluster", Status: registry.OK, Detail: "local view only: " + s.LocalReason})
	default:
		var st clusterStatus
		if err := query(ctx, env, s, &st, "status"); err != nil {
			checks = append(checks, connectionCheck(err))
			for _, n := range clusterChecks {
				checks = append(checks, registry.Check{Name: n, Status: registry.Unknown, Detail: "skipped: the cluster cannot be queried"})
			}
		} else {
			checks = append(checks, registry.Check{Name: "connection", Status: registry.OK, Detail: "cluster " + st.FSID + " answers"},
				evalHealth(st.Health), evalQuorum(st), evalOSDs(st), evalPGs(st))
			if s.Variant == VariantCephadm {
				query(ctx, env, s, &orch, "orch", "ps")
			}
		}
	}

	units, err := localUnits(ctx, env)
	if err != nil {
		checks = append(checks, registry.Check{Name: "daemons", Status: registry.Unknown, Detail: err.Error()})
	} else {
		if len(orch) > 0 {
			markStale(units, orch)
		}
		checks = append(checks, evalDaemons(units))
	}
	checks = append(checks, evalCrashes(localCrashes(s), time.Now()))
	for i := range checks {
		checks[i].Module = Name
	}
	return checks
}

// connectionCheck rates a failed query: unreachable monitors are critical,
// a key or configuration problem on this host is a warning.
func connectionCheck(err error) registry.Check {
	c := registry.Check{Name: "connection", Detail: err.Error()}
	switch failKind(err) {
	case failUnreachable:
		c.Status = registry.Crit
	case failAccess:
		c.Status = registry.Warn
	default:
		c.Status = registry.Unknown
	}
	return c
}

// evalHealth maps the cluster health: HEALTH_WARN is always a warning,
// HEALTH_ERR critical. Muted checks are already excluded by Ceph.
func evalHealth(h healthState) registry.Check {
	c := registry.Check{Name: "health"}
	switch h.Status {
	case "HEALTH_OK":
		c.Status, c.Detail = registry.OK, "HEALTH_OK"
		return c
	case "HEALTH_WARN":
		c.Status = registry.Warn
	case "HEALTH_ERR":
		c.Status = registry.Crit
	default:
		c.Status = registry.Unknown
	}
	var parts []string
	for _, name := range sortedChecks(h.Checks) {
		if hc := h.Checks[name]; !hc.Muted {
			parts = append(parts, name+": "+hc.Summary.Message)
		}
	}
	c.Detail = h.Status
	if len(parts) > 0 {
		c.Detail += " — " + strings.Join(parts, "; ")
	}
	return c
}

func evalQuorum(st clusterStatus) registry.Check {
	in, total := len(st.QuorumNames), st.Monmap.NumMons
	c := registry.Check{Name: "quorum", Status: registry.OK, Detail: fmt.Sprintf("%d/%d monitors in quorum", in, total)}
	if in < total {
		c.Status = registry.Warn
	}
	return c
}

func evalOSDs(st clusterStatus) registry.Check {
	o := st.Osdmap
	c := registry.Check{Name: "osds", Status: registry.OK, Detail: fmt.Sprintf("%d OSDs: %d up, %d in", o.NumOSDs, o.NumUpOSDs, o.NumInOSDs)}
	if o.NumUpOSDs < o.NumOSDs || o.NumInOSDs < o.NumOSDs {
		c.Status = registry.Warn
		c.Detail += fmt.Sprintf(" (%d down, %d out; see ceph_osds)", o.NumOSDs-o.NumUpOSDs, o.NumOSDs-o.NumInOSDs)
	}
	return c
}

// evalPGs: inactive PGs block IO (critical); degraded, peering and similar
// states are warnings; scrubbing and other background work is fine.
func evalPGs(st clusterStatus) registry.Check {
	c := registry.Check{Name: "pgs", Status: registry.OK}
	var bad []string
	for _, ps := range st.Pgmap.PGsByState {
		state := "+" + ps.StateName + "+"
		switch {
		case strings.Contains(state, "+active+") && strings.Contains(state, "+clean+"):
			continue
		case !strings.Contains(state, "+active+"):
			c.Status = registry.Crit
		case c.Status == registry.OK:
			c.Status = registry.Warn
		}
		bad = append(bad, fmt.Sprintf("%d %s", ps.Count, ps.StateName))
	}
	if len(bad) == 0 {
		c.Detail = fmt.Sprintf("all %d PGs active+clean", st.Pgmap.NumPGs)
	} else {
		c.Detail = fmt.Sprintf("%d PGs; not active+clean: %s", st.Pgmap.NumPGs, strings.Join(bad, ", "))
	}
	return c
}

// evalDaemons warns about failed Ceph units; stale cephadm units of moved
// or removed daemons are only reported.
func evalDaemons(units []unit) registry.Check {
	c := registry.Check{Name: "daemons", Status: registry.OK}
	var failed, stale []string
	running := 0
	for _, u := range units {
		switch {
		case u.Failed() && u.Stale:
			stale = append(stale, u.Daemon)
		case u.Failed():
			failed = append(failed, u.Daemon+" ("+u.Active+"/"+u.Sub+")")
		case u.Active == "active":
			running++
		}
	}
	c.Detail = fmt.Sprintf("%d Ceph daemons running on this host", running)
	if len(failed) > 0 {
		c.Status = registry.Warn
		c.Detail += "; FAILED: " + strings.Join(failed, ", ")
	}
	if len(stale) > 0 {
		c.Detail += "; stale failed units of daemons no longer in the orchestrator: " + strings.Join(stale, ", ")
	}
	return c
}

// evalCrashes warns about recent crash reports still on this host; older
// ones are only reported.
func evalCrashes(crashes []crash, now time.Time) registry.Check {
	c := registry.Check{Name: "crashes", Status: registry.OK, Detail: "no unposted crash reports on this host"}
	if len(crashes) == 0 {
		return c
	}
	recent := 0
	for _, cr := range crashes {
		if now.Sub(cr.Time) < crashRecent {
			recent++
		}
	}
	newest := crashes[0]
	c.Detail = fmt.Sprintf("%d crash reports not sent to the cluster, newest %s (%s); see ceph_local",
		len(crashes), newest.Time.Format("2006-01-02"), firstNonEmpty(newest.Entity, "?"))
	if recent > 0 {
		c.Status = registry.Warn
		c.Detail = fmt.Sprintf("%d recent (%d days) ", recent, int(crashRecent.Hours()/24)) + c.Detail
	}
	return c
}
