package install

import (
	"bufio"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// UnitState is the systemd view of a unit ("systemctl show").
type UnitState struct {
	Installed     bool   // LoadState != not-found
	UnitFileState string // enabled, disabled, ...
	ActiveState   string // active, inactive, failed, activating
	SubState      string // running, dead, auto-restart...
	MainPID       int
	Since         string // ActiveEnterTimestamp or InactiveEnterTimestamp
	ExitStatus    int
	Result        string
	NextElapse    string // timers only
}

// Running reports an active unit with a live main process.
func (u UnitState) Running() bool { return u.ActiveState == "active" && u.MainPID > 0 }

var showProps = "LoadState,UnitFileState,ActiveState,SubState,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,ExecMainStatus,Result,NextElapseUSecRealtime"

// QueryUnit asks systemd for the state of unit.
func QueryUnit(unit string) (UnitState, error) {
	out, err := exec.Command("systemctl", "show", unit, "--property="+showProps).Output()
	if err != nil {
		return UnitState{}, err
	}
	return ParseUnitState(string(out)), nil
}

// ParseUnitState parses "systemctl show" key=value output.
func ParseUnitState(out string) UnitState {
	p := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			p[k] = v
		}
	}
	u := UnitState{
		Installed:     p["LoadState"] != "" && p["LoadState"] != "not-found",
		UnitFileState: p["UnitFileState"],
		ActiveState:   p["ActiveState"],
		SubState:      p["SubState"],
		Result:        p["Result"],
		NextElapse:    p["NextElapseUSecRealtime"],
	}
	u.MainPID, _ = strconv.Atoi(p["MainPID"])
	u.ExitStatus, _ = strconv.Atoi(p["ExecMainStatus"])
	u.Since = p["ActiveEnterTimestamp"]
	if u.ActiveState != "active" {
		u.Since = p["InactiveEnterTimestamp"]
	}
	return u
}

var execPathRe = regexp.MustCompile(`(?:^|[{;\s])path=(\S+)`)

// ExecPath returns the binary the installed service runs (the path of its
// ExecStart, drop-ins included), or "" if the unit is not installed.
func ExecPath() (string, error) {
	out, err := exec.Command("systemctl", "show", UnitName, "--property=LoadState,ExecStart").Output()
	if err != nil {
		return "", err
	}
	return ParseExecPath(string(out)), nil
}

// ParseExecPath reads the binary path from "systemctl show
// --property=LoadState,ExecStart" output.
func ParseExecPath(out string) string {
	var load, start string
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			switch k {
			case "LoadState":
				load = v
			case "ExecStart":
				start = v
			}
		}
	}
	if load == "" || load == "not-found" {
		return ""
	}
	if m := execPathRe.FindStringSubmatch(start); m != nil {
		return m[1]
	}
	return ""
}

var startedVersionRe = regexp.MustCompile(`msg="OmnibusMCP started" version=(\S+)`)

// RunningVersion returns the version logged by the current service start,
// or "" if unknown (no journal access, service never started...).
func RunningVersion() string {
	out, err := exec.Command("journalctl", "--unit", UnitName, "--output=cat", "--no-pager", "--lines=200").Output()
	if err != nil {
		return ""
	}
	return ParseRunningVersion(string(out))
}

// ParseRunningVersion returns the version of the last start in the log.
func ParseRunningVersion(journal string) string {
	all := startedVersionRe.FindAllStringSubmatch(journal, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

// Endpoint describes where the MCP endpoint is (or will be) reachable.
type Endpoint struct {
	URL       string // https://192.0.2.10:8765/mcp
	Listen    string // configured listen address
	Probe     string // address used for the TCP check
	Listening bool
}

// ResolveEndpoint builds the client URL for a listen address and checks
// whether something accepts TCP connections on it.
func ResolveEndpoint(listen string, tls bool) Endpoint {
	e := Endpoint{Listen: listen, Probe: listen}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		e.URL = scheme + "://" + listen + "/mcp"
		return e
	}
	urlHost := host
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() || host == "" {
		// A wildcard listener is reachable on every address; probe loopback
		// and show a placeholder the user replaces with a real address.
		urlHost = "<host-address>"
		e.Probe = net.JoinHostPort("127.0.0.1", port)
	}
	e.URL = scheme + "://" + net.JoinHostPort(urlHost, port) + "/mcp"
	if conn, err := net.DialTimeout("tcp", e.Probe, time.Second); err == nil {
		conn.Close()
		e.Listening = true
	}
	return e
}
