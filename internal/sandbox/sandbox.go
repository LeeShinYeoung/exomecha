// Package sandbox runs Docker Sandboxes through the sbx command.
package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Box struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (b Box) Running() bool { return b.Status == "running" }

type CLI struct{}

func (CLI) List(ctx context.Context) ([]Box, error) {
	out, err := run(ctx, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Sandboxes []Box `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("read sbx ls: %w", err)
	}
	return list.Sandboxes, nil
}

// Create starts a sandbox with `sbx run -d`. A sandbox made by `sbx create` stops 30 seconds after
// its last exec session ends, and only `run -d` makes one that keeps running with none open.
// run -d starts no agent session, so nothing in the sandbox runs claude until exomecha does.
// An empty template is sbx's own claude template. Hosts in deny are blocked from the first moment.
func (CLI) Create(ctx context.Context, name, template, workspace string, deny []string) error {
	args := []string{"run", "-d", "--name", name}
	if template != "" {
		// Without this, sbx would pull a public image that happens to have the same name.
		args = append(args, "--pull", "never", "-t", template)
	}
	for _, host := range deny {
		args = append(args, "--deny-network", host)
	}
	_, err := run(ctx, append(args, "claude", workspace)...)
	return err
}

// CreateHelper makes a sandbox with no workspace for exomecha's own commands. Unlike Create's, it
// stops 30 seconds after its last exec session ends, so one a crash leaves behind stops by itself.
func (CLI) CreateHelper(ctx context.Context, name string) error {
	_, err := run(ctx, "create", "--name", name, "claude")
	return err
}

// PatternError is sbx refusing a host pattern. Detail is sbx's own words about it.
type PatternError struct{ Detail string }

func (e *PatternError) Error() string { return "sbx policy: " + e.Detail }

// Allow lets the sandbox reach hosts, in `sbx policy allow network` syntax. The rule goes away with the sandbox.
func (CLI) Allow(ctx context.Context, name string, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	_, err := run(ctx, "policy", "allow", "network", "--sandbox", name, strings.Join(hosts, ","))
	if err != nil {
		if detail, ok := rejectedPattern(err.Error()); ok {
			return &PatternError{Detail: detail}
		}
	}
	return err
}

// sbx prints `error: invalid network pattern "x": <why>` for a pattern it does not accept.
func rejectedPattern(output string) (string, bool) {
	for line := range strings.Lines(output) {
		if _, rest, ok := strings.Cut(line, "invalid network pattern"); ok {
			return "invalid network pattern" + strings.TrimRight(rest, "\n"), true
		}
	}
	return "", false
}

// Exec runs args in the sandbox with no stdin and returns what it printed on stdout. An exec on a
// stopped sandbox starts it first. dir and env may be empty.
func (CLI) Exec(ctx context.Context, name, dir string, env []string, args ...string) ([]byte, error) {
	sbx := []string{"exec"}
	if dir != "" {
		sbx = append(sbx, "-w", dir)
	}
	for _, e := range env {
		sbx = append(sbx, "-e", e)
	}
	return run(ctx, append(append(sbx, name), args...)...)
}

// Start runs args in the sandbox in a session of their own, and returns without waiting for them.
// They keep running after the exec session ends, until they exit or the sandbox stops. env may be empty.
func (c CLI) Start(ctx context.Context, name, dir string, env []string, args ...string) error {
	_, err := c.Exec(ctx, name, dir, env, append([]string{"sh", "-c", `setsid -f "$@" </dev/null >/dev/null 2>&1`, "sh"}, args...)...)
	return err
}

// Secret is a value the sbx proxy puts into the requests a sandbox sends to Hosts, in place of a
// placeholder. The sandbox never holds the value.
type Secret struct {
	Env     string // the environment variable that holds the placeholder
	Hosts   []string
	Command string // prints the value, on this machine
}

// Placeholder is what the sandbox holds instead of the value. It is no secret.
func (s Secret) Placeholder() string { return "exomecha-secret-" + strings.ToLower(s.Env) }

// SetSecret has sbx keep s for the sandbox only. It goes away with the sandbox. sbx does not set
// the environment variable in a sandbox that already exists, so the caller passes Env itself.
func (CLI) SetSecret(ctx context.Context, name string, s Secret) error {
	args := []string{"secret", "set-custom", "--sandbox", name, "--env", s.Env, "--placeholder", s.Placeholder(), "--command", s.Command}
	for _, h := range s.Hosts {
		args = append(args, "--host", h)
	}
	_, err := run(ctx, args...)
	return err
}

// Remove stops and deletes the sandbox and everything in it, its network rules too. A sandbox
// that is already gone is not an error.
func (CLI) Remove(ctx context.Context, name string) error {
	_, err := run(ctx, "rm", "--force", name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}

type Template struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

func (t Template) Ref() string { return t.Repository + ":" + t.Tag }

func (CLI) Templates(ctx context.Context) ([]Template, error) {
	out, err := run(ctx, "template", "ls", "--json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Images []Template `json:"images"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("read sbx template ls: %w", err)
	}
	return list.Images, nil
}

