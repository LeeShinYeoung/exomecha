package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const dockerfilePath = ".coworker/Dockerfile"

var (
	// A Dockerfile can hang, for example on a download that never ends.
	buildTimeout = 30 * time.Minute
	// Removing the helper runs after the caller's context may be gone.
	removeTimeout = time.Minute
)

type Files interface {
	BaseFile(ctx context.Context, path string) (content string, ok bool, err error)
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
	files Files
	slug  string
	run   runner

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

// The same max the task sandbox names use, so every name of one project carries one slug:
// 63 chars minus "coworker-task-" and "-9999999-resolver".
const slugMax = 32

func New(files Files, owner, repo string) *Env {
	return &Env{files: files, slug: Slug(owner, repo, slugMax), run: runSbx}
}

func Slug(owner, repo string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(owner + "-" + repo) {
		switch {
		case 'a' <= r && r <= 'z' || '0' <= r && r <= '9':
			b.WriteRune(r)
		case !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) <= max {
		return slug
	}
	// Lowercase like the slug, since GitHub matches owner and repo names in any case.
	sum := sha256.Sum256([]byte(strings.ToLower(owner + "/" + repo)))
	hash := hex.EncodeToString(sum[:])[:8]
	keep := max - len(hash) - 1
	switch {
	case max <= 0:
		return ""
	case keep <= 0:
		return hash[:min(len(hash), max)]
	}
	return strings.TrimRight(slug[:keep], "-") + "-" + hash
}

func (e *Env) repository() string { return "docker.io/library/coworker-" + e.slug }

func (e *Env) helper() string { return "coworker-build-" + e.slug }

func (e *Env) ref(dockerfile string) string {
	sum := sha256.Sum256([]byte(dockerfile))
	return e.repository() + ":" + hex.EncodeToString(sum[:])[:12]
}

func (e *Env) Template(ctx context.Context) (ref string, ok bool, err error) {
	dockerfile, ok, err := e.files.BaseFile(ctx, dockerfilePath)
	if err != nil || !ok {
		return "", false, err
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
			return "", false, ctx.Err()
		}
		// Waiters share a failed build's result, so Tasks picked together do not each start a
		// build that fails the same way.
		if b.ref == ref && !b.abandoned {
			return result(ref, b.err)
		}
	}
}

func result(ref string, err error) (string, bool, error) {
	if err != nil {
		return "", false, err
	}
	return ref, true, nil
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
	images, err := e.templates(ctx)
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
	if err := e.removeSandbox(ctx, e.helper()); err != nil {
		return fmt.Errorf("remove leftover build sandbox: %w", err)
	}
	err = e.build(ctx, ref, dockerfile, hosts)
	removeCtx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	if err := e.removeSandbox(removeCtx, e.helper()); err != nil {
		slog.Warn("remove build sandbox failed, the next build removes it", "sandbox", e.helper(), "err", err)
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
	helper := e.helper()
	if err := e.sbx(ctx, "create", "--name", helper, "claude"); err != nil {
		return err
	}
	// Downloads in the Dockerfile meet the same network policy as the Task sandboxes.
	if len(hosts) > 0 {
		args := []string{"policy", "allow", "network", "--sandbox", helper, strings.Join(hosts, ",")}
		if output, err := e.run(ctx, nil, nil, args...); err != nil {
			if line, ok := rejectedPattern(output); ok {
				return &Problem{Reason: fmt.Sprintf("sbx rejected `%s`: %s Fix it on the base branch.", hostsPath, line)}
			}
			return sbxError(args, err, output)
		}
	}

	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	// No build context: the Dockerfile arrives on stdin.
	output, err := e.run(buildCtx, strings.NewReader(dockerfile), nil, "exec", "-i", helper, "docker", "build", "-t", ref, "-")
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case buildCtx.Err() != nil:
		return &Problem{
			Reason: fmt.Sprintf("`%s` did not build within %d minutes. The build output is in coworker's log. Fix it on the base branch.", dockerfilePath, int(buildTimeout.Minutes())),
			Output: output,
		}
	case err != nil:
		return &Problem{
			Reason: fmt.Sprintf("`%s` failed to build (%v). The build output is in coworker's log. Fix it on the base branch.", dockerfilePath, err),
			Output: output,
		}
	}

	if err := e.load(ctx, ref); err != nil {
		return err
	}
	images, err := e.templates(ctx)
	if err != nil {
		return err
	}
	if _, ok := find(images, ref); !ok {
		return fmt.Errorf("sbx template ls does not list %s after loading it", ref)
	}
	return nil
}

// The image goes from the helper's Docker straight into sbx, with no tar file on the host.
func (e *Env) load(ctx context.Context, ref string) error {
	r, w := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		args := []string{"exec", e.helper(), "docker", "save", ref}
		output, err := e.run(ctx, nil, w, args...)
		if err != nil {
			err = sbxError(args, err, output)
		}
		w.CloseWithError(err)
		saved <- err
	}()
	args := []string{"template", "load", "/dev/stdin"}
	output, err := e.run(ctx, r, nil, args...)
	// Unblocks the save if load stopped reading early.
	r.CloseWithError(errors.New("template load ended"))
	if saveErr := <-saved; saveErr != nil {
		return saveErr
	}
	if err != nil {
		return sbxError(args, err, output)
	}
	return nil
}

// A sandbox made from a removed template still starts, so Tasks running on an old tag are safe.
func (e *Env) removeOld(ctx context.Context, ref string) {
	images, err := e.templates(ctx)
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
		// Without --force, sbx asks for confirmation and fails when stdin is not a terminal.
		if err := e.sbx(ctx, "template", "rm", "--force", old); err != nil {
			slog.Warn("remove old template failed", "ref", old, "err", err)
			continue
		}
		slog.Info("removed old template", "ref", old)
	}
}

func find(images []image, ref string) (image, bool) {
	for _, img := range images {
		if img.Repository+":"+img.Tag == ref {
			return img, true
		}
	}
	return image{}, false
}

// sbx prints `error: invalid network pattern "x": <why>` for a pattern it does not accept.
func rejectedPattern(output string) (string, bool) {
	for line := range strings.Lines(output) {
		if _, rest, ok := strings.Cut(line, "invalid network pattern"); ok {
			return "invalid network pattern" + strings.TrimRight(rest, "\n") + ".", true
		}
	}
	return "", false
}
