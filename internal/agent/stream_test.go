package agent

import (
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
	if s.PermissionMode != "auto" || !s.Ended || s.Rejected {
		t.Errorf("stream = %+v, want an ended auto run", s)
	}
	res, err := s.Result(true)
	if err != nil || string(res.Structured) != `{"result":"done"}` {
		t.Errorf("Result = %s, %v, want the structured output", res.Structured, err)
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
	if _, err := s.Result(true); err == nil || !strings.Contains(err.Error(), "You've hit your limit") {
		t.Errorf("Result error = %v, want claude's error", err)
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
	if s.PermissionMode != "auto" || s.Ended {
		t.Errorf("stream = %+v, want an auto run with no result", s)
	}
	if _, err := s.Result(false); err == nil {
		t.Error("Result succeeded without a result line")
	}
}

func TestReadStreamWithoutStructuredResult(t *testing.T) {
	stream := `{"type":"result","subtype":"success","is_error":false,"result":"plain text"}` + "\n"
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Result(true); err == nil {
		t.Error("Result succeeded without the structured output the schema asks for")
	}
	if res, err := s.Result(false); err != nil || res.Text != "plain text" {
		t.Errorf("Result without a schema = %+v, %v", res, err)
	}
}

func TestReadStreamSkipsLinesOfOtherShapes(t *testing.T) {
	stream := `{"type":"tool_progress","result":{"not":"a string"}}
` + streamDone
	s, err := ReadStream(strings.NewReader(stream))
	if err != nil || !s.Ended {
		t.Errorf("stream = %+v, %v, want the result after a line of another shape", s, err)
	}
}

func TestArgs(t *testing.T) {
	args := Args(Request{Instructions: "be brief", Schema: `{"type":"object"}`})
	for _, want := range [][]string{{"--permission-mode", "auto"}, {"--append-system-prompt", "be brief"}, {"--json-schema", `{"type":"object"}`}} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("args %q lack %q %q", args, want[0], want[1])
		}
	}
	for _, a := range StreamArgs(Request{}) {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") || a == "--json-schema" {
			t.Errorf("stream args contain %q: %q", a, StreamArgs(Request{}))
		}
	}
}
