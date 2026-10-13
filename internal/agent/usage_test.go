package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines from a real `claude -p --output-format stream-json --verbose` run, trimmed.
const streamWithUsage = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1790763000,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.17,"resetsAt":1790763000},"seven_day":{"utilization":0.31,"resetsAt":1790917200}}},"uuid":"u","session_id":"s"}
{"type":"result","subtype":"success","is_error":false,"result":"ok"}
`

func TestUsage(t *testing.T) {
	fakeClaude(t, streamWithUsage, "", 0)
	u, err := (Claude{}).Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{
		Known:    true,
		FiveHour: Window{Used: 0.17, ResetsAt: time.Unix(1790763000, 0)},
		Weekly:   Window{Used: 0.31, ResetsAt: time.Unix(1790917200, 0)},
	}
	if u != want {
		t.Errorf("Usage = %+v, want %+v", u, want)
	}
}

func TestUsageRejected(t *testing.T) {
	// claude refuses the call and exits with an error, but still reports the limits.
	fakeClaude(t, strings.Replace(streamWithUsage, `"status":"allowed"`, `"status":"rejected"`, 1), "", 1)
	u, err := (Claude{}).Usage(t.Context())
	if err != nil || !u.Known || !u.Rejected {
		t.Errorf("Usage = %+v, %v, want a known, rejected usage", u, err)
	}
}

func TestUsageWithoutEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"result","subtype":"success","is_error":false,"result":"ok"}
`
	fakeClaude(t, stream, "", 0)
	u, err := (Claude{}).Usage(t.Context())
	if err != nil || u.Known {
		t.Errorf("Usage = %+v, %v, want an unknown usage", u, err)
	}
	fakeClaude(t, "", "Not logged in", 1)
	if _, err := (Claude{}).Usage(t.Context()); err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("Usage of a failed run = %v, want the run's stderr", err)
	}
}

func TestCheckStream(t *testing.T) {
	const ok = `{"type":"result","subtype":"success","is_error":false,"result":"ok"}`
	for name, c := range map[string]struct {
		stream string
		want   string // in the error, "" for none
	}{
		"auto": {`{"type":"system","subtype":"init","model":"claude-opus-5-5","permissionMode":"auto"}` + "\n" + ok, ""},
		"not auto": {`{"type":"system","subtype":"init","model":"claude-haiku-4-5","permissionMode":"default"}` + "\n" + ok,
			"claude does not run m in auto permission mode"},
		"unknown model": {`{"type":"system","subtype":"init","model":"opsu","permissionMode":"auto"}` + "\n" +
			`{"type":"result","subtype":"success","is_error":true,"result":"There's an issue with the selected model (opsu)."}`,
			"There's an issue with the selected model (opsu)."},
		"limit used up": {`{"type":"system","subtype":"init","permissionMode":"auto"}` + "\n" +
			`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}` + "\n" +
			`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit"}`, ""},
		"no result": {`{"type":"system","subtype":"init","permissionMode":"auto"}`, "without a result"},
		"no output": {"", "did not start"},
	} {
		err := checkStream(strings.NewReader(c.stream), Model{Name: "m", Effort: "low"})
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: checkStream = %v, want %q", name, err, c.want)
		}
	}
}

func TestCheckRunFailure(t *testing.T) {
	for name, c := range map[string]struct {
		stdout string
		want   string // in the error besides the run's stderr
	}{
		"not started": {"", "run claude"},
		// As a timeout leaves it.
		"killed": {`{"type":"system","subtype":"init","permissionMode":"auto"}` + "\n", "without a result"},
	} {
		fakeClaude(t, c.stdout, "API Error: Connection reset", 1)
		err := (Claude{}).Check(t.Context(), Model{Name: "m", Effort: "low"})
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Connection reset") {
			t.Errorf("%s: Check = %v, want %q and the run's stderr", name, err, c.want)
		}
	}
}

// fakeClaude puts a claude on PATH that prints stdout and stderr and exits with code.
func fakeClaude(t *testing.T, stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"out": stdout, "err": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\ndir=$(dirname \"$0\")\ncat \"$dir/out\"\ncat \"$dir/err\" >&2\nexit %d\n", code)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRealCheck calls the real claude on this machine. It spends a little Claude usage, so it runs
// only with EXOMECHA_TEST_CLAUDE=1.
func TestRealCheck(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_CLAUDE") != "1" {
		t.Skip("set EXOMECHA_TEST_CLAUDE=1 to call the real claude")
	}
	if err := (Claude{}).Check(t.Context(), Model{Name: "opus", Effort: "low"}); err != nil {
		t.Errorf("Check(opus) = %v, want nil", err)
	}
	// Not haiku: claude sometimes reports auto permission mode for it.
	err := (Claude{}).Check(t.Context(), Model{Name: "opsu", Effort: "low"})
	if err == nil {
		t.Error("Check(opsu) = nil, want an error")
	}
	t.Logf("Check(opsu): %v", err)
}

func TestParseAuthStatus(t *testing.T) {
	// From claude 2.x, trimmed.
	for _, c := range []struct {
		out  string
		want bool
	}{
		{`{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty"}`, true},
		{`{"loggedIn": false}`, false},
	} {
		if got, err := parseAuthStatus([]byte(c.out)); err != nil || got != c.want {
			t.Errorf("parseAuthStatus(%s) = %v, %v, want %v", c.out, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "Not logged in", `{"authMethod": "none"}`} {
		if _, err := parseAuthStatus([]byte(bad)); err == nil {
			t.Errorf("parseAuthStatus(%q) = nil error", bad)
		}
	}
}

func TestUserPluginVersion(t *testing.T) {
	cases := []struct {
		out  string
		want string
	}{
		{`[{"id":"exomecha@exomecha","version":"0.4.0","scope":"user","enabled":true}]`, "0.4.0"},
		{`[{"id":"exomecha@exomecha","version":"0.4.0","scope":"local","enabled":true,"projectPath":"/x"}]`, ""},
		{`[{"id":"exomecha@exomecha","version":"0.4.0","scope":"user","enabled":false}]`, ""},
		{`[{"id":"codex@openai-codex","version":"1.0.0","scope":"user","enabled":true}]`, ""},
		{`[]`, ""},
	}
	for _, c := range cases {
		if got, err := userPluginVersion([]byte(c.out)); err != nil || got != c.want {
			t.Errorf("userPluginVersion(%s) = %q, %v, want %q", c.out, got, err, c.want)
		}
	}
	if _, err := userPluginVersion([]byte("Installed plugins:")); err == nil {
		t.Error("userPluginVersion of text = nil error")
	}
}

func TestHasMarketplace(t *testing.T) {
	out := `[{"name":"claude-community","source":"github"},{"name":"exomecha","source":"github","repo":"LeeShinYeoung/exomecha"}]`
	if got, err := hasMarketplace([]byte(out)); err != nil || !got {
		t.Errorf("hasMarketplace = %v, %v, want true", got, err)
	}
	if got, err := hasMarketplace([]byte(`[{"name":"claude-community"}]`)); err != nil || got {
		t.Errorf("hasMarketplace without exomecha = %v, %v, want false", got, err)
	}
}
