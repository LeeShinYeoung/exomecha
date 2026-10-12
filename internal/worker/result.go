package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

// take brings the commits the run left checked out in repos into their shared clones, for the
// worker to push, and returns them in the same order. ok is false when the run went to stuck or
// starts over instead, and err is set only when a step failed. said is what the AI said about its
// work, for a stuck comment.
func (r *runs) take(ctx context.Context, ro role, n int, repos []repoRun, said string) (heads []string, ok bool, err error) {
	name := r.name(ro, n)
	for _, rr := range repos {
		repo, found := r.repo(rr.Repo)
		if !found {
			return nil, false, r.stuck(ctx, ro, n, fmt.Sprintf("The run changed %s, which is no longer one of the repositories exomecha may change, so nothing was pushed.\n\n%s", rr.Repo, said))
		}
		// rev-parse exits 1 for an unborn HEAD, which sh would report as an error.
		out, err := r.sh(ctx, ro, n, rr.Dir, "git rev-parse -q --verify HEAD || true")
		if err != nil {
			return nil, false, err
		}
		// A fetch would fail the same way on every try.
		if strings.TrimSpace(out) == "" {
			return nil, false, r.stuck(ctx, ro, n, "The AI left its repository"+rr.where()+" with no commit checked out, so there is nothing to push.\n\n"+said)
		}
		head, err := repo.Git.FetchFrom(ctx, r.Sandboxes.GitURL(name, rr.Dir), name)
		if err != nil {
			return nil, false, err
		}
		if err := repo.Git.Sync(ctx); err != nil {
			return nil, false, err
		}
		remote, _, err := repo.Git.Rev(ctx, repo.Git.RemoteRef(branchName(n)))
		if err != nil {
			return nil, false, err
		}
		if rr.moved(remote, head) {
			return nil, false, r.restart(ctx, ro, n)
		}
		// GitHub refuses a push that rewrites the branch, and it would refuse it again on every retry.
		kept, err := repo.Git.IsAncestor(ctx, rr.Head, head)
		if err != nil {
			return nil, false, err
		}
		if !kept {
			return nil, false, r.stuck(ctx, ro, n, "The AI rewrote commits that were already on the branch"+rr.where()+", so they cannot be pushed.\n\n"+said)
		}
		heads = append(heads, head)
	}
	return heads, true, nil
}

const badResult = "the AI's result was not in the expected form"

// result reads a finished run's structured result into out. ok is false when the run failed or
// starts over instead, and err is set only when that step failed.
func (r *runs) result(ctx context.Context, ro role, n int, out any) (ok bool, err error) {
	name := r.name(ro, n)
	res, failure, limited, err := r.outcome(name)
	if limited {
		// The reading the start passed missed the limit. A new one keeps the gate closed until the reset.
		if r.gate != nil {
			r.gate.markStale()
		}
		return false, r.restart(ctx, ro, n)
	}
	if err != nil {
		slog.ErrorContext(ctx, "exomecha could not read the run", logging.Sandbox, name, logging.Err, err)
		// The AI can write the run's files, so reading them again is not sure to work.
		return false, r.stuck(ctx, ro, n, fmt.Sprintf("exomecha could not read the run's result: %s. The error is in exomecha's log.", failure))
	}
	if failure != "" {
		return false, r.failed(ctx, ro, n, failure)
	}
	if err := json.Unmarshal(res, out); err != nil {
		return false, r.failed(ctx, ro, n, badResult)
	}
	return true, nil
}

// outcome reads a finished run. failure says why the run failed, in words fit for a comment. err
// is set when exomecha could not read the run, and failure then says why. The AI can write the
// run's files, so that can be the AI's doing as much as exomecha's. limited is set when the run
// ended at the Claude usage limit.
func (r *runs) outcome(name string) (res json.RawMessage, failure string, limited bool, err error) {
	b, err := readCtl(r.ctl(name), "exit", 64)
	if err != nil {
		return res, "the file that records how claude ended could not be read", false, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return res, "the file that records how claude ended holds no exit status", false, fmt.Errorf("exit file holds %q", b)
	}
	f, err := openCtl(r.ctl(name), "stream.jsonl")
	if err != nil {
		return res, "claude's output file could not be read", false, err
	}
	defer f.Close()
	s, err := agent.ReadStream(io.LimitReader(f, streamLimit))
	if err != nil {
		return res, "claude's output file could not be read", false, err
	}
	res, err = s.Result()
	if (err != nil || code != 0) && s.Rejected {
		return res, "", true, nil
	}
	switch {
	case s.PermissionMode != "auto":
		return res, "claude did not run in auto permission mode", false, nil
	case err != nil:
		return res, "claude ended with an error or without a result", false, nil
	case code != 0:
		return res, "claude ended with an error", false, nil
	}
	return res, "", false, nil
}

// tailCtl reads at most the last limit bytes of a file in ctl.
func tailCtl(ctl, file string, limit int64) ([]byte, error) {
	f, err := openCtl(ctl, file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > limit {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, limit))
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
