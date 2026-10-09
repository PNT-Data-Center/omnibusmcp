package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/executor"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

const testFSID = "00000000-0000-0000-0000-000000000001"

var testNow = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

// fakeHost points the module's filesystem locations into a temp dir.
func fakeHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	oldLib, oldConfs, oldKeys, oldPVE, oldSys := varLibCeph, defaultConfs, defaultKeyrings, pveConf, sysBlock
	varLibCeph = filepath.Join(root, "var/lib/ceph")
	defaultConfs = []string{filepath.Join(root, "etc/ceph/ceph.conf")}
	defaultKeyrings = []string{filepath.Join(root, "etc/omnibusmcp/ceph.client.omnibusmcp.keyring"), filepath.Join(root, "etc/pve/priv/ceph.client.omnibusmcp.keyring")}
	pveConf = filepath.Join(root, "etc/pve/ceph.conf")
	sysBlock = filepath.Join(root, "sys/class/block")
	t.Cleanup(func() {
		varLibCeph, defaultConfs, defaultKeyrings, pveConf, sysBlock = oldLib, oldConfs, oldKeys, oldPVE, oldSys
	})
	return root
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliOK(string) error      { return nil }
func cliMissing(string) error { return errors.New("not found") }

func TestInstalled(t *testing.T) {
	root := fakeHost(t)
	// Client packages only (ceph-common on any Proxmox VE host): no module.
	os.MkdirAll(varLibCeph, 0o750)
	write(t, filepath.Join(root, "etc/ceph/rbdmap"), "")
	if installed() {
		t.Fatal("client-only host detected as Ceph")
	}
	write(t, filepath.Join(varLibCeph, testFSID, "osd.3", "config"), "")
	if !installed() {
		t.Fatal("cephadm OSD host not detected")
	}
}

func TestInstalledPVE(t *testing.T) {
	fakeHost(t)
	write(t, pveConf, "[global]\n")
	if !installed() {
		t.Fatal("Proxmox VE Ceph cluster not detected")
	}
}

func TestDiscoverCephadmWithoutAdminLabel(t *testing.T) {
	root := fakeHost(t)
	// No /etc/ceph/ceph.conf: the daemon's minimal config is used.
	write(t, filepath.Join(varLibCeph, testFSID, "mon.node6", "config"), "# minimal\n[global]\n\tfsid = "+testFSID+"\n\tmon_host = [v2:192.0.2.1:3300/0]\n")
	write(t, filepath.Join(varLibCeph, "removed", "x"), "")
	key := filepath.Join(root, "etc/omnibusmcp/ceph.client.omnibusmcp.keyring")
	write(t, key, "[client.omnibusmcp]\n")
	s := discover(config.Ceph{Cluster: config.CephClusterAuto}, cliOK)
	if s.Variant != VariantCephadm || s.FSID != testFSID || !strings.HasSuffix(s.Conf, "mon.node6/config") || s.Keyring != key || !s.Cluster {
		t.Fatalf("%+v", s)
	}
}

