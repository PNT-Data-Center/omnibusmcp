package containers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// maxDataBytes bounds raw JSON from the docker CLI; tools condense it.
const maxDataBytes = 8 << 20

// dockerCLI is the runtime binary. Only Docker is supported for now: Podman
// uses different JSON field names and, being daemonless, needs write access
// to its storage, which the service sandbox forbids (ADR-030).
const dockerCLI = "docker"

var (
	// containerRe accepts container names and (short or full) IDs.
	containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
	sinceRe     = regexp.MustCompile(`^(\d{1,4})(s|m|h|d)$`)
	// secretEnvRe marks environment variables whose values are redacted.
	secretEnvRe = regexp.MustCompile(`(?i)pass|secret|token|key|credential|auth`)
)

func validContainer(c string) error {
	if !containerRe.MatchString(c) {
		return fmt.Errorf("invalid container name or ID %q", c)
	}
	return nil
}

// sinceDuration parses "30m", "2h", "1d" up to max.
func sinceDuration(s string, def, max time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	m := sinceRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid since %q (use e.g. 30m, 2h, 1d)", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if d > max {
		return 0, fmt.Errorf("since %q exceeds the maximum of %s", s, humanPeriod(max))
	}
	return d, nil
}

// dockerData runs a docker command whose stdout is data.
func dockerData(ctx context.Context, env *registry.Env, args ...string) (string, error) {
	r := env.Exec.RunData(ctx, maxDataBytes, dockerCLI, args...)
	if !r.OK() {
		return "", errors.New(strings.TrimSpace(r.Format()))
	}
	if r.Truncated {
		return "", fmt.Errorf("docker %s: output larger than %d bytes", args[0], maxDataBytes)
	}
	return r.Stdout, nil
}

// jsonLines decodes one JSON object per line ("--format {{json .}}").
func jsonLines[T any](data string) ([]T, error) {
	var out []T
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), maxDataBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if err := jsonx.Decode("docker --format json", []byte(line), &v); err != nil {
			return nil, fmt.Errorf("invalid JSON line: %w", err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// dockerAvailable reports a Docker installation with a reachable socket
// path; the daemon itself may still be down (health reports that).
func dockerAvailable() bool {
	if _, err := exec.LookPath(dockerCLI); err != nil {
		if _, err := os.Stat("/usr/bin/docker"); err != nil {
			return false
		}
	}
	for _, sock := range []string{"/run/docker.sock", "/var/run/docker.sock"} {
		if _, err := os.Stat(sock); err == nil {
			return true
		}
	}
	// The socket may be socket-activated later; the unit tells us Docker is installed.
	for _, unit := range []string{"/lib/systemd/system/docker.service", "/usr/lib/systemd/system/docker.service", "/etc/systemd/system/docker.service"} {
		if _, err := os.Stat(unit); err == nil {
			return true
		}
	}
	return false
}

// psEntry is one line of "docker ps --format {{json .}}".
type psEntry struct {
	ID           string `json:"ID"`
	Names        string `json:"Names"`
	Image        string `json:"Image"`
	State        string `json:"State"`  // running, exited, restarting, paused, dead, created
	Status       string `json:"Status"` // "Up 2 hours (healthy)", "Exited (1) 3 minutes ago"
	HealthStatus string `json:"HealthStatus"`
	Ports        string `json:"Ports"`
	Labels       string `json:"Labels"`
	RunningFor   string `json:"RunningFor"`
	CreatedAt    string `json:"CreatedAt"`
}

var exitCodeRe = regexp.MustCompile(`^Exited \((-?\d+)\)`)

// exitCode parses "Exited (137) 5 minutes ago"; ok is false if not exited.
func (p psEntry) exitCode() (int, bool) {
	m := exitCodeRe.FindStringSubmatch(p.Status)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

// health returns healthy/unhealthy/starting, or "" without a healthcheck.
func (p psEntry) health() string {
	if p.HealthStatus != "" && p.HealthStatus != "none" {
		return p.HealthStatus
	}
	for _, h := range []string{"unhealthy", "healthy", "health: starting"} {
		if strings.Contains(p.Status, "("+h+")") {
			return strings.TrimPrefix(h, "health: ")
		}
	}
	return ""
}

// label returns one label from the "k=v,k=v" list docker ps prints.
func (p psEntry) label(key string) string {
	for _, kv := range strings.Split(p.Labels, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// compose returns "project/service" for Docker Compose containers.
func (p psEntry) compose() string {
	proj, svc := p.label("com.docker.compose.project"), p.label("com.docker.compose.service")
	if proj == "" {
		return ""
	}
	return proj + "/" + svc
}

// event is one line of "docker events --format {{json .}}".
type event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

func (e event) name() string {
	if n := e.Actor.Attributes["name"]; n != "" {
		return n
	}
	if len(e.Actor.ID) > 12 {
		return e.Actor.ID[:12]
	}
	return e.Actor.ID
}

// action strips arguments, e.g. "health_status: unhealthy" -> "health_status".
func (e event) action() string {
	a, _, _ := strings.Cut(e.Action, ":")
	return a
}

// inspectInfo is the part of "docker inspect" the tools use.
type inspectInfo struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Image   string `json:"Image"`
	Path    string `json:"Path"`
	Args    []string
	State   struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		Dead       bool   `json:"Dead"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
			Log           []struct {
				Start    string `json:"Start"`
				ExitCode int    `json:"ExitCode"`
				Output   string `json:"Output"`
			} `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Config       struct {
		Image       string            `json:"Image"`
		Cmd         []string          `json:"Cmd"`
		Entrypoint  []string          `json:"Entrypoint"`
		Env         []string          `json:"Env"`
		User        string            `json:"User"`
		WorkingDir  string            `json:"WorkingDir"`
		Labels      map[string]string `json:"Labels"`
		Healthcheck *struct {
			Test     []string `json:"Test"`
			Interval int64    `json:"Interval"`
			Retries  int      `json:"Retries"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		Memory      int64  `json:"Memory"`
		NanoCpus    int64  `json:"NanoCpus"`
		PidsLimit   *int64 `json:"PidsLimit"`
		Privileged  bool   `json:"Privileged"`
		NetworkMode string `json:"NetworkMode"`
		LogConfig   struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIP, HostPort string } `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
			Gateway   string `json:"Gateway"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// redactEnv hides values of secret-looking variables.
func redactEnv(kv string) string {
	k, _, ok := strings.Cut(kv, "=")
	if ok && secretEnvRe.MatchString(k) {
		return k + "=*** (redacted)"
	}
	return kv
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut < max/2 {
		cut = max
	}
	return s[:cut] + "\n[output truncated: narrow the query (filters, lines)]\n"
}

// humanPeriod renders 168h as "7d", 90m as "1h30m", 30m as "30m".
func humanPeriod(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	s := d.String() // e.g. "1h30m0s", "30m0s", "1h0m0s"
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
