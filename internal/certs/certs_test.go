package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testPaths(d string) Paths {
	dir := filepath.Join(d, "tls")
	return Paths{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem")}
}

// verifyWithCA checks the server certificate as a client trusting only the CA.
func verifyWithCA(t *testing.T, p Paths, host string) error {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(p.Cert, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(pair.Certificate) != 2 {
		t.Fatalf("certificate file holds %d certificates, want server + CA", len(pair.Certificate))
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	caPEM, err := ReadCA(p.CA)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestGenerateAndLoad(t *testing.T) {
	p := testPaths(t.TempDir())
	now := time.Now()
	gen, err := Generate(p, []string{"vm.example", "192.0.2.10", "::1"}, DefaultDays, now)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p.Key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode().Perm())
	}
	loaded, err := Load(p.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Fingerprint != gen.Fingerprint || len(loaded.Fingerprint) != 95 || loaded.Issuer != "OmnibusMCP CA vm.example" {
		t.Fatalf("loaded %+v vs %+v", loaded, gen)
	}
	left := loaded.Remaining(now)
	if left < (DefaultDays-1)*24*time.Hour || left > DefaultDays*24*time.Hour {
		t.Fatalf("validity %s", left)
	}
	if got := loaded.Hosts(); len(got) != 3 || got[0] != "vm.example" || got[1] != "192.0.2.10" {
		t.Fatalf("hosts %v", got)
	}
	ca, err := LoadCA(p)
	if err != nil || ca.NotAfter.Before(loaded.NotAfter) {
		t.Fatalf("CA %+v %v", ca, err)
	}
	if kind, _, err := Classify(p); kind != Managed || err != nil {
		t.Fatalf("kind %v %v", kind, err)
	}
	for _, host := range []string{"vm.example", "192.0.2.10", "::1"} {
		if err := verifyWithCA(t, p, host); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
	if err := verifyWithCA(t, p, "other.example"); err == nil {
		t.Error("verified for a host not in SAN")
	}
}

// The CA key never reaches the disk: only the server certificate (with the
// CA in its chain), the server key and the CA certificate are written.
func TestNoCAKeyOnDisk(t *testing.T) {
	p := testPaths(t.TempDir())
	if _, err := Generate(p, []string{"vm.example"}, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p.Cert))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 3 || names[0] != "ca.pem" || names[1] != "cert.pem" || names[2] != "key.pem" {
		t.Fatalf("files %v", names)
	}
	data, _ := os.ReadFile(p.Key)
	block, _ := pem.Decode(data)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := parseFirst(p.CA)
	if key.PublicKey.Equal(ca.PublicKey) {
		t.Fatal("key.pem is the CA key")
	}
}

// Generating again creates a new CA: clients trusting the old one reject
// the new certificate and must trust the new CA.
func TestRegenerateNewCA(t *testing.T) {
	p := testPaths(t.TempDir())
	now := time.Now()
	if _, err := Generate(p, []string{"vm.example"}, 10, now); err != nil {
		t.Fatal(err)
	}
	oldCA, _ := ReadCA(p.CA)
	if _, err := Generate(p, []string{"vm.example"}, 10, now); err != nil {
		t.Fatal(err)
	}
	newCA, _ := ReadCA(p.CA)
	if string(oldCA) == string(newCA) {
		t.Fatal("CA not replaced")
	}
	pair, _ := tls.LoadX509KeyPair(p.Cert, p.Key)
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(oldCA)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "vm.example", Roots: pool}); err == nil {
		t.Fatal("old CA still verifies the new certificate")
	}
	if err := verifyWithCA(t, p, "vm.example"); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyLegacyAndExternal(t *testing.T) {
	d := t.TempDir()
	p := testPaths(d)
	write := func(org string, isCA bool) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: serialNumber(), Subject: pkix.Name{CommonName: "h", Organization: []string{org}},
			NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: isCA, BasicConstraintsValid: true}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		os.MkdirAll(filepath.Dir(p.Cert), 0o755)
		os.WriteFile(p.Cert, pemCert(der), 0o644)
	}
	write("OmnibusMCP self-signed", false)
	if kind, _, _ := Classify(p); kind != Legacy {
		t.Errorf("legacy: %v", kind)
	}
	write("Example Corp", false)
	if kind, _, _ := Classify(p); kind != External {
		t.Errorf("external: %v", kind)
	}
	// ReadCA refuses a CA that OmnibusMCP did not create.
	os.Rename(p.Cert, p.CA)
	if _, err := ReadCA(p.CA); err == nil {
		t.Error("served a foreign certificate as the CA")
	}
}

func TestGenerateRejects(t *testing.T) {
	p := testPaths(t.TempDir())
	if _, err := Generate(p, nil, 10, time.Now()); err == nil {
		t.Error("no hosts accepted")
	}
	if _, err := Generate(p, []string{"a"}, 0, time.Now()); err == nil {
		t.Error("zero days accepted")
	}
}

func TestParseHosts(t *testing.T) {
	got, err := ParseHosts(" vm.example, 10.0.0.1 ,,::1")
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "a b", "-x.example", "x..example", "evil;rm"} {
		if _, err := ParseHosts(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseDays(t *testing.T) {
	cases := map[string]time.Duration{"30": 30 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "720h": 720 * time.Hour, "0": 0}
	for in, want := range cases {
		if got, err := ParseDays(in); err != nil || got != want {
			t.Errorf("%q -> %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "-1d", "3w"} {
		if _, err := ParseDays(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDetectHosts(t *testing.T) {
	h := DetectHosts("192.0.2.10:8765")
	has := map[string]bool{}
	for _, x := range h {
		has[x] = true
	}
	if !has["localhost"] || !has["127.0.0.1"] || !has["192.0.2.10"] {
		t.Fatalf("%v", h)
	}
	for _, x := range DetectHosts("0.0.0.0:8765") {
		if x == "0.0.0.0" {
			t.Fatal("wildcard listen address used as SAN")
		}
	}
}
