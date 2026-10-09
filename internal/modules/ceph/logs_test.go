package ceph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineFilter(t *testing.T) {
	cluster := []string{
		"2026-10-09T08:51:54.227059+0200 mgr.host1 (mgr.212734130) 2743197 : cluster [DBG] pgmap v2743817: 129 pgs: 129 active+clean",
		"2026-10-09T08:52:00.100000+0200 mon.host1 (mon.0) 100 : cluster [WRN] Health check failed: 1 osds down (OSD_DOWN)",
		"2026-10-09T08:53:00.100000+0200 mon.host1 (mon.0) 101 : cluster [ERR] Health check failed: Reduced data availability (PG_AVAILABILITY)",
		"2026-10-09T08:54:00.100000+0200 mon.host1 (mon.0) 102 : cluster [INF] Health check cleared: OSD_DOWN",
	}
	pick := func(lines []string, f func(string) bool) []int {
		var out []int
		for i, l := range lines {
			if f(l) {
				out = append(out, i)
			}
		}
		return out
	}
	if got := pick(cluster, lineFilter("cluster", "warn", "")); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("warn: %v", got)
	}
	if got := pick(cluster, lineFilter("cluster", "error", "")); len(got) != 1 || got[0] != 2 {
		t.Errorf("error: %v", got)
	}
	if got := pick(cluster, lineFilter("cluster", "all", "osd_down")); len(got) != 2 {
		t.Errorf("grep: %v", got)
	}

	daemon := []string{
		"2026-10-06T00:03:01+02:00 host1 ceph-mon[106479]: 2026-10-06T00:03:01.236+0200 7381a09ff6c0 -1 mon.host1@2(peon) e6 *** Got Signal Hangup ***",
		"2026-10-09T08:51:54.227+0200 7f3e9d976700  0 log_channel(cluster) log [DBG] : pgmap v1",
		"2026-10-09T08:51:55.227+0200 7f3e9d976700  0 osd.3 1234 heartbeat_check: no reply from osd.2",
		"2026-10-09T08:51:56.227+0200 7f3e9d976700  5 osd.3 debug noise",
	}
	// Level 0 is informational: on monitors one line per command.
	if got := pick(daemon, lineFilter("osd.3", "warn", "")); len(got) != 1 || got[0] != 0 {
		t.Errorf("daemon warn: %v", got)
	}
	if got := pick(daemon, lineFilter("osd.3", "info", "")); len(got) != 2 || got[1] != 2 {
		t.Errorf("daemon info: %v", got)
	}
	if got := pick(daemon, lineFilter("osd.3", "error", "")); len(got) != 1 || got[0] != 0 {
		t.Errorf("daemon error: %v", got)
	}
	audit := []string{
		"2026-10-09T13:34:47+0200 node bash[1]: debug 2026-10-09T11:34:47.429+0000 7faf  0 log_channel(audit) log [DBG] : from='client.? 192.0.2.1:0/1' entity='client.omnibusmcp' cmd=[{\"prefix\": \"versions\"}]: dispatch",
		"2026-10-09T13:35:00+0200 node bash[1]: debug 2026-10-09T11:35:00.000+0000 7faf  0 log_channel(audit) log [INF] : from='client.admin' cmd=[{\"prefix\": \"osd set\", \"key\": \"noout\"}]: finished",
	}
	if got := pick(audit, lineFilter("audit", "info", "")); len(got) != 1 || got[0] != 1 {
		t.Errorf("audit info: %v", got)
	}
}

func TestMaskSecrets(t *testing.T) {
	cases := map[string]string{
		`from='client.admin' cmd='[{"prefix": "config-key set", "key": "mgr/dashboard/admin_password", "val": "Sup3r!"}]': dispatch`: `"val": "***"`,
		`cmd='[{"prefix": "config set", "who": "client.rgw", "name": "rgw_s3_secret", "value": "abc"}]'`:                             `"value": "***"`,
		`cmd='[{"prefix": "dashboard ac-user-create", "username": "x", "password": "hunter2"}]'`:                                     `"password": "***"`,
		`key = AQBxyzabcdefghijklmnopqrstuvwxyz0123456==`:                                                                            `***CEPH-KEY***`,
	}
	for in, want := range cases {
		got := maskSecrets(in)
		if !strings.Contains(got, want) || strings.Contains(got, "Sup3r!") || strings.Contains(got, "hunter2") || strings.Contains(got, `"abc"`) {
			t.Errorf("%s\n -> %s", in, got)
		}
	}
	// Ordinary config changes stay readable.
	plain := `cmd='[{"prefix": "config set", "who": "osd", "name": "osd_max_backfills", "value": "2"}]'`
	if maskSecrets(plain) != plain {
		t.Errorf("masked a harmless value: %s", maskSecrets(plain))
	}
}

