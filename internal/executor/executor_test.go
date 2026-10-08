package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunOK(t *testing.T) {
	r := New(5*time.Second, 1024).Run(context.Background(), "echo", "hello", "; rm -rf /")
	if !r.OK() || r.Stdout != "hello ; rm -rf /\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestRunExitCode(t *testing.T) {
	r := New(5*time.Second, 1024).Run(context.Background(), "false")
	if r.OK() || r.ExitCode != 1 || r.Err != nil {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !strings.Contains(r.Format(), "[exit status 1]") {
		t.Fatalf("format: %q", r.Format())
	}
}

func TestRunTimeoutKillsGroup(t *testing.T) {
	start := time.Now()
	r := New(200*time.Millisecond, 1024).Run(context.Background(), "sleep", "30")
	if !r.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: %+v after %s", r, time.Since(start))
	}
	if !strings.Contains(r.Format(), "TIMEOUT") {
		t.Fatalf("format: %q", r.Format())
	}
}

func TestRunNotFound(t *testing.T) {
	r := New(time.Second, 1024).Run(context.Background(), "definitely-not-a-command-xyz")
	if !r.NotFound || r.OK() {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestRunTruncates(t *testing.T) {
	r := New(5*time.Second, 1024).Run(context.Background(), "seq", "1", "100000")
	if !r.Truncated || len(r.Stdout) != 1024 || !r.OK() {
		t.Fatalf("truncated=%v len=%d ok=%v", r.Truncated, len(r.Stdout), r.OK())
	}
}

func TestChildEnvIsMinimal(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "x")
	r := New(5*time.Second, 4096).Run(context.Background(), "env")
	if strings.Contains(r.Stdout, "SECRET_TOKEN") || !strings.Contains(r.Stdout, "LC_ALL=C.UTF-8") {
		t.Fatalf("env leaked or incomplete: %s", r.Stdout)
	}
}

func TestRunTailKeepsNewest(t *testing.T) {
	r := New(5*time.Second, 1024).RunTail(context.Background(), "seq", "1", "100000")
	if !r.Truncated || !r.KeptTail || !r.OK() {
		t.Fatalf("truncated=%v keptTail=%v ok=%v", r.Truncated, r.KeptTail, r.OK())
	}
	if !strings.HasSuffix(r.Stdout, "\n100000\n") || len(r.Stdout) > 1024 {
		t.Fatalf("tail lost newest lines: len=%d end=%q", len(r.Stdout), r.Stdout[len(r.Stdout)-20:])
	}
	if first := strings.SplitN(r.Stdout, "\n", 2)[0]; len(first) != 5 && len(first) != 6 {
		t.Fatalf("first line is partial: %q", first)
	}
	if !strings.Contains(r.Format(), "older lines omitted") {
		t.Fatalf("format: %q", r.Format()[:200])
	}
}

func TestRunTailShortOutputUntouched(t *testing.T) {
	r := New(5*time.Second, 1024).RunTail(context.Background(), "seq", "1", "3")
	if r.Truncated || r.Stdout != "1\n2\n3\n" {
		t.Fatalf("%+v", r)
	}
}

func TestRunDataAllowsLargerOutput(t *testing.T) {
	e := New(5*time.Second, 1024)
	r := e.RunData(context.Background(), 1<<20, "seq", "1", "100000")
	if r.Truncated || !r.OK() || !strings.HasSuffix(r.Stdout, "\n100000\n") {
		t.Fatalf("truncated=%v ok=%v len=%d", r.Truncated, r.OK(), len(r.Stdout))
	}
}

func TestConcurrencyLimit(t *testing.T) {
	e := New(5*time.Second, 1024)
	e.slots = make(chan struct{}, 2)
	start := time.Now()
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.Run(context.Background(), "sleep", "0.2"); !r.OK() {
				t.Errorf("sleep: %+v", r)
			}
		}()
	}
	wg.Wait()
	// 6 commands, 2 at a time, 0.2 s each: at least 3 rounds.
	if d := time.Since(start); d < 550*time.Millisecond {
		t.Fatalf("finished in %v: more than 2 commands ran at once", d)
	}
}

func TestBusyWhenNoSlot(t *testing.T) {
	e := New(5*time.Second, 1024)
	e.slots = make(chan struct{}, 1)
	go e.Run(context.Background(), "sleep", "1")
	time.Sleep(100 * time.Millisecond)
	r := e.RunTimeout(context.Background(), 200*time.Millisecond, "true")
	if !errors.Is(r.Err, ErrBusy) || !strings.Contains(r.Format(), "server busy") {
		t.Fatalf("expected ErrBusy, got %+v", r)
	}
}
