package restic

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// OfficialVersion is the release installed when the distro's restic is missing or too old.
const OfficialVersion = "0.18.1"

// OfficialPath is where the official release binary gets installed.
const OfficialPath = "/usr/local/bin/restic"

// FindBin picks the restic binary: an explicit override, then the official install
// (which setup only puts there when the distro package is too old), then $PATH.
func FindBin(override string) string {
	if override != "" {
		return override
	}
	if st, err := os.Stat(OfficialPath); err == nil && st.Mode()&0o111 != 0 {
		return OfficialPath
	}
	if p, err := exec.LookPath("restic"); err == nil {
		return p
	}
	return "restic"
}

// InstallOfficial downloads restic's official release for this OS and CPU, checks it
// against the release's SHA256SUMS, and installs it at dest.
func InstallOfficial(ctx context.Context, version, dest string) error {
	base := fmt.Sprintf("https://github.com/restic/restic/releases/download/v%s/", version)
	name := fmt.Sprintf("restic_%s_%s_%s.bz2", version, runtime.GOOS, runtime.GOARCH)

	sums, err := fetch(ctx, base+"SHA256SUMS")
	if err != nil {
		return err
	}
	want, err := findSum(sums, name)
	if err != nil {
		return err
	}
	archive, err := fetch(ctx, base+name)
	if err != nil {
		return err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s: refusing to install", name)
	}

	bin, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(archive)))
	if err != nil {
		return fmt.Errorf("decompressing %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// Write next to the destination and rename, so a failure never leaves half a binary.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".restic-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func findSum(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s (unsupported CPU %s?)", name, runtime.GOARCH)
}
