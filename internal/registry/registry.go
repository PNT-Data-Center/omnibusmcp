// Package registry defines modules, tools and health checks, and decides
// which tools a server exposes for its tier.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/executor"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Env is what modules get to do their work.
type Env struct {
	Cfg  *config.Config
	Exec *executor.Executor
}

// Module is a group of tools and health checks for one kind of host
// (base Linux, containers, Ceph, Proxmox VE, PBS...).
type Module interface {
	Name() string
	Description() string
	// Detect reports whether the module applies to this host.
	Detect(ctx context.Context, env *Env) bool
	Tools(env *Env) []Tool
	// Health returns quick checks for health_summary.
	Health(ctx context.Context, env *Env) []Check
}

// Versioned is implemented by modules that depend on a versioned product
// (OS, Proxmox VE, PBS, Docker...).
type Versioned interface {
	Version(ctx context.Context, env *Env) compat.Result
}

// VersionCheck turns a compatibility verdict into an informational health
// check: untested versions are annotated, never raised above OK.
func VersionCheck(r compat.Result) Check {
	return Check{Name: "version", Status: OK, Detail: r.String()}
}

// Status is a health verdict, ordered by severity.
type Status int

const (
	OK Status = iota
	Unknown
	Warn
	Crit
)

func (s Status) String() string {
	return [...]string{"OK", "UNKNOWN", "WARN", "CRIT"}[s]
}

// MarshalJSON renders the status as its name.
func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// Check is one health verdict.
type Check struct {
	Module string `json:"module"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Worst returns the most severe status among checks (OK when empty).
func Worst(checks []Check) Status {
	w := OK
	for _, c := range checks {
		if c.Status > w {
			w = c.Status
		}
	}
	return w
}

// CallHook wraps every tool invocation (audit, tier enforcement).
type CallHook func(ctx context.Context, req *mcp.CallToolRequest, t *Tool, args any, call func() (string, error)) (string, error)

// Tool is a tool definition independent of its input type.
type Tool struct {
	Name        string
	Title       string
	Description string
	Module      string
	MinTier     tier.Tier
	// ReadOnly marks tools that never modify the system.
	ReadOnly bool

	install func(s *mcp.Server, t *Tool, hook CallHook)
}

// Handler implements a tool: validated input in, text for the LLM out.
// A returned error is reported to the client as a tool error, together with
// any partial output.
type Handler[In any] func(ctx context.Context, in In) (string, error)

// NewTool builds a Tool whose input schema is inferred from In.
func NewTool[In any](name, title, desc string, minTier tier.Tier, readOnly bool, h Handler[In]) Tool {
	return Tool{
		Name: name, Title: title, Description: desc, MinTier: minTier, ReadOnly: readOnly,
		install: func(s *mcp.Server, t *Tool, hook CallHook) {
			destructive := !readOnly
			mt := &mcp.Tool{
				Name:        t.Name,
				Title:       t.Title,
				Description: t.Description,
				Annotations: &mcp.ToolAnnotations{
					Title:           t.Title,
					ReadOnlyHint:    readOnly,
					IdempotentHint:  readOnly,
					DestructiveHint: &destructive,
				},
			}
			mcp.AddTool(s, mt, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
				out, err := hook(ctx, req, t, in, func() (string, error) { return h(ctx, in) })
				if err != nil {
					text := "error: " + err.Error()
					if out != "" {
						text += "\n\n" + out
					}
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil, nil
			})
		},
	}
}

// Install registers t on s through hook.
func (t *Tool) Install(s *mcp.Server, hook CallHook) { t.install(s, t, hook) }

// Collect gathers the tools of modules allowed at tier cur, sorted by name.
// Duplicate names are a programming error.
func Collect(env *Env, mods []Module, cur tier.Tier) ([]Tool, error) {
	var out []Tool
	seen := map[string]string{}
	for _, m := range mods {
		for _, t := range m.Tools(env) {
			t.Module = m.Name()
			if prev, dup := seen[t.Name]; dup {
				return nil, fmt.Errorf("tool %q defined by both %s and %s", t.Name, prev, m.Name())
			}
			seen[t.Name] = m.Name()
			if cur.Allows(t.MinTier) {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RunHealth runs every module's checks concurrently within timeout. A module
// that does not answer in time is reported as UNKNOWN.
func RunHealth(ctx context.Context, env *Env, mods []Module, timeout time.Duration) []Check {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make([][]Check, len(mods))
	done := make(chan int, len(mods))
	for i, m := range mods {
		go func() {
			results[i] = m.Health(ctx, env)
			done <- i
		}()
	}
	finished := make([]bool, len(mods))
	for range mods {
		select {
		case i := <-done:
			finished[i] = true
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	var all []Check
	for i, m := range mods {
		if !finished[i] {
			all = append(all, Check{Module: m.Name(), Name: "module", Status: Unknown,
				Detail: fmt.Sprintf("health checks did not finish within %s", timeout)})
			continue
		}
		all = append(all, results[i]...)
	}
	return all
}
