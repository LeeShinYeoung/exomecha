package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
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

type spec struct {
	repos []repoSpec
	input string     // claude's input. open adds what setup and the merge did.
	tier  label.Tier // "" for the role's tier
}

// repoSpec is what one repository of a run gets.
type repoSpec struct {
	repo   *Repo
	branch string // checked out in the repository, and set to head. "" leaves base checked out, detached
	base   string // the base branch commit
	head   string // the commit claude starts from
	remote string // the PRD branch on GitHub, "" when there is none
	// merge merges the base branch after setup and lists the conflicts in the input.
	merge bool
}

// problem stops a start, and only a person can fix it. why goes into the stuck comment, and
// outputs, file names to what was printed, only into the kept outputs, since they can hold secrets.
type problem struct {
	why     string
	outputs map[string]string
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
	var p *problem
	if err == nil {
		p, err = r.open(ctx, ro, n, s)
	}
	if p == nil && err == nil {
		return nil
	}
	// Undo the start even when shutdown cancelled ctx.
	cleanup := context.WithoutCancel(ctx)
	if p != nil {
		kept := r.keep(cleanup, ro, n, p.outputs)
		if err := r.remove(cleanup, name); err != nil {
			return err
		}
		return toStuck(cleanup, r.GitHub, n, ro.name, ro.from, ro.back, p.why, kept)
	}
	if rerr := r.restart(cleanup, ro, n); rerr != nil {
		return errors.Join(err, rerr)
	}
	if errors.Is(err, errNoSlot) {
		return nil
	}
	return err
}

// open creates the sandbox, sends the code in, runs setup, and starts claude. The problem is set
// when only a person can fix what stopped it. On a problem or error the caller removes the sandbox.
func (r *runs) open(ctx context.Context, ro role, n int, s spec) (*problem, error) {
	tier := cmp.Or(s.tier, ro.tier)
	model := r.Models[tier]
	if ro.model != nil {
		model = ro.model(r.Deps)
	}
	if model == (agent.Model{}) {
		return nil, fmt.Errorf("no model for %s", cmp.Or(string(tier), ro.name))
	}
	// The sandbox files are read at one commit of the PRD repository for the whole start.
	var prep string
	if ro.project {
		var err error
		if prep, err = r.prepBase(ctx, s); err != nil {
			return nil, err
		}
	}
	if p, err := r.create(ctx, ro, n); p != nil || err != nil {
		return p, err
	}
	in := inputs{Repos: make([]repoRun, len(s.repos))}
	for i, rs := range s.repos {
		in.Repos[i] = repoRun{Repo: rs.repo.Name, Dir: dirOf(len(s.repos), rs.repo), Base: rs.base, Head: rs.head, Remote: rs.remote, Merge: rs.merge}
		if err := r.send(ctx, ro, n, rs, in.Repos[i].Dir); err != nil {
			return nil, err
		}
	}
	// Before setup, so the project's script can change what the defaults set.
	if err := r.defaults(ctx, ro, n); err != nil {
		return nil, err
	}
	input := s.input
	if ro.project {
		rules, err := r.rules(ctx, prep)
		if err != nil {
			return nil, err
		}
		note, p, err := r.setup(ctx, ro, n, in.Repos, prep)
		if p != nil || err != nil {
			return p, err
		}
		input += rules + note
	}
	// After setup, not before: conflict markers in a manifest or lockfile would break an install.
	for i := range in.Repos {
		if !in.Repos[i].Merge {
			continue
		}
		note, p, err := r.merge(ctx, ro, n, &in.Repos[i])
		if p != nil || err != nil {
			return p, err
		}
		input += note
	}
	return nil, r.launch(ctx, ro, n, model, input, in)
}

// prepBase is the PRD repository's base branch commit. A repository of the run already synced it.
func (r *runs) prepBase(ctx context.Context, s spec) (string, error) {
	for _, rs := range s.repos {
		if rs.repo.Git == r.Prep {
			return rs.base, nil
		}
	}
	return baseCommit(ctx, r.Prep)
}

