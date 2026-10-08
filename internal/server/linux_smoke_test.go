package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PNT-Data-Center/omnibusmcp/internal/audit"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/executor"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/linux"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// TestLinuxToolsSmoke calls every linux tool against the real host.
func TestLinuxToolsSmoke(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemd on this host")
	}
	cfg := config.Default()
	env := &registry.Env{Cfg: cfg, Exec: executor.New(10*time.Second, 64*1024)}
	s, err := NewMCPServer(Options{Env: env, Modules: []registry.Module{linux.New()}, Audit: audit.Discard(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	calls := map[string]map[string]any{
		"health_summary":         nil,
		"linux_host_overview":    nil,
		"linux_resources":        {"top": 3},
		"linux_disks":            nil,
		"linux_network":          nil,
		"linux_systemd_overview": nil,
		"linux_list_services":    {"state": "running"},
		"linux_service_status":   {"unit": "systemd-journald.service", "lines": 3},
		"linux_journal":          {"since": "1h", "lines": 5},
		"linux_read_file":        {"path": "/etc/os-release"},
		"linux_list_dir":         {"path": "/etc/systemd"},
	}
	for name, args := range calls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if res.IsError || strings.TrimSpace(text) == "" {
			t.Errorf("%s: error result:\n%s", name, text)
		}
		if testing.Verbose() {
			t.Logf("=== %s ===\n%s", name, text)
		}
	}
	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{Name: "linux_read_file", Arguments: map[string]any{"path": "/etc/shadow"}})
	if !res.IsError {
		t.Error("/etc/shadow was readable")
	}
}
