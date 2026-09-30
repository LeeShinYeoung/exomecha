# End-to-end check

Unit tests cover coworker against a fake GitHub and a fake claude. This checklist runs the real thing on a practice repository: real GitHub, real claude, real skills. Run it once after changes to the workers, the prompts, or the skills.

## Setup

1. A practice repository with a small project, its tests, and a CLAUDE.md that says how to install and test. One base branch, `main`, with no "require approval" protection rule. No GitHub Actions workflow on `main` at the start.
2. A fine-grained token for that repository only: Issues read and write, Pull requests read and write, Contents read and write, Actions read.
3. Install coworker and fill `<name>.env` (see the README). Run coworker in its own terminal and keep the log in view:
   ```
   go install ./cmd/coworker
   set -a; source <name>.env; set +a; ~/go/bin/coworker
   ```
4. In the practice repository, install the plugin from your coworker checkout:
   ```
   claude plugin marketplace add <path to coworker> --scope local
   claude plugin install coworker@coworker --scope local
   ```
   After editing the skills, run `claude plugin update coworker@coworker` and restart Claude Code.
5. For the commands below:
   ```
   R=<owner>/<practice repository>
   ```

Stopping coworker with Ctrl-C pauses every worker. Use it to line up the state a scenario needs, then start coworker again.

## Scenarios

### 1. Labels

- Start coworker. `gh label list -R $R` shows the nine labels.
- Stop and start it again. Still nine, and no error in the log.

### 2. Skills

In the practice repository, in Claude Code:

- `/coworker:issue` with a real problem from the project. A `type:problem` issue appears with Problem and Investigation notes, and no solution.
- `/coworker:discuss <problem>`. The problem gets one comment with Direction, Re-checked on, Options considered, Decisions made, Open questions.
- `/coworker:prd <problem>`. Check the plan file before approving: every ledger row is closed, and the draft has every section of the body format. After approval a `type:prd` issue appears as a sub-issue of the problem, with the same body.

### 3. splitter and sequencer

- Within a minute of the PRD appearing, Tasks appear as its sub-issues. Each body has To do, Files, Acceptance criteria, Test scenarios, Cautions, Out of scope, Context.
- Order inside the PRD shows as Blocked by. The PRD gets `tasked` last.
- Each Task goes `status:pending` then `status:queued`.

### 4. Parallel builds and overlap

- Stop coworker. File two PRDs whose Tasks touch different files, and a third whose Task touches the same file as the first.
- Start coworker. The first two Tasks are `status:working` at the same time and each gets its own PR.
- The third Task gets Blocked by the first Task and stays `status:queued` until that one closes.

### 5. Review, merge, and close

- With no CI, reviewer waits five minutes after the PR's last commit, then reviews.
- On approve: the PR is merged with a merge commit, the Task closes as completed with no status label, and the PRD and then the problem close once all their sub-issues are closed.

### 6. Sent back by review

- While a Task is in `status:review`, stop coworker and push an obviously wrong commit to its branch (for example, undo the fix).
- Start coworker. reviewer leaves a COMMENT review and the Task goes `status:queued`. builder pushes a fix to the same PR. No second PR appears.

### 7. CI failure

- While a Task is in `status:review`, stop coworker and push to its branch a workflow that runs the tests, together with a test that fails:
  ```yaml
  # .github/workflows/ci.yml
  name: ci
  on: pull_request
  jobs:
    test:
      runs-on: ubuntu-latest
      steps:
        - uses: actions/checkout@v4
        - uses: actions/setup-go@v5
          with:
            go-version: stable
        - run: go test ./...
  ```
- Start coworker. reviewer waits while the run is in progress, then leaves a review listing the failed run, and the Task goes `status:queued`. builder fixes it on the same PR.

### 8. stuck and back

- Create a Task that cannot be done without a missing decision:
  ```
  gh issue create -R $R --title "Print the official greeting" \
    --body "## To do
  Print the team's official greeting when the CLI runs without arguments." \
    --label type:task --label status:pending
  ```
- builder comments why it is blocked and what needs deciding, and the Task goes `status:stuck`.
- Answer in a comment, then relabel it:
  ```
  gh issue edit <task> -R $R --remove-label status:stuck --add-label status:pending
  ```
- It goes through sequencer and builder again and uses the answer.

### 9. Conflict

- When a Task reaches `status:review`, stop coworker and push a commit to `main` that changes the same lines its PR changes.
- Start coworker. Once GitHub marks the PR as conflicting, resolver merges `main` into the branch, resolves it, and pushes. The Task stays `status:review`, and reviewer reviews it again. reviewer never picks the Task while its PR is conflicting.
- Repeat with a commit on `main` that contradicts the PR (for example, it explicitly refuses the feature the PR adds). resolver sends the Task to `status:stuck` and asks which side wins. Answer in a comment and relabel it `status:review`. resolver merges again and settles the conflict the way you answered. builder is not involved.

### 10. Restart

- While a Task is `status:working`, stop coworker with Ctrl-C. The Task goes back to `status:queued`.
- Again while a Task is `status:working`, kill coworker hard (`kill -9 <pid>`). The Task stays `status:working`. Start coworker: the log says it requeued the Task, and builder picks it up again.

### 11. PR closed by hand

- While a Task is in `status:review`, close its PR on GitHub without merging it.
- reviewer sends the Task to `status:stuck` and says the PR was closed without being merged. Close the Task if the work is not wanted, or relabel it `status:pending` and builder opens a new PR.

### 12. Commit checks

- Add a commit message check to the practice repository that only accepts first lines like `feat: add Shout`, and turn it on in coworker's clone: `git -C ~/.coworker/<owner>/<repo>/repo config core.hooksPath .githooks`.
- File a Task. builder's commit passes the check and the PR opens. The commit follows the format and ends with `Task: #<task>`.
- Make a conflict as in scenario 9. resolver's merge commit passes the check too.
- Turn the check off again: `git -C ~/.coworker/<owner>/<repo>/repo config --unset core.hooksPath`.

### 13. Claude usage limit

- Start coworker with `COWORKER_FIVE_HOUR_LIMIT=1` and file a Task. The log says once that usage is over the limit, and the Task does not move.
- Restart coworker without it. Within a minute the Task moves on.
