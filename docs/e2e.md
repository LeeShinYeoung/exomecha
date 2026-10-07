# End-to-end check

Unit tests cover exomecha against a fake GitHub, a fake claude and a fake sbx. The end-to-end suite runs the real thing: exomecha's own startup and its three workers, builder, reviewer and resolver, with the real sbx and the real claude. Only GitHub is replaced. The fake from `internal/github/githubtest` acts on a local bare repository the way GitHub acts on its own: it works out whether a PR merges, a merge really merges, and a merge into the default branch closes the PRD the PR names. exomecha checks every 5 seconds instead of every minute, and a scenario waits seconds for CI instead of 5 minutes.

Each scenario has its own fake GitHub, repository, sandboxes and template, so the scenarios run in parallel. Run the suite after changes to the workers, the prompts, or how exomecha uses sbx.

## Before running

- sbx set up as in the README: signed in to Docker, the balanced network policy, the Claude login, and no global `github` secret.
- This machine's claude login, with the same account. exomecha checks the models and reads the plan usage with it.
- Do not run two suites at once. A run starts by removing the sandboxes and templates named `e2e` that an earlier run left behind.

## Run

```
EXOMECHA_TEST_E2E=1 go test -run TestE2E -parallel 4 -timeout 60m ./cmd/exomecha
```

- Add `-v` to follow it, and `-run TestE2EConflict` to run one scenario.
- `-short` runs only the core scenarios: HappyPath, Restart, SentBackByPerson, Conflict, SandboxEnvironment and RunFailures.
- On a 16 GB Mac the whole suite took about 8 minutes at `-parallel 4`, and `-short` about 5 minutes. Most of a scenario is claude working, so more parallel scenarios finish sooner until sbx slows down for everyone.
- The first line of output names a file with the log of the whole run. A failing scenario prints its PRDs, PRs, sandboxes and its part of the log.
- Each scenario removes its sandboxes and templates when it ends, also when it fails. Stopping the run with Ctrl-C or `-timeout` skips that, and the next run removes them.

Component tests that need the real sbx or claude run only when asked:

```
EXOMECHA_TEST_SBX=1 go test ./internal/sandbox ./internal/template
EXOMECHA_TEST_CLAUDE=1 go test -run TestRealCheck ./internal/agent
```

## Scenarios

The practice project is plain POSIX sh, so claude needs nothing installed to build and test it. Runs that only have to exist for a check are frozen with `pkill -STOP` and dropped by closing their PRD, which saves Claude usage.

| Test | What it checks |
|---|---|
| HappyPath | A PRD with no tier label builds on the normal tier, the PR closes the PRD, the commit has the token owner as author and ends with `PRD: #<n>`, reviewer waits for the CI grace and approves with a COMMENT review, the PR is left open, a person's merge closes the PRD with `status:approved` kept, and nothing is left behind. |
| ParallelAndBlocked | Two free PRDs work at the same time. A PRD Blocked by another waits until that one's PR is merged, then builds on main with it. |
| Intake | Drafts, PRDs with no status, with two statuses or already stuck are left alone. Two tier labels take the heavier one. Relabeling a working PRD ready starts it over. |
| StuckAndBack | builder asks for a missing decision and goes stuck. After an answer and a relabel, it uses the answer. |
| BlockedHost | builder names the host the sandbox blocked. After the host is added to allowed-hosts, the PR opens. |
| Restart | Ten labels at the start and none added on a restart. A build and a review that went on while exomecha was stopped are collected without starting claude again. |
| SentBackByReview | A wrong commit gets a review asking for changes, and builder fixes it on the same PR. |
| SentBackByPerson | A person's review and line comment reach builder's and reviewer's input, comments from accounts without write access do not, and the PRD is approved again. |
| PushDuringReview | A review of a commit that is no longer the PR's head is thrown away, and the next review is of the new head. |
| PRClosedByHand | A PR closed without a merge sends its PRD to stuck. A merge into a branch other than the default one has reviewer close the PRD. |
| UsageLimit | Over the usage limit no new sandbox starts, a running review is still collected and a failed CI still gets its review. Under the limit the builds start. |
| Conflict | resolver merges main into a conflicting PR on the light tier, reviewer waits for it, and a conflict that shows up during a review drops that review. |
| Contradiction | A PR that main contradicts goes stuck with a question, and resolver settles it the way the answer says. builder never takes part. |
| ApprovedConflict | An approved PR that comes to conflict goes back to review, is resolved and approved again. An approved PR with no conflict is left alone. |
| SetupAndCommitChecks | The setup script runs before claude and can start a Docker service, its files stay out of the PR, builder's and resolver's commits pass a `commit-msg` hook, and a failing script sends the PRD to stuck without its output in the comment. |
| SandboxEnvironment | The template is built once, project sandboxes and reader sandboxes have their own limits, a project sandbox has the template, the allowed host, the commit identity and no token or remote, and a reader sandbox has only the code. |
| TemplateProblems | A Dockerfile that fails to build and `**` in allowed-hosts send the PRD to stuck, without the build output in the comment. |
| RunFailures | `sbx stop`, `sbx rm` and a frozen claude send the PRD to stuck with the reason and keep the run's files in `failed/`. A stopped review goes back to review. |
| StartRefusesBadModels | exomecha refuses to start on haiku or on a misspelled model. |

