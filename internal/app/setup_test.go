package app

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Daggenthal/FrostFire/internal/platform"
)

// fakeHost answers setup's questions from maps, so every branch of the plan can be
// checked here, including systemd, firewalls and FreeBSD.
type fakeHost struct {
	bins    map[string]bool   // on $PATH
	exists  map[string]bool   // files that exist
	probes  map[string]bool   // "argv joined by spaces" -> exits 0
	outputs map[string]string // "argv joined by spaces" -> stdout
	files   map[string]string
	restic  string // `restic version` output, "" when not installed
}

func (h *fakeHost) LookPath(f string) (string, error) {
	if h.bins[f] {
		return f, nil
	}
	return "", errors.New("not found")
}
func (h *fakeHost) Exists(p string) bool      { return h.exists[p] }
func (h *fakeHost) Probe(argv ...string) bool { return h.probes[strings.Join(argv, " ")] }
func (h *fakeHost) ReadFile(p string) ([]byte, error) {
	if s, ok := h.files[p]; ok {
		return []byte(s), nil
	}
	return nil, errors.New("no such file")
}
func (h *fakeHost) Output(argv ...string) (string, error) {
	if len(argv) == 2 && argv[1] == "version" && filepath.Base(argv[0]) == "restic" {
		if h.restic == "" {
			return "", errors.New("not installed")
		}
		return h.restic, nil
	}
	if out, ok := h.outputs[strings.Join(argv, " ")]; ok {
		return out, nil
	}
	return "", errors.New("failed")
}

func newFake() *fakeHost {
	return &fakeHost{bins: map[string]bool{}, exists: map[string]bool{}, probes: map[string]bool{},
		outputs: map[string]string{}, files: map[string]string{}}
}

