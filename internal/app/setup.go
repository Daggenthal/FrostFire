package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Daggenthal/FrostFire/internal/engine"
	"github.com/Daggenthal/FrostFire/internal/platform"
	"github.com/Daggenthal/FrostFire/internal/restic"
)

// SetupOptions controls `frostfire setup`.
type SetupOptions struct {
	Yes    bool
	DryRun bool
}

type setupStep struct {
	desc string
	argv []string
	env  []string
	fn   func(context.Context) error // used instead of argv for in-process steps
}

// Setup installs what FrostFire needs: a container engine with compose (Podman by
// default), restic 0.17+, restart-on-boot for the containers, and open web ports.
// Anything already present and working is left alone, so it's safe to run again.
func (a *App) Setup(ctx context.Context, opt SetupOptions) error {
	p, err := platform.Detect()
	if err != nil {
		return err
	}
	a.step("Detected %s (%s family)", p.Name, p.Family)
	if !opt.DryRun && os.Geteuid() != 0 {
		return errors.New("setup installs system packages, run it as root (or with --dry-run to see the plan)")
	}

	// What's missing?
	var tools []string
	engineBin, engineErr := engine.Detect(a.engineOverride())
	if engineErr == nil {
		a.step("Container engine: %s compose (already installed)", engineBin)
	} else {
		if _, err := exec.LookPath("podman"); err != nil {
			tools = append(tools, platform.ToolPodman)
		}
		tools = append(tools, platform.ToolCompose)
	}

	resticOK, resticDetail := a.resticStatus(ctx)
	if resticOK {
		a.step("restic: %s (already installed)", resticDetail)
	} else if _, err := exec.LookPath("restic"); err != nil {
		tools = append(tools, platform.ToolRestic)
	}

	// Build the plan.
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
			desc: fmt.Sprintf("Make sure restic is %d.%d+ (installs the official %s release to %s if the distro's is too old)",
				restic.MinVersion[0], restic.MinVersion[1], restic.OfficialVersion, restic.OfficialPath),
			fn: a.ensureRestic,
		})
	}
	steps = append(steps, a.bootSteps(engineBin, engineErr == nil)...)
	steps = append(steps, firewallSteps()...)

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
				return err
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
	if bin, err := engine.Detect(a.engineOverride()); err != nil {
		return fmt.Errorf("installed packages, but still no working compose: %w", err)
	} else {
		a.step("Container engine ready: %s compose", bin)
	}
	if ok, detail := a.resticStatus(ctx); !ok {
		return fmt.Errorf("restic still not usable: %s", detail)
	} else {
		a.step("restic ready: %s", detail)
	}
	return a.setupNext()
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

func (a *App) resticStatus(ctx context.Context) (bool, string) {
	override := os.Getenv("RESTIC_BIN")
	if a.load() == nil {
		override = a.Cfg.Get("RESTIC_BIN", override)
	}
	r := &restic.Restic{Bin: restic.FindBin(override)}
	v, err := r.Version(ctx)
	if err != nil {
		return false, "not installed"
	}
	detail := fmt.Sprintf("%d.%d.%d at %s", v[0], v[1], v[2], r.Bin)
	if !restic.AtLeast(v, restic.MinVersion) {
		return false, detail + " (too old)"
	}
	return true, detail
}

func (a *App) ensureRestic(ctx context.Context) error {
	if ok, _ := a.resticStatus(ctx); ok {
		return nil
	}
	if err := restic.InstallOfficial(ctx, restic.OfficialVersion, restic.OfficialPath); err != nil {
		return fmt.Errorf("installing restic %s: %w", restic.OfficialVersion, err)
	}
	return nil
}

// bootSteps makes the containers come back after a reboot.
func (a *App) bootSteps(engineBin string, engineReady bool) []setupStep {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		a.warn("not a systemd system: make sure your container engine starts on boot")
		return nil
	}
	if engineReady && strings.HasSuffix(engineBin, "docker") {
		return []setupStep{{desc: "Start Docker on boot", argv: []string{"systemctl", "enable", "--now", "docker.service"}}}
	}
	// Podman has no daemon. podman-restart.service starts containers with restart: always at boot.
	return []setupStep{{desc: "Restart containers on boot", argv: []string{"systemctl", "enable", "podman-restart.service"}}}
}

// firewallSteps opens 80 and 443 if a host firewall is active.
func firewallSteps() []setupStep {
	if _, err := exec.LookPath("firewall-cmd"); err == nil && exec.Command("firewall-cmd", "--state").Run() == nil {
		return []setupStep{
			{desc: "Open HTTP and HTTPS in firewalld", argv: []string{"firewall-cmd", "--permanent", "--add-service=http", "--add-service=https"}},
			{desc: "Reload firewalld", argv: []string{"firewall-cmd", "--reload"}},
		}
	}
	if _, err := exec.LookPath("ufw"); err == nil {
		if out, err := exec.Command("ufw", "status").Output(); err == nil && strings.Contains(string(out), "Status: active") {
			return []setupStep{
				{desc: "Open HTTP in ufw", argv: []string{"ufw", "allow", "80/tcp"}},
				{desc: "Open HTTPS in ufw", argv: []string{"ufw", "allow", "443/tcp"}},
			}
		}
	}
	return nil
}