Not covered by the suite:
- Killing exomecha with `kill -9`. Starting again picks the runs up the same way as after Ctrl-C, and a start cut off halfway is covered by the unit tests.
- A global github secret in sbx. It is machine-wide and would break the other scenarios running at the same time. exomecha's check of it runs at every start in the suite, and its parsing is unit tested.

## On real GitHub

What the fake cannot show needs real GitHub. Check it after changes to the skills or to how exomecha talks to GitHub.

### Setup

1. A practice repository with a small project, its tests, and a CLAUDE.md that says how to install and test. One base branch, `main`, with no "require approval" protection rule, since the token owner cannot approve its own PRs.
2. A fine-grained token for that repository only: Issues read and write, Pull requests read and write, Contents read and write, Actions read.
3. Install exomecha and fill `<name>.env` (see the README). Run exomecha in its own terminal and keep the log in view:
   ```
   go install ./cmd/exomecha
   set -a; source <name>.env; set +a; ~/go/bin/exomecha
   ```
4. In the practice repository, install the plugin from your exomecha checkout:
   ```
   claude plugin marketplace add <path to exomecha> --scope local
   claude plugin install exomecha@exomecha --scope local
   ```
   After editing the skills, run `claude plugin update exomecha@exomecha` and restart Claude Code.
5. For the commands below, `R=<owner>/<practice repository>`.

### Labels and the token

- Start exomecha. `gh label list -R $R` shows the ten labels. Stop and start it again: still ten, and no error in the log.
- File a small PRD with `prd` and `status:ready`. It goes through builder and reviewer to `status:approved` with no permission error in the log.

### Skills

In the practice repository, in Claude Code:

- `/exomecha:draft-prd` with a real problem from the project, then again with a second problem that overlaps the first. Two issues with `prd` and `status:draft` appear, each with Problem, Investigation notes, Ideas and Open questions.
- `/exomecha:summary-all-drafts`. Each draft shows as `#<number> <title>` with a summary of up to three lines. No issue changes.
- `/exomecha:complete-prd <first draft>`. It reports what the latest `main` shows, and asks what to do with the second draft because it overlaps. Check the plan file before approving: the PRD sits under a `# PRD 1: <title>` heading, every ledger row is closed, the Filing row names the draft's number, a tier and the decision for the second draft, and the draft has every section of the body format. After approval the first draft's issue has the text under the heading as its body, `status:ready` in place of `status:draft`, and the tier's label. A second draft that was merged is closed with a comment that links the PRD.
- `/exomecha:complete-prd` with no number, after settling a small change in the conversation. A new issue appears with `prd`, `status:ready` and the tier's label. It has a comment with only an artifact URL, added before `status:ready` in the timeline, and the artifact shows the change in pictures before and after.
- `/exomecha:complete-prd` with a change too big for one PR. The plan proposes several PRDs, each under its own heading. After approval a later PRD that needs an earlier one is Blocked by it. Each issue's timeline shows `status:ready` and the tier label added after the Blocked by.
- `/exomecha:setup`. `.exomecha/Dockerfile` and `.exomecha/setup.sh` appear, and `.exomecha/allowed-hosts` too if a check needed a host. None of them is committed. The report has the files, how long the script took, the checks it ran, each added host with the step that needs it, and the blocked hosts it left out. Afterwards `sbx ls` and `sbx template ls` show nothing named `exomecha-setup-`.

### Merging on GitHub

- Let a PRD reach `status:approved`, then merge its PR on GitHub. GitHub closes the PRD as completed, and `status:approved` stays on it.

### GitHub Actions

- While a PRD is in `status:review`, stop exomecha and push to its branch a workflow that runs the tests, together with a test that fails. Start exomecha. reviewer waits while the run is in progress, then leaves a review listing the failed run, and the PRD goes `status:ready`. builder fixes it on the same PR.
