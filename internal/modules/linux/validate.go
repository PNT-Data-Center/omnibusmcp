package linux

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	unitRe     = regexp.MustCompile(`^[A-Za-z0-9@_.:\\-]{1,200}$`)
	relativeRe = regexp.MustCompile(`^(\d{1,5})(s|m|min|h|d|w)$`)
	absoluteRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}( \d{2}:\d{2}(:\d{2})?)?$`)
)

var priorities = map[string]bool{
	"emerg": true, "alert": true, "crit": true, "err": true,
	"warning": true, "notice": true, "info": true, "debug": true,
	"0": true, "1": true, "2": true, "3": true, "4": true, "5": true, "6": true, "7": true,
}

// validUnit accepts systemd unit names and globs are rejected.
func validUnit(u string) error {
	if !unitRe.MatchString(u) || strings.HasPrefix(u, "-") {
		return fmt.Errorf("invalid unit name %q", u)
	}
	return nil
}

// journalTime converts "30m", "2h", "1d", "today", "yesterday",
// "2026-09-28" or "2026-09-28 10:00" to a journalctl time spec.
func journalTime(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "today" || s == "yesterday" || s == "now":
		return s, nil
	case absoluteRe.MatchString(s):
		return s, nil
	}
	if m := relativeRe.FindStringSubmatch(s); m != nil {
		unit := m[2]
		if unit == "m" {
			unit = "min" // systemd: "m" is minutes, but be explicit
		}
		return "-" + m[1] + unit, nil
	}
	return "", fmt.Errorf("invalid time %q (use e.g. 30m, 2h, 1d, today, 2026-09-28 10:00)", s)
}

func validPriority(p string) error {
	if !priorities[p] {
		return fmt.Errorf("invalid priority %q (emerg, alert, crit, err, warning, notice, info, debug or 0-7)", p)
	}
	return nil
}

// clamp returns def when v is 0, otherwise v bounded to [1, max].
func clamp(v, def, max int) int {
	switch {
	case v == 0:
		return def
	case v < 1:
		return 1
	case v > max:
		return max
	}
	return v
}

func itoa(i int) string { return strconv.Itoa(i) }
