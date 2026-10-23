package app

import (
	"testing"
	"time"

	"github.com/Daggenthal/FrostFire/internal/restic"
)

func TestGroupRunsAndPick(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	snaps := []restic.Snapshot{
		{ShortID: "f1", Time: t0, Tags: []string{"frostfire", "run-A", "files"}},
		{ShortID: "d1", Time: t0.Add(time.Minute), Tags: []string{"frostfire", "run-A", "db"}},
		{ShortID: "f2", Time: t0.Add(24 * time.Hour), Tags: []string{"frostfire", "run-B", "files"}}, // dump failed
		{ShortID: "x", Time: t0, Tags: []string{"frostfire"}},                                        // no run tag, ignored
	}
	runs := GroupRuns(snaps)
	if len(runs) != 2 || runs[0].ID != "B" || runs[1].ID != "A" {
		t.Fatalf("unexpected runs %+v", runs)
	}
	if runs[0].Complete() || !runs[1].Complete() {
		t.Fatal("completeness wrong")
	}

	r, err := pickRun(runs, RestoreOptions{Run: "latest", Files: true, Database: true})
	if err != nil || r.ID != "A" {
		t.Fatalf("latest full restore picked %q, %v", r.ID, err)
	}
	r, err = pickRun(runs, RestoreOptions{Run: "latest", Files: true})
	if err != nil || r.ID != "B" {
		t.Fatalf("latest files-only restore picked %q, %v", r.ID, err)
	}
	if _, err := pickRun(runs, RestoreOptions{Run: "B", Files: true, Database: true}); err == nil {
		t.Fatal("incomplete run accepted for a full restore")
	}
	if _, err := pickRun(runs, RestoreOptions{Run: "nope", Files: true}); err == nil {
		t.Fatal("unknown run accepted")
	}
}
