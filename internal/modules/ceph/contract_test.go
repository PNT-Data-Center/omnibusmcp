package ceph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Contract tests: every recorded set of "ceph ... --format json" responses
// (testdata/contract/ceph-<version>, captured with test/contract/record
// -product ceph) must decode strictly and render. A failure after adding a
// version means the output format changed.
func strict(t *testing.T, dir, file string, v any) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Logf("%s: %s not recorded", dir, file)
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Errorf("%s/%s: strict decode failed (format changed?): %v", dir, file, err)
		return false
	}
	return true
}

func TestContractCeph(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/contract/ceph-*")
	if len(dirs) == 0 {
		t.Skip("no Ceph contract recordings yet (record -product ceph)")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var st clusterStatus
			if strict(t, dir, "status.json", &st) {
				if st.FSID == "" || st.Monmap.NumMons == 0 || st.Osdmap.NumOSDs == 0 || st.Pgmap.NumPGs == 0 {
					t.Errorf("status not understood: %+v", st)
				}
				for _, c := range []registry.Check{evalHealth(st.Health), evalQuorum(st), evalOSDs(st), evalPGs(st)} {
					if c.Status == registry.Unknown {
						t.Errorf("%+v", c)
					}
				}
				var hd healthState
				var mgr mgrStat
				var mons monDump
				strict(t, dir, "health-detail.json", &hd)
				strict(t, dir, "mgr-stat.json", &mgr)
				strict(t, dir, "mon-dump.json", &mons)
				if len(mons.Mons) != st.Monmap.NumMons || mgr.ActiveName == "" {
					t.Errorf("mon dump %d mons (status %d), active mgr %q", len(mons.Mons), st.Monmap.NumMons, mgr.ActiveName)
				}
				if out := renderStatus(site{Variant: "test"}, st, mgr, nil, mons, nil); !strings.Contains(out, "in quorum") {
					t.Errorf("%s", out)
				}
			}

			var df osdDF
			var om osdMap
			var perf osdPerf
			if strict(t, dir, "osd-df-tree.json", &df) && strict(t, dir, "osd-dump.json", &om) && strict(t, dir, "osd-perf.json", &perf) {
				rows := osdRows(df, om)
				if len(rows) == 0 || rows[0].Host == "-" || om.NearFullRatio == 0 {
					t.Errorf("OSDs not understood: %d rows, nearfull %v", len(rows), om.NearFullRatio)
				}
				renderOSDs(df, om, nil, perf, nil, "", "all", 500)
			}

			var pools []pool
			var dd dfDetail
			var as []autoscale
			if strict(t, dir, "osd-pool-ls-detail.json", &pools) && strict(t, dir, "df-detail.json", &dd) {
				strict(t, dir, "osd-pool-autoscale-status.json", &as)
				if len(pools) == 0 || len(dd.Pools) == 0 || dd.Stats.TotalBytes == 0 || pools[0].Size == 0 {
					t.Errorf("pools not understood")
				}
				if out := renderPools(pools, dd, nil, as, nil); strings.Count(out, "\n") < len(pools)+1 {
					t.Errorf("%s", out)
				}
			}

			var ps pgStat
			var stuck pgStuck
			var bb blockedBy
			if strict(t, dir, "pg-stat.json", &ps) && strict(t, dir, "pg-dump-stuck.json", &stuck) && strict(t, dir, "osd-blocked-by.json", &bb) {
				if ps.Summary.NumPGs == 0 || !stuck.PGReady {
					t.Errorf("PGs not understood: %+v", ps)
				}
				renderPGs(ps, stuck, nil, bb, nil, 50)
			}

			var vs versions
			if strict(t, dir, "versions.json", &vs) {
				if dominantVersion(vs["overall"]) == "" {
					t.Errorf("versions not understood: %v", vs)
				}
			}
			var crashes []crashEntry
			strict(t, dir, "crash-ls.json", &crashes)
			renderCrashes(crashes, nil, false, 30, time.Now())

			var fsl fsList
			var fs fsStatus
			if strict(t, dir, "fs-ls.json", &fsl) && len(fsl) > 0 && strict(t, dir, "fs-status.json", &fs) {
				if len(fs.MDSMap) == 0 {
					t.Errorf("fs status not understood")
				}
				renderCephFS(fsl, fs, nil)
			}
			var orch []orchDaemon
			if strict(t, dir, "orch-ps.json", &orch) {
				if len(orch) == 0 || orch[0].DaemonName == "" || orch[0].DaemonType == "" {
					t.Errorf("orch ps not understood")
				}
				renderOrch("Daemons (orchestrator)", orch, "", "", "all", 500)
			}
		})
	}
}
