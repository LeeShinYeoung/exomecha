package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/term"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// initProject asks for the project's settings, with the ones in its file as the defaults, and writes
// the answers when they change the file. Without a terminal it asks nothing and keeps the file. Then
// it signs sbx in to Claude when sbx has no login, and checks this machine.
func initProject(ctx context.Context, home, project string) error {
	path, err := settingsPath(home, project)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var current settings
	if err := yaml.Unmarshal(data, &current); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fd := int(os.Stdin.Fd())
	switch {
	case term.IsTerminal(fd):
		state, err := term.GetState(fd)
		if err != nil {
			return err
		}
		a, err := ask(ctx, bufio.NewReader(os.Stdin), os.Stdout, current, func() (string, error) {
			b, err := term.ReadPassword(fd)
			fmt.Println()
			return string(b), err
		}, func(prd target) error { return checkPRDRepo(home, path, prd) })
		if err != nil {
			// Ctrl-C leaves the token's read behind, with the typing still hidden.
			term.Restore(fd, state)
			fmt.Println()
			return err
		}
		if !a.same(current) {
			next, err := updateSettings(data, a)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := writeSettings(path, next); err != nil {
				return err
			}
			fmt.Printf("Wrote %s\n", path)
		}
	case !exists:
		return errors.New("exomecha init asks for the settings, so run it in a terminal")
	}
	c, err := loadConfig(path)
	if err != nil {
		return err
	}
	c.project = project
	if err := checkPRDRepo(home, path, c.prd); err != nil {
		return err
	}

	if _, err := exec.LookPath("sbx"); err == nil {
		sbx := sandbox.CLI{}
		in, err := sbx.ClaudeSignedIn(ctx)
		if err != nil {
			return fmt.Errorf("read sbx's Claude login: %w", err)
		}
		if !in {
			fmt.Println("sbx has no Claude login. Sign in from the sandbox that starts now.")
			if err := sbx.SignIn(ctx, os.Stdin); err != nil {
				return fmt.Errorf("sign sbx in to Claude: %w", err)
			}
		}
	}

	fmt.Println("Checking this machine, which takes a minute.")
	if err := check(ctx, c); err != nil {
		return fmt.Errorf("fix these, then run exomecha init %s again:\n%w", project, err)
	}
	fmt.Printf("Ready. Start the project with: exomecha run %s\n", project)
	return nil
}

type answers struct {
	repos []target
	prd   string // empty with one repository, which then holds the PRDs
	base  string
	token string
}

func (a answers) same(s settings) bool {
	return slices.Equal(fullNames(a.repos), s.Repos) && a.prd == s.PRDRepo && a.base == s.BaseBranch && a.token == s.GitHubToken
}

func fullNames(ts []target) []string {
	var names []string
	for _, t := range ts {
		names = append(names, t.full())
	}
	return names
}

// ask asks for the settings, with the ones in current as the defaults that an empty answer keeps.
// An answer that does not parse, or a PRD repository checkPRD refuses, asks again.
func ask(ctx context.Context, in *bufio.Reader, out io.Writer, current settings, readToken func() (string, error), checkPRD func(target) error) (answers, error) {
	var a answers
	line := func(question, value string) (string, error) {
		fmt.Fprint(out, prompt(question, value))
		s, err := read(ctx, func() (string, error) { return in.ReadString('\n') })
		if errors.Is(err, io.EOF) {
			return "", errors.New("the answers ended before the settings were complete")
		}
		if err != nil {
			return "", err
		}
		return cmp.Or(strings.TrimSpace(s), value), nil
	}
	for {
		s, err := line("Repositories PRDs may change, as owner/name separated by commas", strings.Join(current.Repos, ", "))
		if err != nil {
			return a, err
		}
		// A file can give one repository a PRD repository of its own, which is asked for below.
		if a.repos, err = parseRepos(strings.Split(s, ",")); err == nil && len(a.repos) == 1 && current.PRDRepo == "" {
			err = checkPRD(a.repos[0])
		}
		if err == nil {
			break
		}
		fmt.Fprintln(out, err)
	}
	for len(a.repos) > 1 || current.PRDRepo != "" {
		s, err := line("PRD repository, whose issues are the PRDs, as owner/name", current.PRDRepo)
		if err != nil {
			return a, err
		}
		t, err := parseTarget("prd_repo", s)
		if err == nil {
			err = checkPRD(t)
		}
		if err != nil {
			fmt.Fprintln(out, err)
			continue
		}
		a.prd = s
		break
	}
	s, err := line("Base branch of every repository", cmp.Or(current.BaseBranch, "main"))
	if err != nil {
		return a, err
	}
	a.base = s
	for a.token == "" {
		fmt.Fprint(out, prompt("GitHub token (not shown as you type)", masked(current.GitHubToken)))
		token, err := read(ctx, readToken)
		if err != nil {
			return a, err
		}
		a.token = cmp.Or(strings.TrimSpace(token), current.GitHubToken)
	}
	return a, nil
}

