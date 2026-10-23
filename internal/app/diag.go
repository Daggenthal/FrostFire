package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Daggenthal/FrostFire/internal/config"
	"github.com/Daggenthal/FrostFire/internal/engine"
	"github.com/Daggenthal/FrostFire/internal/restic"
)

// Diag prints a diagnostics report: the host, the settings (names only, never values),
// the container engine and every container's recent logs, the backup repository, and
// on FreeBSD the pf, fdescfs, ZFS and Podman storage setup. Every command gets a time
// limit, so a stuck engine can't hang the report. Save it with: frostfire diag > diag.txt
func (a *App) Diag(ctx context.Context, version string) error {
	w := a.Stdout
	section := func(title string) { fmt.Fprintf(w, "\n===== %s =====\n", title) }
	run := func(timeout time.Duration, argv ...string) {
		fmt.Fprintf(w, "\n$ %s\n", strings.Join(argv, " "))
		if _, err := exec.LookPath(argv[0]); err != nil {
			fmt.Fprintf(w, "(%s is not installed)\n", argv[0])
			return
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
		cmd.WaitDelay = 5 * time.Second
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		start := time.Now()
		err := cmd.Run()
		w.Write(out.Bytes())
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			fmt.Fprintln(w)
		}
		switch {
		case cctx.Err() == context.DeadlineExceeded:
			fmt.Fprintf(w, "(TIMED OUT after %s)\n", timeout)
		case err != nil:
			fmt.Fprintf(w, "(exit: %v, %s)\n", err, time.Since(start).Round(time.Millisecond))
		}
	}
	file := func(path string) {
		fmt.Fprintf(w, "\n--- %s\n", path)
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(w, "(%v)\n", err)
			return
		}
		w.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			fmt.Fprintln(w)
		}
	}

	fmt.Fprintf(w, "frostfire diag: version %s, %s, root %s, running as uid %d\n",
		version, time.Now().Format(time.RFC3339), a.Root, os.Geteuid())

	section("Host")
	run(10*time.Second, "uname", "-a")
	if runtime.GOOS == "freebsd" {
		run(10*time.Second, "freebsd-version", "-ku")
	} else {
		file("/etc/os-release")
	}
	run(10*time.Second, "df", "-h", a.Root)

	section("Settings (frostfire.env, values hidden)")
	if st, err := os.Stat(config.EnvPath(a.Root)); err == nil {
		fmt.Fprintf(w, "%s: mode %04o\n", config.EnvPath(a.Root), st.Mode().Perm())
	}
	if err := a.load(); err != nil {
		fmt.Fprintf(w, "can't load settings: %v\n", err)
	} else {
		keys := make([]string, 0, len(a.Cfg.Vars))
		for k := range a.Cfg.Vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			state := "set"
			if len(a.Cfg.Missing(k)) > 0 {
				state = "EMPTY or placeholder"
			}
			fmt.Fprintf(w, "  %-24s %s\n", k, state)
		}
	}
	run(10*time.Second, "ls", "-la", a.Root)
	run(10*time.Second, "ls", "-laR", filepath.Join(a.Root, "stack"))

	section("Container engine")
	override := ""
	if a.Cfg != nil {
		override = a.Cfg.Get("FROSTFIRE_ENGINE", "")
	}
	bin, err := engine.Detect(override)
	if err != nil {
		fmt.Fprintf(w, "engine detection failed: %v\n", err)
		for _, name := range []string{"podman", "docker"} {
			run(20*time.Second, name, "version")
		}
	} else {
		fmt.Fprintf(w, "using: %s\n", bin)
		run(20*time.Second, bin, "version")
		run(30*time.Second, bin, "compose", "version")
		run(30*time.Second, bin, "info")
		run(30*time.Second, bin, "images")
		run(30*time.Second, bin, "ps", "-a")
		run(30*time.Second, bin, "network", "ls")
		if a.Cfg != nil {
			c := &engine.Compose{Bin: bin, Root: a.Root}
			run(60*time.Second, c.Argv("ps")...)
			for _, svc := range []string{"db", "cache", "php", "web", "certbot"} {
				run(60*time.Second, c.Argv("logs", "--tail", "40", svc)...)
			}
		}
	}

	section("Backups (restic)")
	if a.Cfg != nil {
		rs := &restic.Restic{Bin: restic.FindBin(a.Cfg.Get("RESTIC_BIN", ""))}
		fmt.Fprintf(w, "using: %s\n", rs.Bin)
		run(20*time.Second, rs.Bin, "version")
		if r, err := a.backupRepo(); err == nil {
			if err := r.RepoReachable(ctx); err != nil {
				fmt.Fprintf(w, "repository NOT reachable: %v\n", err)
			} else {
				fmt.Fprintln(w, "repository reachable")
				a.List(ctx)
			}
		} else {
			fmt.Fprintf(w, "%v\n", err)
		}
	}

	if runtime.GOOS == "freebsd" {
		section("FreeBSD host prep")
		run(10*time.Second, "mount", "-t", "fdescfs")
		run(10*time.Second, "sysrc", "-n", "pf_enable", "podman_enable")
		run(10*time.Second, "sysctl", "net.pf.filter_local")
		run(10*time.Second, "service", "pf", "status")
		file(pfConf)
		run(10*time.Second, "pfctl", "-s", "nat")
		run(10*time.Second, "pfctl", "-a", "cni-rdr", "-s", "Anchors")
		run(10*time.Second, "pfctl", "-t", "cni-nat", "-T", "show")
		run(10*time.Second, "route", "-n", "get", "default")
		run(10*time.Second, "zfs", "list", "-o", "name,mountpoint,used,avail")
		run(10*time.Second, "grep", "-n", "driver", storageConf)
		run(10*time.Second, "df", "-h", containersDir)
	} else {
		section("Linux host")
		run(10*time.Second, "systemctl", "is-enabled", "podman-restart.service", "docker.service")
		run(10*time.Second, "firewall-cmd", "--state")
		run(10*time.Second, "ufw", "status")
	}
	fmt.Fprintln(w, "\n===== end of report =====")
	return nil
}