func (CLI) RemoveTemplate(ctx context.Context, ref string) error {
	// Without --force, sbx asks for confirmation and fails when stdin is not a terminal.
	_, err := run(ctx, "template", "rm", "--force", ref)
	return err
}

// BuildImage builds dockerfile with the Docker in the sandbox and tags the image ref. There is no
// build context: the Dockerfile arrives on stdin. output is the end of what the build printed, and
// err never holds it, since build output can hold secrets.
func (CLI) BuildImage(ctx context.Context, name, ref, dockerfile string) (output string, err error) {
	return stream(ctx, strings.NewReader(dockerfile), nil, "exec", "-i", name, "docker", "build", "-t", ref, "-")
}

// LoadTemplate moves the image ref from the Docker in the sandbox into sbx's templates, with no
// tar file on this machine.
func (CLI) LoadTemplate(ctx context.Context, name, ref string) error {
	r, w := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		args := []string{"exec", name, "docker", "save", ref}
		output, err := stream(ctx, nil, w, args...)
		if err != nil {
			err = fmt.Errorf("sbx %s: %w: %s", strings.Join(args, " "), err, output)
		}
		w.CloseWithError(err)
		saved <- err
	}()
	output, err := stream(ctx, r, nil, "template", "load", "/dev/stdin")
	// Unblocks the save if load stopped reading early.
	r.CloseWithError(errors.New("template load ended"))
	if saveErr := <-saved; saveErr != nil {
		return saveErr
	}
	if err != nil {
		return fmt.Errorf("sbx template load: %w: %s", err, output)
	}
	return nil
}

// GitURL is a git URL for the repository at dir inside the sandbox. git reaches it by running git
// there through sbx exec, so nothing listens on a port and the sandbox needs no credentials.
func (CLI) GitURL(name, dir string) string {
	// In an ext URL a space separates arguments and %s is the git service to run. exomecha's
	// names and paths hold neither.
	return "ext::sbx exec -i " + name + " git %s " + dir
}

// GlobalGitHubSecret reports whether sbx has a GitHub secret for every sandbox. Its proxy would
// hand it to the AI's requests, which could then push past exomecha's checks.
func (CLI) GlobalGitHubSecret(ctx context.Context) (bool, error) {
	out, err := run(ctx, "secret", "ls", "--global", "--service", "github", "--json")
	if err != nil {
		return false, err
	}
	var list struct {
		Secrets []json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return false, fmt.Errorf("read sbx secret ls: %w", err)
	}
	return len(list.Secrets) > 0, nil
}

// Leaves room for the prefix, a 7-digit issue number and the longest role in sbx's 63 characters.
// Templates and the build sandbox carry the same slug, so every name of one project shares it.
const slugMax = 32

func Slug(owner, repo string) string {
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
	if len(slug) <= slugMax {
		return slug
	}
	// Lowercase like the slug, since GitHub matches owner and repo names in any case.
	sum := sha256.Sum256([]byte(strings.ToLower(owner + "/" + repo)))
	hash := hex.EncodeToString(sum[:])[:8]
	return strings.TrimRight(slug[:slugMax-len(hash)-1], "-") + "-" + hash
}

func Prefix(owner, repo string) string {
	return "exomecha-run-" + Slug(owner, repo) + "-"
}

func BuildName(owner, repo string) string {
	return "exomecha-build-" + Slug(owner, repo)
}

func Name(prefix string, issue int, role string) string {
	return prefix + strconv.Itoa(issue) + "-" + role
}

// A leading zero would give a second name for the same issue, which Name never builds back.
var runName = regexp.MustCompile(`^([1-9][0-9]*)-([a-z]+)$`)

func Parse(prefix, name string) (issue int, role string, ok bool) {
	rest, found := strings.CutPrefix(name, prefix)
	if !found || prefix == "" {
		return 0, "", false
	}
	m := runName.FindStringSubmatch(rest)
	if m == nil {
		return 0, "", false
	}
	issue, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", false
	}
	return issue, m[2], true
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sbx", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("sbx %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// stream runs sbx with in as its stdin and out as its stdout. With out nil, stdout joins stderr in
// output, which keeps only the end of what sbx printed.
func stream(ctx context.Context, in io.Reader, out io.Writer, args ...string) (output string, err error) {
	cmd := exec.CommandContext(ctx, "sbx", args...)
	var tail tailBuffer
	cmd.Stdin = in
	cmd.Stdout = &tail
	if out != nil {
		cmd.Stdout = out
	}
	cmd.Stderr = &tail
	// Wait also waits for the copy from in, which can block on a reader that never ends.
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	return tail.String(), err
}

// A build can print without limit.
type tailBuffer struct{ b []byte }

const tailLimit = 8000

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailLimit {
		t.b = append(t.b[:0], t.b[len(t.b)-tailLimit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.b
	if len(b) > tailLimit {
		b = b[len(b)-tailLimit:]
	}
	// The cut can land in the middle of a character.
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}