func prompt(question, value string) string {
	if value == "" {
		return question + ": "
	}
	return question + " [" + value + "]: "
}

// masked shows the token's last 4 characters, enough to tell which one the file holds.
func masked(token string) string {
	if token == "" {
		return ""
	}
	return "..." + token[max(0, len(token)-4):]
}

// read stops waiting for f when ctx ends, since a read from the terminal cannot be cut off.
func read(ctx context.Context, f func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := f()
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		return r.s, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("stopped before writing the settings: %w", context.Cause(ctx))
	}
}

// updateSettings puts the answers in data and leaves the rest as it was, such as the optional
// settings the operator turned on and the comments. With no settings in data, it is a new file.
func updateSettings(data []byte, a answers) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return settingsFile(a), nil
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, errors.New("the file holds no settings")
	}
	at := 0 // where a key the file lacks goes: after the last of these keys it has
	for _, kv := range []struct {
		key   string
		value any
	}{{"repos", fullNames(a.repos)}, {"prd_repo", a.prd}, {"base_branch", a.base}, {"github_token", a.token}} {
		i := -1
		for j := 0; j < len(m.Content); j += 2 {
			if m.Content[j].Value == kv.key {
				i = j
				break
			}
		}
		if i < 0 {
			if kv.value == "" {
				continue
			}
			m.Content = slices.Insert(m.Content, at, &yaml.Node{Kind: yaml.ScalarNode, Value: kv.key}, &yaml.Node{})
			i = at
		}
		var v yaml.Node
		if err := v.Encode(kv.value); err != nil {
			return nil, err
		}
		old := m.Content[i+1]
		v.HeadComment, v.LineComment, v.FootComment = old.HeadComment, old.LineComment, old.FootComment
		*old = v
		at = i + 2
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeSettings replaces the file in one step, so an init cut off while it writes leaves the old file
// whole. The temporary name does not end in .yaml, which checkPRDRepo would read as a project.
func writeSettings(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// The defaults are commented out, so a file left alone follows exomecha's defaults as they change.
const optionalSettings = `
# Optional settings. To change one from the default shown, delete "# " at the start of its lines.

# Values builder's and resolver's sandboxes send to some hosts, such as a read-only token for a
# private package registry. Give each its value, or in place of value a command on this machine
# that prints it, such as command: security find-generic-password -s npm-read-token -w
# sbx puts the value into the requests to the hosts. The sandbox holds only a placeholder, in the
# environment variable named by the key.
# secrets:
#   NPM_TOKEN:
#     hosts: [npm.pkg.github.com]
#     value: npm_...

# five_hour and weekly are percent of the Claude plan's 5-hour and weekly limits. exomecha starts
# no new work at or above them. sandboxes is how many PRD sandboxes builder and resolver may have
# at once, read_sandboxes how many reviewer sandboxes, which only read the code, and reviews how
# many times reviewer sends a PR back to builder before a person decides.
# limits:
#   five_hour: %d
#   weekly: %d
#   sandboxes: %d
#   read_sandboxes: %d
#   reviews: %d

# The claude model and effort of each tier, as model:effort. reviewer runs on heavy and builder on
# its PRD's tier label. resolver runs on its own, whatever the PRD's tier.
# models:
#   light: %s
#   normal: %s
#   heavy: %s
#   resolver: %s
`

func settingsFile(a answers) []byte {
	var b strings.Builder
	b.WriteString("# exomecha's settings for this project, written by exomecha init. See Setup in README.md.\n")
	b.WriteString("# The file holds the GitHub token, so keep it readable by you alone.\n\n")
	b.WriteString("repos:\n")
	for _, t := range a.repos {
		fmt.Fprintf(&b, "  - %s\n", scalar(t.full()))
	}
	if a.prd != "" {
		fmt.Fprintf(&b, "prd_repo: %s\n", scalar(a.prd))
	}
	fmt.Fprintf(&b, "base_branch: %s\n", scalar(a.base))
	fmt.Fprintf(&b, "github_token: %s\n", scalar(a.token))
	d := defaults()
	fmt.Fprintf(&b, optionalSettings, int(math.Round(d.fiveHour*100)), int(math.Round(d.weekly*100)),
		d.projectLimit, d.readLimit, d.reviewLimit, d.light, d.normal, d.heavy, d.resolver)
	return []byte(b.String())
}

// scalar is s as one YAML value, quoted where YAML would read it as something else, such as the
// branch 1.0.
func scalar(s string) string {
	b, _ := yaml.Marshal(s)
	return strings.TrimSuffix(string(b), "\n")
}
