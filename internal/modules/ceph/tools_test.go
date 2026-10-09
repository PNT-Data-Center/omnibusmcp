package ceph

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}

const osdDFJSON = `{"nodes":[
{"id":-1,"name":"default","type":"root","children":[-3,-5]},
{"id":-3,"name":"host1","type":"host","children":[0,1]},
{"id":0,"name":"osd.0","type":"osd","device_class":"hdd","reweight":1,"kb":1000000,"utilization":40,"var":0.8,"pgs":30,"status":"up"},
{"id":1,"name":"osd.1","type":"osd","device_class":"hdd","reweight":1,"kb":1000000,"utilization":87,"var":1.7,"pgs":60,"status":"up"},
{"id":-5,"name":"host2","type":"host","children":[2,3]},
{"id":2,"name":"osd.2","type":"osd","device_class":"ssd","reweight":0,"kb":1000000,"utilization":0,"var":0,"pgs":0,"status":"down"},
{"id":3,"name":"osd.3","type":"osd","device_class":"ssd","reweight":1,"kb":1000000,"utilization":91,"var":1.8,"pgs":50,"status":"up"}],
"stray":[],"summary":{"total_kb":4000000,"average_utilization":54.5,"min_var":0,"max_var":1.8,"dev":30}}`

func TestOSDs(t *testing.T) {
	var df osdDF
	mustJSON(t, osdDFJSON, &df)
	om := osdMap{NearFullRatio: 0.85, BackfillFullRatio: 0.90, FullRatio: 0.95, FlagsSet: []string{"sortbitwise", "noout", "pglog_hardlimit"}}
	rows := osdRows(df, om)
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.Host + ":" + strings.Join(r.Problems, ",")
	}
	want := map[string]string{"osd.0": "host1:", "osd.1": "host1:nearfull (>= 85%)", "osd.2": "host2:DOWN,OUT", "osd.3": "host2:backfillfull (>= 90%)"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}

	var perf osdPerf
	mustJSON(t, `{"osdstats":{"osd_perf_infos":[{"id":3,"perf_stats":{"commit_latency_ms":45,"apply_latency_ms":45}},{"id":0,"perf_stats":{"commit_latency_ms":0}}]}}`, &perf)
	out := renderOSDs(df, om, nil, perf, nil, "", "problems", 50)
	for _, w := range []string{"OSDs: 4, 3 up, 3 in", "Flags set: noout", "OSDs with problems: 3", "osd.2  host2", "Highest latency: osd.3 45 ms"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "sortbitwise") || strings.Contains(out, "osd.0  host1") {
		t.Errorf("routine flag or healthy OSD listed:\n%s", out)
	}
	if out := renderOSDs(df, om, nil, perf, nil, "host1", "all", 1); !strings.Contains(out, "OSDs on host1: 2") || !strings.Contains(out, "... 1 more") {
		t.Errorf("host filter and limit:\n%s", out)
	}
	if out := renderOSDs(df, om, nil, perf, nil, "", "nearfull", 50); !strings.Contains(out, "above the nearfull threshold: 2") {
		t.Errorf("nearfull filter:\n%s", out)
	}
	// Without osd dump the default thresholds are assumed and said so.
	if out := renderOSDs(df, osdMap{NearFullRatio: 0.85, BackfillFullRatio: 0.9, FullRatio: 0.95}, errors.New("x"), perf, nil, "", "problems", 50); !strings.Contains(out, "Thresholds: unknown") {
		t.Errorf("%s", out)
	}
}

func TestPools(t *testing.T) {
	var pools []pool
	mustJSON(t, `[{"pool_id":1,"pool_name":".mgr","type":1,"size":3,"min_size":2,"pg_num":1,"pg_num_target":1,"pg_autoscale_mode":"on","application_metadata":{"mgr":{}}},
{"pool_id":16,"pool_name":"cephfs_data","type":1,"size":3,"min_size":1,"pg_num":32,"pg_num_target":64,"pg_autoscale_mode":"on","application_metadata":{"cephfs":{}}},
{"pool_id":20,"pool_name":"quota","type":1,"size":3,"min_size":2,"pg_num":8,"pg_autoscale_mode":"warn","quota_max_bytes":1000,"application_metadata":{"rbd":{}}},
{"pool_id":21,"pool_name":"ec","type":3,"size":6,"min_size":5,"pg_num":64,"erasure_code_profile":"k4m2","pg_autoscale_mode":"on","application_metadata":{"rgw":{}}}]`, &pools)
	var df dfDetail
	mustJSON(t, `{"stats":{"total_bytes":1000000,"total_avail_bytes":600000,"total_used_raw_bytes":400000,"total_used_raw_ratio":0.4},
"pools":[{"name":"quota","id":20,"stats":{"stored":900,"objects":3,"bytes_used":2700,"percent_used":0.1,"max_avail":0}}]}`, &df)
	var as []autoscale
	mustJSON(t, `[{"pool_name":"quota","pg_num_final":32,"pg_autoscale_mode":"warn"}]`, &as)
	out := renderPools(pools, df, nil, as, nil)
	for _, w := range []string{"cephfs_data", "32->64", "warn (ideal 32)", "erasure k4m2 6/5", "cephfs_data: min_size 1", "quota: 900 B of the 1000 B byte quota used", "quota: no space available"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, ".mgr: ") {
		t.Errorf("healthy pool noted:\n%s", out)
	}
}

