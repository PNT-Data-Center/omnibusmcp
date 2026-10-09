package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
)

// crashRecent is the age below which an unposted crash raises a warning;
// older ones are only reported.
const crashRecent = 30 * 24 * time.Hour

// sysBlock is the sysfs directory of block devices; tests replace it.
var sysBlock = "/sys/class/block"

// unit is one Ceph systemd unit on this host.
type unit struct {
	Name   string
	Daemon string // "osd.3", "mon.host1", "crash"; "" for helper units
	Active string // active, inactive, failed, activating
	Sub    string
	// Stale marks a cephadm unit of a daemon the orchestrator no longer
	// lists (moved or removed): informational only.
	Stale bool
}

// Failed reports a unit that is not running as it should.
func (u unit) Failed() bool {
	return u.Active == "failed" || (u.Active == "activating" && u.Sub == "auto-restart")
}

var (
	cephadmUnitRe = regexp.MustCompile(`^ceph-([0-9a-f-]{36})@(.+)\.service$`)
	nativeUnitRe  = regexp.MustCompile(`^ceph-([a-z]+)@(.+)\.service$`)
)

// parseUnits reads "systemctl list-units --all --plain --no-legend" lines.
func parseUnits(out string) []unit {
	var units []unit
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "●"))
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		u := unit{Name: f[0], Active: f[2], Sub: f[3]}
		switch m := cephadmUnitRe.FindStringSubmatch(u.Name); {
		case m != nil:
			u.Daemon = m[2]
		case u.Name == "ceph-crash.service":
			u.Daemon = "crash"
		default:
			if m := nativeUnitRe.FindStringSubmatch(u.Name); m != nil && m[1] != "volume" && m[1] != "fuse" {
				u.Daemon = nativeDaemon(m[1], m[2])
			}
		}
		if u.Daemon == "" {
			continue // ceph-volume activation, ceph-fuse mounts...
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return daemonLess(units[i].Daemon, units[j].Daemon) })
	return units
}

// nativeDaemon names the daemon of a packaged unit: ceph-osd@3 -> osd.3,
// ceph-radosgw@rgw.x -> rgw.x.
func nativeDaemon(typ, id string) string {
	if typ == "radosgw" {
		return id
	}
	return typ + "." + id
}

// daemonLess sorts by type, then numerically for OSDs.
func daemonLess(a, b string) bool {
	ta, ia, _ := strings.Cut(a, ".")
	tb, ib, _ := strings.Cut(b, ".")
	if ta != tb {
		return ta < tb
	}
	if len(ia) != len(ib) && ta == "osd" {
		return len(ia) < len(ib)
	}
	return ia < ib
}

