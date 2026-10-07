# End-to-end check

Unit tests cover exomecha against a fake GitHub, a fake claude and a fake sbx. This checklist runs the real thing on a practice repository: real GitHub, real claude, real sbx, real skills. Run it once after changes to the workers, the prompts, or the skills.

Tests against the real sbx and claude, with no GitHub, run only when asked. Run them after changes to how exomecha uses sbx:

```
EXOMECHA_TEST_SBX=1 go test ./internal/sandbox ./internal/template
EXOMECHA_TEST_SBX=1 EXOMECHA_TEST_CLAUDE=1 go test -run TestReal ./internal/worker
```

The first takes about 2 minutes. The second runs builder, resolver and reviewer, spends a little Claude usage, and takes about 3 minutes. Both remove every sandbox and template they create.

## Setup

1. A practice repository with a small project, its tests, and a CLAUDE.md that says how to install and test. One base branch, `main`, with no "require approval" protection rule, since the token owner cannot approve its own PRs. No GitHub Actions workflow on `main` at the start.
2. A fine-grained token for that repository only: Issues read and write, Pull requests read and write, Contents read and write, Actions read.
3. sbx set up as in the README: signed in to Docker, the balanced network policy, the Claude login, and no global `github` secret. This machine's own claude login, with the same account, is only for the usage check.
4. Install exomecha and fill `<name>.env` (see the README). Run exomecha in its own terminal and keep the log in view:
   ```
   go install ./cmd/exomecha
   set -a; source <name>.env; set +a; ~/go/bin/exomecha
   ```
5. In the practice repository, install the plugin from your exomecha checkout:
   ```
   claude plugin marketplace add <path to exomecha> --scope local
   claude plugin install exomecha@exomecha --scope local
   ```
   After editing the skills, run `claude plugin update exomecha@exomecha` and restart Claude Code.
6. For the commands below:
   ```
   R=<owner>/<practice repository>
   ```
   `<sandbox>` is a PRD's builder sandbox as `sbx ls` shows it, `exomecha-run-<slug>-<prd>-builder` (see the README for the slug). Every worker's sandbox is named the same way, `exomecha-run-<slug>-<prd>-<worker>`.

Stopping exomecha with Ctrl-C pauses every worker. A run already in a sandbox keeps going, and exomecha picks it up when it starts again. Use this to line up the state a scenario needs, then start exomecha again.

reviewer's runs on a small practice project often end in under a minute. To hold one for a check, freeze its claude with `sbx exec <sandbox> pkill -STOP -f claude`. Let it go on with `pkill -CONT` within 15 minutes, before it counts as stalled.

## Scenarios

### 1. Labels

- Start exomecha. `gh label list -R $R` shows the ten labels.
- Stop and start it again. Still ten, and no error in the log.

### 2. Skills

In the practice repository, in Claude Code:

- `/exomecha:draft-prd` with a real problem from the project, then again with a second problem that overlaps the first. Two issues with `prd` and `status:draft` appear, each with Problem, Investigation notes, Ideas and Open questions.
- `/exomecha:summary-all-drafts`. Each draft shows as `#<number> <title>` with a summary of up to three lines. No issue changes.
- `/exomecha:complete-prd <first draft>`. It reports what the latest `main` shows, and asks what to do with the second draft because it overlaps. Check the plan file before approving: the PRD sits under a `# PRD 1: <title>` heading, every ledger row is closed, the Filing row names the draft's number, a tier and the decision for the second draft, and the draft has every section of the body format. After approval the first draft's issue has the text under the heading as its body, `status:ready` in place of `status:draft`, and the tier's label. A second draft that was merged is closed with a comment that links the PRD.
- `/exomecha:complete-prd` with no number, after settling a small change in the conversation. A new issue appears with `prd`, `status:ready` and the tier's label. It has a comment with only an artifact URL, added before `status:ready` in the timeline, and the artifact shows the change in pictures before and after.
- `/exomecha:complete-prd` with a change too big for one PR. The plan proposes several PRDs, each under its own heading. After approval a later PRD that needs an earlier one is Blocked by it. Each issue's timeline shows `status:ready` and the tier label added after the Blocked by.
- `/exomecha:setup`. `.exomecha/Dockerfile` and `.exomecha/setup.sh` appear, and `.exomecha/allowed-hosts` too if a check needed a host. None of them is committed. The report has the files, how long the script took, the checks it ran, each added host with the step that needs it, and the blocked hosts it left out. Afterwards `sbx ls` and `sbx template ls` show nothing named `exomecha-setup-`. Commit the files to `main`, together with `.gitignore` if it changed.

