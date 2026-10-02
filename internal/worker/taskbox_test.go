package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func (e *env) state(name string) state {
	e.t.Helper()
	r, err := e.box.read(e.t.Context(), name)
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
	if err := os.MkdirAll(e.box.ctl(name), 0o755); err != nil {
		e.t.Fatal(err)
	}
	writeFile(e.t, e.box.ctl(name), file, content)
}

func TestTaskboxStates(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.box.name(7, "builder")

	e.wantState(name, none)
	if err := os.MkdirAll(e.box.dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	e.wantState(name, preparing)
	e.sbx.add(name)
	e.wantState(name, preparing)
	if err := e.box.record(name, started{Before: "abc"}); err != nil {
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

// A restarted coworker has seen nothing yet, so the 15 minutes start over.
func TestStallStartsOverForANewTaskbox(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.box.name(7, "builder")
	e.sbx.add(name)
	e.writeCtl(name, "stream.jsonl", "{}\n")
	if err := e.box.record(name, started{}); err != nil {
		t.Fatal(err)
	}
	e.wantState(name, running)
	e.clock.advance(time.Hour)

	e.box = newTaskbox(e.deps, nil)
	e.box.now = e.clock.now
	e.wantState(name, running)
	e.clock.advance(stallAfter)
	e.wantState(name, stalled)
}

func TestBackoffDoubles(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.box.name(7, "builder")
	if !e.box.due(name) {
		t.Fatal("a Task with no tries is not due")
	}
	for _, wait := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		e.box.strike(name)
		e.clock.advance(wait*time.Minute - time.Second)
		if e.box.due(name) {
			t.Fatalf("due before %v passed", wait*time.Minute)
		}
		e.clock.advance(time.Second)
		if !e.box.due(name) {
			t.Fatalf("not due after %v", wait*time.Minute)
		}
	}
	e.box.forget(name)
	if !e.box.due(name) {
		t.Error("not due after forget")
	}
}

func TestFailCountsInARow(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.box.name(7, "builder")
	for want := 1; want <= 3; want++ {
		if got := e.box.fail(name); got != want {
			t.Errorf("fail = %d, want %d", got, want)
		}
	}
	e.box.forget(name)
	if got := e.box.fail(name); got != 1 {
		t.Errorf("fail after forget = %d, want 1", got)
	}
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
			name := e.box.name(7, "builder")
			e.writeCtl(name, "exit", c.exit+"\n")
			e.writeCtl(name, "stream.jsonl", c.stream)
			res, failure, _, limited := e.box.outcome(name, true)
			if limited != c.limited || (c.failure == "") != (failure == "") || !strings.Contains(failure, c.failure) {
				t.Errorf("outcome = failure %q, limited %v, want failure %q, limited %v", failure, limited, c.failure, c.limited)
			}
			if c.failure == "" && !c.limited && string(res.Structured) != `{"result":"done"}` {
				t.Errorf("structured result = %s", res.Structured)
			}
		})
	}
}

// The AI can replace any file in the shared directory. A link to a file of this machine must not be read.
func TestOutcomeDoesNotFollowLinks(t *testing.T) {
	for _, file := range []string{"exit", "stream.jsonl"} {
		t.Run(file, func(t *testing.T) {
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			name := e.box.name(7, "builder")
			outside := filepath.Join(t.TempDir(), "outside")
			content := map[string]string{"exit": "0\n", "stream.jsonl": streamOf(`{"result":"done"}`)}
			writeFile(t, filepath.Dir(outside), "outside", content[file])
			for f, c := range content {
				if f != file {
					e.writeCtl(name, f, c)
				}
			}
			if err := os.Symlink(outside, filepath.Join(e.box.ctl(name), file)); err != nil {
				t.Fatal(err)
			}

			if _, failure, _, _ := e.box.outcome(name, true); failure == "" {
				t.Errorf("a %s linked to a file outside was read", file)
			}
			if got := e.box.tail(name, file); got != "" {
				t.Errorf("tail of a linked %s = %q", file, got)
			}
		})
	}
}

func TestTailKeepsABoundedValidEnd(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.box.name(7, "builder")
	// Three bytes per character and eight here, so the cut at tailLimit lands inside a character.
	e.writeCtl(name, "setup.log", strings.Repeat("한", 300_000)+"the end.")

	got := e.box.tail(name, "setup.log")
	if len(got) > tailLimit || !strings.HasSuffix(got, "한the end.") || !strings.HasPrefix(got, "한") {
		t.Errorf("tail = %d bytes, ends %q", len(got), got[max(0, len(got)-12):])
	}
}

func TestFoundAndPrune(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prefix := e.deps.SandboxPrefix
	for _, dir := range []string{prefix + "3-builder", prefix + "4-resolver", "failed"} {
		if err := os.MkdirAll(filepath.Join(e.deps.SandboxDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{prefix + "5-builder", prefix + "3-builder", "coworker-task-other-repo-6-builder", "impl-run-x"} {
		e.sbx.add(name)
	}

	found, err := e.box.found(t.Context(), "builder")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	wantInts(t, "found builder", found, []int{3, 5})

	e.box.fail(prefix + "3-builder")
	e.box.fail(prefix + "5-builder")
	e.box.fail(prefix + "3-resolver")
	e.box.prune("builder", map[int]bool{5: true})
	if !e.box.due(prefix + "3-builder") {
		t.Error("prune kept the failures of a Task builder no longer owns")
	}
	if e.box.due(prefix+"5-builder") || e.box.due(prefix+"3-resolver") {
		t.Error("prune dropped failures it should keep")
	}
}

func (e *env) wantReady(name string, want bool) {
	e.t.Helper()
	if got, err := e.box.ready(e.t.Context(), name); err != nil || got != want {
		e.t.Errorf("ready(%s) = %v, %v, want %v", name, got, err, want)
	}
}

func TestSlotsCountEveryTaskSandbox(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prefix := e.deps.SandboxPrefix
	e.sbx.add(prefix + "1-builder")
	e.sbx.add("coworker-task-other-repo-2-builder")
	e.wantReady(e.box.name(9, builderRole), true)
	e.box.release(e.box.name(9, builderRole))
	e.sbx.add(prefix + "2-resolver")
	e.sbx.stop(prefix + "2-resolver")
	e.wantReady(e.box.name(9, builderRole), false)
}

// owner/repo-v2's sandboxes start with owner/repo's prefix too, and must not take owner/repo's slots.
func TestSlotsCountOnlyThisRepository(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.sbx.add(e.deps.SandboxPrefix + "1-builder")
	e.sbx.add(e.deps.SandboxPrefix + "v2-3-builder")
	e.sbx.add(e.deps.SandboxPrefix + "v2-4-resolver")
	e.wantReady(e.box.name(9, builderRole), true)
}

func TestReadyHoldsTheSlotUntilRelease(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	first, second := e.box.name(1, builderRole), e.box.name(2, resolverRole)
	third, fourth := e.box.name(3, builderRole), e.box.name(4, builderRole)
	e.wantReady(first, true)
	e.wantReady(second, true)
	e.wantReady(third, false)

	// The first start's sandbox now exists, and counts once.
	e.sbx.add(first)
	e.box.release(second)
	e.wantReady(third, true)

	// Released, the first start's slot is still taken by its sandbox.
	e.box.release(first)
	e.wantReady(fourth, false)
	e.sbx.drop(first)
	e.wantReady(fourth, true)
}
