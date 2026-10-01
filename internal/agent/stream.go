package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// StreamArgs are Args for a run whose output is written to a file and read back after it ends.
func StreamArgs(req Request) []string {
	// Partial messages keep the output growing while the model writes a long answer, so a
	// silent run can be told apart from a busy one.
	return append(Args(req), "--output-format", "stream-json", "--verbose", "--include-partial-messages")
}

type Stream struct {
	PermissionMode string // as claude's init line reports it
	Rejected       bool   // claude reported a plan limit as used up
	Ended          bool   // claude wrote its result line
	IsError        bool
	Subtype        string
	Text           string
	Structured     json.RawMessage
}

// ReadStream reads the output of `claude -p --output-format stream-json --verbose`.
func ReadStream(r io.Reader) (Stream, error) {
	var s Stream
	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		var syntax *json.SyntaxError
		switch {
		case err == io.EOF:
			return s, nil
		// A run killed while writing leaves half a line. The stream ends there.
		case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
			return s, nil
		case err != nil:
			return s, err
		}
		var line struct {
			Type             string          `json:"type"`
			Subtype          string          `json:"subtype"`
			PermissionMode   string          `json:"permissionMode"`
			IsError          bool            `json:"is_error"`
			Result           string          `json:"result"`
			StructuredOutput json.RawMessage `json:"structured_output"`
			Info             struct {
				Status string `json:"status"`
			} `json:"rate_limit_info"`
		}
		// Line types this code does not know may use these field names for other kinds of values.
		if json.Unmarshal(raw, &line) != nil {
			continue
		}
		switch line.Type {
		case "system":
			if line.Subtype == "init" {
				s.PermissionMode = line.PermissionMode
			}
		case "rate_limit_event":
			if line.Info.Status == "rejected" {
				s.Rejected = true
			}
		case "result":
			s.Ended = true
			s.IsError = line.IsError
			s.Subtype = line.Subtype
			s.Text = line.Result
			s.Structured = line.StructuredOutput
		}
	}
}

// Result checks the run's result the way Run does.
func (s Stream) Result(schema bool) (Result, error) {
	switch {
	case !s.Ended:
		return Result{}, errors.New("claude ended without a result")
	case s.IsError:
		return Result{}, fmt.Errorf("claude returned an error (%s): %s", s.Subtype, s.Text)
	case schema && !hasValue(s.Structured):
		return Result{}, errors.New("claude output has no structured result")
	}
	return Result{Text: s.Text, Structured: s.Structured}, nil
}
