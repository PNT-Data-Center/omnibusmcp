package containers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Health thresholds.
const (
	eventsWindow  = time.Hour
	crashLoopDies = 3 // non-zero exits of one container within eventsWindow
)

// Health implements registry.Module.
func (m *Module) Health(ctx context.Context, env *registry.Env) []registry.Check {
	var (
		wg                 sync.WaitGroup
		version, eventsOut string
		ps                 []psEntry
		verErr, psErr      error
		evErr              error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		version, verErr = dockerData(ctx, env, "version", "--format", "{{.Server.Version}}")
	}()
	go func() { defer wg.Done(); ps, psErr = listContainers(ctx, env, true) }()
	go func() {
		defer wg.Done()
		eventsOut, evErr = dockerData(ctx, env, "events", "--since", strconv.FormatInt(time.Now().Add(-eventsWindow).Unix(), 10),
			"--until", "0s", "--filter", "type=container", "--filter", "event=die", "--filter", "event=oom", "--format", "{{json .}}")
	}()
	wg.Wait()

	checks := []registry.Check{}
	if verErr != nil {
		first, _, _ := strings.Cut(verErr.Error(), "\n")
		checks = append(checks, registry.Check{Name: "runtime", Status: registry.Crit,
			Detail: "Docker daemon not reachable (check docker.service): " + lastLine(verErr.Error(), first)})
		checks = append(checks,
			registry.Check{Name: "containers", Status: registry.Unknown, Detail: "skipped: daemon not reachable"},
			registry.Check{Name: "events", Status: registry.Unknown, Detail: "skipped: daemon not reachable"})
		return label(checks)
	}
	checks = append(checks, registry.Check{Name: "runtime", Status: registry.OK, Detail: "Docker daemon reachable"},
		registry.VersionCheck(compat.Assess(compat.Docker, strings.TrimSpace(version))))

	if psErr != nil {
		checks = append(checks, registry.Check{Name: "containers", Status: registry.Unknown, Detail: lastLine(psErr.Error(), "docker ps failed")})
	} else {
		// Persistent state (OOM kill, restart count) comes from inspect:
		// the daemon keeps only its last 256 events, so events alone miss it.
		checks = append(checks, evalContainers(ps, inspectAll(ctx, env, ps)))
	}
	if evErr != nil {
		checks = append(checks, registry.Check{Name: "events", Status: registry.Unknown, Detail: lastLine(evErr.Error(), "docker events failed")})
	} else {
		events, err := jsonLines[event](eventsOut)
		if err != nil {
			checks = append(checks, registry.Check{Name: "events", Status: registry.Unknown, Detail: err.Error()})
		} else {
			checks = append(checks, evalEvents(events))
		}
	}
	return label(checks)
}

// inspectAll inspects all containers in one call; nil on failure (health
// then relies on docker ps alone).
func inspectAll(ctx context.Context, env *registry.Env, ps []psEntry) map[string]inspectInfo {
	if len(ps) == 0 {
		return nil
	}
	args := []string{"inspect", "--type", "container", "--"}
	for _, p := range ps {
		args = append(args, p.ID)
	}
	out, err := dockerData(ctx, env, args...)
	if err != nil {
		return nil
	}
	var list []inspectInfo
	if jsonx.Decode("docker inspect", []byte(out), &list) != nil {
		return nil
	}
	m := make(map[string]inspectInfo, len(list))
	for _, i := range list {
		m[i.ID] = i
	}
	return m
}

// Version implements registry.Versioned: the Docker Engine (server) version.
func (*Module) Version(ctx context.Context, env *registry.Env) compat.Result {
	v, err := dockerData(ctx, env, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		v = ""
	}
	return compat.Assess(compat.Docker, strings.TrimSpace(v))
}

func label(checks []registry.Check) []registry.Check {
	for i := range checks {
		checks[i].Module = Name
	}
	return checks
}

// lastLine returns the last non-empty line of s (docker puts the reason last).
func lastLine(s, def string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "[exit status") {
			return l
		}
	}
	return def
}

// cleanExit reports exit codes of intentional stops: 0, and 143 (SIGTERM
// from docker stop). 137 (SIGKILL) is ambiguous: docker stop timeout or OOM.
func cleanExit(code int) bool { return code == 0 || code == 143 }

// restartWarn is the restart count from which a container is reported.
const restartWarn = 3

func evalContainers(ps []psEntry, insp map[string]inspectInfo) registry.Check {
	c := registry.Check{Name: "containers", Status: registry.OK}
	var crit, warn []string
	counts := map[string]int{}
	healthy := 0
	for _, p := range ps {
		counts[p.State]++
		info, haveInfo := insp[p.ID]
		restarts := ""
		if haveInfo && info.RestartCount >= restartWarn {
			restarts = fmt.Sprintf(" (restarted %d times)", info.RestartCount)
		}
		switch h := p.health(); {
		case p.State == "restarting":
			crit = append(crit, p.Names+" restarting"+restarts)
			restarts = ""
		case p.State == "dead":
			crit = append(crit, p.Names+" dead")
		case h == "unhealthy":
			warn = append(warn, p.Names+" unhealthy")
		case h == "healthy":
			healthy++
		}
		switch code, exited := p.exitCode(); {
		case exited && haveInfo && info.State.OOMKilled:
			warn = append(warn, fmt.Sprintf("%s OOM-killed (exit %d)", p.Names, code))
		case exited && !cleanExit(code):
			warn = append(warn, fmt.Sprintf("%s exited (%d)", p.Names, code))
		}
		if restarts != "" {
			warn = append(warn, p.Names+restarts)
		}
	}
	c.Detail = fmt.Sprintf("%d running, %d stopped, %d healthy", counts["running"], counts["exited"]+counts["created"], healthy)
	if counts["paused"] > 0 {
		c.Detail += fmt.Sprintf(", %d paused", counts["paused"])
	}
	switch {
	case len(crit) > 0:
		c.Status = registry.Crit
	case len(warn) > 0:
		c.Status = registry.Warn
	}
	if problems := append(crit, warn...); len(problems) > 0 {
		sort.Strings(problems)
		c.Detail += "; " + strings.Join(problems, ", ")
	}
	return c
}

func evalEvents(events []event) registry.Check {
	c := registry.Check{Name: "events", Status: registry.OK}
	dies := map[string]int{}
	var ooms []string
	for _, e := range events {
		switch e.action() {
		case "oom":
			ooms = append(ooms, e.name())
		case "die":
			if code, _ := strconv.Atoi(e.Actor.Attributes["exitCode"]); !cleanExit(code) {
				dies[e.name()]++
			}
		}
	}
	var parts []string
	for _, name := range sortedKeys(dies) {
		n := dies[name]
		if n >= crashLoopDies {
			parts = append(parts, fmt.Sprintf("%s crash loop (%d failed exits)", name, n))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d failed exit(s)", name, n))
		}
	}
	if len(ooms) > 0 {
		sort.Strings(ooms)
		parts = append([]string{"OOM kill: " + strings.Join(uniq(ooms), ", ")}, parts...)
	}
	if len(parts) == 0 {
		c.Detail = "no failed exits or OOM kills among recent events (the daemon keeps only its last 256 events)"
		return c
	}
	c.Status = registry.Warn
	c.Detail = "recent events: " + strings.Join(parts, "; ") + " (see containers_events)"
	return c
}

func uniq(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
