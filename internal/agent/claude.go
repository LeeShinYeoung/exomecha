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

func (Claude) Run(ctx context.Context, req Request) (Result, error) {
	args := []string{
		"-p",
		"--permission-mode", "auto",
		"--append-system-prompt", req.Instructions,
		"--output-format", "json",
		// Commits follow the project's own message rules, without claude's Co-Authored-By line.
		"--settings", `{"attribution":{"commit":""}}`,
	}
	if req.Schema != "" {
		args = append(args, "--json-schema", req.Schema)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
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
	if req.Schema != "" && (len(resp.StructuredOutput) == 0 || string(resp.StructuredOutput) == "null") {
		return Result{}, errors.New("claude output has no structured result")
	}
	return Result{Text: resp.Result, Structured: resp.StructuredOutput}, nil
}
