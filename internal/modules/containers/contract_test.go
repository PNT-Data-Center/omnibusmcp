package containers

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Contract tests: recorded Docker CLI output (testdata/contract/docker-<v>,
// captured with test/contract/record; JSON-lines outputs are stored as
// arrays) must decode strictly and render.
func strict(t *testing.T, dir, file string, v any) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Logf("%s: %s not recorded", dir, file)
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Errorf("%s/%s: strict decode failed (output format changed?): %v", dir, file, err)
		return false
	}
	return true
}

// asLines turns a recorded array back into docker's JSON-lines output.
func asLines(t *testing.T, dir, file string) string {
	t.Helper()
	var items []json.RawMessage
	if !strict(t, dir, file, &items) {
		return ""
	}
	parts := make([]string, len(items))
	for i, it := range items {
		var buf bytes.Buffer
		if err := json.Compact(&buf, it); err != nil {
			t.Fatalf("%s/%s: %v", dir, file, err)
		}
		parts[i] = buf.String() // docker prints one compact object per line
	}
	return strings.Join(parts, "\n")
}

func TestContractDocker(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/contract/*")
	if len(dirs) == 0 {
		t.Fatal("no contract recordings")
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var info map[string]any
			if strict(t, dir, "info.json", &info) {
				if out := renderRuntime(info); !strings.Contains(out, "Engine:          Docker") || strings.Contains(out, "Docker - (") {
					t.Errorf("info not understood:\n%s", out)
				}
			}
			ps, err := jsonLines[psEntry](asLines(t, dir, "ps.json"))
			if err != nil {
				t.Errorf("ps: %v", err)
			} else if len(ps) > 0 {
				if ps[0].State == "" || ps[0].Names == "" || ps[0].ID == "" {
					t.Errorf("ps fields missing: %+v", ps[0])
				}
				if c := evalContainers(ps, nil); c.Status == registry.Unknown {
					t.Errorf("%+v", c)
				}
				renderList(ps, "")
			}
			var insp []inspectInfo
			if strict(t, dir, "inspect.json", &insp) {
				for _, i := range insp {
					if i.State.Status == "" || !strings.Contains(renderInspect(i), "Container:") {
						t.Errorf("inspect not understood: %+v", i.State)
					}
				}
			}
			if stats, err := jsonLines[map[string]string](asLines(t, dir, "stats.json")); err != nil {
				t.Errorf("stats: %v", err)
			} else {
				renderStats(stats)
			}
			if out := renderDiskUsage(asLines(t, dir, "system-df.json"), asLines(t, dir, "images.json"), nil, asLines(t, dir, "volumes.json"), nil); !strings.Contains(out, "Images") {
				t.Errorf("disk usage:\n%s", out)
			}
			var nets []network
			if strict(t, dir, "network-inspect.json", &nets) {
				if len(nets) == 0 || !strings.Contains(renderNetworks(nets), "bridge") {
					t.Error("networks not understood")
				}
			}
			if ev, err := jsonLines[event](asLines(t, dir, "events.json")); err != nil {
				t.Errorf("events: %v", err)
			} else {
				renderEvents(ev, time.Hour)
				evalEvents(ev)
			}
		})
	}
}
