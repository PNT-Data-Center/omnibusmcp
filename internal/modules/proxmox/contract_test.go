package proxmox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Contract tests run every recorded API response set (testdata/contract/
// pve-<version>, captured with test/contract/record) through strict
// decoding and the module's renderers. A failure after adding a new
// version means the API format changed.
func contractDirs(t *testing.T) []string {
	dirs, _ := filepath.Glob("testdata/contract/*")
	if len(dirs) == 0 {
		t.Fatal("no contract recordings")
	}
	return dirs
}

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

func TestContractPVE(t *testing.T) {
	for _, dir := range contractDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var res []resource
			if strict(t, dir, "cluster-resources.json", &res) {
				if len(res) == 0 {
					t.Error("no resources")
				}
				for _, out := range []string{renderGuests(res, "", ""), renderStorage(res)} {
					if strings.Count(out, "\n") < 2 {
						t.Errorf("empty rendering:\n%s", out)
					}
				}
				if c := evalStorage(res); !strings.Contains(c.Detail, "storage") {
					t.Errorf("storage verdict: %+v", c)
				}
				evalGuests(res)
			}
			var status []clusterEntry
			if strict(t, dir, "cluster-status.json", &status) {
				if c := evalCluster(status, false); c.Detail == "" {
					t.Error("cluster verdict empty")
				}
			}
			var ha []haEntry
			strict(t, dir, "cluster-ha-status.json", &ha)
			var st nodeStatus
			if strict(t, dir, "node-status.json", &st) {
				out := renderNodeStatus("node1", st, "fw")
				if !strings.Contains(out, "pve-manager/") || st.CPUInfo.CPUs == 0 || st.Memory.Total == 0 {
					t.Errorf("node status incomplete:\n%s", out)
				}
			}
			var tasks []task
			if strict(t, dir, "node-tasks.json", &tasks) {
				if len(tasks) == 0 || !strings.Contains(renderTasks(tasks), "UPID:") {
					t.Error("tasks not rendered")
				}
				evalTasks(tasks)
			}
			var ups []aptUpdate
			var repos repoInfo
			if strict(t, dir, "apt-update.json", &ups) && strict(t, dir, "apt-repositories.json", &repos) {
				if !strings.Contains(renderUpdates(ups, nil, repos, nil), "APT repositories:") || len(repos.Standard) == 0 {
					t.Error("updates/repositories not understood")
				}
			}
			var jobs []map[string]any
			var uncovered []resource
			if strict(t, dir, "cluster-backup.json", &jobs) && strict(t, dir, "not-backed-up.json", &uncovered) {
				renderBackups(jobs, nil, uncovered, nil, nil, nil)
			}
		})
	}
}
