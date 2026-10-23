package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Daggenthal/FrostFire/internal/engine"
	"github.com/Daggenthal/FrostFire/internal/platform"
	"github.com/Daggenthal/FrostFire/internal/restic"
)

// SetupOptions controls `frostfire setup`.
type SetupOptions struct {
	Engine string // "", "podman" or "docker". Empty keeps a working engine, else installs Podman.
	Yes    bool
	DryRun bool
}

// Host is what setup needs to look at on the machine. The real one runs commands;
// tests use a fake so every branch of the plan can be checked without root.
type Host interface {
	LookPath(file string) (string, error)
	Exists(path string) bool
	Probe(argv ...string) bool // runs argv, reports whether it exited 0
	Output(argv ...string) (string, error)
	ReadFile(path string) ([]byte, error)
}

type realHost struct{}

func (realHost) LookPath(f string) (string, error) { return exec.LookPath(f) }
func (realHost) Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
func (realHost) Probe(argv ...string) bool { return exec.Command(argv[0], argv[1:]...).Run() == nil }
func (realHost) Output(argv ...string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	return string(out), err
}
func (realHost) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

type setupStep struct {
	desc string
	argv []string
	env  []string
	fn   func(context.Context) error // in-process step, used instead of argv
}

// Setup installs what FrostFire needs on this machine and leaves anything that
// already works alone, so it's safe to run again.
func (a *App) Setup(ctx context.Context, opt SetupOptions) error {
	p, err := platform.Detect()
	if err != nil {
		return err
	}
	a.step("Detected %s", p.Name)
	if !opt.DryRun && os.Geteuid() != 0 {
		return errors.New("setup installs system packages, run it as root (or with --dry-run to see the plan)")
	}

	steps, err := a.planSetup(realHost{}, p, opt)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		a.step("Everything is already in place")
		return a.setupNext()
	}

	fmt.Fprintln(a.Stdout, "\nPlan:")
	for _, s := range steps {
		if s.argv != nil {
			fmt.Fprintf(a.Stdout, "  - %s: %s\n", s.desc, strings.Join(s.argv, " "))
		} else {
			fmt.Fprintf(a.Stdout, "  - %s\n", s.desc)
		}
	}
	fmt.Fprintln(a.Stdout)
	if p.Family == "freebsd" {
		a.warn("starting pf can drop SSH sessions for a moment: run this inside tmux or screen")
	}
	if opt.DryRun {
		a.step("Dry run: nothing was changed")
		return nil
	}
	if err := a.confirm(opt.Yes, "Run these steps?"); err != nil {
		return err
	}

	for _, s := range steps {
		a.step("%s", s.desc)
		if s.fn != nil {
			if err := s.fn(ctx); err != nil {
				return fmt.Errorf("%s: %w", s.desc, err)
			}
			continue
		}
		cmd := exec.CommandContext(ctx, s.argv[0], s.argv[1:]...)
		cmd.Env = append(os.Environ(), s.env...)
		cmd.Stdout, cmd.Stderr = a.Stdout, a.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(s.argv, " "), err)
		}
	}

	// Verify instead of assuming it worked.
	bin, err := engine.Detect(a.wantedEngine(opt))
	if err != nil {
		return fmt.Errorf("setup ran, but there's still no working compose: %w", err)
	}
	a.step("Container engine ready: %s compose", bin)
	if ok, detail := a.resticStatus(realHost{}); !ok {
		return fmt.Errorf("restic still not usable: %s", detail)
	} else {
		a.step("restic ready: %s", detail)
	}
	return a.setupNext()
}

func (a *App) wantedEngine(opt SetupOptions) string {
	if opt.Engine != "" {
		return opt.Engine
	}
	return a.engineOverride()
}

