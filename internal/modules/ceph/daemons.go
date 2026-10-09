package ceph

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

type daemonsInput struct {
	Type  string `json:"type,omitempty" jsonschema:"daemon type: osd, mon, mgr, mds, rgw, nfs, crash..."`
	Host  string `json:"host,omitempty" jsonschema:"only daemons of this host (orchestrator hostname)"`
	State string `json:"state,omitempty" jsonschema:"problems (default: not running) or all"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of daemons listed (default 50, max 500)"`
}

// versions is "ceph versions": daemon counts per version and type.
type versions map[string]map[string]int

func (m *Module) daemonsTool(ctx context.Context, env *registry.Env, in daemonsInput) (string, error) {
	state := firstNonEmpty(in.State, "problems")
	if state != "problems" && state != "all" {
		return "", fmt.Errorf("invalid state %q (problems, all)", in.State)
	}
	for _, v := range []string{in.Type, in.Host} {
		if v != "" && !nameArgRe.MatchString(v) {
			return "", fmt.Errorf("invalid filter %q", v)
		}
	}
	s := m.site(env)
	var (
		orch             []orchDaemon
		vs               versions
		orchErr, vsErr   error
		st               clusterStatus
		stErr            error
		fsSt             fsStatus
		fsErr            error
		wg               sync.WaitGroup
		run              = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
		limit            = clampLimit(in.Limit, 50, 500)
		header, fallback string
	)
	run(func() { orchErr = query(ctx, env, s, &orch, "orch", "ps") })
	run(func() { vsErr = query(ctx, env, s, &vs, "versions") })
	wg.Wait()

	if orchErr == nil {
		header = renderVersions(vs, vsErr)
		return header + "\n" + renderOrch(orch, in.Type, in.Host, state, limit), nil
	}
	// No orchestrator (packages, e.g. Proxmox VE): what the maps tell.
	run(func() { stErr = query(ctx, env, s, &st, "status") })
	run(func() { fsErr = query(ctx, env, s, &fsSt, "fs", "status") })
	wg.Wait()
	if stErr != nil {
		return "", stErr
	}
	fallback = fmt.Sprintf("No orchestrator (%s): daemons as seen in the cluster maps; per-host detail: ceph_local on each host.\n\n", lastLine(orchErr.Error()))
	return fallback + renderVersions(vs, vsErr) + "\n" + renderMaps(st, fsSt, fsErr), nil
}

// renderVersions lists the versions per daemon type and flags mixed ones.
func renderVersions(vs versions, err error) string {
	if err != nil {
		return fmt.Sprintf("Versions: unknown (%v)\n", err)
	}
	var b strings.Builder
	b.WriteString("Versions:\n")
	for _, typ := range sortedKeys(vs) {
		if typ == "overall" {
			continue
		}
		var parts []string
		for _, v := range sortedKeys(vs[typ]) {
			short := v
			if mm := versionRe.FindStringSubmatch(v); mm != nil {
				short = mm[1]
			}
			parts = append(parts, fmt.Sprintf("%d x %s", vs[typ][v], short))
		}
		fmt.Fprintf(&b, "  %-6s %s\n", typ, strings.Join(parts, ", "))
	}
	if len(vs["overall"]) > 1 {
		b.WriteString("  MIXED VERSIONS: an upgrade is incomplete or some daemons were not restarted\n")
	}
	return b.String()
}

func renderOrch(orch []orchDaemon, typ, host, state string, limit int) string {
	var b strings.Builder
	byType := map[string][2]int{} // running, total
	var shown []orchDaemon
	for _, d := range orch {
		c := byType[d.DaemonType]
		c[1]++
		if d.Status == 1 {
			c[0]++
		}
		byType[d.DaemonType] = c
		if typ != "" && d.DaemonType != typ || host != "" && !hostMatches(d.Hostname, host) {
			continue
		}
		if state == "problems" && d.Status == 1 {
			continue
		}
		shown = append(shown, d)
	}
	b.WriteString("Daemons (orchestrator):\n")
	for _, t := range sortedKeys(byType) {
		c := byType[t]
		note := ""
		if c[0] < c[1] {
			note = fmt.Sprintf(" — %d NOT RUNNING", c[1]-c[0])
		}
		fmt.Fprintf(&b, "  %-14s %d/%d running%s\n", t, c[0], c[1], note)
	}
	sort.Slice(shown, func(i, j int) bool { return daemonLess(shown[i].DaemonName, shown[j].DaemonName) })
	title := "Daemons not running"
	if state == "all" {
		title = "Daemons"
	}
	fmt.Fprintf(&b, "\n%s: %d\n", title, len(shown))
	if len(shown) == 0 {
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  DAEMON\tHOST\tSTATUS\tVERSION\tSTARTED\tLAST REFRESH")
	for i, d := range shown {
		if i == limit {
			fmt.Fprintf(tw, "  ... %d more (raise limit or filter)\n", len(shown)-i)
			break
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", d.DaemonName, d.Hostname, firstNonEmpty(d.StatusDesc, fmt.Sprint(d.Status)),
			firstNonEmpty(d.Version, "-"), firstNonEmpty(d.Started, "-"), firstNonEmpty(d.LastRefr, "-"))
	}
	tw.Flush()
	return b.String()
}

// hostMatches compares a short or full host name with the orchestrator's.
func hostMatches(orchHost, want string) bool {
	short, _, _ := strings.Cut(orchHost, ".")
	return orchHost == want || short == want
}

// fsStatus is "ceph fs status".
type fsStatus struct {
	Clients []struct {
		Clients int    `json:"clients"`
		FS      string `json:"fs"`
	} `json:"clients"`
	MDSMap []struct {
		Name  string  `json:"name"`
		Rank  int     `json:"rank"`
		State string  `json:"state"`
		Rate  float64 `json:"rate"`
		Inos  int64   `json:"inos"`
		Caps  int64   `json:"caps"`
	} `json:"mdsmap"`
	Pools []struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Used  int64  `json:"used"`
		Avail int64  `json:"avail"`
	} `json:"pools"`
}

// renderMaps shows the daemons known from the monitor, manager and MDS maps.
func renderMaps(st clusterStatus, fs fsStatus, fsErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Monitors: %d, in quorum: %s\n", st.Monmap.NumMons, strings.Join(st.QuorumNames, ", "))
	if st.Mgrmap.Available {
		fmt.Fprintf(&b, "Managers: active manager available, %d standby\n", st.Mgrmap.NumStandbys)
	} else {
		b.WriteString("Managers: NO ACTIVE MANAGER\n")
	}
	o := st.Osdmap
	fmt.Fprintf(&b, "OSDs:     %d, %d up, %d in (details: ceph_osds)\n", o.NumOSDs, o.NumUpOSDs, o.NumInOSDs)
	if fsErr == nil && len(fs.MDSMap) > 0 {
		var mds []string
		for _, d := range fs.MDSMap {
			mds = append(mds, d.Name+" "+d.State)
		}
		fmt.Fprintf(&b, "MDS:      %s\n", strings.Join(mds, ", "))
	}
	if svc := st.Mgrmap.Services; len(svc) > 0 {
		fmt.Fprintf(&b, "Manager services: %s\n", strings.Join(sortedKeys(svc), ", "))
	}
	return b.String()
}
