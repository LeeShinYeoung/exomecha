package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	tests := []struct {
		owner, repo string
		max         int
		want        string
	}{
		{"LeeShinYeoung", "jazzing", 32, "leeshinyeoung-jazzing"},
		{"owner", "my.repo_name", 32, "owner-my-repo-name"},
		{"owner", "a..--b", 32, "owner-a-b"},
		{"-owner-", ".repo.", 32, "owner-repo"},
		{"o", strings.Repeat("r", 61), 63, "o-" + strings.Repeat("r", 61)},
	}
	for _, tt := range tests {
		if got := Slug(tt.owner, tt.repo, tt.max); got != tt.want {
			t.Errorf("Slug(%q, %q, %d) = %q, want %q", tt.owner, tt.repo, tt.max, got, tt.want)
		}
	}
}

func TestSlugTooLong(t *testing.T) {
	owner, repo := "o", strings.Repeat("r", 62)
	sum := sha256.Sum256([]byte("o/" + repo))
	hash := hex.EncodeToString(sum[:])[:8]

	got := Slug(owner, repo, 63)
	if want := "o-" + strings.Repeat("r", 52) + "-" + hash; got != want {
		t.Errorf("Slug of 64 chars = %q, want %q", got, want)
	}
	if len(got) != 63 {
		t.Errorf("len = %d, want 63", len(got))
	}
	if other := Slug(owner, repo+"x", 63); other == got {
		t.Errorf("two repos with the same first 54 chars share the slug %q", got)
	}
	if upper := Slug("O", strings.ToUpper(repo), 63); upper != got {
		t.Errorf("Slug in upper case = %q, want the same as lower case %q", upper, got)
	}
}

func TestSlugCutAtADash(t *testing.T) {
	// The cut lands right after "owner-", which must not leave "--" before the hash.
	got := Slug("owner", strings.Repeat("r", 30), 6+9)
	if strings.Contains(got, "--") || !strings.HasPrefix(got, "owner-") || len(got) != 14 {
		t.Errorf("Slug = %q, want owner-<hash>", got)
	}
}

func TestNames(t *testing.T) {
	e := New(fakeFiles{}, "LeeShinYeoung", "jazzing")
	if got := e.helper(); got != "coworker-build-leeshinyeoung-jazzing" {
		t.Errorf("helper = %q", got)
	}
	ref := e.ref("FROM docker/sandbox-templates:claude-code-docker\n")
	repo, tag, _ := strings.Cut(ref, ":")
	if repo != "docker.io/library/coworker-leeshinyeoung-jazzing" || len(tag) != 12 {
		t.Errorf("ref = %q, want docker.io/library/coworker-<slug>:<12 hex>", ref)
	}
	if other := e.ref("FROM docker/sandbox-templates:claude-code-docker\nRUN true\n"); other == ref {
		t.Errorf("two Dockerfiles share the ref %q", ref)
	}

	long := New(fakeFiles{}, "some-organization", strings.Repeat("long-repository-name-", 3))
	if n := len("coworker-task-" + long.slug + "-9999999-resolver"); n > 63 {
		t.Errorf("task sandbox name is %d chars, sbx allows 63", n)
	}
}

const (
	dockerfile = "FROM docker/sandbox-templates:claude-code-docker\nRUN true\n"
	helper     = "coworker-build-owner-repo"
	repository = "docker.io/library/coworker-owner-repo"
)

func newEnv(f *fakeSbx, files fakeFiles) *Env {
	e := New(files, "owner", "repo")
	e.run = f.run
	return e
}

func withDockerfile() fakeFiles { return fakeFiles{dockerfilePath: dockerfile} }

