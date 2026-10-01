package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type Claude struct{}

// Args are the arguments a worker's claude run gets wherever it runs, on this machine or in a sandbox.
func Args(req Request) []string {
	args := []string{
		"-p",
		"--permission-mode", "auto",
		"--append-system-prompt", req.Instructions,
		// Commits follow the project's own message rules, without claude's Co-Authored-By line.
		"--settings", `{"attribution":{"commit":""}}`,
	}
	if req.Schema != "" {
		args = append(args, "--json-schema", req.Schema)
	}
	return args
}

func (Claude) Run(ctx context.Context, req Request) (Result, error) {
	cmd := exec.CommandContext(ctx, "claude", append(Args(req), "--output-format", "json")...)
	cmd.Dir = req.Dir
	// Input goes through stdin, not an argument, so input starting with --- is not read as a flag.
	cmd.Stdin = strings.NewReader(req.Input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var resp struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		if runErr != nil {
			return Result{}, fmt.Errorf("run claude: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return Result{}, fmt.Errorf("read claude output: %w", err)
	}
	if resp.IsError || runErr != nil {
		return Result{}, fmt.Errorf("claude returned an error (%s): %s", resp.Subtype, resp.Result)
	}
	if req.Schema != "" && !hasValue(resp.StructuredOutput) {
		return Result{}, errors.New("claude output has no structured result")
	}
	return Result{Text: resp.Result, Structured: resp.StructuredOutput}, nil
}

func hasValue(v json.RawMessage) bool {
	return len(v) > 0 && string(v) != "null"
}
