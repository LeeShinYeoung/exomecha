package worker

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

//go:embed defaults
var defaultsFS embed.FS

// An install clones the plugin's marketplace, which takes seconds.
var pluginTimeout = 2 * time.Minute

// Seed writes exomecha's default claude settings to dir when it does not exist. Once it exists it
// is the operator's, and exomecha leaves it alone.
func Seed(dir string) error {
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sub, err := fs.Sub(defaultsFS, "defaults")
	if err != nil {
		return err
	}
	return os.CopyFS(dir, sub)
}

// defaults copies ClaudeDir over claude's user settings in the sandbox, where the project's own
// settings take precedence over it. settings.json is merged into the template's, which holds the
// permission mode.
func (r *runs) defaults(ctx context.Context, ro role, n int) error {
	if r.ClaudeDir == "" {
		return nil
	}
	name := r.name(ro, n)
	dir := filepath.Join(r.ctl(name), "claude")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.CopyFS(dir, os.DirFS(r.ClaudeDir)); err != nil {
		return fmt.Errorf("copy %s: %w", r.ClaudeDir, err)
	}
	template, err := r.Sandboxes.Exec(ctx, name, "", nil, "sh", "-c", `[ ! -f "$1" ] || cat "$1"`, "sh", claudeHome+"/settings.json")
	if err != nil {
		return err
	}
	own, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Unmarshal keeps the keys already in the map, so a key in both ends up with the default's value.
	settings := map[string]json.RawMessage{}
	for _, b := range [][]byte{template, own} {
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("merge claude settings: %w", err)
		}
	}
	merged, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), merged, 0o644); err != nil {
		return err
	}
	if _, err := r.Sandboxes.Exec(ctx, name, "", nil, "sh", "-c", `mkdir -p "$2" && cp -R "$1/." "$2/"`, "sh", dir, claudeHome); err != nil {
		return err
	}
	return r.installPlugins(ctx, name, merged)
}

// installPlugins installs the enabled plugins before claude starts: claude -p installs a missing
// one in the background and loads it only from the next session on, and a run has only one. A
// plugin that fails to install is left out of the run.
func (r *runs) installPlugins(ctx context.Context, name string, settings []byte) error {
	var s struct {
		Marketplaces map[string]struct {
			Source struct{ Repo, URL string }
		} `json:"extraKnownMarketplaces"`
		Enabled map[string]bool `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		slog.WarnContext(ctx, "side task failed", logging.Task, "read the default claude plugins", logging.Err, err)
		return nil
	}
	// install finds a plugin only in a marketplace that was added, even one the settings declare.
	var commands [][]string
	for _, m := range slices.Sorted(maps.Keys(s.Marketplaces)) {
		source := s.Marketplaces[m].Source
		commands = append(commands, []string{"marketplace", "add", cmp.Or(source.Repo, source.URL)})
	}
	for _, id := range slices.Sorted(maps.Keys(s.Enabled)) {
		if s.Enabled[id] {
			commands = append(commands, []string{"install", id})
		}
	}
	for _, c := range commands {
		// The timeout inside the sandbox stops claude. This one only covers sbx itself hanging.
		runCtx, cancel := context.WithTimeout(ctx, pluginTimeout+time.Minute)
		args := append([]string{"timeout", strconv.Itoa(int(pluginTimeout.Seconds())), "claude", "plugin"}, c...)
		_, err := r.Sandboxes.Exec(runCtx, name, "", nil, args...)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// The run goes on without the plugin.
			slog.WarnContext(ctx, "side task failed", logging.Task, "claude plugin "+strings.Join(c, " "), logging.Err, err)
		}
	}
	return nil
}
