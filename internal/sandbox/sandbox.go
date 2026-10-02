// Package sandbox runs Docker Sandboxes through the sbx command.
package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type Box struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (b Box) Running() bool { return b.Status == "running" }

// CLI is the sbx command on this machine.
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

// Allow lets the sandbox reach hosts, in `sbx policy allow network` syntax. The rule goes away with the sandbox.
func (CLI) Allow(ctx context.Context, name string, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	_, err := run(ctx, "policy", "allow", "network", "--sandbox", name, strings.Join(hosts, ","))
	return err
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
// They keep running after the exec session ends, until they exit or the sandbox stops.
func (c CLI) Start(ctx context.Context, name, dir string, args ...string) error {
	_, err := c.Exec(ctx, name, dir, nil, append([]string{"sh", "-c", `setsid -f "$@" </dev/null >/dev/null 2>&1`, "sh"}, args...)...)
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

func Name(prefix string, issue int, role string) string {
	return prefix + strconv.Itoa(issue) + "-" + role
}

// A leading zero would give a second name for the same issue, which Name never builds back.
var runName = regexp.MustCompile(`^([1-9][0-9]*)-([a-z]+)$`)

// Parse reads a name Name built with prefix.
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
