package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

const (
	pluginID    = "exomecha@exomecha"
	marketplace = "exomecha"
)

// PluginVersion is the version of claude's exomecha plugin for the user, or empty when it has
// none. A plugin of one project's scope is missing in every other folder.
func (Claude) PluginVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "claude", "plugin", "list", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("claude plugin list: %w", err)
	}
	return userPluginVersion(out)
}

func userPluginVersion(out []byte) (string, error) {
	var plugins []struct {
		ID, Scope, Version string
		Enabled            bool
	}
	if err := json.Unmarshal(out, &plugins); err != nil {
		return "", fmt.Errorf("claude plugin list printed %q: %w", out, err)
	}
	for _, p := range plugins {
		if p.ID == pluginID && p.Scope == "user" && p.Enabled {
			return p.Version, nil
		}
	}
	return "", nil
}

// InstallPlugin installs the exomecha plugin for the user from the marketplace's newest catalog,
// adding the marketplace from GitHub when claude does not know it yet.
func (Claude) InstallPlugin(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "claude", "plugin", "marketplace", "list", "--json").Output()
	if err != nil {
		return fmt.Errorf("claude plugin marketplace list: %w", err)
	}
	known, err := hasMarketplace(out)
	if err != nil {
		return err
	}
	refresh := []string{"plugin", "marketplace", "add", "LeeShinYeoung/exomecha", "--scope", "user"}
	if known {
		refresh = []string{"plugin", "marketplace", "update", marketplace}
	}
	if err := claudeCLI(ctx, refresh...); err != nil {
		return err
	}
	return claudeCLI(ctx, "plugin", "install", pluginID, "--scope", "user")
}

// UpdatePlugin updates the user's exomecha plugin to the version in the marketplace's newest
// catalog.
func (Claude) UpdatePlugin(ctx context.Context) error {
	if err := claudeCLI(ctx, "plugin", "marketplace", "update", marketplace); err != nil {
		return err
	}
	return claudeCLI(ctx, "plugin", "update", pluginID, "--scope", "user")
}

func hasMarketplace(out []byte) (bool, error) {
	var markets []struct{ Name string }
	if err := json.Unmarshal(out, &markets); err != nil {
		return false, fmt.Errorf("claude plugin marketplace list printed %q: %w", out, err)
	}
	for _, m := range markets {
		if m.Name == marketplace {
			return true, nil
		}
	}
	return false, nil
}

func claudeCLI(ctx context.Context, args ...string) error {
	if out, err := exec.CommandContext(ctx, "claude", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}
