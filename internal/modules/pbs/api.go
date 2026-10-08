package pbs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// The PBS API is read through "proxmox-backup-debug api get", which calls
// the local API handlers as root (~0.15 s, ~28 MB RSS per call; Rust, so
// far cheaper than pvesh). proxmox-backup-manager is NOT used: it opens
// /run/proxmox-backup/shmem/config-versions read-write, which the service
// sandbox (ProtectSystem=strict) refuses with EACCES.
const (
	debugCLI     = "proxmox-backup-debug"
	maxAPIBytes  = 8 << 20
	apiSlots     = 4 // health queries ~20 endpoints; bound memory and load
	apiCacheTTL  = 5 * time.Second
	localNodeAPI = "/nodes/localhost"
)

var (
	slots   = make(chan struct{}, apiSlots)
	cacheMu sync.Mutex
	cache   = map[string]*apiResult{}
	// runAPI executes the command; tests replace it.
	runAPI = func(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
		r := env.Exec.RunData(ctx, maxAPIBytes, debugCLI, argv...)
		if !r.OK() {
			return nil, errors.New(strings.TrimSpace(r.Format()))
		}
		if r.Truncated {
			return nil, fmt.Errorf("%s: response larger than %d bytes", argv[2], maxAPIBytes)
		}
		return []byte(r.Stdout), nil
	}
)

type apiResult struct {
	done chan struct{}
	at   time.Time
	out  []byte
	err  error
}

// api runs "proxmox-backup-debug api get <path> --output-format json" and
// decodes the result. Identical calls within apiCacheTTL share one result;
// at most apiSlots calls run at once. Failures are not cached.
func api(ctx context.Context, env *registry.Env, out any, path string, args ...string) error {
	argv := append([]string{"api", "get", path, "--output-format", "json"}, args...)
	data, err := cachedCall(ctx, env, argv)
	if err != nil {
		return err
	}
	if err := jsonx.Decode("pbs api get "+path, data, out); err != nil {
		return fmt.Errorf("api get %s: invalid JSON: %w", path, err)
	}
	return nil
}

