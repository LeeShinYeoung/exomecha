package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

func TestCheckLogsEachStep(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var out strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && len(groups) == 0 {
			return slog.Attr{}
		}
		return a
	}})))
	run := func(line string, problems ...error) func(context.Context) (string, []error) {
		return func(context.Context) (string, []error) { return line, problems }
	}

	err := check(t.Context(), []item{
		{group: "A", step: "check a", run: run("a works")},
		{group: "B", step: "check b", attrs: []any{logging.Repo, "o/r"}, gate: true, run: run("b is broken", errors.New("fix b"))},
		{group: "B", step: "check c", run: run("c works")},
		{group: "C", step: "install d", initOnly: true, run: run("d installed")},
	})
	if err == nil || err.Error() != "fix b" {
		t.Errorf("check = %v, want the problem b found", err)
	}
	want := `level=INFO msg="check a: start"
level=INFO msg="check a: done" result="a works"
level=INFO msg="check b: start" repo=o/r
level=ERROR msg="check b: failed" repo=o/r result="b is broken"
level=INFO msg="check c: skipped"
`
	if got := out.String(); got != want {
		t.Errorf("check logged\n%s\nwant\n%s", got, want)
	}
}
