package stack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStack(t *testing.T) {
	for goos, want := range map[string][]string{
		"linux":   {"compose.yaml", "php/Containerfile", "php/entrypoint.sh"},
		"freebsd": {"compose.yaml", "images/db/Containerfile", "images/db/entrypoint.sh", "images/php/entrypoint.sh", "images/web/nginx.conf", "images/certbot/Containerfile"},
	} {
		root := t.TempDir()
		if err := WriteStack(root, goos); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		for _, f := range want {
			st, err := os.Stat(filepath.Join(root, "stack", f))
			if err != nil {
				t.Errorf("%s: missing %s", goos, f)
				continue
			}
			if filepath.Ext(f) == ".sh" && st.Mode()&0o111 == 0 {
				t.Errorf("%s: %s is not executable", goos, f)
			}
		}
		for _, img := range Images(goos) {
			if _, err := os.Stat(filepath.Join(ImageDir(root, img), "Containerfile")); err != nil {
				t.Errorf("%s: image %s has no Containerfile", goos, img)
			}
		}
	}
	if err := WriteStack(t.TempDir(), "plan9"); err == nil {
		t.Error("expected an error for an unsupported OS")
	}
}

func TestRenderNginx(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteConfigDefaults(root); err != nil {
		t.Fatal(err)
	}
	tpl := filepath.Join(root, "stack", "nginx", "templates")

	if err := RenderNginx(root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tpl, "site.conf.template")); err == nil {
		t.Error("HTTPS site rendered without a certificate")
	}

	if err := RenderNginx(root, true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"default.conf.template", "site.conf.template"} {
		if _, err := os.Stat(filepath.Join(tpl, f)); err != nil {
			t.Errorf("missing %s with a certificate", f)
		}
	}
}
