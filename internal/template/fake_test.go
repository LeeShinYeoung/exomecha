package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// fakeSbx keeps the sandboxes and templates sbx would have, and records every call.
type fakeSbx struct {
	mu        sync.Mutex
	calls     []string
	stdin     map[string]string
	images    []image
	sandboxes map[string]bool

	onBuild   func(ctx context.Context) (string, error)
	policyOut string
	policyErr error
	createErr error
}

var errExit = errors.New("exit status 1")

func newFake() *fakeSbx {
	return &fakeSbx{stdin: map[string]string{}, sandboxes: map[string]bool{}}
}

func fakeID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:12]
}

func (f *fakeSbx) run(ctx context.Context, in io.Reader, out io.Writer, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	if in != nil {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", err
		}
		f.mu.Lock()
		f.stdin[cmd] = string(b)
		f.mu.Unlock()
	}

	switch {
	case cmd == "template ls --json":
		f.mu.Lock()
		b, _ := json.Marshal(map[string]any{"images": f.images})
		f.mu.Unlock()
		out.Write(b)
	case cmd == "ls --json":
		var list []map[string]string
		f.mu.Lock()
		for name := range f.sandboxes {
			list = append(list, map[string]string{"name": name})
		}
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"sandboxes": list})
		out.Write(b)
	case args[0] == "create":
		if f.createErr != nil {
			return "error: create failed", f.createErr
		}
		f.mu.Lock()
		f.sandboxes[args[2]] = true
		f.mu.Unlock()
	case args[0] == "rm":
		name := args[len(args)-1]
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.sandboxes[name] {
			return "error: sandbox not found", errExit
		}
		delete(f.sandboxes, name)
	case args[0] == "policy":
		return f.policyOut, f.policyErr
	case strings.Contains(cmd, " docker build "):
		f.mu.Lock()
		onBuild := f.onBuild
		f.mu.Unlock()
		if onBuild != nil {
			return onBuild(ctx)
		}
	case strings.Contains(cmd, " docker save "):
		io.WriteString(out, "saved "+args[len(args)-1])
	case cmd == "template load /dev/stdin":
		f.mu.Lock()
		defer f.mu.Unlock()
		ref := strings.TrimPrefix(f.stdin[cmd], "saved ")
		repo, tag, _ := strings.Cut(ref, ":")
		f.images = append(f.images, image{ID: fakeID(ref), Repository: repo, Tag: tag})
	case args[0] == "template" && args[1] == "rm":
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, img := range f.images {
			if img.Repository+":"+img.Tag == args[len(args)-1] {
				f.images = append(f.images[:i], f.images[i+1:]...)
				return "", nil
			}
		}
		return "error: no such template", errExit
	}
	return "", nil
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