func TestDiscoverPVEAndLocalReasons(t *testing.T) {
	root := fakeHost(t)
	write(t, pveConf, "[global]\n\t fsid = "+testFSID+"\n")
	write(t, filepath.Join(root, "etc/ceph/ceph.conf"), "[global]\n\tfsid = "+testFSID+"\n")
	write(t, filepath.Join(varLibCeph, "osd", "ceph-0", "keyring"), "")

	s := discover(config.Ceph{Cluster: config.CephClusterAuto}, cliOK)
	if s.Variant != VariantPVE || s.FSID != testFSID || s.Cluster || !strings.Contains(s.LocalReason, KeyCommand) {
		t.Fatalf("no key: %+v", s)
	}
	pveKey := filepath.Join(root, "etc/pve/priv/ceph.client.omnibusmcp.keyring")
	write(t, pveKey, "")
	if s := discover(config.Ceph{Cluster: config.CephClusterAuto}, cliOK); !s.Cluster || s.Keyring != pveKey {
		t.Fatalf("key in /etc/pve/priv: %+v", s)
	}
	if s := discover(config.Ceph{Cluster: config.CephClusterOff}, cliOK); s.Cluster || !strings.Contains(s.LocalReason, "ceph.cluster is false") {
		t.Fatalf("cluster: false: %+v", s)
	}
	if s := discover(config.Ceph{Cluster: config.CephClusterAuto}, cliMissing); s.Cluster || !strings.Contains(s.LocalReason, "ceph-common") {
		t.Fatalf("no ceph CLI: %+v", s)
	}
	// Paths from the configuration take precedence.
	own := filepath.Join(root, "custom.keyring")
	if s := discover(config.Ceph{Cluster: config.CephClusterAuto, Keyring: own}, cliOK); s.Cluster || s.Keyring != "" {
		t.Fatalf("missing configured keyring must not fall back: %+v", s)
	}
	write(t, own, "")
	if s := discover(config.Ceph{Cluster: config.CephClusterAuto, Keyring: own}, cliOK); !s.Cluster || s.Keyring != own {
		t.Fatalf("configured keyring: %+v", s)
	}
}

func TestParseUnits(t *testing.T) {
	out := `● ceph-` + testFSID + `@alertmanager.node2.service loaded failed failed Ceph alertmanager.node2
ceph-` + testFSID + `@osd.10.service loaded active running Ceph osd.10
ceph-` + testFSID + `@osd.9.service loaded active running Ceph osd.9
ceph-` + testFSID + `@crash.node2.service loaded active running Ceph crash.node2
ceph-crash.service loaded active running Ceph crash dump collector
ceph-mon@host1.service loaded active running Ceph cluster monitor daemon
ceph-osd@0.service loaded activating auto-restart Ceph object storage daemon osd.0
ceph-volume@lvm-0-abc.service loaded inactive dead Ceph Volume activation
ceph-radosgw@rgw.host1.service loaded active running Ceph rados gateway
`
	units := parseUnits(out)
	var names []string
	for _, u := range units {
		names = append(names, u.Daemon)
	}
	want := "alertmanager.node2 crash crash.node2 mon.host1 osd.0 osd.9 osd.10 rgw.host1"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("daemons %q, want %q", got, want)
	}
	if !units[0].Failed() || !units[4].Failed() || units[5].Failed() {
		t.Fatalf("failed states: %+v", units)
	}

	markStale(units, []orchDaemon{{DaemonName: "osd.9"}, {DaemonName: "osd.10"}, {DaemonName: "crash.node2"}})
	if !units[0].Stale || units[1].Stale || units[2].Stale {
		t.Fatalf("stale: %+v", units)
	}
	c := evalDaemons(units)
	// osd.0 restarting is a real failure; the stale alertmanager is info.
	if c.Status != registry.Warn || !strings.Contains(c.Detail, "FAILED: osd.0") || !strings.Contains(c.Detail, "stale failed units of daemons no longer in the orchestrator: alertmanager.node2") {
		t.Fatalf("%+v", c)
	}
	if c := evalDaemons(units[:1]); c.Status != registry.OK {
		t.Fatalf("only a stale unit: %+v", c)
	}
}

