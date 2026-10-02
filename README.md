# exomecha

exomecha watches the labels on a repository's GitHub issues. Five workers split each PRD into Tasks, put the Tasks in order, build them, review and merge the PRs, and resolve merge conflicts. builder and resolver run claude in a Docker Sandboxes (sbx) sandbox of its own for each Task. A person only makes the decisions up front (three skills) and answers when a Task is blocked.

```
[person] /exomecha:issue   → type:problem issue (the problem only)
[person] /exomecha:discuss → settle a direction by talking, then leave it as a comment on the problem
[person] /exomecha:prd     → type:prd issue (all decisions, a decision ledger, acceptance criteria, test scenarios), sub-issue of the problem

[splitter]  PRD without tasked → create Tasks (status:pending), add tasked to the PRD
[sequencer] pending Task → Blocked by any overlapping Task from another PRD → status:queued
[builder]   queued, all blockers closed → working → code in a sandbox and PR → status:review
[reviewer]  review → check CI → AI review → queued if something needs fixing, otherwise merge → Task closed
[resolver]  review with a conflicting PR → merge the base branch and resolve in a sandbox → stays in review (stuck if it can't)
Any worker blocked by the Task itself → status:stuck + a comment with the reason → a person closes it, or answers and sets pending
```

## Setup

1. Create a fine-grained token with access to the target repository only (Repository access → Only select repositories). It needs:
   - Issues: read and write
   - Pull requests: read and write
   - Contents: read and write (push and merge)
   - Actions: read (CI results)
2. Install the plugin on the machine that runs exomecha. In Claude Code:
   ```
   /plugin marketplace add LeeShinYeoung/exomecha
   /plugin install exomecha@exomecha
   ```
3. Set up sbx on the same machine. Docker Desktop is not needed.
   - Install sbx and sign in to Docker: `sbx login`.
   - Start from the balanced network policy: `sbx policy init balanced`. A sandbox reaches only the hosts that policy allows, plus the ones the project lists in `.exomecha/allowed-hosts`.
   - Sign in to Claude once from inside a sandbox. sbx keeps the login on this machine, and every sandbox gets a placeholder instead of the real token. Use the same Claude account as this machine's claude login, because the usage limits in step 5 are read from that login.
     ```
     mkdir -p /tmp/sbx-login
     sbx run -d --name sbx-login claude /tmp/sbx-login
     sbx exec -it sbx-login claude auth login
     sbx rm --force sbx-login
     ```
     `sbx secret ls` then lists `anthropic` as `(oauth configured)`. A token from `claude setup-token` does not work, because sbx does not pass a credential it did not issue on to Anthropic. An Anthropic API key works (`sbx secret set anthropic`), but the usage limits in step 5 do not count what it spends.
   - Do not keep a global `github` secret in sbx. Every sandbox could use it, so the AI could push past exomecha's checks. exomecha refuses to start while one exists. Remove it with `sbx secret rm github --force`.
4. Prepare the target project:
   - Write how to install dependencies and run the tests in its CLAUDE.md. builder follows it to run the tests it can run in its sandbox.
   - Run `/exomecha:setup` in the project and commit what it writes to the base branch: `.exomecha/Dockerfile`, `.exomecha/setup.sh`, `.exomecha/allowed-hosts` (only when a check needs a host the sandbox policy blocks), and `.gitignore` if it changed. The skill proves the files in a real sandbox before it hands them over. With them, a Task's sandbox already has the tools, dependencies, ignored config files and services the project needs when the AI starts. Without them, the sandbox starts from sbx's default claude template with only the code, and the AI installs what it needs on its own, following CLAUDE.md.
5. Install and run exomecha:
   - Install from this repository: `go install ./cmd/exomecha` (or `go build -o exomecha ./cmd/exomecha`)
   - Copy `.env.example` to `<project>.env`, fill in the three environment variables, and add the `.env` file to `.gitignore`.
     ```
     EXOMECHA_REPO=owner/name
     EXOMECHA_BASE_BRANCH=main
     EXOMECHA_GITHUB_TOKEN=github_pat_...
     ```
   - Optional: `EXOMECHA_FIVE_HOUR_LIMIT` (default 90) and `EXOMECHA_WEEKLY_LIMIT` (default 80), in percent of the Claude plan's 5-hour and weekly limits. exomecha starts no new work at or above them.
   - Optional: `EXOMECHA_SANDBOX_LIMIT` (default 2), how many Task sandboxes builder and resolver may have at once, together.
   - Run:
     ```
     set -a; source <project>.env; set +a; exomecha
     ```
   - exomecha creates any of the nine labels that are missing when it starts.

## Notes