// planSetup works out every step setup would take on this host.
func (a *App) planSetup(h Host, p platform.Platform, opt SetupOptions) ([]setupStep, error) {
	var tools []string

	// Container engine.
	want := a.wantedEngine(opt)
	switch {
	case p.Family == "freebsd" && want == "docker":
		return nil, errors.New("Docker doesn't run on FreeBSD: FrostFire uses Podman there")
	case want != "" && want != "podman" && want != "docker":
		return nil, fmt.Errorf("unknown engine %q (use podman or docker)", want)
	}
	engineName, engineReady := detectEngine(h, want)
	if engineReady {
		a.step("Container engine: %s compose (already installed)", engineName)
	} else {
		engineName = "podman"
		if want == "docker" {
			engineName = "docker"
		}
		if engineName == "docker" {
			if !p.Has(platform.ToolDocker) {
				return nil, fmt.Errorf("%s doesn't package Docker: use Podman (the default), or install Docker CE from docker.com first", p.Name)
			}
			if _, err := h.LookPath("docker"); err != nil {
				tools = append(tools, platform.ToolDocker)
			}
			tools = append(tools, platform.ToolDockerCompose)
		} else {
			if _, err := h.LookPath("podman"); err != nil {
				tools = append(tools, platform.ToolPodman)
			}
			tools = append(tools, platform.ToolPodmanCompose)
		}
	}

	// restic.
	resticOK, detail := a.resticStatus(h)
	if resticOK {
		a.step("restic: %s (already installed)", detail)
	} else if _, err := h.LookPath("restic"); err != nil {
		tools = append(tools, platform.ToolRestic)
	}

	var steps []setupStep
	for _, argv := range p.InstallCommands(tools) {
		desc := "Install packages"
		switch {
		case strings.Join(argv, " ") == strings.Join(p.Refresh, " "):
			desc = "Refresh package lists"
		case len(argv) < len(p.Install)+len(tools):
			desc = "Enable extra repositories"
		}
		steps = append(steps, setupStep{desc: desc, argv: argv, env: p.Env})
	}
	if !resticOK {
		steps = append(steps, setupStep{
			desc: fmt.Sprintf("Make sure restic is %d.%d+ (installs the official %s release to %s if the packaged one is too old)",
				restic.MinVersion[0], restic.MinVersion[1], restic.OfficialVersion, restic.OfficialPath),
			fn: func(ctx context.Context) error { return a.ensureRestic(ctx) },
		})
	}

	if p.Family == "freebsd" {
		steps = append(steps, freebsdHostSteps(h)...)
	}
	steps = append(steps, a.bootSteps(h, p, engineName)...)
	if p.Family != "freebsd" {
		steps = append(steps, linuxFirewallSteps(h)...)
	}
	return steps, nil
}

// detectEngine finds a working compose. With no preference, Podman wins over Docker.
func detectEngine(h Host, want string) (string, bool) {
	names := []string{"podman", "docker"}
	if want != "" {
		names = []string{want}
	}
	for _, n := range names {
		bin, err := h.LookPath(n)
		if err == nil && h.Probe(bin, "compose", "version") {
			return n, true
		}
	}
	return "", false
}

func (a *App) setupNext() error {
	if _, err := os.Stat(a.Root); err != nil {
		fmt.Fprintln(a.Stdout, "\nNext: frostfire init")
	} else {
		fmt.Fprintln(a.Stdout, "\nNext: frostfire doctor")
	}
	return nil
}

// engineOverride reads FROSTFIRE_ENGINE from frostfire.env if there is one yet.
func (a *App) engineOverride() string {
	if a.load() == nil {
		return a.Cfg.Get("FROSTFIRE_ENGINE", "")
	}
	return os.Getenv("FROSTFIRE_ENGINE")
}

func (a *App) resticStatus(h Host) (bool, string) {
	override := os.Getenv("RESTIC_BIN")
	if a.load() == nil {
		override = a.Cfg.Get("RESTIC_BIN", override)
	}
	bin := restic.FindBin(override)
	out, err := h.Output(bin, "version")
	if err != nil {
		return false, "not installed"
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return false, "unrecognized restic version output"
	}
	v, err := restic.ParseVersion(fields[1])
	if err != nil {
		return false, err.Error()
	}
	detail := fmt.Sprintf("%d.%d.%d at %s", v[0], v[1], v[2], bin)
	if !restic.AtLeast(v, restic.MinVersion) {
		return false, detail + " (too old)"
	}
	return true, detail
}

func (a *App) ensureRestic(ctx context.Context) error {
	if ok, _ := a.resticStatus(realHost{}); ok {
		return nil
	}
	return restic.InstallOfficial(ctx, restic.OfficialVersion, restic.OfficialPath)
}

