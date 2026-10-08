// Package ui adds ANSI colors to human-facing CLI output. Colors are used
// only on terminals; MCP tool output and service logs never use this
// package.
//
// Control: OMNIBUSMCP_COLOR=always|never|auto (default auto) and the
// NO_COLOR convention (https://no-color.org/).
package ui

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

var enabled = decide(os.Stdout)

// decide applies the environment overrides, then checks for a terminal.
func decide(f *os.File) bool {
	switch strings.ToLower(os.Getenv("OMNIBUSMCP_COLOR")) {
	case "always":
		return true
	case "never":
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if t := os.Getenv("TERM"); t == "dumb" {
		return false
	}
	return isTerminal(f)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Enabled reports whether stdout output is colored.
func Enabled() bool { return enabled }

// SetEnabled overrides detection (tests).
func SetEnabled(on bool) { enabled = on }

// StderrEnabled reports whether stderr output may be colored.
func StderrEnabled() bool { return decide(os.Stderr) }

func wrap(code, s string) string {
	if !enabled || s == "" {
		return s
	}
	return code + s + reset
}

func Bold(s string) string   { return wrap(bold, s) }
func Dim(s string) string    { return wrap(dim, s) }
func Red(s string) string    { return wrap(red, s) }
func Green(s string) string  { return wrap(green, s) }
func Yellow(s string) string { return wrap(yellow, s) }
func Cyan(s string) string   { return wrap(cyan, s) }

// Level colors a value by severity.
type Level int

const (
	Info Level = iota
	Good
	Warn
	Bad
)

// Paint colors s according to its level.
func Paint(l Level, s string) string {
	switch l {
	case Good:
		return Green(s)
	case Warn:
		return Yellow(s)
	case Bad:
		return Red(s)
	}
	return s
}

// Label renders a left column label ("Service:" padded to width).
func Label(name string, width int) string {
	return Bold(fmt.Sprintf("%-*s", width, name+":"))
}

// Width is the visible length of s (ANSI codes excluded).
func Width(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
				i += j + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// Pad right-pads s to width visible characters.
func Pad(s string, width int) string {
	if w := Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// Table prints rows with columns aligned by visible width. The first row is
// the header and is rendered bold.
func Table(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && Width(c) > widths[i] {
				widths[i] = Width(c)
			}
		}
	}
	var b strings.Builder
	for ri, r := range rows {
		for i, c := range r {
			cell := c
			if ri == 0 {
				cell = Bold(c)
			}
			if i < len(r)-1 {
				cell = Pad(cell, widths[i]+2)
			}
			b.WriteString(cell)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
