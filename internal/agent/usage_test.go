package agent

import (
	"os"
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

func TestParseUsage(t *testing.T) {
	u, err := parseUsage(strings.NewReader(streamWithUsage))
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{
		Known:    true,
		FiveHour: Window{Used: 0.17, ResetsAt: time.Unix(1790763000, 0)},
		Weekly:   Window{Used: 0.31, ResetsAt: time.Unix(1790917200, 0)},
	}
	if u != want {
		t.Errorf("parseUsage = %+v, want %+v", u, want)
	}
}

func TestParseUsageRejected(t *testing.T) {
	stream := strings.Replace(streamWithUsage, `"status":"allowed"`, `"status":"rejected"`, 1)
	u, err := parseUsage(strings.NewReader(stream))
	if err != nil || !u.Known || !u.Rejected {
		t.Errorf("parseUsage = %+v, %v, want a known, rejected usage", u, err)
	}
}

func TestParseUsageWithoutEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"result","subtype":"success","is_error":false,"result":"ok"}
`
	u, err := parseUsage(strings.NewReader(stream))
	if err != nil || u.Known {
		t.Errorf("parseUsage = %+v, %v, want an unknown usage", u, err)
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
