package agent

type Claude struct{}

// Args are the arguments of a worker's claude run. Its output goes to a file that ReadStream reads.
func Args(instructions, schema string) []string {
	return []string{
		"-p",
		"--permission-mode", "auto",
		"--append-system-prompt", instructions,
		// Commits follow the project's own message rules, without claude's Co-Authored-By line.
		"--settings", `{"attribution":{"commit":""}}`,
		"--json-schema", schema,
		// Partial messages keep the output growing while the model writes a long answer, so a
		// silent run can be told apart from a busy one.
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
	}
}