func localUnits(ctx context.Context, env *registry.Env) ([]unit, error) {
	r := env.Exec.Run(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--no-pager", "--type=service", "ceph*")
	if !r.OK() {
		return nil, fmt.Errorf("systemctl list-units: %s", strings.TrimSpace(r.Format()))
	}
	return parseUnits(r.Stdout), nil
}

// osdDevice maps a local OSD to its block device.
type osdDevice struct {
	OSD    string // osd.3
	Block  string // resolved block path, e.g. /dev/dm-3
	Disks  []string
	Errstr string
}

// localOSDs finds the block device of every OSD data directory on this host.
func localOSDs(s site) []osdDevice {
	var dirs []string
	if s.Variant == VariantCephadm && s.FSID != "" {
		dirs, _ = filepath.Glob(filepath.Join(varLibCeph, s.FSID, "osd.*"))
	} else {
		dirs, _ = filepath.Glob(filepath.Join(varLibCeph, "osd", "ceph-*"))
	}
	var out []osdDevice
	for _, d := range dirs {
		base := filepath.Base(d)
		id := strings.TrimPrefix(strings.TrimPrefix(base, "osd."), "ceph-")
		dev := osdDevice{OSD: "osd." + id}
		// Only the link is read: the service runs with PrivateDevices, so
		// its /dev does not contain the OSD devices; sysfs does.
		target, err := os.Readlink(filepath.Join(d, "block"))
		switch {
		case err != nil:
			dev.Errstr = "no block device link"
		default:
			if !filepath.IsAbs(target) {
				target = filepath.Join(d, target)
			}
			dev.Block = target
			if name := blockName(target); name != "" {
				dev.Disks = backingDisks(name)
			} else {
				dev.Errstr = "device " + target + " not found in sysfs"
			}
		}
		out = append(out, dev)
	}
	sort.Slice(out, func(i, j int) bool { return daemonLess(out[i].OSD, out[j].OSD) })
	return out
}

// blockName finds the kernel name (sdc, nvme0n1, dm-3) of a device path:
// /dev/<vg>/<lv> and /dev/mapper/<name> are matched against the
// device-mapper names in sysfs (LVM doubles "-" inside VG and LV names).
func blockName(dev string) string {
	dev = filepath.Clean(dev)
	var dmName string
	switch rel := strings.TrimPrefix(dev, "/dev/"); {
	case rel == dev:
		return ""
	case strings.HasPrefix(rel, "mapper/"):
		dmName = strings.TrimPrefix(rel, "mapper/")
	case strings.Count(rel, "/") == 1:
		vg, lv, _ := strings.Cut(rel, "/")
		dmName = strings.ReplaceAll(vg, "-", "--") + "-" + strings.ReplaceAll(lv, "-", "--")
	default:
		if _, err := os.Stat(filepath.Join(sysBlock, rel)); err == nil {
			return rel
		}
		return ""
	}
	dms, _ := filepath.Glob(filepath.Join(sysBlock, "dm-*"))
	for _, dm := range dms {
		if b, err := os.ReadFile(filepath.Join(dm, "dm", "name")); err == nil && strings.TrimSpace(string(b)) == dmName {
			return filepath.Base(dm)
		}
	}
	return ""
}

// backingDisks resolves a device-mapper device (LVM, dm-crypt) to the
// disks under it via sysfs; a partition resolves to its disk.
func backingDisks(name string) []string {
	if slaves, err := os.ReadDir(filepath.Join(sysBlock, name, "slaves")); err == nil && len(slaves) > 0 {
		var out []string
		for _, sl := range slaves {
			out = append(out, backingDisks(sl.Name())...)
		}
		return out
	}
	if _, err := os.Stat(filepath.Join(sysBlock, name, "partition")); err == nil {
		if real, err := filepath.EvalSymlinks(filepath.Join(sysBlock, name)); err == nil {
			return []string{filepath.Base(filepath.Dir(real))}
		}
	}
	return []string{name}
}

// crash is an unposted crash report in this host's crash directory.
type crash struct {
	ID     string
	Time   time.Time
	Entity string // osd.12, client.admin...
}

// localCrashes lists crash reports the ceph-crash agent has not posted to
// the cluster (posted ones move to "posted/").
func localCrashes(s site) []crash {
	dirs := []string{filepath.Join(varLibCeph, "crash")}
	if s.FSID != "" {
		dirs = append(dirs, filepath.Join(varLibCeph, s.FSID, "crash"))
	}
	var out []crash
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "posted" {
				continue
			}
			c := crash{ID: e.Name(), Entity: crashEntity(filepath.Join(dir, e.Name(), "meta"))}
			ts, _, _ := strings.Cut(e.Name(), "_")
			c.Time, _ = time.Parse("2006-01-02T15:04:05.999999Z", ts)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func crashEntity(meta string) string {
	f, err := os.Open(meta)
	if err != nil {
		return ""
	}
	defer f.Close()
	var m struct {
		Entity string `json:"entity_name"`
	}
	json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&m)
	return m.Entity
}

// orchDaemon is one entry of "ceph orch ps".
type orchDaemon struct {
	DaemonName string `json:"daemon_name"`
	DaemonType string `json:"daemon_type"`
	Hostname   string `json:"hostname"`
	Status     int    `json:"status"`
	StatusDesc string `json:"status_desc"`
	Version    string `json:"version"`
	IsActive   bool   `json:"is_active"`
	Started    string `json:"started"`
	LastRefr   string `json:"last_refresh"`
}

// markStale flags cephadm units of daemons the orchestrator does not list.
func markStale(units []unit, orch []orchDaemon) {
	known := map[string]bool{}
	for _, d := range orch {
		known[d.DaemonName] = true
	}
	for i := range units {
		if cephadmUnitRe.MatchString(units[i].Name) {
			units[i].Stale = !known[units[i].Daemon]
		}
	}
}

func (m *Module) localTool(ctx context.Context, env *registry.Env) (string, error) {
	s := m.site(env)
	units, uErr := localUnits(ctx, env)
	var orch []orchDaemon
	if s.Variant == VariantCephadm && s.Cluster && uErr == nil {
		if query(ctx, env, s, &orch, "orch", "ps") == nil && len(orch) > 0 {
			markStale(units, orch)
		}
	}
	return renderLocal(s, units, uErr, localOSDs(s), localCrashes(s), time.Now()), nil
}

func renderLocal(s site, units []unit, uErr error, osds []osdDevice, crashes []crash, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Installation: %s", s.Variant)
	if s.FSID != "" {
		fmt.Fprintf(&b, ", cluster %s", s.FSID)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Config:       %s\n", firstNonEmpty(s.Conf, "none found"))
	if s.Cluster {
		fmt.Fprintf(&b, "Cluster view: yes (%s, %s)\n", clientName, s.Keyring)
	} else {
		fmt.Fprintf(&b, "Cluster view: no, local view only: %s\n", s.LocalReason)
	}

	b.WriteString("\nDaemons on this host:\n")
	switch {
	case uErr != nil:
		fmt.Fprintf(&b, "  unknown: %v\n", uErr)
	case len(units) == 0:
		b.WriteString("  none\n")
	default:
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  DAEMON\tSTATE\tUNIT\tNOTE")
		for _, u := range units {
			note := ""
			switch {
			case u.Stale && u.Failed():
				note = "not in the orchestrator (stale unit of a moved or removed daemon)"
			case u.Failed():
				note = "FAILED"
			case u.Stale:
				note = "not in the orchestrator"
			}
			fmt.Fprintf(tw, "  %s\t%s/%s\t%s\t%s\n", u.Daemon, u.Active, u.Sub, u.Name, note)
		}
		tw.Flush()
	}

	if len(osds) > 0 {
		b.WriteString("\nOSD devices:\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, o := range osds {
			if o.Errstr != "" {
				fmt.Fprintf(tw, "  %s\t%s\n", o.OSD, o.Errstr)
				continue
			}
			fmt.Fprintf(tw, "  %s\tdisk %s\t%s\n", o.OSD, strings.Join(o.Disks, ", "), o.Block)
		}
		tw.Flush()
	}

	if len(crashes) > 0 {
		recent := 0
		for _, c := range crashes {
			if now.Sub(c.Time) < crashRecent {
				recent++
			}
		}
		fmt.Fprintf(&b, "\nUnposted crash reports: %d (%d in the last %d days); the ceph-crash agent has not sent them to the cluster:\n",
			len(crashes), recent, int(crashRecent.Hours()/24))
		for i, c := range crashes {
			if i == 10 {
				fmt.Fprintf(&b, "  ... %d more\n", len(crashes)-i)
				break
			}
			fmt.Fprintf(&b, "  %s  %s  %s\n", c.Time.Format("2006-01-02 15:04"), firstNonEmpty(c.Entity, "?"), c.ID)
		}
	}
	return b.String()
}
