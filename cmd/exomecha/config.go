package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// settings is a project's file, ~/.exomecha/<project>.yaml.
type settings struct {
	Repos       []string          `yaml:"repos"`
	PRDRepo     string            `yaml:"prd_repo"`
	BaseBranch  string            `yaml:"base_branch"`
	GitHubToken string            `yaml:"github_token"`
	Secrets     map[string]secret `yaml:"secrets"`
	Limits      struct {
		FiveHour      *int `yaml:"five_hour"`
		Weekly        *int `yaml:"weekly"`
		Sandboxes     *int `yaml:"sandboxes"`
		ReadSandboxes *int `yaml:"read_sandboxes"`
		Reviews       *int `yaml:"reviews"`
	} `yaml:"limits"`
	Models struct {
		Light    string `yaml:"light"`
		Normal   string `yaml:"normal"`
		Heavy    string `yaml:"heavy"`
		Resolver string `yaml:"resolver"`
	} `yaml:"models"`
}

type secret struct {
	Hosts   []string `yaml:"hosts"`
	Value   string   `yaml:"value"`
	Command string   `yaml:"command"`
}

var (
	projectName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	unknownKey  = regexp.MustCompile(`^(line \d+): field (\S+) not found in type .*$`)
)

func settingsPath(home, project string) (string, error) {
	if !projectName.MatchString(project) {
		return "", fmt.Errorf("a project name holds only letters, digits, - and _: %q", project)
	}
	// EqualFold: on macOS Claude names the same folder.
	if strings.EqualFold(project, "claude") {
		return "", errors.New("the default claude settings have the folder claude, so pick another project name")
	}
	return filepath.Join(home, ".exomecha", project+".yaml"), nil
}

// checkPRDRepo refuses prd when a project other than the one in path has it as its PRD repository.
// Each project holds the lock of its own folder only, so two would take the same PRDs and sandboxes.
func checkPRDRepo(home, path string, prd target) error {
	paths, err := filepath.Glob(filepath.Join(home, ".exomecha", "*.yaml"))
	if err != nil {
		return err
	}
	// The error is dropped: during init the file does not exist yet, and then SameFile matches nothing.
	self, _ := os.Stat(path)
	for _, p := range paths {
		// SameFile, not the name: on macOS run Acme reads acme.yaml.
		if info, err := os.Stat(p); err != nil || os.SameFile(info, self) {
			continue
		}
		// A file that does not load cannot run, and this check stops it once it does.
		c, err := loadConfig(p)
		if err == nil && strings.EqualFold(c.prd.full(), prd.full()) {
			return fmt.Errorf("project %s already has %s as its PRD repository, and a PRD repository belongs to one project",
				strings.TrimSuffix(filepath.Base(p), ".yaml"), prd.full())
		}
	}
	return nil
}

