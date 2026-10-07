package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// The end-to-end suite runs exomecha's own startup and workers against the real sbx and claude.
// GitHub is githubtest acting on a local bare repository, and exomecha checks every few seconds.
const e2eEnv = "EXOMECHA_TEST_E2E"

func e2eOn() bool { return os.Getenv(e2eEnv) == "1" }

// e2e starts a scenario. A core scenario also runs with -short.
func e2e(t *testing.T, core bool) {
	t.Helper()
	if !e2eOn() {
		t.Skipf("set %s=1 to run the end-to-end suite against the real sbx and claude", e2eEnv)
	}
	if testing.Short() && !core {
		t.Skip("not in the -short set")
	}
	t.Parallel()
}

const (
	aRound = 30 * time.Second
	aStart = 2 * time.Minute
	aRun   = 8 * time.Minute
	aBuild = 6 * time.Minute
)

const e2eOwner = "e2e"

// gone as a file's content removes the file in push.
const gone = "\x00gone"

// world is one scenario's own GitHub, origin, exomecha and sandboxes.
type world struct {
	t      *testing.T
	gh     *githubtest.Server
	repo   string
	origin string
	root   string
	cfg    config
	first  int // the first issue number, so log lines can be told apart by number
	sbx    sandbox.CLI

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	exitErr error
	prds    []int
}

var worlds atomic.Int64

// newWorld makes a scenario's own GitHub, origin and exomecha root. short names the scenario in
// sandbox names, so keep it under 20 characters.
func newWorld(t *testing.T, short string, files map[string]string, opts ...func(*config)) *world {
	t.Helper()
	w := &world{t: t, repo: short + "-" + randomHex(3)}
	w.first = int(worlds.Add(1))*100 + 1
	w.origin = filepath.Join(t.TempDir(), "origin.git")
	w.git("", "init", "-q", "--bare", "-b", "main", w.origin)
	w.push("main", "", "Start the project", files)

	w.gh = githubtest.New(t)
	w.gh.Owner, w.gh.Repo, w.gh.UserName = e2eOwner, w.repo, "E2E Person"
	w.gh.Live(w.origin, "main")
	w.gh.SetNextNumber(w.first)

	w.root = t.TempDir()
	w.cfg = defaults()
	w.cfg.owner, w.cfg.name, w.cfg.base, w.cfg.token = e2eOwner, w.repo, "main", "e2e-token-"+randomHex(8)
	w.cfg.apiURL, w.cfg.gitURL, w.cfg.root = w.gh.URL, w.origin, w.root
	w.cfg.interval, w.cfg.ciGrace = 5*time.Second, 15*time.Second
	for _, o := range opts {
		o(&w.cfg)
	}
	// Registered after githubtest.New and the temp dirs, so it runs before they go.
	t.Cleanup(w.cleanup)
	return w
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (w *world) prefix() string { return sandbox.Prefix(e2eOwner, w.repo) }

func (w *world) slug() string { return sandbox.Slug(e2eOwner, w.repo) }

// start runs exomecha until stop, and returns once its workers have started.
func (w *world) start(opts ...func(*config)) {
	w.t.Helper()
	c := w.cfg
	for _, o := range opts {
		o(&c)
	}
	started := make(chan struct{})
	c.started = func() { close(started) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.mu.Lock()
	w.cancel, w.done, w.exitErr = cancel, done, nil
	w.mu.Unlock()
	go func() {
		err := serve(ctx, c)
		w.mu.Lock()
		w.exitErr = err
		w.mu.Unlock()
		close(done)
	}()
	select {
	case <-started:
	case <-done:
		w.t.Fatalf("exomecha did not start: %v", w.exited())
	case <-time.After(3 * time.Minute):
		w.t.Fatal("exomecha did not start within 3 minutes")
	}
}

// stop stops exomecha the way Ctrl-C does and waits until it has stopped.
func (w *world) stop() {
	w.t.Helper()
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel = nil
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
		if err := w.exited(); err != nil {
			w.t.Errorf("exomecha stopped with an error: %v", err)
		}
	case <-time.After(3 * time.Minute):
		w.t.Error("exomecha did not stop within 3 minutes")
	}
}

func (w *world) restart(opts ...func(*config)) {
	w.t.Helper()
	w.stop()
	w.start(opts...)
}

func (w *world) exited() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exitErr
}

