package linux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
)

// readRoots are the only trees file tools may read from. /usr/lib and
// /usr/share hold package defaults that /etc files often link to
// (e.g. /etc/os-release).
var readRoots = []string{"/etc", "/var/log", "/proc", "/sys", "/run", "/usr/lib", "/usr/share", "/lib", "/opt"}

// Secrets are refused even under the readable roots (ADR-026). The project
// accepts that ordinary configuration may contain secrets (ADR-007), but
// credential stores and private keys have no diagnostic value and some of
// them escalate read-only access to full control (e.g. Proxmox authkey.key
// signs API tickets).
var (
	// deniedFiles are exact paths.
	deniedFiles = []string{
		"/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-",
		"/etc/corosync/authkey", "/etc/ipsec.secrets",
	}
	// deniedDirs are refused together with everything below them.
	deniedDirs = []string{
		"/etc/pve/priv",    // Proxmox: authkey.key, CA key, token/TFA secrets, storage passwords
		"/etc/ssl/private", // TLS private keys
		"/etc/wireguard",
		"/etc/ipsec.d/private",
		"/etc/letsencrypt/archive", "/etc/letsencrypt/keys",
		"/etc/NetworkManager/system-connections", // Wi-Fi/VPN credentials
	}
	// deniedSuffixes match file names: TLS keys (e.g. Proxmox pve-ssl.key),
	// Ceph keyrings and SSH host keys (ssh_host_*_key).
	deniedSuffixes = []string{".key", ".keyring", "_key"}
)

// privateKeyMarker ends every PEM/OpenSSH private key header
// ("-----BEGIN [EC |RSA |OPENSSH ]PRIVATE KEY-----").
var privateKeyMarker = []byte("PRIVATE KEY-----")

// filePolicy adds the server's own secrets (token, TLS key) to the
// denied files: an agent never needs them and could leak them in a report.
type filePolicy struct {
	roots  []string
	denied []string
}

func newFilePolicy(cfg *config.Config) filePolicy {
	fp := filePolicy{roots: readRoots}
	for _, f := range []string{cfg.TokenFile, cfg.TLS.KeyFile} {
		if f == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(f); err == nil {
			f = real
		}
		fp.denied = append(fp.denied, filepath.Clean(f))
	}
	return fp
}

// deniedReason explains why path is refused, or returns "" if it is not.
func (fp filePolicy) deniedReason(path string) string {
	for _, d := range append(deniedFiles, fp.denied...) {
		if path == d {
			return "not readable by policy"
		}
	}
	for _, d := range deniedDirs {
		if path == d || strings.HasPrefix(path, d+"/") {
			return "in a secret store (" + d + ") and not readable by policy"
		}
	}
	base := filepath.Base(path)
	for _, suf := range deniedSuffixes {
		if strings.HasSuffix(base, suf) {
			return "a private key file (*" + suf + ") and not readable by policy"
		}
	}
	return ""
}

// resolveReadable returns the symlink-resolved path if it lies under an
// allowed root and is not denied. The policy is checked on the requested
// path and again on the resolved one, so a symlink cannot lead into a
// denied location (e.g. /root/.ssh/authorized_keys -> /etc/pve/priv on Proxmox).
func (fp filePolicy) resolveReadable(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %q", p)
	}
	clean := filepath.Clean(p)
	if why := fp.deniedReason(clean); why != "" {
		return "", fmt.Errorf("%s is %s", clean, why)
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	roots := fp.roots
	if roots == nil {
		roots = readRoots
	}
	allowed := false
	for _, root := range roots {
		if real == root || strings.HasPrefix(real, root+"/") {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("%s is outside the readable roots %v", real, roots)
	}
	if why := fp.deniedReason(real); why != "" {
		return "", fmt.Errorf("%s is %s", real, why)
	}
	return real, nil
}

// readFile returns at most max bytes of a regular text file; with tail it
// returns the end of the file instead of the beginning.
func (fp filePolicy) readFile(p string, max int, tail bool) (string, error) {
	real, err := fp.resolveReadable(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file (%s)", real, st.Mode().Type())
	}
	f, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer f.Close()

	truncated := false
	// Pseudo files in /proc and /sys report size 0, so only seek on real sizes.
	if tail && st.Size() > int64(max) {
		if _, err := f.Seek(st.Size()-int64(max), io.SeekStart); err != nil {
			return "", err
		}
		truncated = true
	}
	buf, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", err
	}
	if len(buf) > max {
		buf, truncated = buf[:max], true
	}
	if bytes.IndexByte(buf[:min(len(buf), 8192)], 0) >= 0 {
		return "", errors.New("file looks binary; refusing to return it")
	}
	if bytes.Contains(buf, privateKeyMarker) {
		return "", fmt.Errorf("%s contains a private key; refusing to return it", real)
	}
	if tail && truncated {
		// Drop the partial first line.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s (%d bytes, mode %s, modified %s)\n", real, st.Size(), st.Mode().Perm(), st.ModTime().Format("2006-01-02 15:04:05"))
	b.WriteString(strings.ToValidUTF8(string(buf), "\uFFFD"))
	if truncated {
		where := "beginning"
		if tail {
			where = "end"
		}
		fmt.Fprintf(&b, "\n[truncated: showing the %s of the file, %d bytes]\n", where, len(buf))
	}
	return b.String(), nil
}

// listDir lists a directory under the readable roots.
func (fp filePolicy) listDir(p string, limit int) (string, error) {
	real, err := fp.resolveReadable(p)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (%d entries)\n", real, len(entries))
	for i, e := range entries {
		if i >= limit {
			fmt.Fprintf(&b, "[truncated after %d entries]\n", limit)
			break
		}
		info, err := e.Info()
		if err != nil {
			fmt.Fprintf(&b, "?          %10s  %s\n", "?", e.Name())
			continue
		}
		name := e.Name()
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(filepath.Join(real, name)); err == nil {
				name += " -> " + target
			}
		} else if info.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, "%s %10d  %s  %s\n", info.Mode(), info.Size(), info.ModTime().Format("2006-01-02 15:04"), name)
	}
	return b.String(), nil
}
