// Package containers is the container runtime module (Docker): runtime
// state, containers, logs, resource usage, disk usage, networks and events.
// All tools are read-only (tier 1).
package containers

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Name is the module identifier used in configuration.
const Name = "containers"

// Module implements registry.Module.
type Module struct{}

// New returns the containers module.
func New() *Module { return &Module{} }

func (*Module) Name() string { return Name }

func (*Module) Description() string {
	return "containers (Docker): runtime, containers, logs, stats, disk usage, networks, events"
}

// Detect reports an installed Docker engine.
func (*Module) Detect(context.Context, *registry.Env) bool { return dockerAvailable() }

type noInput struct{}

type listInput struct {
	RunningOnly bool   `json:"running_only,omitempty" jsonschema:"only running containers (default: all, including stopped)"`
	Name        string `json:"name,omitempty" jsonschema:"only containers whose name contains this text"`
}

type containerInput struct {
	Container string `json:"container" jsonschema:"container name or ID"`
}

type logsInput struct {
	Container string `json:"container" jsonschema:"container name or ID"`
	Lines     int    `json:"lines,omitempty" jsonschema:"number of most recent lines (default 100, max 2000)"`
	Since     string `json:"since,omitempty" jsonschema:"only logs newer than this: e.g. 30m, 2h, 1d (max 30d)"`
	Grep      string `json:"grep,omitempty" jsonschema:"regular expression; only matching lines (case-insensitive), searched in the last 10000 lines"`
}

type eventsInput struct {
	Since      string `json:"since,omitempty" jsonschema:"period to look back: e.g. 30m, 6h (default 1h, max 7d)"`
	Container  string `json:"container,omitempty" jsonschema:"only events of this container"`
	AllActions bool   `json:"all_actions,omitempty" jsonschema:"include exec_* events (every health check run produces them); default: lifecycle events only"`
}

// lifecycleActions are the events shown by default; health check runs
// create exec_create/exec_start/exec_die noise every few seconds.
var lifecycleActions = []string{"create", "start", "restart", "die", "oom", "kill", "stop", "pause", "unpause", "health_status", "destroy", "update"}

