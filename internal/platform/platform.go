// Package platform knows how each supported OS installs packages. This is the one
// place host differences live: adding an OS means adding one entry to families.
package platform

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Daggenthal/FrostFire/internal/config"
	"github.com/Daggenthal/FrostFire/internal/trace"
)

// Tools frostfire may need to install.
const (
	ToolPodman        = "podman"
	ToolPodmanCompose = "podman-compose"
	ToolDocker        = "docker"
	ToolDockerCompose = "docker-compose"
	ToolRestic        = "restic"
)

// Platform describes how to install software on one OS family.
type Platform struct {
	Name     string            // what the OS calls itself, e.g. "Ubuntu 26.04 LTS"
	Family   string            // debian, fedora, rhel, arch, suse, alpine, freebsd
	Refresh  []string          // refresh package lists before installing (optional)
	Prepare  [][]string        // extra one-time steps, e.g. enabling EPEL (optional)
	Install  []string          // install command; package names are appended
	Env      []string          // extra environment for package commands
	Packages map[string]string // tool -> package; a missing tool isn't packaged here
	Init     string            // service manager: systemd, openrc, rc (FreeBSD)
}

// Has reports whether tool can be installed from this platform's packages.
func (p Platform) Has(tool string) bool { return p.Packages[tool] != "" }

type family struct {
	matches  []string // os-release IDs that belong to this family
	platform Platform
}

