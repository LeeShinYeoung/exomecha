package worker

import (
	"cmp"
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
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

const (
	// Where the PRD's repository lives inside a sandbox. It is on the sandbox's own disk, not in
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

// A worker's table gives the action for each place its issue can be in and each state of its run.
type (
	table map[place]row
	row   [finished + 1]action // one column per state, so finished has to stay the last state
)

type action int

const (
	idle    action = iota
	begin          // the worker's start
	harvest        // the worker's collect
	fail           // stuck at once, keeping the run's files
	drop           // remove the run and leave the labels alone
	restart        // remove the run without a failure, and builder's PRD goes back to ready
)

// place is where an issue is, as one worker sees it on GitHub.
type place int

const (
	away    place = iota // closed, another worker's, or given a second status by a person
	ready                // the worker may start a run
	working              // a PRD builder is working on
	clean                // a PRD in review whose PR does not conflict, or that has no PR
	waiting              // a PRD in review whose PR GitHub or CI has not checked yet
	merged               // a PRD in review or approved whose PR was merged
	closed               // a PRD in review or approved whose PR was closed without a merge
	failing              // a PRD in review whose PR's CI failed
	stale                // an approved PRD whose PR conflicts
)

type role struct {
	name   string
	schema string
	// A project run gets the project's template, allowed hosts, setup script and a commit identity.
	// Any other run is a reader's, which gets only the code in sbx's own template.
	project bool
	// from is the issue's status while the run goes. A run that needs a person moves the issue
	// from it to stuck, and the comment asks the person to relabel it back.
	from, back label.Status
	// ready is builder's alone: a start moves the PRD from ready to from, and a start that is
	// undone or starts over moves it back.
	ready label.Status
	// tier picks the model of the role's runs, unless the spec names one.
	tier label.Tier
}

type run struct {
	state state
	in    inputs
	box   bool // the sandbox exists
}

// inputs is what exomecha records once claude is running. It lives outside the directory the
// sandbox shares, so the AI cannot change what its work is checked against.
type inputs struct {
	Base      string   `json:"base"`   // the base branch commit the sandbox got
	Head      string   `json:"head"`   // the commit claude started from
	Remote    string   `json:"remote"` // the PRD branch on GitHub at the start, "" when there was none
	Conflicts []string `json:"conflicts,omitempty"`
}

// moved reports whether the PRD branch changed during the run: now, its head on GitHub, is neither
// the one at the start nor pushed, the commit exomecha took from the run. pushed is already there
// when exomecha pushed it and a later step failed.
func (in inputs) moved(now, pushed string) bool {
	return now != in.Remote && now != pushed
}

// runs is what the workers share: the sandboxes, the limits on how many run at once, and what
// exomecha remembers about each run. Memory is lost on restart, and nothing depends on
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

func (r *runs) name(ro role, n int) string {
	return sandbox.Name(r.SandboxPrefix, n, ro.name)
}

// dir holds what exomecha keeps about one sandbox on this machine.
func (r *runs) dir(name string) string { return filepath.Join(r.SandboxDir, name) }

// ctl is the sandbox's workspace: the part of dir the sandbox sees, at the same path. The AI can
// write anything there.
func (r *runs) ctl(name string) string { return filepath.Join(r.dir(name), "ctl") }

func (r *runs) read(ctx context.Context, ro role, n int) (run, error) {
	name := r.name(ro, n)
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
	out.box = i >= 0
	exited := true
	if _, err := os.Lstat(filepath.Join(r.ctl(name), "exit")); errors.Is(err, fs.ErrNotExist) {
		exited = false
	} else if err != nil {
		return out, err
	}
	switch {
	// A reader's result is all in its directory, so its sandbox may already be gone.
	case exited && (out.box || !ro.project):
		out.state = finished
		return out, nil
	case !out.box:
		out.state = gone
		return out, nil
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
	return now.Sub(p.at) >= r.stallTime(), nil
}

// pick returns picked and every issue that has a sandbox or a directory for ro, in ascending order.
func (r *runs) pick(ctx context.Context, ro role, picked []int) ([]int, error) {
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
	out := picked
	for _, name := range names {
		if n, got, ok := sandbox.Parse(r.SandboxPrefix, name); ok && got == ro.name {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// reserve reports whether a new run of ro may start: the usage gate is open and a sandbox slot of
// its kind is free. When it is, the slot is name's until release, so other starts count it while
// the template builds and the code goes in, long before the sandbox exists.
func (r *runs) reserve(ctx context.Context, ro role, name string) (bool, error) {
	if r.gate != nil {
		if open, err := r.gate.Open(ctx); err != nil || !open {
			return false, err
		}
	}
	r.slots.Lock()
	defer r.slots.Unlock()
	if free, err := r.slotFree(ctx, ro, name); err != nil || !free {
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

// roles holds every role, so that a sandbox's name tells its kind.
var roles = []role{builderRole, resolverRole, reviewerRole}

// slotFree counts every sandbox of this repository of ro's kind, stopped ones too, so the count
// stays right across restarts, and every start of that kind that holds a slot. self is the start
// asking. The caller holds slots.
func (r *runs) slotFree(ctx context.Context, ro role, self string) (bool, error) {
	boxes, err := r.Sandboxes.List(ctx)
	if err != nil {
		return false, err
	}
	// By name, since a started sandbox is still in starting until its start returns.
	used := map[string]bool{}
	for _, b := range boxes {
		used[b.Name] = true
	}
	for name := range r.starting {
		used[name] = true
	}
	delete(used, self)
	count := 0
	for name := range used {
		// Not a plain prefix match: owner/repo-v2's names start with owner/repo's prefix too.
		_, got, ok := sandbox.Parse(r.SandboxPrefix, name)
		i := slices.IndexFunc(roles, func(o role) bool { return o.name == got })
		// A role not in roles counts as a reader's.
		if ok && (i >= 0 && roles[i].project) == ro.project {
			count++
		}
	}
	limit := r.ReadLimit
	if ro.project {
		limit = r.ProjectLimit
	}
	return count < limit, nil
}

// step takes the action row gives for the state of the run of ro for issue n. start begins a run,
// and collect takes what a finished run did.
func (r *runs) step(ctx context.Context, ro role, n int, row row,
	start func(context.Context) error, collect func(context.Context, run) error) error {
	name := r.name(ro, n)
	run, err := r.read(ctx, ro, n)
	if err != nil {
		return err
	}
	// The result stays in the directory, so the slot comes back while the result waits to be collected.
	if run.state == finished && run.box && !ro.project {
		if err := r.Sandboxes.Remove(ctx, name); err != nil {
			return fmt.Errorf("remove sandbox %s: %w", name, err)
		}
		slog.Info("removed a finished reader's sandbox", "sandbox", name)
	}
	switch row[run.state] {
	case begin:
		return start(ctx)
	case harvest:
		return collect(ctx, run)
	case fail:
		return r.failed(ctx, ro, n, r.why(run.state), "")
	case drop:
		return r.drop(ctx, ro, n)
	case restart:
		return r.restart(ctx, ro, n, r.why(run.state))
	}
	return nil
}

func (r *runs) stallTime() time.Duration { return cmp.Or(r.StallAfter, stallAfter) }

// why says what happened to a run in state s, for the comment on a failure or the log of a restart.
func (r *runs) why(s state) string {
	switch s {
	case none:
		return "the issue had no run"
	case preparing:
		// Only a start holds the issue's lock while preparing, so this one was cut off.
		return "its start was cut off"
	case stalled:
		return fmt.Sprintf("claude's output did not change for %d minutes", int(r.stallTime().Minutes()))
	case dead:
		return "the sandbox stopped while claude was running"
	case gone:
		return "the sandbox was removed while claude was running"
	}
	return s.String()
}

type spec struct {
	branch string // checked out in the run's repository, and set to head
	base   string // the base branch commit
	head   string // the commit claude starts from
	remote string // the PRD branch on GitHub, "" when there is none
	// merge merges the base branch after setup and lists the conflicts in the input.
	merge bool
	input string     // claude's input. open adds what setup and the merge did.
	tier  label.Tier // "" for the role's tier
}

// start starts a run of ro for issue n, when the usage gate is open and a slot is free. build
// says what goes in. A start that does not get through is undone: a problem only a person can fix
// sends the issue to stuck, and an error leaves builder's PRD ready and returns the error.
func (r *runs) start(ctx context.Context, ro role, n int, build func(context.Context) (spec, error)) error {
	name := r.name(ro, n)
	if ready, err := r.reserve(ctx, ro, name); err != nil || !ready {
		return err
	}
	defer r.release(name)
	if ro.ready != label.None {
		if err := r.GitHub.SetStatus(ctx, n, ro.ready, ro.from); err != nil {
			return err
		}
	}
	s, err := build(ctx)
	problem := ""
	if err == nil {
		problem, err = r.open(ctx, ro, n, s)
	}
	if problem == "" && err == nil {
		return nil
	}
	// Undo the start even when shutdown cancelled ctx.
	cleanup := context.WithoutCancel(ctx)
	if problem != "" {
		if err := r.remove(cleanup, name, false); err != nil {
			return err
		}
		return toStuck(cleanup, r.GitHub, n, ro.from, ro.back, problem)
	}
	if rerr := r.restart(cleanup, ro, n, err.Error()); rerr != nil {
		return errors.Join(err, rerr)
	}
	if errors.Is(err, errNoSlot) {
		return nil
	}
	return err
}

// open creates the sandbox, sends the code in, runs setup, and starts claude. problem is set when
// only a person can fix what stopped it. On problem or error the caller removes the sandbox.
func (r *runs) open(ctx context.Context, ro role, n int, s spec) (problem string, err error) {
	tier := cmp.Or(s.tier, ro.tier)
	model, ok := r.Models[tier]
	if !ok {
		return "", fmt.Errorf("no model for %s", tier)
	}
	ref, problem, err := r.create(ctx, ro, n)
	if problem != "" || err != nil {
		return problem, err
	}
	if err := r.send(ctx, ro, n, s); err != nil {
		return "", err
	}
	in, input := inputs{Base: s.base, Head: s.head, Remote: s.remote}, s.input
	if ro.project {
		note, problem, err := r.setup(ctx, ro, n, s)
		if problem != "" || err != nil {
			return problem, err
		}
		input += note
	}
	// After setup, not before: conflict markers in a manifest or lockfile would break an install.
	if s.merge {
		var note string
		if in.Conflicts, note, problem, err = r.merge(ctx, ro, n); problem != "" || err != nil {
			return problem, err
		}
		input += note
	}
	if err := r.launch(ctx, ro, n, model, input, in); err != nil {
		return "", err
	}
	slog.Info("started claude in a sandbox", "issue", n, "sandbox", r.name(ro, n), "template", ref,
		"tier", tier, "model", model.Name, "effort", model.Effort)
	return "", nil
}

// create makes the run's sandbox, from the project's template and with its allowed hosts for a
// project run, and from sbx's own template ("" ref) for a reader's.
func (r *runs) create(ctx context.Context, ro role, n int) (ref, problem string, err error) {
	var hosts []string
	if ro.project {
		ref, err = r.Environment.Template(ctx)
		if problem, err := needsPerson(n, err); problem != "" || err != nil {
			return "", problem, err
		}
		hosts, err = r.Environment.AllowedHosts(ctx)
		if problem, err := needsPerson(n, err); problem != "" || err != nil {
			return "", problem, err
		}
	}
	name := r.name(ro, n)
	if err := os.MkdirAll(r.ctl(name), 0o755); err != nil {
		return "", "", err
	}
	r.slots.Lock()
	free, err := r.slotFree(ctx, ro, name)
	if err == nil && !free {
		err = errNoSlot
	}
	if err == nil {
		// Every other start waits on the slot lock, so a create that hangs must give up.
		createCtx, cancel := context.WithTimeout(ctx, createTimeout)
		err = r.Sandboxes.Create(createCtx, name, ref, r.ctl(name), deniedHosts)
		cancel()
	}
	r.slots.Unlock()
	if err == nil && ro.project {
		err = r.Sandboxes.Allow(ctx, name, hosts)
	}
	return ref, "", err
}

// send puts the code in the run's repository: the base branch's commit as origin/<base>, and
// s.head checked out as s.branch.
func (r *runs) send(ctx context.Context, ro role, n int, s spec) error {
	name := r.name(ro, n)
	if _, err := r.git(ctx, name, "", "init", "-q", repoDir); err != nil {
		return err
	}
	if ro.project {
		// Before setup, so the project's script can set another.
		for _, kv := range [][2]string{{"user.name", r.GitName}, {"user.email", r.GitEmail}} {
			if _, err := r.git(ctx, name, repoDir, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	if err := r.Git.SendTo(ctx, r.Sandboxes.GitURL(name, repoDir), s.base, s.branch, s.head); err != nil {
		return err
	}
	checkout := []string{"checkout", "-q", s.branch}
	if s.branch == "" {
		// Detached, since SendTo sent no branch: git refuses a push to the branch checked out, and
		// a new repository's unborn branch has whatever name the image's git config gives it.
		checkout = []string{"checkout", "-q", "--detach", r.Git.BaseRef()}
	}
	_, err := r.git(ctx, name, repoDir, checkout...)
	return err
}

func (r *runs) launch(ctx context.Context, ro role, n int, model agent.Model, input string, in inputs) error {
	name := r.name(ro, n)
	ctl := r.ctl(name)
	for file, content := range map[string]string{"run.sh": runScript, "input.md": input} {
		if err := os.WriteFile(filepath.Join(ctl, file), []byte(content), 0o644); err != nil {
			return err
		}
	}
	args := append([]string{"sh", filepath.Join(ctl, "run.sh"), ctl, "claude"}, agent.Args(prompt(ro.name), ro.schema, model)...)
	if err := r.Sandboxes.Start(ctx, name, repoDir, args...); err != nil {
		return err
	}
	return r.record(name, in)
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
func needsPerson(prd int, err error) (string, error) {
	var person interface{ Why() string }
	if errors.As(err, &person) {
		slog.Warn("sandbox environment needs a person", "prd", prd, "err", err)
		return person.Why(), nil
	}
	return "", err
}

func (r *runs) git(ctx context.Context, name, dir string, args ...string) (string, error) {
	out, err := r.Sandboxes.Exec(ctx, name, dir, nil, append([]string{"git"}, args...)...)
	return string(out), err
}

func (r *runs) sh(ctx context.Context, ro role, n int, script string, args ...string) (string, error) {
	out, err := r.Sandboxes.Exec(ctx, r.name(ro, n), repoDir, nil, append([]string{"sh", "-c", script, "sh"}, args...)...)
	return string(out), err
}

func (r *runs) changes(ctx context.Context, name string) (string, error) {
	out, err := r.git(ctx, name, repoDir, "status", "--porcelain")
	// Not TrimSpace: a line for an unstaged change starts with a space.
	return strings.TrimRight(out, "\n"), err
}

// splitStatus reads the output of a script that ends with `echo $?`: what came before, and the
// exit status the echo printed.
func splitStatus(out string) (output string, code int, err error) {
	text := strings.TrimRight(out, "\n")
	output, last := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		output, last = text[:i], text[i+1:]
	}
	code, err = strconv.Atoi(last)
	if err != nil {
		return "", 0, fmt.Errorf("read the exit status: %q", last)
	}
	return output, code, nil
}

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

func (r *runs) done(ctx context.Context, ro role, n int) error {
	return r.remove(ctx, r.name(ro, n), false)
}

func (r *runs) stuck(ctx context.Context, ro role, n int, reason string) error {
	if err := toStuck(ctx, r.GitHub, n, ro.from, ro.back, reason); err != nil {
		return err
	}
	return r.remove(ctx, r.name(ro, n), false)
}

// failed sends the issue to stuck at once and keeps the run's files for a person to read.
func (r *runs) failed(ctx context.Context, ro role, n int, why, detail string) error {
	name := r.name(ro, n)
	slog.Warn("run failed", "role", ro.name, "issue", n, "sandbox", name, "why", why, "detail", detail, "stderr", r.tail(name, "stderr.log"))
	// Stuck goes first. With the run gone and the label as it was, a failed GitHub call would start another run.
	if err := toStuck(ctx, r.GitHub, n, ro.from, ro.back,
		fmt.Sprintf("The AI's run failed because %s. The details are in exomecha's log.", why)); err != nil {
		return err
	}
	return r.remove(ctx, name, true)
}

// restart removes the run without counting it as a failure. builder's PRD goes back to ready.
func (r *runs) restart(ctx context.Context, ro role, n int, why string) error {
	slog.Info("starting a run over", "role", ro.name, "issue", n, "why", why)
	if err := r.remove(ctx, r.name(ro, n), false); err != nil {
		return err
	}
	if ro.ready != label.None {
		return r.GitHub.SetStatus(ctx, n, ro.from, ro.ready)
	}
	return nil
}

// drop removes a run its issue no longer needs, and leaves the labels to whoever moved them.
func (r *runs) drop(ctx context.Context, ro role, n int) error {
	slog.Info("removing a run its issue no longer needs", "role", ro.name, "issue", n)
	return r.remove(ctx, r.name(ro, n), false)
}
