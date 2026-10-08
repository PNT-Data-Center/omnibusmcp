package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/modules/aptrepo"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// maxAPIBytes bounds raw pvesh JSON. Tools condense it into text limited by
// max_output_bytes, so the raw document may be larger than that limit.
const maxAPIBytes = 8 << 20

// Every pvesh call starts a Perl interpreter with the PVE libraries
// (~175 MB RSS, ~0.9 s). Unbounded parallel calls exceed the service's
// MemoryMax and stall in memory reclaim until they time out, so calls are
// limited to apiSlots at a time, and identical calls within cacheTTL share
// one result (e.g. several sessions running health_summary at once).
const (
	apiSlots = 2
	cacheTTL = 5 * time.Second
)

var (
	slots   = make(chan struct{}, apiSlots)
	cacheMu sync.Mutex
	cache   = map[string]*apiResult{}
	// runPvesh executes the command; tests replace it.
	runPvesh = func(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
		r := env.Exec.RunData(ctx, maxAPIBytes, "pvesh", argv...)
		if !r.OK() {
			return nil, errors.New(strings.TrimSpace(r.Format()))
		}
		if r.Truncated {
			return nil, fmt.Errorf("pvesh %s: response larger than %d bytes", strings.Join(argv, " "), maxAPIBytes)
		}
		return []byte(r.Stdout), nil
	}
)

type apiResult struct {
	done chan struct{} // closed when out/err are set
	at   time.Time
	out  []byte
	err  error
}

// pvesh runs "pvesh get <path> --output-format json" and decodes the result
// into out. Errors carry the formatted command result (TIMEOUT, stderr...),
// so the agent sees why the API call failed.
func pvesh(ctx context.Context, env *registry.Env, out any, path string, args ...string) error {
	argv := append([]string{"get", path, "--output-format", "json"}, args...)
	data, err := cachedCall(ctx, env, argv)
	if err != nil {
		return err
	}
	if err := jsonx.Decode("pvesh get "+path, data, out); err != nil {
		return fmt.Errorf("pvesh get %s: invalid JSON: %w", path, err)
	}
	return nil
}

func cachedCall(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
	key := strings.Join(argv, "\x00")
	cacheMu.Lock()
	if r, ok := cache[key]; ok {
		select {
		case <-r.done:
			if r.err == nil && time.Since(r.at) < cacheTTL {
				cacheMu.Unlock()
				return r.out, nil
			}
		default: // in flight: share it
			cacheMu.Unlock()
			select {
			case <-r.done:
				return r.out, r.err
			case <-ctx.Done():
				return nil, fmt.Errorf("pvesh %s: %w", argv[1], ctx.Err())
			}
		}
	}
	r := &apiResult{done: make(chan struct{})}
	cache[key] = r
	for k, old := range cache { // drop stale entries
		select {
		case <-old.done:
			if time.Since(old.at) >= cacheTTL {
				delete(cache, k)
			}
		default:
		}
	}
	cacheMu.Unlock()

	r.out, r.err = limitedCall(ctx, env, argv)
	r.at = time.Now()
	close(r.done)
	if r.err != nil {
		cacheMu.Lock()
		if cache[key] == r {
			delete(cache, key) // never cache failures
		}
		cacheMu.Unlock()
	}
	return r.out, r.err
}

func limitedCall(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("pvesh %s: waiting for a free API slot: %w", argv[1], ctx.Err())
	}
	return runPvesh(ctx, env, argv)
}

var nodeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)

// localNode returns this host's Proxmox node name: from /etc/pve/.members
// (maintained by pmxcfs), falling back to the short hostname.
func localNode() (string, error) {
	var members struct {
		NodeName string `json:"nodename"`
	}
	name := ""
	if data, err := os.ReadFile("/etc/pve/.members"); err == nil && json.Unmarshal(data, &members) == nil {
		name = members.NodeName
	}
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return "", err
		}
		name, _, _ = strings.Cut(h, ".")
	}
	if !nodeNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid node name %q", name)
	}
	return name, nil
}

