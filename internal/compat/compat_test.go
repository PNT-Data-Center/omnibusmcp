package compat

import (
	"strings"
	"testing"
)

func TestAssess(t *testing.T) {
	Tested[PVE] = []string{"9.2"}
	cases := []struct {
		v      string
		tested bool
		note   string
	}{
		{"9.2", true, "tested"},
		{"9.2.2", true, "tested"},
		{"9.3.1", false, "minor version is not tested yet (tested: 9.2); same major version"},
		{"10.0.1", false, "major version is not tested (tested: 9.2)"},
		{"9.20.1", false, "minor version"}, // "9.2" must not match "9.20"
		{"", false, "version unknown"},
	}
	for _, c := range cases {
		r := Assess(PVE, c.v)
		if r.Tested != c.tested || !strings.Contains(r.Note, c.note) {
			t.Errorf("%q: %+v", c.v, r)
		}
	}
	if r := Assess("kubernetes", "1.31"); r.Tested || r.Note != "not tested with OmnibusMCP yet" {
		t.Fatalf("%+v", r)
	}
	if s := Assess(PBS, "4.0.11").String(); s != "Proxmox Backup Server 4.0.11 (tested)" {
		t.Fatalf("%q", s)
	}
	if !Assess(Docker, "29.8.1").Tested || !Assess(Debian, "13").Tested {
		t.Fatal("tested table")
	}
}

func TestPackageVersion(t *testing.T) {
	db := `Package: pve-manager
Status: install ok installed
Version: 9.2.2

Package: proxmox-backup-server
Status: deinstall ok config-files
Version: 3.4.1

Package: docker-ce
Status: install ok installed
Architecture: amd64
Version: 5:29.8.1-1~debian.13~trixie
`
	if v := packageVersion(strings.NewReader(db), "pve-manager"); v != "9.2.2" {
		t.Fatalf("%q", v)
	}
	if v := packageVersion(strings.NewReader(db), "proxmox-backup-server"); v != "" {
		t.Fatalf("removed package reported: %q", v)
	}
	if v := packageVersion(strings.NewReader(db), "docker-ce"); v != "5:29.8.1-1~debian.13~trixie" {
		t.Fatalf("last stanza: %q", v)
	}
	if packageVersion(strings.NewReader(db), "absent") != "" {
		t.Fatal("absent package")
	}
}

func TestParseOSRelease(t *testing.T) {
	p, v := parseOSRelease("PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n")
	if p != Debian || v != "13" {
		t.Fatalf("%s %s", p, v)
	}
	p, v = parseOSRelease("ID=\"almalinux\"\nVERSION_ID=\"9.6\"\n")
	if p != AlmaLinux || v != "9.6" {
		t.Fatalf("%s %s", p, v)
	}
}

func TestCephTested(t *testing.T) {
	if !Assess(Ceph, "18.2.1").Tested || Assess(Ceph, "19.2.3").Tested {
		t.Fatal("Ceph tested table")
	}
}
