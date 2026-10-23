// Package app implements the frostfire commands.
package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Daggenthal/FrostFire/internal/config"
	"github.com/Daggenthal/FrostFire/internal/engine"
	"github.com/Daggenthal/FrostFire/internal/platform"
	"github.com/Daggenthal/FrostFire/internal/restic"
	"github.com/Daggenthal/FrostFire/internal/stack"
)

// Directories under the root. db is the live MariaDB datadir and is never copied
// directly: it is backed up as a consistent dump instead.
var dataDirs = []string{"www", "certs", "acme", "db", "config"}

// What a files snapshot contains, relative to the root.
var backupItems = []string{"www", "certs", "config", config.EnvFileName}

const dbDumpName = "frostfire-db.sql"

type App struct {
	Root   string
	OS     string // "linux" or "freebsd": which container stack to run
	Cfg    *config.Config
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	compose *engine.Compose
	restic  *restic.Restic
}

// New creates an App for root. Settings are loaded lazily so `init` works on an empty root.
func New(root string) *App {
	return &App{Root: root, OS: runtime.GOOS, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
}

func (a *App) step(format string, args ...any) {
	fmt.Fprintf(a.Stdout, "==> "+format+"\n", args...)
}

func (a *App) warn(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "warning: "+format+"\n", args...)
}

func (a *App) load() error {
	if a.Cfg != nil {
		return nil
	}
	cfg, err := config.Load(a.Root)
	if err != nil {
		return err
	}
	a.Cfg = cfg
	return nil
}

func (a *App) engine() (*engine.Compose, error) {
	if a.compose != nil {
		return a.compose, nil
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	bin, err := engine.Detect(a.Cfg.Get("FROSTFIRE_ENGINE", ""))
	if err != nil {
		return nil, err
	}
	a.compose = &engine.Compose{Bin: bin, Root: a.Root, Stdout: a.Stdout, Stderr: a.Stderr}
	if a.OS == "freebsd" {
		v, err := platform.FreeBSDImageVersion(a.Cfg.Get("FREEBSD_IMAGE_VERSION", ""))
		if err != nil {
			return nil, err
		}
		a.compose.Env = []string{"FREEBSD_VERSION=" + v}
	}
	return a.compose, nil
}

func (a *App) backupRepo() (*restic.Restic, error) {
	if a.restic != nil {
		return a.restic, nil
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	if missing := a.Cfg.Missing("RESTIC_REPOSITORY"); len(missing) > 0 {
		return nil, fmt.Errorf("set RESTIC_REPOSITORY in %s", config.EnvPath(a.Root))
	}
	if a.Cfg.Get("RESTIC_PASSWORD", "") == "" && a.Cfg.Get("RESTIC_PASSWORD_FILE", "") == "" &&
		a.Cfg.Get("RESTIC_PASSWORD_COMMAND", "") == "" {
		return nil, fmt.Errorf("set RESTIC_PASSWORD (or RESTIC_PASSWORD_FILE) in %s", config.EnvPath(a.Root))
	}
	a.restic = &restic.Restic{
		Bin:    restic.FindBin(a.Cfg.Get("RESTIC_BIN", "")),
		Env:    a.Cfg.Environ(),
		Stdout: a.Stdout,
		Stderr: a.Stderr,
	}
	return a.restic, nil
}

// ---------------------------------------------------------------- init / up / down

// Init creates the root layout, a frostfire.env with generated secrets, and the default config.
func (a *App) Init() error {
	if err := os.MkdirAll(a.Root, 0o750); err != nil {
		return err
	}
	for _, d := range dataDirs {
		if err := os.MkdirAll(filepath.Join(a.Root, d), 0o755); err != nil {
			return err
		}
	}

	envPath := config.EnvPath(a.Root)
	if _, err := os.Stat(envPath); errors.Is(err, os.ErrNotExist) {
		env := string(stack.EnvExample())
		env = strings.Replace(env, "MARIADB_ROOT_PASSWORD=__GENERATED__", "MARIADB_ROOT_PASSWORD="+randomSecret(32), 1)
		env = strings.Replace(env, "RESTIC_PASSWORD=__GENERATED__", "RESTIC_PASSWORD="+randomSecret(40), 1)
		if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
			return err
		}
		a.step("Created %s with generated passwords", envPath)
	} else {
		a.step("Keeping existing %s", envPath)
	}

	created, err := stack.WriteConfigDefaults(a.Root)
	if err != nil {
		return err
	}
	for _, p := range created {
		a.step("Created %s", p)
	}

	fmt.Fprintf(a.Stdout, `
Next steps:
  1. Edit %s (DOMAIN, ADMIN_EMAIL, SMTP_PASSWORD, RESTIC_REPOSITORY)
  2. Save RESTIC_PASSWORD in your password manager
  3. frostfire doctor         (if anything is missing: frostfire setup)
  4. frostfire repo init      (once, creates the backup repository)
  5. frostfire up
  6. frostfire certs issue
`, envPath)
	return nil
}

// Up writes the stack files and starts (or updates) every container.
func (a *App) Up(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	if err := a.prepare(ctx); err != nil {
		return err
	}
	a.step("Starting the stack")
	args := []string{"up", "--detach", "--remove-orphans"}
	if len(stack.Images(a.OS)) == 0 {
		args = append(args, "--build") // Linux: compose builds the PHP image itself
	}
	return c.Run(ctx, args...)
}

// prepare writes the stack files for this OS and, where images are built locally
// (FreeBSD), builds them. Builds are cached, so this is quick after the first time.
func (a *App) prepare(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	for _, d := range dataDirs {
		if err := os.MkdirAll(filepath.Join(a.Root, d), 0o755); err != nil {
			return err
		}
	}
	if err := stack.WriteStack(a.Root, a.OS); err != nil {
		return fmt.Errorf("writing stack files: %w", err)
	}
	if images := stack.Images(a.OS); len(images) > 0 {
		args := map[string]string{}
		for _, kv := range c.Env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == "FREEBSD_VERSION" {
				args[k] = v
			}
		}
		for _, name := range images {
			a.step("Building image %s", name)
			if err := c.Build(ctx, "localhost/frostfire-"+name+":latest", stack.ImageDir(a.Root, name), args); err != nil {
				return err
			}
		}
	}
	if _, err := stack.WriteConfigDefaults(a.Root); err != nil {
		return err
	}
	haveCert := a.haveCert()
	if !haveCert {
		a.warn("no certificate for %s yet: serving ACME challenges only until you run `frostfire certs issue`", a.Cfg.Get("DOMAIN", "DOMAIN"))
	}
	return stack.RenderNginx(a.Root, haveCert)
}

