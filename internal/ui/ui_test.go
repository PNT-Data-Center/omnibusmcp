package ui

import (
	"strings"
	"testing"
)

func TestDisabledIsPlain(t *testing.T) {
	SetEnabled(false)
	for _, s := range []string{Red("x"), Green("x"), Bold("x"), Paint(Bad, "x"), Label("State", 12)} {
		if strings.Contains(s, "\033") {
			t.Fatalf("ANSI with colors disabled: %q", s)
		}
	}
}

func TestEnabledAndWidth(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)
	s := Green("active") + " " + Red("żółw")
	if !strings.Contains(s, "\033[32m") || Width(s) != len("active żółw")-3 {
		t.Fatalf("%q width %d", s, Width(s))
	}
	if Width(Pad(Red("ab"), 6)) != 6 {
		t.Fatal("pad by visible width")
	}
	tbl := Table([][]string{{"MODULE", "DETECTED"}, {Cyan("linux"), Green("yes")}, {"containers", Dim("no")}})
	lines := strings.Split(strings.TrimSpace(tbl), "\n")
	if len(lines) != 3 || Width(lines[1][:strings.Index(lines[1], "\033[32m")]) != Width(lines[2][:strings.Index(lines[2], "\033[2m")]) {
		t.Fatalf("columns not aligned:\n%s", tbl)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("OMNIBUSMCP_COLOR", "always")
	if !decide(nil) {
		t.Fatal("always")
	}
	t.Setenv("OMNIBUSMCP_COLOR", "never")
	if decide(nil) {
		t.Fatal("never")
	}
	t.Setenv("OMNIBUSMCP_COLOR", "")
	t.Setenv("NO_COLOR", "1")
	if decide(nil) {
		t.Fatal("NO_COLOR")
	}
}