// Tools returns the module's tools.
func (m *Module) Tools(env *registry.Env) []registry.Tool {
	ro := tier.ReadOnly
	limit := func(s string) string { return truncate(s, env.Cfg.Limits.MaxOutputBytes) }
	return []registry.Tool{
		registry.NewTool("containers_runtime", "Container runtime",
			"Docker engine version and state: container counts, storage and logging driver, cgroup version, root directory, and engine warnings.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				out, err := dockerData(ctx, env, "info", "--format", "{{json .}}")
				if err != nil {
					return "", err
				}
				var info map[string]any
				if err := jsonx.Decode("docker info", []byte(out), &info); err != nil {
					return "", err
				}
				return limit(renderRuntime(info)), nil
			}),

		registry.NewTool("containers_list", "List containers",
			"Containers with state, status (exit code, uptime), health check result, image, published ports and Docker Compose project/service. Includes stopped containers unless running_only.",
			ro, true, func(ctx context.Context, in listInput) (string, error) {
				ps, err := listContainers(ctx, env, !in.RunningOnly)
				if err != nil {
					return "", err
				}
				return limit(renderList(ps, in.Name)), nil
			}),

		registry.NewTool("containers_inspect", "Container details",
			"Condensed docker inspect of one container: state (exit code, OOM kill, error, restarts), health check log, restart policy, limits, command, mounts, networks, ports, labels and environment (secret-looking values redacted).",
			ro, true, func(ctx context.Context, in containerInput) (string, error) {
				if err := validContainer(in.Container); err != nil {
					return "", err
				}
				out, err := dockerData(ctx, env, "inspect", "--type", "container", "--", in.Container)
				if err != nil {
					return "", err
				}
				var list []inspectInfo
				if err := jsonx.Decode("docker inspect", []byte(out), &list); err != nil || len(list) == 0 {
					return "", fmt.Errorf("unexpected docker inspect output: %v", err)
				}
				return limit(renderInspect(list[0])), nil
			}),

		registry.NewTool("containers_logs", "Container logs",
			"Most recent log lines of a container (stdout and stderr, with timestamps). Narrow with since and grep.",
			ro, true, func(ctx context.Context, in logsInput) (string, error) {
				if err := validContainer(in.Container); err != nil {
					return "", err
				}
				lines := in.Lines
				switch {
				case lines == 0:
					lines = 100
				case lines < 1:
					lines = 1
				case lines > 2000:
					lines = 2000
				}
				args := []string{"logs", "--timestamps"}
				if in.Since != "" {
					d, err := sinceDuration(in.Since, 0, 30*24*time.Hour)
					if err != nil {
						return "", err
					}
					args = append(args, "--since", strconv.FormatInt(time.Now().Add(-d).Unix(), 10))
				}
				if in.Grep == "" {
					args = append(args, "--tail", strconv.Itoa(lines), "--", in.Container)
					return limit(env.Exec.RunTail(ctx, dockerCLI, args...).Format()), nil
				}
				if len(in.Grep) > 200 {
					return "", fmt.Errorf("grep pattern too long (max 200 characters)")
				}
				re, err := regexp.Compile("(?i)" + in.Grep)
				if err != nil {
					return "", fmt.Errorf("invalid grep pattern: %v", err)
				}
				args = append(args, "--tail", "10000", "--", in.Container)
				r := env.Exec.RunData(ctx, maxDataBytes, dockerCLI, args...)
				if !r.OK() {
					return limit(r.Format()), nil // e.g. no such container, daemon down
				}
				return limit(grepLogs(r.Stdout, r.Stderr, re, lines, in.Container)), nil
			}),

		registry.NewTool("containers_stats", "Container resource usage",
			"Current CPU, memory (usage/limit), network and block I/O and process count of running containers (one sample).",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				out, err := dockerData(ctx, env, "stats", "--no-stream", "--format", "{{json .}}")
				if err != nil {
					return "", err
				}
				stats, err := jsonLines[map[string]string](out)
				if err != nil {
					return "", err
				}
				return limit(renderStats(stats)), nil
			}),

		registry.NewTool("containers_disk_usage", "Container disk usage",
			"Disk used by images, containers, volumes and build cache (with reclaimable space), plus image and volume lists. Useful when the Docker root filesystem fills up.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				df, err := dockerData(ctx, env, "system", "df", "--format", "{{json .}}")
				if err != nil {
					return "", err
				}
				images, imgErr := dockerData(ctx, env, "images", "--all", "--format", "{{json .}}")
				volumes, volErr := dockerData(ctx, env, "volume", "ls", "--format", "{{json .}}")
				return limit(renderDiskUsage(df, images, imgErr, volumes, volErr)), nil
			}),

		registry.NewTool("containers_networks", "Container networks",
			"Docker networks with driver, scope, subnet, gateway and attached containers with their IP addresses.",
			ro, true, func(ctx context.Context, _ noInput) (string, error) {
				ids, err := dockerData(ctx, env, "network", "ls", "--quiet", "--no-trunc")
				if err != nil {
					return "", err
				}
				list := strings.Fields(ids)
				if len(list) == 0 {
					return "No networks.\n", nil
				}
				out, err := dockerData(ctx, env, append([]string{"network", "inspect", "--"}, list...)...)
				if err != nil {
					return "", err
				}
				var nets []network
				if err := jsonx.Decode("docker network inspect", []byte(out), &nets); err != nil {
					return "", err
				}
				return limit(renderNetworks(nets)), nil
			}),

		registry.NewTool("containers_events", "Container events",
			"Container lifecycle events in a past period: start, die (with exit code), oom, kill, restart, health_status changes. Note: the Docker daemon keeps only its last 256 events, so on busy hosts (frequent health checks) history may cover only minutes; use containers_inspect for persistent state (OOM kill, restart count, exit code).",
			ro, true, func(ctx context.Context, in eventsInput) (string, error) {
				d, err := sinceDuration(in.Since, time.Hour, 7*24*time.Hour)
				if err != nil {
					return "", err
				}
				args := []string{"events", "--since", strconv.FormatInt(time.Now().Add(-d).Unix(), 10), "--until", "0s",
					"--filter", "type=container", "--format", "{{json .}}"}
				if !in.AllActions {
					for _, a := range lifecycleActions {
						args = append(args, "--filter", "event="+a)
					}
				}
				if in.Container != "" {
					if err := validContainer(in.Container); err != nil {
						return "", err
					}
					args = append(args, "--filter", "container="+in.Container)
				}
				out, err := dockerData(ctx, env, args...)
				if err != nil {
					return "", err
				}
				events, err := jsonLines[event](out)
				if err != nil {
					return "", err
				}
				return limit(renderEvents(events, d)), nil
			}),
	}
}

func listContainers(ctx context.Context, env *registry.Env, all bool) ([]psEntry, error) {
	args := []string{"ps", "--no-trunc", "--format", "{{json .}}"}
	if all {
		args = append(args, "--all")
	}
	out, err := dockerData(ctx, env, args...)
	if err != nil {
		return nil, err
	}
	ps, err := jsonLines[psEntry](out)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Names < ps[j].Names })
	return ps, err
}