func pkgs(extra map[string]string) map[string]string {
	m := map[string]string{ToolPodman: "podman", ToolPodmanCompose: "podman-compose", ToolRestic: "restic"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var families = []family{
	{[]string{"debian", "ubuntu", "raspbian"}, Platform{
		Family:   "debian",
		Refresh:  []string{"apt-get", "update"},
		Install:  []string{"apt-get", "install", "-y"},
		Env:      []string{"DEBIAN_FRONTEND=noninteractive"},
		Packages: pkgs(map[string]string{ToolDocker: "docker.io", ToolDockerCompose: "docker-compose"}),
		Init:     "systemd",
	}},
	{[]string{"fedora"}, Platform{
		Family:   "fedora",
		Install:  []string{"dnf", "install", "-y"},
		Packages: pkgs(map[string]string{ToolDocker: "moby-engine", ToolDockerCompose: "docker-compose"}),
		Init:     "systemd",
	}},
	{[]string{"rhel", "centos", "rocky", "almalinux", "ol"}, Platform{
		Family: "rhel",
		// podman-compose and restic live in EPEL. Docker isn't packaged on the RHEL family.
		Prepare:  [][]string{{"dnf", "install", "-y", "epel-release"}},
		Install:  []string{"dnf", "install", "-y"},
		Packages: pkgs(nil),
		Init:     "systemd",
	}},
	{[]string{"arch", "archarm", "manjaro", "endeavouros"}, Platform{
		Family:   "arch",
		Install:  []string{"pacman", "-S", "--needed", "--noconfirm"},
		Packages: pkgs(map[string]string{ToolDocker: "docker", ToolDockerCompose: "docker-compose"}),
		Init:     "systemd",
	}},
	{[]string{"suse", "opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles"}, Platform{
		Family:   "suse",
		Refresh:  []string{"zypper", "--non-interactive", "refresh"},
		Install:  []string{"zypper", "--non-interactive", "install"},
		Packages: pkgs(map[string]string{ToolDocker: "docker", ToolDockerCompose: "docker-compose"}),
		Init:     "systemd",
	}},
	{[]string{"alpine"}, Platform{
		Family:   "alpine",
		Install:  []string{"apk", "add", "--no-cache"},
		Packages: pkgs(map[string]string{ToolDocker: "docker", ToolDockerCompose: "docker-cli-compose"}),
		Init:     "openrc",
	}},
}

// FreeBSD installs by port origin, so the current Python flavor of podman-compose
// (py311, py312, ...) is picked automatically instead of being hardcoded.
var freebsd = Platform{
	Family:  "freebsd",
	Install: []string{"pkg", "install", "-y"},
	Env:     []string{"ASSUME_ALWAYS_YES=yes"},
	Packages: map[string]string{
		ToolPodman:        "sysutils/podman-suite",
		ToolPodmanCompose: "sysutils/podman-compose",
		ToolRestic:        "sysutils/restic",
	},
	Init: "rc",
}

// ForFreeBSD returns the FreeBSD platform (for planning on other OSes, e.g. in tests).
func ForFreeBSD() Platform {
	p := freebsd
	p.Name = "FreeBSD"
	return p
}

// Detect identifies the running OS.
func Detect() (Platform, error) {
	switch runtime.GOOS {
	case "freebsd":
		p := freebsd
		p.Name = "FreeBSD"
		if out, err := trace.Output(exec.Command("freebsd-version")); err == nil {
			p.Name += " " + strings.TrimSpace(string(out))
		}
		return p, nil
	case "linux":
	default:
		return Platform{}, fmt.Errorf("%s is not supported (FrostFire runs on Linux and FreeBSD)", runtime.GOOS)
	}
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		return FromOSRelease(f)
	}
	return Platform{}, errors.New("can't identify this Linux distribution: no /etc/os-release")
}

// FromOSRelease picks a platform from os-release contents. ID is tried first, then
// each ID_LIKE entry, so derivatives (Pop!_OS, Linux Mint, AlmaLinux, ...) resolve
// to their parent family instead of needing their own entry.
func FromOSRelease(r io.Reader) (Platform, error) {
	vars, err := config.Parse(r)
	if err != nil {
		return Platform{}, fmt.Errorf("reading os-release: %w", err)
	}
	name := vars["PRETTY_NAME"]
	if name == "" {
		name = vars["NAME"]
	}
	ids := append([]string{vars["ID"]}, strings.Fields(vars["ID_LIKE"])...)
	for _, id := range ids {
		for _, fam := range families {
			for _, m := range fam.matches {
				if id != m {
					continue
				}
				p := fam.platform
				p.Name = name
				p.Packages = pkgs(p.Packages) // copy, so tweaks don't leak into the table
				// Ubuntu's plain docker-compose is the old Python 1.x; v2 has its own package.
				if p.Family == "debian" && contains(ids, "ubuntu") {
					p.Packages[ToolDockerCompose] = "docker-compose-v2"
				}
				return p, nil
			}
		}
	}
	return Platform{Name: name}, fmt.Errorf("%s is not supported yet (ID=%q ID_LIKE=%q): install podman, podman-compose and restic by hand",
		name, vars["ID"], vars["ID_LIKE"])
}

// InstallCommands returns the commands that install tools, in order.
func (p Platform) InstallCommands(tools []string) [][]string {
	if len(tools) == 0 {
		return nil
	}
	var cmds [][]string
	if p.Refresh != nil {
		cmds = append(cmds, p.Refresh)
	}
	cmds = append(cmds, p.Prepare...)
	install := append([]string{}, p.Install...)
	for _, t := range tools {
		install = append(install, p.Packages[t])
	}
	return append(cmds, install)
}

// FreeBSDImageVersion returns the freebsd-runtime image tag to build from. A jail
// can't run a newer userland than the host kernel, so it follows the host:
// 15.0-RELEASE-p3 uses "15.0", and -STABLE or -CURRENT use the "15.snap" snapshots.
func FreeBSDImageVersion(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	out, err := trace.Output(exec.Command("freebsd-version", "-k"))
	if err != nil {
		return "", fmt.Errorf("running freebsd-version: %w", err)
	}
	return ParseFreeBSDVersion(strings.TrimSpace(string(out)))
}

// ParseFreeBSDVersion maps a freebsd-version string to an image tag.
func ParseFreeBSDVersion(v string) (string, error) {
	num, branch, ok := strings.Cut(v, "-")
	major, _, hasMinor := strings.Cut(num, ".")
	if !ok || !hasMinor || major == "" {
		return "", fmt.Errorf("unexpected FreeBSD version %q (set FREEBSD_IMAGE_VERSION in frostfire.env)", v)
	}
	if strings.HasPrefix(branch, "RELEASE") {
		return num, nil
	}
	return major + ".snap", nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
