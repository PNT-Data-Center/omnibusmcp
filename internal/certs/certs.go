// Package certs manages the TLS certificate of the OmnibusMCP HTTPS
// endpoint. A single-use local CA signs the server certificate and its key
// is discarded right away: clients trust the CA, and nobody, not even root
// on the server, can issue another certificate under it.
package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultDays is the default validity of the server certificate: 5 years.
// Every renewal brings a new CA that clients must trust again, so the
// certificate is long-lived; only its own key stays on the server.
const DefaultDays = 1826

// Info describes a certificate.
type Info struct {
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	DNSNames    []string
	IPs         []net.IP
	Fingerprint string // SHA-256 of the DER certificate, colon-separated hex
}

// Hosts returns the certificate's SANs (DNS names, then IPs) as strings.
func (i *Info) Hosts() []string {
	out := append([]string{}, i.DNSNames...)
	for _, ip := range i.IPs {
		out = append(out, ip.String())
	}
	return out
}

// Remaining is the time left until expiry (negative once expired).
func (i *Info) Remaining(now time.Time) time.Duration { return i.NotAfter.Sub(now) }

// DaysLeft is Remaining rounded to whole days.
func (i *Info) DaysLeft(now time.Time) int { return int(math.Round(i.Remaining(now).Hours() / 24)) }

// String renders the certificate for humans.
func (i *Info) String() string {
	days := i.DaysLeft(time.Now())
	return fmt.Sprintf("subject:     %s\nvalid from:  %s\nvalid until: %s (%d days left)\nhosts (SAN): %s\nsha256:      %s\n",
		i.Subject, i.NotBefore.Format(time.RFC3339), i.NotAfter.Format(time.RFC3339), days,
		strings.Join(i.Hosts(), ", "), i.Fingerprint)
}

// Organization marks certificates issued by OmnibusMCP; legacyOrganization
// marks the self-signed certificates of versions before 0.4.
const (
	Organization       = "OmnibusMCP"
	legacyOrganization = "OmnibusMCP self-signed"
)

// Kind classifies the configured server certificate.
type Kind int

const (
	// Managed: issued by the single-use OmnibusMCP CA stored next to it.
	Managed Kind = iota
	// Legacy: self-signed by an older OmnibusMCP; some clients (the native
	// Claude Code build) do not accept a self-signed leaf as a trust anchor.
	Legacy
	// External: issued by some other CA; OmnibusMCP never replaces it.
	External
)

// Paths locates the server certificate (with its chain), its key and the
// local CA certificate. There is no CA key: it never leaves memory.
type Paths struct{ Cert, Key, CA string }

// Generate creates a single-use CA, signs a server certificate for hosts
// with it and discards the CA key. Clients must trust the new CA.
func Generate(p Paths, hosts []string, days int, now time.Time) (*Info, error) {
	if days < 1 {
		return nil, errors.New("validity must be at least 1 day")
	}
	if len(hosts) == 0 {
		return nil, errors.New("at least one host name or IP is required")
	}
	notAfter := now.Add(time.Duration(days) * 24 * time.Hour)
	ca, caKey, err := newCA(hosts[0], now, notAfter)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p.CA), 0o755); err != nil {
		return nil, err
	}
	if err := writeAtomic(p.CA, pemCert(ca.Raw), 0o644); err != nil {
		return nil, err
	}
	// A CA key written by 0.4.0 pre-releases must not survive.
	if err := os.Remove(filepath.Join(filepath.Dir(p.CA), "ca.key")); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return issue(p, ca, caKey, hosts, notAfter, now)
}

// Classify reports what kind of certificate is configured and returns it.
func Classify(p Paths) (Kind, *x509.Certificate, error) {
	leaf, err := parseFirst(p.Cert)
	if err != nil {
		return External, nil, err
	}
	if len(leaf.Subject.Organization) > 0 && leaf.Subject.Organization[0] == legacyOrganization {
		return Legacy, leaf, nil
	}
	if ca, err := parseFirst(p.CA); err == nil && isOurs(ca) && leaf.CheckSignatureFrom(ca) == nil {
		return Managed, leaf, nil
	}
	return External, leaf, nil
}

