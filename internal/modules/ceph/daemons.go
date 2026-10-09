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
		orch           []orchDaemon
		vs             versions
		orchErr, vsErr error
		st             clusterStatus
		mons           monDump
		mgr            mgrDump
		tree           osdDF
		fsSt           fsStatus
		stErr, monErr  error
		mgrErr, trErr  error
		fsErr          error
		wg             sync.WaitGroup
		run            = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
		limit          = clampLimit(in.Limit, 50, 500)
	)
	run(func() { orchErr = query(ctx, env, s, &orch, "orch", "ps") })
	run(func() { vsErr = query(ctx, env, s, &vs, "versions") })
	wg.Wait()

	if orchErr == nil {
		return renderVersions(vs, vsErr) + "\n" + renderOrch("Daemons (orchestrator)", orch, in.Type, in.Host, state, limit), nil
	}
	// No orchestrator (packages, e.g. Proxmox VE): the daemons the cluster
	// maps know, filtered the same way.
	run(func() { stErr = query(ctx, env, s, &st, "status") })
	run(func() { monErr = query(ctx, env, s, &mons, "mon", "dump") })
	run(func() { mgrErr = query(ctx, env, s, &mgr, "mgr", "dump") })
	run(func() { trErr = query(ctx, env, s, &tree, "osd", "tree") })
	run(func() { fsErr = query(ctx, env, s, &fsSt, "fs", "status") })
	wg.Wait()
	if stErr != nil {
		return "", stErr
	}
	var notes []string
	for name, err := range map[string]error{"monitors": monErr, "managers": mgrErr, "OSDs": trErr, "MDS": fsErr} {
		if err != nil && failKind(err) != failCommand {
			notes = append(notes, fmt.Sprintf("%s unknown: %v", name, err))
		}
	}
	rows := mapDaemons(st, mons, monErr, mgr, mgrErr, tree, trErr, fsSt, fsErr)
	out := fmt.Sprintf("No orchestrator (%s): daemons from the cluster maps (hosts known for OSDs only); per-host detail: ceph_local on each host.\n\n", lastLine(orchErr.Error())) +
		renderVersions(vs, vsErr) + "\n" + renderOrch("Daemons (cluster maps)", rows, in.Type, in.Host, state, limit)
	sort.Strings(notes)
	for _, n := range notes {
		out += n + "\n"
	}
	return out, nil
}

// mgrDump is the part of "ceph mgr dump" naming the managers.
type mgrDump struct {
	ActiveName string `json:"active_name"`
	Available  bool   `json:"available"`
	Standbys   []struct {
		Name string `json:"name"`
	} `json:"standbys"`
}

// mapDaemons lists the daemons the monitor, manager, OSD and MDS maps know,
// as orchestrator entries (status 1 running, -1 not).
func mapDaemons(st clusterStatus, mons monDump, monErr error, mgr mgrDump, mgrErr error, tree osdDF, trErr error, fs fsStatus, fsErr error) []orchDaemon {
	var out []orchDaemon
	add := func(typ, name, host string, ok bool, desc string) {
		d := orchDaemon{DaemonName: typ + "." + name, DaemonType: typ, Hostname: firstNonEmpty(host, "-"), Status: -1, StatusDesc: desc}
		if ok {
			d.Status = 1
		}
		out = append(out, d)
	}
	if monErr == nil {
		inQ := map[string]bool{}
		for _, q := range st.QuorumNames {
			inQ[q] = true
		}
		for _, m := range mons.Mons {
			desc := "in quorum"
			if !inQ[m.Name] {
				desc = "OUT OF QUORUM"
			}
			add("mon", m.Name, "", inQ[m.Name], desc)
		}
	}
	if mgrErr == nil {
		if mgr.ActiveName != "" {
			add("mgr", mgr.ActiveName, "", mgr.Available, "active")
		}
		for _, sb := range mgr.Standbys {
			add("mgr", sb.Name, "", true, "standby")
		}
	}
	if trErr == nil {
		host := map[int]string{}
		for _, n := range tree.Nodes {
			if n.Type == "host" {
				for _, c := range n.Children {
					host[c] = n.Name
				}
			}
		}
		for _, n := range tree.Nodes {
			if n.Type != "osd" {
				continue
			}
			desc := n.Status
			if n.Reweight == 0 {
				desc += ", out"
			}
			out = append(out, orchDaemon{DaemonName: n.Name, DaemonType: "osd", Hostname: firstNonEmpty(host[n.ID], "-"),
				Status: map[bool]int{true: 1, false: -1}[n.Status == "up"], StatusDesc: desc})
		}
	}
	if fsErr == nil {
		for _, d := range fs.MDSMap {
			ok := d.State == "active" || strings.HasPrefix(d.State, "standby")
			add("mds", strings.TrimPrefix(d.Name, "mds."), "", ok, d.State)
		}
	}
	return out
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

func renderOrch(title string, orch []orchDaemon, typ, host, state string, limit int) string {
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
	b.WriteString(title + ":\n")
	for _, t := range sortedKeys(byType) {
		c := byType[t]
		note := ""
		if c[0] < c[1] {
			note = fmt.Sprintf(" — %d NOT RUNNING", c[1]-c[0])
		}
		fmt.Fprintf(&b, "  %-14s %d/%d running%s\n", t, c[0], c[1], note)
	}
	sort.Slice(shown, func(i, j int) bool { return daemonLess(shown[i].DaemonName, shown[j].DaemonName) })
	list := "Daemons not running"
	if state == "all" {
		list = "Daemons"
	}
	fmt.Fprintf(&b, "\n%s: %d\n", list, len(shown))
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