func TestLogSources(t *testing.T) {
	root := fakeHost(t)
	old := varLogCeph
	varLogCeph = filepath.Join(root, "var/log/ceph")
	t.Cleanup(func() { varLogCeph = old })

	// Packages (Proxmox VE): files.
	pve := site{Variant: VariantPVE, FSID: testFSID}
	write(t, filepath.Join(varLogCeph, "ceph.log"), "x\n")
	write(t, filepath.Join(varLogCeph, "ceph-osd.3.log"), "x\n")
	if f := logFile(pve, "cluster"); !strings.HasSuffix(f, "/ceph.log") {
		t.Errorf("cluster: %q", f)
	}
	if f := logFile(pve, "osd.3"); !strings.HasSuffix(f, "/ceph-osd.3.log") {
		t.Errorf("osd.3: %q", f)
	}
	os.MkdirAll(filepath.Join(varLibCeph, "osd", "ceph-7"), 0o755)
	if u, ch := journalUnit(pve, "osd.7"); u != "ceph-osd@7.service" || ch != "" {
		t.Errorf("native unit: %q %q", u, ch)
	}

	// cephadm: empty files, journald units; cluster log from the local mon.
	adm := site{Variant: VariantCephadm, FSID: testFSID}
	write(t, filepath.Join(varLogCeph, testFSID, "ceph-osd.10.log"), "")
	os.MkdirAll(filepath.Join(varLibCeph, testFSID, "osd.10"), 0o700)
	if f := logFile(adm, "osd.10"); f != "" {
		t.Errorf("empty cephadm file used: %q", f)
	}
	if u, _ := journalUnit(adm, "osd.10"); u != "ceph-"+testFSID+"@osd.10.service" {
		t.Errorf("cephadm unit: %q", u)
	}
	if u, _ := journalUnit(adm, "cluster"); u != "" {
		t.Errorf("cluster log without a local mon: %q", u)
	}
	os.MkdirAll(filepath.Join(varLibCeph, testFSID, "mon.node6"), 0o700)
	if u, ch := journalUnit(adm, "audit"); u != "ceph-"+testFSID+"@mon.node6.service" || ch != "audit" {
		t.Errorf("audit via mon: %q %q", u, ch)
	}
	if u, _ := journalUnit(adm, "osd.99"); u != "" {
		t.Errorf("foreign daemon: %q", u)
	}
}

func TestTailFileAndRender(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	os.WriteFile(p, []byte("first line\nsecond [WRN] a\nthird [WRN] b\n"), 0o600)
	text, err := tailFile(p, 20) // starts mid "second" line: dropped
	if err != nil || strings.Contains(text, "second") || !strings.Contains(text, "third") {
		t.Fatalf("%q %v", text, err)
	}
	out := renderLog("f", "a [WRN] 1\nb [WRN] 2\nc [WRN] 3\n", lineFilter("cluster", "warn", ""), 2)
	if !strings.Contains(out, "3 matching lines, last 2 shown") || strings.Contains(out, "a [WRN]") {
		t.Errorf("%s", out)
	}
}

func TestCrashesAndCephFS(t *testing.T) {
	var list []crashEntry
	mustJSON(t, `[{"crash_id":"2026-10-01T10:00:00.000000Z_00000000-0000-0000-0000-000000000002","timestamp":"2026-10-01T10:00:00.000000Z","entity_name":"osd.3"},
{"crash_id":"2025-01-01T10:00:00.000000Z_00000000-0000-0000-0000-000000000003","timestamp":"2025-01-01T10:00:00.000000Z","entity_name":"mgr.a","archived":"2025-01-02 00:00:00"}]`, &list)
	out := renderCrashes(list, nil, false, 30, testNow)
	if !strings.Contains(out, "Crashes reported to the cluster: 2 (1 not archived)") || !strings.Contains(out, "2026-10-01 10:00  osd.3") {
		t.Errorf("%s", out)
	}
	if !crashIDRe.MatchString(list[0].ID) || crashIDRe.MatchString("../../etc") {
		t.Error("crash ID validation")
	}
	var ci crashInfo
	mustJSON(t, `{"crash_id":"x","entity_name":"osd.3","process_name":"ceph-osd","ceph_version":"18.2.1","utsname_hostname":"node2","assert_condition":"r == 0","assert_func":"f","assert_file":"a.cc","assert_line":42,"assert_msg":"boom","backtrace":["frame0","frame1"],"stack_sig":"abc"}`, &ci)
	if out := renderCrashInfo(ci); !strings.Contains(out, "Assertion: r == 0 in f (a.cc:42)") || !strings.Contains(out, "frame1") || !strings.Contains(out, "Stack signature: abc") {
		t.Errorf("%s", out)
	}

	var fsl fsList
	mustJSON(t, `[{"name":"fs1","metadata_pool":"fs1_meta","data_pools":["fs1_data"]}]`, &fsl)
	var st fsStatus
	mustJSON(t, `{"clients":[{"clients":5,"fs":"fs1"}],"mdsmap":[{"name":"mds.b","state":"standby"},{"name":"mds.a","rank":0,"state":"active","rate":12,"inos":734572,"caps":191}],"pools":[{"name":"fs1_meta","type":"metadata","used":1000,"avail":2000}]}`, &st)
	out = renderCephFS(fsl, st, nil)
	if !strings.Contains(out, "Clients of fs1: 5") || strings.Index(out, "mds.a") > strings.Index(out, "mds.b") || strings.Contains(out, "NO ACTIVE MDS") {
		t.Errorf("%s", out)
	}
	var standbyOnly fsStatus
	mustJSON(t, `{"mdsmap":[{"name":"mds.b","state":"standby"}]}`, &standbyOnly)
	if out := renderCephFS(fsl, standbyOnly, nil); !strings.Contains(out, "NO ACTIVE MDS") {
		t.Errorf("%s", out)
	}
}
