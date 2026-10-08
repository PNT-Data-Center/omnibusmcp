package pbs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

var now = time.Unix(1790800000, 0)

func ds(name, path, fs string, used, total int64, opts ...func(*datastore)) datastore {
	d := datastore{Config: datastoreConfig{Name: name, Path: path}, FSKey: fs,
		Usage: &datastoreUsage{Store: name, Used: used, Total: total, Avail: total - used, MountStatus: "nonremovable", EstimatedFull: 1074167634},
		GC:    &gcStatus{LastRunState: "ok", LastRunEnd: now.Unix() - 3600}}
	for _, o := range opts {
		o(&d)
	}
	return d
}

func TestEvalDatastoresSharedFSAndEstimates(t *testing.T) {
	stores := []datastore{
		ds("hdd-a", "/mnt/hdd/a", "1", 175, 1000),
		ds("hdd-b", "/mnt/hdd/b", "1", 175, 1000),
		ds("hdd-c", "/mnt/hdd/c", "1", 175, 1000),
		ds("ssd", "/mnt/ssd/s", "2", 227, 1000),
	}
	c := evalDatastores(stores, now)
	if c.Status != registry.OK || !strings.Contains(c.Detail, "4 datastore(s), highest usage 23% (ssd)") {
		t.Fatalf("%+v", c)
	}
	// Full filesystem shared by three stores is reported once, with all names.
	full := []datastore{ds("hdd-a", "/a", "1", 960, 1000), ds("hdd-b", "/b", "1", 960, 1000), ds("ssd", "/s", "2", 100, 1000)}
	c = evalDatastores(full, now)
	if c.Status != registry.Crit || strings.Count(c.Detail, "96% full") != 1 || !strings.Contains(c.Detail, "hdd-a (+hdd-b, shared filesystem) 96% full") {
		t.Fatalf("%+v", c)
	}
	soon := ds("x", "/x", "3", 100, 1000, func(d *datastore) { d.Usage.EstimatedFull = now.Add(10 * 24 * time.Hour).Unix() })
	maint := ds("y", "/y", "4", 100, 1000, func(d *datastore) { d.Config.MaintenanceMode = "read-only" })
	unmounted := ds("z", "/z", "5", 0, 0, func(d *datastore) { d.Usage.MountStatus = "notmounted" })
	c = evalDatastores([]datastore{soon, maint, unmounted}, now)
	for _, want := range []string{"x estimated full on", "y in maintenance mode (read-only)", "z mount status notmounted"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("missing %q in %q", want, c.Detail)
		}
	}
	if c.Status != registry.Crit {
		t.Fatalf("%+v", c)
	}
}

func TestEstimatedFullIgnoresPast(t *testing.T) {
	if estimatedFull(1074167634, now) != "" || estimatedFull(0, now) != "" {
		t.Fatal("past/unknown estimate shown")
	}
	if estimatedFull(now.Add(48*time.Hour).Unix(), now) == "" {
		t.Fatal("future estimate hidden")
	}
}

func TestEvalGC(t *testing.T) {
	ok := []datastore{ds("a", "/a", "1", 1, 10), ds("b", "/b", "2", 1, 10, func(d *datastore) { d.GC.LastRunState = "" })}
	if c := evalGC(ok); c.Status != registry.OK || !strings.Contains(c.Detail, "OK on 1 of 2") {
		t.Fatalf("%+v", c)
	}
	bad := []datastore{
		ds("a", "/a", "1", 1, 10, func(d *datastore) { d.GC.StillBad = 3 }),
		ds("b", "/b", "2", 1, 10, func(d *datastore) { d.GC.LastRunState = "unable to acquire lock" }),
	}
	c := evalGC(bad)
	if c.Status != registry.Crit || !strings.Contains(c.Detail, "a has 3 bad chunk(s)") || !strings.Contains(c.Detail, "b last GC: unable to acquire lock") {
		t.Fatalf("%+v", c)
	}
}

func TestEvalBackups(t *testing.T) {
	stores := []datastore{ds("fresh", "/f", "1", 1, 10), ds("stalled", "/s", "2", 1, 10), ds("empty", "/e", "3", 0, 10)}
	groups := map[string][]group{
		"fresh":   {{BackupType: "vm", BackupID: "100", LastBackup: now.Unix() - 3600}, {BackupType: "vm", BackupID: "999", LastBackup: now.Unix() - 30*86400}},
		"stalled": {{BackupType: "ct", BackupID: "200", LastBackup: now.Unix() - 5*86400}},
		"empty":   nil,
	}
	c := evalBackups(stores, groups, map[string]error{}, now)
	if c.Status != registry.Warn || !strings.Contains(c.Detail, "stalled: no backup for 5d") || !strings.Contains(c.Detail, "3 group(s), 2 without a backup in 48h") || !strings.Contains(c.Detail, "fresh newest 1h 0m ago") {
		t.Fatalf("%+v", c)
	}
	delete(groups, "stalled")
	if c := evalBackups(stores[:1], groups, map[string]error{}, now); c.Status != registry.OK {
		t.Fatalf("a stale group alone must not warn: %+v", c)
	}
}