func (a *App) haveCert() bool {
	_, err := os.Stat(stack.CertPath(a.Root, a.Cfg.Get("DOMAIN", "")))
	return err == nil
}

func (a *App) Down(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	return c.Run(ctx, "down")
}

func (a *App) Status(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	return c.Run(ctx, "ps")
}

func (a *App) Logs(ctx context.Context, services []string) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	return c.Run(ctx, append([]string{"logs", "--follow", "--tail", "100"}, services...)...)
}

// ---------------------------------------------------------------- certs

// IssueCert gets a Let's Encrypt certificate over HTTP-01 using the running web container,
// then restarts nginx with the HTTPS config.
func (a *App) IssueCert(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	if missing := a.Cfg.Missing("DOMAIN", "ADMIN_EMAIL"); len(missing) > 0 {
		return fmt.Errorf("set %s in %s", strings.Join(missing, ", "), config.EnvPath(a.Root))
	}
	domain := a.Cfg.Get("DOMAIN", "")
	args := []string{"run", "--rm", "--entrypoint", "certbot", "certbot", "certonly",
		"--webroot", "-w", "/srv/acme",
		"--cert-name", domain, "-d", domain,
		"--email", a.Cfg.Get("ADMIN_EMAIL", ""),
		"--agree-tos", "--no-eff-email", "--non-interactive", "--keep-until-expiring"}
	for _, d := range strings.Split(a.Cfg.Get("CERT_EXTRA_DOMAINS", ""), ",") {
		if d = strings.TrimSpace(d); d != "" && !strings.Contains(d, "example.com") {
			args = append(args, "-d", d)
		}
	}

	a.step("Making sure nginx is answering ACME challenges")
	if err := a.prepare(ctx); err != nil {
		return err
	}
	if err := c.Run(ctx, "up", "--detach", "web"); err != nil {
		return err
	}
	a.step("Requesting a certificate for %s", domain)
	if err := c.Run(ctx, args...); err != nil {
		return err
	}
	a.step("Switching nginx to HTTPS")
	if err := stack.RenderNginx(a.Root, true); err != nil {
		return err
	}
	return c.Run(ctx, "up", "--detach", "--force-recreate", "web")
}