func loadConfig(path string) (config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return config{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return config{}, fmt.Errorf("others can read %s, which holds the GitHub token. Run: chmod 600 %s", path, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	c, err := parseConfig(data)
	if err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func parseConfig(data []byte) (config, error) {
	var s settings
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// A misspelled key would otherwise leave its default in place without a word.
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		// yaml names the Go type the key is missing from, which means nothing to the operator.
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			for i, e := range typeErr.Errors {
				typeErr.Errors[i] = unknownKey.ReplaceAllString(e, "$1: exomecha has no setting $2")
			}
		}
		return config{}, err
	}
	c := defaults()
	c.base, c.token = s.BaseBranch, s.GitHubToken
	if len(s.Repos) == 0 || c.base == "" || c.token == "" {
		return config{}, errors.New("repos, base_branch and github_token must all be set")
	}
	var err error
	if c.repos, err = parseRepos(s.Repos); err != nil {
		return config{}, err
	}
	switch {
	case s.PRDRepo != "":
		t, err := parseTarget("prd_repo", s.PRDRepo)
		if err != nil {
			return config{}, err
		}
		c.prd = t
	case len(c.repos) == 1:
		c.prd = c.repos[0]
	default:
		return config{}, errors.New("prd_repo must be set when repos lists several repositories")
	}
	for _, t := range c.repos {
		// The same repository under the case GitHub has it, so the PRD repository's clone is shared.
		if strings.EqualFold(t.full(), c.prd.full()) {
			c.prd = t
		}
	}
	for name, v := range s.Secrets {
		sec, err := parseSecret(name, v)
		if err != nil {
			return config{}, err
		}
		c.secrets = append(c.secrets, sec)
	}
	slices.SortFunc(c.secrets, func(a, b sandbox.Secret) int { return strings.Compare(a.Env, b.Env) })
	if c.fiveHour, err = usageLimit("limits.five_hour", s.Limits.FiveHour, c.fiveHour); err != nil {
		return config{}, err
	}
	if c.weekly, err = usageLimit("limits.weekly", s.Limits.Weekly, c.weekly); err != nil {
		return config{}, err
	}
	if c.projectLimit, err = limit("limits.sandboxes", s.Limits.Sandboxes, c.projectLimit); err != nil {
		return config{}, err
	}
	if c.readLimit, err = limit("limits.read_sandboxes", s.Limits.ReadSandboxes, c.readLimit); err != nil {
		return config{}, err
	}
	if c.reviewLimit, err = limit("limits.reviews", s.Limits.Reviews, c.reviewLimit); err != nil {
		return config{}, err
	}
	if c.light, err = model("models.light", s.Models.Light, c.light); err != nil {
		return config{}, err
	}
	if c.normal, err = model("models.normal", s.Models.Normal, c.normal); err != nil {
		return config{}, err
	}
	if c.heavy, err = model("models.heavy", s.Models.Heavy, c.heavy); err != nil {
		return config{}, err
	}
	if c.resolver, err = model("models.resolver", s.Models.Resolver, c.resolver); err != nil {
		return config{}, err
	}
	return c, nil
}

func parseRepos(entries []string) ([]target, error) {
	var repos []target
	for _, r := range entries {
		t, err := parseTarget("repos", strings.TrimSpace(r))
		if err != nil {
			return nil, err
		}
		for _, o := range repos {
			if strings.EqualFold(o.full(), t.full()) {
				return nil, fmt.Errorf("repos lists %s twice", t.full())
			}
			// A sandbox with several repositories gives each a folder named after it.
			if strings.EqualFold(o.name, t.name) {
				return nil, fmt.Errorf("repos lists two repositories named %s: %s and %s", t.name, o.full(), t.full())
			}
		}
		repos = append(repos, t)
	}
	return repos, nil
}

func parseTarget(key, repo string) (target, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return target{}, fmt.Errorf("%s must hold owner/name: %q", key, repo)
	}
	return target{owner: owner, name: name}, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseSecret reads secrets.<name>: the hosts sbx hands the value to, and the value or a command
// on this machine that prints it. The sandbox holds a placeholder in the environment variable <name>.
func parseSecret(name string, v secret) (sandbox.Secret, error) {
	key := "secrets." + name
	s := sandbox.Secret{Env: name, Value: v.Value, Command: strings.TrimSpace(v.Command)}
	if !envName.MatchString(name) {
		return s, fmt.Errorf("%s: %q is not an environment variable name", key, name)
	}
	switch {
	case s.Value == "" && s.Command == "":
		return s, fmt.Errorf("%s needs value, or command, a command that prints the value", key)
	case s.Value != "" && s.Command != "":
		return s, fmt.Errorf("%s takes value or command, not both", key)
	}
	if len(v.Hosts) == 0 {
		return s, fmt.Errorf("%s needs hosts, the hosts sbx hands the value to", key)
	}
	for _, h := range v.Hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return s, fmt.Errorf("%s lists an empty host", key)
		}
		// The proxy would hand the value to any request the AI makes there, so it could push past exomecha's checks.
		for _, gh := range []string{"github.com", "api.github.com"} {
			if hostMatches(strings.ToLower(h), gh) {
				return s, fmt.Errorf("%s: %s reaches %s, and the AI could use the secret on GitHub", key, h, gh)
			}
		}
		s.Hosts = append(s.Hosts, h)
	}
	return s, nil
}

// hostMatches reports whether an sbx host pattern covers host: "*" stands for one label and "**"
// for any number of them.
func hostMatches(pattern, host string) bool {
	var match func(p, h []string) bool
	match = func(p, h []string) bool {
		switch {
		case len(p) == 0:
			return len(h) == 0
		case p[0] == "**":
			for i := 0; i <= len(h); i++ {
				if match(p[1:], h[i:]) {
					return true
				}
			}
			return false
		case len(h) == 0:
			return false
		case p[0] == "*" || p[0] == h[0]:
			return match(p[1:], h[1:])
		}
		return false
	}
	return match(strings.Split(pattern, "."), strings.Split(host, "."))
}

// usageLimit reads a percentage of a Claude plan limit, at which exomecha stops starting new work.
func usageLimit(key string, v *int, fallback float64) (float64, error) {
	if v == nil {
		return fallback, nil
	}
	if *v < 1 || *v > 100 {
		return 0, fmt.Errorf("%s must be a whole number from 1 to 100: %d", key, *v)
	}
	return float64(*v) / 100, nil
}

func limit(key string, v *int, fallback int) (int, error) {
	if v == nil {
		return fallback, nil
	}
	if *v < 1 {
		return 0, fmt.Errorf("%s must be a whole number of at least 1: %d", key, *v)
	}
	return *v, nil
}

func model(key, v string, fallback agent.Model) (agent.Model, error) {
	if v == "" {
		return fallback, nil
	}
	m, err := agent.ParseModel(v)
	if err != nil {
		return agent.Model{}, fmt.Errorf("%s must be model:effort, such as sonnet:low: %w: %q", key, err, v)
	}
	return m, nil
}
