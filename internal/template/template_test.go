package template

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

func TestNames(t *testing.T) {
	e := New("", nil, "LeeShinYeoung", "jazzing")
	ref := e.ref("FROM docker/sandbox-templates:claude-code-docker\n")
	repo, tag, _ := strings.Cut(ref, ":")
	if repo != "docker.io/library/exomecha-leeshinyeoung-jazzing" || len(tag) != 12 {
		t.Errorf("ref = %q, want docker.io/library/exomecha-<slug>:<12 hex>", ref)
	}
	if other := e.ref("FROM docker/sandbox-templates:claude-code-docker\nRUN true\n"); other == ref {
		t.Errorf("two Dockerfiles share the ref %q", ref)
	}
}

const (
	dockerfile = "FROM docker/sandbox-templates:claude-code-docker\nRUN true\n"
	helper     = "exomecha-build-owner-repo"
	repository = "docker.io/library/exomecha-owner-repo"
)

func newEnv(t *testing.T, f *fakeSbx, files fakeFiles) *Env {
	return New(setupDir(t, files), f, "owner", "repo")
}

func withDockerfile() fakeFiles { return fakeFiles{dockerfileName: dockerfile} }

func TestTemplateWithoutDockerfile(t *testing.T) {
	f := newFake()
	ref, err := newEnv(t, f, fakeFiles{}).Template(t.Context())
	if ref != "" || err != nil {
		t.Errorf("Template = %q, %v, want no template", ref, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("sbx calls = %q, want none", f.calls)
	}
}

func TestTemplateAlreadyLoaded(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, withDockerfile())
	want := e.ref(dockerfile)
	_, tag, _ := strings.Cut(want, ":")
	f.images = []sandbox.Template{{ID: "abc", Repository: repository, Tag: tag}}

	ref, err := e.Template(t.Context())
	if ref != want || err != nil {
		t.Errorf("Template = %q, %v, want %q", ref, err, want)
	}
	if !slices.Equal(f.calls, []string{"Templates"}) {
		t.Errorf("sbx calls = %q, want only the template list", f.calls)
	}
}

