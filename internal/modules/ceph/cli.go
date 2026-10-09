package ceph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/executor"
	"github.com/PNT-Data-Center/omnibusmcp/internal/jsonx"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// Every cluster query is "ceph --name client.omnibusmcp ... --format json"
// (0.3-1 s each). The read-only key is enough for every command used here.
const (
	connectTimeout = "10" // seconds; "ceph" gives up on unreachable monitors
	maxCLIBytes    = 16 << 20
	cliSlots       = 3 // health runs several queries; spare the monitors
	cliCacheTTL    = 10 * time.Second
)

// Failure kinds of a cluster query, which decide the health status.
const (
	failUnreachable = "unreachable" // monitors did not answer: CRIT
	failAccess      = "access"      // key, permission or config problem: WARN
	failCommand     = "command"     // the command itself failed (e.g. no orchestrator)
)

// cliError is a failed cluster query.
type cliError struct {
	Kind string
	Msg  string
}

func (e *cliError) Error() string { return e.Msg }

var (
	slots   = make(chan struct{}, cliSlots)
	cacheMu sync.Mutex
	cache   = map[string]*cliResult{}
	// runCLI executes ceph; tests replace it.
	runCLI = func(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
		r := env.Exec.RunData(ctx, maxCLIBytes, "ceph", argv...)
		if r.OK() && !r.Truncated {
			return []byte(r.Stdout), nil
		}
		return nil, classify(r)
	}
)

type cliResult struct {
	done chan struct{}
	at   time.Time
	out  []byte
	err  error
}

// clusterArgs are the connection arguments for s.
func clusterArgs(s site) []string {
	return []string{"--name", clientName, "--keyring", s.Keyring, "--conf", s.Conf, "--connect-timeout", connectTimeout}
}

// query runs "ceph <args> --format json" against the cluster and decodes
// the first JSON value of the output (some commands print "ok" after it).
// Identical queries within cliCacheTTL share one result: every host runs
// OmnibusMCP, and agents ask several of them at once.
func query(ctx context.Context, env *registry.Env, s site, out any, args ...string) error {
	if !s.Cluster {
		return &cliError{Kind: failAccess, Msg: "cluster view disabled: " + s.LocalReason}
	}
	argv := append(append(clusterArgs(s), args...), "--format", "json")
	data, err := cached(ctx, env, argv)
	if err != nil {
		return err
	}
	label := "ceph " + strings.Join(args, " ")
	if err := decodeFirst(label, data, out); err != nil {
		return fmt.Errorf("%s: invalid JSON: %w", label, err)
	}
	return nil
}

// decodeFirst decodes the first JSON value in data, tolerating a leading
// blank line and trailing text.
func decodeFirst(label string, data []byte, out any) error {
	var raw json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return err
	}
	return jsonx.Decode(label, raw, out)
}

func cached(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
	key := strings.Join(argv, "\x00")
	cacheMu.Lock()
	if r, ok := cache[key]; ok {
		select {
		case <-r.done:
			if r.err == nil && time.Since(r.at) < cliCacheTTL {
				cacheMu.Unlock()
				return r.out, nil
			}
		default:
			cacheMu.Unlock()
			select {
			case <-r.done:
				return r.out, r.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	r := &cliResult{done: make(chan struct{})}
	cache[key] = r
	for k, old := range cache {
		select {
		case <-old.done:
			if time.Since(old.at) >= cliCacheTTL {
				delete(cache, k)
			}
		default:
		}
	}
	cacheMu.Unlock()

	r.out, r.err = limited(ctx, env, argv)
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

func limited(ctx context.Context, env *registry.Env, argv []string) ([]byte, error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a free ceph slot: %w", ctx.Err())
	}
	return runCLI(ctx, env, argv)
}

// classify turns a failed ceph run into a cliError. "ceph" reports the
// cases with distinct messages and exit codes.
func classify(r executor.Result) error {
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(r.Stdout)
	}
	last := lastLine(msg)
	low := strings.ToLower(msg)
	switch {
	case r.NotFound:
		return &cliError{Kind: failAccess, Msg: "the ceph command is not installed (package ceph-common)"}
	case r.Truncated:
		return &cliError{Kind: failCommand, Msg: fmt.Sprintf("ceph output larger than %d bytes", maxCLIBytes)}
	case r.TimedOut || strings.Contains(low, "timed out") || r.ExitCode == 110:
		return &cliError{Kind: failUnreachable, Msg: "monitors unreachable: possible loss of quorum or network failure (" + firstNonEmpty(last, "timed out") + ")"}
	case errors.Is(r.Err, executor.ErrBusy):
		return &cliError{Kind: failCommand, Msg: r.Err.Error()}
	case r.Err != nil:
		return &cliError{Kind: failCommand, Msg: r.Err.Error()}
	case strings.Contains(low, "permission denied") || strings.Contains(low, "access denied") || r.ExitCode == 13 ||
		strings.Contains(low, "keyring") || strings.Contains(low, "conf_read_file") || strings.Contains(low, "error initializing cluster client"):
		return &cliError{Kind: failAccess, Msg: "this host cannot query the cluster: " + firstNonEmpty(last, fmt.Sprintf("exit status %d", r.ExitCode))}
	}
	return &cliError{Kind: failCommand, Msg: firstNonEmpty(last, fmt.Sprintf("ceph exited with status %d", r.ExitCode))}
}

// failKind returns the kind of a query error ("" if not a cliError).
func failKind(err error) string {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return ""
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
