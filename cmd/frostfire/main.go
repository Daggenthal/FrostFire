// Command frostfire runs a MariaDB + PHP + nginx website in containers and backs it up with restic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Daggenthal/FrostFire/internal/app"
	"github.com/Daggenthal/FrostFire/internal/config"
	"github.com/Daggenthal/FrostFire/internal/trace"
)

var version = "dev"

const usage = `FrostFire: containerized website + MariaDB, with real backups.

Usage:
  frostfire [--root DIR] <command> [options]

Setup:
  setup                   Install what's missing: Podman (or --engine docker), compose,
                          restic, boot and firewall setup (--dry-run, --yes)
  init                    Create the root, frostfire.env and default config
  doctor                  Check settings, engine, restic and the backup repository
  diag                    Print a full diagnostics report (save it: frostfire diag > diag.txt)
  repo init               Create the backup repository (once)

Stack:
  up                      Start or update every container
  down                    Stop and remove the containers (data stays)
  status                  Show container status
  logs [service...]       Follow logs (services: db, cache, php, web, certbot)
  certs issue             Get a Let's Encrypt certificate and enable HTTPS

Backups:
  backup                  Back up site files and all databases
  list                    List backups
  restore [RUN|latest]    Restore a backup (--files-only, --db-only, --yes)
  prune                   Apply the KEEP_* retention policy
  repo check              Verify the backup repository

Database:
  db shell                Open a MariaDB prompt as root
  db dump [FILE]          Write a full SQL dump (default: stdout)
  db load FILE            Load an SQL dump (.sql or .sql.gz), e.g. from the old server

Global options:
  --root DIR              FrostFire root (default $FROSTFIRE_ROOT or /srv/frostfire)
  -v, --verbose           Print every command frostfire runs, with timing and exit status
                          (or set FROSTFIRE_VERBOSE=1). Secrets are never printed
  --version               Print the version
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "frostfire: error:", err)
		os.Exit(1)
	}
}

func run() error {
	global := flag.NewFlagSet("frostfire", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	root := global.String("root", envOr("FROSTFIRE_ROOT", config.DefaultRoot), "")
	showVersion := global.Bool("version", false, "")
	var verbose bool
	global.BoolVar(&verbose, "verbose", os.Getenv("FROSTFIRE_VERBOSE") != "", "")
	global.BoolVar(&verbose, "v", os.Getenv("FROSTFIRE_VERBOSE") != "", "")
	if err := global.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println("frostfire", version)
		return nil
	}
	if verbose {
		trace.Enable(os.Stderr)
		trace.Logf("frostfire %s on %s/%s, root %s", version, runtime.GOOS, runtime.GOARCH, *root)
	}
	args := global.Args()
	if len(args) == 0 {
		global.Usage()
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := app.New(*root)
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "setup":
		fs := flag.NewFlagSet("setup", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "don't ask for confirmation")
		dryRun := fs.Bool("dry-run", false, "show the plan without changing anything")
		engineName := fs.String("engine", "", "podman or docker (default: keep a working one, else Podman)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return a.Setup(ctx, app.SetupOptions{Engine: *engineName, Yes: *yes, DryRun: *dryRun})
	case "diag":
		return a.Diag(ctx, version)
	case "init":
		return a.Init()
	case "doctor":
		return a.Doctor(ctx)
	case "up":
		return a.Up(ctx)
	case "down":
		return a.Down(ctx)
	case "status", "ps":
		return a.Status(ctx)
	case "logs":
		return a.Logs(ctx, rest)
	case "backup":
		return a.Backup(ctx)
	case "list", "snapshots":
		return a.List(ctx)
	case "prune":
		return a.Prune(ctx)
	case "restore":
		return restore(ctx, a, rest)
	case "certs":
		if len(rest) == 1 && rest[0] == "issue" {
			return a.IssueCert(ctx)
		}
		return errors.New("usage: frostfire certs issue")
	case "repo":
		switch {
		case len(rest) == 1 && rest[0] == "init":
			return a.RepoInit(ctx)
		case len(rest) == 1 && rest[0] == "check":
			return a.RepoCheck(ctx)
		}
		return errors.New("usage: frostfire repo init|check")
	case "db":
		return db(ctx, a, rest)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (see `frostfire help`)", cmd)
}

func restore(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	filesOnly := fs.Bool("files-only", false, "restore www, certs and config only")
	dbOnly := fs.Bool("db-only", false, "restore the databases only")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if *filesOnly && *dbOnly {
		return errors.New("--files-only and --db-only are mutually exclusive")
	}
	runID := "latest"
	if fs.NArg() > 1 {
		return errors.New("usage: frostfire restore [RUN|latest] [--files-only|--db-only] [--yes]")
	}
	if fs.NArg() == 1 {
		runID = fs.Arg(0)
	}
	return a.Restore(ctx, app.RestoreOptions{
		Run:      runID,
		Files:    !*dbOnly,
		Database: !*filesOnly,
		Yes:      *yes,
	})
}

func db(ctx context.Context, a *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: frostfire db shell | db dump [FILE] | db load FILE [--yes]")
	}
	switch args[0] {
	case "shell":
		return a.DBShell(ctx)
	case "dump":
		path := "-"
		if len(args) == 2 {
			path = args[1]
		}
		if path == "-" {
			if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				return errors.New("refusing to print a database dump to the terminal: give a FILE or redirect stdout")
			}
		}
		return a.DBDump(ctx, path)
	case "load":
		fs := flag.NewFlagSet("db load", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "don't ask for confirmation")
		if err := fs.Parse(reorder(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: frostfire db load FILE [--yes]")
		}
		return a.DBLoad(ctx, fs.Arg(0), *yes)
	}
	return fmt.Errorf("unknown db command %q", args[0])
}

// reorder moves flags in front of positional args so `restore latest --yes` works
// as well as `restore --yes latest`.
func reorder(args []string) []string {
	var flags, pos []string
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
