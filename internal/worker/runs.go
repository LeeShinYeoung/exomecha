package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

const (
	// Where the Task's repository lives inside a sandbox. It is on the sandbox's own disk, not in
	// the directory shared with this machine, so exomecha never runs git in a .git the AI controls.
	repoDir = "/home/agent/repo"

	// A foreground command is capped at 10 minutes, so a run that is working still writes within this.
	stallAfter = 15 * time.Minute

	// A create takes seconds. The first one may pull sbx's own template, which takes longer.
	createTimeout = 15 * time.Minute

	// Far more than any run writes. It only bounds what a misbehaving run can make exomecha read.
	streamLimit = 256 << 20
	tailLimit   = 4000
)

// sbx forwards this machine's SSH agent into every sandbox, and GitHub takes SSH on this host's port 443.
var deniedHosts = []string{"ssh.github.com"}

// runScript starts claude inside the sandbox and records how it ended. ctl is the directory shared
// with this machine. The exit file appears only once claude has ended.
const runScript = `ctl=$1
shift
"$@" < "$ctl/input.md" > "$ctl/stream.jsonl" 2> "$ctl/stderr.log"
echo $? > "$ctl/exit.tmp"
# A reader must never see a half-written exit file.
mv "$ctl/exit.tmp" "$ctl/exit"
`

// errNoSlot means the slots filled up between reserve and the create, which only a sandbox made
// outside this exomecha can do.
var errNoSlot = errors.New("no free sandbox slot")

type state int

const (
	none      state = iota // no sandbox and no directory
	preparing              // a start that has not finished, or one that was cut off
	running
	stalled // running, but its output has not changed for stallAfter
	dead    // the sandbox stopped before claude ended
	gone    // the sandbox disappeared before claude ended
	finished
)

func (s state) String() string {
	return [...]string{"none", "preparing", "running", "stalled", "dead", "gone", "finished"}[s]
}

type run struct {
	state state
	in    inputs
}