### 3. PRD intake

- A draft filed with `/exomecha:draft-prd` keeps `status:draft`, and a PRD filed by hand with only `prd` keeps no status. No sandbox shows in `sbx ls` for them.
- A PRD labeled both `status:draft` and `status:ready` keeps both, and builder does not start it.
- A PRD filed by hand with `prd`, `status:ready` and no tier label goes `status:working`, and builder's "started claude in a sandbox" line shows `tier:normal`.
- Of the PRDs from the split in scenario 2, the one that is Blocked by another stays `status:ready` until that one closes, after you merge its PR.
- A PRD that already has another status, such as `status:stuck`, keeps it.

### 4. Parallel builds and order

- Stop exomecha. File two PRDs that touch different files, and a third that is Blocked by the first.
- Start exomecha. The first two PRDs are `status:working` at the same time and each gets its own PR.
- The third PRD stays `status:ready` until you merge the first one's PR and the first one closes.

### 5. Review, approval, and merge

- With no CI, reviewer waits five minutes after the PR's last commit, then reviews.
- Within a minute after that, `sbx ls` shows `exomecha-run-<slug>-<prd>-reviewer`. Within a minute after its claude ends, the sandbox is gone and the review or the approval follows.
- On approve: the PR gets a COMMENT review from exomecha that starts with `exomecha's reviewer approved this PR.` and summarizes the PR, and the PRD goes `status:approved`. The PR is not merged, and no reviewer sandbox for the PRD shows in `sbx ls` again.
- Merge the PR on GitHub. The PRD closes as completed. When the PR targets the repository's default branch, GitHub closes it at the merge and `status:approved` stays on the closed PRD. Otherwise reviewer closes it within a minute and removes the status.

### 6. Sent back by review

- While a PRD is in `status:review`, stop exomecha and push an obviously wrong commit to its branch (for example, undo the fix).
- Start exomecha. reviewer leaves a COMMENT review and the PRD goes `status:ready`. builder pushes a fix to the same PR. No second PR appears.

### 7. CI failure

- While a PRD is in `status:review`, stop exomecha and push to its branch a workflow that runs the tests, together with a test that fails:
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
- Start exomecha. reviewer waits while the run is in progress, then leaves a review listing the failed run, and the PRD goes `status:ready`. builder fixes it on the same PR.

### 8. stuck and back

- Create a PRD that cannot be done without a missing decision:
  ```
  gh issue create -R $R --title "Print the official greeting" \
    --body "## To do
  Print the team's official greeting when the CLI runs without arguments." \
    --label prd --label status:ready
  ```
- builder comments why it is blocked and what needs deciding, and the PRD goes `status:stuck`.
- Answer in a comment, then relabel it:
  ```
  gh issue edit <prd> -R $R --remove-label status:stuck --add-label status:ready
  ```
- builder takes it again and uses the answer.

### 9. Conflict

- When a PRD reaches `status:review`, stop exomecha and push a commit to `main` that changes the same lines its PR changes.
- Start exomecha. Once GitHub marks the PR as conflicting, resolver merges `main` into the branch, resolves it, and pushes. The PRD stays `status:review`, and reviewer reviews it again. reviewer never picks the PRD while its PR is conflicting, so `sbx ls` shows no `exomecha-run-<slug>-<prd>-reviewer` until resolver has pushed.
- Repeat, but push the commit to `main` while the PRD's reviewer sandbox shows in `sbx ls`. The PRD does not go `status:approved`. Within a minute of GitHub marking the PR as conflicting, the reviewer sandbox and its folder are gone, and resolver takes the PRD as above.
- Repeat with a commit on `main` that contradicts the PR (for example, it explicitly refuses the feature the PR adds). resolver sends the PRD to `status:stuck` and asks which side wins. Answer in a comment and relabel it `status:review`. resolver merges again and settles the conflict the way you answered. builder is not involved.

