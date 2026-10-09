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

// pgStat is "ceph pg stat".
type pgStat struct {
	Summary struct {
		ByState []struct {
			Name string `json:"name"`
			Num  int    `json:"num"`
		} `json:"num_pg_by_state"`
		NumPGs int `json:"num_pgs"`
	} `json:"pg_summary"`
}

// pgStuck is "ceph pg dump_stuck" (inactive, unclean, stale and similar).
type pgStuck struct {
	PGReady bool `json:"pg_ready"`
	Stuck   []struct {
		PGID          string `json:"pgid"`
		State         string `json:"state"`
		Up            []int  `json:"up"`
		Acting        []int  `json:"acting"`
		UpPrimary     int    `json:"up_primary"`
		ActingPrimary int    `json:"acting_primary"`
		LastClean     string `json:"last_clean"`
		LastActive    string `json:"last_active"`
	} `json:"stuck_pg_stats"`
}

// blockedBy is "ceph osd blocked-by": OSDs that block peering.
type blockedBy struct {
	OSDBlockedBy struct {
		Infos []struct {
			ID  int `json:"id"`
			Num int `json:"num_blocked"`
		} `json:"osd_blocked_by_infos"`
	} `json:"osd_blocked_by"`
}

type pgsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of stuck PGs listed (default 50, max 500)"`
}

func (m *Module) pgsTool(ctx context.Context, env *registry.Env, in pgsInput) (string, error) {
	s := m.site(env)
	var (
		st                  pgStat
		stuck               pgStuck
		bb                  blockedBy
		stErr, skErr, bbErr error
		wg                  sync.WaitGroup
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { stErr = query(ctx, env, s, &st, "pg", "stat") })
	run(func() {
		skErr = query(ctx, env, s, &stuck, "pg", "dump_stuck", "inactive", "unclean", "stale", "undersized", "degraded")
	})
	run(func() { bbErr = query(ctx, env, s, &bb, "osd", "blocked-by") })
	wg.Wait()
	if stErr != nil {
		return "", stErr
	}
	return renderPGs(st, stuck, skErr, bb, bbErr, clampLimit(in.Limit, 50, 500)), nil
}

func renderPGs(st pgStat, stuck pgStuck, skErr error, bb blockedBy, bbErr error, limit int) string {
	var b strings.Builder
	states := st.Summary.ByState
	sort.Slice(states, func(i, j int) bool { return states[i].Num > states[j].Num })
	fmt.Fprintf(&b, "PGs: %d\n", st.Summary.NumPGs)
	for _, s := range states {
		fmt.Fprintf(&b, "  %6d %s\n", s.Num, s.Name)
	}

	switch {
	case skErr != nil:
		fmt.Fprintf(&b, "\nStuck PGs: unknown (%v)\n", skErr)
	case len(stuck.Stuck) == 0:
		b.WriteString("\nStuck PGs: none (inactive, unclean, stale, undersized, degraded)\n")
	default:
		list := stuck.Stuck
		sort.Slice(list, func(i, j int) bool { return list[i].PGID < list[j].PGID })
		fmt.Fprintf(&b, "\nStuck PGs: %d\n", len(list))
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PG\tSTATE\tUP\tACTING\tPRIMARY\tLAST CLEAN")
		for i, p := range list {
			if i == limit {
				fmt.Fprintf(tw, "  ... %d more (raise limit)\n", len(list)-i)
				break
			}
			fmt.Fprintf(tw, "  %s\t%s\t%v\t%v\tosd.%d\t%s\n", p.PGID, p.State, p.Up, p.Acting, p.ActingPrimary, firstNonEmpty(p.LastClean, "-"))
		}
		tw.Flush()
	}

	if bbErr == nil && len(bb.OSDBlockedBy.Infos) > 0 {
		var parts []string
		for _, x := range bb.OSDBlockedBy.Infos {
			parts = append(parts, fmt.Sprintf("osd.%d blocks %d PGs", x.ID, x.Num))
		}
		fmt.Fprintf(&b, "\nPeering blocked by: %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}