// ---------------------------------------------------------------- database

func (a *App) dbAlive(ctx context.Context) bool {
	c, err := a.engine()
	if err != nil {
		return false
	}
	return c.Cmd(ctx, "exec", "-T", "db", "mariadb-admin", "--silent", "ping").Run() == nil
}

func (a *App) waitForDB(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !a.dbAlive(ctx) {
		if time.Now().After(deadline) {
			return fmt.Errorf("MariaDB did not come up within %s (check `frostfire logs db`)", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

func dumpArgs() []string {
	return engine.DBShell("exec mariadb-dump -uroot --single-transaction --quick --routines --events --triggers --flush-privileges --all-databases")
}

func loadArgs() []string {
	return engine.DBShell("exec mariadb -uroot")
}

// DBDump writes a full logical dump to path ("-" for stdout).
func (a *App) DBDump(ctx context.Context, path string) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	if !a.dbAlive(ctx) {
		return errors.New("the database container is not running (start it with `frostfire up`)")
	}
	cmd := c.Cmd(ctx, dumpArgs()...)
	cmd.Stderr = a.Stderr
	if path == "-" {
		cmd.Stdout = a.Stdout
		return cmd.Run()
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("dump failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	a.step("Wrote %s", path)
	return nil
}

// DBLoad feeds an SQL file (plain or .gz) into MariaDB. This is how you bring the
// database over from the old server.
func (a *App) DBLoad(ctx context.Context, path string, yes bool) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	if err := a.confirm(yes, fmt.Sprintf("Load %s into MariaDB? Databases in the file replace existing ones with the same name.", path)); err != nil {
		return err
	}
	in, err := openMaybeGzip(path)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := a.ensureDB(ctx); err != nil {
		return err
	}
	a.step("Loading %s", path)
	cmd := c.Cmd(ctx, loadArgs()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, a.Stdout, a.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("loading %s: %w", path, err)
	}
	a.step("Database loaded")
	return nil
}

// DBShell opens an interactive MariaDB prompt as root.
func (a *App) DBShell(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	if err := a.ensureDB(ctx); err != nil {
		return err
	}
	return c.Run(ctx, "exec", "db", "sh", "-c", `export MYSQL_PWD="$MARIADB_ROOT_PASSWORD"; exec mariadb -uroot`)
}

func (a *App) ensureDB(ctx context.Context) error {
	c, err := a.engine()
	if err != nil {
		return err
	}
	if a.dbAlive(ctx) {
		return nil
	}
	a.step("Starting MariaDB")
	if err := a.prepare(ctx); err != nil {
		return err
	}
	if err := c.Run(ctx, "up", "--detach", "db"); err != nil {
		return err
	}
	return a.waitForDB(ctx, 2*time.Minute)
}

// ---------------------------------------------------------------- helpers

func (a *App) confirm(yes bool, question string) error {
	if yes {
		return nil
	}
	if f, ok := a.Stdin.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
			return errors.New("refusing to continue without a terminal: pass --yes")
		}
	}
	fmt.Fprintf(a.Stdout, "%s\nType 'yes' to continue: ", question)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return errors.New("aborted")
	}
	return nil
}

// pipe runs src | dst and reports the first failure of either side.
func pipe(src, dst *exec.Cmd) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	src.Stdout, dst.Stdin = w, r
	if err := src.Start(); err != nil {
		r.Close()
		w.Close()
		return err
	}
	if err := dst.Start(); err != nil {
		r.Close()
		w.Close()
		src.Process.Kill()
		src.Wait()
		return err
	}
	// The children hold their own copies now.
	r.Close()
	w.Close()
	srcErr, dstErr := src.Wait(), dst.Wait()
	if srcErr != nil {
		return fmt.Errorf("%s: %w", filepath.Base(src.Path), srcErr)
	}
	if dstErr != nil {
		return fmt.Errorf("%s: %w", filepath.Base(dst.Path), dstErr)
	}
	return nil
}

const secretAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// randomSecret returns n characters that need no quoting in env files or shells.
func randomSecret(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(secretAlphabet)))
	for i := range b {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = secretAlphabet[k.Int64()]
	}
	return string(b)
}
