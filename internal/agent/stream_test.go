package agent

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Lines from a real run in a sandbox, trimmed. The partial message lines are what keep the file growing.
const streamDone = `{"type":"system","subtype":"commands_changed","commands":[]}
{"type":"system","subtype":"init","cwd":"/home/agent/repo","model":"claude-sonnet-5","permissionMode":"auto","claude_code_version":"2.1.280"}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.17,"resetsAt":1790763000}}}}
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"{\"result\":\"done\"}","structured_output":{"result":"done"}}
`

func TestReadStream(t *testing.T) {
	s, err := ReadStream(strings.NewReader(streamDone))
	if err != nil {
		t.Fatal(err)
	}
	if s.PermissionMode != "auto" || s.Model != "claude-sonnet-5" || !s.ended || s.Rejected {
		t.Errorf("stream = %+v, want an ended auto run on claude-sonnet-5", s)
	}
	res, err := s.Result()
	if err != nil || string(res) != `{"result":"done"}` {
		t.Errorf("Result = %s, %v, want the structured output", res, err)
	}
}

func TestReadStreamOtherPermissionMode(t *testing.T) {
	stream := strings.Replace(streamDone, `"permissionMode":"auto"`, `"permissionMode":"default"`, 1)
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil || s.PermissionMode != "default" {
		t.Errorf("PermissionMode = %q, %v, want default", s.PermissionMode, err)
	}
}

func TestReadStreamErrorResult(t *testing.T) {
	stream := `{"type":"system","subtype":"init","permissionMode":"auto"}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}
{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit"}
`
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Rejected {
		t.Error("a rejected rate limit event was not noticed")
	}
	if _, err := s.Result(); err == nil || !strings.Contains(err.Error(), "You've hit your limit") {
		t.Errorf("Result error = %v, want claude's error", err)
	}
}

func TestReadStreamRejectedWithWindowsOfAnotherShape(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","unifiedWindows":{"five_hour":{"utilization":"high"}}}}` + "\n"
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil || !s.Rejected || s.UsageKnown {
		t.Errorf("stream = %+v, %v, want a rejection without known usage", s, err)
	}
}

func TestReadStreamWithoutResult(t *testing.T) {
	// A run killed in the middle of a line.
	stream := `{"type":"system","subtype":"init","permissionMode":"auto"}
{"type":"assistant","message":{"content":[{"type":"te`
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if s.PermissionMode != "auto" || s.ended {
		t.Errorf("stream = %+v, want an auto run with no result", s)
	}
	if _, err := s.Result(); err == nil {
		t.Error("Result succeeded without a result line")
	}
}

func TestReadStreamWithoutStructuredResult(t *testing.T) {
	stream := `{"type":"result","subtype":"success","is_error":false,"result":"plain text"}` + "\n"
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Result(); err == nil {
		t.Error("Result succeeded without the structured output the schema asks for")
	}
}

// A background task that ended after the result woke claude, which wrote a second result.
func TestReadStreamResumedAfterResult(t *testing.T) {
	stream := streamDone + `{"type":"result","subtype":"success","is_error":false,"result":"more text"}` + "\n"
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Result(); !errors.Is(err, ErrResumed) {
		t.Errorf("Result error = %v, want ErrResumed", err)
	}
}

func TestReadStreamSkipsLinesOfOtherShapes(t *testing.T) {
	stream := `{"type":"tool_progress","result":{"not":"a string"}}
` + streamDone
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil || !s.ended {
		t.Errorf("stream = %+v, %v, want the result after a line of another shape", s, err)
	}
}

func TestArgs(t *testing.T) {
	args := Args("be brief", `{"type":"object"}`, Model{Name: "sonnet", Effort: "low"})
	for _, want := range [][]string{
		{"--permission-mode", "auto"}, {"--model", "sonnet"}, {"--effort", "low"}, {"--append-system-prompt", "be brief"}, {"--json-schema", `{"type":"object"}`}, {"--output-format", "stream-json"},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("args %q lack %q %q", args, want[0], want[1])
		}
	}
	for _, a := range args {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") {
			t.Errorf("args contain %q: %q", a, args)
		}
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) || json.Unmarshal([]byte(args[i+1]), &settings) != nil {
		t.Fatalf("args %q lack --settings with JSON", args)
	}
	if settings.Env["CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"] != "1" || settings.Env["BASH_MAX_TIMEOUT_MS"] != "1800000" {
		t.Errorf("settings env = %v, want background tasks off and a 30 minute command limit", settings.Env)
	}
}

func TestParseModel(t *testing.T) {
	for in, want := range map[string]Model{
		"sonnet:low": {"sonnet", "low"},
		"opus:max":   {"opus", "max"},
		"arn:aws:bedrock:us-east-1:1:model/claude:high": {"arn:aws:bedrock:us-east-1:1:model/claude", "high"},
	} {
		if got, err := ParseModel(in); err != nil || got != want {
			t.Errorf("ParseModel(%q) = %+v, %v, want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "sonnet", ":low", "sonnet:", "sonnet:extreme", "sonnet:LOW"} {
		if got, err := ParseModel(in); err == nil {
			t.Errorf("ParseModel(%q) = %+v, want an error", in, got)
		}
	}
}
