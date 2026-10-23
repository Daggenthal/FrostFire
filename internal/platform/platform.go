// Package platform knows how each supported OS installs packages. This is the one
// place host differences live: adding an OS means adding one entry to families.
package platform

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/Daggenthal/FrostFire/internal/config"
)

// Platform describes how to install software on one OS family.
type Platform struct {
	Name     string            // what the OS calls itself, e.g. "Ubuntu 26.04 LTS"
	Family   string            // debian, fedora, rhel, arch, suse, alpine, freebsd
	Refresh  []string          // refresh package lists before installing (optional)
	Prepare  [][]string        // extra one-time steps, e.g. enabling EPEL (optional)
	Install  []string          // install command; package names are appended
	Env      []string          // extra environment for package commands
	Packages map[string]string // tool name -> package name
}

type family struct {
	matches  []string // os-release IDs that belong to this family
	platform Platform
}

// Tools frostfire may need to install.
const (
	ToolPodman  = "podman"
	ToolCompose = "podman-compose"
	ToolRestic  = "restic"
)

var families = []family{
	{[]string{"debian", "ubuntu", "raspbian"}, Platform{
		Family:   "debian",
		Refresh:  []string{"apt-get", "update"},
		Install:  []string{"apt-get", "install", "-y"},
		Env:      []string{"DEBIAN_FRONTEND=noninteractive"},
		Packages: map[string]string{ToolPodman: "podman", ToolCompose: "podman-compose", ToolRestic: "restic"},
	}},
	{[]string{"fedora"}, Platform{
		Family:   "fedora",
		Install:  []string{"dnf", "install", "-y"},
		Packages: map[string]string{ToolPodman: "podman", ToolCompose: "podman-compose", ToolRestic: "restic"},
	}},
	{[]string{"rhel", "centos", "rocky", "almalinux", "ol"}, Platform{
		Family: "rhel",
		// podman-compose and restic live in EPEL on the RHEL family.
		Prepare:  [][]string{{"dnf", "install", "-y", "epel-release"}},
		Install:  []string{"dnf", "install", "-y"},
		Packages: map[string]string{ToolPodman: "podman", ToolCompose: "podman-compose", ToolRestic: "restic"},
	}},
	{[]string{"arch", "archarm", "manjaro", "endeavouros"}, Platform{
		Family:   "arch",
		Install:  []string{"pacman", "-S", "--needed", "--noconfirm"},
		Packages: map[string]string{ToolPodman: "podman", ToolCompose: "podman-compose", ToolRestic: "restic"},
	}},
	{[]string{"suse", "opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles"}, Platform{
		Family:   "suse",
		Refresh:  []string{"zypper", "--non-interactive", "refresh"},
		Install:  []string{"zypper", "--non-interactive", "install"},
		Packages: map[string]string{ToolPodman: "podman", ToolCompose: "podman-compose", ToolRestic: "restic"},
	}},
	{[]string{"alpine"}, Platform{
		Family:   "alpine",
		Install:  []string{"apk", "add", "--no-cache"},
		Packages: map[string]string{ToolPodman: "podman", ToolCompose: "podman-compose", ToolRestic: "restic"},
	}},
}

// ErrFreeBSD is returned on FreeBSD until its backend lands.
var ErrFreeBSD = errors.New("FreeBSD is detected, but its backend isn't ready yet (Linux first, FreeBSD next)")

// Detect identifies the running OS.
func Detect() (Platform, error) {
	if runtime.GOOS == "freebsd" {
		return Platform{Name: "FreeBSD", Family: "freebsd"}, ErrFreeBSD
	}
	if runtime.GOOS != "linux" {
		return Platform{}, fmt.Errorf("%s is not supported (FrostFire runs on Linux, FreeBSD is next)", runtime.GOOS)
	}
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		f, err := os.Open(p)
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
				if id == m {
					p := fam.platform
					p.Name = name
					return p, nil
				}
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