// inputs is what exomecha records once claude is running. It lives outside the directory the
// sandbox shares, so the AI cannot change what its work is checked against.
type inputs struct {
	Head      string   `json:"head"`   // the commit claude started from
	Remote    string   `json:"remote"` // the Task branch on GitHub at the start, "" when there was none
	Base      string   `json:"base"`   // the base branch commit the sandbox got
	PR        int      `json:"pr,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// runs is what builder and resolver share: the sandboxes, the limit on how many run at once,
// and what exomecha remembers about each run. Memory is lost on restart, and nothing depends on
// it except stall times.
type runs struct {
	Deps
	gate *Gate
	now  func() time.Time

	// Held from counting the sandboxes until the new one exists. It also guards starting.
	slots sync.Mutex
	// The sandboxes whose starts hold a slot, from reserve until release.
	starting map[string]bool

	mu   sync.Mutex
	seen map[string]progress
}

type progress struct {
	size int64
	mod  time.Time
	at   time.Time // when exomecha first saw this size and time
}

func newRuns(d Deps, gate *Gate) *runs {
	return &runs{
		Deps:     d,
		gate:     gate,
		now:      time.Now,
		starting: map[string]bool{},
		seen:     map[string]progress{},
	}
}

func (r *runs) name(task int, role string) string {
	return sandbox.Name(r.SandboxPrefix, task, role)
}

// dir holds what exomecha keeps about one sandbox on this machine.
func (r *runs) dir(name string) string { return filepath.Join(r.SandboxDir, name) }

// ctl is the sandbox's workspace: the part of dir the sandbox sees, at the same path. The AI can
// write anything there.
func (r *runs) ctl(name string) string { return filepath.Join(r.dir(name), "ctl") }

func (r *runs) read(ctx context.Context, name string) (run, error) {
	boxes, err := r.Sandboxes.List(ctx)
	if err != nil {
		return run{}, err
	}
	i := slices.IndexFunc(boxes, func(b sandbox.Box) bool { return b.Name == name })
	var out run
	b, err := os.ReadFile(filepath.Join(r.dir(name), "start.json"))
	if errors.Is(err, fs.ErrNotExist) {
		_, err := os.Lstat(r.dir(name))
		switch {
		case i < 0 && errors.Is(err, fs.ErrNotExist):
			out.state = none
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return out, err
		default:
			out.state = preparing
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out.in); err != nil {
		return out, fmt.Errorf("read start of %s: %w", name, err)
	}
	if i < 0 {
		out.state = gone
		return out, nil
	}
	if _, err := os.Lstat(filepath.Join(r.ctl(name), "exit")); err == nil {
		out.state = finished
		return out, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return out, err
	}
	if !boxes[i].Running() {
		out.state = dead
		return out, nil
	}
	quiet, err := r.quiet(name)
	if err != nil {
		return out, err
	}
	out.state = running
	if quiet {
		out.state = stalled
	}
	return out, nil
}

// quiet reports whether the run's output has not changed for stallAfter, as exomecha saw it.
// It counts only time exomecha watched, on a clock that stands still while this machine sleeps.
func (r *runs) quiet(name string) (bool, error) {
	size, mod := int64(-1), time.Time{}
	info, err := os.Lstat(filepath.Join(r.ctl(name), "stream.jsonl"))
	if err == nil {
		size, mod = info.Size(), info.ModTime()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.seen[name]
	if !ok || p.size != size || !p.mod.Equal(mod) {
		r.seen[name] = progress{size: size, mod: mod, at: now}
		return false, nil
	}
	return now.Sub(p.at) >= stallAfter, nil
}

// found returns the Tasks that have a sandbox or a directory for role.
func (r *runs) found(ctx context.Context, role string) ([]int, error) {
	boxes, err := r.Sandboxes.List(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, b := range boxes {
		names = append(names, b.Name)
	}
	entries, err := os.ReadDir(r.SandboxDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var out []int
	for _, name := range names {
		if n, got, ok := sandbox.Parse(r.SandboxPrefix, name); ok && got == role && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// reserve reports whether a new run may start: the usage gate is open and a sandbox slot is free.
// When it is, the slot is name's until release, so other starts count it while the template
// builds and the code goes in, long before the sandbox exists.
func (r *runs) reserve(ctx context.Context, name string) (bool, error) {
	if r.gate != nil {
		if open, err := r.gate.Open(ctx); err != nil || !open {
			return false, err
		}
	}
	r.slots.Lock()
	defer r.slots.Unlock()
	if free, err := r.slotFree(ctx, name); err != nil || !free {
		return false, err
	}
	r.starting[name] = true
	return true, nil
}

func (r *runs) release(name string) {
	r.slots.Lock()
	defer r.slots.Unlock()
	delete(r.starting, name)
}

// slotFree counts every sandbox of this repository, stopped ones too, so the count stays right
// across restarts, and every start that holds a slot. self is the start asking. The caller holds
// slots.
func (r *runs) slotFree(ctx context.Context, self string) (bool, error) {
	boxes, err := r.Sandboxes.List(ctx)
	if err != nil {
		return false, err
	}
	// By name, since a started sandbox is still in starting until its start returns.
	used := map[string]bool{}
	for _, b := range boxes {
		// Not a plain prefix match: owner/repo-v2's names start with owner/repo's prefix too.
		if _, role, ok := sandbox.Parse(r.SandboxPrefix, b.Name); ok && (role == builderRole || role == resolverRole) {
			used[b.Name] = true
		}
	}
	for name := range r.starting {
		used[name] = true
	}
	delete(used, self)
	return len(used) < r.SandboxLimit, nil
}

// launch is a run about to start.
type launch struct {
	task    int
	name    string
	branch  string
	in      inputs
	request agent.Request // Instructions and Schema. The input comes from input.
	// input runs once setup has run, and builds claude's input. It may fill in the inputs, and
	// it returns a problem the way open does.
	input func(ctx context.Context, ranSetup bool, in *inputs) (input, problem string, err error)
}

// open creates the sandbox, sends the code in, runs setup, and starts claude. problem is set when
// only a person can fix what stopped it. On problem or error the caller removes the sandbox.
func (r *runs) open(ctx context.Context, l launch) (problem string, err error) {
	ref, err := r.Environment.Template(ctx)
	if problem, err := needsPerson(l.task, err); problem != "" || err != nil {
		return problem, err
	}
	hosts, err := r.Environment.AllowedHosts(ctx)
	if problem, err := needsPerson(l.task, err); problem != "" || err != nil {
		return problem, err
	}
	script, hasScript, err := r.Git.Show(ctx, l.in.Base, setupScript)
	if err != nil {
		return "", err
	}

	ctl := r.ctl(l.name)
	if err := os.MkdirAll(ctl, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(ctl, "run.sh"), []byte(runScript), 0o644); err != nil {
		return "", err
	}
	if hasScript {
		if err := os.WriteFile(filepath.Join(ctl, "setup.sh"), []byte(script), 0o644); err != nil {
			return "", err
		}
	}

	r.slots.Lock()
	free, err := r.slotFree(ctx, l.name)
	if err == nil && !free {
		err = errNoSlot
	}
	if err == nil {
		// Every other start waits on the slot lock, so a create that hangs must give up.
		createCtx, cancel := context.WithTimeout(ctx, createTimeout)
		err = r.Sandboxes.Create(createCtx, l.name, ref, ctl, deniedHosts)
		cancel()
	}
	r.slots.Unlock()
	if err != nil {
		return "", err
	}
	if err := r.Sandboxes.Allow(ctx, l.name, hosts); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, l.name, "", "init", "-q", repoDir); err != nil {
		return "", err
	}
	// Before setup, so the project's script can set another.
	for _, kv := range [][2]string{{"user.name", r.GitName}, {"user.email", r.GitEmail}} {
		if _, err := r.git(ctx, l.name, repoDir, "config", kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if err := r.Git.SendTo(ctx, r.Sandboxes.GitURL(l.name, repoDir), l.in.Base, l.branch, l.in.Head); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, l.name, repoDir, "checkout", "-q", l.branch); err != nil {
		return "", err
	}
	if hasScript {
		if problem, err := r.setup(ctx, l); problem != "" || err != nil {
			return problem, err
		}
	}

	input, problem, err := l.input(ctx, hasScript, &l.in)
	if problem != "" || err != nil {
		return problem, err
	}
	if err := os.WriteFile(filepath.Join(ctl, "input.md"), []byte(input), 0o644); err != nil {
		return "", err
	}
	args := append([]string{"sh", filepath.Join(ctl, "run.sh"), ctl, "claude"}, agent.StreamArgs(l.request)...)
	if err := r.Sandboxes.Start(ctx, l.name, repoDir, args...); err != nil {
		return "", err
	}
	if err := r.record(l.name, l.in); err != nil {
		return "", err
	}
	slog.Info("started claude in a sandbox", "task", l.task, "sandbox", l.name, "template", ref)
	return "", nil
}

func (r *runs) record(name string, in inputs) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	path := filepath.Join(r.dir(name), "start.json")
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// needsPerson turns an environment error that only a person can fix into the reason to give them.
// Build output stays in the log, never in the comment.
func needsPerson(task int, err error) (string, error) {
	var person interface{ Why() string }
	if errors.As(err, &person) {
		slog.Warn("sandbox environment needs a person", "task", task, "err", err)
		return person.Why(), nil
	}
	return "", err
}

// git runs git in the sandbox, in dir if it is not empty.
func (r *runs) git(ctx context.Context, name, dir string, args ...string) (string, error) {
	out, err := r.Sandboxes.Exec(ctx, name, dir, nil, append([]string{"git"}, args...)...)
	return string(out), err
}

const noCommit = "The AI left its repository with no commit checked out, so there is nothing to push."

// committed reports whether the sandbox's repository has a commit checked out. Without one a fetch
// fails the same way on every try, so it has to be caught before.
func (r *runs) committed(ctx context.Context, name string) (bool, error) {
	// rev-parse exits 1 for an unborn HEAD, which Exec would report as an error.
	out, err := r.Sandboxes.Exec(ctx, name, repoDir, nil, "sh", "-c", "git rev-parse -q --verify HEAD || true")
	return strings.TrimSpace(string(out)) != "", err
}

func (r *runs) fetch(ctx context.Context, name string) (string, error) {
	return r.Git.FetchFrom(ctx, r.Sandboxes.GitURL(name, repoDir), name)
}

// outcome reads a finished run. failure says why the run failed, in words fit for a comment, and
// detail adds what only the log gets. limited is set when the run ended at the Claude usage limit.
func (r *runs) outcome(name string, schema bool) (res agent.Result, failure, detail string, limited bool) {
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
	res, err = s.Result(schema)
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

// tail returns the end of a file in the sandbox's directory, for the log.
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

// remove deletes the sandbox, which stops everything in it, and then its directory. keep moves
// the run's files to failed/<name> first, for a person to read.
func (r *runs) remove(ctx context.Context, name string, keep bool) error {
	if err := r.Sandboxes.Remove(ctx, name); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", name, err)
	}
	if keep {
		kept := filepath.Join(r.SandboxDir, "failed", name)
		err := os.RemoveAll(kept)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(kept), 0o755)
		}
		if err == nil {
			err = os.Rename(r.ctl(name), kept)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("keep the failed run's files", "sandbox", name, "err", err)
		} else if err == nil {
			slog.Info("kept the failed run's files", "sandbox", name, "dir", kept)
		}
	}
	if err := os.RemoveAll(r.dir(name)); err != nil {
		return err
	}
	// The ref only matters while a run is harvested, and the next fetch for this name replaces it.
	r.Git.DropRef(context.WithoutCancel(ctx), name)
	r.mu.Lock()
	delete(r.seen, name)
	r.mu.Unlock()
	slog.Info("removed sandbox", "sandbox", name)
	return nil
}

// holds reports whether the Task is an open Task whose status labels are all among statuses.
// SetStatus adds the new label before it removes the old one, so any other status means the Task
// is on its way somewhere else, to stuck or back to a person.
func holds(task github.Issue, statuses ...label.Status) bool {
	if !task.Open || !task.HasLabel(label.Task) {
		return false
	}
	found := false
	for _, l := range task.Labels {
		if !label.IsStatus(l) {
			continue
		}
		if !slices.Contains(statuses, label.Status(l)) {
			return false
		}
		found = true
	}
	return found
}

func failedReason(why string) string {
	return fmt.Sprintf("The AI's run failed because %s. The details are in exomecha's log. To try again, relabel this issue as below. You don't need to answer.", why)
}
