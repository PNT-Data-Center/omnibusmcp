package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, found, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg.Listen != DefaultListen || cfg.Tier != 1 || !cfg.AutoModules() {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("tier: 2\nmodules: [linux]\nlimits:\n  command_timeout: 5s\n"), 0o600)
	cfg, found, err := Load(p)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cfg.Tier != 2 || cfg.AutoModules() || cfg.Limits.CommandTimeout != 5*time.Second {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.Limits.MaxOutputBytes != Default().Limits.MaxOutputBytes {
		t.Fatal("unset limit lost its default")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"tier0":           func(c *Config) { c.Tier = 0 },
		"tier99":          func(c *Config) { c.Tier = 99 },
		"bad listen":      func(c *Config) { c.Listen = "nope" },
		"remote no tls":   func(c *Config) { c.Listen = "0.0.0.0:8765" },
		"half tls":        func(c *Config) { c.TLS.CertFile = "x" },
		"no token":        func(c *Config) { c.TokenFile = "" },
		"tiny output cap": func(c *Config) { c.Limits.MaxOutputBytes = 10 },
	}
	for name, mut := range cases {
		c := Default()
		mut(c)
		if c.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c := Default()
	c.Listen = "0.0.0.0:8765"
	c.AllowInsecureRemote = true
	if err := c.Validate(); err != nil {
		t.Errorf("insecure remote explicitly allowed: %v", err)
	}
}

func TestCephSection(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		"":                          CephClusterAuto,
		"ceph:\n  cluster: false\n": CephClusterOff,
		"ceph:\n  cluster: auto\n":  CephClusterAuto,
	} {
		p := filepath.Join(dir, "c.yaml")
		os.WriteFile(p, []byte(body), 0o600)
		cfg, _, err := Load(p)
		if err != nil || cfg.Ceph.Cluster != want || cfg.Ceph.ClusterView() != (want == CephClusterAuto) {
			t.Errorf("%q: %v %+v", body, err, cfg)
		}
	}
	for _, body := range []string{"ceph:\n  cluster: yes-please\n", "ceph:\n  keyring: relative/key\n", "ceph:\n  conf: ceph.conf\n"} {
		p := filepath.Join(dir, "c.yaml")
		os.WriteFile(p, []byte(body), 0o600)
		if _, _, err := Load(p); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
}
