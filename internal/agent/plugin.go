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

// PluginInstalled reports whether claude has the exomecha plugin for the user. A plugin of one
// project's scope is missing in every other folder.
func (Claude) PluginInstalled(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "claude", "plugin", "list", "--json").Output()
	if err != nil {
		return false, fmt.Errorf("claude plugin list: %w", err)
	}
	return hasUserPlugin(out)
}

func hasUserPlugin(out []byte) (bool, error) {
	var plugins []struct {
		ID, Scope string
		Enabled   bool
	}
	if err := json.Unmarshal(out, &plugins); err != nil {
		return false, fmt.Errorf("claude plugin list printed %q: %w", out, err)
	}
	for _, p := range plugins {
		if p.ID == pluginID && p.Scope == "user" && p.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// InstallPlugin installs the exomecha plugin for the user, from GitHub when claude does not know
// the exomecha marketplace yet.
func (Claude) InstallPlugin(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "claude", "plugin", "marketplace", "list", "--json").Output()
	if err != nil {
		return fmt.Errorf("claude plugin marketplace list: %w", err)
	}
	known, err := hasMarketplace(out)
	if err != nil {
		return err
	}
	if !known {
		if err := claudeCLI(ctx, "plugin", "marketplace", "add", "LeeShinYeoung/exomecha", "--scope", "user"); err != nil {
			return err
		}
	}
	return claudeCLI(ctx, "plugin", "install", pluginID, "--scope", "user")
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
