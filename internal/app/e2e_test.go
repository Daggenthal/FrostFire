package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Daggenthal/FrostFire/internal/config"
)

// fakeEngine stands in for `podman`/`docker`. It understands just enough compose to
// let backup and restore run for real against a local restic repository:
// `mariadb-dump` prints $STATE/db.sql and `mariadb` stores its stdin in $STATE/loaded.sql.
const fakeEngine = `#!/bin/sh
echo "$*" >> "$STATE/calls"
[ "$1" = compose ] || exit 1
shift
while [ $# -gt 0 ]; do
  case "$1" in
    --project-name|--project-directory|--file|--env-file) shift 2 ;;
    *) break ;;
  esac
done
cmd="$1"; shift
case "$cmd" in
  version) exit 0 ;;
  up) touch "$STATE/db_up"; exit 0 ;;
  stop|down|ps|logs|run) exit 0 ;;
  exec)
    [ -f "$STATE/db_up" ] || exit 1
    shift 2  # -T db
    if [ "$1" = mariadb-admin ]; then exit 0; fi
    snippet="$3"
    case "$snippet" in
      *'MYSQL_PWD="$MARIADB_ROOT_PASSWORD"'*) ;;
      *) echo "password not passed via MYSQL_PWD" >&2; exit 3 ;;
    esac
    case "$snippet" in
      *mariadb-dump*) [ -f "$STATE/dump_fails" ] && { echo partial; exit 2; }; cat "$STATE/db.sql" ;;
      *"exec mariadb -uroot") cat > "$STATE/loaded.sql" ;;
      *) exit 4 ;;
    esac ;;
  *) echo "fake engine: unhandled $cmd" >&2; exit 5 ;;
esac
`

func TestBackupRestoreEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not installed")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	state := filepath.Join(tmp, "state")
	must(t, os.MkdirAll(state, 0o755))
	t.Setenv("STATE", state)

	enginePath := filepath.Join(tmp, "fake-engine")
	must(t, os.WriteFile(enginePath, []byte(fakeEngine), 0o755))

	var out bytes.Buffer
	a := New(root)
	a.Stdout, a.Stderr = &out, &out
	must(t, a.Init())

	// Point the generated settings at the fake engine and a local restic repo.
	envPath := config.EnvPath(root)
	env, _ := os.ReadFile(envPath)
	env = append(env, []byte("\nFROSTFIRE_ENGINE="+enginePath+"\nRESTIC_REPOSITORY="+filepath.Join(tmp, "repo")+"\n")...)
	must(t, os.WriteFile(envPath, env, 0o600))

	st, _ := os.Stat(envPath)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("frostfire.env mode = %o, want 600", st.Mode().Perm())
	}

	must(t, a.RepoInit(ctx))
	must(t, os.WriteFile(filepath.Join(root, "www", "index.php"), []byte("<?php echo 'v1';"), 0o644))
	must(t, os.WriteFile(filepath.Join(state, "db.sql"), []byte("CREATE DATABASE site; -- v1\n"), 0o644))

	// Backup refuses while the database is down.
	if err := a.Backup(ctx); err == nil {
		t.Fatal("backup succeeded with the database down")
	}
	must(t, os.WriteFile(filepath.Join(state, "db_up"), nil, 0o644))
	must(t, a.Backup(ctx))

	// A failing dump must not leave a database snapshot behind.
	must(t, os.WriteFile(filepath.Join(state, "dump_fails"), nil, 0o644))
	if err := a.Backup(ctx); err == nil {
		t.Fatal("backup succeeded with a failing dump")
	}
	os.Remove(filepath.Join(state, "dump_fails"))

	runs, err := a.runs(ctx)
	must(t, err)
	complete := 0
	for _, r := range runs {
		if r.Complete() {
			complete++
		}
	}
	if complete != 1 || len(runs) != 1 {
		t.Fatalf("want exactly 1 run and it complete, got %d complete of %d", complete, len(runs))
	}

	// Break things, then restore.
	must(t, os.WriteFile(filepath.Join(root, "www", "index.php"), []byte("<?php echo 'broken';"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "www", "junk.txt"), []byte("junk"), 0o644))
	must(t, a.Restore(ctx, RestoreOptions{Run: "latest", Files: true, Database: true, Yes: true}))

	got, _ := os.ReadFile(filepath.Join(root, "www", "index.php"))
	if string(got) != "<?php echo 'v1';" {
		t.Fatalf("index.php after restore = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "www", "junk.txt")); err == nil {
		t.Fatal("junk.txt survived the restore")
	}
	loaded, _ := os.ReadFile(filepath.Join(state, "loaded.sql"))
	if string(loaded) != "CREATE DATABASE site; -- v1\n" {
		t.Fatalf("database received %q", loaded)
	}
	if _, err := os.Stat(filepath.Join(root, "frostfire.env.restored")); err != nil {
		t.Fatal("backed-up settings were not saved as frostfire.env.restored")
	}
	asides, _ := filepath.Glob(filepath.Join(root, ".pre-restore", "*", "www", "junk.txt"))
	if len(asides) != 1 {
		t.Fatal("previous www was not moved aside")
	}
	if _, err := os.Stat(filepath.Join(root, ".restore")); err == nil {
		t.Fatal("staging directory left behind")
	}
	// No cert in the test, so nginx must be in bootstrap mode.
	if _, err := os.Stat(filepath.Join(root, "stack", "nginx", "templates", "site.conf.template")); err == nil {
		t.Fatal("HTTPS site enabled without a certificate")
	}

	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if strings.Contains(string(calls), a.Cfg.Get("MARIADB_ROOT_PASSWORD", "")) {
		t.Fatal("MariaDB root password appeared on a command line")
	}

	must(t, a.Prune(ctx))
	if testing.Verbose() {
		t.Log(out.String())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