func TestTemplateBuild(t *testing.T) {
	f := newFake()
	files := withDockerfile()
	files[hostsName] = "a.example.com\n# needed by lint\nb.example.com\n"
	e := newEnv(t, f, files)
	ref := e.ref(dockerfile)

	got, err := e.Template(t.Context())
	if got != ref || err != nil {
		t.Fatalf("Template = %q, %v, want %q", got, err, ref)
	}
	want := []string{
		"Templates",
		"Remove " + helper,
		"CreateHelper " + helper,
		"Allow " + helper + " a.example.com,b.example.com",
		"BuildImage " + helper + " " + ref,
		"LoadTemplate " + helper + " " + ref,
		"Templates",
		"Remove " + helper,
		"Templates",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("sbx calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := f.dockerfiles[ref]; got != dockerfile {
		t.Errorf("build got %q, want the Dockerfile", got)
	}
	if len(f.sandboxes) != 0 {
		t.Errorf("sandboxes left: %v", f.sandboxes)
	}
}

func TestTemplateRemovesLeftoverHelper(t *testing.T) {
	f := newFake()
	f.sandboxes[helper] = true
	if _, err := newEnv(t, f, withDockerfile()).Template(t.Context()); err != nil {
		t.Fatal(err)
	}
	rm, create := f.index("Remove "+helper), f.index("CreateHelper "+helper)
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
			_, errs[i] = e.Template(t.Context())
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
	e := newEnv(t, f, withDockerfile())
	for i, err := range startTogether(t, e, f, 5, func() (string, error) { return "", nil }) {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if n := f.count("CreateHelper "); n != 1 {
		t.Errorf("%d creates, want 1", n)
	}
}

func TestTemplateBuildFailure(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, withDockerfile())
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
	if !strings.Contains(first.Output(), "TOKEN=s3cret") {
		t.Errorf("Output = %q, want the build output", first.Output())
	}
	if strings.Contains(first.Error(), "s3cret") || strings.Contains(first.Why(), "s3cret") {
		t.Errorf("Error() = %q, must not hold the build output", first.Error())
	}
	if !strings.Contains(first.Why(), dockerfileName) {
		t.Errorf("Why() = %q, want it to name the Dockerfile", first.Why())
	}
	if n := f.count("CreateHelper "); n != 1 {
		t.Errorf("%d creates, want 1 shared by all callers", n)
	}
	if f.count("LoadTemplate") != 0 || len(f.sandboxes) != 0 {
		t.Errorf("sbx calls = %q, want no load and the helper removed", f.calls)
	}

	f.onBuild = nil
	if ref, err := e.Template(t.Context()); ref == "" || err != nil {
		t.Fatalf("Template after a fix = %q, %v", ref, err)
	}
	if n := f.count("CreateHelper "); n != 2 {
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
	_, err := newEnv(t, f, withDockerfile()).Template(t.Context())
	var p *Problem
	if !errors.As(err, &p) || !strings.Contains(p.Why(), "did not build within") || p.Output() != "still downloading" {
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
	if ref, err := newEnv(t, f, withDockerfile()).Template(t.Context()); ref == "" || err != nil {
		t.Errorf("Template = %q, %v, want the build that exited 0", ref, err)
	}
}

func TestTemplateCancelledStillRemovesHelper(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(t.Context())
	f.onBuild = func(context.Context) (string, error) {
		cancel()
		return "", context.Canceled
	}
	_, err := newEnv(t, f, withDockerfile()).Template(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Template = %v, want the cancellation", err)
	}
	if _, ok := errors.AsType[*Problem](err); ok {
		t.Errorf("Template = %v, a cancellation is not a Problem", err)
	}
	if len(f.sandboxes) != 0 || f.index("Remove "+helper) < 0 {
		t.Errorf("sbx calls = %q, want the helper removed", f.calls)
	}
}

func TestTemplateWaiterBuildsWhenTheBuilderLeft(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, withDockerfile())
	started := make(chan struct{})
	builderCtx, cancel := context.WithCancel(t.Context())
	f.onBuild = func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	builder := make(chan error)
	go func() {
		_, err := e.Template(builderCtx)
		builder <- err
	}()
	<-started
	waiter := make(chan error)
	go func() {
		_, err := e.Template(t.Context())
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
	if n := f.count("CreateHelper "); n != 2 {
		t.Errorf("%d creates, want 2", n)
	}
}

func TestTemplateRemovesOnlyOldTags(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, withDockerfile())
	ref := e.ref(dockerfile)
	f.images = []sandbox.Template{
		{ID: "111111111111", Repository: repository, Tag: "aaaaaaaaaaaa"},
		{ID: fakeID(ref), Repository: repository, Tag: "bbbbbbbbbbbb"},
		{ID: "222222222222", Repository: "docker.io/library/exomecha-owner-other", Tag: "aaaaaaaaaaaa"},
		{ID: "549730947ed8", Repository: "docker.io/docker/sandbox-templates", Tag: "claude-code-docker"},
	}
	if _, err := e.Template(t.Context()); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "RemoveTemplate ") {
			removed = append(removed, c)
		}
	}
	if want := []string{"RemoveTemplate " + repository + ":aaaaaaaaaaaa"}; !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q", removed, want)
	}
}

func TestTemplateHostsProblemStopsBeforeCreate(t *testing.T) {
	f := newFake()
	files := withDockerfile()
	files[hostsName] = "**\n"
	_, err := newEnv(t, f, files).Template(t.Context())
	if _, ok := errors.AsType[*Problem](err); !ok {
		t.Fatalf("Template = %v, want a Problem", err)
	}
	if f.count("CreateHelper ") != 0 {
		t.Errorf("sbx calls = %q, want no create", f.calls)
	}
}

func TestTemplateHostsRejectedBySbx(t *testing.T) {
	f := newFake()
	f.allowErr = &sandbox.PatternError{Detail: `invalid network pattern "a.example.com:x": bad port`}
	files := withDockerfile()
	files[hostsName] = "a.example.com:x\n"
	_, err := newEnv(t, f, files).Template(t.Context())
	p, ok := errors.AsType[*Problem](err)
	if !ok || !strings.Contains(p.Why(), `invalid network pattern "a.example.com:x": bad port.`) {
		t.Fatalf("Template = %v, want a Problem quoting sbx", err)
	}
	if f.count("BuildImage ") != 0 || len(f.sandboxes) != 0 {
		t.Errorf("sbx calls = %q, want no build and the helper removed", f.calls)
	}
}

func TestTemplateSbxFailureIsNoProblem(t *testing.T) {
	f := newFake()
	f.createErr = fmt.Errorf("sbx create: %w: create failed", errExit)
	_, err := newEnv(t, f, withDockerfile()).Template(t.Context())
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("Template = %v, want the create error", err)
	}
	if _, ok := errors.AsType[*Problem](err); ok {
		t.Errorf("Template = %v, an sbx failure is not a Problem", err)
	}
}
