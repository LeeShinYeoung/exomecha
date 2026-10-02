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

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
	"github.com/LeeShinYeoung/coworker/internal/sandbox"
)

const (
	// Where the Task's repository lives inside a sandbox. It is on the sandbox's own disk, not in
	// the directory shared with this machine, so coworker never runs git in a .git the AI controls.
	repoDir = "/home/agent/repo"

	// A foreground command is capped at 10 minutes, so a run that is working still writes within this.
	stallAfter  = 15 * time.Minute
	maxFailures = 3
	minBackoff  = time.Minute
	maxBackoff  = 30 * time.Minute

	// A create takes seconds. The first one may pull sbx's own template, which takes longer.
	createTimeout = 15 * time.Minute

	// Far more than any run writes. It only bounds what a misbehaving run can make coworker read.
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

// errNoSlot means the slots filled up between ready and the create, which only a sandbox made
// outside this coworker can do.
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
	start started
}

// started is what coworker records once claude is running. It lives outside the directory the
// sandbox shares, so the AI cannot change what its work is checked against.
type started struct {
	Before    string   `json:"before"` // the commit claude started from
	Remote    string   `json:"remote"` // the Task branch on GitHub at the start, "" when there was none
	Base      string   `json:"base"`   // the base branch commit the sandbox got
	PR        int      `json:"pr,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// taskbox is what builder and resolver share: the sandboxes, the limit on how many run at once,
// and what coworker remembers about each run. Memory is lost on restart, and nothing depends on
// it except stall times and failure counts.
type taskbox struct {
	Deps
	gate *Gate
	now  func() time.Time

	// Held from counting the sandboxes until the new one exists. It also guards starting.
	slots sync.Mutex
	// The sandboxes whose starts hold a slot, from ready until release.
	starting map[string]bool

	mu    sync.Mutex
	seen  map[string]progress
	tries map[string]*tries
}

type progress struct {
	size int64
	mod  time.Time
	at   time.Time // when coworker first saw this size and time
}

type tries struct {
	failures int // runs that failed in a row
	strikes  int // failures and errors in a row, for the backoff
	next     time.Time
}

func newTaskbox(d Deps, gate *Gate) *taskbox {
	return &taskbox{
		Deps:     d,
		gate:     gate,
		now:      time.Now,
		starting: map[string]bool{},
		seen:     map[string]progress{},
		tries:    map[string]*tries{},
	}
}

func (t *taskbox) name(task int, role string) string {
	return sandbox.Name(t.SandboxPrefix, task, role)
}

// dir holds what coworker keeps about one sandbox on this machine.
func (t *taskbox) dir(name string) string { return filepath.Join(t.SandboxDir, name) }

// ctl is the sandbox's workspace: the part of dir the sandbox sees, at the same path. The AI can
// write anything there.
func (t *taskbox) ctl(name string) string { return filepath.Join(t.dir(name), "ctl") }

func (t *taskbox) read(ctx context.Context, name string) (run, error) {
	boxes, err := t.Sandboxes.List(ctx)
	if err != nil {
		return run{}, err
	}
	i := slices.IndexFunc(boxes, func(b sandbox.Box) bool { return b.Name == name })
	var r run
	b, err := os.ReadFile(filepath.Join(t.dir(name), "start.json"))
	if errors.Is(err, fs.ErrNotExist) {
		_, err := os.Lstat(t.dir(name))
		switch {
		case i < 0 && errors.Is(err, fs.ErrNotExist):
			r.state = none
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return r, err
		default:
			r.state = preparing
		}
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r.start); err != nil {
		return r, fmt.Errorf("read start of %s: %w", name, err)
	}
	if i < 0 {
		r.state = gone
		return r, nil
	}
	if _, err := os.Lstat(filepath.Join(t.ctl(name), "exit")); err == nil {
		r.state = finished
		return r, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return r, err
	}
	if !boxes[i].Running() {
		r.state = dead
		return r, nil
	}
	quiet, err := t.quiet(name)
	if err != nil {
		return r, err
	}
	r.state = running
	if quiet {
		r.state = stalled
	}
	return r, nil
}

// quiet reports whether the run's output has not changed for stallAfter, as coworker saw it.
// It counts only time coworker watched, on a clock that stands still while this machine sleeps.
func (t *taskbox) quiet(name string) (bool, error) {
	size, mod := int64(-1), time.Time{}
	info, err := os.Lstat(filepath.Join(t.ctl(name), "stream.jsonl"))
	if err == nil {
		size, mod = info.Size(), info.ModTime()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.seen[name]
	if !ok || p.size != size || !p.mod.Equal(mod) {
		t.seen[name] = progress{size: size, mod: mod, at: now}
		return false, nil
	}
	return now.Sub(p.at) >= stallAfter, nil
}

func (t *taskbox) due(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.tries[name]
	return !ok || !t.now().Before(tr.next)
}

// strike puts off the next attempt, twice as long as the last one each time in a row.
func (t *taskbox) strike(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.strikeLocked(name)
}

func (t *taskbox) strikeLocked(name string) *tries {
	tr := t.tries[name]
	if tr == nil {
		tr = &tries{}
		t.tries[name] = tr
	}
	tr.strikes++
	tr.next = t.now().Add(min(maxBackoff, minBackoff<<min(tr.strikes-1, 10)))
	return tr
}

func (t *taskbox) failures(name string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr := t.tries[name]; tr != nil {
		return tr.failures
	}
	return 0
}

// fail counts a failed run and returns how many failed in a row.
func (t *taskbox) fail(name string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.strikeLocked(name)
	tr.failures++
	return tr.failures
}

func (t *taskbox) forget(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tries, name)
}

// prune forgets the failures of role's Tasks that are not in keep, the Tasks still under that
// worker's labels.
func (t *taskbox) prune(role string, keep map[int]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.tries {
		if n, r, ok := sandbox.ParseTask(t.SandboxPrefix, name); ok && r == role && !keep[n] {
			delete(t.tries, name)
		}
	}
}

// found returns the Tasks that have a sandbox or a directory for role.
func (t *taskbox) found(ctx context.Context, role string) ([]int, error) {
	boxes, err := t.Sandboxes.List(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, b := range boxes {
		names = append(names, b.Name)
	}
	entries, err := os.ReadDir(t.SandboxDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var out []int
	for _, name := range names {
		if n, r, ok := sandbox.ParseTask(t.SandboxPrefix, name); ok && r == role && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// ready reports whether a new run may start: the usage gate is open and a sandbox slot is free.
// When it is, the slot is name's until release, so other starts count it while the template
// builds and the code goes in, long before the sandbox exists.
func (t *taskbox) ready(ctx context.Context, name string) (bool, error) {
	if t.gate != nil {
		if open, err := t.gate.Open(ctx); err != nil || !open {
			return false, err
		}
	}
	t.slots.Lock()
	defer t.slots.Unlock()
	if free, err := t.slotFree(ctx, name); err != nil || !free {
		return false, err
	}
	t.starting[name] = true
	return true, nil
}

func (t *taskbox) release(name string) {
	t.slots.Lock()
	defer t.slots.Unlock()
	delete(t.starting, name)
}

// slotFree counts every sandbox of this repository, stopped ones too, so the count stays right
// across restarts, and every start that holds a slot. self is the start asking. The caller holds
// slots.
func (t *taskbox) slotFree(ctx context.Context, self string) (bool, error) {
	boxes, err := t.Sandboxes.List(ctx)
	if err != nil {
		return false, err
	}
	// By name, since a started sandbox is still in starting until its start returns.
	used := map[string]bool{}
	for _, b := range boxes {
		// Not a plain prefix match: owner/repo-v2's names start with owner/repo's prefix too.
		if _, _, ok := sandbox.ParseTask(t.SandboxPrefix, b.Name); ok {
			used[b.Name] = true
		}
	}
	for name := range t.starting {
		used[name] = true
	}
	delete(used, self)
	return len(used) < t.SandboxLimit, nil
}

// launch is a run about to start.
type launch struct {
	task    int
	name    string
	branch  string
	start   started
	request agent.Request // Instructions and Schema. The input comes from input.
	// input runs once setup has run, and builds claude's input. It may fill in start, and it
	// returns a problem the way open does.
	input func(ctx context.Context, ranSetup bool, s *started) (input, problem string, err error)
}

// open creates the sandbox, sends the code in, runs setup, and starts claude. problem is set when
// only a person can fix what stopped it. On problem or error the caller removes the sandbox.
func (t *taskbox) open(ctx context.Context, l launch) (problem string, err error) {
	ref, hasTemplate, err := t.Environment.Template(ctx)
	if problem, err := needsPerson(l.task, err); problem != "" || err != nil {
		return problem, err
	}
	if !hasTemplate {
		ref = ""
	}
	hosts, err := t.Environment.AllowedHosts(ctx)
	if problem, err := needsPerson(l.task, err); problem != "" || err != nil {
		return problem, err
	}
	script, hasScript, err := t.Git.Show(ctx, l.start.Base, setupScript)
	if err != nil {
		return "", err
	}

	ctl := t.ctl(l.name)
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

	t.slots.Lock()
	free, err := t.slotFree(ctx, l.name)
	if err == nil && !free {
		err = errNoSlot
	}
	if err == nil {
		// Every other start waits on the slot lock, so a create that hangs must give up.
		createCtx, cancel := context.WithTimeout(ctx, createTimeout)
		err = t.Sandboxes.Create(createCtx, l.name, ref, ctl, deniedHosts)
		cancel()
	}
	t.slots.Unlock()
	if err != nil {
		return "", err
	}
	if err := t.Sandboxes.Allow(ctx, l.name, hosts); err != nil {
		return "", err
	}
	if _, err := t.git(ctx, l.name, "", "init", "-q", repoDir); err != nil {
		return "", err
	}
	// Before setup, so the project's script can set another.
	for _, kv := range [][2]string{{"user.name", t.GitName}, {"user.email", t.GitEmail}} {
		if _, err := t.git(ctx, l.name, repoDir, "config", kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if err := t.Git.SendTo(ctx, t.Sandboxes.GitURL(l.name, repoDir), l.start.Base, l.branch, l.start.Before); err != nil {
		return "", err
	}
	if _, err := t.git(ctx, l.name, repoDir, "checkout", "-q", l.branch); err != nil {
		return "", err
	}
	if hasScript {
		if problem, err := t.setup(ctx, l); problem != "" || err != nil {
			return problem, err
		}
	}

	input, problem, err := l.input(ctx, hasScript, &l.start)
	if problem != "" || err != nil {
		return problem, err
	}
	if err := os.WriteFile(filepath.Join(ctl, "input.md"), []byte(input), 0o644); err != nil {
		return "", err
	}
	args := append([]string{"sh", filepath.Join(ctl, "run.sh"), ctl, "claude"}, agent.StreamArgs(l.request)...)
	if err := t.Sandboxes.Start(ctx, l.name, repoDir, args...); err != nil {
		return "", err
	}
	if err := t.record(l.name, l.start); err != nil {
		return "", err
	}
	slog.Info("started claude in a sandbox", "task", l.task, "sandbox", l.name, "template", ref)
	return "", nil
}

func (t *taskbox) record(name string, s started) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	path := filepath.Join(t.dir(name), "start.json")
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
func (t *taskbox) git(ctx context.Context, name, dir string, args ...string) (string, error) {
	out, err := t.Sandboxes.Exec(ctx, name, dir, nil, append([]string{"git"}, args...)...)
	return string(out), err
}

const noCommit = "The AI left its repository with no commit checked out, so there is nothing to push."

// committed reports whether the sandbox's repository has a commit checked out. Without one a fetch
// fails the same way on every try, so it has to be caught before.
func (t *taskbox) committed(ctx context.Context, name string) (bool, error) {
	// rev-parse exits 1 for an unborn HEAD, which Exec would report as an error.
	out, err := t.Sandboxes.Exec(ctx, name, repoDir, nil, "sh", "-c", "git rev-parse -q --verify HEAD || true")
	return strings.TrimSpace(string(out)) != "", err
}

func (t *taskbox) fetch(ctx context.Context, name string) (string, error) {
	return t.Git.FetchFrom(ctx, t.Sandboxes.GitURL(name, repoDir), name)
}

// outcome reads a finished run. failure says why the run failed, in words fit for a comment, and
// detail adds what only the log gets. limited is set when the run ended at the Claude usage limit.
func (t *taskbox) outcome(name string, schema bool) (res agent.Result, failure, detail string, limited bool) {
	b, err := readCtl(t.ctl(name), "exit", 64)
	if err != nil {
		return res, "coworker could not read how claude ended", err.Error(), false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return res, "coworker could not read how claude ended", fmt.Sprintf("exit file holds %q", b), false
	}
	f, err := openCtl(t.ctl(name), "stream.jsonl")
	if err != nil {
		return res, "coworker could not read claude's output", err.Error(), false
	}
	defer f.Close()
	s, err := agent.ReadStream(io.LimitReader(f, streamLimit))
	if err != nil {
		return res, "coworker could not read claude's output", err.Error(), false
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
func (t *taskbox) tail(name, file string) string {
	f, err := openCtl(t.ctl(name), file)
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
func (t *taskbox) remove(ctx context.Context, name string, keep bool) error {
	if err := t.Sandboxes.Remove(ctx, name); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", name, err)
	}
	if keep {
		kept := filepath.Join(t.SandboxDir, "failed", name)
		err := os.RemoveAll(kept)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(kept), 0o755)
		}
		if err == nil {
			err = os.Rename(t.ctl(name), kept)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("keep the failed run's files", "sandbox", name, "err", err)
		} else if err == nil {
			slog.Info("kept the failed run's files", "sandbox", name, "dir", kept)
		}
	}
	if err := os.RemoveAll(t.dir(name)); err != nil {
		return err
	}
	// The ref only matters while a run is harvested, and the next fetch for this name replaces it.
	t.Git.DropRef(context.WithoutCancel(ctx), name)
	t.mu.Lock()
	delete(t.seen, name)
	t.mu.Unlock()
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

func failedReason(count int, why string) string {
	return fmt.Sprintf("The AI's run failed %d times in a row. The last time, %s. The details are in coworker's log.", count, why)
}
