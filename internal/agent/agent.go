package agent

import (
	"context"
	"encoding/json"
)

type Request struct {
	Dir          string
	Instructions string
	Input        string
	Schema       string // JSON schema for the result, or empty for plain text only
}

type Result struct {
	Text       string
	Structured json.RawMessage
}

type Agent interface {
	Run(ctx context.Context, req Request) (Result, error)
}
