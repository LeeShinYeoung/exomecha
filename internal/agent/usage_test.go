package agent

import (
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