func TestTemplateWithoutDockerfile(t *testing.T) {
	f := newFake()
	ref, ok, err := newEnv(f, fakeFiles{}).Template(t.Context())
	if ref != "" || ok || err != nil {
		t.Errorf("Template = %q, %v, %v, want no template", ref, ok, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("sbx calls = %q, want none", f.calls)
	}
}

func TestTemplateAlreadyLoaded(t *testing.T) {
	f := newFake()
	e := newEnv(f, withDockerfile())
	want := e.ref(dockerfile)
	_, tag, _ := strings.Cut(want, ":")
	f.images = []image{{ID: "abc", Repository: repository, Tag: tag}}

	ref, ok, err := e.Template(t.Context())
	if ref != want || !ok || err != nil {
		t.Errorf("Template = %q, %v, %v, want %q", ref, ok, err, want)
	}
	if !slices.Equal(f.calls, []string{"template ls --json"}) {
		t.Errorf("sbx calls = %q, want only the template list", f.calls)
	}
}

func TestTemplateBuild(t *testing.T) {
	f := newFake()
	files := withDockerfile()
	files[hostsPath] = "a.example.com\n# needed by lint\nb.example.com\n"
	e := newEnv(f, files)
	ref := e.ref(dockerfile)

	got, ok, err := e.Template(t.Context())
	if got != ref || !ok || err != nil {
		t.Fatalf("Template = %q, %v, %v, want %q", got, ok, err, ref)
	}
	want := []string{
		"template ls --json",
		"ls --json",
		"create --name " + helper + " claude",
		"policy allow network --sandbox " + helper + " a.example.com,b.example.com",
		"exec -i " + helper + " docker build -t " + ref + " -",
		// save and load run together
		"exec " + helper + " docker save " + ref,
		"template load /dev/stdin",
		"template ls --json",
		"ls --json",
		"rm --force " + helper,
		"template ls --json",
	}
	calls := slices.Clone(f.calls)
	if len(calls) == len(want) {
		slices.Sort(calls[5:7])
		slices.Sort(want[5:7])
	}
	if !slices.Equal(calls, want) {
		t.Errorf("sbx calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := f.stdin["exec -i "+helper+" docker build -t "+ref+" -"]; got != dockerfile {
		t.Errorf("docker build stdin = %q, want the Dockerfile", got)
	}
	if got := f.stdin["template load /dev/stdin"]; got != "saved "+ref {
		t.Errorf("template load stdin = %q, want what docker save printed", got)
	}
	if len(f.sandboxes) != 0 {
		t.Errorf("sandboxes left: %v", f.sandboxes)
	}
}

func TestTemplateRemovesLeftoverHelper(t *testing.T) {
	f := newFake()
	f.sandboxes[helper] = true
	if _, _, err := newEnv(f, withDockerfile()).Template(t.Context()); err != nil {
		t.Fatal(err)
	}
	rm, create := f.index("rm --force "+helper), f.index("create --name "+helper+" claude")
	if rm < 0 || rm > create {
		t.Errorf("sbx calls = %q, want the leftover removed before create", f.calls)
	}
}

// startTogether calls Template n times at once while the build waits, and returns the results
// once the build is let go.
func startTogether(t *testing.T, e *Env, f *fakeSbx, n int, build func() (string, error)) []error {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.onBuild = func(ctx context.Context) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return build()
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, _, errs[i] = e.Template(t.Context())
		})
	}
	<-started
	// Lets the other calls reach the wait for the running build.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	return errs
}

func TestTemplateConcurrentCallsBuildOnce(t *testing.T) {
	f := newFake()
	e := newEnv(f, withDockerfile())
	for i, err := range startTogether(t, e, f, 5, func() (string, error) { return "", nil }) {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if n := f.count("create "); n != 1 {
		t.Errorf("%d creates, want 1", n)
	}
}

func TestTemplateBuildFailure(t *testing.T) {
	f := newFake()
	e := newEnv(f, withDockerfile())
	errs := startTogether(t, e, f, 5, func() (string, error) {
		return "#5 [2/2] RUN false\nTOKEN=s3cret\nERROR: failed to build", errExit
	})

	var first *Problem
	for i, err := range errs {
		var p *Problem
		if !errors.As(err, &p) {
			t.Fatalf("call %d: %v, want a Problem", i, err)
		}
		if first == nil {
			first = p
		}
		if p != first {
			t.Errorf("call %d got a different Problem than call 0", i)
		}
	}
	if !strings.Contains(first.Output, "TOKEN=s3cret") {
		t.Errorf("Output = %q, want the build output", first.Output)
	}
	if strings.Contains(first.Error(), "s3cret") || strings.Contains(first.Why(), "s3cret") {
		t.Errorf("Error() = %q, must not hold the build output", first.Error())
	}
	if !strings.Contains(first.Why(), dockerfilePath) {
		t.Errorf("Why() = %q, want it to name the Dockerfile", first.Why())
	}
	if n := f.count("create "); n != 1 {
		t.Errorf("%d creates, want 1 shared by all callers", n)
	}
	if f.count("template load") != 0 || len(f.sandboxes) != 0 {
		t.Errorf("sbx calls = %q, want no load and the helper removed", f.calls)
	}

	f.onBuild = nil
	if _, ok, err := e.Template(t.Context()); !ok || err != nil {
		t.Fatalf("Template after a fix = %v, %v", ok, err)
	}
	if n := f.count("create "); n != 2 {
		t.Errorf("%d creates, want the next call to build again", n)
	}
}

func TestTemplateBuildTimeout(t *testing.T) {
	defer func(d time.Duration) { buildTimeout = d }(buildTimeout)
	buildTimeout = 50 * time.Millisecond
	f := newFake()
	f.onBuild = func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "still downloading", ctx.Err()
	}
	_, _, err := newEnv(f, withDockerfile()).Template(t.Context())
	var p *Problem
	if !errors.As(err, &p) || !strings.Contains(p.Why(), "did not build within") || p.Output != "still downloading" {
		t.Fatalf("Template = %v, want a timeout Problem", err)
	}
	if len(f.sandboxes) != 0 {
		t.Errorf("sandboxes left: %v", f.sandboxes)
	}
}

