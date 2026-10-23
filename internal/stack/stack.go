// Package stack holds the container stack definition embedded in the frostfire
// binary and writes it into the FrostFire root, so the binary is all you need to deploy.
package stack

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

//go:embed all:files
var files embed.FS

// EnvExample returns the template for frostfire.env.
func EnvExample() []byte {
	b, err := files.ReadFile("files/frostfire.env.example")
	if err != nil {
		panic(err) // embedded at build time, cannot be missing
	}
	return b
}

// WriteStack replaces root/stack with the embedded stack files. That directory is
// owned by frostfire, so anything edited there by hand is overwritten.
func WriteStack(root string) error {
	dir := filepath.Join(root, "stack")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return copyTree("files/stack", dir, true)
}

// WriteConfigDefaults copies the default config files into root/config, skipping
// any that already exist. It returns the paths it created.
func WriteConfigDefaults(root string) ([]string, error) {
	var created []string
	err := fs.WalkDir(files, "files/config", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("files", filepath.FromSlash(p))
		dst := filepath.Join(root, rel)
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		data, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		created = append(created, dst)
		return nil
	})
	return created, err
}

// CertPath is where the live certificate for domain lives under root.
func CertPath(root, domain string) string {
	return filepath.Join(root, "certs", "live", domain, "fullchain.pem")
}

// RenderNginx writes the nginx templates the web container will load. With a
// certificate it serves the site over HTTPS and redirects port 80; without one
// it only answers ACME challenges so `frostfire certs issue` can get a cert.
func RenderNginx(root string, haveCert bool) error {
	dir := filepath.Join(root, "stack", "nginx", "templates")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	http := "files/nginx/bootstrap.conf.template"
	if haveCert {
		http = "files/nginx/redirect.conf.template"
	}
	data, err := files.ReadFile(http)
	if err != nil {
		return err
	}
	// Named default.conf so it replaces the stock default server in the nginx image.
	if err := os.WriteFile(filepath.Join(dir, "default.conf.template"), data, 0o644); err != nil {
		return err
	}

	if !haveCert {
		return nil
	}
	site, err := os.ReadFile(filepath.Join(root, "config", "nginx", "site.conf.template"))
	if err != nil {
		return fmt.Errorf("reading your site config: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "site.conf.template"), site, 0o644)
}

func copyTree(src, dst string, overwrite bool) error {
	return fs.WalkDir(files, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := p[len(src):]
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !overwrite {
			if _, err := os.Stat(target); err == nil {
				return nil
			}
		}
		data, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if path.Ext(p) == ".sh" || bytes.HasPrefix(data, []byte("#!")) {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
}
