// Package modules lists every available module and resolves the set enabled
// by the configuration.
package modules

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/containers"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/linux"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/pbs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/proxmox"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// All returns every compiled-in module; the base module comes first.
func All() []registry.Module {
	return []registry.Module{linux.New(), containers.New(), proxmox.New(), pbs.New()}
}

// Detection is the outcome of probing one module.
type Detection struct {
	Module   registry.Module
	Detected bool
	Enabled  bool
}

// Resolve decides which modules are enabled. With "auto" every detected
// module is enabled; otherwise the listed ones are, even if not detected.
// The base linux module is always enabled.
func Resolve(ctx context.Context, env *registry.Env, cfg *config.Config) ([]Detection, error) {
	all := All()
	known := map[string]bool{}
	for _, m := range all {
		known[m.Name()] = true
	}
	wanted := map[string]bool{}
	for _, name := range cfg.Modules {
		if name == config.ModulesAuto {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("unknown module %q (available: %s)", name, strings.Join(Names(), ", "))
		}
		wanted[name] = true
	}
	auto := cfg.AutoModules()
	out := make([]Detection, 0, len(all))
	for _, m := range all {
		d := Detection{Module: m, Detected: m.Detect(ctx, env)}
		d.Enabled = m.Name() == linux.Name || wanted[m.Name()] || (auto && d.Detected)
		out = append(out, d)
	}
	return out, nil
}

// Enabled filters the enabled modules.
func Enabled(ds []Detection) []registry.Module {
	var out []registry.Module
	for _, d := range ds {
		if d.Enabled {
			out = append(out, d.Module)
		}
	}
	return out
}

// Names lists all module names.
func Names() []string {
	var out []string
	for _, m := range All() {
		out = append(out, m.Name())
	}
	sort.Strings(out)
	return out
}
