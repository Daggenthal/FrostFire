package app

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Daggenthal/FrostFire/internal/config"
	"github.com/Daggenthal/FrostFire/internal/restic"
	"github.com/Daggenthal/FrostFire/internal/stack"
)

const (
	tagAll   = "frostfire"
	tagFiles = "files"
	tagDB    = "db"
	runTag   = "run-"
)

// A Run is one `frostfire backup`: a files snapshot and a database snapshot sharing a run tag.
type Run struct {
	ID    string
	Time  time.Time
	Host  string
	Files *restic.Snapshot
	DB    *restic.Snapshot
}

func (r Run) Complete() bool { return r.Files != nil && r.DB != nil }

// GroupRuns pairs snapshots by run tag, newest first.
func GroupRuns(snaps []restic.Snapshot) []Run {
	byID := map[string]*Run{}
	for i := range snaps {
		s := &snaps[i]
		id := ""
		for _, t := range s.Tags {
			if strings.HasPrefix(t, runTag) {
				id = strings.TrimPrefix(t, runTag)
			}
		}
		if id == "" {
			continue
		}
		r := byID[id]
		if r == nil {
			r = &Run{ID: id, Time: s.Time, Host: s.Hostname}
			byID[id] = r
		}
		if s.Time.Before(r.Time) {
			r.Time = s.Time
		}
		switch {
		case s.HasTag(tagFiles):
			r.Files = s
		case s.HasTag(tagDB):
			r.DB = s
		}
	}
	runs := make([]Run, 0, len(byID))
	for _, r := range byID {
		runs = append(runs, *r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Time.After(runs[j].Time) })
	return runs
}

// Backup snapshots the site files and a consistent dump of every database.
func (a *App) Backup(ctx context.Context) error {
	rs, err := a.backupRepo()
	if err != nil {
		return err
	}
	c, err := a.engine()
	if err != nil {
		return err
	}
	if !a.dbAlive(ctx) {
		return errors.New("the database container is not running (start it with `frostfire up`)")
	}

	id := time.Now().UTC().Format("20060102T150405Z")
	tags := []string{tagAll, runTag + id}

	var paths []string
	for _, item := range backupItems {
		p := filepath.Join(a.Root, item)
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}

	// Database first: if the dump fails, the run stops before leaving a lone files snapshot.
	start := time.Now()
	a.step("Backing up databases")
	if err := rs.BackupCommand(ctx, append(tags, tagDB), dbDumpName, c.Argv(dumpArgs()...)); err != nil {
		return err
	}
	a.step("Backing up files (%d paths)", len(paths))
	if err := rs.BackupPaths(ctx, append(tags, tagFiles), paths); err != nil {
		return err
	}
	a.step("Backup %s finished in %s", id, time.Since(start).Round(time.Second))
	return nil
}

// List prints backup runs, newest first.
func (a *App) List(ctx context.Context) error {
	runs, err := a.runs(ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(a.Stdout, "No backups yet. Run: frostfire backup")
		return nil
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tTAKEN\tHOST\tFILES\tDB")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Time.Local().Format("2006-01-02 15:04"),
			r.Host, shortID(r.Files), shortID(r.DB))
	}
	return tw.Flush()
}

func shortID(s *restic.Snapshot) string {
	if s == nil {
		return "missing"
	}
	return s.ShortID
}

func (a *App) runs(ctx context.Context) ([]Run, error) {
	rs, err := a.backupRepo()
	if err != nil {
		return nil, err
	}
	snaps, err := rs.Snapshots(ctx, tagAll)
	if err != nil {
		return nil, err
	}
	return GroupRuns(snaps), nil
}

// RestoreOptions picks what to restore.
type RestoreOptions struct {
	Run      string // run ID, or "latest"
	Files    bool
	Database bool
	Yes      bool
}

// Restore puts a backup run back in place. Replaced directories are moved aside into
// .pre-restore/ rather than deleted, so a bad restore can be undone by hand.
func (a *App) Restore(ctx context.Context, opt RestoreOptions) error {
	rs, err := a.backupRepo()
	if err != nil {
		return err
	}
	c, err := a.engine()
	if err != nil {
		return err
	}
	runs, err := a.runs(ctx)
	if err != nil {
		return err
	}
	run, err := pickRun(runs, opt)
	if err != nil {
		return err
	}

	var what []string
	if opt.Files {
		what = append(what, "www, certs and config")
	}
	if opt.Database {
		what = append(what, "ALL databases")
	}
	q := fmt.Sprintf("Restore %s from run %s (taken %s on %s)?",
		strings.Join(what, " and "), run.ID, run.Time.Local().Format("2006-01-02 15:04"), run.Host)
	if err := a.confirm(opt.Yes, q); err != nil {
		return err
	}

	a.step("Stopping web services")
	if err := c.Run(ctx, "stop", "web", "php", "certbot"); err != nil {
		a.warn("%v (continuing)", err)
	}

	if opt.Files {
		if err := a.restoreFiles(ctx, rs, run.Files); err != nil {
			return err
		}
	}
	if opt.Database {
		if err := a.ensureDB(ctx); err != nil {
			return err
		}
		a.step("Restoring databases from snapshot %s", run.DB.ShortID)
		if err := pipe(rs.DumpCmd(ctx, run.DB.ID, "/"+dbDumpName), c.Cmd(ctx, loadArgs()...)); err != nil {
			return fmt.Errorf("database restore failed: %w", err)
		}
		a.checkRootPassword()
	}

	if err := a.Up(ctx); err != nil {
		return err
	}
	a.step("Restore of %s complete", run.ID)
	return nil
}

