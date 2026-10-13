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
		{group: "A", step: "check a1", run: run("a1 works")},
		{group: "A", step: "check a2", run: run("a2 is broken", errors.New("fix a2"))},
		{group: "B", step: "check b1", attrs: []any{logging.Repo, "o/r"}, gate: true, run: run("b1 is broken", errors.New("fix b1"))},
		{group: "B", step: "check b2", run: run("b2 works")},
		{group: "C", step: "install c", initOnly: true, run: run("c installed")},
	})
	if err == nil || err.Error() != "fix a2\nfix b1" {
		t.Errorf("check = %v, want the problems in the items' order", err)
	}
	// The groups run at the same time, so only the order within a group is fixed.
	for prefix, want := range map[string]string{
		`msg="check a`: `level=INFO msg="check a1: start"
level=INFO msg="check a1: done" result="a1 works"
level=INFO msg="check a2: start"
level=ERROR msg="check a2: failed" result="a2 is broken"
`,
		`msg="check b`: `level=INFO msg="check b1: start" repo=o/r
level=ERROR msg="check b1: failed" repo=o/r result="b1 is broken"
level=INFO msg="check b2: skipped"
`,
	} {
		var got strings.Builder
		for line := range strings.Lines(out.String()) {
			if strings.Contains(line, prefix) {
				got.WriteString(line)
			}
		}
		if got.String() != want {
			t.Errorf("check logged\n%s\nwant\n%s", got.String(), want)
		}
	}
	if strings.Contains(out.String(), "install c") {
		t.Errorf("check logged the init only item:\n%s", out.String())
	}
}
