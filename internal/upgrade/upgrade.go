// Package upgrade finds, downloads and verifies OmnibusMCP release binaries
// published on GitHub, and replaces the installed binary with one of them.
package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultRepo is the GitHub repository releases are downloaded from.
const DefaultRepo = "PNT-Data-Center/omnibusmcp"

// LockPath serializes concurrent upgrades on one host.
const LockPath = "/run/lock/omnibusmcp-upgrade.lock"

// maxDownload caps a downloaded file; a release binary is a few dozen MB.
const maxDownload = 256 << 20

// Source is a place releases are published, laid out like GitHub releases:
// Base/latest redirects to Base/tag/<tag>, files are at Base/download/<tag>/<name>.
type Source struct {
	Base   string
	Client *http.Client
}

// DefaultSource returns the GitHub releases of DefaultRepo. The environment
// variables OMNIBUSMCP_REPO and OMNIBUSMCP_BASE_URL override it, as in install.sh.
func DefaultSource() Source {
	repo := os.Getenv("OMNIBUSMCP_REPO")
	if repo == "" {
		repo = DefaultRepo
	}
	base := os.Getenv("OMNIBUSMCP_BASE_URL")
	if base == "" {
		base = "https://github.com/" + repo + "/releases"
	}
	return Source{Base: strings.TrimRight(base, "/"), Client: &http.Client{Timeout: 5 * time.Minute}}
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// Latest returns the tag of the latest release, read from the redirect of
// Base/latest. It needs no API call, so neither a token nor API rate limits.
func (s Source) Latest(ctx context.Context) (string, error) {
	c := *s.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("latest release: unexpected HTTP %d from %s", resp.StatusCode, req.URL)
	}
	return TagFromLocation(resp.Header.Get("Location"))
}

// TagFromLocation extracts the tag from a ".../releases/tag/<tag>" URL.
func TagFromLocation(loc string) (string, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	dir, tag := path.Split(strings.TrimRight(u.Path, "/"))
	if path.Base(strings.TrimRight(dir, "/")) != "tag" || !ValidTag(tag) {
		return "", fmt.Errorf("latest release: cannot read the tag from redirect %q", loc)
	}
	return tag, nil
}

// Download fetches one file of release tag.
func (s Source) Download(ctx context.Context, tag, name string) ([]byte, error) {
	u := s.Base + "/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d (does release %s exist?)", u, resp.StatusCode, tag)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %d bytes", u, maxDownload)
	}
	return data, nil
}

// AssetName is the release file for this machine.
func AssetName() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return "omnibusmcp-linux-" + runtime.GOARCH, nil
	}
	return "", fmt.Errorf("unsupported architecture %s (supported: amd64, arm64)", runtime.GOARCH)
}

// VerifySum checks data against the entry for name in a SHA256SUMS file.
func VerifySum(sums []byte, name string, data []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		got := sha256.Sum256(data)
		if !strings.EqualFold(f[0], hex.EncodeToString(got[:])) {
			return fmt.Errorf("checksum of %s does not match SHA256SUMS: download corrupted", name)
		}
		return nil
	}
	return fmt.Errorf("SHA256SUMS has no entry for %s", name)
}

var (
	tagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:[-.]([0-9A-Za-z.-]+))?$`)
	// pseudoRe matches Go pseudo-versions (v0.4.2-0.20261008060622-16dbbe61d287):
	// builds of an untagged commit, not releases.
	pseudoRe = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)
)

// ValidTag reports whether s is a release tag such as v0.4.1.
func ValidTag(s string) bool { return tagRe.MatchString(s) && !pseudoRe.MatchString(s) }

// Compare orders release tags like semver: -1 if a < b, 0 if equal, 1 if
// a > b. A pre-release sorts before its release. ok is false when either
// is not a release tag (for example a "dev+abc1234" build).
func Compare(a, b string) (c int, ok bool) {
	if !ValidTag(a) || !ValidTag(b) {
		return 0, false
	}
	ma, mb := tagRe.FindStringSubmatch(a), tagRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0, false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return sign(x - y), true
		}
	}
	switch pa, pb := ma[4], mb[4]; {
	case pa == pb:
		return 0, true
	case pa == "":
		return 1, true
	case pb == "":
		return -1, true
	default:
		return sign(strings.Compare(pa, pb)), true
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Lock takes an exclusive lock on path so two upgrades never overlap.
// The lock is released by calling the returned function or on exit.
func Lock(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another upgrade is already running")
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// PrevPath is where Replace keeps the previous binary.
func PrevPath(target string) string { return target + ".prev" }

// Stage writes data next to target as an executable temp file and checks
// that it runs and reports want as its version. The caller removes the file
// if it does not pass it to Replace.
func Stage(target string, data []byte, want string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".new-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	out, err := exec.Command(name, "version").Output()
	if err != nil {
		os.Remove(name)
		return "", fmt.Errorf("the downloaded binary does not run on this machine: %v", err)
	}
	if f := strings.Fields(firstLine(out)); len(f) < 2 || f[1] != want {
		os.Remove(name)
		return "", fmt.Errorf("the downloaded binary reports %q, expected %s", strings.TrimSpace(firstLine(out)), want)
	}
	return name, nil
}

// Replace keeps a copy of target in PrevPath(target) and moves staged over
// target. The rename is atomic: a running service keeps its old binary
// until it is restarted.
func Replace(target, staged string) error {
	if err := copyFile(target, PrevPath(target)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keep previous binary: %w", err)
	}
	return os.Rename(staged, target)
}

// Restore puts the binary kept by Replace back in place.
func Restore(target string) error {
	prev := PrevPath(target)
	tmp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".restore")
	if err := copyFile(prev, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}
