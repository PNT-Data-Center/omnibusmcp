package install

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
)

func testPaths(t *testing.T) Paths {
	d := t.TempDir()
	return Paths{
		Bin:        filepath.Join(d, "bin", "omnibusmcp"),
		Unit:       filepath.Join(d, "omnibusmcp.service"),
		RenewUnit:  filepath.Join(d, "omnibusmcp-tls-renew.service"),
		RenewTimer: filepath.Join(d, "omnibusmcp-tls-renew.timer"),
		ConfigDir:  filepath.Join(d, "etc"),
		LogDir:     filepath.Join(d, "log"),
	}
}

func TestFilesIdempotent(t *testing.T) {
	p := testPaths(t)
	var out bytes.Buffer
	o := Options{Tier: 2, Modules: []string{"linux"}, Out: &out}
	if err := Files(p, o); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := config.Load(filepath.Join(p.ConfigDir, "config.yaml"))
	if err != nil || !found {
		t.Fatalf("rendered config does not load: %v", err)
	}
	if cfg.Tier != 2 || cfg.AutoModules() || cfg.TokenFile != filepath.Join(p.ConfigDir, "token") {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	tok1, _ := os.ReadFile(cfg.TokenFile)
	if len(strings.TrimSpace(string(tok1))) != 64 {
		t.Fatalf("bad token length")
	}
	if st, _ := os.Stat(cfg.TokenFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", st.Mode().Perm())
	}

	// Second run keeps the token and, without --force, the config.
	o.Tier = 1
	if err := Files(p, o); err != nil {
		t.Fatal(err)
	}
	tok2, _ := os.ReadFile(cfg.TokenFile)
	cfg2, _, _ := config.Load(filepath.Join(p.ConfigDir, "config.yaml"))
	if !bytes.Equal(tok1, tok2) || cfg2.Tier != 2 {
		t.Fatal("reinstall without --force changed token or config")
	}
	o.Force = true
	if err := Files(p, o); err != nil {
		t.Fatal(err)
	}
	cfg3, _, _ := config.Load(filepath.Join(p.ConfigDir, "config.yaml"))
	if cfg3.Tier != 1 {
		t.Fatal("--force did not rewrite config")
	}
}

func TestFilesValidatesBeforeWriting(t *testing.T) {
	p := testPaths(t)
	for _, o := range []Options{{Tier: 9}, {Listen: "0.0.0.0:8765"}, {Listen: "nope"}} {
		o.Out = &bytes.Buffer{}
		if err := Files(p, o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
		for _, f := range []string{p.Bin, p.Unit, p.ConfigDir} {
			if _, err := os.Stat(f); err == nil {
				t.Fatalf("%s created despite invalid options %+v", f, o)
			}
		}
	}
}

func TestRenderConfigListenAddresses(t *testing.T) {
	for _, l := range []string{"127.0.0.1:9000", "[::1]:8765", "localhost:8765"} {
		data, err := RenderConfig(Options{Listen: l}, testPaths(t))
		if err != nil || !strings.Contains(string(data), "\nlisten: "+l+"\n") {
			t.Fatalf("%s: %v", l, err)
		}
	}
}

func TestRemoveReport(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	os.WriteFile(f, nil, 0o600)
	var out bytes.Buffer
	removeReport(&out, f, os.Remove)
	removeReport(&out, f, os.Remove)
	if got := out.String(); got != "removed: "+f+"\nabsent:  "+f+"\n" {
		t.Fatalf("%q", got)
	}
}

func TestRenderConfigRejectsInvalid(t *testing.T) {
	if _, err := RenderConfig(Options{Tier: 9}, testPaths(t)); err == nil {
		t.Fatal("expected tier error")
	}
	if _, err := RenderConfig(Options{Listen: "0.0.0.0:8765"}, testPaths(t)); err == nil {
		t.Fatal("expected insecure remote error")
	}
}

func TestRenewUnitsWritten(t *testing.T) {
	p := testPaths(t)
	if err := Files(p, Options{Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	svc, _ := os.ReadFile(p.RenewUnit)
	timer, _ := os.ReadFile(p.RenewTimer)
	if !strings.Contains(string(svc), "tls renew --config "+filepath.Join(p.ConfigDir, "config.yaml")+" --before 30d") ||
		!strings.Contains(string(svc), "ReadWritePaths="+p.ConfigDir+" "+p.LogDir) {
		t.Fatalf("renew service:\n%s", svc)
	}
	if !strings.Contains(string(timer), "OnCalendar=daily") || !strings.Contains(string(timer), "Persistent=true") {
		t.Fatalf("renew timer:\n%s", timer)
	}
}

func TestRenderUnit(t *testing.T) {
	u, err := RenderUnit(DefaultPaths)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/usr/local/bin/omnibusmcp serve --config /etc/omnibusmcp/config.yaml", "ProtectSystem=strict", "ReadWritePaths=/var/log/omnibusmcp", "RestartPreventExitStatus=78",
		"CapabilityBoundingSet=CAP_DAC_READ_SEARCH CAP_SYS_PTRACE CAP_NET_BIND_SERVICE", "SystemCallFilter=@system-service",
		"RestrictNamespaces=yes", "PrivateDevices=yes"} {
		if !strings.Contains(string(u), want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

// config.Update on the generated config must change only the edited lines.
func TestRenderedConfigSurvivesUpdate(t *testing.T) {
	p := testPaths(t)
	data, err := RenderConfig(Options{}, p)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(f, data, 0o600)
	if err := config.Update(f, map[string]any{"tls.cert_file": "/etc/omnibusmcp/tls/cert.pem", "tls.key_file": "/etc/omnibusmcp/tls/key.pem"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f)
	before, got := strings.Split(string(data), "\n"), strings.Split(string(after), "\n")
	if len(before) != len(got) {
		t.Fatalf("line count %d -> %d:\n%s", len(before), len(got), after)
	}
	var changed []string
	for i := range before {
		if before[i] != got[i] {
			changed = append(changed, got[i])
		}
	}
	if len(changed) != 2 || !strings.Contains(changed[0], "cert_file: /etc/omnibusmcp/tls/cert.pem") {
		t.Fatalf("changed lines: %q\n%s", changed, after)
	}
}

func TestParseUnitState(t *testing.T) {
	running := ParseUnitState("LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\nMainPID=4242\nActiveEnterTimestamp=Tue 2026-09-30 13:51:45 CEST\nInactiveEnterTimestamp=Tue 2026-09-30 13:51:44 CEST\nExecMainStatus=0\nResult=success\n")
	if !running.Installed || !running.Running() || running.MainPID != 4242 || running.Since != "Tue 2026-09-30 13:51:45 CEST" {
		t.Fatalf("%+v", running)
	}
	failed := ParseUnitState("LoadState=loaded\nUnitFileState=enabled\nActiveState=failed\nSubState=failed\nMainPID=0\nActiveEnterTimestamp=a\nInactiveEnterTimestamp=b\nExecMainStatus=78\nResult=exit-code\n")
	if failed.Running() || failed.ExitStatus != 78 || failed.Since != "b" || failed.Result != "exit-code" {
		t.Fatalf("%+v", failed)
	}
	if missing := ParseUnitState("LoadState=not-found\nActiveState=inactive\nMainPID=0\n"); missing.Installed {
		t.Fatalf("%+v", missing)
	}
}

func TestParseRunningVersion(t *testing.T) {
	j := "Started omnibusmcp.service\ntime=x level=INFO msg=\"OmnibusMCP started\" version=v0.0.9 listen=a\nStopped\ntime=y level=INFO msg=\"OmnibusMCP started\" version=v0.1.0 listen=a tls=true\n"
	if v := ParseRunningVersion(j); v != "v0.1.0" {
		t.Fatalf("%q", v)
	}
	if ParseRunningVersion("nothing") != "" {
		t.Fatal("version from empty log")
	}
}

func TestResolveEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	e := ResolveEndpoint(ln.Addr().String(), true)
	if !e.Listening || e.URL != "https://"+ln.Addr().String()+"/mcp" {
		t.Fatalf("%+v", e)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	w := ResolveEndpoint("0.0.0.0:"+port, false)
	if !w.Listening || w.URL != "http://<host-address>:"+port+"/mcp" || w.Probe != "127.0.0.1:"+port {
		t.Fatalf("%+v", w)
	}
	ln.Close()
	if d := ResolveEndpoint(ln.Addr().String(), false); d.Listening {
		t.Fatalf("closed port reported listening: %+v", d)
	}
	if v6 := ResolveEndpoint("[::1]:8765", true); v6.URL != "https://[::1]:8765/mcp" {
		t.Fatalf("%+v", v6)
	}
}
