package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
)

func TestJournalArgs(t *testing.T) {
	boot := -1
	args, err := journalArgs(journalInput{Unit: "ssh.service", Since: "30m", Priority: "err", Grep: "fail|error", Lines: 5000, Boot: &boot})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{"--unit=ssh.service", "--since=-30min", "--priority=err", "--grep=fail|error", "--lines=2000", "--boot=-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	bad := []journalInput{
		{Unit: "-x"},
		{Unit: "a b"},
		{Since: "1h; rm -rf /"},
		{Since: "--all"},
		{Priority: "loud"},
		{Grep: strings.Repeat("a", 201)},
	}
	for _, in := range bad {
		if _, err := journalArgs(in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	b := 3
	if _, err := journalArgs(journalInput{Boot: &b}); err == nil {
		t.Error("accepted positive boot")
	}
}

func TestJournalTime(t *testing.T) {
	ok := map[string]string{"2h": "-2h", "15m": "-15min", "1d": "-1d", "today": "today", "2026-09-28 10:00": "2026-09-28 10:00"}
	for in, want := range ok {
		if got, err := journalTime(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
}

func TestResolveReadable(t *testing.T) {
	resolveReadable := filePolicy{}.resolveReadable
	for _, p := range []string{"/etc/hostname", "/proc/loadavg", "/etc/../etc/os-release"} {
		if _, err := resolveReadable(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"/etc/shadow", "/root/.bashrc", "/home", "relative/path", "/etc/../root"} {
		if _, err := resolveReadable(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestReadFileTail(t *testing.T) {
	out, err := filePolicy{}.readFile("/proc/self/status", 1024, false)
	if err != nil || !strings.Contains(out, "Name:") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestClamp(t *testing.T) {
	if clamp(0, 10, 50) != 10 || clamp(-5, 10, 50) != 1 || clamp(99, 10, 50) != 50 || clamp(7, 10, 50) != 7 {
		t.Fatal("clamp")
	}
}

func TestFilePolicyDeniesOwnSecrets(t *testing.T) {
	dir := t.TempDir()
	// Only paths under the readable roots matter; use /proc/self to stand in
	// for a readable file and check the configured token is refused.
	cfg := config.Default()
	cfg.TokenFile = "/proc/self/status"
	cfg.TLS.KeyFile = dir + "/key.pem"
	fp := newFilePolicy(cfg)
	if _, err := fp.readFile("/proc/self/status", 1024, false); err == nil || !strings.Contains(err.Error(), "by policy") {
		t.Fatalf("own token readable: %v", err)
	}
	if _, err := fp.readFile("/proc/self/limits", 1024, false); err != nil {
		t.Fatalf("unrelated file refused: %v", err)
	}
}

func TestDeniedSecrets(t *testing.T) {
	fp := filePolicy{}
	for _, p := range []string{
		"/etc/pve/priv", "/etc/pve/priv/authkey.key", "/etc/pve/priv/token.cfg", "/etc/pve/priv/shadow.cfg",
		"/etc/pve/nodes/ai/pve-ssl.key", "/etc/pve/nodes/ai/pveproxy-ssl.key",
		"/etc/ceph/ceph.client.admin.keyring", "/etc/ssh/ssh_host_ed25519_key", "/etc/ssl/private/server.pem",
		"/etc/corosync/authkey", "/etc/wireguard/wg0.conf", "/etc/shadow",
	} {
		if _, err := fp.resolveReadable(p); err == nil || !strings.Contains(err.Error(), "by policy") {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"/etc/ssh/ssh_host_ed25519_key.pub", "/etc/pve/storage.cfg", "/etc/pve/nodes/ai/pve-ssl.pem", "/etc/keyboard", "/proc/keys"} {
		if why := fp.deniedReason(p); why != "" {
			t.Errorf("%s over-denied: %s", p, why)
		}
	}
}

func TestSymlinkIntoDeniedFile(t *testing.T) {
	d := t.TempDir()
	secret := filepath.Join(d, "secret.cfg")
	os.WriteFile(secret, []byte("token=abc\n"), 0o600)
	link := filepath.Join(d, "innocent.conf")
	os.Symlink(secret, link)
	fp := filePolicy{roots: []string{d}, denied: []string{secret}}
	if _, err := fp.readFile(link, 1024, false); err == nil || !strings.Contains(err.Error(), "by policy") {
		t.Fatalf("symlink bypassed the policy: %v", err)
	}
}

func TestPrivateKeyContentRefused(t *testing.T) {
	d := t.TempDir()
	fp := filePolicy{roots: []string{d}}
	for name, body := range map[string]string{
		"a.pem":  "-----BEGIN EC PRIVATE KEY-----\nMHc...\n-----END EC PRIVATE KEY-----\n", // gitleaks:allow (fake test data)
		"b.conf": "# bundle\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl\n",
		"c.pem":  "-----BEGIN PRIVATE KEY-----\nMIIE\n",
	} {
		f := filepath.Join(d, name)
		os.WriteFile(f, []byte(body), 0o600)
		if _, err := fp.readFile(f, 1024, false); err == nil || !strings.Contains(err.Error(), "private key") {
			t.Errorf("%s: %v", name, err)
		}
	}
	cert := filepath.Join(d, "cert.pem")
	os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644)
	if _, err := fp.readFile(cert, 1024, false); err != nil {
		t.Fatalf("certificate refused: %v", err)
	}
}

func TestFormatUptime(t *testing.T) {
	cases := map[time.Duration]string{
		1147*time.Hour + 47*time.Minute: "47d 19h 47m",
		5*time.Hour + 3*time.Minute:     "5h 3m",
		12 * time.Minute:                "12m",
	}
	for d, want := range cases {
		if got := formatUptime(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}

func TestTimeService(t *testing.T) {
	cases := map[string]string{
		"active\nactive\ninactive\ninactive\ninactive\ninactive\ninactive\n":     "chrony",            // Debian: chrony + chronyd alias
		"inactive\nactive\ninactive\ninactive\ninactive\ninactive\ninactive\n":   "chrony",            // RHEL: chronyd
		"inactive\ninactive\nactive\ninactive\ninactive\ninactive\ninactive\n":   "systemd-timesyncd", // Debian default
		"inactive\ninactive\ninactive\ninactive\ninactive\nactive\ninactive\n":   "ntpd",
		"inactive\ninactive\ninactive\ninactive\ninactive\ninactive\ninactive\n": "no known NTP service active",
		"": "no known NTP service active",
	}
	for in, want := range cases {
		if got := timeService(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