func TestEvalJobsTasksCerts(t *testing.T) {
	js := jobSet{Verify: []job{{ID: "v1", Store: "a", LastRunState: "OK", LastRunEnd: 1, NextRun: now.Unix() + 3600}, {ID: "v2", Store: "b", LastRunState: "verification failed - 2 errors", LastRunEnd: 1}}, Prune: []job{{ID: "p1", Store: "a", Retention: "last=3"}}}
	if c := evalJobs(js, []string{"a", "b"}, now); c.Status != registry.Warn || !strings.Contains(c.Detail, "verify job v2 (b) last run: verification failed") {
		t.Fatalf("%+v", c)
	}
	if c := evalJobs(jobSet{Prune: []job{{ID: "p", Store: "a", LastRunState: "OK", LastRunEnd: 1, Retention: "daily=7"}}}, []string{"a"}, now); c.Status != registry.OK || c.Detail != "1 prune, 0 verify, 0 sync job(s), no problems" {
		t.Fatalf("%+v", c)
	}
	// e.g. prune jobs created months ago, never run, no keep-* options, one for a deleted store.
	dead := jobSet{Prune: []job{
		{ID: "default-ssd", Store: "ssd", NextRun: now.Unix() - 300*86400, Retention: "none"},
		{ID: "default-hdd", Store: "hdd", NextRun: now.Unix() - 290*86400, Retention: "none"},
	}}
	c := evalJobs(dead, []string{"hdd"}, now)
	for _, want := range []string{`prune job default-ssd references missing datastore "ssd"`, "2 job(s) never ran although overdue", "2 prune job(s) without keep-* retention options"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("missing %q in %q", want, c.Detail)
		}
	}
	if c.Status != registry.Warn {
		t.Fatalf("%+v", c)
	}
	if retentionOf(map[string]any{"id": "x", "keep-last": 3.0, "keep-daily": 7.0}) != "last=3,daily=7" || retentionOf(map[string]any{"id": "x"}) != "none" {
		t.Fatal("retentionOf")
	}
	tasks := []task{{WorkerType: "backup", Status: "OK", EndTime: 2}, {WorkerType: "backup", Status: "connection error", EndTime: 2}, {WorkerType: "verificationjob", Status: "WARNINGS: 1", EndTime: 2}}
	if c := evalTasks(tasks); c.Status != registry.Warn || !strings.Contains(c.Detail, "1 failed task(s) in the last 24h: backup x1") {
		t.Fatalf("%+v", c)
	}
	certs := []certInfo{{Filename: "proxy.pem", NotAfter: now.Add(10 * 24 * time.Hour).Unix()}}
	if c := evalCerts(certs, now); c.Status != registry.Warn || !strings.Contains(c.Detail, "proxy.pem valid until") {
		t.Fatalf("%+v", c)
	}
	certs[0].NotAfter = now.Unix() - 1
	if c := evalCerts(certs, now); c.Status != registry.Crit || !strings.Contains(c.Detail, "EXPIRED") {
		t.Fatalf("%+v", c)
	}
}

func TestValidation(t *testing.T) {
	for _, u := range []string{
		`UPID:pbs1:0000041D:00001385:00001595:6ABD9475:backup:store\x2dhdd\x3avm-100:root@pam:`,
		`UPID:pbs1:001365F8:1A29178C:00000000:6ABDBE39:aptupdate::root@pam:`,
		`UPID:pbs1:0000041D:00001385:00001592:6ABD8660:garbage_collection:store1:sync@pbs!job:`,
	} {
		if !upidRe.MatchString(u) {
			t.Errorf("rejected %q", u)
		}
	}
	for _, u := range []string{"", "UPID:pbs1", `UPID:pbs1:0000041D:00001385:00001595:6ABD9475:backup:a/../b:root@pam:`, `UPID:pbs1:0000041D:00001385:00001595:6ABD9475:back up:x:root@pam:`,
		`UPID:ai:002F3304:1078E9F5:6AA7E60B:qmshutdown:100:root@pam:`} {
		if upidRe.MatchString(u) {
			t.Errorf("accepted %q", u)
		}
	}
	for _, s := range []string{"store-hdd", "store_1", "a.b"} {
		if validStore(s) != nil {
			t.Errorf("rejected %q", s)
		}
	}
	for _, s := range []string{"", "-x", "a/b", "..", "a..b", "a b"} {
		if validStore(s) == nil {
			t.Errorf("accepted %q", s)
		}
	}
	if _, err := taskArgs(tasksInput{Since: "1w"}); err == nil {
		t.Error("bad since accepted")
	}
	args, err := taskArgs(tasksInput{ErrorsOnly: true, Since: "24h", Type: "backup", Store: "ssd", Limit: 500})
	if err != nil || !strings.Contains(strings.Join(args, " "), "--limit 200 --errors 1 --since") || !strings.Contains(strings.Join(args, " "), "--typefilter backup --store ssd") {
		t.Fatalf("%v %v", args, err)
	}
}

