package ceph

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// osdDF is "ceph osd df tree": CRUSH tree with usage per OSD and host.
type osdDF struct {
	Nodes []struct {
		ID          int     `json:"id"`
		Name        string  `json:"name"`
		Type        string  `json:"type"`
		DeviceClass string  `json:"device_class"`
		Children    []int   `json:"children"`
		CrushWeight float64 `json:"crush_weight"`
		Reweight    float64 `json:"reweight"`
		KB          int64   `json:"kb"`
		KBUsed      int64   `json:"kb_used"`
		KBAvail     int64   `json:"kb_avail"`
		Util        float64 `json:"utilization"`
		Var         float64 `json:"var"`
		PGs         int     `json:"pgs"`
		Status      string  `json:"status"`
	} `json:"nodes"`
	Summary struct {
		TotalKB      int64   `json:"total_kb"`
		TotalKBUsed  int64   `json:"total_kb_used"`
		AverageUtil  float64 `json:"average_utilization"`
		MinVar       float64 `json:"min_var"`
		MaxVar       float64 `json:"max_var"`
		StdDeviation float64 `json:"dev"`
	} `json:"summary"`
}

// osdMap is the part of "ceph osd dump" the module uses: the cluster's own
// fullness thresholds and flags.
type osdMap struct {
	FullRatio         float64  `json:"full_ratio"`
	BackfillFullRatio float64  `json:"backfillfull_ratio"`
	NearFullRatio     float64  `json:"nearfull_ratio"`
	FlagsSet          []string `json:"flags_set"`
	RequireOSDRelease string   `json:"require_osd_release"`
	Pools             []pool   `json:"pools"`
}

// routineFlags are set on every healthy cluster; others (noout, norebalance,
// pause...) change behaviour and are worth showing.
var routineFlags = map[string]bool{"sortbitwise": true, "recovery_deletes": true, "purged_snapdirs": true, "pglog_hardlimit": true}

type osdPerf struct {
	OSDStats struct {
		Infos []struct {
			ID   int `json:"id"`
			Perf struct {
				Commit float64 `json:"commit_latency_ms"`
				Apply  float64 `json:"apply_latency_ms"`
			} `json:"perf_stats"`
		} `json:"osd_perf_infos"`
	} `json:"osdstats"`
}

type osdsInput struct {
	Host  string `json:"host,omitempty" jsonschema:"only OSDs of this CRUSH host"`
	State string `json:"state,omitempty" jsonschema:"problems (default: down, out or nearly full), all, down, out, nearfull"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of OSDs listed (default 50, max 500)"`
}

// osdRow is one OSD with its host and the problems found.
type osdRow struct {
	ID       int
	Name     string
	Host     string
	Class    string
	Status   string
	Reweight float64
	Util     float64
	Var      float64
	PGs      int
	SizeKB   int64
	Problems []string
}

var nameArgRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (m *Module) osdsTool(ctx context.Context, env *registry.Env, in osdsInput) (string, error) {
	state := firstNonEmpty(in.State, "problems")
	switch state {
	case "problems", "all", "down", "out", "nearfull":
	default:
		return "", fmt.Errorf("invalid state %q (problems, all, down, out, nearfull)", in.State)
	}
	if in.Host != "" && !nameArgRe.MatchString(in.Host) {
		return "", fmt.Errorf("invalid host %q", in.Host)
	}
	s := m.site(env)
	var (
		df                  osdDF
		om                  osdMap
		perf                osdPerf
		dfErr, omErr, pfErr error
		wg                  sync.WaitGroup
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { dfErr = query(ctx, env, s, &df, "osd", "df", "tree") })
	run(func() { omErr = query(ctx, env, s, &om, "osd", "dump") })
	run(func() { pfErr = query(ctx, env, s, &perf, "osd", "perf") })
	wg.Wait()
	if dfErr != nil {
		return "", dfErr
	}
	if omErr != nil {
		om = osdMap{NearFullRatio: 0.85, BackfillFullRatio: 0.90, FullRatio: 0.95}
	}
	return renderOSDs(df, om, omErr, perf, pfErr, in.Host, state, clampLimit(in.Limit, 50, 500)), nil
}