// stoppedEarly reports whether exomecha ended while the scenario still wanted it running.
func (w *world) stoppedEarly() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel == nil {
		return false
	}
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *world) cleanup() {
	w.stop()
	ctx := context.Background()
	boxes, err := w.sbx.List(ctx)
	if err != nil {
		w.t.Errorf("list sandboxes: %v", err)
	}
	for _, b := range boxes {
		if strings.HasPrefix(b.Name, w.prefix()) || b.Name == sandbox.BuildName(e2eOwner, w.repo) {
			if err := w.sbx.Remove(ctx, b.Name); err != nil {
				w.t.Errorf("remove %s: %v", b.Name, err)
			}
		}
	}
	removeTemplates(ctx, "docker.io/library/exomecha-"+w.slug())
	if w.t.Failed() {
		w.t.Logf("state at the end:\n%s", w.describe())
	}
}

// removeTemplates removes every sbx template whose repository starts with repo.
func removeTemplates(ctx context.Context, repo string) {
	images, err := sandbox.CLI{}.Templates(ctx)
	if err != nil {
		return
	}
	for _, img := range images {
		if strings.HasPrefix(img.Repository, repo) {
			_ = sandbox.CLI{}.RemoveTemplate(ctx, img.Repository+":"+img.Tag)
		}
	}
}

// waitFor fails the test unless cond holds within the given time.
func (w *world) waitFor(what string, within time.Duration, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if w.stoppedEarly() {
			w.t.Fatalf("exomecha stopped (%v) while waiting for %s.\n%s", w.exited(), what, w.describe())
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("waited %v for %s.\n%s", within, what, w.describe())
		}
		time.Sleep(time.Second)
	}
}

// holds fails the test if cond stops holding at any check within d.
func (w *world) holds(what string, d time.Duration, cond func() bool) {
	w.t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if !cond() {
			w.t.Fatalf("%s stopped holding.\n%s", what, w.describe())
		}
		time.Sleep(time.Second)
	}
}

