package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

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
	stdout, stderr, runErr := probe(ctx, "--model", "haiku", "--effort", "low")

	// claude still reports the limits when it refuses the call because one is used up.
	u, err := parseUsage(bytes.NewReader(stdout))
	if err != nil || u.Known {
		return u, err
	}
	if runErr != nil {
		return Usage{}, fmt.Errorf("run claude: %w: %s", runErr, stderr)
	}
	return u, nil
}

// Check makes a short claude call on m in auto permission mode, as every worker's run is, so a
// model no run can use is found before the first run fails on it.
func (Claude) Check(ctx context.Context, m Model) error {
	stdout, stderr, runErr := probe(ctx, "--model", m.Name, "--effort", m.Effort, "--permission-mode", "auto")
	err := checkStream(bytes.NewReader(stdout), m)
	if errors.Is(err, errNotStarted) && runErr != nil {
		return fmt.Errorf("run claude: %w: %s", runErr, stderr)
	}
	return err
}

var errNotStarted = errors.New("claude did not start")

// SignedIn reports whether claude on this machine has a login, which the usage and model checks use.
func (Claude) SignedIn(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "claude", "auth", "status", "--json").Output()
	in, parseErr := parseAuthStatus(out)
	if parseErr != nil && err != nil {
		return false, fmt.Errorf("claude auth status: %w", err)
	}
	return in, parseErr
}

func parseAuthStatus(out []byte) (bool, error) {
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil || status.LoggedIn == nil {
		return false, fmt.Errorf("claude auth status printed no loggedIn: %q", out)
	}
	return *status.LoggedIn, nil
}

func checkStream(r io.Reader, m Model) error {
	s, err := ReadStream(r)
	if err != nil {
		return err
	}
	switch {
	case s.PermissionMode == "":
		return errNotStarted
	case s.PermissionMode != "auto":
		return fmt.Errorf("claude does not run %s in auto permission mode", m.Name)
	// A used up limit is a normal state that exomecha waits out, so it must not stop the start.
	case s.Rejected:
		return nil
	case !s.ended:
		return errors.New("claude ended without a result")
	case s.isError:
		return fmt.Errorf("claude returned an error: %s", s.text)
	}
	return nil
}

func probe(ctx context.Context, args ...string) (stdout []byte, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "claude", append([]string{"-p",
		"--system-prompt", "Reply with ok.",
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
	}, args...)...)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader("ok?")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.Bytes(), strings.TrimSpace(errOut.String()), err
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
