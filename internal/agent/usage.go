package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Usage is how much of the Claude plan's limits is used, as claude reports it.
type Usage struct {
	Known    bool // false when claude reports no plan limits, e.g. when logged in with an API key
	Rejected bool // claude already refuses new work until a limit resets
	FiveHour Window
	Weekly   Window
}

type Window struct {
	Used     float64 // from 0 to 1
	ResetsAt time.Time
}

// Usage makes the cheapest claude call it can, because claude reports the plan's limits only
// alongside a real answer and has no command that reads them on their own.
func (Claude) Usage(ctx context.Context) (Usage, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", "haiku",
		"--effort", "low",
		"--system-prompt", "Reply with ok.",
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader("ok?")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// claude still reports the limits when it refuses the call because one is used up.
	u, err := parseUsage(&stdout)
	if err != nil || u.Known {
		return u, err
	}
	if runErr != nil {
		return Usage{}, fmt.Errorf("run claude: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return u, nil
}

func parseUsage(r io.Reader) (Usage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var line struct {
			Type string `json:"type"`
			Info struct {
				Status  string `json:"status"`
				Windows map[string]struct {
					Utilization float64 `json:"utilization"`
					ResetsAt    int64   `json:"resetsAt"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Type != "rate_limit_event" {
			continue
		}
		window := func(name string) Window {
			w, ok := line.Info.Windows[name]
			if !ok {
				return Window{}
			}
			return Window{Used: w.Utilization, ResetsAt: time.Unix(w.ResetsAt, 0)}
		}
		return Usage{
			Known:    true,
			Rejected: line.Info.Status == "rejected",
			FiveHour: window("five_hour"),
			Weekly:   window("seven_day"),
		}, nil
	}
	return Usage{}, sc.Err()
}
