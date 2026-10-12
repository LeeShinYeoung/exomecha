package logging

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetDefaultWritesTerminalAndFile(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	dir := filepath.Join(t.TempDir(), "logs")
	var term bytes.Buffer
	if err := SetDefault(dir, &term); err != nil {
		t.Fatal(err)
	}
	slog.Info("exomecha started", Repo, "o/r")

	b, err := os.ReadFile(filepath.Join(dir, "exomecha.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != term.String() || !strings.Contains(term.String(), `level=INFO msg="exomecha started" repo=o/r`) {
		t.Errorf("file %q and terminal %q, want the same start line in both", b, term.String())
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "exomecha.log"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %v, want %v", path, got, want)
		}
	}
}

func TestHandlerPutsContextFirst(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewTextHandler(&buf, nil)))
	ctx := With(With(context.Background(), Worker, "builder"), PRD, 4)
	log.ErrorContext(ctx, "handling the PRD failed", Attempt, 2)
	log.InfoContext(context.Background(), "exomecha stopped")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.HasSuffix(lines[0], `msg="handling the PRD failed" worker=builder prd=4 attempt=2`) {
		t.Errorf("line %q, want the context's values first", lines[0])
	}
	if strings.Contains(lines[1], "worker=") {
		t.Errorf("line %q, want no values from another line's context", lines[1])
	}
}

func TestFileKeepsOnePrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exomecha.log")
	f, err := openFile(path, 100, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 39) + "\n")
	for range 10 {
		f.Write(line)
	}
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 100 || info.Size()%40 != 0 {
			t.Errorf("%s holds %d bytes, want whole lines within the limit", p, info.Size())
		}
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("found a second previous file, want only one")
	}
}

func TestFileReportsAFailureOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exomecha.log")
	var term bytes.Buffer
	f, err := openFile(path, 1<<20, &term)
	if err != nil {
		t.Fatal(err)
	}
	f.f.Close()
	if n, err := f.Write([]byte("lost\n")); n != 5 || err != nil {
		t.Fatalf("Write = %d, %v, want the line taken without an error", n, err)
	}
	// The next write cannot open the file again either.
	os.Remove(path)
	os.Chmod(dir, 0o500)
	f.Write([]byte("lost too\n"))
	os.Chmod(dir, 0o700)
	f.Write([]byte("kept\n"))

	if got := strings.Count(term.String(), "level=WARN"); got != 1 {
		t.Errorf("terminal %q, want the failure reported once", term.String())
	}
	if !strings.Contains(term.String(), `level=INFO msg="side task works again"`) {
		t.Errorf("terminal %q, want the recovery reported", term.String())
	}
	if b, _ := os.ReadFile(path); string(b) != "kept\n" {
		t.Errorf("file holds %q, want the line after the recovery", b)
	}
}