// LoadCA describes the local CA certificate.
func LoadCA(p Paths) (*Info, error) { return Load(p.CA) }

// ReadCA returns the local CA certificate as PEM, for clients to download.
func ReadCA(path string) ([]byte, error) {
	ca, err := parseFirst(path)
	if err != nil {
		return nil, err
	}
	if !isOurs(ca) {
		return nil, errors.New("not an OmnibusMCP CA")
	}
	return pemCert(ca.Raw), nil
}

func isOurs(ca *x509.Certificate) bool {
	return ca.IsCA && len(ca.Subject.Organization) > 0 && ca.Subject.Organization[0] == Organization
}

// newCA creates the single-use CA. Its key is returned to sign one server
// certificate and is never written anywhere. No name constraints: without
// the key nothing else can be signed, and some TLS stacks (the native Claude
// Code build) reject IP address constraints.
func newCA(host string, now, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: "OmnibusMCP CA " + host, Organization: []string{Organization}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	return ca, key, err
}

// issue writes a new server key and a certificate signed by ca; the
// certificate file holds the chain (server certificate, then the CA).
func issue(p Paths, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string, notAfter, now time.Time) (*Info, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{Organization}},
		NotBefore:             now.Add(-5 * time.Minute), // tolerate small clock skew
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	// Key first: a certificate without its key would break the server.
	if err := writeKey(p.Key, key); err != nil {
		return nil, err
	}
	if err := writeAtomic(p.Cert, append(pemCert(der), pemCert(ca.Raw)...), 0o644); err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return info(cert), nil
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func serialNumber() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return n
}

// parseFirst parses the first PEM certificate of a file.
func parseFirst(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no PEM certificate found", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cert, nil
}

// Load reads and describes the first certificate of a PEM file.
func Load(certPath string) (*Info, error) {
	cert, err := parseFirst(certPath)
	if err != nil {
		return nil, err
	}
	return info(cert), nil
}

func info(c *x509.Certificate) *Info {
	sum := sha256.Sum256(c.Raw)
	hexParts := make([]string, len(sum))
	for i, b := range sum {
		hexParts[i] = fmt.Sprintf("%02X", b)
	}
	return &Info{
		Subject:     c.Subject.CommonName,
		Issuer:      c.Issuer.CommonName,
		NotBefore:   c.NotBefore,
		NotAfter:    c.NotAfter,
		DNSNames:    c.DNSNames,
		IPs:         c.IPAddresses,
		Fingerprint: strings.Join(hexParts, ":"),
	}
}

// DetectHosts proposes SANs: the hostname (and FQDN if resolvable), the
// listen address unless it is a wildcard, global unicast interface addresses
// and loopback (so an SSH tunnel keeps working).
func DetectHosts(listen string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.TrimSuffix(strings.TrimSpace(h), ".")
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if name, err := os.Hostname(); err == nil {
		add(name)
		// Bounded: a host with broken DNS must not stall the CLI.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		fqdn, err := net.DefaultResolver.LookupCNAME(ctx, name)
		cancel()
		if err == nil && strings.Contains(strings.TrimSuffix(fqdn, "."), ".") {
			add(fqdn)
		}
	}
	add("localhost")
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
			add(host)
		}
	}
	var ips []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
				ips = append(ips, n.IP.String())
			}
		}
	}
	sort.Strings(ips)
	for _, ip := range ips {
		add(ip)
	}
	add("127.0.0.1")
	add("::1")
	return out
}

// ParseHosts splits a comma-separated host list and validates each entry.
func ParseHosts(s string) ([]string, error) {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if net.ParseIP(h) == nil && !validDNSName(h) {
			return nil, fmt.Errorf("invalid host %q (expected a DNS name or IP address)", h)
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, errors.New("empty host list")
	}
	return out, nil
}

func validDNSName(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ParseDays accepts "30", "30d" or a Go duration such as "720h".
func ParseDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		s = n
	}
	var days int
	if _, err := fmt.Sscanf(s, "%d", &days); err == nil && fmt.Sprint(days) == s {
		if days < 0 {
			return 0, fmt.Errorf("negative duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30d or 720h)", s)
	}
	return d, nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