func TestLocalOSDsAndCrashes(t *testing.T) {
	fakeHost(t)
	s := site{Variant: VariantCephadm, FSID: testFSID}
	// The OSD links point into /dev, which the sandboxed service cannot
	// see: only the link and sysfs are read.
	os.MkdirAll(filepath.Join(varLibCeph, testFSID, "osd.12"), 0o700)
	os.Symlink("/dev/ceph-1a2b-3c/osd-block-4d5e", filepath.Join(varLibCeph, testFSID, "osd.12", "block"))
	write(t, filepath.Join(sysBlock, "dm-3", "dm", "name"), "ceph--1a2b--3c-osd--block--4d5e\n")
	os.MkdirAll(filepath.Join(sysBlock, "dm-3", "slaves", "sdc"), 0o755)
	os.MkdirAll(filepath.Join(varLibCeph, testFSID, "osd.13"), 0o700)
	os.Symlink("/dev/nvme0n1", filepath.Join(varLibCeph, testFSID, "osd.13", "block"))
	os.MkdirAll(filepath.Join(sysBlock, "nvme0n1"), 0o755)
	os.MkdirAll(filepath.Join(varLibCeph, testFSID, "osd.2"), 0o700)

	osds := localOSDs(s)
	if len(osds) != 3 || osds[0].OSD != "osd.2" || osds[0].Errstr == "" || osds[1].OSD != "osd.12" || strings.Join(osds[1].Disks, ",") != "sdc" ||
		strings.Join(osds[2].Disks, ",") != "nvme0n1" {
		t.Fatalf("%+v", osds)
	}

	crashDir := filepath.Join(varLibCeph, testFSID, "crash")
	os.MkdirAll(filepath.Join(crashDir, "posted", "2026-01-01T00:00:00.000000Z_x"), 0o700)
	write(t, filepath.Join(crashDir, "2025-11-20T09:40:01.603597Z_aaaa", "meta"), `{"entity_name":"osd.12","timestamp":"2025-11-20"}`)
	write(t, filepath.Join(crashDir, "2026-10-01T10:00:00.000000Z_bbbb", "meta"), `{"entity_name":"mgr.node2.x"}`)
	crashes := localCrashes(s)
	if len(crashes) != 2 || crashes[0].Entity != "mgr.node2.x" || crashes[1].Entity != "osd.12" {
		t.Fatalf("%+v", crashes)
	}
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	if c := evalCrashes(crashes, now); c.Status != registry.Warn || !strings.HasPrefix(c.Detail, "1 recent") {
		t.Fatalf("%+v", c)
	}
	if c := evalCrashes(crashes[1:], now); c.Status != registry.OK || !strings.Contains(c.Detail, "1 crash reports") {
		t.Fatalf("old crash only: %+v", c)
	}

	out := renderLocal(s, nil, nil, osds, crashes, now)
	for _, want := range []string{"Installation: cephadm", "osd.12", "disk sdc", "/dev/ceph-1a2b-3c/osd-block-4d5e", "Unposted crash reports: 2 (1 in the last 30 days)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		r    executor.Result
		kind string
	}{
		{executor.Result{ExitCode: 1, Stderr: "2026-10-09T08:45:00 7f -1 monclient(hunting): authenticate timed out after 5\n[errno 110] RADOS timed out (error connecting to the cluster)"}, failUnreachable},
		{executor.Result{TimedOut: true, ExitCode: -1}, failUnreachable},
		{executor.Result{ExitCode: 13, Stderr: "auth: unable to find a keyring on /etc/ceph/ceph.client.admin.keyring\n[errno 13] RADOS permission denied (error connecting to the cluster)"}, failAccess},
		{executor.Result{ExitCode: 1, Stderr: "Error initializing cluster client: ObjectNotFound('RADOS object not found (error calling conf_read_file)')"}, failAccess},
		{executor.Result{NotFound: true, Err: errors.New("not found")}, failAccess},
		{executor.Result{ExitCode: 2, Stderr: "Error ENOENT: No orchestrator configured (try `ceph orch set backend`)"}, failCommand},
	}
	for _, c := range cases {
		err := classify(c.r)
		if failKind(err) != c.kind {
			t.Errorf("%+v: got %s (%v), want %s", c.r, failKind(err), err, c.kind)
		}
	}
	if c := connectionCheck(classify(cases[0].r)); c.Status != registry.Crit || !strings.Contains(c.Detail, "quorum") {
		t.Errorf("unreachable: %+v", c)
	}
	if c := connectionCheck(classify(cases[2].r)); c.Status != registry.Warn || !strings.Contains(c.Detail, "permission denied") {
		t.Errorf("access: %+v", c)
	}
}