func TestTemplateBuildEndingAtTheLimitIsNoTimeout(t *testing.T) {
	defer func(d time.Duration) { buildTimeout = d }(buildTimeout)
	buildTimeout = 50 * time.Millisecond
	f := newFake()
	f.onBuild = func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", nil
	}
	if _, ok, err := newEnv(f, withDockerfile()).Template(t.Context()); !ok || err != nil {
		t.Errorf("Template = %v, %v, want the build that exited 0", ok, err)
	}
}

func TestTemplateCancelledStillRemovesHelper(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(t.Context())
	f.onBuild = func(context.Context) (string, error) {
		cancel()
		return "", context.Canceled
	}
	_, _, err := newEnv(f, withDockerfile()).Template(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Template = %v, want the cancellation", err)
	}
	if _, ok := errors.AsType[*Problem](err); ok {
		t.Errorf("Template = %v, a cancellation is not a Problem", err)
	}
	if len(f.sandboxes) != 0 || f.index("rm --force "+helper) < 0 {
		t.Errorf("sbx calls = %q, want the helper removed", f.calls)
	}
}

func TestTemplateWaiterBuildsWhenTheBuilderLeft(t *testing.T) {
	f := newFake()
	e := newEnv(f, withDockerfile())
	started := make(chan struct{})
	builderCtx, cancel := context.WithCancel(t.Context())
	f.onBuild = func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	builder := make(chan error)
	go func() {
		_, _, err := e.Template(builderCtx)
		builder <- err
	}()
	<-started
	waiter := make(chan error)
	go func() {
		_, _, err := e.Template(t.Context())
		waiter <- err
	}()
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	f.onBuild = nil
	f.mu.Unlock()
	cancel()

	if err := <-builder; !errors.Is(err, context.Canceled) {
		t.Errorf("builder = %v, want its cancellation", err)
	}
	if err := <-waiter; err != nil {
		t.Errorf("waiter = %v, want its own build", err)
	}
	if n := f.count("create "); n != 2 {
		t.Errorf("%d creates, want 2", n)
	}
}

func TestTemplateRemovesOnlyOldTags(t *testing.T) {
	f := newFake()
	e := newEnv(f, withDockerfile())
	ref := e.ref(dockerfile)
	f.images = []image{
		{ID: "111111111111", Repository: repository, Tag: "aaaaaaaaaaaa"},
		{ID: fakeID(ref), Repository: repository, Tag: "bbbbbbbbbbbb"},
		{ID: "222222222222", Repository: "docker.io/library/coworker-owner-other", Tag: "aaaaaaaaaaaa"},
		{ID: "549730947ed8", Repository: "docker.io/docker/sandbox-templates", Tag: "claude-code-docker"},
	}
	if _, _, err := e.Template(t.Context()); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "template rm ") {
			removed = append(removed, c)
		}
	}
	if want := []string{"template rm --force " + repository + ":aaaaaaaaaaaa"}; !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q", removed, want)
	}
}

func TestTemplateHostsProblemStopsBeforeCreate(t *testing.T) {
	f := newFake()
	files := withDockerfile()
	files[hostsPath] = "**\n"
	_, _, err := newEnv(f, files).Template(t.Context())
	if _, ok := errors.AsType[*Problem](err); !ok {
		t.Fatalf("Template = %v, want a Problem", err)
	}
	if f.count("create ") != 0 {
		t.Errorf("sbx calls = %q, want no create", f.calls)
	}
}

func TestTemplateHostsRejectedBySbx(t *testing.T) {
	f := newFake()
	f.policyOut = "error: invalid network pattern \"a.example.com:x\": bad port\n  try: sbx policy allow network --help"
	f.policyErr = errExit
	files := withDockerfile()
	files[hostsPath] = "a.example.com:x\n"
	_, _, err := newEnv(f, files).Template(t.Context())
	p, ok := errors.AsType[*Problem](err)
	if !ok || !strings.Contains(p.Why(), `invalid network pattern "a.example.com:x": bad port.`) {
		t.Fatalf("Template = %v, want a Problem quoting sbx", err)
	}
	if f.count("exec ") != 0 || len(f.sandboxes) != 0 {
		t.Errorf("sbx calls = %q, want no build and the helper removed", f.calls)
	}
}

func TestTemplateSbxFailureIsNoProblem(t *testing.T) {
	f := newFake()
	f.createErr = errExit
	_, _, err := newEnv(f, withDockerfile()).Template(t.Context())
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("Template = %v, want the create error", err)
	}
	if _, ok := errors.AsType[*Problem](err); ok {
		t.Errorf("Template = %v, an sbx failure is not a Problem", err)
	}
}
