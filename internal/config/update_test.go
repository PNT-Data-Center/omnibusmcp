package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# Top comment
listen: 192.0.2.10:8765

# TLS section comment
tls:
  cert_file: ""
  key_file: ""

allow_insecure_remote: true
`

func TestUpdateKeepsCommentsAndValidates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte(sample), 0o600)

	err := Update(p, map[string]any{"tls.cert_file": "/x/cert.pem", "tls.key_file": "/x/key.pem", "allow_insecure_remote": false})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{"# Top comment", "# TLS section comment", "cert_file: /x/cert.pem", "allow_insecure_remote: false"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	cfg, _, err := Load(p)
	if err != nil || !cfg.TLS.Enabled() || cfg.AllowInsecureRemote {
		t.Fatalf("%+v %v", cfg, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", st.Mode().Perm())
	}

	// Disabling TLS on a network listener without allow_insecure_remote is invalid.
	if err := Update(p, map[string]any{"tls.cert_file": "", "tls.key_file": ""}); err == nil {
		t.Fatal("wrote an invalid config")
	}
	if after, _ := os.ReadFile(p); string(after) != s {
		t.Fatal("file changed despite validation error")
	}
	if err := Update(p, map[string]any{"tls.cert_file": "", "tls.key_file": "", "allow_insecure_remote": true}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = Load(p)
	if cfg.TLS.Enabled() || !cfg.AllowInsecureRemote {
		t.Fatalf("%+v", cfg)
	}
}

func TestUpdateAddsMissingKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("tier: 1\n"), 0o600)
	if err := Update(p, map[string]any{"tls.cert_file": "/c", "tls.key_file": "/k"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(p)
	if err != nil || cfg.TLS.CertFile != "/c" || cfg.TLS.KeyFile != "/k" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestUpdateKeepsSectionSpacing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte(sample), 0o600)
	if err := Update(p, map[string]any{"tls.cert_file": "/c", "tls.key_file": "/k"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "# Top comment\nlisten: 192.0.2.10:8765\n\n# TLS section comment\ntls:\n  cert_file: /c\n  key_file: /k\n\nallow_insecure_remote: true\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
