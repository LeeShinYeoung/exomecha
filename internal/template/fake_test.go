package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// fakeSbx keeps the sandboxes, images and templates sbx would have, and records every call.
type fakeSbx struct {
	mu          sync.Mutex
	calls       []string
	dockerfiles map[string]string // by ref, what each build got
	built       map[string]bool   // images in a helper's Docker, by "<sandbox> <ref>"
	images      []sandbox.Template
	sandboxes   map[string]bool

	onBuild   func(ctx context.Context) (string, error)
	allowErr  error
	createErr error
}

var errExit = errors.New("exit status 1")

func newFake() *fakeSbx {
	return &fakeSbx{dockerfiles: map[string]string{}, built: map[string]bool{}, sandboxes: map[string]bool{}}
}

func fakeID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:12]
}

func (f *fakeSbx) record(ctx context.Context, call ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(call, " "))
	return nil
}

func (f *fakeSbx) Templates(ctx context.Context) ([]sandbox.Template, error) {
	if err := f.record(ctx, "Templates"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandbox.Template(nil), f.images...), nil
}

func (f *fakeSbx) RemoveTemplate(ctx context.Context, ref string) error {
	if err := f.record(ctx, "RemoveTemplate", ref); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, img := range f.images {
		if img.Repository+":"+img.Tag == ref {
			f.images = append(f.images[:i], f.images[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("sbx template: %w: no such template", errExit)
}

func (f *fakeSbx) CreateHelper(ctx context.Context, name string) error {
	if err := f.record(ctx, "CreateHelper", name); err != nil {
		return err
	}
	if f.createErr != nil {
		return f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sandboxes[name] {
		return fmt.Errorf("sbx create: %w: sandbox %s already exists", errExit, name)
	}
	f.sandboxes[name] = true
	return nil
}

func (f *fakeSbx) Allow(ctx context.Context, name string, hosts []string) error {
	if err := f.record(ctx, "Allow", name, strings.Join(hosts, ",")); err != nil {
		return err
	}
	return f.allowErr
}

func (f *fakeSbx) BuildImage(ctx context.Context, name, ref, dockerfile string) (string, error) {
	if err := f.record(ctx, "BuildImage", name, ref); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.dockerfiles[ref] = dockerfile
	onBuild := f.onBuild
	exists := f.sandboxes[name]
	f.mu.Unlock()
	if !exists {
		return "", fmt.Errorf("sbx exec: %w: sandbox %s not found", errExit, name)
	}
	if onBuild != nil {
		if output, err := onBuild(ctx); err != nil {
			return output, err
		}
	}
	f.mu.Lock()
	f.built[name+" "+ref] = true
	f.mu.Unlock()
	return "", nil
}

func (f *fakeSbx) LoadTemplate(ctx context.Context, name, ref string) error {
	if err := f.record(ctx, "LoadTemplate", name, ref); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.built[name+" "+ref] {
		return fmt.Errorf("sbx exec %s docker save %s: %w: no such image", name, ref, errExit)
	}
	repo, tag, _ := strings.Cut(ref, ":")
	f.images = append(f.images, sandbox.Template{ID: fakeID(ref), Repository: repo, Tag: tag})
	return nil
}

func (f *fakeSbx) Remove(ctx context.Context, name string) error {
	if err := f.record(ctx, "Remove", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, name)
	for key := range f.built {
		if strings.HasPrefix(key, name+" ") {
			delete(f.built, key)
		}
	}
	return nil
}

func (f *fakeSbx) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeSbx) index(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c == call {
			return i
		}
	}
	return -1
}
