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

// pool is one entry of "ceph osd pool ls detail" (and of "osd dump").
type pool struct {
	ID              int                       `json:"pool_id"`
	Name            string                    `json:"pool_name"`
	Type            int                       `json:"type"` // 1 replicated, 3 erasure
	Size            int                       `json:"size"`
	MinSize         int                       `json:"min_size"`
	PGNum           int                       `json:"pg_num"`
	PGNumTarget     int                       `json:"pg_num_target"`
	CrushRule       int                       `json:"crush_rule"`
	ECProfile       string                    `json:"erasure_code_profile"`
	AutoscaleMode   string                    `json:"pg_autoscale_mode"`
	QuotaMaxBytes   int64                     `json:"quota_max_bytes"`
	QuotaMaxObjects int64                     `json:"quota_max_objects"`
	Applications    map[string]map[string]any `json:"application_metadata"`
	FlagsNames      string                    `json:"flags_names"`
}

// dfDetail is "ceph df detail".
type dfDetail struct {
	Stats struct {
		TotalBytes        int64   `json:"total_bytes"`
		TotalAvailBytes   int64   `json:"total_avail_bytes"`
		TotalUsedRawBytes int64   `json:"total_used_raw_bytes"`
		UsedRawRatio      float64 `json:"total_used_raw_ratio"`
	} `json:"stats"`
	Pools []struct {
		Name  string    `json:"name"`
		ID    int       `json:"id"`
		Stats poolUsage `json:"stats"`
	} `json:"pools"`
}

// poolUsage is the usage of one pool in "ceph df detail".
type poolUsage struct {
	Stored      int64   `json:"stored"`
	Objects     int64   `json:"objects"`
	BytesUsed   int64   `json:"bytes_used"`
	PercentUsed float64 `json:"percent_used"`
	MaxAvail    int64   `json:"max_avail"`
}

// autoscale is one entry of "ceph osd pool autoscale-status".
type autoscale struct {
	PoolName    string `json:"pool_name"`
	PGNumTarget int    `json:"pg_num_target"`
	PGNumFinal  int    `json:"pg_num_final"`
	Mode        string `json:"pg_autoscale_mode"`
	WouldAdjust bool   `json:"would_adjust"`
}

type poolsInput struct {
	Pool string `json:"pool,omitempty" jsonschema:"only this pool"`
}

// quotaWarn is the share of a pool quota above which it is reported.
const quotaWarn = 0.8

func (m *Module) poolsTool(ctx context.Context, env *registry.Env, in poolsInput) (string, error) {
	if in.Pool != "" && !nameArgRe.MatchString(in.Pool) {
		return "", fmt.Errorf("invalid pool name %q", in.Pool)
	}
	s := m.site(env)
	var (
		pools               []pool
		df                  dfDetail
		as                  []autoscale
		plErr, dfErr, asErr error
		wg                  sync.WaitGroup
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { plErr = query(ctx, env, s, &pools, "osd", "pool", "ls", "detail") })
	run(func() { dfErr = query(ctx, env, s, &df, "df", "detail") })
	run(func() { asErr = query(ctx, env, s, &as, "osd", "pool", "autoscale-status") })
	wg.Wait()
	if plErr != nil {
		return "", plErr
	}
	if in.Pool != "" {
		var only []pool
		for _, p := range pools {
			if p.Name == in.Pool {
				only = append(only, p)
			}
		}
		if len(only) == 0 {
			return "", fmt.Errorf("no pool %q", in.Pool)
		}
		pools = only
	}
	return renderPools(pools, df, dfErr, as, asErr), nil
}

func renderPools(pools []pool, df dfDetail, dfErr error, as []autoscale, asErr error) string {
	var b strings.Builder
	if dfErr == nil && df.Stats.TotalBytes > 0 {
		fmt.Fprintf(&b, "Raw capacity: %s, %s used (%.1f%%), %s available\n\n",
			bytesHuman(df.Stats.TotalBytes), bytesHuman(df.Stats.TotalUsedRawBytes), 100*df.Stats.UsedRawRatio, bytesHuman(df.Stats.TotalAvailBytes))
	}
	use := map[int]poolUsage{}
	for _, p := range df.Pools {
		use[p.ID] = p.Stats
	}
	auto := map[string]autoscale{}
	for _, a := range as {
		auto[a.PoolName] = a
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "POOL\tID\tTYPE\tPGS\tAUTOSCALE\tAPP\tSTORED\tUSED\tUSE%\tMAX AVAIL\tOBJECTS\n")
	var notes []string
	for _, p := range pools {
		typ := fmt.Sprintf("replicated %d/%d", p.Size, p.MinSize)
		if p.Type == 3 || p.ECProfile != "" && p.Type != 1 {
			typ = fmt.Sprintf("erasure %s %d/%d", p.ECProfile, p.Size, p.MinSize)
		}
		pgs := fmt.Sprint(p.PGNum)
		if p.PGNumTarget > 0 && p.PGNumTarget != p.PGNum {
			pgs += fmt.Sprintf("->%d", p.PGNumTarget)
		}
		mode := p.AutoscaleMode
		if a, ok := auto[p.Name]; ok && a.PGNumFinal > 0 && a.PGNumFinal != p.PGNum && asErr == nil {
			mode += fmt.Sprintf(" (ideal %d)", a.PGNumFinal)
		}
		u, hasUse := use[p.ID]
		stored, used, pct, avail, objs := "-", "-", "-", "-", "-"
		if hasUse {
			stored, used, avail = bytesHuman(u.Stored), bytesHuman(u.BytesUsed), bytesHuman(u.MaxAvail)
			pct = fmt.Sprintf("%.1f", 100*u.PercentUsed)
			objs = count(u.Objects)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.ID, typ, pgs, mode,
			strings.Join(sortedKeys(p.Applications), ","), stored, used, pct, avail, objs)

		if p.Type == 1 && p.MinSize <= 1 && p.Size > 1 {
			notes = append(notes, fmt.Sprintf("%s: min_size %d accepts writes with a single copy (data loss risk)", p.Name, p.MinSize))
		}
		if p.Type == 1 && p.Size <= 1 {
			notes = append(notes, fmt.Sprintf("%s: size %d keeps no redundant copy", p.Name, p.Size))
		}
		if hasUse && p.QuotaMaxBytes > 0 && float64(u.Stored) >= quotaWarn*float64(p.QuotaMaxBytes) {
			notes = append(notes, fmt.Sprintf("%s: %s of the %s byte quota used", p.Name, bytesHuman(u.Stored), bytesHuman(p.QuotaMaxBytes)))
		}
		if hasUse && p.QuotaMaxObjects > 0 && float64(u.Objects) >= quotaWarn*float64(p.QuotaMaxObjects) {
			notes = append(notes, fmt.Sprintf("%s: %d of %d objects quota used", p.Name, u.Objects, p.QuotaMaxObjects))
		}
		if hasUse && u.MaxAvail == 0 && p.Size > 0 {
			notes = append(notes, fmt.Sprintf("%s: no space available (MAX AVAIL 0)", p.Name))
		}
	}
	tw.Flush()
	if dfErr != nil {
		fmt.Fprintf(&b, "\nUsage unknown: %v\n", dfErr)
	}
	if len(notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range notes {
			fmt.Fprintf(&b, "  %s\n", n)
		}
	}
	return b.String()
}
