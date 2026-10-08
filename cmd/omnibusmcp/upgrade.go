package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/PNT-Data-Center/omnibusmcp/internal/install"
	"github.com/PNT-Data-Center/omnibusmcp/internal/ui"
	"github.com/PNT-Data-Center/omnibusmcp/internal/upgrade"
)

// startWait is how long a restarted service must stay active to count as up.
const startWait = 5 * time.Second

// updateCheckTimeout bounds the release check of "omnibusmcp status".
const updateCheckTimeout = 2 * time.Second

func runUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	want := fs.String("version", "", "install this release (vX.Y.Z) instead of the latest; older releases are allowed")
	force := fs.Bool("force", false, "reinstall even if the version is already installed")
	yes := fs.Bool("yes", false, "do not ask for confirmation (downgrade, development build)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (see: omnibusmcp help)", fs.Arg(0))
	}
	if *want != "" && !upgrade.ValidTag(*want) {
		return fmt.Errorf("invalid version %q (expected e.g. v0.4.1)", *want)
	}

	src := upgrade.DefaultSource()
	tag := *want
	if tag == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		latest, err := src.Latest(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("cannot check the latest release: %w", err)
		}
		tag = latest
	}
	cmp, comparable := upgrade.Compare(version, tag)

	if *check {
		line("Installed", ui.Cyan(version))
		line("Latest", ui.Cyan(tag))
		switch {
		case !comparable:
			fmt.Println(ui.Yellow("development build; install the release with: sudo omnibusmcp upgrade --yes"))
		case cmp < 0:
			fmt.Println(ui.Green("update available: sudo omnibusmcp upgrade"))
		default:
			fmt.Println(ui.Green("up to date"))
		}
		return nil
	}

	switch {
	case !comparable:
		if !*yes {
			return fmt.Errorf("this is a development build (%s); replace it with %s using --yes", version, tag)
		}
	case cmp == 0 && !*force:
		fmt.Printf("%s %s %s\n", ui.Green("already at"), ui.Cyan(tag), ui.Dim("(use --force to reinstall)"))
		return nil
	case cmp > 0 && !*yes:
		if !confirm(fmt.Sprintf("Downgrade %s to %s?", version, tag)) {
			return errors.New("downgrade not confirmed (use --yes to skip the question)")
		}
	}

	if os.Geteuid() != 0 {
		return errors.New("must be run as root")
	}
	unlock, err := upgrade.Lock(upgrade.LockPath)
	if err != nil {
		return err
	}
	defer unlock()

	// The service decides which binary is upgraded: the one its ExecStart runs.
	target, err := install.ExecPath()
	if err != nil {
		return fmt.Errorf("systemctl: %w", err)
	}
	service := target != ""
	if !service {
		target = install.BinPath
	}

	asset, err := upgrade.AssetName()
	if err != nil {
		return err
	}
	fmt.Printf("%s%s %s\n", ui.Label("download", 10), asset, ui.Cyan(tag))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	data, err := src.Download(ctx, tag, asset)
	if err != nil {
		return err
	}
	sums, err := src.Download(ctx, tag, "SHA256SUMS")
	if err != nil {
		return err
	}
	if err := upgrade.VerifySum(sums, asset, data); err != nil {
		return fmt.Errorf("%w, nothing installed", err)
	}
	fmt.Printf("%s%s\n", ui.Label("checksum", 10), ui.Green("OK (SHA256SUMS)"))

	staged, err := upgrade.Stage(target, data, tag)
	if err != nil {
		return err
	}
	if err := upgrade.Replace(target, staged); err != nil {
		os.Remove(staged)
		return err
	}
	fmt.Printf("%s%s %s %s\n", ui.Label("binary", 10), target, ui.Green("replaced"), ui.Dim("(previous: "+upgrade.PrevPath(target)+")"))

	if !service {
		fmt.Printf("\n%s %s %s\n", ui.Bold(ui.Green("OmnibusMCP upgraded to")), ui.Bold(ui.Cyan(tag)), ui.Dim("(service not installed: omnibusmcp install)"))
		return nil
	}
	// The new binary rewrites the units from its own templates and restarts
	// the service; config and token are kept.
	err = refreshService(target)
	if err == nil {
		fmt.Printf("\n%s %s\n", ui.Bold(ui.Green("OmnibusMCP upgraded to")), ui.Bold(ui.Cyan(tag)))
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s %v\n", ui.Red("upgrade failed:"), err)

	fmt.Printf("%s%s %s\n", ui.Label("rollback", 10), target, ui.Yellow("restoring the previous binary"))
	if err := upgrade.Restore(target); err != nil {
		return fmt.Errorf("%s failed to start and the previous binary could not be restored: %w", tag, err)
	}
	if err := refreshService(target); err != nil {
		return fmt.Errorf("%s failed to start; previous binary restored but the service is not active either: %w", tag, err)
	}
	return fmt.Errorf("%s failed to start; rolled back to the previous binary (see: journalctl -u %s -n 50)", tag, install.UnitName)
}

// refreshService runs "bin install" (units, daemon-reload, restart) and
// checks that the service is still active after startWait.
func refreshService(bin string) error {
	cmd := exec.Command(bin, "install")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s install: %w", bin, err)
	}
	fmt.Printf("%s%s\n", ui.Label("waiting", 10), ui.Dim(fmt.Sprintf("%s for %s to stay active", startWait, install.UnitName)))
	time.Sleep(startWait)
	u, err := install.QueryUnit(install.UnitName)
	if err != nil {
		return err
	}
	if !u.Running() {
		return fmt.Errorf("%s is %s (%s) %s after restart", install.UnitName, u.ActiveState, u.SubState, startWait)
	}
	fmt.Printf("%s%s\n", ui.Label("service", 10), ui.Green("active (running)"))
	return nil
}

// confirm asks a yes/no question on the terminal; without one it says no.
func confirm(q string) bool {
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	fmt.Printf("%s [y/N] ", q)
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}

// updateStatus describes the latest release for "omnibusmcp status".
func updateStatus() string {
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	latest, err := upgrade.DefaultSource().Latest(ctx)
	if err != nil {
		return ui.Yellow("could not check (GitHub unreachable or not responding)")
	}
	switch cmp, ok := upgrade.Compare(version, latest); {
	case !ok:
		return ui.Dim("development build; latest release " + latest)
	case cmp < 0:
		return ui.Yellow(latest+" available") + ui.Dim(" (sudo omnibusmcp upgrade)")
	default:
		return ui.Green("up to date")
	}
}
