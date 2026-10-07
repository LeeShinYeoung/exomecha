package main

import "maps"

// The practice project is plain POSIX sh, so claude can build and test it in sbx's own template
// with nothing to install.
func project(extra ...map[string]string) map[string]string {
	files := map[string]string{
		"README.md": "# greet\n\nA tiny command line greeter written in POSIX sh.\n",
		"CLAUDE.md": `# greet

Plain POSIX sh. There is nothing to install or build.

- Run the tests from the repository root with ` + "`sh test.sh`" + `. It prints ` + "`ok`" + ` when every test passes.
- Each script has its tests in ` + "`tests/<script>_test.sh`" + `. Add tests there for every change.
- Keep the scripts POSIX sh, with no bash features.
`,
		"greet.sh": `#!/bin/sh
# Prints a greeting.
greeting="Hello"
name=${1:-world}
echo "$greeting, $name!"
`,
		"test.sh": `#!/bin/sh
# Runs every test in tests/ and prints ok when all of them pass.
set -e
for t in tests/*_test.sh; do
	sh "$t"
done
echo ok
`,
		"tests/greet_test.sh": `#!/bin/sh
set -e
fail() { echo "FAIL: $1" >&2; exit 1; }
[ "$(sh greet.sh)" = "Hello, world!" ] || fail "no argument"
[ "$(sh greet.sh Ada)" = "Hello, Ada!" ] || fail "a name"
`,
	}
	for _, e := range extra {
		maps.Copy(files, e)
	}
	return files
}

type prdText struct{ title, body string }

var shoutPRD = prdText{"Add a --shout flag to greet.sh", `## To do

` + "`sh greet.sh --shout`" + ` prints the greeting in capital letters: ` + "`HELLO, WORLD!`" + `. ` + "`sh greet.sh --shout Ada`" + ` prints ` + "`HELLO, ADA!`" + `. Without the flag nothing changes.

## Acceptance criteria

- The two commands above print exactly those lines.
- ` + "`tests/greet_test.sh`" + ` tests both, and ` + "`sh test.sh`" + ` prints ` + "`ok`" + `.

## Files

- greet.sh
- tests/greet_test.sh
`}

var farewellPRD = prdText{"Add farewell.sh", `## To do

Add a script ` + "`farewell.sh`" + ` that prints ` + "`Goodbye, world!`" + `, or ` + "`Goodbye, Ada!`" + ` for ` + "`sh farewell.sh Ada`" + `.

## Acceptance criteria

- The two commands print exactly those lines.
- ` + "`tests/farewell_test.sh`" + ` tests both, and ` + "`sh test.sh`" + ` prints ` + "`ok`" + `.

## Files

- farewell.sh (new)
- tests/farewell_test.sh (new)
`}

var helpPRD = prdText{"Add --help to greet.sh", `## To do

` + "`sh greet.sh --help`" + ` prints exactly one line, ` + "`usage: greet.sh [--shout] [name]`" + `, and nothing else. This builds on the ` + "`--shout`" + ` flag, which must already be on main.

## Acceptance criteria

- The command prints exactly that line.
- ` + "`tests/greet_test.sh`" + ` tests it, and ` + "`sh test.sh`" + ` prints ` + "`ok`" + `.

## Files

- greet.sh
- tests/greet_test.sh
`}

// officialPRD cannot be done without a decision only a person can make.
var officialPRD = prdText{"Print the team's official greeting", `## To do

Change ` + "`greet.sh`" + ` so it greets with the team's official greeting instead of "Hello".

The official greeting is not written anywhere in this repository or in this issue. Do not guess it or pick one yourself. If no comment on this issue says what it is, stop and ask for it.

## Acceptance criteria

- ` + "`sh greet.sh`" + ` prints the official greeting followed by ", world!".
- ` + "`tests/greet_test.sh`" + ` is updated, and ` + "`sh test.sh`" + ` prints ` + "`ok`" + `.
`}

// pagePRD needs a host the sandbox does not allow.
var pagePRD = prdText{"Record the hash of example.com", `## To do

Download https://example.com/ and write the SHA-256 of the response body, as 64 lowercase hex characters followed by a newline, to ` + "`page.sha256`" + ` at the repository root. You cannot know the hash without downloading the page, so do not make one up. If the download is blocked, stop and say which host was blocked.

## Acceptance criteria

- ` + "`page.sha256`" + ` holds the hash of the page as served when you downloaded it.
`}