func newTable(b *strings.Builder) *tabwriter.Writer { return tabwriter.NewWriter(b, 0, 4, 2, ' ', 0) }

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func renderRuntime(info map[string]any) string {
	var b strings.Builder
	get := func(k string) any {
		if v, ok := info[k]; ok && v != nil && v != "" {
			return v
		}
		return "-"
	}
	fmt.Fprintf(&b, "Engine:          Docker %v (%v, %v)\n", get("ServerVersion"), get("OperatingSystem"), get("Architecture"))
	fmt.Fprintf(&b, "Containers:      %v total, %v running, %v paused, %v stopped\n", get("Containers"), get("ContainersRunning"), get("ContainersPaused"), get("ContainersStopped"))
	fmt.Fprintf(&b, "Images:          %v\n", get("Images"))
	fmt.Fprintf(&b, "Storage driver:  %v\n", get("Driver"))
	fmt.Fprintf(&b, "Logging driver:  %v\n", get("LoggingDriver"))
	fmt.Fprintf(&b, "Cgroup:          %v, v%v\n", get("CgroupDriver"), get("CgroupVersion"))
	fmt.Fprintf(&b, "Root dir:        %v\n", get("DockerRootDir"))
	fmt.Fprintf(&b, "Live restore:    %v\n", get("LiveRestoreEnabled"))
	fmt.Fprintf(&b, "Kernel / CPUs:   %v / %v\n", get("KernelVersion"), get("NCPU"))
	warnings, _ := info["Warnings"].([]any)
	if len(warnings) == 0 {
		b.WriteString("Warnings:        none\n")
	} else {
		b.WriteString("Warnings:\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "  - %v\n", w)
		}
	}
	return b.String()
}

