package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// fakeSandboxes keeps each sandbox's disk in a directory of its own. Exec runs the command on
// this machine with the sandbox's repository path swapped for that directory, so git and the
// setup script really run. Start only records what it would start: tests play the AI with play.
type fakeSandboxes struct {
	t    *testing.T
	root string
	path string // PATH for Exec: a timeout that only runs its command, then the real PATH
	// git's global config for Exec. A sandbox has no commit identity, unlike the tests' own config.
	gitConfig string

	mu    sync.Mutex
	boxes map[string]*fakeBox
	fail  map[string]error // by sbx verb: create, allow, exec, start, remove
	// An exec whose command line contains this fails, as sbx failing that one command.
	failExec string
}

type fakeBox struct {
	template  string
	workspace string
	deny      []string
	allowed   []string
	stopped   bool
	starts    [][]string // dir, then args
	execs     [][]string
}

func newFakeSandboxes(t *testing.T) *fakeSandboxes {
	t.Helper()
	bin := t.TempDir()
	// coreutils timeout is in every sandbox, but not on every machine that runs these tests.
	if err := os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(bin, "gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tuseConfigOnly = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &fakeSandboxes{
		t:         t,
		root:      t.TempDir(),
		path:      bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		gitConfig: gitConfig,
		boxes:     map[string]*fakeBox{},
		fail:      map[string]error{},
	}
}

func (f *fakeSandboxes) disk(name string) string { return filepath.Join(f.root, name) }

// repo is the sandbox's repository, where the AI works.
func (f *fakeSandboxes) repo(name string) string { return filepath.Join(f.disk(name), "repo") }

func (f *fakeSandboxes) local(name, s string) string {
	return strings.ReplaceAll(s, repoDir, f.repo(name))
}

func (f *fakeSandboxes) failing(verb string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail[verb]
}

func (f *fakeSandboxes) List(context.Context) ([]sandbox.Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sandbox.Box
	for name, b := range f.boxes {
		status := "running"
		if b.stopped {
			status = "stopped"
		}
		out = append(out, sandbox.Box{Name: name, Status: status})
	}
	slices.SortFunc(out, func(a, b sandbox.Box) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *fakeSandboxes) Create(_ context.Context, name, template, workspace string, deny []string) error {
	if err := f.failing("create"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[name]; ok {
		return fmt.Errorf("sandbox %s already exists", name)
	}
	if err := os.MkdirAll(f.disk(name), 0o755); err != nil {
		return err
	}
	f.boxes[name] = &fakeBox{template: template, workspace: workspace, deny: deny}
	return nil
}

func (f *fakeSandboxes) Allow(_ context.Context, name string, hosts []string) error {
	if err := f.failing("allow"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %s not found", name)
	}
	b.allowed = append(b.allowed, hosts...)
	return nil
}

func (f *fakeSandboxes) Exec(ctx context.Context, name, dir string, env []string, args ...string) ([]byte, error) {
	if err := f.failing("exec"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	b, ok := f.boxes[name]
	if ok {
		b.stopped = false // as sbx exec starts a stopped sandbox
		b.execs = append(b.execs, args)
	}
	failExec := f.failExec
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", name)
	}
	if failExec != "" && strings.Contains(strings.Join(args, " "), failExec) {
		return nil, fmt.Errorf("sbx exec %s: connection lost", name)
	}
	local := make([]string, len(args))
	for i, a := range args {
		local[i] = f.local(name, a)
	}
	cmd := exec.CommandContext(ctx, local[0], local[1:]...)
	cmd.Dir = f.disk(name)
	if dir != "" {
		cmd.Dir = f.local(name, dir)
	}
	// Like sbx exec, which passes the sandbox only its own environment and the -e values.
	cmd.Env = append([]string{"PATH=" + f.path, "GIT_CONFIG_GLOBAL=" + f.gitConfig, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("exec %q: %w: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (f *fakeSandboxes) Start(_ context.Context, name, dir string, args ...string) error {
	if err := f.failing("start"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %s not found", name)
	}
	b.starts = append(b.starts, append([]string{dir}, args...))
	return nil
}

func (f *fakeSandboxes) Remove(_ context.Context, name string) error {
	if err := f.failing("remove"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.boxes, name)
	return os.RemoveAll(f.disk(name))
}

func (f *fakeSandboxes) GitURL(name, dir string) string { return f.local(name, dir) }

// box returns a copy of what the fake knows about a sandbox, failing the test if there is none.
func (f *fakeSandboxes) box(name string) fakeBox {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boxes[name]
	if !ok {
		f.t.Fatalf("no sandbox %s", name)
	}
	return *b
}

func (f *fakeSandboxes) exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.boxes[name]
	return ok
}

// stop stops the sandbox the way sbx stop does, killing what runs in it.
func (f *fakeSandboxes) stop(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxes[name].stopped = true
}

// drop deletes the sandbox behind exomecha's back, the way sbx rm does.
func (f *fakeSandboxes) drop(name string) {
	f.t.Helper()
	if err := f.Remove(context.Background(), name); err != nil {
		f.t.Fatal(err)
	}
}

// add makes a sandbox that exomecha did not create, as a leftover from another run would be.
func (f *fakeSandboxes) add(name string) {
	f.t.Helper()
	if err := f.Create(context.Background(), name, "", "", nil); err != nil {
		f.t.Fatal(err)
	}
}

type fakeEnvironment struct {
	mu    sync.Mutex
	ref   string
	hosts []string
	err   error
	// With wait set, Template is a build: it hands wait a channel when it starts, and returns once
	// that channel is closed.
	wait chan chan struct{}
}

func (f *fakeEnvironment) Template(ctx context.Context) (string, error) {
	f.mu.Lock()
	wait := f.wait
	f.mu.Unlock()
	if wait != nil {
		built := make(chan struct{})
		select {
		case wait <- built:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		select {
		case <-built:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ref, f.err
}

func (f *fakeEnvironment) AllowedHosts(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts, nil
}

// personError is an environment error only a person can fix, as the environment package reports one.
type personError struct{ why, detail string }

func (e personError) Error() string { return e.detail }
func (e personError) Why() string   { return e.why }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// streamOf is what claude writes for a run that ends with structured output.
func streamOf(structured string) string {
	return `{"type":"system","subtype":"init","permissionMode":"auto"}
{"type":"stream_event","event":{"type":"content_block_delta"}}
{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":` + structured + `}
`
}
