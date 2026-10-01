# End-to-end check

Unit tests cover coworker against a fake GitHub, a fake claude and a fake sbx. This checklist runs the real thing on a practice repository: real GitHub, real claude, real sbx, real skills. Run it once after changes to the workers, the prompts, or the skills.

Tests against the real sbx and claude, with no GitHub, run only when asked. Run them after changes to how coworker uses sbx:

```
COWORKER_TEST_SBX=1 go test ./internal/sandbox ./internal/template
COWORKER_TEST_SBX=1 COWORKER_TEST_CLAUDE=1 go test -run TestReal ./internal/worker
```

The first takes about 2 minutes. The second spends a little Claude usage and takes a few minutes. Both remove every sandbox and template they create.

## Setup

1. A practice repository with a small project, its tests, and a CLAUDE.md that says how to install and test. One base branch, `main`, with no "require approval" protection rule. No GitHub Actions workflow on `main` at the start.
2. A fine-grained token for that repository only: Issues read and write, Pull requests read and write, Contents read and write, Actions read.
3. sbx set up as in the README: signed in to Docker, the balanced network policy, the Claude login, and no global `github` secret.
4. Install coworker and fill `<name>.env` (see the README). Run coworker in its own terminal and keep the log in view:
   ```
   go install ./cmd/coworker
   set -a; source <name>.env; set +a; ~/go/bin/coworker
   ```
5. In the practice repository, install the plugin from your coworker checkout:
   ```
   claude plugin marketplace add <path to coworker> --scope local
   claude plugin install coworker@coworker --scope local
   ```
   After editing the skills, run `claude plugin update coworker@coworker` and restart Claude Code.
6. For the commands below:
   ```
   R=<owner>/<practice repository>
   ```
   `<sandbox>` is a Task's sandbox as `sbx ls` shows it, `coworker-task-<slug>-<task>-builder` (see the README for the slug).

Stopping coworker with Ctrl-C pauses every worker. A run already in a sandbox keeps going, and coworker picks it up when it starts again. Use this to line up the state a scenario needs, then start coworker again.

## Scenarios

### 1. Labels

- Start coworker. `gh label list -R $R` shows the nine labels.
- Stop and start it again. Still nine, and no error in the log.

### 2. Skills

In the practice repository, in Claude Code:

- `/coworker:issue` with a real problem from the project. A `type:problem` issue appears with Problem and Investigation notes, and no solution.
- `/coworker:discuss <problem>`. The problem gets one comment with Direction, Re-checked on, Options considered, Decisions made, Open questions.
- `/coworker:prd <problem>`. Check the plan file before approving: every ledger row is closed, and the draft has every section of the body format. After approval a `type:prd` issue appears as a sub-issue of the problem, with the same body.
- `/coworker:setup`. `.coworker/Dockerfile` and `.coworker/setup.sh` appear, and `.coworker/allowed-hosts` too if a check needed a host. None of them is committed. The report has the files, how long the script took, the checks it ran, each added host with the step that needs it, and the blocked hosts it left out. Afterwards `sbx ls` and `sbx template ls` show nothing named `coworker-setup-`. Commit the files to `main`, together with `.gitignore` if it changed.

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

- While a Task is `status:working`, stop coworker with Ctrl-C. The Task stays `status:working`, and `sbx ls` still shows its sandbox. Start coworker again. Once claude finishes, coworker collects the run and the PR opens.
- Repeat, but kill coworker hard (`kill -9 <pid>`) instead. Same result.

### 11. PR closed by hand

- While a Task is in `status:review`, close its PR on GitHub without merging it.
- reviewer sends the Task to `status:stuck` and says the PR was closed without being merged. Close the Task if the work is not wanted, or relabel it `status:pending` and builder opens a new PR.

### 12. Commit checks

- Add a commit message check to the practice repository that only accepts first lines like `feat: add Shout`, as a `commit-msg` hook in `.githooks`. Turn it on in each Task's sandbox by adding this line to `.coworker/setup.sh`, and commit both to `main`:
  ```
  git config core.hooksPath .githooks
  ```
- File a Task. builder's commit passes the check and the PR opens. The commit follows the format and ends with `Task: #<task>`.
- Make a conflict as in scenario 9. resolver's merge commit passes the check too.
- Remove the line from the script again.