### 10. Restart

- While a PRD is `status:working`, stop exomecha with Ctrl-C. The PRD stays `status:working`, and `sbx ls` still shows its sandbox. Start exomecha again. Once claude finishes, exomecha collects the run and the PR opens.
- Repeat, but kill exomecha hard (`kill -9 <pid>`) instead. Same result.
- Stop exomecha while a PRD's reviewer sandbox shows in `sbx ls`. Start it again. exomecha collects the run without starting claude again: the review or the approval follows. This holds when claude ended while exomecha was stopped too, since the run's folder keeps the result.

### 11. PR closed by hand

- While a PRD is in `status:review`, close its PR on GitHub without merging it. Repeat with a PRD in `status:approved`.
- reviewer sends the PRD to `status:stuck` and says the PR was closed without being merged. Close the PRD if the work is not wanted, or relabel it `status:ready` and builder opens a new PR.

### 12. Commit checks

- Add a commit message check to the practice repository that only accepts first lines like `feat: add Shout`, as a `commit-msg` hook in `.githooks`. Turn it on in each PRD's sandbox by adding this line to `.exomecha/setup.sh`, and commit both to `main`:
  ```
  git config core.hooksPath .githooks
  ```
- File a PRD. builder's commit passes the check and the PR opens. The commit follows the format and ends with `PRD: #<prd>`.
- Make a conflict as in scenario 9. resolver's merge commit passes the check too.
- Remove the line from the script again.

### 13. Claude usage limit

- Stop exomecha while a PRD's reviewer sandbox shows in `sbx ls`. Line up a `status:ready` PRD with no open blockers and a PRD in `status:review` whose CI failed (as in scenario 7).
- Start exomecha with `EXOMECHA_FIVE_HOUR_LIMIT=1`. The log says once that usage is over the limit. No new sandbox shows in `sbx ls`, and the ready PRD stays `status:ready`.
- The running review is still collected: the PRD goes `status:approved`, or `status:ready` with a review.
- The PRD whose CI failed still gets a review listing the failed run and goes `status:ready`, since that needs no claude.
- Restart exomecha without the setting. Within a minute the ready PRDs' builder sandboxes show in `sbx ls`.

### 14. Setup script

- With `.exomecha/setup.sh` on `main` (scenario 2), file a PRD. The log says the setup script ran in the PRD's sandbox before its PR opens. The PR holds none of the files the script created.
- A script that starts a service as a Docker container still passes, and the tests that need the service pass in the sandbox.
- Stop exomecha. Push a commit to `main` that adds `echo boom >&2; exit 1` as the first line of the script, then file another PRD.
- Start exomecha. The PRD goes `status:stuck`. The comment says the setup script failed, and `boom` is in the log, not in the comment.
- Push a commit to `main` that removes the line, and relabel the PRD `status:ready`. builder takes it again, and the PR opens.

### 15. Every worker's sandbox

- File a PRD. `sbx ls` shows `<sandbox>`. While claude works, `stream.jsonl` in the sandbox's folder under `~/.exomecha/<owner>/<repo>/sandboxes/` grows.
- After the PR opens, the sandbox is gone from `sbx ls`, and its folder is gone too.
- The PR's commits have the token owner as author, with the address `<id>+<login>@users.noreply.github.com`.
- Follow one PRD from filing to approval, and a conflict as in scenario 9. Each of the three workers shows its own sandbox in `sbx ls` while its claude works, and the sandbox's folder has a growing `stream.jsonl` too. builder's and resolver's sandboxes go away once their result is pushed. reviewer's go away within a minute after claude ends, even when exomecha cannot collect the result yet, and their folders once it does.
- Afterwards `sbx ls` shows no sandbox of the project, and `~/.exomecha/<owner>/<repo>/sandboxes/` holds only `failed`, if a run failed.

