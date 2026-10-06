package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type Claude struct{}

type Model struct {
	Name   string
	Effort string
}

func (m Model) String() string { return m.Name + ":" + m.Effort }

// As `claude --help` lists them.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ParseModel reads model:effort, such as sonnet:low.
func ParseModel(s string) (Model, error) {
	// The last colon, so a model name with colons of its own still parses.
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return Model{}, errors.New("no effort after a colon")
	}
	m := Model{Name: s[:i], Effort: s[i+1:]}
	if m.Name == "" {
		return Model{}, errors.New("no model before the colon")
	}
	if !slices.Contains(efforts, m.Effort) {
		return Model{}, fmt.Errorf("effort %q is not one of %s", m.Effort, strings.Join(efforts, ", "))
	}
	return m, nil
}

// Args are the arguments of a worker's claude run. Its output goes to a file that ReadStream reads.
func Args(instructions, schema string, m Model) []string {
	return []string{
		"-p",
		"--permission-mode", "auto",
		"--model", m.Name,
		"--effort", m.Effort,
		"--append-system-prompt", instructions,
		// Commits follow the project's own message rules, without claude's Co-Authored-By line.
		"--settings", `{"attribution":{"commit":""}}`,
		"--json-schema", schema,
		// Partial messages keep the output growing while the model writes a long answer, so a
		// silent run can be told apart from a busy one.
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
	}
}