// bootSteps makes the containers come back after a reboot. Containers use
// restart: always, which is what each OS's Podman boot service looks for.
func (a *App) bootSteps(h Host, p platform.Platform, engineName string) []setupStep {
	switch {
	case p.Family == "freebsd":
		if out, _ := h.Output("sysrc", "-n", "podman_enable"); strings.TrimSpace(out) == "YES" {
			return nil
		}
		return []setupStep{{desc: "Restart containers on boot", argv: []string{"sysrc", "podman_enable=YES"}}}

	case h.Exists("/run/systemd/system"):
		if engineName == "docker" {
			if h.Probe("systemctl", "is-enabled", "--quiet", "docker.service") {
				return nil
			}
			return []setupStep{{desc: "Start Docker on boot", argv: []string{"systemctl", "enable", "--now", "docker.service"}}}
		}
		// Podman has no daemon. podman-restart.service starts restart: always containers at boot.
		if h.Probe("systemctl", "is-enabled", "--quiet", "podman-restart.service") {
			return nil
		}
		return []setupStep{{desc: "Restart containers on boot", argv: []string{"systemctl", "enable", "podman-restart.service"}}}

	case p.Init == "openrc" && h.Exists("/sbin/rc-update"):
		svc := "podman"
		if engineName == "docker" {
			svc = "docker"
		}
		return []setupStep{
			{desc: "Start " + svc + " on boot", argv: []string{"rc-update", "add", svc, "default"}},
			{desc: "Start " + svc + " now", argv: []string{"rc-service", svc, "start"}},
		}
	}
	a.warn("no systemd or OpenRC found: make sure your container engine starts on boot")
	return nil
}

