package ceph

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// varLogCeph holds the log files of package installations; tests replace it.
var varLogCeph = "/var/log/ceph"

// tailBytes bounds how much of a log file is read: ceph.log grows by
// several MB a day, mostly debug lines the level filter drops.
const tailBytes = 8 << 20

type logsInput struct {
	Source string `json:"source,omitempty" jsonschema:"cluster (default: the cluster log; only on hosts with a monitor), audit (commands run against the cluster), or a daemon of this host: osd.3, mon.host1, mgr.host1, mds.x"`
	Level  string `json:"level,omitempty" jsonschema:"warn (default: warnings and errors), error, or all"`
	Lines  int    `json:"lines,omitempty" jsonschema:"number of last matching lines (default 100, max 1000)"`
	Grep   string `json:"grep,omitempty" jsonschema:"only lines containing this text (case-insensitive)"`
}

var sourceRe = regexp.MustCompile(`^(cluster|audit|[a-z][a-z-]*\.[A-Za-z0-9._-]+)$`)

func (m *Module) logsTool(ctx context.Context, env *registry.Env, in logsInput) (string, error) {
	src := firstNonEmpty(in.Source, "cluster")
	if !sourceRe.MatchString(src) || strings.Contains(src, "..") {
		return "", fmt.Errorf("invalid source %q (cluster, audit or a daemon such as osd.3)", in.Source)
	}
	level := firstNonEmpty(in.Level, "warn")
	if level != "warn" && level != "error" && level != "all" {
		return "", fmt.Errorf("invalid level %q (warn, error, all)", in.Level)
	}
	if len(in.Grep) > 200 {
		return "", fmt.Errorf("grep text too long")
	}
	lines := clampLimit(in.Lines, 100, 1000)
	s := m.site(env)
	keep := lineFilter(src, level, in.Grep)

	// Package installations (and cephadm with log_to_file) write files.
	if f := logFile(s, src); f != "" {
		text, err := tailFile(f, tailBytes)
		if err != nil {
			return "", err
		}
		return renderLog(f, text, keep, lines), nil
	}
	// cephadm logs to journald, per daemon unit; the cluster and audit
	// channels go through the monitor's log.
	unit, channel := journalUnit(s, src)
	if unit == "" {
		units, _ := localUnits(ctx, env)
		return "", fmt.Errorf("%s: no log on this host (%s)", src, noLogHint(src, units))
	}
	if channel != "" {
		keep = and(keep, func(l string) bool { return strings.Contains(l, "log_channel("+channel+")") })
	}
	// Read more than asked: the filters drop most lines.
	want := lines * 20
	if level == "all" && in.Grep == "" && channel == "" {
		want = lines
	}
	r := env.Exec.RunTail(ctx, "journalctl", "--no-pager", "--quiet", "--output=short-iso", "--unit="+unit, "--lines="+strconv.Itoa(min(want, 50000)))
	if !r.OK() {
		return "", fmt.Errorf("journalctl: %s", strings.TrimSpace(r.Format()))
	}
	return renderLog("journal of "+unit, r.Stdout, keep, lines), nil
}

