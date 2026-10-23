package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Daggenthal/FrostFire/internal/config"
	"github.com/Daggenthal/FrostFire/internal/engine"
	"github.com/Daggenthal/FrostFire/internal/restic"
)

// Doctor checks everything frostfire depends on and reports each result. It returns
// an error if any check failed, so it can gate a cron job or CI run.
func (a *App) Doctor(ctx context.Context) error {
	failed := 0
	check := func(ok bool, label, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		if detail != "" {
			label += ": " + detail
		}
		fmt.Fprintf(a.Stdout, "[%s] %s\n", mark, label)
	}

	if err := a.load(); err != nil {
		check(false, "settings", err.Error())
		return fmt.Errorf("1 check failed")
	}
	envPath := config.EnvPath(a.Root)
	if st, err := os.Stat(envPath); err == nil {
		perm := st.Mode().Perm()
		check(perm&0o077 == 0, "frostfire.env permissions", fmt.Sprintf("%04o (want 0600)", perm))
	}

	missing := a.Cfg.Missing("DOMAIN", "ADMIN_EMAIL", "MARIADB_ROOT_PASSWORD", "RESTIC_REPOSITORY")
	check(len(missing) == 0, "required settings", strings.Join(missing, ", "))
	if a.Cfg.Get("SMTP_HOST", "") != "" {
		missing := a.Cfg.Missing("SMTP_PASSWORD", "SMTP_FROM")
		check(len(missing) == 0, "mail settings", strings.Join(missing, ", "))
	}

	bin, err := engine.Detect(a.Cfg.Get("FROSTFIRE_ENGINE", ""))
	if err != nil {
		check(false, "container engine", err.Error())
	} else {
		check(true, "container engine", bin+" compose")
	}

	if a.OS == "freebsd" {
		h := realHost{}
		mounts, _ := h.Output("mount", "-t", "fdescfs")
		check(strings.Contains(mounts, "/dev/fd"), "fdescfs on /dev/fd", "")
		check(h.Probe("service", "pf", "status"), "pf running", "")
		pf, _ := h.ReadFile(pfConf)
		check(strings.Contains(string(pf), "cni-rdr"), "pf.conf has Podman's anchors", "")
		fl, _ := h.Output("sysctl", "-n", "net.pf.filter_local")
		check(strings.TrimSpace(fl) == "1", "net.pf.filter_local=1", "")
		pe, _ := h.Output("sysrc", "-n", "podman_enable")
		check(strings.TrimSpace(pe) == "YES", "containers restart on boot (podman_enable)", "")
		if failed > 0 {
			fmt.Fprintln(a.Stdout, "      FreeBSD host prep is incomplete: run `frostfire setup`")
		}
	}

	if rs, err := a.backupRepo(); err != nil {
		check(false, "backup settings", err.Error())
	} else if v, err := rs.Version(ctx); err != nil {
		check(false, "restic", err.Error()+" (run `frostfire setup`)")
	} else {
		ok := restic.AtLeast(v, restic.MinVersion)
		detail := fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
		if !ok {
			detail += fmt.Sprintf(" (need %d.%d.%d or newer, run `frostfire setup`)", restic.MinVersion[0], restic.MinVersion[1], restic.MinVersion[2])
		}
		check(ok, "restic", detail)
		if ok {
			if err := rs.RepoReachable(ctx); err != nil {
				check(false, "backup repository", err.Error()+" (new repository? run `frostfire repo init`)")
			} else {
				check(true, "backup repository", a.Cfg.Get("RESTIC_REPOSITORY", ""))
			}
		}
	}

	certDetail := a.Cfg.Get("DOMAIN", "")
	if !a.haveCert() {
		certDetail += " (run `frostfire certs issue` after `frostfire up`)"
	}
	check(a.haveCert(), "TLS certificate", certDetail)

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Fprintln(a.Stdout, "All good.")
	return nil
}
