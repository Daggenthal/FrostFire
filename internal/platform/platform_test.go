package platform

import (
	"strings"
	"testing"
)

func TestFromOSRelease(t *testing.T) {
	cases := []struct {
		osRelease string
		family    string
	}{
		{`ID=ubuntu
ID_LIKE=debian
PRETTY_NAME="Ubuntu 26.04 LTS"`, "debian"},
		{`ID=debian`, "debian"},
		{`ID=raspbian
ID_LIKE=debian`, "debian"},
		{`ID=pop
ID_LIKE="ubuntu debian"`, "debian"},
		{`ID="centos"
ID_LIKE="rhel fedora"`, "rhel"},
		{`ID="rocky"
ID_LIKE="rhel centos fedora"`, "rhel"},
		{`ID=fedora`, "fedora"},
		{`ID=arch`, "arch"},
		{`ID="opensuse-tumbleweed"
ID_LIKE="opensuse suse"`, "suse"},
		{`ID=alpine`, "alpine"},
	}
	for _, c := range cases {
		p, err := FromOSRelease(strings.NewReader(c.osRelease))
		if err != nil {
			t.Errorf("%q: %v", c.osRelease, err)
			continue
		}
		if p.Family != c.family {
			t.Errorf("%q: family %s, want %s", c.osRelease, p.Family, c.family)
		}
	}

	if _, err := FromOSRelease(strings.NewReader("ID=plan9")); err == nil {
		t.Error("unknown distro accepted")
	}
}

func TestInstallCommands(t *testing.T) {
	p, _ := FromOSRelease(strings.NewReader("ID=centos\nID_LIKE=\"rhel fedora\""))
	got := p.InstallCommands([]string{ToolPodman, ToolRestic})
	want := "dnf install -y epel-release | dnf install -y podman restic"
	var parts []string
	for _, c := range got {
		parts = append(parts, strings.Join(c, " "))
	}
	if strings.Join(parts, " | ") != want {
		t.Fatalf("got %q, want %q", strings.Join(parts, " | "), want)
	}
	if p.InstallCommands(nil) != nil {
		t.Fatal("expected no commands when nothing is missing")
	}
}

func TestParseFreeBSDVersion(t *testing.T) {
	for in, want := range map[string]string{
		"15.0-RELEASE-p3": "15.0",
		"14.4-RELEASE":    "14.4",
		"15.1-STABLE":     "15.snap",
		"16.0-CURRENT":    "16.snap",
		"15.1-RC2":        "15.snap",
	} {
		got, err := ParseFreeBSDVersion(in)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseFreeBSDVersion("garbage"); err == nil {
		t.Error("expected an error")
	}
}

func TestUbuntuDockerComposePackage(t *testing.T) {
	u, _ := FromOSRelease(strings.NewReader("ID=ubuntu\nID_LIKE=debian"))
	d, _ := FromOSRelease(strings.NewReader("ID=debian"))
	if u.Packages[ToolDockerCompose] != "docker-compose-v2" || d.Packages[ToolDockerCompose] != "docker-compose" {
		t.Fatalf("ubuntu=%s debian=%s", u.Packages[ToolDockerCompose], d.Packages[ToolDockerCompose])
	}
}
