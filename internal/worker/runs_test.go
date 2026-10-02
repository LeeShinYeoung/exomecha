package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

func (e *env) state(name string) state {
	e.t.Helper()
	r, err := e.runs.read(e.t.Context(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.state
}

func (e *env) wantState(name string, want state) {
	e.t.Helper()
	if got := e.state(name); got != want {
		e.t.Errorf("state of %s = %s, want %s", name, got, want)
	}
}

// writeCtl writes a file into the sandbox's shared directory, as the runner inside would.
func (e *env) writeCtl(name, file, content string) {
	e.t.Helper()
	if err := os.MkdirAll(e.runs.ctl(name), 0o755); err != nil {
		e.t.Fatal(err)
	}
	writeFile(e.t, e.runs.ctl(name), file, content)
}

func TestRunStates(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.runs.name(builderRole, 7)

	e.wantState(name, none)
	if err := os.MkdirAll(e.runs.dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	e.wantState(name, preparing)
	e.sbx.add(name)
	e.wantState(name, preparing)
	if err := e.runs.record(name, inputs{Head: "abc"}); err != nil {
		t.Fatal(err)
	}
	e.wantState(name, running)
	e.writeCtl(name, "stream.jsonl", "{}\n")
	e.wantState(name, running)

	e.clock.advance(stallAfter - time.Second)
	e.wantState(name, running)
	e.clock.advance(2 * time.Second)
	e.wantState(name, stalled)
	e.writeCtl(name, "stream.jsonl", "{}\n{}\n")
	e.wantState(name, running)
	e.clock.advance(stallAfter - time.Second)
	e.wantState(name, running)

	e.sbx.stop(name)
	e.wantState(name, dead)
	e.writeCtl(name, "exit", "0\n")
	e.wantState(name, finished)
	e.sbx.drop(name)
	e.wantState(name, gone)
}

// A restarted exomecha has seen nothing yet, so the 15 minutes start over.
func TestStallStartsOverForNewRuns(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.runs.name(builderRole, 7)
	e.sbx.add(name)
	e.writeCtl(name, "stream.jsonl", "{}\n")
	if err := e.runs.record(name, inputs{}); err != nil {
		t.Fatal(err)
	}
	e.wantState(name, running)
	e.clock.advance(time.Hour)

	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
	e.wantState(name, running)
	e.clock.advance(stallAfter)
	e.wantState(name, stalled)
}

func TestOutcome(t *testing.T) {
	const init = `{"type":"system","subtype":"init","permissionMode":"auto"}` + "\n"
	for _, c := range []struct {
		name    string
		exit    string
		stream  string
		failure string
		limited bool
	}{
		{"done", "0", streamOf(`{"result":"done"}`), "", false},
		{"not auto", "0", strings.Replace(streamOf(`{"result":"done"}`), `"auto"`, `"default"`, 1), "auto permission mode", false},
		{"no init line", "0", `{"type":"result","subtype":"success","is_error":false,"structured_output":{}}` + "\n", "auto permission mode", false},
		{"error result", "1", init + `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}` + "\n", "with an error or without a result", false},
		{"no result", "143", init, "with an error or without a result", false},
		{"no structured output", "0", init + `{"type":"result","subtype":"success","is_error":false,"result":"text"}` + "\n", "with an error or without a result", false},
		{"bad exit with a result", "1", streamOf(`{"result":"done"}`), "claude ended with an error", false},
		{"bad exit file", "", streamOf(`{"result":"done"}`), "could not read how claude ended", false},
		{"usage limit", "1", init + `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}` + "\n" +
			`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit"}` + "\n", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			name := e.runs.name(builderRole, 7)
			e.writeCtl(name, "exit", c.exit+"\n")
			e.writeCtl(name, "stream.jsonl", c.stream)
			res, failure, _, limited := e.runs.outcome(name)
			if limited != c.limited || (c.failure == "") != (failure == "") || !strings.Contains(failure, c.failure) {
				t.Errorf("outcome = failure %q, limited %v, want failure %q, limited %v", failure, limited, c.failure, c.limited)
			}
			if c.failure == "" && !c.limited && string(res) != `{"result":"done"}` {
				t.Errorf("structured result = %s", res)
			}
		})
	}
}

// The AI can replace any file in the shared directory. A link to a file of this machine must not be read.
func TestOutcomeDoesNotFollowLinks(t *testing.T) {
	for _, file := range []string{"exit", "stream.jsonl"} {
		t.Run(file, func(t *testing.T) {
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			name := e.runs.name(builderRole, 7)
			outside := filepath.Join(t.TempDir(), "outside")
			content := map[string]string{"exit": "0\n", "stream.jsonl": streamOf(`{"result":"done"}`)}
			writeFile(t, filepath.Dir(outside), "outside", content[file])
			for f, c := range content {
				if f != file {
					e.writeCtl(name, f, c)
				}
			}
			if err := os.Symlink(outside, filepath.Join(e.runs.ctl(name), file)); err != nil {
				t.Fatal(err)
			}

			if _, failure, _, _ := e.runs.outcome(name); failure == "" {
				t.Errorf("a %s linked to a file outside was read", file)
			}
			if got := e.runs.tail(name, file); got != "" {
				t.Errorf("tail of a linked %s = %q", file, got)
			}
		})
	}
}

func TestTailKeepsABoundedValidEnd(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.runs.name(builderRole, 7)
	// Three bytes per character and eight here, so the cut at tailLimit lands inside a character.
	e.writeCtl(name, "setup.log", strings.Repeat("한", 300_000)+"the end.")

	got := e.runs.tail(name, "setup.log")
	if len(got) > tailLimit || !strings.HasSuffix(got, "한the end.") || !strings.HasPrefix(got, "한") {
		t.Errorf("tail = %d bytes, ends %q", len(got), got[max(0, len(got)-12):])
	}
}

func TestFound(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prefix := e.deps.SandboxPrefix
	for _, dir := range []string{prefix + "3-builder", prefix + "4-resolver", "failed"} {
		if err := os.MkdirAll(filepath.Join(e.deps.SandboxDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{prefix + "5-builder", prefix + "3-builder", sandbox.Name(sandbox.Prefix("other", "repo"), 6, builderRole.name), "impl-run-x"} {
		e.sbx.add(name)
	}

	found, err := e.runs.found(t.Context(), builderRole)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	wantInts(t, "found builder", found, []int{3, 5})
}

func (e *env) wantReserve(ro role, n int, want bool) {
	e.t.Helper()
	if got, err := e.runs.reserve(e.t.Context(), ro, e.runs.name(ro, n)); err != nil || got != want {
		e.t.Errorf("reserve(%s) = %v, %v, want %v", e.runs.name(ro, n), got, err, want)
	}
}

func TestSlotsCountEveryTaskSandbox(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prefix := e.deps.SandboxPrefix
	e.sbx.add(prefix + "1-builder")
	e.sbx.add(sandbox.Name(sandbox.Prefix("other", "repo"), 2, builderRole.name))
	e.wantReserve(builderRole, 9, true)
	e.runs.release(e.runs.name(builderRole, 9))
	e.sbx.add(prefix + "2-resolver")
	e.sbx.stop(prefix + "2-resolver")
	e.wantReserve(builderRole, 9, false)
}

// owner/repo-v2's sandboxes start with owner/repo's prefix too, and must not take owner/repo's slots.
func TestSlotsCountOnlyThisRepository(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.sbx.add(e.deps.SandboxPrefix + "1-builder")
	e.sbx.add(e.deps.SandboxPrefix + "v2-3-builder")
	e.sbx.add(e.deps.SandboxPrefix + "v2-4-resolver")
	e.wantReserve(builderRole, 9, true)
}

func TestSlotsCountEachKindApart(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ProjectLimit, e.runs.ReadLimit = 1, 1
	e.sbx.add(e.runs.name(builderRole, 1))
	e.wantReserve(reviewerRole, 2, true)
	e.wantReserve(resolverRole, 3, false)
	e.wantReserve(reviewerRole, 4, false)

	e.sbx.drop(e.runs.name(builderRole, 1))
	e.wantReserve(resolverRole, 3, true)
	e.runs.release(e.runs.name(reviewerRole, 2))
	e.wantReserve(reviewerRole, 4, true)
}

func TestReserveHoldsTheSlotUntilRelease(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	first, second := e.runs.name(builderRole, 1), e.runs.name(resolverRole, 2)
	e.wantReserve(builderRole, 1, true)
	e.wantReserve(resolverRole, 2, true)
	e.wantReserve(builderRole, 3, false)

	// The first start's sandbox now exists, and counts once.
	e.sbx.add(first)
	e.runs.release(second)
	e.wantReserve(builderRole, 3, true)

	// Released, the first start's slot is still taken by its sandbox.
	e.runs.release(first)
	e.wantReserve(builderRole, 4, false)
	e.sbx.drop(first)
	e.wantReserve(builderRole, 4, true)
}

// A reader's result is all in its directory, so a finished reader run needs no sandbox. A project
// run's commits are only in its sandbox.
func TestReaderRunFinishesWithoutItsSandbox(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	reader, project := e.runs.name(reviewerRole, 7), e.runs.name(builderRole, 7)
	for _, name := range []string{reader, project} {
		e.sbx.add(name)
		e.writeCtl(name, "stream.jsonl", "{}\n")
		if err := e.runs.record(name, inputs{}); err != nil {
			t.Fatal(err)
		}
		e.sbx.drop(name)
		e.wantState(name, gone)
		e.writeCtl(name, "exit", "0\n")
	}
	e.wantState(reader, finished)
	e.wantState(project, gone)
}

func TestFinishedReaderGivesItsSlotBack(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ReadLimit = 1
	reader, project := e.runs.name(reviewerRole, 7), e.runs.name(builderRole, 8)
	for _, name := range []string{reader, project} {
		e.sbx.add(name)
		e.writeCtl(name, "stream.jsonl", "{}\n")
		if err := e.runs.record(name, inputs{}); err != nil {
			t.Fatal(err)
		}
		e.writeCtl(name, "exit", "0\n")
	}
	e.wantReserve(reviewerRole, 9, false)

	// Every action idle, so only the finished reader's step itself acts.
	if err := e.runs.step(t.Context(), reviewerRole, 7, row{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.runs.step(t.Context(), builderRole, 8, row{}, nil, nil); err != nil {
		t.Fatal(err)
	}

	if e.sbx.exists(reader) {
		t.Error("the finished reader's sandbox is still there")
	}
	e.wantState(reader, finished)
	if !e.sbx.exists(project) {
		t.Error("the finished project run's sandbox was removed before its commits were taken")
	}
	e.wantReserve(reviewerRole, 9, true)
}
