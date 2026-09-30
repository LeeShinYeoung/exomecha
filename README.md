# coworker

coworker watches the labels on a repository's GitHub issues. Five workers split each PRD into Tasks, put the Tasks in order, build them, review and merge the PRs, and resolve merge conflicts. A person only makes the decisions up front (three skills) and answers when a Task is blocked.

```
[person] /coworker:issue   → type:problem issue (the problem only)
[person] /coworker:discuss → settle a direction by talking, then leave it as a comment on the problem
[person] /coworker:prd     → type:prd issue (all decisions, a decision ledger, acceptance criteria, test scenarios), sub-issue of the problem

[splitter]  PRD without tasked → create Tasks (status:pending), add tasked to the PRD
[sequencer] pending Task → Blocked by any overlapping Task from another PRD → status:queued
[builder]   queued, all blockers closed → working → code and PR → status:review
[reviewer]  review → check CI → AI review → queued if something needs fixing, otherwise merge → Task closed
[resolver]  review with a conflicting PR → merge the base branch and resolve → stays in review (stuck if it can't)
Any worker blocked by the Task itself → status:stuck + a comment with the reason → a person closes it, or answers and sets pending
```

## Setup

1. Create a fine-grained token with access to the target repository only (Repository access → Only select repositories). It needs:
   - Issues: read and write
   - Pull requests: read and write
   - Contents: read and write (push and merge)
   - Actions: read (CI results)
2. Install the plugin on the machine that runs coworker. In Claude Code:
   ```
   /plugin marketplace add LeeShinYeoung/coworker
   /plugin install coworker@coworker
   ```
3. Prepare the target project:
   - Write how to install dependencies and run the tests in its CLAUDE.md. builder follows it to run the tests it can run in its working copy.
   - Run `/coworker:setup` in the project and commit what it writes to the base branch: `.coworker/setup.sh`, and `.gitignore` if it changed. coworker runs that script in a fresh working copy before builder or resolver starts the AI, so dependencies and ignored config files are already in place. Without the script, the AI installs on its own, following CLAUDE.md.
4. Install and run coworker:
   - Install from this repository: `go install ./cmd/coworker` (or `go build -o coworker ./cmd/coworker`)
   - Copy `.env.example` to `<project>.env`, fill in the three environment variables, and add the `.env` file to `.gitignore`.
     ```
     COWORKER_REPO=owner/name
     COWORKER_BASE_BRANCH=main
     COWORKER_GITHUB_TOKEN=github_pat_...
     ```
   - Optional: `COWORKER_FIVE_HOUR_LIMIT` (default 90) and `COWORKER_WEEKLY_LIMIT` (default 80), in percent of the Claude plan's 5-hour and weekly limits. coworker starts no new work at or above them.
   - Run:
     ```
     set -a; source <project>.env; set +a; coworker
     ```
   - coworker creates any of the nine labels that are missing when it starts.

## Notes

- coworker uses the claude login already on this machine.
- When there is work to start, coworker first reads the Claude plan's usage with a small claude call, at most once every three minutes. At or above the limits in Setup it starts nothing new, and checks again only once the limit resets. Work already running finishes, so usage can end up a little over.
- If the base branch has `.coworker/setup.sh`, coworker runs it with `bash -eo pipefail` at the root of a fresh working copy before builder or resolver starts the AI, with the Task number in `COWORKER_TASK`. reviewer, splitter and sequencer only read the code and do not run it. The script must end within 10 minutes, must leave nothing git can see, and must not commit. coworker does not pass it the GitHub token.
- coworker always runs the base branch's copy of the script, even in a PR's working copy, so a fix to the script reaches every open Task. A branch older than the script can still fail it, for example when the branch lacks a `.gitignore` entry the script relies on. Such a branch needs the base branch merged into it.
- coworker's own git commands (clone, fetch, push) carry the token, so they run none of the project's git hooks. The AI's git commands run the hooks as usual. A repository that uses Git LFS is not supported, because LFS uploads its files from a pre-push hook.
- The AI in builder and resolver makes the commits, following the project's commit message rules and commit checks (in CLAUDE.md, or enforced by a check). Without rules, a commit looks like `feat: add greeting` with `Task: #12` as its last line.
- The token owner is both the PR author and the reviewer, so a "require approval" protection rule on the base branch blocks the automatic merge.
- coworker only acts on issues, comments and reviews written by the repository's owners, members and collaborators. On a public repository, anything an outsider writes is ignored. GitHub may report a member who keeps their organization membership private as an outsider, so their comments can be left out.
- coworker checks GitHub every minute. A check whose answer has not changed uses a conditional request, which GitHub does not count against the API rate limit. If the limit still runs out, coworker waits until it resets.
- GitHub links a PR to its Task (and closes the Task on merge) only when the PR targets the repository's default branch. With another base branch the link does not show, and coworker still closes the Task itself.

## When a Task is blocked (status:stuck)

The worker's comment says why it is blocked and what needs deciding.

- If there is nothing left to do, close the issue.
- Otherwise, answer in a comment and relabel it the way the comment says. That sends it back to the worker that owns the problem:
  - `status:pending` when builder got stuck. It goes through sequencer and builder again, and builder continues on the existing PR branch.
  - `status:review` when resolver got stuck on a conflict. resolver merges the base branch again and settles the conflict the way you answered.
- When `.coworker/setup.sh` failed, there is nothing to answer. The script's output is in coworker's log. Fix the script on the base branch, or merge the base branch into the Task's branch if the comment says the branch is behind. Then relabel the Task the way the comment says.
