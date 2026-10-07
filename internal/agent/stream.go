package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type Stream struct {
	PermissionMode string // as claude's init line reports it
	Model          string // as claude's init line reports it
	Rejected       bool   // claude reported a plan limit as used up

	ended      bool // claude wrote its result line
	isError    bool
	subtype    string
	text       string
	structured json.RawMessage
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
			Model            string          `json:"model"`
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
				s.Model = line.Model
			}
		case "rate_limit_event":
			if line.Info.Status == "rejected" {
				s.Rejected = true
			}
		case "result":
			s.ended = true
			s.isError = line.IsError
			s.subtype = line.Subtype
			s.text = line.Result
			s.structured = line.StructuredOutput
		}
	}
}

func (s Stream) Result() (json.RawMessage, error) {
	switch {
	case !s.ended:
		return nil, errors.New("claude ended without a result")
	case s.isError:
		return nil, fmt.Errorf("claude returned an error (%s): %s", s.subtype, s.text)
	case len(s.structured) == 0 || string(s.structured) == "null":
		return nil, errors.New("claude output has no structured result")
	}
	return s.structured, nil
}