### 16. Inside a PRD's sandbox

With `.exomecha/allowed-hosts` on `main` listing one host, while a PRD is `status:working`:

- `sbx policy ls` shows a rule for `<sandbox>` that allows that host.
- `sbx exec <sandbox> env` shows no GitHub token other than sbx's placeholder.
- `sbx exec -w /home/agent/repo <sandbox> git remote -v` prints nothing.
- `sbx exec -w /home/agent/repo <sandbox> git config user.email` prints the token owner's `<id>+<login>@users.noreply.github.com`.
- `sbx exec <sandbox> curl -s https://<listed host>` reaches the host. The same for a host that is not listed gets a 403 with `Approval required for <host>:443.`, and for `ssh.github.com` one with `Blocked by local rule for ssh.github.com:443`.

### 17. Sandbox stopped or removed

- While a PRD is `status:working`, run `sbx stop <sandbox>`. Within a minute the PRD goes `status:stuck`, and the comment says the sandbox stopped while claude was running. The run's files are in `~/.exomecha/<owner>/<repo>/sandboxes/failed/<sandbox>`.
- Relabel the PRD `status:ready`. Once builder starts it again, run `sbx rm --force <sandbox>`. The PRD goes `status:stuck` the same way, and the comment says the sandbox was removed.
- While a PRD's reviewer sandbox shows in `sbx ls`, stop it with `sbx stop`. Within a minute the PRD goes `status:stuck`, the comment says to relabel it `status:review`, and the run's files are in `failed/`. Relabel it. reviewer reviews the PR again in a new sandbox.

### 18. Stalled run

- While a PRD is `status:working`, freeze claude with `sbx exec <sandbox> pkill -STOP -f claude`. After 15 minutes the sandbox is removed, and the PRD goes `status:stuck` with a comment that claude's output did not change for 15 minutes. The run's files are in `failed/`.

### 19. A working PRD relabeled

- Relabel a `status:working` PRD `status:ready`. Its sandbox is removed within a minute, and builder starts it over.

### 20. Sandbox limit

- With `.exomecha/Dockerfile` on `main`, stop exomecha and remove the project's template (`sbx template rm --force`), so exomecha starts with no template and builds it first. Line up three ready PRDs that do not block each other.
- Start exomecha with `EXOMECHA_SANDBOX_LIMIT=2`. `sbx ls` never shows more than two PRD sandboxes, and no more than two PRDs are `status:working` at once, during the build too. The third PRD starts once one of the first two is done.
- reviewer's sandboxes that show at the same time do not count toward the two. They have a limit of their own (scenario 26).

### 21. Blocked host

- File a PRD that needs a host the sandbox blocks. The AI sends it to `status:stuck`, and the comment names the host from "Approval required for <host>".
- Add the host to `.exomecha/allowed-hosts` on `main`, and relabel the PRD `status:ready`. The PR opens.

### 22. Global github secret

- Store a global github secret in sbx (`sbx secret set github`), then start exomecha. It refuses to start and says to run `sbx secret rm github --force`.
- Run it. exomecha starts.

### 23. Template build

With `.exomecha/Dockerfile` on `main` (scenario 2):

- Stop exomecha, line up two ready PRDs, and start exomecha. The log has exactly one "building template" and one "built template". Afterwards `sbx ls` shows no `exomecha-build-` sandbox.
- Push a change to the Dockerfile to `main`, and file a PRD. After "removed old template" in the log, `sbx template ls` shows only the new `exomecha-<slug>` tag.

### 24. Template problems

- Push a commit to `main` that adds `RUN echo exomecha-secret && false` to the Dockerfile, and file a PRD. The PRD goes `status:stuck`, the comment names the Dockerfile, and `exomecha-secret` is only in the log. Revert the commit and relabel the PRD `status:ready`. The PR opens.
- Put `**` in `.exomecha/allowed-hosts` on `main`, and file a PRD. The PRD goes `status:stuck`, and the comment quotes that line. Revert it and relabel the PRD. The PR opens.

### 25. Reader sandboxes

