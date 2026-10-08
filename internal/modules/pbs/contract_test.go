package pbs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/aptrepo"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Contract tests: every recorded PBS API response set (testdata/contract/
// pbs-<version>, captured with test/contract/record) must decode strictly
// and render. A failure after adding a version means the API changed.
func strict(t *testing.T, dir, file string, v any) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Logf("%s: %s not recorded", dir, file)
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Errorf("%s/%s: strict decode failed (API format changed?): %v", dir, file, err)
		return false
	}
	return true
}

func TestContractPBS(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/contract/*")
	if len(dirs) == 0 {
		t.Fatal("no contract recordings")
	}
	now := time.Now()
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var st nodeStatus
			var v version
			var sub subscription
			var certs []certInfo
			if strict(t, dir, "node-status.json", &st) && strict(t, dir, "version.json", &v) &&
				strict(t, dir, "subscription.json", &sub) && strict(t, dir, "certificates.json", &certs) {
				out := renderNodeStatus(st, v, nil, sub, nil, certs, nil, now)
				if v.Version == "" || st.CPUInfo.CPUs == 0 || len(st.LoadAvg) == 0 || !strings.Contains(out, "Certificate:") {
					t.Errorf("node status incomplete:\n%s", out)
				}
				if c := evalCerts(certs, now); c.Status == registry.Unknown {
					t.Errorf("%+v", c)
				}
			}

			var cfgs []datastoreConfig
			var usage []datastoreUsage
			if strict(t, dir, "config-datastore.json", &cfgs) && strict(t, dir, "datastore-usage.json", &usage) {
				if len(cfgs) == 0 || len(usage) == 0 || usage[0].Total == 0 {
					t.Fatal("datastores not understood")
				}
				byName := map[string]*datastoreUsage{}
				for i := range usage {
					byName[usage[i].Store] = &usage[i]
				}
				var gc gcStatus
				strict(t, dir, "datastore-gc.json", &gc)
				ds := make([]datastore, len(cfgs))
				for i, c := range cfgs {
					ds[i] = datastore{Config: c, Usage: byName[c.Name], GC: &gc, FSKey: c.Name}
				}
				if out := renderDatastores(ds, now); strings.Count(out, "\n") <= len(ds) {
					t.Errorf("datastores not rendered:\n%s", out)
				}
				if c := evalDatastores(ds, now); c.Status == registry.Unknown {
					t.Errorf("%+v", c)
				}
				evalGC(ds)

				var groups []group
				var nss []struct {
					NS string `json:"ns"`
				}
				if strict(t, dir, "datastore-groups.json", &groups) && strict(t, dir, "datastore-namespace.json", &nss) {
					if len(groups) == 0 || groups[0].LastBackup == 0 || groups[0].BackupType == "" {
						t.Error("groups not understood")
					}
					if out := renderDatastore(ds[0], groups, nil, now); !strings.Contains(out, "group(s)") {
						t.Errorf("datastore detail:\n%s", out)
					}
				}
			}

			var js jobSet
			var pruneCfg []map[string]any
			if strict(t, dir, "admin-prune.json", &js.Prune) && strict(t, dir, "admin-verify.json", &js.Verify) &&
				strict(t, dir, "admin-sync.json", &js.Sync) && strict(t, dir, "config-prune.json", &pruneCfg) {
				strict(t, dir, "config-tape-backup-job.json", &js.Tape)
				if !strings.Contains(renderJobs(js, now), "Verify jobs:") {
					t.Error("jobs not rendered")
				}
				evalJobs(js, nil, now)
			}

			var tasks []task
			if strict(t, dir, "tasks.json", &tasks) {
				if len(tasks) == 0 || tasks[0].WorkerType == "" || !upidRe.MatchString(tasks[0].UPID) {
					t.Errorf("tasks not understood (UPID format changed?): %+v", tasks[:min(1, len(tasks))])
				}
				renderTasks(tasks)
				evalTasks(tasks)
			}

			var ups []aptrepo.Update
			var repos aptrepo.Repos
			if strict(t, dir, "apt-update.json", &ups) && strict(t, dir, "apt-repositories.json", &repos) {
				if len(repos.Standard) == 0 || !strings.Contains(aptrepo.Render(ups, nil, repos, nil, "Proxmox Backup Server"), "APT repositories:") {
					t.Error("repositories not understood")
				}
			}
			var pkgs []map[string]any
			strict(t, dir, "apt-versions.json", &pkgs)
		})
	}
}

// TestContractSurvivesTypeChanges simulates a future API change on a real
// recording: strict decoding must notice it, the tolerant decoder used by
// the module must still produce a usable report.
func TestContractSurvivesTypeChanges(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/contract/*")
	data, err := os.ReadFile(filepath.Join(dirs[0], "node-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	load := m["loadavg"].([]any)
	for i := range load {
		load[i] = fmt.Sprint(load[i]) // numbers -> strings
	}
	m["cpu"] = fmt.Sprint(m["cpu"])
	m["boot-info"].(map[string]any)["secureboot"] = 0.0
	changed, _ := json.Marshal(m)

	var st nodeStatus
	if json.Unmarshal(changed, &st) == nil {
		t.Fatal("strict decoding did not notice the type change")
	}
	st = nodeStatus{}
	if err := jsonx.Decode("test node-status", changed, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.LoadAvg) != 3 || st.CPUInfo.CPUs == 0 || st.Memory.Total == 0 {
		t.Fatalf("tolerant decoding lost data: %+v", st)
	}
	if out := renderNodeStatus(st, version{Version: "4.0"}, nil, subscription{}, nil, nil, nil, time.Now()); !strings.Contains(out, "load ") {
		t.Fatalf("%s", out)
	}
}