// logFile returns a non-empty log file for src, or "" (cephadm leaves the
// files empty unless log_to_file is enabled).
func logFile(s site, src string) string {
	name := "ceph-" + src + ".log"
	switch src {
	case "cluster":
		name = "ceph.log"
	case "audit":
		name = "ceph.audit.log"
	}
	dirs := []string{varLogCeph}
	if s.FSID != "" {
		dirs = append([]string{filepath.Join(varLogCeph, s.FSID)}, dirs...)
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// journalUnit maps src to the systemd unit holding its log and, for the
// cluster and audit logs, the channel to keep from the monitor's log.
func journalUnit(s site, src string) (unit, channel string) {
	if src == "cluster" || src == "audit" {
		mons, _ := filepath.Glob(filepath.Join(varLibCeph, s.FSID, "mon.*"))
		if s.Variant != VariantCephadm || s.FSID == "" || len(mons) == 0 {
			return "", ""
		}
		return "ceph-" + s.FSID + "@" + filepath.Base(mons[0]) + ".service", src
	}
	if s.Variant == VariantCephadm && s.FSID != "" {
		if exists(filepath.Join(varLibCeph, s.FSID, src)) {
			return "ceph-" + s.FSID + "@" + src + ".service", ""
		}
		return "", ""
	}
	typ, id, _ := strings.Cut(src, ".")
	if exists(filepath.Join(varLibCeph, typ, "ceph-"+id)) {
		return "ceph-" + typ + "@" + id + ".service", ""
	}
	return "", ""
}

func noLogHint(src string, units []unit) string {
	if src == "cluster" || src == "audit" {
		return "the cluster and audit logs are kept by the monitors; ask a host running a monitor (see ceph_status)"
	}
	var ds []string
	for _, u := range units {
		ds = append(ds, u.Daemon)
	}
	if len(ds) == 0 {
		return "no Ceph daemons run here"
	}
	return "daemons here: " + strings.Join(ds, ", ")
}

// lineFilter keeps lines of the requested level and text. Cluster and
// audit lines carry [DBG] [INF] [WRN] [ERR]; daemon lines a numeric level
// after the thread id, where -1 is an error and 0 important.
func lineFilter(src, level, grep string) func(string) bool {
	grep = strings.ToLower(grep)
	return func(l string) bool {
		if grep != "" && !strings.Contains(strings.ToLower(l), grep) {
			return false
		}
		switch level {
		case "all":
			return true
		case "error":
			return strings.Contains(l, "[ERR]") || daemonLevel(l) == -1 && !strings.Contains(l, "[DBG]") && !strings.Contains(l, "[INF]") && !strings.Contains(l, "[WRN]")
		}
		if strings.Contains(l, "[WRN]") || strings.Contains(l, "[ERR]") || strings.Contains(l, "[SEC]") {
			return true
		}
		if strings.Contains(l, "[DBG]") || strings.Contains(l, "[INF]") {
			return false // cluster log entries echoed by a daemon at level 0
		}
		if src == "audit" {
			return false // audit entries are [INF]; only warnings pass
		}
		lv := daemonLevel(l)
		return lv == -1 || lv == 0
	}
}

var daemonLevelRe = regexp.MustCompile(`\d{2}:\d{2}:\d{2}\.\d+[+-]\d{4} [0-9a-f]+ +(-?\d+) `)

// daemonLevel extracts the level of a daemon log line, 99 if none.
func daemonLevel(l string) int {
	if m := daemonLevelRe.FindStringSubmatch(l); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 99
}

func and(a, b func(string) bool) func(string) bool {
	return func(l string) bool { return a(l) && b(l) }
}

// tailFile reads at most n bytes from the end of path, starting at a line.
func tailFile(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if st.Size() > n {
		off = st.Size() - n
	}
	data, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return "", err
	}
	text := string(data)
	if off > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return text, nil
}

func renderLog(source, text string, keep func(string) bool, lines int) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l != "" && keep(l) {
			out = append(out, l)
		}
	}
	total := len(out)
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Source: %s — %d matching lines", source, total)
	if total > len(out) {
		fmt.Fprintf(&b, ", last %d shown", len(out))
	}
	b.WriteString("\n")
	for _, l := range out {
		b.WriteString(maskSecrets(l))
		b.WriteByte('\n')
	}
	return b.String()
}

var (
	cephKeyRe = regexp.MustCompile(`AQ[A-Za-z0-9+/]{30,}={0,2}`)
	// secretFieldRe matches JSON-ish fields whose name says secret.
	secretFieldRe = regexp.MustCompile(`(?i)("?(?:password|passwd|secret|secret_key|access_key|token|api_key)"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,}\]]+)`)
	// The value of "config-key set" / "config set" commands whose key or
	// option name looks secret ("val" or "value" field).
	secretNameRe = regexp.MustCompile(`(?i)"(?:key|name|who)"\s*:\s*"[^"]*(?:pass|secret|token|key|cred)[^"]*"`)
	valFieldRe   = regexp.MustCompile(`("(?:val|value)"\s*:\s*)"[^"]*"`)
)

// maskSecrets hides Ceph keys and values of secret-looking fields; the
// audit log records full command lines, including such values.
func maskSecrets(l string) string {
	l = cephKeyRe.ReplaceAllString(l, "***CEPH-KEY***")
	l = secretFieldRe.ReplaceAllString(l, `$1"***"`)
	if secretNameRe.MatchString(l) {
		l = valFieldRe.ReplaceAllString(l, `$1"***"`)
	}
	return l
}