### 13. Claude usage limit

- Start coworker with `COWORKER_FIVE_HOUR_LIMIT=1` and file a Task. The log says once that usage is over the limit, and the Task does not move.
- Restart coworker without it. Within a minute the Task moves on.

### 14. Setup script

- With `.coworker/setup.sh` on `main` (scenario 2), file a Task. The log says the setup script ran in the Task's sandbox before its PR opens. The PR holds none of the files the script created.
- A script that starts a service as a Docker container still passes, and the tests that need the service pass in the sandbox.
- Stop coworker. Push a commit to `main` that adds `echo boom >&2; exit 1` as the first line of the script, then file another Task.
- Start coworker. The Task goes `status:stuck`. The comment says the setup script failed, and `boom` is in the log, not in the comment.
- Push a commit to `main` that removes the line, and relabel the Task `status:pending`. It goes through sequencer and builder again, and the PR opens.

### 15. A Task's sandbox

- File a Task. `sbx ls` shows `<sandbox>`. While claude works, `stream.jsonl` in the sandbox's folder under `~/.coworker/<owner>/<repo>/sandboxes/` grows.
- After the PR opens, the sandbox is gone from `sbx ls`, and its folder is gone too.

### 16. Inside a Task's sandbox

With `.coworker/allowed-hosts` on `main` listing one host, while a Task is `status:working`:

- `sbx policy ls` shows a rule for `<sandbox>` that allows that host.
- `sbx exec <sandbox> env` shows no GitHub token other than sbx's placeholder.
- `sbx exec -w /home/agent/repo <sandbox> git remote -v` prints nothing.
- `sbx exec <sandbox> curl -s https://<listed host>` reaches the host. The same for `ssh.github.com`, or for a host that is not listed, gets a 403 with `Approval required for <host>:443.`

### 17. Sandbox stopped or removed

- While a Task is `status:working`, run `sbx stop <sandbox>`. Within a minute the Task goes back to `status:queued`, and builder starts it again a minute later.
- Again, with `sbx rm --force <sandbox>`. Same result.
- A third time in a row on the same Task. The Task goes `status:stuck`, and the comment says the run failed 3 times in a row. The last run's files are in `~/.coworker/<owner>/<repo>/sandboxes/failed/<sandbox>`.

### 18. Stalled run

- While a Task is `status:working`, freeze claude with `sbx exec <sandbox> pkill -STOP -f claude`. After 15 minutes the sandbox is removed and the Task goes back to `status:queued`.

### 19. A working Task relabeled

- Relabel a `status:working` Task `status:pending`. Its sandbox is removed within a minute.
- Again, relabeling it `status:queued`. Its sandbox is removed within a minute, and builder starts it over.

### 20. Sandbox limit

- Stop coworker and line up three queued Tasks that do not block each other. Start coworker with `COWORKER_SANDBOX_LIMIT=2`. `sbx ls` never shows more than two Task sandboxes, and the third Task starts once one of the first two is done.

### 21. Blocked host

- File a Task that needs a host the sandbox blocks. The AI sends it to `status:stuck`, and the comment names the host from "Approval required for <host>".
- Add the host to `.coworker/allowed-hosts` on `main`, and relabel the Task `status:pending`. The PR opens.

### 22. Global github secret

- Store a global github secret in sbx (`sbx secret set github`), then start coworker. It refuses to start and says to run `sbx secret rm github`.
- Run it. coworker starts.

### 23. Template build

With `.coworker/Dockerfile` on `main` (scenario 2):

- Stop coworker, line up two queued Tasks, and start coworker. The log has exactly one "building template" and one "built template". Afterwards `sbx ls` shows no `coworker-build-` sandbox.
- Push a change to the Dockerfile to `main`, and file a Task. After "removed old template" in the log, `sbx template ls` shows only the new `coworker-<slug>` tag.

### 24. Template problems

- Push a commit to `main` that adds `RUN echo coworker-secret && false` to the Dockerfile, and file a Task. The Task goes `status:stuck`, the comment names the Dockerfile, and `coworker-secret` is only in the log. Revert the commit and relabel the Task `status:pending`. The PR opens.
- Put `**` in `.coworker/allowed-hosts` on `main`, and file a Task. The Task goes `status:stuck`, and the comment quotes that line. Revert it and relabel the Task. The PR opens.
