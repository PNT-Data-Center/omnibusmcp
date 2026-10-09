package ceph

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// crashEntry is one entry of "ceph crash ls".
type crashEntry struct {
	ID        string `json:"crash_id"`
	Timestamp string `json:"timestamp"`
	Entity    string `json:"entity_name"`
	Archived  string `json:"archived"`
}

// crashInfo is "ceph crash info <id>".
type crashInfo struct {
	ID            string   `json:"crash_id"`
	Timestamp     string   `json:"timestamp"`
	Entity        string   `json:"entity_name"`
	Process       string   `json:"process_name"`
	Version       string   `json:"ceph_version"`
	Host          string   `json:"utsname_hostname"`
	OSRelease     string   `json:"os_version"`
	AssertCond    string   `json:"assert_condition"`
	AssertFunc    string   `json:"assert_func"`
	AssertFile    string   `json:"assert_file"`
	AssertLine    int      `json:"assert_line"`
	AssertMsg     string   `json:"assert_msg"`
	Backtrace     []string `json:"backtrace"`
	StackSig      string   `json:"stack_sig"`
	CrashedThread string   `json:"assert_thread_name"`
}

type crashesInput struct {
	ID    string `json:"id,omitempty" jsonschema:"crash ID from the list: shows its details (assertion, backtrace)"`
	New   bool   `json:"new,omitempty" jsonschema:"only crashes not yet archived (acknowledged)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of crashes listed (default 30, max 300)"`
}

var crashIDRe = regexp.MustCompile(`^[0-9T:.\-]+Z_[0-9a-f-]{36}$`)

const backtraceLines = 25

func (m *Module) crashesTool(ctx context.Context, env *registry.Env, in crashesInput) (string, error) {
	s := m.site(env)
	if in.ID != "" {
		if !crashIDRe.MatchString(in.ID) {
			return "", fmt.Errorf("invalid crash ID %q", in.ID)
		}
		var ci crashInfo
		if err := query(ctx, env, s, &ci, "crash", "info", in.ID); err != nil {
			return "", err
		}
		return renderCrashInfo(ci), nil
	}
	sub := "ls"
	if in.New {
		sub = "ls-new"
	}
	var list []crashEntry
	if err := query(ctx, env, s, &list, "crash", sub); err != nil {
		return "", err
	}
	return renderCrashes(list, localCrashes(s), in.New, clampLimit(in.Limit, 30, 300), time.Now()), nil
}

func renderCrashes(list []crashEntry, local []crash, onlyNew bool, limit int, now time.Time) string {
	var b strings.Builder
	sort.Slice(list, func(i, j int) bool { return list[i].Timestamp > list[j].Timestamp })
	what := "Crashes reported to the cluster"
	if onlyNew {
		what = "New (not archived) crashes"
	}
	newCount := 0
	for _, c := range list {
		if c.Archived == "" {
			newCount++
		}
	}
	fmt.Fprintf(&b, "%s: %d", what, len(list))
	if !onlyNew {
		fmt.Fprintf(&b, " (%d not archived)", newCount)
	}
	b.WriteString("\n")
	if len(list) > 0 {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TIME\tENTITY\tARCHIVED\tID")
		for i, c := range list {
			if i == limit {
				fmt.Fprintf(tw, "  ... %d more (raise limit)\n", len(list)-i)
				break
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", shortTime(c.Timestamp), c.Entity, firstNonEmpty(c.Archived, "no"), c.ID)
		}
		tw.Flush()
		b.WriteString("Details of one crash: id parameter. Archived crashes no longer raise RECENT_CRASH.\n")
	}
	if len(local) > 0 {
		fmt.Fprintf(&b, "\nOn this host %d crash reports were never sent to the cluster (newest %s, %s); see ceph_local.\n",
			len(local), local[0].Time.Format("2006-01-02"), firstNonEmpty(local[0].Entity, "?"))
	}
	return b.String()
}

func renderCrashInfo(c crashInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Crash:   %s\nTime:    %s\nEntity:  %s (%s) on %s\nVersion: %s\n",
		c.ID, c.Timestamp, c.Entity, firstNonEmpty(c.Process, "?"), firstNonEmpty(c.Host, "?"), firstNonEmpty(c.Version, "?"))
	if c.CrashedThread != "" {
		fmt.Fprintf(&b, "Thread:  %s\n", c.CrashedThread)
	}
	if c.AssertCond != "" || c.AssertMsg != "" {
		fmt.Fprintf(&b, "Assertion: %s in %s (%s:%d)\n", c.AssertCond, c.AssertFunc, c.AssertFile, c.AssertLine)
		if c.AssertMsg != "" {
			fmt.Fprintf(&b, "Message: %s\n", strings.TrimSpace(c.AssertMsg))
		}
	}
	if c.StackSig != "" {
		fmt.Fprintf(&b, "Stack signature: %s (same signature = same bug)\n", c.StackSig)
	}
	if len(c.Backtrace) > 0 {
		b.WriteString("Backtrace:\n")
		for i, l := range c.Backtrace {
			if i == backtraceLines {
				fmt.Fprintf(&b, "  ... %d more frames\n", len(c.Backtrace)-i)
				break
			}
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	return b.String()
}

// shortTime trims Ceph timestamps ("2025-11-20T09:40:01.603597Z") to minutes.
func shortTime(ts string) string {
	if len(ts) >= 16 {
		return strings.Replace(ts[:16], "T", " ", 1)
	}
	return ts
}
