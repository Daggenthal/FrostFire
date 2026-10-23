// Package engine runs `compose` commands against the FrostFire stack using Podman or Docker.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Detect picks the container engine. An explicit override wins; otherwise the first of
// podman and docker that has a working `compose` subcommand is used.
func Detect(override string) (string, error) {
	if override != "" {
		bin, err := exec.LookPath(override)
		if err != nil {
			return "", fmt.Errorf("FROSTFIRE_ENGINE=%s: %w", override, err)
		}
		return bin, nil
	}
	names := []string{"podman", "docker"}
	if runtime.GOOS == "freebsd" {
		names = []string{"podman"} // Docker doesn't run on FreeBSD
	}
	for _, name := range names {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if exec.Command(bin, "compose", "version").Run() == nil {
			return bin, nil
		}
	}
	if runtime.GOOS == "freebsd" {
		return "", errors.New("no working `podman compose` found: run `frostfire setup`")
	}
	return "", errors.New("no container engine with compose support found: run `frostfire setup` (or install podman with podman-compose, or docker with the compose plugin)")
}

type Compose struct {
	Bin    string   // path to podman or docker
	Root   string   // FrostFire root
	Env    []string // extra environment for compose and build commands
	Stdout io.Writer
	Stderr io.Writer
}

// Argv returns the full command line for a compose subcommand. Relative paths in
// compose.yaml resolve against its own directory, so no --project-directory is
// needed (podman-compose doesn't support it).
func (c *Compose) Argv(args ...string) []string {
	base := []string{
		c.Bin, "compose",
		"--project-name", "frostfire",
		"--file", filepath.Join(c.Root, "stack", "compose.yaml"),
		"--env-file", filepath.Join(c.Root, "frostfire.env"),
	}
	return append(base, args...)
}

// Cmd builds a compose command with no stdio attached.
func (c *Compose) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	argv := c.Argv(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	return cmd
}

// Run runs a compose command with output going to the terminal.
func (c *Compose) Run(ctx context.Context, args ...string) error {
	cmd := c.Cmd(ctx, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compose %v: %w", args, err)
	}
	return nil
}

// Build builds a local image with the engine directly. On FreeBSD --no-hosts is
// required: Podman's generated /etc/hosts makes pkg fail with "Cross-device link".
func (c *Compose) Build(ctx context.Context, tag, dir string, buildArgs map[string]string) error {
	args := []string{"build", "--no-hosts", "--tag", tag}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	cmd := exec.CommandContext(ctx, c.Bin, append(args, dir)...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building %s: %w", tag, err)
	}
	return nil
}

// DBShell returns compose args that run a shell snippet inside the db container
// with MYSQL_PWD set from the container's own root password. The password never
// appears on a command line, on the host or in the container.
func DBShell(snippet string) []string {
	return []string{"exec", "-T", "db", "sh", "-c",
		`export MYSQL_PWD="$MARIADB_ROOT_PASSWORD"; ` + snippet}
}
