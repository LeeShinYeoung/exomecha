package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func TestKeepTakesTheEndOfEachOutput(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.runs.name(builderRole, 3)
	e.writeCtl(name, "stream.jsonl", strings.Repeat("x", keptFileLimit)+"the end.")
	e.writeCtl(name, "input.md", "the PRD")

	dir := e.runs.keep(t.Context(), builderRole, 3, map[string]string{"merge.log": "conflict"})

	if got := e.wantKept(name, "stream.jsonl"); len(got) != keptFileLimit || !strings.HasSuffix(got, "xthe end.") {
		t.Errorf("kept stream.jsonl holds %d bytes, want the last %d", len(got), keptFileLimit)
	}
	if got := e.wantKept(name, "merge.log"); got != "conflict" {
		t.Errorf("kept merge.log = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "input.md")); err == nil {
		t.Error("kept input.md, want only what the run printed")
	}
	for path, want := range map[string]os.FileMode{filepath.Join(e.deps.OutputDir, "3"): 0o700, dir: 0o700, filepath.Join(dir, "merge.log"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %v, want %v", path, got, want)
		}
	}
}

func TestKeepWithNothingPrinted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	if dir := e.runs.keep(t.Context(), reviewerRole, 3, nil); dir != "" {
		t.Errorf("keep = %q, want nothing kept", dir)
	}
	if entries, _ := os.ReadDir(e.deps.OutputDir); len(entries) != 0 {
		t.Errorf("OutputDir holds %d entries, want none", len(entries))
	}
}

func TestTrimKeptRemovesTheOldest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	run := func(issue, dir string) string {
		path := filepath.Join(root, issue, dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "stream.jsonl", strings.Repeat("x", 40))
		return path
	}
	oldest := run("1", "20261001T090000Z-builder")
	other := run("2", "20261002T090000Z-reviewer")
	newest := run("1", "20261003T090000Z-builder")

	if err := trimKept(root, newest, 80); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); err == nil {
		t.Error("the oldest run's output is still kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a run's output that fit was removed: %v", err)
	}

	if err := trimKept(root, newest, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "2")); err == nil {
		t.Error("issue 2 is still there with no output left")
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("the output just kept was removed: %v", err)
	}
}

func TestRunThatLostItsSandboxIsAnError(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.sbx.stop(name)

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantContains(t, "log", log.String(), `level=ERROR msg="the run lost its sandbox" sandbox=`+name)
}