func renderList(ps []psEntry, name string) string {
	var b strings.Builder
	tw := newTable(&b)
	fmt.Fprintln(tw, "NAME\tSTATE\tSTATUS\tHEALTH\tIMAGE\tCOMPOSE\tPORTS\tID")
	shown := 0
	for _, p := range ps {
		if name != "" && !strings.Contains(strings.ToLower(p.Names), strings.ToLower(name)) {
			continue
		}
		shown++
		id := p.ID
		if len(id) > 12 {
			id = id[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Names, p.State, p.Status, dash(p.health()), p.Image, dash(p.compose()), dash(p.Ports), id)
	}
	tw.Flush()
	if shown == 0 {
		return "No containers match.\n"
	}
	return fmt.Sprintf("%d container(s):\n\n%s", shown, b.String())
}

func renderInspect(c inspectInfo) string {
	var b strings.Builder
	st := c.State
	fmt.Fprintf(&b, "Container:  %s (%s)\n", strings.TrimPrefix(c.Name, "/"), shortID(c.ID))
	fmt.Fprintf(&b, "Image:      %s\n", c.Config.Image)
	fmt.Fprintf(&b, "State:      %s", st.Status)
	if !st.Running {
		fmt.Fprintf(&b, ", exit code %d", st.ExitCode)
	}
	if st.OOMKilled {
		b.WriteString(", OOM KILLED (memory limit reached)")
	}
	if st.Restarting {
		b.WriteString(", RESTARTING")
	}
	b.WriteString("\n")
	if st.Error != "" {
		fmt.Fprintf(&b, "Error:      %s\n", st.Error)
	}
	fmt.Fprintf(&b, "Started:    %s\n", st.StartedAt)
	if !st.Running {
		fmt.Fprintf(&b, "Finished:   %s\n", st.FinishedAt)
	}
	fmt.Fprintf(&b, "Restarts:   %d (policy: %s", c.RestartCount, dash(c.HostConfig.RestartPolicy.Name))
	if c.HostConfig.RestartPolicy.MaximumRetryCount > 0 {
		fmt.Fprintf(&b, ", max %d", c.HostConfig.RestartPolicy.MaximumRetryCount)
	}
	b.WriteString(")\n")
	lim := "none"
	if c.HostConfig.Memory > 0 {
		lim = fmt.Sprintf("%d MiB", c.HostConfig.Memory>>20)
	}
	cpus := "unlimited"
	if c.HostConfig.NanoCpus > 0 {
		cpus = fmt.Sprintf("%.2f", float64(c.HostConfig.NanoCpus)/1e9)
	}
	fmt.Fprintf(&b, "Limits:     memory %s, CPUs %s, privileged %v\n", lim, cpus, c.HostConfig.Privileged)
	fmt.Fprintf(&b, "Command:    %s\n", strings.TrimSpace(strings.Join(append(append([]string{}, c.Config.Entrypoint...), c.Config.Cmd...), " ")))
	if c.Config.User != "" {
		fmt.Fprintf(&b, "User:       %s\n", c.Config.User)
	}
	fmt.Fprintf(&b, "Logging:    %s\n", dash(c.HostConfig.LogConfig.Type))

	if st.Health != nil {
		fmt.Fprintf(&b, "\nHealth:     %s (failing streak %d)\n", st.Health.Status, st.Health.FailingStreak)
		if hc := c.Config.Healthcheck; hc != nil && len(hc.Test) > 0 {
			fmt.Fprintf(&b, "  check:    %s (every %s, retries %d)\n", strings.Join(hc.Test, " "), time.Duration(hc.Interval), hc.Retries)
		}
		logs := st.Health.Log
		if len(logs) > 3 {
			logs = logs[len(logs)-3:]
		}
		for _, l := range logs {
			out := strings.TrimSpace(l.Output)
			if len(out) > 300 {
				out = out[:300] + "…"
			}
			fmt.Fprintf(&b, "  %s exit %d: %s\n", l.Start, l.ExitCode, strings.ReplaceAll(out, "\n", " | "))
		}
	}

	b.WriteString("\nNetworks:\n")
	fmt.Fprintf(&b, "  mode %s\n", dash(c.HostConfig.NetworkMode))
	netNames := sortedKeys(c.NetworkSettings.Networks)
	for _, n := range netNames {
		nw := c.NetworkSettings.Networks[n]
		fmt.Fprintf(&b, "  %s: ip %s, gateway %s\n", n, dash(nw.IPAddress), dash(nw.Gateway))
	}
	var ports []string
	for p, binds := range c.NetworkSettings.Ports {
		for _, bnd := range binds {
			ports = append(ports, fmt.Sprintf("%s:%s->%s", dash(bnd.HostIP), bnd.HostPort, p))
		}
		if len(binds) == 0 {
			ports = append(ports, p+" (not published)")
		}
	}
	sort.Strings(ports)
	fmt.Fprintf(&b, "Ports:      %s\n", dash(strings.Join(ports, ", ")))

	b.WriteString("\nMounts:\n")
	if len(c.Mounts) == 0 {
		b.WriteString("  none\n")
	}
	for _, mt := range c.Mounts {
		src := mt.Source
		if mt.Type == "volume" && mt.Name != "" {
			src = "volume " + mt.Name
		}
		mode := "ro"
		if mt.RW {
			mode = "rw"
		}
		fmt.Fprintf(&b, "  %s -> %s (%s, %s)\n", src, mt.Destination, mt.Type, mode)
	}

	if len(c.Config.Labels) > 0 {
		b.WriteString("\nLabels:\n")
		for _, k := range sortedKeys(c.Config.Labels) {
			fmt.Fprintf(&b, "  %s=%s\n", k, c.Config.Labels[k])
		}
	}
	b.WriteString("\nEnvironment (secret-looking values redacted):\n")
	env := append([]string{}, c.Config.Env...)
	sort.Strings(env)
	for _, kv := range env {
		fmt.Fprintf(&b, "  %s\n", redactEnv(kv))
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// grepLogs filters stdout and stderr lines and keeps the last n matches.
func grepLogs(stdout, stderr string, re *regexp.Regexp, n int, container string) string {
	var matches []string
	for _, stream := range []struct{ name, data string }{{"stdout", stdout}, {"stderr", stderr}} {
		for _, l := range strings.Split(stream.data, "\n") {
			if l != "" && re.MatchString(l) {
				matches = append(matches, l+"  ["+stream.name+"]")
			}
		}
	}
	// Timestamps (--timestamps) sort the merged streams chronologically.
	sort.SliceStable(matches, func(i, j int) bool { return matches[i] < matches[j] })
	total := len(matches)
	if total > n {
		matches = matches[total-n:]
	}
	if total == 0 {
		return fmt.Sprintf("No log lines of %s match %q (searched the last 10000 lines).\n", container, re.String()[4:])
	}
	return fmt.Sprintf("%d matching line(s) of %s, showing the last %d:\n%s\n", total, container, len(matches), strings.Join(matches, "\n"))
}

func renderStats(stats []map[string]string) string {
	if len(stats) == 0 {
		return "No running containers.\n"
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i]["Name"] < stats[j]["Name"] })
	var b strings.Builder
	tw := newTable(&b)
	fmt.Fprintln(tw, "NAME\tCPU%\tMEM USAGE / LIMIT\tMEM%\tNET I/O\tBLOCK I/O\tPIDS")
	for _, s := range stats {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s["Name"], s["CPUPerc"], s["MemUsage"], s["MemPerc"], s["NetIO"], s["BlockIO"], s["PIDs"])
	}
	tw.Flush()
	return b.String()
}

