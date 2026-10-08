package main

import (
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
)

func TestMain(m *testing.M) {
	if e2eOn() {
		setUpE2E()
	}
	gittest.Main(m)
}

var envNames = []string{
	"EXOMECHA_REPOS", "EXOMECHA_PRD_REPO", "EXOMECHA_BASE_BRANCH", "EXOMECHA_GITHUB_TOKEN",
	"EXOMECHA_FIVE_HOUR_LIMIT", "EXOMECHA_WEEKLY_LIMIT", "EXOMECHA_SANDBOX_LIMIT", "EXOMECHA_READ_SANDBOX_LIMIT", "EXOMECHA_REVIEW_LIMIT",
	"EXOMECHA_TIER_LIGHT", "EXOMECHA_TIER_NORMAL", "EXOMECHA_TIER_HEAVY", "EXOMECHA_RESOLVER_MODEL",
}

func TestConfigFromEnv(t *testing.T) {
	required := map[string]string{
		"EXOMECHA_REPOS": "acme/app", "EXOMECHA_BASE_BRANCH": "main", "EXOMECHA_GITHUB_TOKEN": "secret",
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
		{name: "nothing set", env: map[string]string{}, wantErr: "EXOMECHA_REPOS, EXOMECHA_BASE_BRANCH and EXOMECHA_GITHUB_TOKEN"},
		{name: "repo without owner", env: with(map[string]string{"EXOMECHA_REPOS": "app"}), wantErr: "EXOMECHA_REPOS must hold owner/name"},
		{name: "empty entry", env: with(map[string]string{"EXOMECHA_REPOS": "acme/app,"}), wantErr: "EXOMECHA_REPOS must hold owner/name"},
		{name: "defaults", env: required, check: func(t *testing.T, c config) {
			d := defaults()
			if c.prd.full() != "acme/app" || len(c.repos) != 1 || c.repos[0] != c.prd || c.base != "main" || c.token != "secret" {
				t.Errorf("PRD repository %s, repositories %v on %s, token %q", c.prd.full(), c.repos, c.base, c.token)
			}
			if c.fiveHour != d.fiveHour || c.weekly != d.weekly || c.projectLimit != d.projectLimit || c.readLimit != d.readLimit || c.reviewLimit != d.reviewLimit {
				t.Errorf("limits = %v %v %d %d %d, want the defaults", c.fiveHour, c.weekly, c.projectLimit, c.readLimit, c.reviewLimit)
			}
			if c.light != d.light || c.normal != d.normal || c.heavy != d.heavy || c.resolver != d.resolver {
				t.Errorf("models = %v %v %v %v, want the defaults", c.light, c.normal, c.heavy, c.resolver)
			}
			opus := func(effort string) agent.Model { return agent.Model{Name: "opus", Effort: effort} }
			if d.light != opus("low") || d.normal != opus("high") || d.heavy != opus("xhigh") || d.resolver != opus("medium") || d.projectLimit != 2 || d.reviewLimit != 5 {
				t.Errorf("defaults: models %v %v %v %v, sandbox limit %d, review limit %d", d.light, d.normal, d.heavy, d.resolver, d.projectLimit, d.reviewLimit)
			}
		}},
		{name: "set values", env: with(map[string]string{
			"EXOMECHA_FIVE_HOUR_LIMIT": "50", "EXOMECHA_SANDBOX_LIMIT": "3", "EXOMECHA_TIER_LIGHT": "sonnet:low",
			"EXOMECHA_RESOLVER_MODEL": "sonnet:high", "EXOMECHA_REVIEW_LIMIT": "2",
		}), check: func(t *testing.T, c config) {
			if c.fiveHour != 0.5 || c.projectLimit != 3 || c.light != (agent.Model{Name: "sonnet", Effort: "low"}) {
				t.Errorf("five-hour %v, sandbox limit %d, light %v", c.fiveHour, c.projectLimit, c.light)
			}
			if c.resolver != (agent.Model{Name: "sonnet", Effort: "high"}) || c.reviewLimit != 2 {
				t.Errorf("resolver %v, review limit %d", c.resolver, c.reviewLimit)
			}
		}},
		{name: "several repositories", env: with(map[string]string{"EXOMECHA_REPOS": "acme/api, acme/web", "EXOMECHA_PRD_REPO": "acme/plans"}), check: func(t *testing.T, c config) {
			if c.prd.full() != "acme/plans" || len(c.repos) != 2 || c.repos[0].full() != "acme/api" || c.repos[1].full() != "acme/web" {
				t.Errorf("PRD repository %s, repositories %v", c.prd.full(), c.repos)
			}
		}},
		{name: "PRD repository among the repositories", env: with(map[string]string{"EXOMECHA_REPOS": "acme/API,acme/web", "EXOMECHA_PRD_REPO": "acme/api"}), check: func(t *testing.T, c config) {
			if c.prd != c.repos[0] {
				t.Errorf("PRD repository %v, want the first repository %v", c.prd, c.repos[0])
			}
		}},
		{name: "several repositories without a PRD repository", env: with(map[string]string{"EXOMECHA_REPOS": "acme/api,acme/web"}), wantErr: "EXOMECHA_PRD_REPO must be set"},
		{name: "a repository twice", env: with(map[string]string{"EXOMECHA_REPOS": "acme/api,Acme/Api", "EXOMECHA_PRD_REPO": "acme/plans"}), wantErr: "lists Acme/Api twice"},
		{name: "two repositories of one name", env: with(map[string]string{"EXOMECHA_REPOS": "acme/api,other/api", "EXOMECHA_PRD_REPO": "acme/plans"}), wantErr: "two repositories named api"},
		{name: "PRD repository without owner", env: with(map[string]string{"EXOMECHA_PRD_REPO": "plans"}), wantErr: "EXOMECHA_PRD_REPO must hold owner/name"},
		{name: "usage limit out of range", env: with(map[string]string{"EXOMECHA_WEEKLY_LIMIT": "0"}), wantErr: "EXOMECHA_WEEKLY_LIMIT"},
		{name: "model without effort", env: with(map[string]string{"EXOMECHA_TIER_HEAVY": "opus"}), wantErr: "EXOMECHA_TIER_HEAVY"},
		{name: "resolver model without effort", env: with(map[string]string{"EXOMECHA_RESOLVER_MODEL": "opus"}), wantErr: "EXOMECHA_RESOLVER_MODEL"},
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
