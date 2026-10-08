// Package executor runs diagnostic commands safely: no shell, fixed argv,
// a hard timeout that kills the whole process group, and bounded output.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// safePATH is used both for lookup and inside the child environment, so the
// service environment cannot redirect commands.
const safePATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// MaxConcurrent bounds the commands running at once. Every child process
// and the OS thread waiting for it count against the unit's TasksMax; a burst
// of parallel requests (each health_summary runs a dozen commands, the docker
// CLI alone has ~10 threads) used to reach the limit, and a Go runtime that
// cannot create a thread aborts the whole server.
const MaxConcurrent = 8

// ErrBusy is returned when no command slot frees up within the timeout.
var ErrBusy = errors.New("server busy: too many commands running at once, retry shortly")

// Executor runs commands with default limits.
type Executor struct {
	Timeout   time.Duration
	MaxOutput int
	// slots limits concurrent commands; nil means unlimited.
	slots chan struct{}
}

// New returns an Executor with the given limits and at most MaxConcurrent
// commands running at once.
func New(timeout time.Duration, maxOutput int) *Executor {
	return &Executor{Timeout: timeout, MaxOutput: maxOutput, slots: make(chan struct{}, MaxConcurrent)}
}

// Result describes one finished (or failed) command.
type Result struct {
	Argv      []string
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  time.Duration
	Timeout   time.Duration
	TimedOut  bool
	Truncated bool
	// KeptTail is set when truncation kept the end of stdout (logs).
	KeptTail bool
	NotFound bool
	// Err is set when the command could not be started or was cancelled.
	Err error
}

// OK reports whether the command ran and exited with status 0.
func (r Result) OK() bool {
	return r.Err == nil && !r.TimedOut && !r.NotFound && r.ExitCode == 0
}

// Run executes name with args using the default timeout.
func (e *Executor) Run(ctx context.Context, name string, args ...string) Result {
	return e.RunTimeout(ctx, e.Timeout, name, args...)
}

// RunTail is like Run but, when stdout exceeds the limit, keeps its end
// instead of its beginning. Use it for logs, where the newest entries come last.
func (e *Executor) RunTail(ctx context.Context, name string, args ...string) Result {
	return e.run(ctx, e.Timeout, true, e.MaxOutput, name, args...)
}

// RunTimeout executes name with args and an explicit timeout.
func (e *Executor) RunTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) Result {
	return e.run(ctx, timeout, false, e.MaxOutput, name, args...)
}

// RunData runs a command whose output is machine-readable data (e.g. JSON)
// that the caller parses and condenses itself. It allows up to maxBytes so
// the data is not cut mid-document; the caller must bound what it returns.
func (e *Executor) RunData(ctx context.Context, maxBytes int, name string, args ...string) Result {
	return e.run(ctx, e.Timeout, false, maxBytes, name, args...)
}

func (e *Executor) run(ctx context.Context, timeout time.Duration, keepTail bool, maxOut int, name string, args ...string) Result {
	res := Result{Argv: append([]string{name}, args...), Timeout: timeout, ExitCode: -1}

	path, err := lookPath(name)
	if err != nil {
		res.NotFound = true
		res.Err = err
		return res
	}

	// Waiting for a slot is bounded by the command timeout, so a request
	// never queues longer than its command could have run.
	if e.slots != nil {
		wait := time.NewTimer(timeout)
		select {
		case e.slots <- struct{}{}:
			wait.Stop()
			defer func() { <-e.slots }()
		case <-ctx.Done():
			wait.Stop()
			res.Err = ctx.Err()
			return res
		case <-wait.C:
			res.Err = ErrBusy
			return res
		}
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, path, args...)
	cmd.Env = childEnv()
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the whole process group: tools like cephadm spawn children that
	// would otherwise survive the timeout and keep the pipes open.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	var stdout outputBuffer = &limitedBuffer{max: maxOut}
	var stderr outputBuffer = &limitedBuffer{max: max(e.MaxOutput/4, 1024)}
	if keepTail {
		// Logs: both streams keep their newest lines (docker logs writes
		// the container's stderr to stderr).
		stdout = &tailBuffer{max: maxOut}
		stderr = &tailBuffer{max: max(e.MaxOutput/4, 1024)}
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	err = cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	res.Truncated = stdout.Truncated() || stderr.Truncated()
	res.KeptTail = keepTail && (stdout.Truncated() || stderr.Truncated())

	var exitErr *exec.ExitError
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
	case ctx.Err() != nil:
		res.Err = ctx.Err()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		res.Err = err
	default:
		res.ExitCode = 0
	}
	return res
}

// Format renders the result for an LLM: the command line, its output and any
// abnormal condition spelled out explicitly.
func (r Result) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", quoteArgv(r.Argv))
	switch {
	case r.NotFound:
		fmt.Fprintf(&b, "[command not found: %s is not installed or not in PATH]\n", r.Argv[0])
		return b.String()
	case r.TimedOut:
		fmt.Fprintf(&b, "[TIMEOUT after %s: the command did not finish; the underlying service may be hung or unreachable (e.g. no quorum)]\n", r.Timeout)
	}
	if r.KeptTail {
		b.WriteString("[output truncated: older lines omitted, showing only the most recent output; narrow the query to see earlier entries]\n")
	}
	if out := strings.TrimRight(r.Stdout, "\n"); out != "" {
		b.WriteString(out)
		b.WriteByte('\n')
	}
	if errOut := strings.TrimRight(r.Stderr, "\n"); errOut != "" {
		b.WriteString("--- stderr ---\n")
		b.WriteString(errOut)
		b.WriteByte('\n')
	}
	if r.Truncated && !r.KeptTail {
		b.WriteString("[output truncated: the end of the output was omitted; narrow the query, e.g. fewer lines or a filter]\n")
	}
	if r.Err != nil {
		fmt.Fprintf(&b, "[error: %v]\n", r.Err)
	} else if !r.TimedOut && r.ExitCode != 0 {
		fmt.Fprintf(&b, "[exit status %d]\n", r.ExitCode)
	}
	return b.String()
}

func lookPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		return exec.LookPath(name)
	}
	for _, dir := range strings.Split(safePATH, ":") {
		p := dir + "/" + name
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

func childEnv() []string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return []string{
		"PATH=" + safePATH,
		"HOME=" + home,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=dumb",
		"PAGER=cat",
		"SYSTEMD_PAGER=",
		"SYSTEMD_COLORS=0",
		"NO_COLOR=1",
	}
}

func quoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$*?;|&<>()") {
			parts[i] = strconv.Quote(a)
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

type outputBuffer interface {
	io.Writer
	String() string
	Truncated() bool
}

// limitedBuffer keeps the first max bytes and silently discards the rest,
// reporting full writes so the child never blocks or gets EPIPE.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
			l.truncated = true
		} else {
			l.buf.Write(p)
		}
	} else if len(p) > 0 {
		l.truncated = true
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string  { return strings.ToValidUTF8(l.buf.String(), "\uFFFD") }
func (l *limitedBuffer) Truncated() bool { return l.truncated }

// tailBuffer keeps the last max bytes, starting at a line boundary once
// anything was dropped.
type tailBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	// Compact only when the buffer doubles, to keep copying amortised.
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	out := t.buf
	dropped := t.truncated
	if len(out) > t.max {
		out, dropped = out[len(out)-t.max:], true
	}
	if dropped {
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return strings.ToValidUTF8(string(out), "\uFFFD")
}

func (t *tailBuffer) Truncated() bool { return t.truncated || len(t.buf) > t.max }