func cachedCall(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
	key := strings.Join(argv, "\x00")
	cacheMu.Lock()
	if r, ok := cache[key]; ok {
		select {
		case <-r.done:
			if r.err == nil && time.Since(r.at) < apiCacheTTL {
				cacheMu.Unlock()
				return r.out, nil
			}
		default:
			cacheMu.Unlock()
			select {
			case <-r.done:
				return r.out, r.err
			case <-ctx.Done():
				return nil, fmt.Errorf("api get %s: %w", argv[2], ctx.Err())
			}
		}
	}
	r := &apiResult{done: make(chan struct{})}
	cache[key] = r
	for k, old := range cache {
		select {
		case <-old.done:
			if time.Since(old.at) >= apiCacheTTL {
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
			delete(cache, key)
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
		return nil, fmt.Errorf("api get %s: waiting for a free API slot: %w", argv[2], ctx.Err())
	}
	return runAPI(ctx, env, argv)
}

var (
	// storeRe matches PBS datastore names (SAFE_ID).
	storeRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	// upidRe matches PBS task IDs: node, pid, pstart, task id, start time,
	// worker type, escaped worker id (\x2d...), user/token.
	upidRe     = regexp.MustCompile(`^UPID:[A-Za-z0-9.-]+:[0-9A-Fa-f]{8}:[0-9A-Fa-f]{8,16}:[0-9A-Fa-f]{8}:[0-9A-Fa-f]{8}:[a-z0-9_-]+:[A-Za-z0-9\\._-]*:[^\s:/]+:$`)
	taskTypeRe = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	sinceRe    = regexp.MustCompile(`^(\d{1,4})(h|d)$`)
)

func validStore(s string) error {
	if !storeRe.MatchString(s) || strings.Contains(s, "..") {
		return fmt.Errorf("invalid datastore name %q", s)
	}
	return nil
}

// --- response types (only the fields the tools use) ---

type datastoreUsage struct {
	Store         string    `json:"store"`
	Total         int64     `json:"total"`
	Used          int64     `json:"used"`
	Avail         int64     `json:"avail"`
	EstimatedFull int64     `json:"estimated-full-date"`
	MountStatus   string    `json:"mount-status"`
	GC            *gcStatus `json:"gc-status"`
}

type datastoreConfig struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Comment         string `json:"comment"`
	GCSchedule      string `json:"gc-schedule"`
	MaintenanceMode string `json:"maintenance-mode"`
}

type gcStatus struct {
	Store          string `json:"store"`
	LastRunState   string `json:"last-run-state"`
	LastRunEnd     int64  `json:"last-run-endtime"`
	NextRun        int64  `json:"next-run"`
	Schedule       string `json:"schedule"`
	Duration       int64  `json:"duration"`
	DiskBytes      int64  `json:"disk-bytes"`
	IndexDataBytes int64  `json:"index-data-bytes"`
	PendingBytes   int64  `json:"pending-bytes"`
	RemovedBytes   int64  `json:"removed-bytes"`
	StillBad       int64  `json:"still-bad"`
	RemovedBad     int64  `json:"removed-bad"`
}

type job struct {
	Retention    string `json:"-"` // prune jobs: keep-* options from the config
	ID           string `json:"id"`
	Store        string `json:"store"`
	Schedule     string `json:"schedule"`
	NextRun      int64  `json:"next-run"`
	LastRunState string `json:"last-run-state"`
	LastRunEnd   int64  `json:"last-run-endtime"`
	Comment      string `json:"comment"`
	Remote       string `json:"remote"`
	RemoteStore  string `json:"remote-store"`
}

type task struct {
	UPID       string  `json:"upid"`
	WorkerType string  `json:"worker_type"`
	WorkerID   *string `json:"worker_id"`
	User       string  `json:"user"`
	Status     string  `json:"status"`
	StartTime  int64   `json:"starttime"`
	EndTime    int64   `json:"endtime"`
	// /tasks/{upid}/status uses other names:
	Type       string `json:"type"`
	ID         string `json:"id"`
	ExitStatus string `json:"exitstatus"`
}

// failed reports a finished task that did not end OK (warnings are not failures).
func (t task) failed() bool {
	return t.EndTime != 0 && t.Status != "" && t.Status != "OK" && !strings.HasPrefix(t.Status, "WARNINGS")
}

type logLine struct {
	N int    `json:"n"`
	T string `json:"t"`
}

type group struct {
	NS          string `json:"-"` // namespace the group was listed in ("" = root)
	BackupType  string `json:"backup-type"`
	BackupID    string `json:"backup-id"`
	LastBackup  int64  `json:"last-backup"`
	BackupCount int64  `json:"backup-count"`
	Owner       string `json:"owner"`
}

type nodeStatus struct {
	CPU      float64   `json:"cpu"`
	Wait     float64   `json:"wait"`
	LoadAvg  []float64 `json:"loadavg"`
	Uptime   int64     `json:"uptime"`
	KVersion string    `json:"kversion"`
	CPUInfo  struct {
		Model   string `json:"model"`
		CPUs    int    `json:"cpus"`
		Sockets int    `json:"sockets"`
	} `json:"cpuinfo"`
	Memory   usage `json:"memory"`
	Swap     usage `json:"swap"`
	Root     usage `json:"root"`
	BootInfo struct {
		Mode       string `json:"mode"`
		SecureBoot any    `json:"secureboot"`
	} `json:"boot-info"`
	Kernel struct {
		Release string `json:"release"`
	} `json:"current-kernel"`
}

type usage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
	Avail int64 `json:"avail"`
}

type version struct {
	Version string `json:"version"`
	Release string `json:"release"`
	RepoID  string `json:"repoid"`
}

type certInfo struct {
	Filename    string   `json:"filename"`
	Subject     string   `json:"subject"`
	Issuer      string   `json:"issuer"`
	NotAfter    int64    `json:"notafter"`
	NotBefore   int64    `json:"notbefore"`
	Fingerprint string   `json:"fingerprint"`
	SAN         []string `json:"san"`
}

type subscription struct {
	Status  string `json:"status"`
	Message string `json:"message"`
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
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
}

// ago renders the age of a timestamp, e.g. "10h 41m ago".
func ago(ts int64, now time.Time) string {
	if ts <= 0 {
		return "never"
	}
	return humanDuration(now.Unix()-ts) + " ago"
}

// estimatedFull returns the projected full date, or "" when PBS has no
// meaningful estimate (unknown, or a past date because usage is not growing).
func estimatedFull(ts int64, now time.Time) string {
	if ts <= now.Unix() {
		return ""
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

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
