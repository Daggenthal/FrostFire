// Package config loads frostfire.env, the single settings file for a FrostFire install.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultRoot = "/srv/frostfire"
	EnvFileName = "frostfire.env"
)

type Config struct {
	Root string
	Vars map[string]string
}

// EnvPath is the location of frostfire.env under root.
func EnvPath(root string) string { return filepath.Join(root, EnvFileName) }

// Load reads root/frostfire.env.
func Load(root string) (*Config, error) {
	p := EnvPath(root)
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s not found (run `frostfire init` first, or pass --root)", p)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &Config{Root: root, Vars: vars}, nil
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads KEY=VALUE lines the same way compose reads an env file: blank lines and
// # comments are skipped, an optional `export ` prefix is allowed, quotes are stripped,
// and ` #` starts a trailing comment on unquoted values.
func Parse(r io.Reader) (map[string]string, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("line %d: invalid key %q", n, k)
		}
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		vars[k] = v
	}
	return vars, sc.Err()
}

// Get returns the value for key, or def when it is unset or empty.
func (c *Config) Get(key, def string) string {
	if v := c.Vars[key]; v != "" {
		return v
	}
	return def
}

// Int returns key as an integer, or def when unset.
func (c *Config) Int(key string, def int) (int, error) {
	v := c.Vars[key]
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number", key, v)
	}
	return n, nil
}

// Missing returns which of keys are unset, empty, or still a placeholder.
func (c *Config) Missing(keys ...string) []string {
	var out []string
	for _, k := range keys {
		v := c.Vars[k]
		if v == "" || v == "__GENERATED__" || strings.Contains(v, "example.com") {
			out = append(out, k)
		}
	}
	return out
}

// Environ returns every setting as KEY=VALUE, sorted, for passing to child processes.
func (c *Config) Environ() []string {
	out := make([]string, 0, len(c.Vars))
	for k, v := range c.Vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Path joins parts onto the FrostFire root.
func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.Root}, parts...)...)
}