// shoutDone is a correct implementation of shoutPRD, for scenarios that start with a PR already open.
var shoutDone = map[string]string{
	"greet.sh": `#!/bin/sh
# Prints a greeting. --shout prints it in capital letters.
shout=
if [ "$1" = "--shout" ]; then
	shout=1
	shift
fi
greeting="Hello"
name=${1:-world}
line="$greeting, $name!"
if [ -n "$shout" ]; then
	line=$(printf '%s\n' "$line" | tr '[:lower:]' '[:upper:]')
fi
echo "$line"
`,
	"tests/greet_test.sh": `#!/bin/sh
set -e
fail() { echo "FAIL: $1" >&2; exit 1; }
[ "$(sh greet.sh)" = "Hello, world!" ] || fail "no argument"
[ "$(sh greet.sh Ada)" = "Hello, Ada!" ] || fail "a name"
[ "$(sh greet.sh --shout)" = "HELLO, WORLD!" ] || fail "shout"
[ "$(sh greet.sh --shout Ada)" = "HELLO, ADA!" ] || fail "shout a name"
`,
}

// shoutBroken undoes shoutDone's script but keeps its tests, which a review has to catch.
var shoutBroken = map[string]string{
	"greet.sh": `#!/bin/sh
# Prints a greeting.
greeting="Hello"
name=${1:-world}
echo "$greeting, $name!"
`,
}

// mainRefactor rewrites the lines shoutDone changes without changing what greet.sh prints, so a
// shout PR conflicts with main after it and resolver has nothing to ask about.
var mainRefactor = map[string]string{
	"greet.sh": `#!/bin/sh
# Prints a greeting to standard output.
greeting='Hello'
name="${1:-world}"
printf '%s, %s!\n' "$greeting" "$name"
`,
	"tests/greet_test.sh": `#!/bin/sh
set -e
fail() { echo "FAIL: $1" >&2; exit 1; }
[ "$(sh greet.sh)" = "Hello, world!" ] || fail "no argument"
[ "$(sh greet.sh Ada)" = "Hello, Ada!" ] || fail "a name"
[ "$(sh greet.sh 'Ada Lovelace')" = "Hello, Ada Lovelace!" ] || fail "a full name"
`,
}

// mainNoFlags conflicts with a shout PR and also forbids what it adds.
var mainNoFlags = map[string]string{
	"greet.sh": `#!/bin/sh
# Prints a greeting. It takes no flags: every argument is a name.
greeting="Hello"
name=${*:-world}
echo "$greeting, $name!"
`,
	"CLAUDE.md": `# greet

Plain POSIX sh. There is nothing to install or build.

- Run the tests from the repository root with ` + "`sh test.sh`" + `. It prints ` + "`ok`" + ` when every test passes.
- Each script has its tests in ` + "`tests/<script>_test.sh`" + `. Add tests there for every change.
- Keep the scripts POSIX sh, with no bash features.
- Decided: greet.sh takes no flags, ever. Every argument is part of the name. Do not add flags such as --shout.
`,
}

var farewellDone = map[string]string{
	"farewell.sh": "#!/bin/sh\n# Says goodbye.\nname=${1:-world}\necho \"Goodbye, $name!\"\n",
	"tests/farewell_test.sh": `#!/bin/sh
set -e
fail() { echo "FAIL: $1" >&2; exit 1; }
[ "$(sh farewell.sh)" = "Goodbye, world!" ] || fail "no argument"
[ "$(sh farewell.sh Ada)" = "Goodbye, Ada!" ] || fail "a name"
`,
}

// mainFunction rewrites greet.sh once more, still printing the same, so a branch that merged
// mainRefactor conflicts again.
var mainFunction = map[string]string{
	"greet.sh": `#!/bin/sh
# Prints a greeting to standard output.
greet() {
	printf '%s, %s!\n' "$1" "$2"
}
greet Hello "${1:-world}"
`,
}

var wavePRD = prdText{"Add wave.sh", `## To do

Add a script ` + "`wave.sh`" + ` that prints ` + "`*waves at world*`" + `, or ` + "`*waves at Ada*`" + ` for ` + "`sh wave.sh Ada`" + `.

## Acceptance criteria

- The two commands print exactly those lines.
- ` + "`tests/wave_test.sh`" + ` tests both, and ` + "`sh test.sh`" + ` prints ` + "`ok`" + `.

## Files

- wave.sh (new)
- tests/wave_test.sh (new)
`}
