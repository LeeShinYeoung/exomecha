package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSeed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "claude")
	if err := Seed(dir); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"rules/comments.md", "rules/writing.md", "settings.json"} {
		want, err := defaultsFS.ReadFile("defaults/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(got) != string(want) {
			t.Errorf("%s = %q, %v", file, got, err)
		}
	}
	settings, _ := defaultsFS.ReadFile("defaults/settings.json")
	if !json.Valid(settings) {
		t.Errorf("default settings.json is not valid JSON: %s", settings)
	}

	// The directory is the operator's from now on.
	writeFile(t, dir, "settings.json", "{}")
	if err := os.Remove(filepath.Join(dir, "rules", "writing.md")); err != nil {
		t.Fatal(err)
	}
	if err := Seed(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "settings.json")); string(got) != "{}" {
		t.Errorf("Seed rewrote settings.json: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "rules", "writing.md")); !os.IsNotExist(err) {
		t.Errorf("Seed brought back a removed rule: %v", err)
	}
}

// newClaudeDir is an operator's defaults with one rule and settings.
func newClaudeDir(t *testing.T, settings string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "rules"), "style.md", "Write short sentences.\n")
	writeFile(t, dir, "settings.json", settings)
	return dir
}

// pluginCommands are the claude plugin commands run in the sandbox, without the timeout around them.
func pluginCommands(execs [][]string) []string {
	var got []string
	for _, args := range execs {
		if i := slices.Index(args, "claude"); i >= 0 && i+1 < len(args) && args[i+1] == "plugin" {
			got = append(got, strings.Join(args[i+2:], " "))
		}
	}
	return got
}

func TestOpenGivesClaudeTheDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ClaudeDir = newClaudeDir(t, `{"themeId": 2, "enabledPlugins": {"on@market": true, "off@market": false},
		"extraKnownMarketplaces": {"market": {"source": {"source": "github", "repo": "owner/market"}}}}`)
	s, name := e.testSpec()
	home := filepath.Join(e.sbx.disk(name), ".claude")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, home, "settings.json", `{"permissions": {"defaultMode": "bypassPermissions"}, "themeId": 1}`)

	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}

	if got, err := os.ReadFile(filepath.Join(home, "rules", "style.md")); err != nil || string(got) != "Write short sentences.\n" {
		t.Errorf("rule in the sandbox = %q, %v", got, err)
	}
	b, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["permissions"] == nil || settings["enabledPlugins"] == nil || settings["themeId"] != 2.0 {
		t.Errorf("settings in the sandbox = %s, want the template's merged with the defaults, the defaults winning", b)
	}
	box := e.sbx.box(name)
	if got, want := pluginCommands(box.execs), []string{"marketplace add owner/market", "install on@market"}; !slices.Equal(got, want) {
		t.Errorf("plugin commands = %q, want %q", got, want)
	}
	if len(box.starts) != 1 {
		t.Errorf("starts = %q, want one", box.starts)
	}
}

func TestOpenWithoutDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	s, name := e.testSpec()

	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}

	if _, err := os.Stat(filepath.Join(e.sbx.disk(name), ".claude")); !os.IsNotExist(err) {
		t.Errorf("claude settings in the sandbox without ClaudeDir: %v", err)
	}
	if got := pluginCommands(e.sbx.box(name).execs); len(got) != 0 {
		t.Errorf("plugin commands without ClaudeDir: %q", got)
	}
}

func TestOpenGoesOnWhenAPluginFails(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ClaudeDir = newClaudeDir(t, `{"enabledPlugins": {"on@market": true}}`)
	e.sbx.failExec = "plugin install"
	s, name := e.testSpec()

	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}

	if starts := e.sbx.box(name).starts; len(starts) != 1 {
		t.Errorf("starts = %q, want one", starts)
	}
	wantContains(t, "log", log.String(), `level=WARN msg="side task failed" task="claude plugin install on@market"`)
}

func TestOpenBadDefaultSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ClaudeDir = newClaudeDir(t, `{"enabledPlugins": `)
	s, name := e.testSpec()

	if _, err := e.runs.open(t.Context(), builderRole, 1, s); err == nil || !strings.Contains(err.Error(), "merge claude settings") {
		t.Errorf("open err = %v, want one about the settings", err)
	}
	if starts := e.sbx.box(name).starts; len(starts) != 0 {
		t.Errorf("claude started with broken settings: %q", starts)
	}
}
