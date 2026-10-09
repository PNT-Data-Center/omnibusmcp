package ceph

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// clusterStatus is "ceph status" (tested with reef 18.2 and tentacle 20.2).
type clusterStatus struct {
	FSID        string      `json:"fsid"`
	Health      healthState `json:"health"`
	QuorumNames []string    `json:"quorum_names"`
	QuorumAge   int64       `json:"quorum_age"`
	Monmap      struct {
		NumMons       int    `json:"num_mons"`
		MinMonRelease string `json:"min_mon_release_name"`
	} `json:"monmap"`
	Osdmap struct {
		NumOSDs        int `json:"num_osds"`
		NumUpOSDs      int `json:"num_up_osds"`
		NumInOSDs      int `json:"num_in_osds"`
		NumRemappedPGs int `json:"num_remapped_pgs"`
	} `json:"osdmap"`
	Pgmap struct {
		PGsByState []struct {
			StateName string `json:"state_name"`
			Count     int    `json:"count"`
		} `json:"pgs_by_state"`
		NumPGs        int     `json:"num_pgs"`
		NumPools      int     `json:"num_pools"`
		NumObjects    int64   `json:"num_objects"`
		DataBytes     int64   `json:"data_bytes"`
		BytesUsed     int64   `json:"bytes_used"`
		BytesAvail    int64   `json:"bytes_avail"`
		BytesTotal    int64   `json:"bytes_total"`
		ReadBytesSec  float64 `json:"read_bytes_sec"`
		WriteBytesSec float64 `json:"write_bytes_sec"`
		ReadOpsSec    float64 `json:"read_op_per_sec"`
		WriteOpsSec   float64 `json:"write_op_per_sec"`
		RecoveringBPS float64 `json:"recovering_bytes_per_sec"`
		DegradedRatio float64 `json:"degraded_ratio"`
		MisplacedRat  float64 `json:"misplaced_ratio"`
	} `json:"pgmap"`
	Mgrmap struct {
		Available   bool              `json:"available"`
		NumStandbys int               `json:"num_standbys"`
		Modules     []string          `json:"modules"`
		Services    map[string]string `json:"services"`
	} `json:"mgrmap"`
	Fsmap struct {
		Up        int `json:"up"`
		In        int `json:"in"`
		Max       int `json:"max"`
		UpStandby int `json:"up:standby"`
		ByRank    []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Rank   int    `json:"rank"`
		} `json:"by_rank"`
	} `json:"fsmap"`
	ProgressEvents map[string]struct {
		Message  string  `json:"message"`
		Progress float64 `json:"progress"`
	} `json:"progress_events"`
}

// healthState is the health part of "ceph status" and "ceph health detail".
type healthState struct {
	Status string                 `json:"status"`
	Checks map[string]healthCheck `json:"checks"`
	Mutes  []struct {
		Code string `json:"code"`
	} `json:"mutes"`
}

type healthCheck struct {
	Severity string `json:"severity"`
	Summary  struct {
		Message string `json:"message"`
		Count   int    `json:"count"`
	} `json:"summary"`
	Detail []struct {
		Message string `json:"message"`
	} `json:"detail"`
	Muted bool `json:"muted"`
}

type mgrStat struct {
	ActiveName string `json:"active_name"`
	Available  bool   `json:"available"`
	NumStandby int    `json:"num_standby"`
}

type monDump struct {
	Mons []struct {
		Rank int    `json:"rank"`
		Name string `json:"name"`
	} `json:"mons"`
}

// healthDetailLines bounds the detail lines shown per health check.
const healthDetailLines = 10

func (m *Module) statusTool(ctx context.Context, env *registry.Env) (string, error) {
	s := m.site(env)
	var (
		st                            clusterStatus
		hd                            healthState
		mgr                           mgrStat
		mons                          monDump
		stErr, hdErr, mgrErr, monsErr error
		wg                            sync.WaitGroup
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { stErr = query(ctx, env, s, &st, "status") })
	run(func() { hdErr = query(ctx, env, s, &hd, "health", "detail") })
	run(func() { mgrErr = query(ctx, env, s, &mgr, "mgr", "stat") })
	run(func() { monsErr = query(ctx, env, s, &mons, "mon", "dump") })
	wg.Wait()
	if stErr != nil {
		return "", stErr
	}
	if hdErr == nil {
		st.Health = hd
	}
	return renderStatus(s, st, mgr, mgrErr, mons, monsErr), nil
}

