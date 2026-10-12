package main

import (
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

	"charm.land/huh/v2"
	"charm.land/huh/v2/spinner"
	"charm.land/lipgloss/v2"
	"go.yaml.in/yaml/v3"
	"golang.org/x/term"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// initProject asks for the project's settings, with the ones in its file filled in, and writes the
// answers when they change the file. Without a terminal it asks nothing and keeps the file. Then it
// signs sbx in to Claude when sbx has no login, checks this machine item by item, and installs the
// exomecha plugin, which /exomecha:setup needs.
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
	shown := "~" + strings.TrimPrefix(path, home)
	settingsLine := "Kept " + shown
	switch {
	case term.IsTerminal(int(os.Stdin.Fd())):
		q := newQuestions(current, func(prd target) error { return checkPRDRepo(home, path, prd) })
		if err := q.form().RunWithContext(ctx); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return errors.New("stopped before writing the settings")
			}
			return err
		}
		if a := q.answers(); !a.same(current) {
			next, err := updateSettings(data, a)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := writeSettings(path, next); err != nil {
				return err
			}
			settingsLine = "Saved to " + shown
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
	out := os.Stdout
	printHeading(out, "Settings")
	printLine(out, settingsLine, nil)

	if _, err := exec.LookPath("sbx"); err == nil {
		sbx := sandbox.CLI{}
		in, err := sbx.ClaudeSignedIn(ctx)
		if err != nil {
			return fmt.Errorf("read sbx's Claude login: %w", err)
		}
		if !in {
			fmt.Fprintln(out, "\nsbx has no Claude login. Sign in from the sandbox that starts now.")
			if err := sbx.SignIn(ctx, os.Stdin); err != nil {
				return fmt.Errorf("sign sbx in to Claude: %w", err)
			}
		}
	}

	ok, err := report(ctx, out, term.IsTerminal(int(out.Fd())), checks(c))
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	if !ok {
		return fmt.Errorf("fix the problems above, then run exomecha init %s again", project)
	}
	if _, err := os.Stat(filepath.Join(setupDir(projectRoot(home, project)), "setup.sh")); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "Next, run /exomecha:setup %s in Claude Code to write the sandbox's setup files.\n", project)
		fmt.Fprintf(out, "Then start the project with: exomecha run %s\n", project)
		return nil
	}
	fmt.Fprintf(out, "Start the project with: exomecha run %s\n", project)
	return nil
}

var (
	headingStyle = lipgloss.NewStyle().Bold(true)
	passStyle    = lipgloss.NewStyle().Foreground(lipgloss.Green)
	failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Red)
	detailStyle  = lipgloss.NewStyle().Faint(true)
)

func printHeading(out io.Writer, group string) {
	lipgloss.Fprintln(out, "\n"+headingStyle.Render(group))
}

// printLine shows line as passed without problems, and as failed with each problem below it.
func printLine(out io.Writer, line string, problems []error) {
	if len(problems) == 0 {
		lipgloss.Fprintln(out, "  "+passStyle.Render("✓")+" "+line)
		return
	}
	lipgloss.Fprintln(out, "  "+failStyle.Render("✗")+" "+line)
	for _, p := range problems {
		for l := range strings.Lines(p.Error()) {
			lipgloss.Fprintln(out, "    "+detailStyle.Render(strings.TrimSuffix(l, "\n")))
		}
	}
}

// report runs the items in order and shows each one's line under its group, with a spinner while
// it runs when spin is set. ok is false when an item found a problem.
func report(ctx context.Context, out io.Writer, spin bool, items []item) (ok bool, err error) {
	ok = true
	group, skip := "", ""
	for _, it := range items {
		if it.group == skip {
			continue
		}
		if it.group != group {
			group = it.group
			printHeading(out, group)
		}
		var line string
		var problems []error
		if spin {
			err = spinner.New().Title(it.doing).Context(ctx).WithOutput(out).ActionWithErr(func(ctx context.Context) error {
				line, problems = it.run(ctx)
				return nil
			}).Run()
			if err != nil {
				return false, err
			}
			// The spinner leaves its last frame with the cursor at its start: write over it.
			fmt.Fprint(out, "\r\x1b[2K")
		} else {
			line, problems = it.run(ctx)
		}
		printLine(out, line, problems)
		if len(problems) > 0 {
			ok = false
			if it.gate {
				skip = it.group
			}
		}
	}
	return ok, nil
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

// questions are the answers to init's form as the person edits them, starting from the file's.
type questions struct {
	current  settings
	checkPRD func(target) error // refuses a PRD repository another project has

	repos, prd, base, token string
}

func newQuestions(current settings, checkPRD func(target) error) *questions {
	return &questions{
		current:  current,
		checkPRD: checkPRD,
		repos:    strings.Join(current.Repos, ", "),
		prd:      current.PRDRepo,
		base:     cmp.Or(current.BaseBranch, "main"),
		token:    current.GitHubToken,
	}
}

func (q *questions) form() *huh.Form {
	return huh.NewForm(
		huh.NewGroup(huh.NewInput().
			Title("Repositories PRDs may change").
			Description("owner/name or clone URLs, separated by commas").
			Value(&q.repos).Validate(q.checkRepos)),
		huh.NewGroup(huh.NewInput().
			Title("PRD repository, whose issues are the PRDs").
			Description("owner/name or a clone URL").
			Value(&q.prd).Validate(q.checkPRDRepo)).
			WithHideFunc(func() bool { return !q.asksPRD() }),
		huh.NewGroup(
			huh.NewInput().Title("Base branch of every repository").Value(&q.base).Validate(required("the base branch")),
			huh.NewInput().Title("GitHub token").EchoMode(huh.EchoModePassword).Value(&q.token).Validate(required("the GitHub token")),
		),
	)
}

// With one repository, that repository holds the PRDs, unless the file gives it a PRD repository
// of its own.
func (q *questions) asksPRD() bool {
	repos, _ := parseRepos(strings.Split(q.repos, ","))
	return len(repos) > 1 || q.current.PRDRepo != ""
}

func (q *questions) checkRepos(s string) error {
	repos, err := parseRepos(strings.Split(s, ","))
	if err == nil && len(repos) == 1 && q.current.PRDRepo == "" {
		err = q.checkPRD(repos[0])
	}
	return err
}

func (q *questions) checkPRDRepo(s string) error {
	t, err := parseTarget("prd_repo", strings.TrimSpace(s))
	if err != nil {
		return err
	}
	return q.checkPRD(t)
}

func required(what string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s must be set", what)
		}
		return nil
	}
}

// answers reads the form once it has checked every answer.
func (q *questions) answers() answers {
	a := answers{base: strings.TrimSpace(q.base), token: strings.TrimSpace(q.token)}
	a.repos, _ = parseRepos(strings.Split(q.repos, ","))
	if q.asksPRD() {
		prd, _ := parseTarget("prd_repo", strings.TrimSpace(q.prd))
		a.prd = prd.full()
	}
	return a
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
	b.WriteString("# exomecha's settings for this project, written by exomecha init. See README.md.\n")
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