func TestPGs(t *testing.T) {
	var st pgStat
	mustJSON(t, `{"pg_ready":true,"pg_summary":{"num_pg_by_state":[{"name":"active+clean","num":120},{"name":"undersized+degraded+peered","num":9}],"num_pgs":129}}`, &st)
	var stuck pgStuck
	mustJSON(t, `{"pg_ready":true,"stuck_pg_stats":[{"pgid":"2.1f","state":"undersized+degraded+peered","up":[3],"acting":[3],"up_primary":3,"acting_primary":3,"last_clean":"2026-10-09T08:00:00"}]}`, &stuck)
	var bb blockedBy
	mustJSON(t, `{"osd_blocked_by":{"osd_blocked_by_infos":[{"id":2,"num_blocked":9}]}}`, &bb)
	out := renderPGs(st, stuck, nil, bb, nil, 50)
	for _, w := range []string{"PGs: 129", "9 undersized+degraded+peered", "Stuck PGs: 1", "2.1f", "osd.3", "osd.2 blocks 9 PGs"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestDaemons(t *testing.T) {
	orch := []orchDaemon{
		{DaemonName: "osd.10", DaemonType: "osd", Hostname: "node2.example.com", Status: 1, StatusDesc: "running"},
		{DaemonName: "osd.11", DaemonType: "osd", Hostname: "node2.example.com", Status: -1, StatusDesc: "error"},
		{DaemonName: "mon.node4", DaemonType: "mon", Hostname: "node4.example.com", Status: 1, StatusDesc: "running"},
	}
	out := renderOrch("Daemons (orchestrator)", orch, "", "", "problems", 50)
	for _, w := range []string{"osd            1/2 running — 1 NOT RUNNING", "Daemons not running: 1", "osd.11  node2.example.com  error"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if out := renderOrch("Daemons (orchestrator)", orch, "mon", "node4", "all", 50); !strings.Contains(out, "Daemons: 1") || !strings.Contains(out, "mon.node4") {
		t.Errorf("filters:\n%s", out)
	}
	vs := versions{"osd": {"ceph version 18.2.1 (a) reef (stable)": 70, "ceph version 18.2.7 (b) reef (stable)": 2},
		"overall": {"ceph version 18.2.1 (a) reef (stable)": 76, "ceph version 18.2.7 (b) reef (stable)": 2}}
	if out := renderVersions(vs, nil); !strings.Contains(out, "70 x 18.2.1, 2 x 18.2.7") || !strings.Contains(out, "MIXED VERSIONS") {
		t.Errorf("%s", out)
	}
}

func TestDaemonsFromMaps(t *testing.T) {
	var st clusterStatus
	mustJSON(t, `{"quorum_names":["a","b"],"monmap":{"num_mons":3}}`, &st)
	var mons monDump
	mustJSON(t, `{"mons":[{"rank":0,"name":"a"},{"rank":1,"name":"b"},{"rank":2,"name":"c"}]}`, &mons)
	var mgr mgrDump
	mustJSON(t, `{"active_name":"a","available":true,"standbys":[{"name":"b"},{"name":"c"}]}`, &mgr)
	var tree osdDF
	mustJSON(t, osdDFJSON, &tree)
	var fs fsStatus
	mustJSON(t, `{"mdsmap":[{"name":"mds.x","state":"active"},{"name":"mds.y","state":"up:replay"}]}`, &fs)
	rows := mapDaemons(st, mons, nil, mgr, nil, tree, nil, fs, nil)
	out := renderOrch("Daemons (cluster maps)", rows, "", "", "problems", 50)
	for _, w := range []string{"mon            2/3 running — 1 NOT RUNNING", "mgr            3/3 running", "osd            3/4 running", "mon.c", "OUT OF QUORUM", "osd.2", "host2", "down, out", "mds.y", "up:replay"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if out := renderOrch("Daemons (cluster maps)", rows, "osd", "host1", "all", 50); !strings.Contains(out, "Daemons: 2") || strings.Contains(out, "osd.2") {
		t.Errorf("filters:\n%s", out)
	}
}
