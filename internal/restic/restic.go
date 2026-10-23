// Package restic wraps the restic CLI. restic does the encryption, deduplication
// and transport; frostfire decides what goes in and how runs are tagged.
package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Daggenthal/FrostFire/internal/trace"
)

// MinVersion is the oldest restic with `backup --stdin-from-command`, which lets a
// failed database dump fail the snapshot instead of saving a truncated one.
var MinVersion = [3]int{0, 17, 0}

type Restic struct {
	Bin    string
	Env    []string // settings passed to restic (RESTIC_REPOSITORY, RESTIC_PASSWORD, backend creds)
	Stdout io.Writer
	Stderr io.Writer
}

type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
}

// HasTag reports whether the snapshot carries tag.
func (s Snapshot) HasTag(tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Cmd builds a restic command with frostfire's settings in its environment.
func (r *Restic) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = append(os.Environ(), r.Env...)
	return cmd
}

func (r *Restic) run(ctx context.Context, args ...string) error {
	cmd := r.Cmd(ctx, args...)
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	if err := trace.Run(cmd); err != nil {
		return fmt.Errorf("restic %s: %w", args[0], err)
	}
	return nil
}

// Version returns restic's version as [major, minor, patch].
func (r *Restic) Version(ctx context.Context) ([3]int, error) {
	var v [3]int
	out, err := trace.Output(r.Cmd(ctx, "version"))
	if err != nil {
		return v, fmt.Errorf("running %s version: %w", r.Bin, err)
	}
	// "restic 0.18.0 compiled with go1.24.1 on linux/amd64"
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return v, fmt.Errorf("unexpected restic version output %q", out)
	}
	return ParseVersion(fields[1])
}

// ParseVersion parses "0.18.0" (optionally with a -dev suffix).
func ParseVersion(s string) ([3]int, error) {
	var v [3]int
	s, _, _ = strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("unexpected version %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, fmt.Errorf("unexpected version %q", s)
		}
		v[i] = n
	}
	return v, nil
}

// AtLeast reports whether v >= min.
func AtLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// Init creates a new repository.
func (r *Restic) Init(ctx context.Context) error { return r.run(ctx, "init") }

// RepoReachable checks the repository exists and the password opens it.
func (r *Restic) RepoReachable(ctx context.Context) error {
	out, err := trace.CombinedOutput(r.Cmd(ctx, "cat", "config"))
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(lastLine(string(out))))
	}
	return nil
}

// BackupPaths snapshots files and directories.
func (r *Restic) BackupPaths(ctx context.Context, tags, paths []string) error {
	args := append([]string{"backup", "--tag", strings.Join(tags, ","), "--exclude-caches"}, paths...)
	return r.run(ctx, args...)
}

// BackupCommand snapshots the stdout of argv as a single file named filename.
// If argv exits non-zero, restic discards the snapshot.
func (r *Restic) BackupCommand(ctx context.Context, tags []string, filename string, argv []string) error {
	args := []string{"backup", "--tag", strings.Join(tags, ","),
		"--stdin-filename", filename, "--stdin-from-command", "--"}
	return r.run(ctx, append(args, argv...)...)
}

// Snapshots lists snapshots that carry every one of tags.
func (r *Restic) Snapshots(ctx context.Context, tags ...string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	cmd := r.Cmd(ctx, args...)
	cmd.Stderr = r.Stderr
	out, err := trace.Output(cmd)
	if err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("parsing restic snapshots: %w", err)
	}
	return snaps, nil
}

// Restore restores a snapshot into target. Absolute paths in the snapshot are
// recreated below target, so /srv/frostfire/www lands in target/srv/frostfire/www.
func (r *Restic) Restore(ctx context.Context, id, target string) error {
	return r.run(ctx, "restore", id, "--target", target)
}

// DumpCmd returns a command that writes one file from a snapshot to stdout.
func (r *Restic) DumpCmd(ctx context.Context, id, filename string) *exec.Cmd {
	return r.Cmd(ctx, "dump", id, filename)
}

// Keep is a retention policy.
type Keep struct{ Daily, Weekly, Monthly int }

// Forget applies keep to snapshots tagged with tag and prunes unreferenced data.
func (r *Restic) Forget(ctx context.Context, tag string, keep Keep) error {
	return r.run(ctx, "forget", "--tag", tag,
		"--keep-daily", strconv.Itoa(keep.Daily),
		"--keep-weekly", strconv.Itoa(keep.Weekly),
		"--keep-monthly", strconv.Itoa(keep.Monthly),
		"--prune")
}

// Check verifies repository integrity, reading a sample of the data as well.
func (r *Restic) Check(ctx context.Context) error {
	return r.run(ctx, "check", "--read-data-subset", "5%")
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
