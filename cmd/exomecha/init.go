package main

import (
	"bufio"
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
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/term"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// initProject writes the project's file from the operator's answers, unless it is there already,
// and signs sbx in to Claude when sbx has no login.
func initProject(ctx context.Context, home, project string) error {
	path, err := settingsPath(home, project)
	if err != nil {
		return err
	}
	switch _, err := os.Stat(path); {
	case errors.Is(err, fs.ErrNotExist):
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			return errors.New("exomecha init asks for the settings, so run it in a terminal")
		}
		a, err := ask(bufio.NewReader(os.Stdin), os.Stdout, func() (string, error) {
			b, err := term.ReadPassword(fd)
			fmt.Println()
			return string(b), err
		})
		if err != nil {
			return err
		}
		if err := writeSettings(path, a); err != nil {
			return err
		}
		fmt.Printf("Wrote %s\n", path)
	case err != nil:
		return err
	default:
		fmt.Printf("%s is there already. To change a setting, edit the file.\n", path)
	}
	c, err := loadConfig(path)
	if err != nil {
		return err
	}
	c.project = project

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

// ask asks for the settings without defaults. An answer that does not parse asks again.
func ask(in *bufio.Reader, out io.Writer, readToken func() (string, error)) (answers, error) {
	var a answers
	line := func(question string) (string, error) {
		fmt.Fprint(out, question)
		s, err := in.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return "", errors.New("the answers ended before the settings were complete")
		}
		return strings.TrimSpace(s), err
	}
	for {
		s, err := line("Repositories PRDs may change, as owner/name separated by commas: ")
		if err != nil {
			return a, err
		}
		if a.repos, err = parseRepos(strings.Split(s, ",")); err == nil {
			break
		}
		fmt.Fprintln(out, err)
	}
	for len(a.repos) > 1 {
		s, err := line("PRD repository, whose issues are the PRDs, as owner/name: ")
		if err != nil {
			return a, err
		}
		if _, err := parseTarget("prd_repo", s); err != nil {
			fmt.Fprintln(out, err)
			continue
		}
		a.prd = s
		break
	}
	s, err := line("Base branch of every repository [main]: ")
	if err != nil {
		return a, err
	}
	a.base = cmp.Or(s, "main")
	for a.token == "" {
		fmt.Fprint(out, "GitHub token (not shown as you type): ")
		token, err := readToken()
		if err != nil {
			return a, err
		}
		a.token = strings.TrimSpace(token)
	}
	return a, nil
}

func writeSettings(path string, a answers) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(settingsFile(a)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// The defaults are commented out, so a file left alone follows exomecha's defaults as they change.
const optionalSettings = `
# Optional settings. To change one from the default shown, delete "# " at the start of its lines.

# Values builder's and resolver's sandboxes send to some hosts, such as a read-only token for a
# private package registry. sbx puts what the command prints on this machine into the requests to
# the hosts. The sandbox holds only a placeholder, in the environment variable named by the key.
# secrets:
#   NPM_TOKEN:
#     hosts: [npm.pkg.github.com]
#     command: /usr/local/bin/npm-read-token

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
