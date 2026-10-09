package ceph

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// fsList is "ceph fs ls".
type fsList []struct {
	Name         string   `json:"name"`
	MetadataPool string   `json:"metadata_pool"`
	DataPools    []string `json:"data_pools"`
}

func (m *Module) cephfsTool(ctx context.Context, env *registry.Env) (string, error) {
	s := m.site(env)
	var fsl fsList
	if err := query(ctx, env, s, &fsl, "fs", "ls"); err != nil {
		return "", err
	}
	if len(fsl) == 0 {
		return "No CephFS file systems in this cluster.\n", nil
	}
	var st fsStatus
	stErr := query(ctx, env, s, &st, "fs", "status")
	return renderCephFS(fsl, st, stErr), nil
}

func renderCephFS(fsl fsList, st fsStatus, stErr error) string {
	var b strings.Builder
	for _, f := range fsl {
		fmt.Fprintf(&b, "File system %s: metadata pool %s, data pools %s\n", f.Name, f.MetadataPool, strings.Join(f.DataPools, ", "))
	}
	if stErr != nil {
		fmt.Fprintf(&b, "\nStatus unknown: %v\n", stErr)
		return b.String()
	}
	for _, c := range st.Clients {
		fmt.Fprintf(&b, "Clients of %s: %d\n", c.FS, c.Clients)
	}
	mds := st.MDSMap
	sort.SliceStable(mds, func(i, j int) bool {
		ai, aj := mds[i].State == "standby", mds[j].State == "standby"
		if ai != aj {
			return !ai
		}
		return mds[i].Rank < mds[j].Rank
	})
	b.WriteString("\nMDS:\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  DAEMON\tSTATE\tRANK\tREQ/S\tINODES\tCAPS")
	active := 0
	for _, d := range mds {
		rank := "-"
		if d.State != "standby" && d.State != "standby-replay" {
			rank = fmt.Sprint(d.Rank)
		}
		if d.State == "active" {
			active++
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.0f\t%s\t%s\n", d.Name, d.State, rank, d.Rate, count(d.Inos), count(d.Caps))
	}
	tw.Flush()
	if active == 0 {
		b.WriteString("  NO ACTIVE MDS: the file system is unavailable\n")
	}
	if len(st.Pools) > 0 {
		b.WriteString("\nPools:\n")
		for _, p := range st.Pools {
			fmt.Fprintf(&b, "  %s (%s): %s used, %s available\n", p.Name, p.Type, bytesHuman(p.Used), bytesHuman(p.Avail))
		}
	}
	return b.String()
}
