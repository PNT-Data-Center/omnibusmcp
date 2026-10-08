package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.4.1", "v0.4.1", 0, true},
		{"v0.4.1", "v0.5.0", -1, true},
		{"v0.10.0", "v0.9.9", 1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.5.0-rc1", "v0.5.0", -1, true},
		{"v0.5.0-rc2", "v0.5.0-rc1", 1, true},
		{"dev+abc1234", "v0.4.1", 0, false},
		{"v0.4.1", "dev", 0, false},
		{"0.4.1", "v0.4.1", 0, false},
		{"v0.4.2-0.20261008060622-16dbbe61d287", "v0.4.1", 0, false},
		{"v0.4.2-0.20261008060622-16dbbe61d287+dirty", "v0.4.1", 0, false},
	}
	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestTagFromLocation(t *testing.T) {
	for loc, want := range map[string]string{
		"https://github.com/example/omnibusmcp/releases/tag/v0.4.1":  "v0.4.1",
		"https://github.com/example/omnibusmcp/releases/tag/v1.2.3/": "v1.2.3",
		"/releases/tag/v0.5.0-rc1":                                   "v0.5.0-rc1",
	} {
		if got, err := TagFromLocation(loc); err != nil || got != want {
			t.Errorf("TagFromLocation(%q) = %q, %v; want %q", loc, got, err, want)
		}
	}
	for _, loc := range []string{"", "https://github.com/example/omnibusmcp/releases", "https://github.com/example/omnibusmcp/releases/tag/latest", "https://example.com/v0.4.1"} {
		if got, err := TagFromLocation(loc); err == nil {
			t.Errorf("TagFromLocation(%q) = %q, want error", loc, got)
		}
	}
}

func sumLine(name string, data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]) + "  " + name + "\n"
}

func TestVerifySum(t *testing.T) {
	bin := []byte("binary")
	sums := []byte(sumLine("omnibusmcp-linux-arm64", []byte("other")) + sumLine("omnibusmcp-linux-amd64", bin))
	if err := VerifySum(sums, "omnibusmcp-linux-amd64", bin); err != nil {
		t.Fatal(err)
	}
	if err := VerifySum(sums, "omnibusmcp-linux-amd64", []byte("tampered")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered binary: %v", err)
	}
	if err := VerifySum(sums, "omnibusmcp-linux-riscv64", bin); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("missing entry: %v", err)
	}
}

// fakeReleases serves a GitHub-like release layout with one release.
func fakeReleases(t *testing.T, tag string, files map[string][]byte) Source {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.PathValue("name")]
		if r.PathValue("tag") != tag || !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return Source{Base: srv.URL + "/releases", Client: srv.Client()}
}

func TestSourceLatestAndDownload(t *testing.T) {
	src := fakeReleases(t, "v0.5.0", map[string][]byte{"SHA256SUMS": []byte("sums")})
	ctx := context.Background()
	tag, err := src.Latest(ctx)
	if err != nil || tag != "v0.5.0" {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
	data, err := src.Download(ctx, tag, "SHA256SUMS")
	if err != nil || string(data) != "sums" {
		t.Fatalf("Download = %q, %v", data, err)
	}
	if _, err := src.Download(ctx, "v0.1.0", "SHA256SUMS"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing release: %v", err)
	}
}

func TestLatestWithoutRedirect(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	src := Source{Base: srv.URL, Client: srv.Client()}
	if _, err := src.Latest(context.Background()); err == nil {
		t.Fatal("expected an error without a redirect")
	}
}

// script returns a shell script that prints "omnibusmcp <ver>" for "version".
func script(ver string) []byte {
	return []byte("#!/bin/sh\necho omnibusmcp " + ver + "\n")
}

func TestStageReplaceRestore(t *testing.T) {
	target := filepath.Join(t.TempDir(), "omnibusmcp")
	if err := os.WriteFile(target, script("v0.4.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Stage(target, script("v0.4.10"), "v0.4.1"); err == nil {
		t.Fatal("Stage accepted a binary reporting a different version")
	}
	if _, err := Stage(target, []byte("not a program"), "v0.5.0"); err == nil {
		t.Fatal("Stage accepted a binary that does not run")
	}
	if left, _ := filepath.Glob(target + "*"); len(left) != 1 {
		t.Fatalf("failed stages left files behind: %v", left)
	}

	staged, err := Stage(target, script("v0.5.0"), "v0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, staged); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(script("v0.5.0")) {
		t.Fatalf("target after Replace: %q", got)
	}
	if got, _ := os.ReadFile(PrevPath(target)); string(got) != string(script("v0.4.1")) {
		t.Fatalf("previous binary: %q", got)
	}

	if err := Restore(target); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(script("v0.4.1")) {
		t.Fatalf("target after Restore: %q", got)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o755 {
		t.Fatalf("restored mode %v", st.Mode().Perm())
	}
}

func TestReplaceWithoutPrevious(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "omnibusmcp")
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, script("v0.5.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, staged); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PrevPath(target)); !os.IsNotExist(err) {
		t.Fatalf("unexpected previous binary: %v", err)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock2, err := Lock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}