func TestDecodeFirst(t *testing.T) {
	var v struct {
		PGReady bool `json:"pg_ready"`
	}
	// "ceph pg dump_stuck" prints "ok" after the JSON; json-pretty adds a
	// leading blank line.
	if err := decodeFirst("t", []byte("\n{\n  \"pg_ready\": true,\n  \"stuck_pg_stats\": []\n}\nok\n"), &v); err != nil || !v.PGReady {
		t.Fatalf("%v %+v", err, v)
	}
	if err := decodeFirst("t", []byte("not json"), &v); err == nil {
		t.Fatal("accepted garbage")
	}
}

// fakeCLI answers queries from a map keyed by the command after the
// connection arguments.
func fakeCLI(t *testing.T, answers map[string]string, calls *[]string) {
	t.Helper()
	old := runCLI
	cacheMu.Lock()
	cache = map[string]*cliResult{}
	cacheMu.Unlock()
	runCLI = func(_ context.Context, _ *registry.Env, argv []string) ([]byte, error) {
		if argv[0] != "--name" || argv[1] != clientName || argv[len(argv)-2] != "--format" {
			t.Errorf("unexpected argv %q", argv)
		}
		cmd := strings.Join(argv[8:len(argv)-2], " ")
		if calls != nil {
			*calls = append(*calls, cmd)
		}
		if a, ok := answers[cmd]; ok {
			return []byte(a), nil
		}
		return nil, &cliError{Kind: failCommand, Msg: "Error ENOENT: " + cmd}
	}
	t.Cleanup(func() { runCLI = old })
}

const statusJSON = `{"fsid":"` + testFSID + `","health":{"status":"HEALTH_WARN","checks":{"OSD_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1 osds down","count":1},"muted":false},"MON_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1/3 mons down, quorum a,b","count":1},"muted":false}},"mutes":[]},
"quorum_names":["a","b"],"quorum_age":3600,"monmap":{"epoch":3,"min_mon_release_name":"tentacle","num_mons":3},
"osdmap":{"epoch":10,"num_osds":12,"num_up_osds":11,"osd_up_since":1,"num_in_osds":12,"osd_in_since":1,"num_remapped_pgs":0},
"pgmap":{"pgs_by_state":[{"state_name":"active+clean","count":120},{"state_name":"active+undersized+degraded","count":9}],"num_pgs":129,"num_pools":2,"num_objects":695350,"data_bytes":2836515900441,"bytes_used":8484401033216,"bytes_avail":10719437934592,"bytes_total":19203838967808,"read_bytes_sec":2581042,"write_bytes_sec":4965308,"read_op_per_sec":424,"write_op_per_sec":180,"degraded_ratio":0.05},
"mgrmap":{"available":true,"num_standbys":2,"modules":["iostat"],"services":{}},"fsmap":{"epoch":1,"by_rank":[],"up:standby":0},"progress_events":{}}`

const healthDetailJSON = `{"status":"HEALTH_WARN","checks":{"OSD_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1 osds down","count":1},"detail":[{"message":"osd.3 (root=default,host=host2) is down"}],"muted":false},"MON_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1/3 mons down, quorum a,b","count":1},"detail":[{"message":"mon.c (rank 2) addr [v2:192.0.2.3:3300/0] is down (out of quorum)"}],"muted":false}},"mutes":[]}`

