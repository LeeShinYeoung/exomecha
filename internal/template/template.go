package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

const dockerfilePath = ".exomecha/Dockerfile"

var (
	// A Dockerfile can hang, for example on a download that never ends.
	buildTimeout = 30 * time.Minute
	// Removing the helper runs after the caller's context may be gone.
	removeTimeout = time.Minute
)

type Files interface {
	BaseFile(ctx context.Context, path string) (content string, ok bool, err error)
}

type Sandboxes interface {
	Templates(ctx context.Context) ([]sandbox.Template, error)
	RemoveTemplate(ctx context.Context, ref string) error
	CreateHelper(ctx context.Context, name string) error
	Allow(ctx context.Context, name string, hosts []string) error
	// output is the end of what the build printed, and err never holds it.
	BuildImage(ctx context.Context, name, ref, dockerfile string) (output string, err error)
	LoadTemplate(ctx context.Context, name, ref string) error
	// Remove of a sandbox that does not exist is not an error.
	Remove(ctx context.Context, name string) error
}

// Problem is a failure only a person can fix on the base branch. Its Reason goes into the
// Task's stuck comment.
type Problem struct {
	Reason string
	Output string // Log only: build output can hold secrets.
}

func (p *Problem) Error() string { return p.Reason }

func (p *Problem) Why() string { return p.Reason }

type Env struct {
	files  Files
	sbx    Sandboxes
	slug   string
	helper string // the sandbox a build runs in

	mu       sync.Mutex
	building *build
}

type build struct {
	ref  string
	done chan struct{}
	err  error
	// The caller running the build went away, so its error says nothing about the Dockerfile.
	abandoned bool
}

func New(files Files, sbx Sandboxes, owner, repo string) *Env {
	return &Env{files: files, sbx: sbx, slug: sandbox.Slug(owner, repo), helper: sandbox.BuildName(owner, repo)}
}

func (e *Env) repository() string { return "docker.io/library/exomecha-" + e.slug }

func (e *Env) ref(dockerfile string) string {
	sum := sha256.Sum256([]byte(dockerfile))
	return e.repository() + ":" + hex.EncodeToString(sum[:])[:12]
}

// An empty ref means the base branch has no Dockerfile for exomecha.
func (e *Env) Template(ctx context.Context) (ref string, err error) {
	dockerfile, ok, err := e.files.BaseFile(ctx, dockerfilePath)
	if err != nil || !ok {
		return "", err
	}
	ref = e.ref(dockerfile)
	for {
		e.mu.Lock()
		b := e.building
		if b == nil {
			b = &build{ref: ref, done: make(chan struct{})}
			e.building = b
			e.mu.Unlock()
			e.runBuild(ctx, b, dockerfile)
			return result(ref, b.err)
		}
		e.mu.Unlock()

		select {
		case <-b.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		// Waiters share a failed build's result, so Tasks picked together do not each start a
		// build that fails the same way.
		if b.ref == ref && !b.abandoned {
			return result(ref, b.err)
		}
	}
}

func result(ref string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return ref, nil
}

func (e *Env) runBuild(ctx context.Context, b *build, dockerfile string) {
	defer func() {
		b.abandoned = ctx.Err() != nil
		e.mu.Lock()
		e.building = nil
		e.mu.Unlock()
		close(b.done)
	}()
	b.err = e.ensure(ctx, b.ref, dockerfile)
}

func (e *Env) ensure(ctx context.Context, ref, dockerfile string) error {
	images, err := e.sbx.Templates(ctx)
	if err != nil {
		return err
	}
	if _, ok := find(images, ref); ok {
		return nil
	}
	hosts, err := e.AllowedHosts(ctx)
	if err != nil {
		return err
	}

	slog.Info("building template", "ref", ref)
	start := time.Now()
	if err := e.sbx.Remove(ctx, e.helper); err != nil {
		return fmt.Errorf("remove leftover build sandbox: %w", err)
	}
	err = e.build(ctx, ref, dockerfile, hosts)
	removeCtx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	if err := e.sbx.Remove(removeCtx, e.helper); err != nil {
		slog.Warn("remove build sandbox failed, the next build removes it", "sandbox", e.helper, "err", err)
	}
	if p, ok := errors.AsType[*Problem](err); ok && p.Output != "" {
		slog.Warn("template build failed", "ref", ref, "why", p.Reason, "output", p.Output)
	}
	if err != nil {
		return err
	}

	e.removeOld(ctx, ref)
	slog.Info("built template", "ref", ref, "took", time.Since(start).Round(time.Second))
	return nil
}

func (e *Env) build(ctx context.Context, ref, dockerfile string, hosts []string) error {
	if err := e.sbx.CreateHelper(ctx, e.helper); err != nil {
		return err
	}
	// Downloads in the Dockerfile meet the same network policy as the Task sandboxes.
	if err := e.sbx.Allow(ctx, e.helper, hosts); err != nil {
		if p, ok := errors.AsType[*sandbox.PatternError](err); ok {
			return &Problem{Reason: fmt.Sprintf("sbx rejected `%s`: %s. Fix it on the base branch.", hostsPath, p.Detail)}
		}
		return err
	}

	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	output, err := e.sbx.BuildImage(buildCtx, e.helper, ref, dockerfile)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return ctx.Err()
	case buildCtx.Err() != nil:
		return &Problem{
			Reason: fmt.Sprintf("`%s` did not build within %d minutes. The build output is in exomecha's log. Fix it on the base branch.", dockerfilePath, int(buildTimeout.Minutes())),
			Output: output,
		}
	default:
		return &Problem{
			Reason: fmt.Sprintf("`%s` failed to build (%v). The build output is in exomecha's log. Fix it on the base branch.", dockerfilePath, err),
			Output: output,
		}
	}

	if err := e.sbx.LoadTemplate(ctx, e.helper, ref); err != nil {
		return err
	}
	images, err := e.sbx.Templates(ctx)
	if err != nil {
		return err
	}
	if _, ok := find(images, ref); !ok {
		return fmt.Errorf("sbx template ls does not list %s after loading it", ref)
	}
	return nil
}

// A sandbox made from a removed template still starts, so Tasks running on an old tag are safe.
func (e *Env) removeOld(ctx context.Context, ref string) {
	images, err := e.sbx.Templates(ctx)
	if err != nil {
		slog.Warn("list templates to remove old ones failed", "err", err)
		return
	}
	current, ok := find(images, ref)
	if !ok {
		return
	}
	for _, img := range images {
		// Same ID: two Dockerfiles that built the same image, where removing one tag could take both.
		if img.Repository != e.repository() || img.Tag == current.Tag || img.ID == current.ID {
			continue
		}
		old := img.Repository + ":" + img.Tag
		if err := e.sbx.RemoveTemplate(ctx, old); err != nil {
			slog.Warn("remove old template failed", "ref", old, "err", err)
			continue
		}
		slog.Info("removed old template", "ref", old)
	}
}

func find(images []sandbox.Template, ref string) (sandbox.Template, bool) {
	for _, img := range images {
		if img.Repository+":"+img.Tag == ref {
			return img, true
		}
	}
	return sandbox.Template{}, false
}