// resource is one entry of /cluster/resources (node, VM, CT or storage).
type resource struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"` // node, qemu, lxc, storage, sdn...
	Node       string  `json:"node"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	VMID       int     `json:"vmid"`
	Template   int     `json:"template"`
	Tags       string  `json:"tags"`
	CPU        float64 `json:"cpu"`
	MaxCPU     float64 `json:"maxcpu"`
	Mem        int64   `json:"mem"`
	MaxMem     int64   `json:"maxmem"`
	Disk       int64   `json:"disk"`
	MaxDisk    int64   `json:"maxdisk"`
	Uptime     int64   `json:"uptime"`
	Storage    string  `json:"storage"`
	PluginType string  `json:"plugintype"`
	Content    string  `json:"content"`
	Shared     int     `json:"shared"`
	HAState    string  `json:"hastate"`
}

// clusterEntry is one entry of /cluster/status.
type clusterEntry struct {
	Type    string `json:"type"` // cluster or node
	Name    string `json:"name"`
	Online  int    `json:"online"`
	Local   int    `json:"local"`
	NodeID  int    `json:"nodeid"`
	IP      string `json:"ip"`
	Quorate int    `json:"quorate"`
	Nodes   int    `json:"nodes"`
	Version int    `json:"version"`
}

type haEntry struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Node       string `json:"node"`
	Status     string `json:"status"`
	Quorate    int    `json:"quorate"`
	ArmedState string `json:"armed-state"`
	State      string `json:"state"`
}

type task struct {
	ExitStatus string `json:"exitstatus"` // only in /tasks/{upid}/status
	UPID       string `json:"upid"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	User       string `json:"user"`
	Node       string `json:"node"`
	Status     string `json:"status"`
	StartTime  int64  `json:"starttime"`
	EndTime    int64  `json:"endtime"`
}

// failed reports whether a finished task did not end with OK. Warnings
// ("WARNINGS: n") are not failures.
func (t task) failed() bool {
	return t.EndTime != 0 && t.Status != "" && t.Status != "OK" && !strings.HasPrefix(t.Status, "WARNINGS")
}

type logLine struct {
	N int    `json:"n"`
	T string `json:"t"`
}

type aptUpdate = aptrepo.Update

type nodeStatus struct {
	PVEVersion string    `json:"pveversion"`
	KVersion   string    `json:"kversion"`
	Uptime     int64     `json:"uptime"`
	CPU        float64   `json:"cpu"`
	Wait       float64   `json:"wait"`
	LoadAvg    []string  `json:"loadavg"`
	CPUInfo    cpuInfo   `json:"cpuinfo"`
	Memory     usage     `json:"memory"`
	Swap       usage     `json:"swap"`
	RootFS     usage     `json:"rootfs"`
	KSM        ksm       `json:"ksm"`
	BootInfo   bootInfo  `json:"boot-info"`
	Kernel     kernelRel `json:"current-kernel"`
}

type cpuInfo struct {
	Model   string `json:"model"`
	Sockets int    `json:"sockets"`
	Cores   int    `json:"cores"`
	CPUs    int    `json:"cpus"`
	MHz     float64
}

// UnmarshalJSON accepts mhz as a string or a number (it varies by version).
func (c *cpuInfo) UnmarshalJSON(b []byte) error {
	var raw struct {
		Model   string          `json:"model"`
		Sockets int             `json:"sockets"`
		Cores   int             `json:"cores"`
		CPUs    int             `json:"cpus"`
		MHz     json.RawMessage `json:"mhz"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.Model, c.Sockets, c.Cores, c.CPUs = raw.Model, raw.Sockets, raw.Cores, raw.CPUs
	s := strings.Trim(string(raw.MHz), `"`)
	fmt.Sscanf(s, "%g", &c.MHz)
	return nil
}

type usage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
	Avail int64 `json:"avail"`
}

type ksm struct {
	Shared int64 `json:"shared"`
}

type bootInfo struct {
	Mode       string `json:"mode"`
	SecureBoot int    `json:"secureboot"`
}

type kernelRel struct {
	Release string `json:"release"`
	Machine string `json:"machine"`
}

// --- formatting helpers ---

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

func pct(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

func humanDuration(secs int64) string {
	if secs <= 0 {
		return "-"
	}
	d := time.Duration(secs) * time.Second
	days := int(d.Hours()) / 24
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func epoch(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
}

// truncate bounds text returned to the agent.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut < max/2 {
		cut = max
	}
	return s[:cut] + "\n[output truncated: narrow the query (filters, limit)]\n"
}