// create makes the run's sandbox, from the project's template and with its allowed hosts for a
// project run, and from sbx's own template ("" ref) for a reader's.
func (r *runs) create(ctx context.Context, ro role, n int) (*problem, error) {
	var ref string
	var hosts []string
	if ro.project {
		var err error
		ref, err = r.Environment.Template(ctx)
		if p, err := needsPerson(err); p != nil || err != nil {
			return p, err
		}
		hosts, err = r.Environment.AllowedHosts(ctx)
		if p, err := needsPerson(err); p != nil || err != nil {
			return p, err
		}
	}
	name := r.name(ro, n)
	if err := os.MkdirAll(r.ctl(name), 0o755); err != nil {
		return nil, err
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
	for _, secret := range r.Secrets {
		if err == nil && ro.project {
			err = r.Sandboxes.SetSecret(ctx, name, secret)
		}
	}
	return nil, err
}

// secretEnv sets each secret's environment variable to its placeholder in a project run.
func (r *runs) secretEnv(ro role) []string {
	var env []string
	if ro.project {
		for _, s := range r.Secrets {
			env = append(env, s.Env+"="+s.Placeholder())
		}
	}
	return env
}

// send puts one repository's code at dir in the run's sandbox: the base branch's commit as
// origin/<base>, and rs.head checked out as rs.branch.
func (r *runs) send(ctx context.Context, ro role, n int, rs repoSpec, dir string) error {
	name := r.name(ro, n)
	if _, err := r.git(ctx, name, "", "init", "-q", dir); err != nil {
		return err
	}
	if ro.project {
		// Before setup, so the project's script can set another.
		for _, kv := range [][2]string{{"user.name", r.GitName}, {"user.email", r.GitEmail}} {
			if _, err := r.git(ctx, name, dir, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	if err := rs.repo.Git.SendTo(ctx, r.Sandboxes.GitURL(name, dir), rs.base, rs.branch, rs.head); err != nil {
		return err
	}
	checkout := []string{"checkout", "-q", rs.branch}
	if rs.branch == "" {
		// Detached, since SendTo sent no branch: git refuses a push to the branch checked out, and
		// a new repository's unborn branch has whatever name the image's git config gives it.
		checkout = []string{"checkout", "-q", "--detach", rs.repo.Git.BaseRef()}
	}
	_, err := r.git(ctx, name, dir, checkout...)
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
	if err := r.Sandboxes.Start(ctx, name, repoDir, r.secretEnv(ro), args...); err != nil {
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

// needsPerson turns an environment error that only a person can fix into the problem to give them.
func needsPerson(err error) (*problem, error) {
	var person interface{ Why() string }
	if !errors.As(err, &person) {
		return nil, err
	}
	p := &problem{why: person.Why()}
	var printed interface{ Output() string }
	if errors.As(err, &printed) && printed.Output() != "" {
		p.outputs = map[string]string{"build.log": printed.Output()}
	}
	return p, nil
}

func (r *runs) git(ctx context.Context, name, dir string, args ...string) (string, error) {
	out, err := r.Sandboxes.Exec(ctx, name, dir, nil, append([]string{"git"}, args...)...)
	return string(out), err
}

func (r *runs) sh(ctx context.Context, ro role, n int, dir, script string, args ...string) (string, error) {
	out, err := r.Sandboxes.Exec(ctx, r.name(ro, n), dir, nil, append([]string{"sh", "-c", script, "sh"}, args...)...)
	return string(out), err
}

func (r *runs) changes(ctx context.Context, name, dir string) (string, error) {
	out, err := r.git(ctx, name, dir, "status", "--porcelain")
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

// where names the repository in a run of several, for a comment or the input, and is "" in a run
// of one.
func (rr repoRun) where() string {
	if rr.Dir == repoDir {
		return ""
	}
	return " in " + rr.Repo
}

// folder is the repository's folder in the run's sandbox, relative to where claude starts.
func (rr repoRun) folder() string {
	return strings.TrimPrefix(strings.TrimPrefix(rr.Dir, repoDir), "/")
}

// merge merges the base branch in one repository of the run but stops before the merge commit, so
// the AI makes that commit itself and sees whatever the project's commit checks report. It lists
// the conflicted files in rr.Conflicts and in note, for claude's input. The problem is set when git
// refused the merge without a conflict.
func (r *runs) merge(ctx context.Context, ro role, n int, rr *repoRun) (note string, p *problem, err error) {
	repo, ok := r.repo(rr.Repo)
	if !ok {
		return "", nil, fmt.Errorf("no repository %s", rr.Repo)
	}
	// The exit status comes back on stdout, apart from sbx's own failures. diff3 puts the original
	// lines between the two sides of a conflict, so the AI sees what each side changed.
	out, err := r.sh(ctx, ro, n, rr.Dir, `git -c merge.conflictStyle=diff3 merge --no-commit --no-ff "$1" 2>&1; echo $?`, repo.Git.BaseRef())
	if err != nil {
		return "", nil, err
	}
	output, code, err := splitStatus(out)
	if err != nil {
		return "", nil, fmt.Errorf("merge: %w", err)
	}
	var conflicts []string
	if code != 0 {
		// -z keeps non-ASCII file names as they are instead of quoting them.
		list, err := r.sh(ctx, ro, n, rr.Dir, "git diff --name-only -z --diff-filter=U")
		if err != nil {
			return "", nil, err
		}
		for f := range strings.SplitSeq(list, "\x00") {
			if f != "" {
				conflicts = append(conflicts, f)
			}
		}
	}
	if code != 0 && len(conflicts) == 0 {
		// The same merge fails the same way on every try, so only a person can get past it.
		return "", &problem{
			why: fmt.Sprintf("Merging `%s` into this PRD's branch%s in the sandbox failed without a conflict, so the AI did not start. "+
				"Fix what git reports, then relabel this issue as below. You don't need to answer.", repo.Git.Base(), rr.where()),
			outputs: map[string]string{"merge.log": output},
		}, nil
	}
	rr.Conflicts = conflicts
	in := ""
	if f := rr.folder(); f != "" {
		in = " in `" + f + "/`"
	}
	note = fmt.Sprintf("## Conflicted files%s (merging `%s`)\n\n", in, repo.Git.BaseRef())
	if len(conflicts) == 0 {
		note += "None. The merge only needs to be committed.\n"
	}
	for _, f := range conflicts {
		note += "- " + f + "\n"
	}
	return note, nil, nil
}