func TestRenderers(t *testing.T) {
	stores := []datastore{ds("hdd-a", "/mnt/hdd/a", "1", 175<<30, 1000<<30), ds("hdd-b", "/mnt/hdd/b", "1", 175<<30, 1000<<30)}
	out := renderDatastores(stores, now)
	for _, want := range []string{"hdd-a", "17.5%", "not growing", "nonremovable", "1h 0m ago", "hdd-a shares one filesystem with hdd-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	groups := []group{{BackupType: "vm", BackupID: "100", LastBackup: now.Unix() - 3600, BackupCount: 10}, {BackupType: "ct", BackupID: "200", LastBackup: now.Unix() - 9*86400, BackupCount: 3}}
	d := ds("hdd-a", "/mnt/hdd/a", "1", 1, 10, func(d *datastore) { d.GC.DiskBytes, d.GC.IndexDataBytes = 100, 450 })
	groups[0].NS = "NS1"
	out = renderDatastore(d, groups, nil, now)
	for _, want := range []string{"2 group(s) (1 vm, 1 ct), 13 snapshot(s)", "by namespace: <root>: 1, NS1: 1", "deduplication factor 4.5", "1 group(s) without a backup in the last 48h", "<root> ct/200"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(renderDatastore(d, nil, nil, now), "none in any namespace (empty datastore)") {
		t.Error("empty store")
	}
	id := "store:vm/100"
	tl := renderTasks([]task{{UPID: "UPID:x", WorkerType: "backup", WorkerID: &id, User: "root@pam", Status: "OK", StartTime: 1, EndTime: 61}})
	if !strings.Contains(tl, "store:vm/100") || !strings.Contains(tl, "1m 0s") {
		t.Fatalf("%s", tl)
	}
	jobs := renderJobs(jobSet{Sync: []job{{ID: "s1", Store: "a", Remote: "pbs2", RemoteStore: "b"}}, Verify: []job{{ID: "v1", Store: "a", LastRunState: "OK", LastRunEnd: now.Unix() - 60}},
		Prune: []job{{ID: "p1", Store: "a", NextRun: now.Unix() - 86400, Retention: "none"}}}, now)
	for _, want := range []string{"RETENTION", "(OVERDUE)", "none", "pbs2:b", "v1", "1m 0s ago", "Tape backup jobs:\n  none configured"} {
		if !strings.Contains(jobs, want) {
			t.Errorf("missing %q in:\n%s", want, jobs)
		}
	}
	log := renderTaskLog(task{Type: "backup", ID: "s:vm/1", User: "root@pam", Status: "stopped", ExitStatus: "connection error"}, []logLine{{1, "a"}, {2, "TASK ERROR: connection error"}}, 1)
	if !strings.Contains(log, "stopped, result: connection error") || !strings.Contains(log, "last 1 of 2") {
		t.Fatalf("%s", log)
	}
}

func TestSharedWith(t *testing.T) {
	got := sharedWith([]datastore{ds("a", "/a", "1", 1, 1), ds("b", "/b", "1", 1, 1), ds("c", "/c", "2", 1, 1), ds("d", "/d", "", 1, 1)})
	if len(got["a"]) != 1 || got["a"][0] != "b" || len(got["c"]) != 0 || len(got["d"]) != 0 {
		t.Fatalf("%v", got)
	}
}

// --- API limiter/cache ---

func fakeAPI(t *testing.T, delay time.Duration) (calls, peak *int64) {
	t.Helper()
	calls, peak = new(int64), new(int64)
	var active int64
	orig := runAPI
	runAPI = func(ctx context.Context, _ *registry.Env, argv []string) ([]byte, error) {
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
		if strings.Contains(argv[2], "fail") {
			return nil, errors.New("boom")
		}
		return []byte(`{"version":"4.0"}`), nil
	}
	t.Cleanup(func() {
		runAPI = orig
		cacheMu.Lock()
		cache = map[string]*apiResult{}
		cacheMu.Unlock()
	})
	return calls, peak
}

func TestAPILimitAndCache(t *testing.T) {
	calls, peak := fakeAPI(t, 20*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v version
			api(context.Background(), nil, &v, "/p"+string(rune('a'+i)))
		}()
	}
	wg.Wait()
	if *peak > apiSlots {
		t.Fatalf("peak %d > %d", *peak, apiSlots)
	}
	*calls = 0
	for i := 0; i < 5; i++ {
		var v version
		if err := api(context.Background(), nil, &v, "/version"); err != nil || v.Version != "4.0" {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Fatalf("cached call ran %d times", *calls)
	}
	*calls = 0
	for i := 0; i < 3; i++ {
		var v version
		if api(context.Background(), nil, &v, "/fail") == nil {
			t.Fatal("expected error")
		}
	}
	if *calls != 3 {
		t.Fatalf("failure cached: %d", *calls)
	}
}
