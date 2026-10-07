package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
)

// take brings the commit the run left checked out into the shared clone, for the worker to push.
// ok is false when the run went to stuck or starts over instead, and err is set only when a step
// failed. said is what the AI said about its work, for a stuck comment.
func (r *runs) take(ctx context.Context, ro role, n int, in inputs, said string) (head string, ok bool, err error) {
	// rev-parse exits 1 for an unborn HEAD, which sh would report as an error.
	out, err := r.sh(ctx, ro, n, "git rev-parse -q --verify HEAD || true")
	if err != nil {
		return "", false, err
	}
	// A fetch would fail the same way on every try.
	if strings.TrimSpace(out) == "" {
		return "", false, r.stuck(ctx, ro, n, "The AI left its repository with no commit checked out, so there is nothing to push.\n\n"+said)
	}
	name := r.name(ro, n)
	if head, err = r.Git.FetchFrom(ctx, r.Sandboxes.GitURL(name, repoDir), name); err != nil {
		return "", false, err
	}
	if err := r.Git.Sync(ctx); err != nil {
		return "", false, err
	}
	remote, _, err := r.Git.Rev(ctx, r.Git.RemoteRef(branchName(n)))
	if err != nil {
		return "", false, err
	}
	if in.moved(remote, head) {
		return "", false, r.restart(ctx, ro, n, "the PRD branch changed on GitHub during the run")
	}
	// GitHub refuses a push that rewrites the branch, and it would refuse it again on every retry.
	kept, err := r.Git.IsAncestor(ctx, in.Head, head)
	if err != nil {
		return "", false, err
	}
	if !kept {
		return "", false, r.stuck(ctx, ro, n, "The AI rewrote commits that were already on the branch, so they cannot be pushed.\n\n"+said)
	}
	return head, true, nil
}

const badResult = "the AI's result was not in the expected form"

// result reads a finished run's structured result into out. ok is false when the run failed or
// starts over instead, and err is set only when that step failed.
func (r *runs) result(ctx context.Context, ro role, n int, out any) (ok bool, err error) {
	name := r.name(ro, n)
	res, failure, detail, limited := r.outcome(name)
	if limited {
		// The reading the start passed missed the limit. A new one keeps the gate closed until the reset.
		if r.gate != nil {
			r.gate.stale()
		}
		return false, r.restart(ctx, ro, n, "the run ended at the Claude usage limit")
	}
	if failure != "" {
		return false, r.failed(ctx, ro, n, failure, detail)
	}
	if err := json.Unmarshal(res, out); err != nil {
		return false, r.failed(ctx, ro, n, badResult, err.Error())
	}
	return true, nil
}

// outcome reads a finished run. failure says why the run failed, in words fit for a comment, and
// detail adds what only the log gets. limited is set when the run ended at the Claude usage limit.
func (r *runs) outcome(name string) (res json.RawMessage, failure, detail string, limited bool) {
	b, err := readCtl(r.ctl(name), "exit", 64)
	if err != nil {
		return res, "exomecha could not read how claude ended", err.Error(), false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return res, "exomecha could not read how claude ended", fmt.Sprintf("exit file holds %q", b), false
	}
	f, err := openCtl(r.ctl(name), "stream.jsonl")
	if err != nil {
		return res, "exomecha could not read claude's output", err.Error(), false
	}
	defer f.Close()
	s, err := agent.ReadStream(io.LimitReader(f, streamLimit))
	if err != nil {
		return res, "exomecha could not read claude's output", err.Error(), false
	}
	res, err = s.Result()
	if (err != nil || code != 0) && s.Rejected {
		return res, "", "", true
	}
	switch {
	case s.PermissionMode != "auto":
		return res, "claude did not run in auto permission mode", fmt.Sprintf("permission mode %q", s.PermissionMode), false
	case err != nil:
		return res, "claude ended with an error or without a result", err.Error(), false
	case code != 0:
		return res, "claude ended with an error", fmt.Sprintf("exit status %d", code), false
	}
	return res, "", "", false
}

func (r *runs) tail(name, file string) string {
	f, err := openCtl(r.ctl(name), file)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > tailLimit {
		if _, err := f.Seek(-tailLimit, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, tailLimit))
	// The cut can land in the middle of a character.
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}

func readCtl(ctl, file string, limit int64) ([]byte, error) {
	f, err := openCtl(ctl, file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// openCtl opens a file the AI may have swapped for a link to a file of this machine, or for a
// FIFO that would block the open. A link that leaves ctl fails to open.
func openCtl(ctl, file string) (*os.File, error) {
	info, err := os.Lstat(filepath.Join(ctl, file))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", file)
	}
	root, err := os.OpenRoot(ctl)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The file can still be swapped after Lstat. Non-blocking, an open of a FIFO returns at once
	// and fails the check below.
	f, err := root.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", file)
	}
	return f, nil
}