func renderDiskUsage(df, images string, imgErr error, volumes string, volErr error) string {
	var b strings.Builder
	rows, _ := jsonLines[map[string]string](df)
	tw := newTable(&b)
	fmt.Fprintln(tw, "TYPE\tTOTAL\tACTIVE\tSIZE\tRECLAIMABLE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r["Type"], r["TotalCount"], r["Active"], r["Size"], r["Reclaimable"])
	}
	tw.Flush()

	b.WriteString("\nImages:\n")
	if imgErr != nil {
		fmt.Fprintf(&b, "  unavailable: %v\n", imgErr)
	} else {
		imgs, _ := jsonLines[map[string]string](images)
		dangling := 0
		tw = newTable(&b)
		fmt.Fprintln(tw, "  REPOSITORY:TAG\tID\tSIZE\tCREATED\tCONTAINERS")
		for _, im := range imgs {
			if im["Repository"] == "<none>" {
				dangling++
			}
			fmt.Fprintf(tw, "  %s:%s\t%s\t%s\t%s\t%s\n", im["Repository"], im["Tag"], shortID(strings.TrimPrefix(im["ID"], "sha256:")), im["Size"], im["CreatedSince"], im["Containers"])
		}
		tw.Flush()
		fmt.Fprintf(&b, "  %d image(s), %d dangling (<none>)\n", len(imgs), dangling)
	}

	b.WriteString("\nVolumes:\n")
	if volErr != nil {
		fmt.Fprintf(&b, "  unavailable: %v\n", volErr)
	} else {
		vols, _ := jsonLines[map[string]string](volumes)
		if len(vols) == 0 {
			b.WriteString("  none\n")
		}
		for _, v := range vols {
			fmt.Fprintf(&b, "  %s (%s)\n", v["Name"], v["Driver"])
		}
	}
	return b.String()
}

type network struct {
	Name     string `json:"Name"`
	ID       string `json:"Id"`
	Driver   string `json:"Driver"`
	Scope    string `json:"Scope"`
	Internal bool   `json:"Internal"`
	IPAM     struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
}

func renderNetworks(nets []network) string {
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
	var b strings.Builder
	for _, n := range nets {
		var subnets []string
		for _, c := range n.IPAM.Config {
			s := c.Subnet
			if c.Gateway != "" {
				s += " gw " + c.Gateway
			}
			subnets = append(subnets, s)
		}
		fmt.Fprintf(&b, "%s (%s, %s%s): %s\n", n.Name, n.Driver, n.Scope, map[bool]string{true: ", internal", false: ""}[n.Internal], dash(strings.Join(subnets, "; ")))
		var members []string
		for _, c := range n.Containers {
			members = append(members, fmt.Sprintf("%s %s", c.Name, dash(c.IPv4Address)))
		}
		sort.Strings(members)
		for _, m := range members {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
	}
	return b.String()
}

func renderEvents(events []event, period time.Duration) string {
	if len(events) == 0 {
		return fmt.Sprintf("No container events in the last %s.\n", humanPeriod(period))
	}
	counts := map[string]int{}
	var b strings.Builder
	tw := newTable(&b)
	fmt.Fprintln(tw, "TIME\tCONTAINER\tACTION\tDETAILS")
	shown := events
	if len(shown) > 200 {
		shown = shown[len(shown)-200:]
	}
	for _, e := range events {
		counts[e.action()]++
	}
	for _, e := range shown {
		var details []string
		if c := e.Actor.Attributes["exitCode"]; c != "" {
			details = append(details, "exit code "+c)
		}
		if _, h, ok := strings.Cut(e.Action, ": "); ok {
			details = append(details, h)
		}
		if img := e.Actor.Attributes["image"]; img != "" {
			details = append(details, "image "+img)
		}
		ts := time.Unix(e.Time, 0)
		if e.TimeNano > 0 {
			ts = time.Unix(0, e.TimeNano)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ts.Local().Format("2006-01-02 15:04:05"), e.name(), e.action(), strings.Join(details, ", "))
	}
	tw.Flush()
	var parts []string
	for _, k := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	head := fmt.Sprintf("%d event(s) in the last %s: %s\n", len(events), humanPeriod(period), strings.Join(parts, ", "))
	if len(events) > len(shown) {
		head += fmt.Sprintf("[showing the last %d]\n", len(shown))
	}
	return head + "\n" + b.String()
}
