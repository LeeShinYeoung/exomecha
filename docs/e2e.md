# End-to-end check

Unit tests cover exomecha against a fake GitHub, a fake claude and a fake sbx. The end-to-end suite runs the real thing: exomecha's own startup and its three workers, builder, reviewer and resolver, with the real sbx and the real claude. Only GitHub is replaced. The fake from `internal/github/githubtest` acts on a local bare repository the way GitHub acts on its own: it works out whether a PR merges, a merge really merges, and a merge into the default branch closes the PRD the PR names. exomecha checks every 5 seconds instead of every minute, and a scenario waits seconds for CI instead of 5 minutes.

Each scenario has its own fake GitHub, repository and sandboxes, so the scenarios run in parallel. The suite keeps only what needs the real claude or the real sbx. exomecha's own decisions, such as which PRD a worker takes, the sandbox limits and the stuck reasons, are in the unit tests. Run the suite after changes to the workers, the prompts, or how exomecha uses sbx.

## Before running

- sbx set up as in the README: signed in to Docker, the balanced network policy, the Claude login, and no global `github` secret.
- This machine's claude login, with the same account. exomecha checks the models and reads the plan usage with it.
- git 2.38 or later. The fake GitHub works out whether a PR merges with `git merge-tree --write-tree`, which older git does not have.
- Do not run two suites at once. A run starts by removing the sandboxes named `e2e` that an earlier run left behind.

## Run

```
EXOMECHA_TEST_E2E=1 go test -run TestE2E -parallel 4 -timeout 60m ./cmd/exomecha
```

- Add `-v` to follow it, and `-run TestE2EConflict` to run one scenario.
- `-short` runs only the core scenarios: HappyPath, Restart, SentBackByPerson and Conflict.
- On a 16 GB Mac with 8 cores the whole suite took about 3 minutes at `-parallel 4`. A run takes about 50 seconds, half of it starting the sandbox. `-parallel 5` was no faster, since every scenario ran slower, and `-parallel 6` overloaded the machine until scenarios timed out.
- The first line of output names a file with the log of the whole run. A failing scenario prints its PRDs, PRs, sandboxes, the outputs kept for its stuck PRDs and its part of the log. A scenario fails at the first WARN or ERROR about it, or the first PRD in stuck it did not send there.
- Each scenario removes its sandboxes when it ends, also when it fails. Stopping the run with Ctrl-C or `-timeout` skips that, and the next run removes them.

Component tests that need the real sbx or claude run only when asked:

```
EXOMECHA_TEST_SBX=1 go test ./internal/sandbox ./internal/template
EXOMECHA_TEST_CLAUDE=1 go test -run TestRealCheck ./internal/agent
```

- `TestRealSandbox`: code goes in and out of a sandbox with no remote left in it. ssh.github.com is denied, an allowed host answers, and any other host needs approval. After `sbx stop` the sandbox is listed as not running and a detached command writes no exit file, which is how a worker tells a stopped run.
- `TestRealBuild`: a template builds once, a sandbox made from it the way a worker makes one has what the Dockerfile wrote, and a failed build keeps its output out of the error.

## Scenarios

The practice project is plain POSIX sh, so claude needs nothing installed to build and test it.

| Test | What it checks |
|---|---|
| HappyPath | A PRD with no tier label builds with opus, the PR closes the PRD, the commit has the token owner as author and ends with `PRD: #<n>`, the PR's tests pass, builder leaves its summary on the PR, and reviewer approves with a COMMENT review and leaves the PR open. |
| StuckAndBack | builder asks for a missing decision and goes stuck with a notice that names it. After an answer and a relabel, it uses the answer. |
| BlockedHost | builder names the host the sandbox blocked. After the host is added to allowed-hosts, the PR opens. |
| Restart | Stopping exomecha leaves a running build's sandbox alone, and after a restart the build is collected without starting claude again. |
| SentBackByReview | A wrong commit gets reviewer's review asking for changes, which reaches builder's record, and builder fixes it on the same PR. |
| SentBackByPerson | A person's review and line comment reach builder's record after reviewer's approval, comments from accounts without write access do not, and builder does what the person asked on the same PR. |
| Conflict | resolver merges main into a conflicting PR on its model for the PRD's tier, with a merge commit that ends with `PRD: #<n>` and passing tests. reviewer does not take the PR while it conflicts. |
| Contradiction | A PR that main contradicts goes stuck with a question, and resolver settles it the way the answer says. builder never takes part. |
| SetupAndCommitChecks | The setup script runs before claude and can start a Docker service, its files stay out of the PR, and builder's and resolver's commits pass a `commit-msg` hook. |
| StartRefusesUnknownModel | exomecha refuses to start on a misspelled model. |

