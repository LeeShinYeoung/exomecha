package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

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