func TestStatusTool(t *testing.T) {
	fakeCLI(t, map[string]string{
		"status":        statusJSON,
		"health detail": healthDetailJSON,
		"mgr stat":      `{"epoch":456,"available":true,"active_name":"a","num_standby":2}`,
		"mon dump":      `{"epoch":3,"mons":[{"rank":0,"name":"a"},{"rank":1,"name":"b"},{"rank":2,"name":"c"}]}` + "\ndumped monmap epoch 3\n",
	}, nil)
	m := &Module{}
	m.once.Do(func() {})
	m.s = site{Variant: VariantPVE, Cluster: true, Keyring: "/k", Conf: "/c"}
	out, err := m.statusTool(context.Background(), &registry.Env{Cfg: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Health:   HEALTH_WARN", "[WARN] OSD_DOWN: 1 osds down", "osd.3 (root=default,host=host2) is down",
		"2/3 in quorum: a, b; OUT OF QUORUM: c", "Managers: active a, 2 standby", "12 total, 11 up, 12 in — 1 DOWN",
		"9 active+undersized+degraded", "5.00% degraded", "(44.2% used)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestHealthChecks(t *testing.T) {
	var st clusterStatus
	if err := json.Unmarshal([]byte(statusJSON), &st); err != nil {
		t.Fatal(err)
	}
	if c := evalHealth(st.Health); c.Status != registry.Warn || !strings.Contains(c.Detail, "MON_DOWN: 1/3 mons down") {
		t.Errorf("%+v", c)
	}
	if c := evalQuorum(st); c.Status != registry.Warn || c.Detail != "2/3 monitors in quorum" {
		t.Errorf("%+v", c)
	}
	if c := evalOSDs(st); c.Status != registry.Warn || !strings.Contains(c.Detail, "1 down, 0 out") {
		t.Errorf("%+v", c)
	}
	if c := evalPGs(st); c.Status != registry.Warn || !strings.Contains(c.Detail, "9 active+undersized+degraded") {
		t.Errorf("%+v", c)
	}
	st.Pgmap.PGsByState = append(st.Pgmap.PGsByState, struct {
		StateName string `json:"state_name"`
		Count     int    `json:"count"`
	}{"undersized+degraded+peered", 2})
	if c := evalPGs(st); c.Status != registry.Crit {
		t.Errorf("inactive PGs: %+v", c)
	}
	st.Pgmap.PGsByState = st.Pgmap.PGsByState[:1]
	st.Pgmap.PGsByState[0].StateName = "active+clean+scrubbing+deep"
	if c := evalPGs(st); c.Status != registry.OK {
		t.Errorf("scrubbing is fine: %+v", c)
	}
	if c := evalHealth(healthState{Status: "HEALTH_ERR"}); c.Status != registry.Crit {
		t.Errorf("%+v", c)
	}
}

func TestQueryCacheAndLocalOnly(t *testing.T) {
	var calls []string
	fakeCLI(t, map[string]string{"status": statusJSON}, &calls)
	s := site{Cluster: true, Keyring: "/k", Conf: "/c"}
	env := &registry.Env{Cfg: config.Default()}
	var a, b clusterStatus
	if err := query(context.Background(), env, s, &a, "status"); err != nil {
		t.Fatal(err)
	}
	if err := query(context.Background(), env, s, &b, "status"); err != nil || len(calls) != 1 {
		t.Fatalf("second identical query not cached: %v %q", err, calls)
	}
	if err := query(context.Background(), env, site{LocalReason: "no key"}, &a, "status"); failKind(err) != failAccess || len(calls) != 1 {
		t.Fatalf("local-only site queried the cluster: %v", err)
	}
}

func TestToolsDependOnClusterView(t *testing.T) {
	env := &registry.Env{Cfg: config.Default()}
	local := &Module{}
	local.once.Do(func() {})
	local.s = site{LocalReason: "no key"}
	if tools := local.Tools(env); len(tools) != 2 || tools[0].Name != "ceph_local" || tools[1].Name != "ceph_logs" {
		t.Fatalf("local view tools: %+v", tools)
	}
	full := &Module{}
	full.once.Do(func() {})
	full.s = site{Cluster: true}
	if tools := full.Tools(env); len(tools) < 2 {
		t.Fatalf("cluster view tools: %d", len(tools))
	}
}

func TestDominantVersion(t *testing.T) {
	v := dominantVersion(map[string]int{
		"ceph version 18.2.1 (7fe91d) reef (stable)": 84,
		"ceph version 18.2.7 (6b0e98) reef (stable)": 3,
	})
	if v != "18.2.1" {
		t.Fatal(v)
	}
}
