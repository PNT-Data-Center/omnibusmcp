package proxmox

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// PVE firewall state is read from its configuration, the same way PVE
// decides it: "pve-firewall status" cannot be used because it runs
// "ipset save", which needs CAP_NET_ADMIN (not granted to the sandbox).
type fwState struct {
	DCEnabled   bool   // cluster.fw [OPTIONS] enable: 1 (default 0)
	HostEnabled bool   // host.fw [OPTIONS] enable (default 1)
	NFTables    bool   // host.fw [OPTIONS] nftables: 1 -> proxmox-firewall
	Daemon      string // unit expected to enforce the rules
	DaemonState string // its systemctl is-active state
	ReadErr     error  // cluster.fw unreadable (not "missing")
}

// fwOptions returns the [OPTIONS] section of a PVE .fw file; a missing file
// yields no options (all defaults).
func fwOptions(path string) (map[string]string, error) {
	opts := map[string]string{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return opts, nil
	}
	if err != nil {
		return opts, err
	}
	defer f.Close()
	return parseFwOptions(bufio.NewScanner(f)), nil
}

func parseFwOptions(sc *bufio.Scanner) map[string]string {
	opts := map[string]string{}
	inOptions := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inOptions = strings.EqualFold(strings.Trim(line, "[] "), "OPTIONS")
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok && inOptions {
			opts[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return opts
}

func firewallState(ctx context.Context, env *registry.Env, node string) fwState {
	var st fwState
	dc, err := fwOptions("/etc/pve/firewall/cluster.fw")
	st.ReadErr = err
	host, _ := fwOptions("/etc/pve/nodes/" + node + "/host.fw")
	st.DCEnabled = dc["enable"] == "1"
	st.HostEnabled = host["enable"] != "0"
	st.NFTables = host["nftables"] == "1"
	st.Daemon = "pve-firewall"
	if st.NFTables {
		st.Daemon = "proxmox-firewall"
	}
	r := env.Exec.Run(ctx, "systemctl", "is-active", st.Daemon)
	st.DaemonState = firstNonEmpty(strings.TrimSpace(r.Stdout), "unknown")
	return st
}

// check turns the state into a health verdict. A disabled firewall is not
// a fault (many hosts sit behind a perimeter firewall), but it is stated
// explicitly so an agent does not mistake a running daemon for protection.
func (st fwState) check() registry.Check {
	c := registry.Check{Name: "firewall", Status: registry.OK}
	backend := "iptables"
	if st.NFTables {
		backend = "nftables"
	}
	switch {
	case st.ReadErr != nil:
		c.Status, c.Detail = registry.Unknown, "cannot read /etc/pve/firewall/cluster.fw: "+st.ReadErr.Error()
	case !st.DCEnabled:
		c.Detail = "PVE firewall DISABLED at datacenter level (cluster.fw enable != 1): no PVE firewall rules are enforced (" + st.Daemon + " daemon " + st.DaemonState + ")"
	case !st.HostEnabled:
		c.Detail = "PVE firewall enabled for the datacenter but DISABLED for this node (host.fw enable: 0)"
	case st.DaemonState != "active":
		c.Status = registry.Warn
		c.Detail = "PVE firewall enabled but " + st.Daemon + " is " + st.DaemonState + ": rules are NOT enforced"
	default:
		c.Detail = "PVE firewall enabled and enforced (" + backend + " via " + st.Daemon + ")"
	}
	return c
}
