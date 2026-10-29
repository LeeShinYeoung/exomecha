package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type Stream struct {
	PermissionMode string // as claude's init line reports it
	Model          string // as claude's init line reports it
	Rejected       bool   // claude reported a plan limit as used up
	UsageKnown     bool   // false when no rate limit event reported the windows
	FiveHour       Window // as the last rate limit event reports it
	Weekly         Window // as the last rate limit event reports it

	ended      bool // claude wrote its result line
	resumed    bool // claude went on after its result line and wrote another
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
		// The first text that is not JSON ends the stream, wherever it is, and nothing after it is
		// read. A run killed while writing leaves half a line at the end.
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
			// Apart from line, so a window of another shape cannot hide a rejection.
			var limits struct {
				Info struct {
					Windows map[string]struct {
						Utilization float64 `json:"utilization"`
						ResetsAt    int64   `json:"resetsAt"`
					} `json:"unifiedWindows"`
				} `json:"rate_limit_info"`
			}
			if json.Unmarshal(raw, &limits) == nil {
				window := func(name string) Window {
					w, ok := limits.Info.Windows[name]
					if !ok {
						return Window{}
					}
					return Window{Used: w.Utilization, ResetsAt: time.Unix(w.ResetsAt, 0)}
				}
				s.UsageKnown = true
				s.FiveHour, s.Weekly = window("five_hour"), window("seven_day")
			}
		case "result":
			s.resumed = s.ended
			s.ended = true
			s.isError = line.IsError
			s.subtype = line.Subtype
			s.text = line.Result
			s.structured = line.StructuredOutput
		}
	}
}

var ErrResumed = errors.New("claude went on after its result")

func (s Stream) Result() (json.RawMessage, error) {
	switch {
	case !s.ended:
		return nil, errors.New("claude ended without a result")
	// The result it gave may no longer describe the work.
	case s.resumed:
		return nil, ErrResumed
	case s.isError:
		return nil, fmt.Errorf("claude returned an error (%s): %s", s.subtype, s.text)
	case len(s.structured) == 0 || string(s.structured) == "null":
		return nil, errors.New("claude output has no structured result")
	}
	return s.structured, nil
}
