package ceph

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
)

// Installation variants.
const (
	VariantCephadm = "cephadm" // daemons in containers, units ceph-<fsid>@type.id
	VariantPVE     = "pveceph" // Proxmox VE packages, config in /etc/pve
	VariantNative  = "native"  // distribution packages, units ceph-type@id
)

// Default locations; paths in the configuration take precedence.
var (
	defaultConfs    = []string{"/etc/ceph/ceph.conf"}
	defaultKeyrings = []string{
		"/etc/omnibusmcp/ceph.client.omnibusmcp.keyring",
		"/etc/pve/priv/ceph.client.omnibusmcp.keyring",
	}
	varLibCeph = "/var/lib/ceph"
	pveConf    = "/etc/pve/ceph.conf"
	// fsTimeout bounds filesystem probes: /etc/pve is a FUSE filesystem
	// that blocks when pve-cluster hangs.
	fsTimeout = 3 * time.Second
)

// clientName is the read-only key the module authenticates with.
const clientName = "client.omnibusmcp"

// KeyCommand creates the key; it is shown where the key is missing.
const KeyCommand = `ceph auth get-or-create client.omnibusmcp mon 'allow r' mgr 'allow r' -o /etc/omnibusmcp/ceph.client.omnibusmcp.keyring`

// site describes the Ceph installation on this host.
type site struct {
	Variant string
	FSID    string
	Conf    string // cluster configuration, "" if none found
	Keyring string // client.omnibusmcp keyring, "" if none found
	CLI     bool   // the ceph command is installed
	// Cluster is set when the module queries the cluster; otherwise
	// LocalReason says why it shows only this host.
	Cluster     bool
	LocalReason string
}

var fsidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// installed reports any trace of a Ceph daemon installation: the Proxmox
// VE cluster config or daemon data in /var/lib/ceph. Client packages alone
// (ceph-common on every Proxmox VE host) leave neither.
func installed() bool {
	found := false
	if !withinFSTimeout(func() { found = exists(pveConf) || hasDaemonData() }) {
		return exists(varLibCeph) // /etc/pve hung: decide on local data only
	}
	return found
}

// withinFSTimeout runs f and reports whether it finished within fsTimeout.
// A probe stuck on a hung FUSE mount is abandoned, not waited for.
func withinFSTimeout(f func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
		return true
	case <-time.After(fsTimeout):
		return false
	}
}

// hasDaemonData finds daemon directories of a native (osd/ceph-0,
// mon/ceph-host) or cephadm (<fsid>/osd.0) installation.
func hasDaemonData() bool {
	for _, pat := range []string{"osd/ceph-*", "mon/ceph-*", "mgr/ceph-*", "mds/ceph-*", "*/osd.*", "*/mon.*", "*/mgr.*", "*/mds.*", "*/crash.*"} {
		if m, _ := filepath.Glob(filepath.Join(varLibCeph, pat)); len(m) > 0 {
			return true
		}
	}
	return false
}

// discover inspects the host. It reads only files and never blocks longer
// than fsTimeout.
func discover(cfg config.Ceph, lookPath func(string) error) site {
	var s site
	if !withinFSTimeout(func() { s = discoverFS(cfg) }) {
		return site{LocalReason: "configuration files did not answer within " + fsTimeout.String() + " (is /etc/pve hung?)"}
	}
	s.CLI = lookPath("ceph") == nil
	switch {
	case !cfg.ClusterView():
		s.LocalReason = "ceph.cluster is false in the OmnibusMCP configuration"
	case !s.CLI:
		s.LocalReason = "the ceph command is not installed (package ceph-common)"
	case s.Conf == "":
		s.LocalReason = "no ceph.conf found"
	case s.Keyring == "":
		s.LocalReason = "no " + clientName + " keyring; to see the cluster from this host, on a host with the admin key run: " + KeyCommand + " and copy the file here"
	default:
		s.Cluster = true
	}
	return s
}

func discoverFS(cfg config.Ceph) site {
	s := site{Variant: VariantNative}
	fsids := cephadmFSIDs()
	switch {
	case len(fsids) > 0:
		s.Variant = VariantCephadm
	case exists(pveConf):
		s.Variant = VariantPVE
	}

	confs := defaultConfs
	if cfg.Conf != "" {
		confs = []string{cfg.Conf}
	}
	for _, c := range confs {
		if exists(c) {
			s.Conf = c
			break
		}
	}
	// A cephadm host without the _admin label has no /etc/ceph/ceph.conf;
	// every daemon directory holds a minimal one.
	if s.Conf == "" && cfg.Conf == "" {
		for _, fsid := range fsids {
			if m, _ := filepath.Glob(filepath.Join(varLibCeph, fsid, "*", "config")); len(m) > 0 {
				s.Conf = m[0]
				break
			}
		}
	}
	if s.Conf != "" {
		s.FSID = confFSID(s.Conf)
	}
	if s.FSID == "" && len(fsids) == 1 {
		s.FSID = fsids[0]
	}

	keyrings := defaultKeyrings
	if cfg.Keyring != "" {
		keyrings = []string{cfg.Keyring}
	}
	for _, k := range keyrings {
		if exists(k) {
			s.Keyring = k
			break
		}
	}
	return s
}

// cephadmFSIDs lists /var/lib/ceph/<fsid> directories with daemons.
func cephadmFSIDs() []string {
	entries, _ := os.ReadDir(varLibCeph)
	var out []string
	for _, e := range entries {
		if e.IsDir() && fsidRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// confFSID reads "fsid = ..." from a ceph.conf.
func confFSID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && strings.TrimSpace(strings.ReplaceAll(k, "_", " ")) == "fsid" {
			if v = strings.TrimSpace(v); fsidRe.MatchString(v) {
				return v
			}
		}
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
