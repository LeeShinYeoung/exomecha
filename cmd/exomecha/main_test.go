package main

import (
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
)

var envNames = []string{
	"EXOMECHA_REPO", "EXOMECHA_BASE_BRANCH", "EXOMECHA_GITHUB_TOKEN",
	"EXOMECHA_FIVE_HOUR_LIMIT", "EXOMECHA_WEEKLY_LIMIT", "EXOMECHA_SANDBOX_LIMIT", "EXOMECHA_READ_SANDBOX_LIMIT",
	"EXOMECHA_TIER_LIGHT", "EXOMECHA_TIER_NORMAL", "EXOMECHA_TIER_HEAVY",
}

func TestConfigFromEnv(t *testing.T) {
	required := map[string]string{
		"EXOMECHA_REPO": "acme/app", "EXOMECHA_BASE_BRANCH": "main", "EXOMECHA_GITHUB_TOKEN": "secret",
	}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range required {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	for _, c := range []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c config)
	}{
		{name: "nothing set", env: map[string]string{}, wantErr: "EXOMECHA_REPO, EXOMECHA_BASE_BRANCH and EXOMECHA_GITHUB_TOKEN"},
		{name: "repo without owner", env: with(map[string]string{"EXOMECHA_REPO": "app"}), wantErr: "EXOMECHA_REPO must be owner/name"},
		{name: "defaults", env: required, check: func(t *testing.T, c config) {
			d := defaults()
			if c.owner != "acme" || c.name != "app" || c.base != "main" || c.token != "secret" {
				t.Errorf("repo = %s/%s on %s, token %q", c.owner, c.name, c.base, c.token)
			}
			if c.fiveHour != d.fiveHour || c.weekly != d.weekly || c.projectLimit != d.projectLimit || c.readLimit != d.readLimit {
				t.Errorf("limits = %v %v %d %d, want the defaults", c.fiveHour, c.weekly, c.projectLimit, c.readLimit)
			}
			if c.light != d.light || c.normal != d.normal || c.heavy != d.heavy {
				t.Errorf("tiers = %v %v %v, want the defaults", c.light, c.normal, c.heavy)
			}
		}},
		{name: "set values", env: with(map[string]string{
			"EXOMECHA_FIVE_HOUR_LIMIT": "50", "EXOMECHA_SANDBOX_LIMIT": "3", "EXOMECHA_TIER_LIGHT": "sonnet:low",
		}), check: func(t *testing.T, c config) {
			if c.fiveHour != 0.5 || c.projectLimit != 3 || c.light != (agent.Model{Name: "sonnet", Effort: "low"}) {
				t.Errorf("five-hour %v, sandbox limit %d, light %v", c.fiveHour, c.projectLimit, c.light)
			}
		}},
		{name: "usage limit out of range", env: with(map[string]string{"EXOMECHA_WEEKLY_LIMIT": "0"}), wantErr: "EXOMECHA_WEEKLY_LIMIT"},
		{name: "model without effort", env: with(map[string]string{"EXOMECHA_TIER_HEAVY": "opus"}), wantErr: "EXOMECHA_TIER_HEAVY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range envNames {
				t.Setenv(k, c.env[k])
			}
			got, err := configFromEnv()
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, got)
		})
	}
}
