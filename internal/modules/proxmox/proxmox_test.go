package proxmox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

func loadResources(t *testing.T) []resource {
	t.Helper()
	data, err := os.ReadFile("testdata/resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var res []resource
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRenderGuests(t *testing.T) {
	out := renderGuests(loadResources(t), "", "")
	for _, want := range []string{"4 shown", "qemu/running=1", "qemu/stopped=2", "lxc/running=1", "web", "12.5% of 2", "1.0 GiB / 2.0 GiB", "started"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "local-lvm") {
		t.Error("storage listed as guest")
	}
	out = renderGuests(loadResources(t), "lxc", "running")
	if !strings.Contains(out, "1 shown") || strings.Contains(out, "\n100 ") {
		t.Errorf("filter not applied:\n%s", out)
	}
}

func TestEvalGuests(t *testing.T) {
	c := evalGuests(loadResources(t))
	if c.Status != registry.OK || c.Detail != "2 running, 1 stopped, 1 template(s)" {
		t.Fatalf("%+v", c)
	}
	res := append(loadResources(t), resource{Type: "qemu", VMID: 300, Status: "paused"}, resource{Type: "qemu", VMID: 301, Status: "running", HAState: "error"})
	if c := evalGuests(res); c.Status != registry.Warn || !strings.Contains(c.Detail, "300(paused)") || !strings.Contains(c.Detail, "301(HA error)") {
		t.Fatalf("%+v", c)
	}
}

func TestEvalStorage(t *testing.T) {
	c := evalStorage(loadResources(t))
	if c.Status != registry.Crit || !strings.Contains(c.Detail, "nfs@pve1 unknown") || !strings.Contains(c.Detail, "local-lvm@pve1 88%") {
		t.Fatalf("%+v", c)
	}
	ok := []resource{{Type: "storage", Storage: "local", Node: "n", Status: "available", Disk: 10, MaxDisk: 100}}
	if c := evalStorage(ok); c.Status != registry.OK || !strings.Contains(c.Detail, "highest usage 10% (local@n)") {
		t.Fatalf("%+v", c)
	}
}

func TestRenderStorage(t *testing.T) {
	out := renderStorage(loadResources(t))
	for _, want := range []string{"local-lvm", "lvmthin", "88.0%", "nfs", "unknown", "yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestEvalCluster(t *testing.T) {
	standalone := []clusterEntry{{Type: "node", Name: "pve1", Online: 1, Local: 1}}
	if c := evalCluster(standalone, false); c.Status != registry.OK || !strings.Contains(c.Detail, "standalone") {
		t.Fatalf("%+v", c)
	}
	quorate := []clusterEntry{{Type: "cluster", Name: "lab", Quorate: 1, Nodes: 3}, {Type: "node", Name: "a", Online: 1}, {Type: "node", Name: "b", Online: 1}, {Type: "node", Name: "c", Online: 0}}
	if c := evalCluster(quorate, true); c.Status != registry.Warn || !strings.Contains(c.Detail, "offline node(s): c") {
		t.Fatalf("%+v", c)
	}
	lost := []clusterEntry{{Type: "cluster", Name: "lab", Quorate: 0, Nodes: 3}, {Type: "node", Name: "a", Online: 1}, {Type: "node", Name: "b", Online: 0}, {Type: "node", Name: "c", Online: 0}}
	if c := evalCluster(lost, true); c.Status != registry.Crit || !strings.Contains(c.Detail, "NOT quorate (1 of 3") {
		t.Fatalf("%+v", c)
	}
	if c := evalCluster(standalone, true); c.Status != registry.Crit {
		t.Fatalf("corosync.conf without cluster entry: %+v", c)
	}
	out := renderCluster(standalone, []haEntry{{ID: "quorum", Status: "OK"}}, nil, false)
	if !strings.Contains(out, "standalone node") || !strings.Contains(out, "quorum: OK") {
		t.Fatalf("%s", out)
	}
}

func TestEvalTasks(t *testing.T) {
	tasks := []task{
		{Type: "vzdump", Status: "OK", StartTime: 1, EndTime: 1},
		{Type: "qmstart", Status: "start failed: QEMU exited with code 1", StartTime: 1, EndTime: 2},
		{Type: "qmstart", Status: "timeout", StartTime: 1, EndTime: 2},
		{Type: "vzdump", Status: "WARNINGS: 1", StartTime: 1, EndTime: 2},
		{Type: "qmigrate", Status: "", StartTime: 1}, // running
	}
	c := evalTasks(tasks)
	if c.Status != registry.Warn || !strings.Contains(c.Detail, "2 failed task(s)") || !strings.Contains(c.Detail, "qmstart x2") {
		t.Fatalf("%+v", c)
	}
	if c := evalTasks(tasks[:1]); c.Status != registry.OK {
		t.Fatalf("%+v", c)
	}
	out := renderTasks(tasks)
	if !strings.Contains(out, "running") || !strings.Contains(out, "<1s") {
		t.Fatalf("%s", out)
	}
}

func TestPmxcfsMounted(t *testing.T) {
	mounted := "36 29 0:33 / /etc/pve rw,nosuid,nodev,relatime shared:31 - fuse /dev/fuse rw,user_id=0\n" +
		"27 1 252:1 / / rw,relatime shared:1 - ext4 /dev/mapper/pve-root rw\n"
	if !pmxcfsMounted(mounted) {
		t.Error("mounted pmxcfs not detected")
	}
	if pmxcfsMounted("27 1 252:1 / / rw,relatime shared:1 - ext4 /dev/mapper/pve-root rw\n") {
		t.Error("detected without mount")
	}
	if pmxcfsMounted("40 1 0:5 / /etc/pve rw - ext4 /dev/sda1 rw\n") {
		t.Error("non-fuse /etc/pve accepted")
	}
}

func TestConfigValueRedaction(t *testing.T) {
	cases := map[string]struct {
		key  string
		val  any
		want string
	}{
		"sshkeys":  {"sshkeys", "ssh-ed25519%20AAAA%20a%40b%0Assh-rsa%20BBBB%20c%40d%0A", "(2 public key(s), not shown)"},
		"password": {"cipassword", "$5$abc", "*** (redacted)"},
		"plain":    {"memory", "2048", "2048"},
		"net":      {"net0", "virtio=AA:BB,bridge=vmbr0", "virtio=AA:BB,bridge=vmbr0"},
	}
	for name, c := range cases {
		if got := configValue(c.key, c.val); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestRenderGuest(t *testing.T) {
	g := resource{Type: "qemu", VMID: 100, Name: "web", Node: "pve1", Status: "running"}
	cfg := map[string]any{"memory": "2048", "cipassword": "x", "digest": "abc", "sshkeys": "ssh-rsa%20A%0A"}
	cur := map[string]any{"status": "running", "uptime": 3600.0, "mem": 1073741824.0, "cpu": 0.5}
	out := renderGuest(g, cfg, cur, nil)
	for _, want := range []string{`qemu 100 "web" on node pve1: running`, "uptime:", "1h 0m", "1.0 GiB", "50.0%", "memory: 2048", "cipassword: *** (redacted)", "(1 public key(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "digest") {
		t.Error("digest shown")
	}
}

func TestRenderTaskLog(t *testing.T) {
	var log []logLine
	for i := 1; i <= 250; i++ {
		log = append(log, logLine{N: i, T: "line " + string(rune('a'+i%26))})
	}
	log[249].T = "TASK ERROR: boom"
	out := renderTaskLog(task{Type: "vzdump", Status: "boom", User: "root@pam", Node: "pve1", StartTime: 1}, log, 100)
	if !strings.Contains(out, "last 100 of 250") || !strings.HasSuffix(out, "TASK ERROR: boom\n") {
		t.Fatalf("%s", out)
	}
}

func TestUPIDValidation(t *testing.T) {
	ok := []string{
		"UPID:ai:002F3304:1078E9F5:6AA7E60B:qmshutdown:100:root@pam:",
		"UPID:pve-1.lab:0000ABCD:00112233:65F0A1B2:vzdump::root@pam!backup:",
	}
	for _, u := range ok {
		if !upidRe.MatchString(u) {
			t.Errorf("rejected %q", u)
		}
	}
	bad := []string{"", "UPID:ai", "UPID:ai:1:2:3:x:y:z:", "UPID:../x:002F3304:1078E9F5:6AA7E60B:t:1:root@pam:", "UPID:ai:002F3304:1078E9F5:6AA7E60B:qm start:100:root@pam:"}
	for _, u := range bad {
		if upidRe.MatchString(u) {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestRenderUpdates(t *testing.T) {
	var repos repoInfo
	json.Unmarshal([]byte(`{"files":[{"path":"/etc/apt/sources.list.d/pve-enterprise.sources","repositories":[{"Types":["deb"],"URIs":["https://enterprise.proxmox.com/debian/pve"],"Suites":["trixie"],"Components":["pve-enterprise"],"Enabled":true}]}],
	 "errors":[],"infos":[{"path":"/etc/apt/sources.list","index":0,"kind":"origin","message":"Debian"},{"path":"/etc/apt/sources.list.d/pve-enterprise.sources","index":0,"kind":"warning","message":"no subscription key"}],
	 "standard-repos":[{"handle":"enterprise","name":"Enterprise","status":1},{"handle":"no-subscription","name":"No-Subscription"},{"handle":"test","name":"Test"}]}`), &repos)
	out := renderUpdates([]aptUpdate{{Package: "pve-manager", OldVersion: "9.2.1", Version: "9.2.2", Origin: "Proxmox"}}, nil, repos, nil)
	for _, want := range []string{"Pending updates: 1", "pve-manager", "9.2.1", "[enabled] deb https://enterprise.proxmox.com", `"Enterprise": enabled`, "not configured: No-Subscription, Test", "WARNING: no subscription key"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Debian (") {
		t.Error("origin info shown")
	}
}

func TestRenderNodeStatus(t *testing.T) {
	var st nodeStatus
	err := json.Unmarshal([]byte(`{"pveversion":"pve-manager/9.2.2/abc","kversion":"Linux 7.0","uptime":90061,"cpu":0.05,"wait":0.001,"loadavg":["0.1","0.2","0.3"],
	 "cpuinfo":{"model":"Xeon","sockets":2,"cores":18,"cpus":72,"mhz":"2600.000"},"memory":{"total":1000,"used":250},"swap":{"total":0,"used":0},
	 "rootfs":{"total":100,"used":9},"ksm":{"shared":0},"boot-info":{"mode":"efi","secureboot":0},"current-kernel":{"release":"7.0.2-6-pve"}}`), &st)
	if err != nil {
		t.Fatal(err)
	}
	out := renderNodeStatus("pve1", st, "PVE firewall DISABLED at datacenter level")
	for _, want := range []string{"pve-manager/9.2.2", "7.0.2-6-pve", "1d 1h", "2 socket(s) x 18 cores, 72 threads", "CPU usage:   5.0%", "(25.0%)", "efi", "Firewall:    PVE firewall DISABLED"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestParseSince(t *testing.T) {
	for _, s := range []string{"24h", "7d"} {
		if _, err := parseSince(s); err != nil {
			t.Error(s, err)
		}
	}
	for _, s := range []string{"", "1w", "-1h", "24"} {
		if _, err := parseSince(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestTruncate(t *testing.T) {
	s := strings.Repeat("line\n", 1000)
	out := truncate(s, 1024)
	if len(out) > 1100 || !strings.Contains(out, "output truncated") {
		t.Fatalf("len %d", len(out))
	}
	if truncate("short", 1024) != "short" {
		t.Fatal("short text changed")
	}
}

// fakePvesh replaces the command runner for concurrency tests.
func fakePvesh(t *testing.T, delay time.Duration, fail bool) (calls, peak *int64) {
	t.Helper()
	calls, peak = new(int64), new(int64)
	var active int64
	orig := runPvesh
	runPvesh = func(ctx context.Context, _ *registry.Env, argv []string) ([]byte, error) {
		atomic.AddInt64(calls, 1)
		n := atomic.AddInt64(&active, 1)
		defer atomic.AddInt64(&active, -1)
		for {
			p := atomic.LoadInt64(peak)
			if n <= p || atomic.CompareAndSwapInt64(peak, p, n) {
				break
			}
		}
		time.Sleep(delay)
		if fail {
			return nil, errors.New("boom")
		}
		return []byte(`[{"type":"node","name":"` + argv[1] + `"}]`), nil
	}
	t.Cleanup(func() {
		runPvesh = orig
		cacheMu.Lock()
		cache = map[string]*apiResult{}
		cacheMu.Unlock()
	})
	return calls, peak
}

func TestPveshLimitsConcurrency(t *testing.T) {
	calls, peak := fakePvesh(t, 30*time.Millisecond, false)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out []clusterEntry
			// distinct paths: no sharing, only the slot limit applies
			if err := pvesh(context.Background(), nil, &out, "/p"+string(rune('a'+i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if *calls != 10 || *peak > apiSlots {
		t.Fatalf("calls=%d peak=%d (limit %d)", *calls, *peak, apiSlots)
	}
}

func TestPveshSharesIdenticalCalls(t *testing.T) {
	calls, _ := fakePvesh(t, 50*time.Millisecond, false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out []clusterEntry
			if err := pvesh(context.Background(), nil, &out, "/cluster/status"); err != nil || len(out) != 1 {
				t.Error(err, out)
			}
		}()
	}
	wg.Wait()
	var out []clusterEntry
	pvesh(context.Background(), nil, &out, "/cluster/status") // within TTL: cached
	if *calls != 1 {
		t.Fatalf("identical calls ran %d times, want 1", *calls)
	}
}

func TestPveshDoesNotCacheErrors(t *testing.T) {
	calls, _ := fakePvesh(t, 0, true)
	var out []clusterEntry
	for i := 0; i < 3; i++ {
		if err := pvesh(context.Background(), nil, &out, "/cluster/status"); err == nil {
			t.Fatal("expected error")
		}
	}
	if *calls != 3 {
		t.Fatalf("failed call cached: %d runs", *calls)
	}
}

func TestPveshRespectsContextWhileQueued(t *testing.T) {
	fakePvesh(t, 300*time.Millisecond, false)
	var busy sync.WaitGroup
	defer busy.Wait() // before cleanup restores runPvesh
	for i := 0; i < apiSlots; i++ {
		busy.Add(1)
		go func() {
			defer busy.Done()
			var out []clusterEntry
			pvesh(context.Background(), nil, &out, "/busy"+string(rune('a'+i)))
		}()
	}
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var out []clusterEntry
	start := time.Now()
	err := pvesh(ctx, nil, &out, "/queued")
	if err == nil || !strings.Contains(err.Error(), "free API slot") || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestEvalUpdatesRepositories(t *testing.T) {
	var none, ok repoInfo
	json.Unmarshal([]byte(`{"standard-repos":[{"name":"Enterprise","status":0},{"name":"No-Subscription"},{"name":"Ceph Squid Enterprise","status":1}]}`), &none)
	json.Unmarshal([]byte(`{"standard-repos":[{"name":"Enterprise","status":0},{"name":"No-Subscription","status":1}]}`), &ok)
	if c := evalUpdates(make([]aptUpdate, 3), none, nil); c.Status != registry.Warn || !strings.Contains(c.Detail, "no Proxmox VE repository enabled") {
		t.Fatalf("Ceph-only repo must not count: %+v", c)
	}
	if c := evalUpdates(nil, ok, nil); c.Status != registry.OK || c.Detail != "0 pending package update(s)" {
		t.Fatalf("%+v", c)
	}
	if c := evalUpdates(nil, repoInfo{}, errors.New("x")); c.Status != registry.OK || !strings.Contains(c.Detail, "unavailable") {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(renderUpdates(nil, nil, none, nil), "WARNING: no Proxmox VE repository is enabled") {
		t.Fatal("render warning missing")
	}
}

func TestRenderGuestMultilineAndTaskResult(t *testing.T) {
	out := renderGuest(resource{Type: "qemu", VMID: 1, Name: "x", Node: "n"}, map[string]any{"description": "* Rola: web\n* Sieć: vmbr0\n"}, nil, nil)
	if !strings.Contains(out, "description: * Rola: web\n      | * Sieć: vmbr0\n") {
		t.Fatalf("%q", out)
	}
	log := renderTaskLog(task{Type: "qmshutdown", Status: "stopped", ExitStatus: "received interrupt"}, nil, 10)
	if !strings.Contains(log, "Status:   stopped, result: received interrupt") {
		t.Fatalf("%s", log)
	}
}

func TestParseFwOptions(t *testing.T) {
	cfg := "[OPTIONS]\n# comment\nenable: 1   # on\npolicy_in: DROP\n\n[RULES]\nIN ACCEPT -p tcp -dport 22\nenable: 0\n"
	opts := parseFwOptions(bufio.NewScanner(strings.NewReader(cfg)))
	if opts["enable"] != "1" || opts["policy_in"] != "DROP" || len(opts) != 2 {
		t.Fatalf("%v", opts)
	}
	if o, err := fwOptions("/nonexistent/cluster.fw"); err != nil || len(o) != 0 {
		t.Fatalf("missing file: %v %v", o, err)
	}
}

func TestFirewallCheck(t *testing.T) {
	cases := []struct {
		st     fwState
		status registry.Status
		want   string
	}{
		{fwState{Daemon: "pve-firewall", DaemonState: "active"}, registry.OK, "DISABLED at datacenter level"},
		{fwState{DCEnabled: true, Daemon: "pve-firewall", DaemonState: "active"}, registry.OK, "DISABLED for this node"},
		{fwState{DCEnabled: true, HostEnabled: true, Daemon: "pve-firewall", DaemonState: "active"}, registry.OK, "enabled and enforced (iptables via pve-firewall)"},
		{fwState{DCEnabled: true, HostEnabled: true, NFTables: true, Daemon: "proxmox-firewall", DaemonState: "inactive"}, registry.Warn, "proxmox-firewall is inactive: rules are NOT enforced"},
		{fwState{ReadErr: errors.New("permission denied")}, registry.Unknown, "cannot read"},
	}
	for _, c := range cases {
		got := c.st.check()
		if got.Status != c.status || !strings.Contains(got.Detail, c.want) {
			t.Errorf("%+v -> %+v", c.st, got)
		}
	}
}