With `.exomecha/Dockerfile`, `.exomecha/setup.sh` and `.exomecha/allowed-hosts` on `main` (scenario 2), hold a PRD's reviewer sandbox. Call it `<reader>`.

- The log's "started claude in a sandbox" line for `<reader>` has an empty template, so it came from sbx's default claude template.
- Its folder under `~/.exomecha/<owner>/<repo>/sandboxes/` has no `setup.sh`, and the log has no "ran setup script" line for it. `sbx exec -w /home/agent/repo <reader> git status --short --ignored` lists none of the files the setup script creates.
- `sbx exec -w /home/agent/repo <reader> git config user.email` prints nothing, and `git remote -v` prints nothing.
- `sbx exec <reader> env` shows no GitHub token other than sbx's placeholder.
- `sbx policy ls` has no rule for `<reader>` that allows the listed host. `sbx exec <reader> curl -s https://<listed host>` gets a 403 with `Approval required for <host>:443.`, and `ssh.github.com` one with `Blocked by local rule for ssh.github.com:443`.

### 26. Reader limit

- Stop exomecha. Line up three PRDs in `status:review` with mergeable PRs whose last commits are more than five minutes old, and one ready PRD.
- Start exomecha with `EXOMECHA_READ_SANDBOX_LIMIT=1`. `sbx ls` never shows more than one reviewer sandbox at once. The reviews run one after another.
- The ready PRD's builder sandbox shows next to the reader sandbox, since `EXOMECHA_SANDBOX_LIMIT` counts it apart.

### 27. Push during review

- Hold a PRD's reviewer sandbox. Push a commit to the PRD's branch that keeps the PR correct, for example one that adds a line to its README. Then let claude go on.
- The PR gets no review from that run, and the PRD stays `status:review`. Five minutes after the new commit (the wait for CI), the log says the run starts over because the PR branch changed during the review, and its folder is gone.
- Within a minute a new reviewer sandbox shows, and its review covers the commit you pushed.

### 28. Approved PR conflicts

- Let a PRD reach `status:approved`. Push a commit to `main` that changes the same lines its PR changes.
- Once GitHub marks the PR as conflicting, resolver moves the PRD to `status:review` within a minute. A minute later resolver merges `main` into the branch as in scenario 9. reviewer reviews it again, and the PRD goes `status:approved` with a new approval on the PR.
- A PRD in `status:approved` whose PR does not conflict keeps its status, and `sbx ls` shows no sandbox for it.

### 29. Model tiers

- Start exomecha with `EXOMECHA_TIER_LIGHT=sonnet:low`. The "exomecha started" line in the log shows the three tiers.
- When resolver works on a conflict (as in scenario 9), its "started claude in a sandbox" line shows tier `tier:light`, model `sonnet` and effort `low`. The init line in the run's `stream.jsonl` names a sonnet model.
- While builder works on a PRD, `sbx exec <sandbox> ps -eo args` shows claude with the `--model` and `--effort` of the PRD's tier.
- Give a ready PRD both `tier:light` and `tier:heavy`. Its builder's "started claude in a sandbox" line shows `tier:heavy`.
- Start exomecha with `EXOMECHA_TIER_HEAVY=opus`. It refuses to start and names `EXOMECHA_TIER_HEAVY`.
- Start exomecha with `EXOMECHA_TIER_LIGHT=haiku:low`. It refuses to start and says `EXOMECHA_TIER_LIGHT` and that claude does not run haiku in auto permission mode. With `EXOMECHA_TIER_LIGHT=opsu:low` it refuses too, with claude's message about the model.

### 30. Sent back by a person

- Let a PRD reach `status:approved`. On its PR, leave a review with "Comment" that asks for a change, and a comment on one line of the diff. Relabel the PRD `status:ready`.
- builder takes it again. The run's `input.md` has both under "Comments on the PR", the line comment with its file and line, and no "Last review". builder pushes to the same PR. No second PR appears.
- reviewer's next run has the same comments in its `input.md`, and the PRD goes `status:approved` again.
- Comments on the PR from an account that cannot write to the repository are in neither input.
