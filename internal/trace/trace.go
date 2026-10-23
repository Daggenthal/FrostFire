// Package trace prints every external command frostfire runs when verbose mode is on
// (--verbose, -v, or FROSTFIRE_VERBOSE=1): the command line, extra environment
// variable names, how it ended, and how long it took. Secrets never appear: frostfire
// passes passwords through the environment, and only variable names are printed.
package trace

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	enabled bool
	out     io.Writer = os.Stderr
	start             = time.Now()
)

// Enable turns tracing on and sends it to w.
func Enable(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	enabled, out = true, w
}

// Enabled reports whether tracing is on.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// Logf prints a trace line.
func Logf(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !enabled {
		return
	}
	fmt.Fprintf(out, "[%s +%6.1fs] %s\n", time.Now().Format("15:04:05"),
		time.Since(start).Seconds(), fmt.Sprintf(format, args...))
}

// Run runs cmd, tracing it.
func Run(cmd *exec.Cmd) error {
	begin(cmd)
	t := time.Now()
	err := cmd.Run()
	end(cmd, err, t)
	return err
}

// Output runs cmd and returns its stdout, tracing it.
func Output(cmd *exec.Cmd) ([]byte, error) {
	begin(cmd)
	t := time.Now()
	b, err := cmd.Output()
	end(cmd, err, t)
	return b, err
}

// CombinedOutput runs cmd and returns stdout and stderr, tracing it.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	begin(cmd)
	t := time.Now()
	b, err := cmd.CombinedOutput()
	end(cmd, err, t)
	return b, err
}

// Start starts cmd, tracing it. Pair it with Wait.
func Start(cmd *exec.Cmd) error {
	begin(cmd)
	return cmd.Start()
}

// Wait waits for a command started with Start, tracing how it ended.
func Wait(cmd *exec.Cmd, started time.Time) error {
	err := cmd.Wait()
	end(cmd, err, started)
	return err
}

func begin(cmd *exec.Cmd) {
	if !Enabled() {
		return
	}
	line := "$ " + Quote(cmd.Args)
	if extra := extraEnv(cmd); extra != "" {
		line += "   (env: " + extra + ")"
	}
	if cmd.Dir != "" {
		line += "   (in " + cmd.Dir + ")"
	}
	Logf("%s", line)
}

func end(cmd *exec.Cmd, err error, t time.Time) {
	if !Enabled() {
		return
	}
	d := time.Since(t).Round(10 * time.Millisecond)
	name := cmd.Args[0]
	if len(cmd.Args) > 1 {
		name += " " + cmd.Args[1]
	}
	switch {
	case err == nil:
		Logf("  ok: %s (%s)", name, d)
	default:
		Logf("  FAILED: %s: %v (%s)", name, err, d)
	}
}

// extraEnv lists the names of variables set on cmd beyond the inherited environment.
func extraEnv(cmd *exec.Cmd) string {
	if cmd.Env == nil {
		return ""
	}
	inherited := map[string]bool{}
	for _, kv := range os.Environ() {
		inherited[kv] = true
	}
	var names []string
	for _, kv := range cmd.Env {
		if inherited[kv] {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	if len(names) > 12 {
		return fmt.Sprintf("%s and %d more", strings.Join(names[:12], ","), len(names)-12)
	}
	return strings.Join(names, ",")
}

// Quote renders argv the way you'd type it in a shell.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"$`\\;&|<>*?()[]{}#~!") {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