// linuxFirewallSteps opens 80 and 443 if firewalld or ufw is active.
func linuxFirewallSteps(h Host) []setupStep {
	if _, err := h.LookPath("firewall-cmd"); err == nil && h.Probe("firewall-cmd", "--state") {
		return []setupStep{
			{desc: "Open HTTP and HTTPS in firewalld", argv: []string{"firewall-cmd", "--permanent", "--add-service=http", "--add-service=https"}},
			{desc: "Reload firewalld", argv: []string{"firewall-cmd", "--reload"}},
		}
	}
	if _, err := h.LookPath("ufw"); err == nil {
		if out, err := h.Output("ufw", "status"); err == nil && strings.Contains(out, "Status: active") {
			return []setupStep{
				{desc: "Open HTTP in ufw", argv: []string{"ufw", "allow", "80/tcp"}},
				{desc: "Open HTTPS in ufw", argv: []string{"ufw", "allow", "443/tcp"}},
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- FreeBSD host prep

const (
	fdescFstabLine  = "fdescfs\t/dev/fd\tfdescfs\trw\t0\t0"
	containersDir   = "/var/db/containers"
	storageConf     = "/usr/local/etc/containers/storage.conf"
	pfConf          = "/etc/pf.conf"
	filterLocalLine = "net.pf.filter_local=1"
)

// freebsdHostSteps prepares FreeBSD for Podman the way the Handbook describes:
// fdescfs, container storage (ZFS if available), pf NAT with Podman's anchors, and
// net.pf.filter_local. Each step is skipped when it's already done.
func freebsdHostSteps(h Host) []setupStep {
	var steps []setupStep

	// fdescfs: Podman needs /dev/fd.
	if mounts, _ := h.Output("mount", "-t", "fdescfs"); !strings.Contains(mounts, "/dev/fd") {
		steps = append(steps, setupStep{desc: "Mount fdescfs on /dev/fd", argv: []string{"mount", "-t", "fdescfs", "fdesc", "/dev/fd"}})
	}
	if fstab, _ := h.ReadFile("/etc/fstab"); !hasFstabEntry(string(fstab)) {
		steps = append(steps, setupStep{desc: "Mount fdescfs at boot (/etc/fstab)", fn: func(context.Context) error {
			return appendLine("/etc/fstab", fdescFstabLine)
		}})
	}

	// Storage: a ZFS dataset gives copy-on-write image layers and snapshots for free.
	if pool := zfsPool(h); pool != "" {
		if mps, _ := h.Output("zfs", "list", "-H", "-o", "mountpoint"); !containsLine(mps, containersDir) {
			ds := pool + "/containers"
			steps = append(steps, setupStep{desc: "Create ZFS dataset " + ds + " for container storage",
				argv: []string{"zfs", "create", "-o", "mountpoint=" + containersDir, ds}})
		}
	} else {
		steps = append(steps, setupStep{desc: "No ZFS: switch Podman storage to the vfs driver", fn: func(context.Context) error {
			return useVFSStorage()
		}})
	}

	// pf: Podman's NAT and port redirects live in pf anchors.
	pfReady := false
	if conf, err := h.ReadFile(pfConf); err == nil && strings.Contains(string(conf), "cni-rdr") {
		pfReady = true
	}
	if !pfReady {
		steps = append(steps, setupStep{desc: "Configure pf for Podman (" + pfConf + ")", fn: func(context.Context) error {
			return ensurePFConf(realHost{})
		}})
	}
	if !h.Probe("service", "pf", "enabled") {
		steps = append(steps, setupStep{desc: "Enable pf at boot", argv: []string{"sysrc", "pf_enable=YES"}})
	}
	pfRunning := h.Probe("service", "pf", "status")
	switch {
	case !pfRunning:
		steps = append(steps, setupStep{desc: "Start pf", argv: []string{"service", "pf", "start"}})
	case !pfReady:
		steps = append(steps, setupStep{desc: "Reload pf with the new rules", argv: []string{"service", "pf", "reload"}})
	}

	// Let published ports work for connections that start on the host itself.
	if out, _ := h.Output("sysctl", "-n", "net.pf.filter_local"); strings.TrimSpace(out) != "1" {
		steps = append(steps, setupStep{desc: "Set net.pf.filter_local=1", argv: []string{"sysctl", filterLocalLine}})
	}
	if conf, _ := h.ReadFile("/etc/sysctl.conf"); !containsLine(string(conf), filterLocalLine) {
		steps = append(steps, setupStep{desc: "Persist net.pf.filter_local=1 (/etc/sysctl.conf)", fn: func(context.Context) error {
			return appendLine("/etc/sysctl.conf", filterLocalLine)
		}})
	}
	return steps
}

func hasFstabEntry(fstab string) bool {
	for _, line := range strings.Split(fstab, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && !strings.HasPrefix(f[0], "#") && f[1] == "/dev/fd" && f[2] == "fdescfs" {
			return true
		}
	}
	return false
}

// zfsPool returns the pool holding the root filesystem, or "" without ZFS root.
func zfsPool(h Host) string {
	if _, err := h.LookPath("zfs"); err != nil {
		return ""
	}
	out, err := h.Output("zfs", "list", "-H", "-o", "name", "/")
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(out)
	pool, _, _ := strings.Cut(name, "/")
	return pool
}

func useVFSStorage() error {
	data, err := os.ReadFile(storageConf)
	if errors.Is(err, os.ErrNotExist) {
		data, err = os.ReadFile(storageConf + ".sample")
	}
	if err != nil {
		return fmt.Errorf("reading Podman storage config: %w", err)
	}
	conf := strings.Replace(string(data), `driver = "zfs"`, `driver = "vfs"`, 1)
	if !strings.Contains(conf, `driver = "vfs"`) {
		return fmt.Errorf("couldn't find the storage driver setting in %s", storageConf)
	}
	return os.WriteFile(storageConf, []byte(conf), 0o644)
}

// PFConf is the pf.conf FrostFire writes when the host has none. It only adds
// Podman's NAT and redirect anchors and blocks nothing, so SSH keeps working.
func PFConf(egress string) string {
	return fmt.Sprintf(`# Written by frostfire setup: NAT and port redirects for Podman containers.
# Based on /usr/local/etc/containers/pf.conf.sample. Add your own rules below.
v4egress_if = "%[1]s"
v6egress_if = "%[1]s"

table <cni-nat>

nat on $v4egress_if inet from <cni-nat> to any -> ($v4egress_if)
nat on $v6egress_if inet6 from <cni-nat> to !ff00::/8 -> ($v6egress_if)

rdr-anchor "cni-rdr/*"
nat-anchor "cni-rdr/*"
`, egress)
}

// ensurePFConf writes pf.conf if there is none. An existing pf.conf is the admin's
// firewall, so it's never rewritten: if it lacks Podman's anchors, setup stops and
// says exactly what to add.
func ensurePFConf(h Host) error {
	existing, err := h.ReadFile(pfConf)
	if err == nil {
		if strings.Contains(string(existing), "cni-rdr") {
			return nil
		}
		return fmt.Errorf("%s exists but has no Podman rules. Add these lines (set the interface), then run setup again:\n\n%s",
			pfConf, PFConf(egressInterface(h)))
	}
	egress := egressInterface(h)
	if egress == "" {
		return errors.New("couldn't find the default route's interface to NAT containers through")
	}
	return os.WriteFile(pfConf, []byte(PFConf(egress)), 0o644)
}

func egressInterface(h Host) string {
	out, err := h.Output("route", "-n", "get", "default")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "interface" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func appendLine(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, _ := os.ReadFile(path)
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func containsLine(text, want string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}