Not covered by the suite:
- Killing exomecha with `kill -9`. Starting again picks the runs up the same way as after Ctrl-C, and a start cut off halfway is covered by the unit tests.
- A global github secret in sbx. It is machine-wide and would break the other scenarios running at the same time. exomecha's check of it runs at every start in the suite, and its parsing is unit tested.

## On real GitHub

What the fake cannot show needs real GitHub. Check it after changes to the skills or to how exomecha talks to GitHub.

### Setup

1. A practice repository with a small project, its tests, and a CLAUDE.md that says how to install and test. One base branch, `main`, with no "require approval" protection rule, since the token owner cannot approve its own PRs.
2. A fine-grained token for that repository only: Issues read and write, Pull requests read and write, Contents read and write, Actions read.
3. Install exomecha and run `exomecha init <name>` (see the README). Run exomecha in its own terminal and keep the log in view:
   ```
   go install ./cmd/exomecha
   ~/go/bin/exomecha init <name>
   ~/go/bin/exomecha run <name>
   ```
4. init installs the plugin from GitHub for your user. To try the skills of your checkout instead, install the plugin from it in the practice repository:
   ```
   claude plugin marketplace add <path to exomecha> --scope local
   claude plugin install exomecha@exomecha --scope local
   ```
   After editing the skills, run `claude plugin update exomecha@exomecha` and restart Claude Code.
5. For the commands below, `R=<owner>/<practice repository>`.

### Labels and the token

- Start exomecha. `gh label list -R $R` shows the ten labels. Stop and start it again: still ten, and no error in the log.
- `~/.exomecha/<project>/logs/exomecha.log` holds the same lines as the terminal, and only you can read it. Start a second exomecha for the same project while the first runs: it says another one is already running and exits with status 1.
- File a small PRD with `prd` and `status:ready`. It goes through builder and reviewer to `status:approved` with no permission error in the log.

### Skills

In the practice repository, in Claude Code:

- `/exomecha:draft-prd` with a real problem from the project, then again with a second problem that overlaps the first. Two issues with `prd` and `status:draft` appear, each with Problem, Investigation notes, Ideas and Open questions.
- `/exomecha:summary-all-drafts`. Each draft shows as `#<number> <title>` with a summary of up to three lines. No issue changes.
- `/exomecha:complete-prd <first draft>`. It reports what the latest `main` shows, and asks what to do with the second draft because it overlaps. Check the plan file before approving: the PRD sits under a `# PRD 1: <title>` heading, every ledger row is closed, the Filing row names the draft's number, a tier and the decision for the second draft, and the draft has every section of the body format. After approval the first draft's issue has the text under the heading as its body, `status:ready` in place of `status:draft`, and the tier's label. A second draft that was merged is closed with a comment that links the PRD.
- `/exomecha:complete-prd` with no number, after settling a small change in the conversation. A new issue appears with `prd`, `status:ready` and the tier's label. It has a comment with only an artifact URL, added before `status:ready` in the timeline, and the artifact shows the change in pictures before and after.
- `/exomecha:complete-prd` with a change too big for one PR. The plan proposes several PRDs, each under its own heading. After approval a later PRD that needs an earlier one is Blocked by it. Each issue's timeline shows `status:ready` and the tier label added after the Blocked by.
- `exomecha init <name>` for a project with no setup files says to run `/exomecha:setup <name>`. Run it. `Dockerfile` and `setup.sh` appear in `~/.exomecha/<name>/setup/`, and `allowed-hosts` too if a check needed a host. No repository changes. The report has the files, how long the script took, the checks it ran, each added host with the step that needs it, and the blocked hosts it left out. Afterwards `sbx ls` and `sbx template ls` show nothing named `exomecha-setup-`.

### Merging on GitHub

- Let a PRD reach `status:approved`, then merge its PR on GitHub. GitHub closes the PRD as completed, and `status:approved` stays on it.

### GitHub Actions

- While a PRD is in `status:review`, stop exomecha and push to its branch a workflow that runs the tests, together with a test that fails. Start exomecha. reviewer waits while the run is in progress, then leaves a review listing the failed run, and the PRD goes `status:ready`. builder fixes it on the same PR.
