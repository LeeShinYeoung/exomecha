package worker

import (
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

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/logging"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

const (
	// The home directory of sbx's claude templates.
	homeDir = "/home/agent"
	// Where the PRD's repository lives inside a sandbox. It is on the sandbox's own disk, not in
	// the directory shared with this machine, so exomecha never runs git in a .git the AI controls.
	repoDir = homeDir + "/repo"
	// claude's user settings, under the project's own settings.
	claudeHome = homeDir + "/.claude"

	// A command is capped at agent.ForegroundLimit, so a run that is working still writes within this.
	stallAfter = agent.ForegroundLimit + 5*time.Minute

	// A create takes seconds. The first one may pull sbx's own template, which takes longer.
	createTimeout = 15 * time.Minute

	// Far more than any run writes. It only bounds what a misbehaving run can make exomecha read.
	streamLimit = 256 << 20
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
	fail           // stuck at once, keeping what the run printed
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
	// model picks the role's own from the models of the PRD's tier.
	model func(Models) agent.Model
}

type run struct {
	state state
	in    inputs
	box   bool // the sandbox exists
}

// inputs is what exomecha records once claude is running. It lives outside the directory the
// sandbox shares, so the AI cannot change what its work is checked against.
type inputs struct {
	Repos []repoRun `json:"repos"`
}

// repoRun is one repository of a run.
type repoRun struct {
	Repo   string `json:"repo"`   // owner/name
	Dir    string `json:"dir"`    // where it is in the sandbox
	Base   string `json:"base"`   // the base branch commit the sandbox got
	Head   string `json:"head"`   // the commit claude started from
	Remote string `json:"remote"` // the PRD branch on GitHub at the start, "" when there was none
	// Merge is set when the run merges the base branch here, and Conflicts lists what conflicted.
	Merge     bool     `json:"merge,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// moved reports whether the PRD branch changed during the run: now, its head on GitHub, is neither
// the one at the start nor pushed, the commit exomecha took from the run. pushed is already there
// when exomecha pushed it and a later step failed.
func (r repoRun) moved(now, pushed string) bool {
	return now != r.Remote && now != pushed
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
	return now.Sub(p.at) >= stallAfter, nil
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
		if n, got, ok := sandbox.ParseName(r.SandboxPrefix, name); ok && got == ro.name {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// step takes the action tableRow gives for the state of the run of ro for issue n. start begins a
// run, and collect takes what a finished run did.
func (r *runs) step(ctx context.Context, ro role, n int, tableRow row,
	start func(context.Context) error, collect func(context.Context, run) error) error {
	name := r.name(ro, n)
	current, err := r.read(ctx, ro, n)
	if err != nil {
		return err
	}
	// The result stays in the directory, so the slot comes back while the result waits to be collected.
	if current.state == finished && current.box && !ro.project {
		if err := r.Sandboxes.Remove(ctx, name); err != nil {
			return fmt.Errorf("remove sandbox %s: %w", name, err)
		}
	}
	switch tableRow[current.state] {
	case begin:
		return start(ctx)
	case harvest:
		// Before collect, so this line comes before the status change collect logs.
		slog.InfoContext(ctx, "run finished")
		return collect(ctx, current)
	case fail:
		if current.state == dead || current.state == gone {
			slog.ErrorContext(ctx, "the run lost its sandbox", logging.Sandbox, name, logging.Reason, r.why(current.state))
		}
		return r.failed(ctx, ro, n, r.why(current.state))
	case drop:
		return r.drop(ctx, ro, n)
	case restart:
		return r.restart(ctx, ro, n)
	}
	return nil
}

// why says what happened to a run that failed in state s, for the stuck comment.
func (r *runs) why(s state) string {
	switch s {
	case stalled:
		return fmt.Sprintf("claude's output did not change for %d minutes", int(stallAfter.Minutes()))
	case dead:
		return "the sandbox stopped while claude was running"
	case gone:
		return "the sandbox was removed while claude was running"
	}
	return s.String()
}

// remove deletes the sandbox, which stops everything in it, and then its directory.
func (r *runs) remove(ctx context.Context, name string) error {
	if err := r.Sandboxes.Remove(ctx, name); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", name, err)
	}
	if err := os.RemoveAll(r.dir(name)); err != nil {
		return err
	}
	// The ref only matters while a run is harvested, and the next fetch for this name replaces it.
	for _, repo := range r.Repos {
		repo.Git.DropRef(context.WithoutCancel(ctx), name)
	}
	r.mu.Lock()
	delete(r.seen, name)
	r.mu.Unlock()
	return nil
}

// stuck sends the issue to stuck and keeps what the run printed for the person it goes to.
// said is what the AI said about its work, or "".
func (r *runs) stuck(ctx context.Context, ro role, n int, reason, said string) error {
	kept := r.keepOutputs(ctx, ro, n, nil)
	// Stuck goes first. With the run gone and the label as it was, a failed GitHub call would start another run.
	if err := toStuck(ctx, r.GitHub, n, ro.name, ro.from, ro.back, reason, said, kept); err != nil {
		return err
	}
	return r.remove(ctx, r.name(ro, n))
}

func (r *runs) failed(ctx context.Context, ro role, n int, why string) error {
	return r.stuck(ctx, ro, n, fmt.Sprintf("The AI's run failed because %s.", why), "")
}

// restart removes the run without counting it as a failure. builder's PRD goes back to ready.
func (r *runs) restart(ctx context.Context, ro role, n int) error {
	if err := r.remove(ctx, r.name(ro, n)); err != nil {
		return err
	}
	if ro.ready != label.None {
		return r.GitHub.SetStatus(ctx, n, ro.from, ro.ready)
	}
	return nil
}

// invalid sends a PRD whose Repositories section only a person can fix to stuck, after dropping any
// run of ro for it.
func (r *runs) invalid(ctx context.Context, ro role, prd github.Issue, why string) error {
	if err := r.drop(ctx, ro, prd.Number); err != nil {
		return err
	}
	return toStuck(ctx, r.GitHub, prd.Number, ro.name, prd.Status(), ro.back, why, "", "")
}

// drop removes a run its issue no longer needs: one a collect is done with, or one whose issue
// moved. It leaves the labels to whoever set them.
func (r *runs) drop(ctx context.Context, ro role, n int) error {
	return r.remove(ctx, r.name(ro, n))
}