func pickRun(runs []Run, opt RestoreOptions) (Run, error) {
	usable := func(r Run) bool {
		return (!opt.Files || r.Files != nil) && (!opt.Database || r.DB != nil)
	}
	for _, r := range runs {
		if opt.Run == "latest" && usable(r) {
			return r, nil
		}
		if r.ID == opt.Run {
			if !usable(r) {
				return Run{}, fmt.Errorf("run %s is incomplete (files: %s, db: %s)", r.ID, shortID(r.Files), shortID(r.DB))
			}
			return r, nil
		}
	}
	if opt.Run == "latest" {
		return Run{}, errors.New("no usable backup found (see `frostfire list`)")
	}
	return Run{}, fmt.Errorf("no backup run %q (see `frostfire list`)", opt.Run)
}

func (a *App) restoreFiles(ctx context.Context, rs *restic.Restic, snap *restic.Snapshot) error {
	stamp := time.Now().Format("20060102-150405")
	staging := filepath.Join(a.Root, ".restore", stamp)
	aside := filepath.Join(a.Root, ".pre-restore", stamp)

	a.step("Restoring files from snapshot %s", snap.ShortID)
	if err := rs.Restore(ctx, snap.ID, staging); err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Join(a.Root, ".restore"))

	// Snapshot paths are absolute and may come from a server with a different root,
	// so each one is matched by its base name.
	for _, p := range snap.Paths {
		name := filepath.Base(p)
		src := filepath.Join(staging, p)
		if _, err := os.Stat(src); err != nil {
			a.warn("%s missing from snapshot, skipping", p)
			continue
		}
		if name == config.EnvFileName {
			dst := filepath.Join(a.Root, config.EnvFileName+".restored")
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			a.step("Saved the backed-up settings as %s (your current frostfire.env is untouched)", dst)
			continue
		}
		dst := filepath.Join(a.Root, name)
		if _, err := os.Lstat(dst); err == nil {
			if err := os.MkdirAll(aside, 0o750); err != nil {
				return err
			}
			if err := os.Rename(dst, filepath.Join(aside, name)); err != nil {
				return err
			}
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		a.step("Restored %s", name)
	}
	if _, err := os.Stat(aside); err == nil {
		a.step("Previous files moved to %s (delete once you're happy)", aside)
	}
	return stack.RenderNginx(a.Root, a.haveCert())
}

// A full dump includes the mysql schema, so after loading it the root password is
// whatever it was on the server the backup came from.
func (a *App) checkRootPassword() {
	f, err := os.Open(filepath.Join(a.Root, config.EnvFileName+".restored"))
	if err != nil {
		return
	}
	defer f.Close()
	old, err := config.Parse(f)
	if err != nil {
		return
	}
	if p := old["MARIADB_ROOT_PASSWORD"]; p != "" && p != a.Cfg.Get("MARIADB_ROOT_PASSWORD", "") {
		a.warn("the restored databases use the MariaDB root password from the backed-up server. " +
			"Copy MARIADB_ROOT_PASSWORD from frostfire.env.restored into frostfire.env, then run `frostfire up`.")
	}
}

// Prune applies the retention policy from frostfire.env.
func (a *App) Prune(ctx context.Context) error {
	rs, err := a.backupRepo()
	if err != nil {
		return err
	}
	var keep restic.Keep
	for _, k := range []struct {
		key string
		def int
		dst *int
	}{{"KEEP_DAILY", 7, &keep.Daily}, {"KEEP_WEEKLY", 4, &keep.Weekly}, {"KEEP_MONTHLY", 6, &keep.Monthly}} {
		n, err := a.Cfg.Int(k.key, k.def)
		if err != nil {
			return err
		}
		*k.dst = n
	}
	a.step("Pruning: keeping %d daily, %d weekly, %d monthly", keep.Daily, keep.Weekly, keep.Monthly)
	return rs.Forget(ctx, tagAll, keep)
}

// RepoInit creates the backup repository.
func (a *App) RepoInit(ctx context.Context) error {
	rs, err := a.backupRepo()
	if err != nil {
		return err
	}
	return rs.Init(ctx)
}

// RepoCheck verifies the backup repository.
func (a *App) RepoCheck(ctx context.Context) error {
	rs, err := a.backupRepo()
	if err != nil {
		return err
	}
	return rs.Check(ctx)
}

type gzipFile struct {
	*gzip.Reader
	f *os.File
}

func (g gzipFile) Close() error {
	g.Reader.Close()
	return g.f.Close()
}

func openMaybeGzip(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return gzipFile{zr, f}, nil
}