// git runs git in dir and fails the test on an error.
func (w *world) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// push commits files to branch in origin as a person would, and returns the commit. A branch that
// does not exist yet starts from from. A file starting with "#!" is executable, and gone removes one.
func (w *world) push(branch, from, message string, files map[string]string) string {
	w.t.Helper()
	dir := filepath.Join(w.t.TempDir(), "clone")
	w.git("", "clone", "-q", w.origin, dir)
	switch {
	case w.head(branch) != "":
		w.git(dir, "checkout", "-q", "-B", branch, "origin/"+branch)
	case from != "":
		w.git(dir, "checkout", "-q", "-B", branch, "origin/"+from)
	default:
		w.git(dir, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if content == gone {
			if err := os.Remove(path); err != nil {
				w.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			w.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			w.t.Fatal(err)
		}
	}
	w.git(dir, "add", "-A")
	w.git(dir, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", message)
	w.git(dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	return w.git(dir, "rev-parse", "HEAD")
}

// head returns the commit branch points at in origin, or "" when there is no such branch.
func (w *world) head(branch string) string {
	out, err := exec.Command("git", "-C", w.origin, "rev-parse", "-q", "--verify", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// file returns path at ref in origin, or "" when it is not there.
func (w *world) file(ref, path string) string {
	out, err := exec.Command("git", "-C", w.origin, "show", ref+":"+path).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

type commitInfo struct {
	author, committer, message string
	parents                    []string
}

func (w *world) commit(ref string) commitInfo {
	w.t.Helper()
	out := w.git(w.origin, "log", "-1", "--format=%an <%ae>%x00%cn <%ce>%x00%P%x00%B", ref)
	f := strings.SplitN(out, "\x00", 4)
	return commitInfo{author: f[0], committer: f[1], parents: strings.Fields(f[2]), message: strings.TrimSpace(f[3])}
}

// testsPass runs the project's tests at ref on this machine.
func (w *world) testsPass(ref string) error {
	dir := filepath.Join(w.t.TempDir(), "check")
	w.git("", "clone", "-q", w.origin, dir)
	w.git(dir, "checkout", "-q", ref)
	out, err := exec.Command("sh", "-c", "cd "+dir+" && sh test.sh").CombinedOutput()
	if err != nil {
		return fmt.Errorf("sh test.sh: %v\n%s", err, out)
	}
	return nil
}

// The person's side of GitHub.

func (w *world) filePRD(p prdText, labels ...string) int {
	n := w.gh.AddIssue(p.title, p.body, append([]string{label.PRD}, labels...)...)
	w.track(n)
	return n
}

func (w *world) track(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prds = append(w.prds, n)
}

func (w *world) relabel(n int, from, to label.Status) { w.gh.Relabel(n, string(from), string(to)) }

func (w *world) statuses(n int) []string {
	var out []string
	for _, l := range w.gh.Issue(n).Labels {
		if label.IsStatus(l) {
			out = append(out, l)
		}
	}
	return out
}

func (w *world) status(n int) label.Status {
	s := w.statuses(n)
	if len(s) != 1 {
		return label.Status(strings.Join(s, ","))
	}
	return label.Status(s[0])
}

func (w *world) wantStatus(n int, want label.Status) {
	w.t.Helper()
	if got := w.status(n); got != want {
		w.t.Errorf("PRD #%d status = %q, want %q.\n%s", n, got, want, w.describe())
	}
}

func (w *world) lastComment(n int) string {
	c := w.gh.Issue(n).Comments
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1].Body
}

func branchOf(prd int) string { return "exomecha/prd-" + strconv.Itoa(prd) }

// prsOf returns every PR from the PRD's branch, oldest first.
func (w *world) prsOf(prd int) []githubtest.Pull {
	var out []githubtest.Pull
	for _, p := range w.gh.Pulls() {
		if p.Head == branchOf(prd) {
			out = append(out, p)
		}
	}
	return out
}

func (w *world) openPR(prd int) (githubtest.Pull, bool) {
	for _, p := range w.prsOf(prd) {
		if p.Open {
			return p, true
		}
	}
	return githubtest.Pull{}, false
}

// Sandboxes and runs.

func (w *world) box(prd int, role string) string { return sandbox.Name(w.prefix(), prd, role) }

// listBoxes lists sbx's sandboxes at most once a second for all scenarios. sbx slows down for
// everyone, exomecha included, when many processes ask it at once.
func listBoxes() []sandbox.Box {
	boxCache.mu.Lock()
	defer boxCache.mu.Unlock()
	if time.Since(boxCache.at) >= time.Second {
		if boxes, err := (sandbox.CLI{}).List(context.Background()); err == nil {
			boxCache.boxes, boxCache.at = boxes, time.Now()
		}
	}
	return boxCache.boxes
}

var boxCache struct {
	mu    sync.Mutex
	at    time.Time
	boxes []sandbox.Box
}

func (w *world) boxes() []sandbox.Box {
	var out []sandbox.Box
	for _, b := range listBoxes() {
		if strings.HasPrefix(b.Name, w.prefix()) {
			out = append(out, b)
		}
	}
	return out
}

func (w *world) exists(name string) bool {
	return slices.ContainsFunc(w.boxes(), func(b sandbox.Box) bool { return b.Name == name })
}

func (w *world) runDir(name string) string { return filepath.Join(w.root, "sandboxes", name) }

func (w *world) ctl(name, file string) string { return filepath.Join(w.runDir(name), "ctl", file) }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// started reports whether claude was started in the run, which writes start.json.
func (w *world) started(name string) bool { return exists(filepath.Join(w.runDir(name), "start.json")) }

func (w *world) startHead(name string) string {
	b, err := os.ReadFile(filepath.Join(w.runDir(name), "start.json"))
	if err != nil {
		return ""
	}
	var in struct {
		Head string `json:"head"`
	}
	_ = json.Unmarshal(b, &in)
	return in.Head
}

func (w *world) startedAt(name string) time.Time {
	info, err := os.Stat(filepath.Join(w.runDir(name), "start.json"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func (w *world) ended(name string) bool { return exists(w.ctl(name, "exit")) }

func (w *world) streamSize(name string) int64 {
	info, err := os.Stat(w.ctl(name, "stream.jsonl"))
	if err != nil {
		return 0
	}
	return info.Size()
}

// input returns what claude was given. exomecha removes it once it collects the run.
func (w *world) input(name string) string {
	b, _ := os.ReadFile(w.ctl(name, "input.md"))
	return string(b)
}

// model returns the model claude's init line names.
func (w *world) model(name string) string {
	f, err := os.Open(w.ctl(name, "stream.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()
	s, _ := agent.ReadStream(f)
	return s.Model
}

func (w *world) exec(name string, args ...string) (string, error) {
	out, err := w.sbx.Exec(context.Background(), name, "", nil, args...)
	return string(out), err
}

// claudeArgs returns claude's command line in the sandbox.
func (w *world) claudeArgs(name string) string {
	w.t.Helper()
	out, err := w.exec(name, "ps", "-eo", "args")
	if err != nil {
		w.t.Fatalf("ps in %s: %v\n%s", name, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "claude") && strings.Contains(line, "--model") {
			return line
		}
	}
	w.t.Fatalf("no claude in %s:\n%s", name, out)
	return ""
}

// hold freezes claude in the run's sandbox, so the run neither ends nor spends usage. release
// lets it go on.
func (w *world) hold(name string) { w.signal(name, "-STOP") }

func (w *world) release(name string) { w.signal(name, "-CONT") }

func (w *world) signal(name, sig string) {
	w.t.Helper()
	w.waitFor("claude to take "+sig+" in "+name, aStart, func() bool {
		if w.ended(name) {
			w.t.Fatalf("claude in %s ended before it got %s.\n%s", name, sig, w.describe())
		}
		_, err := w.exec(name, "pkill", sig, "-f", "claude -p")
		return err == nil
	})
}

// holdAsTheyStart holds claude in each of names as soon as it starts, until want of them are held,
// and returns those in the order they started. A light run on the practice project can end within
// 20 seconds, so waiting on one run while another starts would miss it.
func (w *world) holdAsTheyStart(within time.Duration, want int, names ...string) []string {
	w.t.Helper()
	var held []string
	deadline := time.Now().Add(within)
	for len(held) < want {
		for _, n := range names {
			if !slices.Contains(held, n) && w.started(n) {
				w.hold(n)
				held = append(held, n)
			}
		}
		if len(held) < want && time.Now().After(deadline) {
			w.t.Fatalf("waited %v for %d of %v to start, held %v.\n%s", within, want, names, held, w.describe())
		}
		time.Sleep(200 * time.Millisecond)
	}
	return held
}

func (w *world) describe() string {
	var b strings.Builder
	w.mu.Lock()
	prds := slices.Clone(w.prds)
	w.mu.Unlock()
	for _, n := range prds {
		i := w.gh.Issue(n)
		fmt.Fprintf(&b, "PRD #%d open=%v labels=%v comments=%d\n", n, i.Open, i.Labels, len(i.Comments))
		if c := w.lastComment(n); c != "" {
			fmt.Fprintf(&b, "  last comment: %s\n", oneLine(c, 400))
		}
	}
	for _, p := range w.gh.Pulls() {
		fmt.Fprintf(&b, "PR #%d %s -> %s open=%v merged=%v reviews=%d\n", p.Number, p.Head, p.Base, p.Open, p.Merged, len(p.Reviews))
		if n := len(p.Reviews); n > 0 {
			fmt.Fprintf(&b, "  last review: %s\n", oneLine(p.Reviews[n-1].Body, 400))
		}
	}
	for _, x := range w.boxes() {
		fmt.Fprintf(&b, "sandbox %s %s\n", x.Name, x.Status)
	}
	entries, _ := os.ReadDir(filepath.Join(w.root, "sandboxes"))
	for _, e := range entries {
		fmt.Fprintf(&b, "run dir %s\n", e.Name())
	}
	failed, _ := filepath.Glob(filepath.Join(w.root, "sandboxes", "failed", "*", "ctl", "stderr.log"))
	for _, f := range failed {
		out, _ := os.ReadFile(f)
		fmt.Fprintf(&b, "%s:\n%s\n", f, tail(string(out), 1500))
	}
	fmt.Fprintf(&b, "log:\n%s", strings.Join(logs.about(w), ""))
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// e2eLog keeps the log of the whole run. Workers log through the process-wide slog, so scenarios
// running in parallel cannot each capture their own, and a scenario picks its lines out by slug
// and issue numbers.
type e2eLog struct {
	mu    sync.Mutex
	file  *os.File
	lines []string
}

var logs = &e2eLog{}

func (l *e2eLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_, _ = l.file.Write(p)
	}
	l.lines = append(l.lines, string(p))
	return len(p), nil
}

func (l *e2eLog) about(w *world) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, w.slug()) || w.mentions(line) {
			out = append(out, line)
		}
	}
	if len(out) > 60 {
		out = out[len(out)-60:]
	}
	return out
}

// mentions reports whether a log line names one of the world's issues or PRs.
func (w *world) mentions(line string) bool {
	for _, key := range []string{"prd=", "issue=", "pr="} {
		for rest := line; ; {
			_, after, ok := strings.Cut(rest, key)
			if !ok {
				break
			}
			rest = after
			end := strings.IndexFunc(after, func(r rune) bool { return r < '0' || r > '9' })
			if end < 0 {
				end = len(after)
			}
			if n, err := strconv.Atoi(after[:end]); err == nil && n >= w.first && n < w.first+100 {
				return true
			}
		}
	}
	return false
}

// setUpE2E removes what an earlier run that was cut off left behind, and starts the log.
func setUpE2E() {
	ctx := context.Background()
	if boxes, err := (sandbox.CLI{}).List(ctx); err == nil {
		for _, b := range boxes {
			if strings.HasPrefix(b.Name, "exomecha-run-"+e2eOwner+"-") || strings.HasPrefix(b.Name, "exomecha-build-"+e2eOwner+"-") {
				_ = sandbox.CLI{}.Remove(ctx, b.Name)
			}
		}
	}
	removeTemplates(ctx, "docker.io/library/exomecha-"+e2eOwner+"-")
	path := filepath.Join(os.TempDir(), "exomecha-e2e-"+time.Now().Format("20060102-150405")+".log")
	f, err := os.Create(path)
	if err == nil {
		logs.file = f
		fmt.Fprintf(os.Stderr, "e2e log: %s\n", path)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
}

func dirEntries(dir string) []string {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// seedReview files a PRD in review with its PR open, the way builder leaves one. Each map is one
// commit on the PRD's branch.
func (w *world) seedReview(p prdText, commits ...map[string]string) (prd, pr int) {
	w.t.Helper()
	prd = w.filePRD(p, string(label.Review))
	for _, files := range commits {
		w.push(branchOf(prd), "main", fmt.Sprintf("feat: %s\n\nPRD: #%d", strings.ToLower(p.title), prd), files)
	}
	pr = w.gh.OpenPull(branchOf(prd), "main", p.title, fmt.Sprintf("Closes #%d\n\nDoes what the PRD asks.", prd))
	return prd, pr
}

// seedApproved is seedReview after reviewer approved the PR.
func (w *world) seedApproved(p prdText, commits ...map[string]string) (prd, pr int) {
	w.t.Helper()
	prd, pr = w.seedReview(p, commits...)
	w.gh.AddReview(pr, github.Marker+"\n"+approvedLine+" A person reviews and merges it.\n\nThe PR does what the PRD asks.")
	w.relabel(prd, label.Review, label.Approved)
	return prd, pr
}

// run runs script with sh in a checkout of ref on this machine.
func (w *world) run(ref, script string) (string, error) {
	dir := filepath.Join(w.t.TempDir(), "check")
	w.git("", "clone", "-q", w.origin, dir)
	w.git(dir, "checkout", "-q", ref)
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// execIn runs a command in the sandbox's repository.
func (w *world) execIn(name string, args ...string) (string, error) {
	out, err := w.sbx.Exec(context.Background(), name, "/home/agent/repo", nil, args...)
	return strings.TrimSpace(string(out)), err
}

func (w *world) sbxCmd(args ...string) (string, error) {
	out, err := exec.Command("sbx", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (w *world) reviews(pr int) []githubtest.Review { return w.gh.Pull(pr).Reviews }

// peaks follows the world in the background until stop, and keeps the most sandboxes and working
// PRDs it saw at once.
type peaks struct {
	mu                     sync.Mutex
	builders, readers      int
	working                int
	builderAndReaderAtOnce bool
	stop                   func()
}

func (w *world) watchPeaks(prds ...int) *peaks {
	p := &peaks{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.stop = func() { cancel(); <-done }
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			var builders, readers, working int
			for _, b := range w.boxes() {
				switch {
				case strings.HasSuffix(b.Name, "-builder") || strings.HasSuffix(b.Name, "-resolver"):
					builders++
				case strings.HasSuffix(b.Name, "-reviewer"):
					readers++
				}
			}
			for _, n := range prds {
				// A PRD closed while it worked keeps its label.
				if w.gh.Issue(n).Open && w.status(n) == label.Working {
					working++
				}
			}
			p.mu.Lock()
			p.builders, p.readers, p.working = max(p.builders, builders), max(p.readers, readers), max(p.working, working)
			p.builderAndReaderAtOnce = p.builderAndReaderAtOnce || builders > 0 && readers > 0
			p.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	w.t.Cleanup(p.stop)
	return p
}

func (w *world) running(name string) bool {
	return slices.ContainsFunc(w.boxes(), func(b sandbox.Box) bool { return b.Name == name && b.Running() })
}

func (w *world) startBase(name string) string {
	b, err := os.ReadFile(filepath.Join(w.runDir(name), "start.json"))
	if err != nil {
		return ""
	}
	var in struct {
		Base string `json:"base"`
	}
	_ = json.Unmarshal(b, &in)
	return in.Base
}

func (w *world) isAncestor(ancestor, commit string) bool {
	return exec.Command("git", "-C", w.origin, "merge-base", "--is-ancestor", ancestor, commit).Run() == nil
}

// approvals counts exomecha's approvals among a PR's reviews.
func approvals(reviews []githubtest.Review) int {
	n := 0
	for _, r := range reviews {
		if strings.HasPrefix(r.Body, github.Marker+"\n"+approvedLine) {
			n++
		}
	}
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }

// count returns how many of the world's log lines contain s.
func (l *e2eLog) count(w *world, s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, s) && (strings.Contains(line, w.slug()) || w.mentions(line)) {
			n++
		}
	}
	return n
}
