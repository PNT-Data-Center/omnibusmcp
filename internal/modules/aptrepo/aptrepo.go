// Package aptrepo renders pending APT updates and repository configuration
// as reported by the Proxmox VE and Proxmox Backup Server APIs
// (.../apt/update and .../apt/repositories share one format).
package aptrepo

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Update is one pending package update.
type Update struct {
	Package    string `json:"Package"`
	OldVersion string `json:"OldVersion"`
	Version    string `json:"Version"`
	Origin     string `json:"Origin"`
	Priority   string `json:"Priority"`
}

// Repos is the apt/repositories response.
type Repos struct {
	Files []struct {
		Path         string `json:"path"`
		Repositories []struct {
			Types      []string `json:"Types"`
			URIs       []string `json:"URIs"`
			Suites     []string `json:"Suites"`
			Components []string `json:"Components"`
			Enabled    any      `json:"Enabled"`
		} `json:"repositories"`
	} `json:"files"`
	Errors []struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"errors"`
	Infos []struct {
		Path    string `json:"path"`
		Index   any    `json:"index"`
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"infos"`
	Standard []struct {
		Handle string `json:"handle"`
		Name   string `json:"name"`
		Status any    `json:"status"`
	} `json:"standard-repos"`
}

// ProductEnabled reports whether any standard product repository (not
// Ceph) is enabled: enterprise, no-subscription or test.
func (r Repos) ProductEnabled() bool {
	for _, s := range r.Standard {
		if !strings.HasPrefix(strings.ToLower(s.Name), "ceph") && s.Status != nil && Truthy(s.Status) {
			return true
		}
	}
	return false
}

// Render lists pending updates and repositories; product names the vendor
// repository in warnings, e.g. "Proxmox VE" or "Proxmox Backup Server".
func Render(ups []Update, upErr error, repos Repos, repoErr error, product string) string {
	var b strings.Builder
	if upErr != nil {
		fmt.Fprintf(&b, "Pending updates: unavailable: %v\n", upErr)
	} else {
		sort.Slice(ups, func(i, j int) bool { return ups[i].Package < ups[j].Package })
		fmt.Fprintf(&b, "Pending updates: %d (from the last 'apt update'; this tool does not refresh the package lists)\n", len(ups))
		if len(ups) > 0 {
			tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PACKAGE\tINSTALLED\tAVAILABLE\tORIGIN")
			for _, u := range ups {
				old := u.OldVersion
				if old == "" {
					old = "(new)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Package, old, u.Version, u.Origin)
			}
			tw.Flush()
		}
	}
	b.WriteString("\nAPT repositories:\n")
	if repoErr != nil {
		fmt.Fprintf(&b, "  unavailable: %v\n", repoErr)
		return b.String()
	}
	for _, f := range repos.Files {
		for _, r := range f.Repositories {
			state := "enabled"
			if !Truthy(r.Enabled) {
				state = "disabled"
			}
			fmt.Fprintf(&b, "  [%s] %s %s %s %s (%s)\n", state, strings.Join(r.Types, ","), strings.Join(r.URIs, " "),
				strings.Join(r.Suites, ","), strings.Join(r.Components, " "), f.Path)
		}
	}
	var notConfigured []string
	for _, s := range repos.Standard {
		if s.Status == nil {
			notConfigured = append(notConfigured, s.Name)
			continue
		}
		fmt.Fprintf(&b, "  standard repository %q: %s\n", s.Name, map[bool]string{true: "enabled", false: "disabled"}[Truthy(s.Status)])
	}
	if len(notConfigured) > 0 {
		fmt.Fprintf(&b, "  not configured: %s\n", strings.Join(notConfigured, ", "))
	}
	if !repos.ProductEnabled() {
		fmt.Fprintf(&b, "  WARNING: no %s repository is enabled (enterprise or no-subscription): this node receives no %s updates\n", product, product)
	}
	for _, e := range repos.Errors {
		fmt.Fprintf(&b, "  ERROR %s: %s\n", e.Path, e.Error)
	}
	for _, i := range repos.Infos {
		if i.Kind == "origin" {
			continue // "Configured packages from: ..." carries no diagnostic value
		}
		fmt.Fprintf(&b, "  %s: %s (%s)\n", strings.ToUpper(i.Kind), i.Message, i.Path)
	}
	return b.String()
}

// Eval is the health verdict: pending updates are informational, a node
// without any enabled vendor repository is a warning.
func Eval(ups []Update, repos Repos, repoErr error, product string) registry.Check {
	c := registry.Check{Name: "updates", Status: registry.OK, Detail: fmt.Sprintf("%d pending package update(s)", len(ups))}
	switch {
	case repoErr != nil:
		c.Detail += "; repository check unavailable"
	case len(repos.Standard) > 0 && !repos.ProductEnabled():
		c.Status = registry.Warn
		c.Detail += fmt.Sprintf("; no %s repository enabled (enterprise disabled, no-subscription not configured): no %s updates", product, product)
	}
	return c
}

// Truthy interprets PVE/PBS booleans: true, 1, "1", "true".
func Truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	}
	return false
}
