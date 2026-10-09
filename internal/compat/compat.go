// Package compat records which product versions OmnibusMCP was tested
// against and annotates others. Untested versions are never refused: a
// diagnostic tool is most useful exactly when something changed, so the
// verdict is informational only (OK with a note).
package compat

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Product identifies a versioned dependency of a module.
type Product string

const (
	Debian    Product = "debian"
	Ubuntu    Product = "ubuntu"
	AlmaLinux Product = "almalinux"
	PVE       Product = "pve"
	PBS       Product = "pbs"
	Docker    Product = "docker"
	Ceph      Product = "ceph"
)

// names are human-readable product names.
var names = map[Product]string{
	Debian: "Debian", Ubuntu: "Ubuntu", AlmaLinux: "AlmaLinux",
	PVE: "Proxmox VE", PBS: "Proxmox Backup Server", Docker: "Docker Engine", Ceph: "Ceph",
}

// Tested lists "major.minor" versions with an e2e test report in
// test-reports/. Add a version only after running the e2e suites on it.
var Tested = map[Product][]string{
	Debian: {"13"},
	PVE:    {"9.2"},
	PBS:    {"4.0"},
	Docker: {"29.8"},
	Ceph:   {"18.2"},
}

// Name returns the display name of p.
func Name(p Product) string {
	if n, ok := names[p]; ok {
		return n
	}
	return string(p)
}

// Result is the compatibility verdict for one product version.
type Result struct {
	Product Product `json:"product"`
	Version string  `json:"version"`
	Tested  bool    `json:"tested"`
	Note    string  `json:"note"`
}

// String renders e.g. "Proxmox VE 9.2.2 (tested)".
func (r Result) String() string {
	return fmt.Sprintf("%s %s (%s)", Name(r.Product), r.Version, r.Note)
}

// Assess compares version with the tested list of p.
func Assess(p Product, version string) Result {
	r := Result{Product: p, Version: version}
	tested := Tested[p]
	if version == "" {
		r.Note = "version unknown"
		return r
	}
	for _, t := range tested {
		if versionMatches(version, t) {
			r.Tested, r.Note = true, "tested"
			return r
		}
	}
	switch {
	case len(tested) == 0:
		r.Note = "not tested with OmnibusMCP yet"
	case sameMajor(version, tested):
		r.Note = fmt.Sprintf("this minor version is not tested yet (tested: %s); same major version, expected to work", strings.Join(tested, ", "))
	default:
		r.Note = fmt.Sprintf("this major version is not tested (tested: %s); results may be incomplete if the API changed", strings.Join(tested, ", "))
	}
	return r
}

// versionMatches reports whether version is t or starts with t + ".".
func versionMatches(version, t string) bool {
	return version == t || strings.HasPrefix(version, t+".") || strings.HasPrefix(version, t+"-")
}

func sameMajor(version string, tested []string) bool {
	major, _, _ := strings.Cut(version, ".")
	for _, t := range tested {
		if tm, _, _ := strings.Cut(t, "."); tm == major {
			return true
		}
	}
	return false
}

// DpkgStatusFile is the dpkg database read by PackageVersion.
var DpkgStatusFile = "/var/lib/dpkg/status"

// PackageVersion returns the installed version of a Debian package from
// the dpkg database (no process is started), or "" if not installed.
func PackageVersion(pkg string) string {
	f, err := os.Open(DpkgStatusFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	return packageVersion(f, pkg)
}

func packageVersion(r io.Reader, pkg string) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var name, status, version string
	flush := func() string {
		if name == pkg && strings.HasSuffix(status, " installed") {
			return version
		}
		return ""
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if v := flush(); v != "" {
				return v
			}
			name, status, version = "", "", ""
			continue
		}
		switch {
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimPrefix(line, "Package: ")
		case strings.HasPrefix(line, "Status: "):
			status = strings.TrimPrefix(line, "Status: ")
		case strings.HasPrefix(line, "Version: "):
			version = strings.TrimPrefix(line, "Version: ")
		}
	}
	return flush()
}

// OSRelease returns the distribution as a product and its VERSION_ID from
// /etc/os-release (e.g. debian, "13").
func OSRelease() (Product, string) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", ""
	}
	return parseOSRelease(string(data))
}

func parseOSRelease(data string) (Product, string) {
	var id, ver string
	for _, l := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			ver = v
		}
	}
	return Product(strings.ToLower(id)), ver
}
