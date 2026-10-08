// Package tier defines the ordered permission levels of OmnibusMCP.
//
// Tiers are cumulative: a server running at tier N exposes every tool whose
// minimum tier is <= N. New levels are added by appending a constant and
// raising Max.
package tier

import "fmt"

// Tier is an ordered permission level.
type Tier int

const (
	// ReadOnly allows only commands that do not modify the system
	// (status, logs, configuration reads).
	ReadOnly Tier = 1
	// ServiceControl adds restarting of allow-listed services.
	ServiceControl Tier = 2

	// Min and Max bound the valid range.
	Min = ReadOnly
	Max = ServiceControl
)

var names = map[Tier]string{
	ReadOnly:       "read-only",
	ServiceControl: "service-control",
}

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool { return t >= Min && t <= Max }

// Allows reports whether a server at tier t may expose a tool requiring min.
func (t Tier) Allows(min Tier) bool { return t.Valid() && min.Valid() && min <= t }

func (t Tier) String() string {
	if n, ok := names[t]; ok {
		return fmt.Sprintf("%d (%s)", int(t), n)
	}
	return fmt.Sprintf("%d (unknown)", int(t))
}