func osRelease(t *testing.T, s string) platform.Platform {
	t.Helper()
	p, err := platform.FromOSRelease(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// render turns a plan into one line per step: the command, or the description for in-process steps.
func render(steps []setupStep) string {
	var lines []string
	for _, s := range steps {
		if s.argv != nil {
			lines = append(lines, strings.Join(s.argv, " "))
		} else {
			lines = append(lines, "["+s.desc+"]")
		}
	}
	return strings.Join(lines, "\n")
}

func plan(t *testing.T, h *fakeHost, p platform.Platform, opt SetupOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	a := New(filepath.Join(t.TempDir(), "root"))
	a.Stdout, a.Stderr = &out, &out
	steps, err := a.planSetup(h, p, opt)
	return render(steps), err
}

const ubuntu = "ID=ubuntu\nID_LIKE=debian\nPRETTY_NAME=\"Ubuntu 26.04 LTS\""

func TestSetupPlanUbuntuFreshWithUFW(t *testing.T) {
	h := newFake()
	h.exists["/run/systemd/system"] = true
	h.bins["ufw"] = true
	h.outputs["ufw status"] = "Status: active\n"

	got, err := plan(t, h, osRelease(t, ubuntu), SetupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"apt-get update",
		"apt-get install -y podman podman-compose restic",
		"[Make sure restic is 0.17+ (installs the official 0.18.1 release to /usr/local/bin/restic if the packaged one is too old)]",
		"systemctl enable podman-restart.service",
		"ufw allow 80/tcp",
		"ufw allow 443/tcp",
	}, "\n")
	if got != want {
		t.Fatalf("plan:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestSetupPlanUbuntuDocker(t *testing.T) {
	h := newFake()
	h.exists["/run/systemd/system"] = true
	h.restic = "restic 0.18.1 compiled with go1.25 on linux/amd64"

	got, err := plan(t, h, osRelease(t, ubuntu), SetupOptions{Engine: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	want := "apt-get update\napt-get install -y docker.io docker-compose-v2\nsystemctl enable --now docker.service"
	if got != want {
		t.Fatalf("plan:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestSetupPlanDebianDockerUsesDebianPackage(t *testing.T) {
	h := newFake()
	h.exists["/run/systemd/system"] = true
	h.restic = "restic 0.18.0"
	got, _ := plan(t, h, osRelease(t, "ID=debian"), SetupOptions{Engine: "docker"})
	if !strings.Contains(got, "apt-get install -y docker.io docker-compose\n") {
		t.Fatalf("plan:\n%s", got)
	}
}

func TestSetupPlanCentOSWithFirewalld(t *testing.T) {
	h := newFake()
	h.exists["/run/systemd/system"] = true
	h.bins["podman"] = true // podman installed, compose missing
	h.bins["firewall-cmd"] = true
	h.probes["firewall-cmd --state"] = true
	h.restic = "restic 0.17.3"

	got, err := plan(t, h, osRelease(t, "ID=\"centos\"\nID_LIKE=\"rhel fedora\""), SetupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"dnf install -y epel-release",
		"dnf install -y podman-compose",
		"systemctl enable podman-restart.service",
		"firewall-cmd --permanent --add-service=http --add-service=https",
		"firewall-cmd --reload",
	}, "\n")
	if got != want {
		t.Fatalf("plan:\n%s\n\nwant:\n%s", got, want)
	}

	if _, err := plan(t, h, osRelease(t, "ID=rocky\nID_LIKE=\"rhel centos fedora\""), SetupOptions{Engine: "docker"}); err == nil {
		t.Fatal("expected an error: Docker isn't packaged on the RHEL family")
	}
}

func TestSetupPlanNothingToDo(t *testing.T) {
	h := newFake()
	h.exists["/run/systemd/system"] = true
	h.bins["podman"] = true
	h.probes["podman compose version"] = true
	h.probes["systemctl is-enabled --quiet podman-restart.service"] = true
	h.restic = "restic 0.18.1"

	got, err := plan(t, h, osRelease(t, "ID=fedora"), SetupOptions{})
	if err != nil || got != "" {
		t.Fatalf("expected an empty plan, got %q, %v", got, err)
	}
}

func TestSetupPlanAlpineOpenRC(t *testing.T) {
	h := newFake()
	h.exists["/sbin/rc-update"] = true
	h.restic = "restic 0.18.1"
	got, _ := plan(t, h, osRelease(t, "ID=alpine"), SetupOptions{})
	want := "apk add --no-cache podman podman-compose\nrc-update add podman default\nrc-service podman start"
	if got != want {
		t.Fatalf("plan:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestSetupPlanFreeBSDFreshZFS(t *testing.T) {
	h := newFake()
	h.bins["zfs"] = true
	h.outputs["zfs list -H -o name /"] = "zroot/ROOT/default\n"
	h.outputs["zfs list -H -o mountpoint"] = "/\n/home\n/var/log\n"
	h.files["/etc/fstab"] = "/dev/gpt/efiboot0 /boot/efi msdosfs rw 2 2\n"

	got, err := plan(t, h, platform.ForFreeBSD(), SetupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"pkg install -y sysutils/podman-suite sysutils/podman-compose sysutils/restic",
		"[Make sure restic is 0.17+ (installs the official 0.18.1 release to /usr/local/bin/restic if the packaged one is too old)]",
		"mount -t fdescfs fdesc /dev/fd",
		"[Mount fdescfs at boot (/etc/fstab)]",
		"zfs create -o mountpoint=/var/db/containers zroot/containers",
		"[Configure pf for Podman (/etc/pf.conf)]",
		"sysrc pf_enable=YES",
		"service pf start",
		"sysctl net.pf.filter_local=1",
		"[Persist net.pf.filter_local=1 (/etc/sysctl.conf)]",
		"sysrc podman_enable=YES",
	}, "\n")
	if got != want {
		t.Fatalf("plan:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestSetupPlanFreeBSDPrepared(t *testing.T) {
	h := newFake()
	h.bins["podman"] = true
	h.probes["podman compose version"] = true
	h.restic = "restic 0.18.1"
	h.outputs["mount -t fdescfs"] = "fdescfs on /dev/fd (fdescfs)\n"
	h.files["/etc/fstab"] = "fdescfs /dev/fd fdescfs rw 0 0\n"
	h.bins["zfs"] = true
	h.outputs["zfs list -H -o name /"] = "zroot/ROOT/default\n"
	h.outputs["zfs list -H -o mountpoint"] = "/\n/var/db/containers\n"
	h.files["/etc/pf.conf"] = PFConf("vtnet0")
	h.probes["service pf enabled"] = true
	h.probes["service pf status"] = true
	h.outputs["sysctl -n net.pf.filter_local"] = "1\n"
	h.files["/etc/sysctl.conf"] = "net.pf.filter_local=1\n"
	h.outputs["sysrc -n podman_enable"] = "YES\n"

	got, err := plan(t, h, platform.ForFreeBSD(), SetupOptions{})
	if err != nil || got != "" {
		t.Fatalf("expected an empty plan on a prepared host, got:\n%s\n%v", got, err)
	}
}

func TestSetupPlanFreeBSDNoZFSAndDocker(t *testing.T) {
	h := newFake()
	h.restic = "restic 0.18.1"
	got, _ := plan(t, h, platform.ForFreeBSD(), SetupOptions{})
	if !strings.Contains(got, "[No ZFS: switch Podman storage to the vfs driver]") {
		t.Fatalf("expected the vfs storage step:\n%s", got)
	}
	if _, err := plan(t, h, platform.ForFreeBSD(), SetupOptions{Engine: "docker"}); err == nil {
		t.Fatal("expected an error for Docker on FreeBSD")
	}
}

func TestEnsurePFConfLeavesExistingFirewallAlone(t *testing.T) {
	h := newFake()
	h.files["/etc/pf.conf"] = "block in all\npass out all\n"
	h.outputs["route -n get default"] = "   route to: default\n  interface: igb0\n"
	err := ensurePFConf(h)
	if err == nil || !strings.Contains(err.Error(), `v4egress_if = "igb0"`) || !strings.Contains(err.Error(), `rdr-anchor "cni-rdr/*"`) {
		t.Fatalf("expected instructions for the existing pf.conf, got %v", err)
	}
}
