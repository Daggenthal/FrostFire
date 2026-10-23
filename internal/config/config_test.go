package config

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := `
# comment
DOMAIN=example.org
export SMTP_USER=apikey
QUOTED='pa$$word # not a comment'
DOUBLE="spaced value"
TRAILING=value # comment
EMPTY=
`
	vars, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOMAIN":    "example.org",
		"SMTP_USER": "apikey",
		"QUOTED":    "pa$$word # not a comment",
		"DOUBLE":    "spaced value",
		"TRAILING":  "value",
		"EMPTY":     "",
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q", k, vars[k], v)
		}
	}

	if _, err := Parse(strings.NewReader("not a pair")); err == nil {
		t.Error("expected error for a line without =")
	}
	if _, err := Parse(strings.NewReader("1BAD=x")); err == nil {
		t.Error("expected error for an invalid key")
	}
}

func TestMissing(t *testing.T) {
	c := &Config{Vars: map[string]string{"A": "set", "B": "__GENERATED__", "C": "you@example.com"}}
	got := strings.Join(c.Missing("A", "B", "C", "D"), ",")
	if got != "B,C,D" {
		t.Fatalf("Missing = %s", got)
	}
}
