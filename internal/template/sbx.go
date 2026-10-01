package template

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// runner runs sbx with in as its stdin and out as its stdout. With out nil, stdout joins
// stderr in output, which keeps only the end of what sbx printed.
type runner func(ctx context.Context, in io.Reader, out io.Writer, args ...string) (output string, err error)

func runSbx(ctx context.Context, in io.Reader, out io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "sbx", args...)
	var tail tailBuffer
	cmd.Stdin = in
	cmd.Stdout = &tail
	if out != nil {
		cmd.Stdout = out
	}
	cmd.Stderr = &tail
	// Wait also waits for the copy from in, which can block on a reader that never ends.
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	return tail.String(), err
}

func (e *Env) sbx(ctx context.Context, args ...string) error {
	output, err := e.run(ctx, nil, nil, args...)
	if err != nil {
		return sbxError(args, err, output)
	}
	return nil
}

func (e *Env) sbxJSON(ctx context.Context, v any, args ...string) error {
	var out bytes.Buffer
	output, err := e.run(ctx, nil, &out, args...)
	if err != nil {
		return sbxError(args, err, output)
	}
	if err := json.Unmarshal(out.Bytes(), v); err != nil {
		return fmt.Errorf("sbx %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func sbxError(args []string, err error, output string) error {
	return fmt.Errorf("sbx %s: %w: %s", strings.Join(args, " "), err, output)
}

type image struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

func (e *Env) templates(ctx context.Context) ([]image, error) {
	var list struct {
		Images []image `json:"images"`
	}
	err := e.sbxJSON(ctx, &list, "template", "ls", "--json")
	return list.Images, err
}

func (e *Env) sandboxExists(ctx context.Context, name string) (bool, error) {
	var list struct {
		Sandboxes []struct {
			Name string `json:"name"`
		} `json:"sandboxes"`
	}
	if err := e.sbxJSON(ctx, &list, "ls", "--json"); err != nil {
		return false, err
	}
	for _, s := range list.Sandboxes {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// sbx rm exits 1 for a sandbox that does not exist, so look first.
func (e *Env) removeSandbox(ctx context.Context, name string) error {
	exists, err := e.sandboxExists(ctx, name)
	if err != nil || !exists {
		return err
	}
	return e.sbx(ctx, "rm", "--force", name)
}

// A build can print without limit.
type tailBuffer struct{ b []byte }

const tailLimit = 8000

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailLimit {
		t.b = append(t.b[:0], t.b[len(t.b)-tailLimit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.b
	if len(b) > tailLimit {
		b = b[len(b)-tailLimit:]
	}
	// The cut can land in the middle of a character.
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}
