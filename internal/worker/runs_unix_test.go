//go:build unix

package worker

import (
	"path/filepath"
	"syscall"
	"testing"
)

// Opening a FIFO for reading waits for a writer. The AI must not be able to freeze exomecha that way.
func TestOutcomeDoesNotOpenAFIFO(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	name := e.runs.name(builderRole, 7)
	e.writeCtl(name, "exit", "0\n")
	if err := syscall.Mkfifo(filepath.Join(e.runs.ctl(name), "stream.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, failure, _, _ := e.runs.outcome(name); failure == "" {
		t.Error("a FIFO was read as claude's output")
	}
}
