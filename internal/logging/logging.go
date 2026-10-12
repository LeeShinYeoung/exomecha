package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// The keys a log line may use. No line uses any other, so a search for one key finds every line
// about that thing.
const (
	Worker   = "worker"
	PRD      = "prd"
	Sandbox  = "sandbox"
	Template = "template"
	Attempt  = "attempt" // the try a line is about, counting the failures in a row before it
	Err      = "err"
	Reason   = "reason"
	Task     = "task"
	Until    = "until"
	Waited   = "waited"
	FiveHour = "five_hour"
	Weekly   = "weekly"

	// Only in the start line.
	Repo             = "repo"
	Repos            = "repos"
	Base             = "base"
	FiveHourLimit    = "five_hour_limit"
	WeeklyLimit      = "weekly_limit"
	SandboxLimit     = "sandbox_limit"
	ReadSandboxLimit = "read_sandbox_limit"
	ReviewLimit      = "review_limit"
	TierLight        = "tier_light"
	TierNormal       = "tier_normal"
	TierHeavy        = "tier_heavy"
	ResolverModel    = "resolver_model"
	CreatedLabels    = "created_labels"
)

const fileLimit = 10 << 20

// SetDefault makes slog's default logger send every line to term and to exomecha.log in dir,
// which only this user can read. The file is never closed: each line goes straight to the OS,
// which closes it once exomecha ends, after the last line.
func SetDefault(dir string, term io.Writer) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := openFile(filepath.Join(dir, "exomecha.log"), fileLimit, term)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(NewHandler(slog.NewMultiHandler(slog.NewTextHandler(term, nil), slog.NewTextHandler(f, nil)))))
	return nil
}

type ctxKey struct{}

// With adds key and value to every line logged with the returned context.
func With(ctx context.Context, key string, value any) context.Context {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return context.WithValue(ctx, ctxKey{}, append(slices.Clip(attrs), slog.Any(key, value)))
}

// NewHandler puts the values With added to a line's context in front of the line's own.
func NewHandler(h slog.Handler) slog.Handler { return handler{h} }

type handler struct{ slog.Handler }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	if len(attrs) == 0 {
		return h.Handler.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(attrs...)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return handler{h.Handler.WithAttrs(attrs)}
}

func (h handler) WithGroup(name string) slog.Handler { return handler{h.Handler.WithGroup(name)} }

// file starts a new file once the current one would pass limit, and keeps the one before as
// path.1, so the log never takes more than twice limit.
type file struct {
	path  string
	limit int64
	// Where a failed write is reported, once, since the log itself cannot take the line.
	term *slog.Logger

	mu      sync.Mutex
	f       *os.File
	size    int64
	failing bool
}

func openFile(path string, limit int64, term io.Writer) (*file, error) {
	f := &file{path: path, limit: limit, term: slog.New(slog.NewTextHandler(term, nil))}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *file) open() error {
	fd, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	info, err := fd.Stat()
	if err != nil {
		fd.Close()
		return err
	}
	f.f, f.size = fd, info.Size()
	return nil
}

// Write drops a line the file cannot take and never fails, so a full disk does not stop exomecha.
// The terminal still gets every line.
func (f *file) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.write(p)
	switch {
	case err != nil && !f.failing:
		f.failing = true
		f.term.Warn("side task failed", Task, "write the log file "+f.path, Err, err)
	case err == nil && f.failing:
		f.failing = false
		f.term.Info("side task works again", Task, "write the log file "+f.path)
	}
	return len(p), nil
}

func (f *file) write(p []byte) error {
	if f.f == nil {
		if err := f.open(); err != nil {
			return err
		}
	}
	if f.size > 0 && f.size+int64(len(p)) > f.limit {
		if err := f.rotate(); err != nil {
			return err
		}
	}
	n, err := f.f.Write(p)
	f.size += int64(n)
	if err != nil {
		f.f.Close()
		f.f = nil
	}
	return err
}

// rotate leaves f.f nil when it fails, so the next write opens the file again.
func (f *file) rotate() error {
	err := f.f.Close()
	f.f = nil
	if err != nil {
		return err
	}
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		return err
	}
	return f.open()
}