func renderStatus(s site, st clusterStatus, mgr mgrStat, mgrErr error, mons monDump, monsErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster:  %s (%s, %s)\n", st.FSID, s.Variant, firstNonEmpty(st.Monmap.MinMonRelease, "release unknown"))
	fmt.Fprintf(&b, "Health:   %s\n", firstNonEmpty(st.Health.Status, "unknown"))
	for _, name := range sortedChecks(st.Health.Checks) {
		c := st.Health.Checks[name]
		muted := ""
		if c.Muted {
			muted = " (muted)"
		}
		fmt.Fprintf(&b, "  [%s] %s: %s%s\n", strings.TrimPrefix(c.Severity, "HEALTH_"), name, c.Summary.Message, muted)
		for i, d := range c.Detail {
			if i == healthDetailLines {
				fmt.Fprintf(&b, "      ... %d more\n", len(c.Detail)-i)
				break
			}
			fmt.Fprintf(&b, "      %s\n", d.Message)
		}
	}

	quorum := fmt.Sprintf("%d/%d in quorum: %s", len(st.QuorumNames), st.Monmap.NumMons, strings.Join(st.QuorumNames, ", "))
	if monsErr == nil {
		inQ := map[string]bool{}
		for _, n := range st.QuorumNames {
			inQ[n] = true
		}
		var out []string
		for _, mo := range mons.Mons {
			if !inQ[mo.Name] {
				out = append(out, mo.Name)
			}
		}
		if len(out) > 0 {
			quorum += "; OUT OF QUORUM: " + strings.Join(out, ", ")
		}
	}
	if st.QuorumAge > 0 {
		quorum += fmt.Sprintf(" (quorum for %s)", age(time.Duration(st.QuorumAge)*time.Second))
	}
	fmt.Fprintf(&b, "\nMonitors: %s\n", quorum)
	switch {
	case mgrErr == nil && mgr.ActiveName != "":
		fmt.Fprintf(&b, "Managers: active %s, %d standby\n", mgr.ActiveName, mgr.NumStandby)
	case !st.Mgrmap.Available:
		b.WriteString("Managers: NO ACTIVE MANAGER\n")
	default:
		fmt.Fprintf(&b, "Managers: available, %d standby\n", st.Mgrmap.NumStandbys)
	}
	o := st.Osdmap
	fmt.Fprintf(&b, "OSDs:     %d total, %d up, %d in", o.NumOSDs, o.NumUpOSDs, o.NumInOSDs)
	if down := o.NumOSDs - o.NumUpOSDs; down > 0 {
		fmt.Fprintf(&b, " — %d DOWN", down)
	}
	if out := o.NumOSDs - o.NumInOSDs; out > 0 {
		fmt.Fprintf(&b, " — %d OUT", out)
	}
	if o.NumRemappedPGs > 0 {
		fmt.Fprintf(&b, ", %d remapped PGs", o.NumRemappedPGs)
	}
	b.WriteString("\n")
	if fs := st.Fsmap; len(fs.ByRank) > 0 || fs.UpStandby > 0 {
		var ranks []string
		for _, r := range fs.ByRank {
			ranks = append(ranks, fmt.Sprintf("%s %s", r.Name, r.Status))
		}
		fmt.Fprintf(&b, "MDS:      %d/%d up (%s), %d standby\n", fs.Up, fs.In, strings.Join(ranks, ", "), fs.UpStandby)
	}
	if svcs := sortedKeys(st.Mgrmap.Services); len(svcs) > 0 {
		var parts []string
		for _, k := range svcs {
			parts = append(parts, k+" "+st.Mgrmap.Services[k])
		}
		fmt.Fprintf(&b, "Services: %s\n", strings.Join(parts, ", "))
	}

	p := st.Pgmap
	fmt.Fprintf(&b, "\nData:     %d pools, %d PGs, %s objects, %s stored\n", p.NumPools, p.NumPGs, count(p.NumObjects), bytesHuman(p.DataBytes))
	if p.BytesTotal > 0 {
		fmt.Fprintf(&b, "Usage:    %s used, %s / %s available (%.1f%% used)\n",
			bytesHuman(p.BytesUsed), bytesHuman(p.BytesAvail), bytesHuman(p.BytesTotal), 100*float64(p.BytesUsed)/float64(p.BytesTotal))
	}
	var states []string
	sort.Slice(p.PGsByState, func(i, j int) bool { return p.PGsByState[i].Count > p.PGsByState[j].Count })
	for _, ps := range p.PGsByState {
		states = append(states, fmt.Sprintf("%d %s", ps.Count, ps.StateName))
	}
	fmt.Fprintf(&b, "PGs:      %s\n", strings.Join(states, ", "))
	if p.DegradedRatio > 0 || p.MisplacedRat > 0 || p.RecoveringBPS > 0 {
		fmt.Fprintf(&b, "Recovery: %.2f%% degraded, %.2f%% misplaced, recovering %s/s\n", 100*p.DegradedRatio, 100*p.MisplacedRat, bytesHuman(int64(p.RecoveringBPS)))
	}
	fmt.Fprintf(&b, "Client IO: %s/s rd, %s/s wr, %.0f op/s rd, %.0f op/s wr\n",
		bytesHuman(int64(p.ReadBytesSec)), bytesHuman(int64(p.WriteBytesSec)), p.ReadOpsSec, p.WriteOpsSec)
	if len(st.ProgressEvents) > 0 {
		b.WriteString("\nIn progress:\n")
		for _, k := range sortedKeys(st.ProgressEvents) {
			e := st.ProgressEvents[k]
			fmt.Fprintf(&b, "  %3.0f%% %s\n", 100*e.Progress, e.Message)
		}
	}
	return b.String()
}

// sortedChecks orders health checks by severity (errors first), then name.
func sortedChecks(checks map[string]healthCheck) []string {
	names := sortedKeys(checks)
	sort.SliceStable(names, func(i, j int) bool {
		return checks[names[i]].Severity == "HEALTH_ERR" && checks[names[j]].Severity != "HEALTH_ERR"
	})
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func count(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fG", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func age(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
