// Package config loads and validates the OmnibusMCP configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Default filesystem locations.
const (
	DefaultPath      = "/etc/omnibusmcp/config.yaml"
	DefaultTokenFile = "/etc/omnibusmcp/token"
	DefaultAuditLog  = "/var/log/omnibusmcp/audit.log"
	DefaultListen    = "127.0.0.1:8765"
	// ModulesAuto enables every module whose presence is detected on the host.
	ModulesAuto = "auto"
)

// Config is the top-level configuration.
type Config struct {
	// Listen is the host:port of the HTTP endpoint.
	Listen string `yaml:"listen"`
	// Tier is the permission level (see package tier).
	Tier tier.Tier `yaml:"tier"`
	// Modules lists enabled modules, or ["auto"] for detection.
	// The base "linux" module is always enabled.
	Modules []string `yaml:"modules"`
	// TokenFile holds the Bearer token clients must present.
	TokenFile string `yaml:"token_file"`
	TLS       TLS    `yaml:"tls"`
	// AllowInsecureRemote permits a non-loopback listener without TLS.
	AllowInsecureRemote bool `yaml:"allow_insecure_remote"`
	// AuditLog is the JSON-lines audit file; "-" means stderr.
	AuditLog string `yaml:"audit_log"`
	Limits   Limits `yaml:"limits"`
	Ceph     Ceph   `yaml:"ceph"`
}

// Ceph configures the ceph module. Empty paths mean the default locations.
type Ceph struct {
	// Conf is the cluster configuration (default: /etc/ceph/ceph.conf,
	// then the config of a cephadm daemon on this host).
	Conf string `yaml:"conf"`
	// Keyring holds the read-only client.omnibusmcp key (default:
	// /etc/omnibusmcp/ceph.client.omnibusmcp.keyring, then
	// /etc/pve/priv/ceph.client.omnibusmcp.keyring).
	Keyring string `yaml:"keyring"`
	// Cluster is "auto" (query the cluster when a key is available) or
	// "false" (only this host's view: daemons, crashes, logs).
	Cluster string `yaml:"cluster"`
}

// Ceph.Cluster values.
const (
	CephClusterAuto = "auto"
	CephClusterOff  = "false"
)

// ClusterView reports whether the ceph module may query the cluster.
func (c Ceph) ClusterView() bool { return c.Cluster != CephClusterOff }

// TLS enables HTTPS when both files are set.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Enabled reports whether TLS is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// CAFile locates the certificate of the server's single-use CA, kept next
// to the server certificate (written by "omnibusmcp tls generate").
func (t TLS) CAFile() string { return filepath.Join(filepath.Dir(t.CertFile), "ca.pem") }

// Limits bound the cost of every command.
type Limits struct {
	CommandTimeout time.Duration `yaml:"command_timeout"`
	HealthTimeout  time.Duration `yaml:"health_timeout"`
	MaxOutputBytes int           `yaml:"max_output_bytes"`
}

// Default returns a configuration with safe defaults.
func Default() *Config {
	return &Config{
		Listen:    DefaultListen,
		Tier:      tier.ReadOnly,
		Modules:   []string{ModulesAuto},
		TokenFile: DefaultTokenFile,
		AuditLog:  DefaultAuditLog,
		Limits: Limits{
			CommandTimeout: 15 * time.Second,
			HealthTimeout:  30 * time.Second,
			MaxOutputBytes: 64 * 1024,
		},
		Ceph: Ceph{Cluster: CephClusterAuto},
	}
}

// Load reads path on top of Default. A missing file yields the defaults and
// found=false, so the caller can warn.
func Load(path string) (cfg *Config, found bool, err error) {
	cfg = Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, cfg.Validate()
	}
	if err != nil {
		return nil, false, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, true, nil
}

// Validate checks values and cross-field constraints.
func (c *Config) Validate() error {
	if !c.Tier.Valid() {
		return fmt.Errorf("tier %d out of range %d..%d", int(c.Tier), int(tier.Min), int(tier.Max))
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("tls: cert_file and key_file must be set together")
	}
	if !c.ListenIsLoopback() && !c.TLS.Enabled() && !c.AllowInsecureRemote {
		return fmt.Errorf("listen %q is not loopback: configure tls or set allow_insecure_remote", c.Listen)
	}
	if c.TokenFile == "" {
		return errors.New("token_file must be set")
	}
	if len(c.Modules) == 0 {
		c.Modules = []string{ModulesAuto}
	}
	if c.Limits.CommandTimeout <= 0 || c.Limits.HealthTimeout <= 0 {
		return errors.New("limits: timeouts must be positive")
	}
	if c.Limits.MaxOutputBytes < 1024 {
		return errors.New("limits: max_output_bytes must be >= 1024")
	}
	switch c.Ceph.Cluster {
	case "":
		c.Ceph.Cluster = CephClusterAuto
	case CephClusterAuto, CephClusterOff:
	default:
		return fmt.Errorf("ceph.cluster %q: expected auto or false", c.Ceph.Cluster)
	}
	for name, p := range map[string]string{"ceph.conf": c.Ceph.Conf, "ceph.keyring": c.Ceph.Keyring} {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("%s %q: must be an absolute path", name, p)
		}
	}
	return nil
}

// ListenIsLoopback reports whether Listen binds only to a loopback address.
func (c *Config) ListenIsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// AutoModules reports whether modules should be detected.
func (c *Config) AutoModules() bool {
	for _, m := range c.Modules {
		if m == ModulesAuto {
			return true
		}
	}
	return false
}