// osdRows joins OSDs with their CRUSH host and rates them against the
// cluster's thresholds.
func osdRows(df osdDF, om osdMap) []osdRow {
	host := map[int]string{}
	for _, n := range df.Nodes {
		if n.Type == "host" {
			for _, c := range n.Children {
				host[c] = n.Name
			}
		}
	}
	var rows []osdRow
	for _, n := range df.Nodes {
		if n.Type != "osd" {
			continue
		}
		r := osdRow{ID: n.ID, Name: n.Name, Host: firstNonEmpty(host[n.ID], "-"), Class: n.DeviceClass, Status: n.Status,
			Reweight: n.Reweight, Util: n.Util, Var: n.Var, PGs: n.PGs, SizeKB: n.KB}
		if r.Status != "up" {
			r.Problems = append(r.Problems, "DOWN")
		}
		if r.Reweight == 0 {
			r.Problems = append(r.Problems, "OUT")
		}
		switch u := r.Util / 100; {
		case om.FullRatio > 0 && u >= om.FullRatio:
			r.Problems = append(r.Problems, fmt.Sprintf("FULL (>= %.0f%%)", 100*om.FullRatio))
		case om.BackfillFullRatio > 0 && u >= om.BackfillFullRatio:
			r.Problems = append(r.Problems, fmt.Sprintf("backfillfull (>= %.0f%%)", 100*om.BackfillFullRatio))
		case om.NearFullRatio > 0 && u >= om.NearFullRatio:
			r.Problems = append(r.Problems, fmt.Sprintf("nearfull (>= %.0f%%)", 100*om.NearFullRatio))
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

func (r osdRow) matches(state string) bool {
	has := func(p string) bool {
		for _, x := range r.Problems {
			if strings.HasPrefix(strings.ToLower(x), p) {
				return true
			}
		}
		return false
	}
	switch state {
	case "all":
		return true
	case "down":
		return has("down")
	case "out":
		return has("out")
	case "nearfull":
		return has("nearfull") || has("backfillfull") || has("full")
	}
	return len(r.Problems) > 0
}

func renderOSDs(df osdDF, om osdMap, omErr error, perf osdPerf, pfErr error, hostFilter, state string, limit int) string {
	rows := osdRows(df, om)
	var b strings.Builder
	up, in := 0, 0
	for _, r := range rows {
		if r.Status == "up" {
			up++
		}
		if r.Reweight > 0 {
			in++
		}
	}
	sm := df.Summary
	fmt.Fprintf(&b, "OSDs: %d, %d up, %d in; usage %.1f%% average (variance %.2f-%.2f, std dev %.1f)\n",
		len(rows), up, in, sm.AverageUtil, sm.MinVar, sm.MaxVar, sm.StdDeviation)
	if omErr != nil {
		fmt.Fprintf(&b, "Thresholds: unknown (osd dump failed: %v); assuming nearfull 85%%, backfillfull 90%%, full 95%%\n", omErr)
	} else {
		fmt.Fprintf(&b, "Thresholds (cluster): nearfull %.0f%%, backfillfull %.0f%%, full %.0f%%\n",
			100*om.NearFullRatio, 100*om.BackfillFullRatio, 100*om.FullRatio)
		var flags []string
		for _, f := range om.FlagsSet {
			if !routineFlags[f] {
				flags = append(flags, f)
			}
		}
		if len(flags) > 0 {
			fmt.Fprintf(&b, "Flags set: %s\n", strings.Join(flags, ", "))
		}
	}

	// Per host: count, down, usage.
	type hostSum struct {
		n, down  int
		kb, used int64
	}
	hosts := map[string]*hostSum{}
	for _, r := range rows {
		h := hosts[r.Host]
		if h == nil {
			h = &hostSum{}
			hosts[r.Host] = h
		}
		h.n++
		if r.Status != "up" {
			h.down++
		}
		h.kb += r.SizeKB
		h.used += int64(float64(r.SizeKB) * r.Util / 100)
	}
	b.WriteString("\nHosts:\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  HOST\tOSDS\tDOWN\tSIZE\tUSE%")
	for _, name := range sortedKeys(hosts) {
		h := hosts[name]
		pct := 0.0
		if h.kb > 0 {
			pct = 100 * float64(h.used) / float64(h.kb)
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%.1f\n", name, h.n, h.down, bytesHuman(h.kb*1024), pct)
	}
	tw.Flush()

	var shown []osdRow
	for _, r := range rows {
		if (hostFilter == "" || r.Host == hostFilter) && r.matches(state) {
			shown = append(shown, r)
		}
	}
	title := map[string]string{"problems": "OSDs with problems", "all": "OSDs", "down": "OSDs down", "out": "OSDs out", "nearfull": "OSDs above the nearfull threshold"}[state]
	if hostFilter != "" {
		title += " on " + hostFilter
	}
	fmt.Fprintf(&b, "\n%s: %d\n", title, len(shown))
	if len(shown) > 0 {
		tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  OSD\tHOST\tCLASS\tSTATUS\tREWEIGHT\tSIZE\tUSE%\tVAR\tPGS\tPROBLEMS")
		for i, r := range shown {
			if i == limit {
				fmt.Fprintf(tw, "  ... %d more (raise limit or filter by host)\n", len(shown)-i)
				break
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.2f\t%s\t%.1f\t%.2f\t%d\t%s\n", r.Name, r.Host, r.Class, r.Status, r.Reweight,
				bytesHuman(r.SizeKB*1024), r.Util, r.Var, r.PGs, strings.Join(r.Problems, ", "))
		}
		tw.Flush()
	}

	if pfErr == nil {
		infos := perf.OSDStats.Infos
		sort.Slice(infos, func(i, j int) bool { return infos[i].Perf.Commit > infos[j].Perf.Commit })
		var slow []string
		for i, p := range infos {
			if i == 5 || p.Perf.Commit == 0 {
				break
			}
			slow = append(slow, fmt.Sprintf("osd.%d %.0f ms commit / %.0f ms apply", p.ID, p.Perf.Commit, p.Perf.Apply))
		}
		if len(slow) > 0 {
			fmt.Fprintf(&b, "\nHighest latency: %s\n", strings.Join(slow, "; "))
		}
	}
	return b.String()
}

// clampLimit applies a default and a maximum to a limit argument.
func clampLimit(n, def, max int) int {
	switch {
	case n <= 0:
		return def
	case n > max:
		return max
	}
	return n
}