- splitter, sequencer, reviewer and the usage check use the claude login on this machine. builder and resolver run claude in sandboxes, signed in with the login sbx keeps (Setup step 3).
- When there is work to start, exomecha first reads the Claude plan's usage with a small claude call, at most once every three minutes. At or above the limits in Setup it starts nothing new, and checks again only once the limit resets. Work already running finishes, so usage can end up a little over.
- exomecha sends a Task's code into its sandbox and takes the AI's commits out with git, then checks them and pushes them from this machine. The GitHub token never enters a sandbox. Task sandboxes cannot reach `ssh.github.com`, because sbx hands this machine's SSH agent to every sandbox.
- A run keeps going in its sandbox while exomecha is stopped. Ctrl-C leaves working Tasks as they are, and exomecha picks up their runs when it starts again. claude's output streams to `stream.jsonl` in the run's folder under `~/.exomecha/<owner>/<repo>/sandboxes/`.
- If the base branch has `.exomecha/Dockerfile`, exomecha builds it into an sbx template the first time a Task needs a sandbox after the file changed. The build runs in a sandbox of its own and takes a few minutes (about 2 to 3 for a Dockerfile that installs Playwright browsers), and the first sandbox from a new template takes about 25 seconds more to start. Tasks that need a sandbox during a build wait for it. Once the new template is in, exomecha removes the project's older ones.
- Only a change to the Dockerfile rebuilds the template. To pick up a newer base image or claude, remove the template with `sbx template rm --force`, and the next Task builds it again.
- `.exomecha/allowed-hosts` lists the hosts that Task sandboxes and the template build may reach beyond sbx's network policy, one per line. `**` (every host) is rejected. Other sandboxes on this machine are not affected.
- If the base branch has `.exomecha/setup.sh`, exomecha runs it with `bash -eo pipefail` in the Task's sandbox, at the root of the code (`/home/agent/repo`), before builder or resolver starts the AI, with the Task number in `EXOMECHA_TASK`. reviewer, splitter and sequencer only read the code and do not run it. The script may leave services running as Docker containers started with `--restart unless-stopped`, which come back when a stopped sandbox starts again. It must end within 10 minutes, must leave nothing git can see, and must not commit. exomecha does not pass it the GitHub token.
- exomecha always runs the base branch's copy of the script, even for a PR's branch, so a fix to the script reaches every open Task. A branch older than the script can still fail it, for example when the branch lacks a `.gitignore` entry the script relies on. Such a branch needs the base branch merged into it.
- exomecha's own git commands (clone, fetch, push, and moving code in and out of sandboxes) run none of the project's git hooks. The AI commits in a repository exomecha creates inside the sandbox, so the project's git hooks run there only if setup turns them on. A repository that uses Git LFS is not supported.
- The AI in builder and resolver makes the commits, following the project's commit message rules and commit checks (in CLAUDE.md, or enforced by a check). Without rules, a commit looks like `feat: add greeting` with `Task: #12` as its last line.
- Those commits have the token owner as author: the GitHub profile name (or the login when it has none) and the address `<id>+<login>@users.noreply.github.com`. exomecha sets them in the sandbox's repository before `.exomecha/setup.sh` runs, so the script can change them.
- The token owner is both the PR author and the reviewer, so a "require approval" protection rule on the base branch blocks the automatic merge.
- exomecha only acts on issues, comments and reviews written by the repository's owners, members and collaborators. On a public repository, anything an outsider writes is ignored. GitHub may report a member who keeps their organization membership private as an outsider, so their comments can be left out.
- exomecha checks GitHub every minute. A check whose answer has not changed uses a conditional request, which GitHub does not count against the API rate limit. If the limit still runs out, exomecha waits until it resets.
- GitHub links a PR to its Task (and closes the Task on merge) only when the PR targets the repository's default branch. With another base branch the link does not show, and exomecha still closes the Task itself.

## When a Task is blocked (status:stuck)

The worker's comment says why it is blocked and what needs deciding.

- If there is nothing left to do, close the issue.
- Otherwise, answer in a comment and relabel it the way the comment says. That sends it back to the worker that owns the problem:
  - `status:pending` when builder got stuck. It goes through sequencer and builder again, and builder continues on the existing PR branch.
  - `status:review` when resolver got stuck on a conflict. resolver merges the base branch again and settles the conflict the way you answered.
- When the AI says the sandbox blocked a host, add that host to `.exomecha/allowed-hosts` on the base branch if the Task really needs it. Then relabel the Task the way the comment says.
- When `.exomecha/setup.sh` failed, there is nothing to answer. The script's output is in exomecha's log. Fix the script on the base branch, or merge the base branch into the Task's branch if the comment says the branch is behind. Then relabel the Task the way the comment says.
- When `.exomecha/Dockerfile` failed to build or ran over 30 minutes, or `.exomecha/allowed-hosts` has a bad line, there is nothing to answer either. The build output is only in exomecha's log. Fix the file on the base branch, then relabel the Task the way the comment says.
- When resolver's merge of the base branch failed without a conflict (for example, a git setting from `.exomecha/setup.sh` refused it), there is nothing to answer either. git's output is only in exomecha's log. Fix what it reports, then relabel the Task the way the comment says.
- When the AI's run failed 3 times in a row (the sandbox stopped or went away, claude's output did not change for 15 minutes, or claude ended without a usable result), the Task goes to stuck with the last reason. The details are in exomecha's log, and the last failed run's files are in `~/.exomecha/<owner>/<repo>/sandboxes/failed/`. A run that ended at the Claude usage limit does not count, and neither does a failure in exomecha's own steps (sbx, git, GitHub) other than that merge. Those wait and try again.

## Removing a project

Stop exomecha, then remove what it keeps on this machine for the project:

- its sandboxes, named `exomecha-task-<slug>-...` and `exomecha-build-<slug>`, with `sbx rm --force`
- its templates, `exomecha-<slug>` with any tag, with `sbx template rm --force`
- `~/.exomecha/<owner>/<repo>`

`<slug>` is `owner-repo` in lowercase, with every run of characters other than letters and digits turned into one `-`. A slug longer than 32 characters is cut and ends with a short hash of the repository's name. `sbx ls` and `sbx template ls` show the exact names.
