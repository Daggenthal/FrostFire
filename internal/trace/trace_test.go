package trace

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestTraceShowsCommandsButNeverSecretValues(t *testing.T) {
	var buf bytes.Buffer
	Enable(&buf)
	defer func() { enabled = false }()

	cmd := exec.Command("sh", "-c", "exit 3")
	cmd.Env = append(os.Environ(), "MARIADB_ROOT_PASSWORD=hunter2-secret")
	if err := Run(cmd); err == nil {
		t.Fatal("expected the command to fail")
	}
	got := buf.String()
	for _, want := range []string{"$ sh -c 'exit 3'", "env: MARIADB_ROOT_PASSWORD", "FAILED: sh -c", "exit status 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") {
		t.Fatalf("secret value leaked into the trace:\n%s", got)
	}
}

func TestQuote(t *testing.T) {
	got := Quote([]string{"podman", "build", "--tag", "x:y", "it's here", ""})
	want := `podman build --tag x:y 'it'\''s here' ''`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
