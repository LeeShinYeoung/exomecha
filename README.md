# exomecha

exomecha watches the labels on the GitHub issues of one repository, the PRD repository. Each PRD lists the repositories it changes, one or several, and gets one PR in each. Three workers build each PRD, review its PRs, and resolve merge conflicts. Every worker runs claude in a Docker Sandboxes (sbx) sandbox of its own for each run, and only builder's and resolver's get the project's environment. A person makes the decisions up front (three skills), merges each PR the AI approved, and answers when a PRD is blocked.

```
[person] /exomecha:draft-prd          → a draft: an issue labeled prd and status:draft (the problem, what is known, ideas not yet decided), which no worker takes
[person] /exomecha:summary-all-drafts → each open draft's title and a summary of up to three lines, to pick what to settle next
[person] /exomecha:complete-prd       → PRDs of one PR each (all decisions, a decision ledger, acceptance criteria, test scenarios, a tier label), from the conversation or from drafts, each Blocked by any earlier one it needs → status:ready

[builder]   ready, all blockers closed → working → code in a sandbox and a PR in each repository it changed → status:review. A PRD with no status or status:draft is left alone
[reviewer]  review → check CI → AI review of all the PRD's PRs together → ready if something needs fixing (stuck after 5 send-backs in a row), otherwise an approval on each PR → status:approved
[person]    approved → merge the PRs → PRD closed once all are merged. Or ask for changes on a PR or the PRD → status:ready
[resolver]  review with a conflicting PR → merge the base branch and resolve in a sandbox → stays in review (stuck if it can't). approved with a conflicting PR → back to review first
Any worker blocked by the PRD itself → status:stuck + a comment with the reason → a person closes it, or answers and sets ready
```

## Setup

1. Create a fine-grained token with access to the PRD repository and every repository PRDs may change, and no other (Repository access → Only select repositories). A fine-grained token covers repositories of one owner only. It needs:
   - Issues: read and write
   - Pull requests: read and write
   - Contents: read and write (push)
   - Actions: read (CI results)
2. Install the plugin on the machine that runs exomecha. In Claude Code:
   ```
   /plugin marketplace add LeeShinYeoung/exomecha
   /plugin install exomecha@exomecha
   ```
3. Set up sbx on the same machine. Docker Desktop is not needed.
   - Install sbx and sign in to Docker: `sbx login`.
   - Start from the balanced network policy: `sbx policy init balanced`. A sandbox reaches only the hosts that policy allows, plus the ones the project lists in `.exomecha/allowed-hosts`.
   - Use the same Claude account for this machine's claude login and for sbx. exomecha uses this machine's login only to read the usage limits in step 5, and every worker runs claude with the login sbx keeps. When sbx has no Claude login, `exomecha init` in step 5 starts a sandbox, runs `claude auth login` in it on your terminal, and removes the sandbox. sbx keeps the login on this machine, and every sandbox gets a placeholder instead of the real token. A token from `claude setup-token` does not work, because sbx does not pass a credential it did not issue on to Anthropic. An Anthropic API key works (`sbx secret set anthropic`), but the usage limits in step 5 do not count what it spends.
   - Do not keep a global `github` secret in sbx. Every sandbox could use it, so the AI could push past exomecha's checks. exomecha refuses to start while one exists. Remove it with `sbx secret rm github --force`.
4. Prepare the projects:
   - Write how to install dependencies and run the tests in each repository's CLAUDE.md. builder follows it to run the tests it can run in its sandbox.
   - Run `/exomecha:setup` in the PRD repository and commit what it writes to its base branch: `.exomecha/Dockerfile`, `.exomecha/setup.sh`, `.exomecha/allowed-hosts` (only when a check needs a host the sandbox policy blocks), `.exomecha/sandbox.md` (only when you chose how the sandbox handles a check it cannot run as CLAUDE.md says), and `.gitignore` if it changed. One set of files prepares the sandbox for every repository. With a single repository that also holds the PRDs, run it in that repository. The skill proves the files in a real sandbox before it hands them over. With them, builder's and resolver's sandboxes already have the tools, dependencies, ignored config files and services the project needs when the AI starts. Without them, those sandboxes start from sbx's default claude template with only the code, and the AI installs what it needs on its own, following CLAUDE.md.
5. Install and run exomecha:
   - Install exomecha on the machine that runs it. Update it the same way.
     - On a Mac, with Homebrew. Update with `brew upgrade exomecha`.
       ```
       brew install leeshinyeoung/tap/exomecha
       ```
     - On Linux, from the latest release. On an ARM machine, replace `amd64` with `arm64`. Run the same commands again to update.
       ```
       curl -fsSLO https://github.com/LeeShinYeoung/exomecha/releases/latest/download/exomecha_linux_amd64.tar.gz
       tar -xzf exomecha_linux_amd64.tar.gz
       sudo install exomecha /usr/local/bin/
       ```
     - From the source, with Go 1.27 or later. The command lands in `$(go env GOPATH)/bin`, which must be on your `PATH`. Pull and run `go install` again to update.
       ```
       git clone https://github.com/LeeShinYeoung/exomecha.git
       cd exomecha
       go install ./cmd/exomecha
       ```
   - Run `exomecha init <project>`, where `<project>` is a name of your choice made of letters, digits, `-` and `_`. It asks for the repositories PRDs may change, the PRD repository when there are several, the base branch and the GitHub token, which it does not show as you type. It writes them to `~/.exomecha/<project>.yaml`, which only you can read, and lists the optional settings in the file as comments with their defaults. Then it signs sbx in to Claude when sbx has no login (step 3) and checks this machine. Run it again at any time to sign in again and check, without the questions. To change a setting, edit the file.
     ```yaml
     repos:
       - acme/api
       - acme/web
     prd_repo: acme/plans
     base_branch: main
     github_token: github_pat_...
     ```
   - `repos` lists the repositories PRDs may change, such as `acme/api` and `acme/web`. Their names after the owner must differ, since a sandbox gives each a folder named after it. Every repository uses `base_branch`.
   - `prd_repo` is the repository whose issues are the PRDs, such as `acme/plans`. It holds the labels and the files from `/exomecha:setup`, and it may also be one of `repos`. With one repository in `repos` it is optional and defaults to that repository. With several, exomecha does not start without it. Choose a repository whose writers may all change the code in `repos`, since exomecha acts on what they write, and keep it private when the code is.
   - Optional: `secrets`, values that builder's and resolver's sandboxes need for requests to some hosts, such as a read-only token for a private package registry. Each key is the name of the environment variable that holds the placeholder in the sandbox, with `hosts`, the hosts that get the value, and either `value`, the value itself, or `command`, a command on this machine that prints it:
     ```yaml
     secrets:
       NPM_TOKEN:
         hosts: [npm.pkg.github.com]
         value: npm_...
       STRIPE_SECRET_KEY:
         hosts: [api.stripe.com]
         command: security find-generic-password -s stripe-test-key -w
     ```
     `command` keeps the value out of the file, in the keychain or a password manager. sbx runs it on this machine when it checks or refreshes the value, from a temporary folder, so give any path in full.
     exomecha has sbx keep the value for each of those sandboxes only (`sbx secret set-custom`, an experimental sbx command). The sandbox holds a placeholder in `$NPM_TOKEN`, and the sbx proxy puts the value in place of the placeholder in the request headers sent to those hosts, so the AI never sees the value. setup.sh and the AI use the variable, such as `//npm.pkg.github.com/:_authToken=${NPM_TOKEN}` in `.npmrc`. exomecha refuses a secret whose hosts reach `github.com` or `api.github.com`, where the AI could use it to push.
   - Optional: `limits.five_hour` (default 90) and `limits.weekly` (default 80), in percent of the Claude plan's 5-hour and weekly limits. exomecha starts no new work at or above them.
   - Optional: `limits.sandboxes` (default 2), how many PRD sandboxes builder and resolver may have at once, together.
   - Optional: `limits.read_sandboxes` (default 2), how many reviewer sandboxes, which only read the code, may exist at once. `limits.sandboxes` does not count them.
   - Optional: `limits.reviews` (default 5), how many times reviewer sends a PR back to builder, for a failed CI or a review that asks for changes, before a person decides. The count starts again after reviewer approves the PR or a person answers a stuck PRD.
   - Optional: `models.light` (default `opus:low`), `models.normal` (default `opus:high`) and `models.heavy` (default `opus:xhigh`), the claude model and effort of each tier, as `model:effort`. Effort is one of low, medium, high, xhigh and max. The model must support that effort and claude's auto permission mode, which haiku does not. reviewer runs on heavy, and builder on its PRD's tier.
   - Optional: `models.resolver` (default `opus:medium`), the claude model and effort of every resolver run, as `model:effort`. A conflict asks the same judgment whatever the PRD's tier, so resolver does not follow the tier label.
   - Run it in a terminal, where it keeps running until you press Ctrl-C or close the terminal:
     ```
     exomecha run <project>
     ```
   - At start, as `exomecha init` does, exomecha checks this machine and does not start until every check passes: sbx is installed, its daemon answers and it is signed in to Docker, sbx has a Claude login and no global `github` secret, this machine's claude is signed in, claude runs each model in auto permission mode (a short call on this machine), and the token reaches every repository. It reports every problem it finds, each with how to fix it. It does not check what the token may do in a repository, since GitHub does not say for a fine-grained token, nor whether sbx's network policy was set up.
   - `exomecha list` shows each project, its PRD repository, and whether an exomecha runs it.
   - exomecha creates any of the ten labels that are missing in the PRD repository when it starts.
   - One exomecha runs per PRD repository. A second one for the same PRD repository says so and does not start. Do not let two exomechas change the same repository: both name their branches `exomecha/prd-<number>`.

## Notes

- exomecha's log says only how exomecha itself is doing: when it starts and stops, when one of its own steps fails (sbx, git, GitHub, the Claude usage check) and when that step works again, when it pauses for a rate or usage limit, and when a run loses its sandbox. A line at ERROR means exomecha could not do what it set out to do. Each line goes to the terminal and to `~/.exomecha/<owner>/<repo>/logs/exomecha.log`, under the PRD repository's owner and name, as are all the folders below. Once the file would pass 10MB it becomes `exomecha.log.1` and a new one starts, so the log takes at most 20MB. Only you can read the folder.
- When a PRD goes to stuck, exomecha keeps what that run printed (claude's output, and the setup script's, the Dockerfile build's or the merge's) in a folder under `~/.exomecha/<owner>/<repo>/outputs/<PRD number>/`, and the stuck comment names the folder. Output can hold secrets, so it never goes into a comment or the log. Each file keeps its last 1MB (the Dockerfile build its last 8KB), and once the folders pass 100MB together the oldest run's go. Only you can read them.
- Only the usage check and the models' check at start use the claude login on this machine. Every worker runs claude in sandboxes, signed in with the login sbx keeps (Setup step 3).
- reviewer only reads the code. Its sandboxes start from sbx's default claude template and get only the code: no project template, allowed hosts, setup script or commit identity. These sandboxes count against `limits.read_sandboxes`, take about 625MB each, and are removed at exomecha's next check, within about a minute after claude ends. Only the run's folder stays until exomecha collects the result. The claude settings in this machine's `~/.claude` do not apply in any sandbox.
- Every worker's sandbox gets the claude user settings in `~/.exomecha/claude`, laid out like `~/.claude`. exomecha writes the folder the first time it starts: rules on comments and writing in `rules/`, and a `settings.json` that turns on the ponytail and humanizer plugins. After that the folder is yours. Edit a file to change a default and delete it to turn the default off. A change applies from the next sandbox. A deleted folder comes back with the first defaults at the next start. The project's own CLAUDE.md, rules and settings take precedence, so a project can turn a plugin off in its `.claude/settings.json`. exomecha merges `settings.json` into the one in sbx's template and installs the enabled plugins before claude starts, which adds a few seconds to each start. A plugin that fails to install is left out of that run, with a warning in exomecha's log.
- /exomecha:complete-prd gives each PRD a tier label, `tier:light`, `tier:normal` or `tier:heavy`, by how much building it asks. builder runs on that tier's model, also when it applies a review or fixes CI. A PRD with no tier label runs on normal, and one with several on the heaviest. Change the label to change the model. A run already going keeps its model, and the change applies from builder's next start.
- Each PRD starts with a `## Repositories` section that lists the repositories it changes, one `- owner/name` item each, in the order the AI should see them. /exomecha:complete-prd writes it, also for a single repository. A PRD whose section is missing, lists nothing, or names a repository not in `repos` goes to stuck.
- A PRD with one repository has it at `/home/agent/repo` in the sandbox, and claude starts there, as in a single checkout. A PRD with several has each in `/home/agent/repo/<name>`, and claude starts in `/home/agent/repo`. The AI's input names each folder, its PR and its base branch.
- builder opens a PR only in the repositories where it committed. The body of a PR starts with `Closes` and the PRD when the PRD has one repository, and with `Part of` and the PRD when it has several, so GitHub does not close the PRD at the first merge. builder's summary says in which order to merge several PRs. reviewer approves, or sends back, all of a PRD's PRs together. Once every PR of the PRD is merged, exomecha closes the PRD. A PR closed without a merge sends the PRD to stuck.
- exomecha writes the same summary, review or resolution on each of a PRD's PRs, and the AI's input shows it once.
- With several repositories, claude starts above them, so it loads each repository's CLAUDE.md only once it reads a file there (the AI is told to read each one first), and it does not apply the hooks, permissions or plugins in a repository's `.claude/settings.json`.
- When CI fails on a PR, reviewer's review lists only the failed runs and their links. When builder picks the PRD up again, exomecha downloads the end of each failed job's log and gives it to builder in its input only. A log holds whatever the project's CI printed, so it never goes into a comment.
- When there is work to start, exomecha first reads the Claude plan's usage with a small claude call, at most once every three minutes. At or above the limits in Setup it starts nothing new, and checks again only once the limit resets. Work already running finishes, so usage can end up a little over.
- exomecha sends a PRD's code into its sandbox and takes the AI's commits out with git, then checks them and pushes them from this machine. The GitHub token never enters a sandbox. exomecha's sandboxes cannot reach `ssh.github.com`, because sbx hands this machine's SSH agent to every sandbox.
- A run keeps going in its sandbox while exomecha is stopped. Ctrl-C leaves working PRDs as they are, and exomecha picks up their runs when it starts again. claude's output streams to `stream.jsonl` in the run's folder under `~/.exomecha/<owner>/<repo>/sandboxes/`.
- The files in `.exomecha/` are always read from the PRD repository's base branch, whichever repositories a PRD changes.
- If `.exomecha/sandbox.md` is there, builder's and resolver's input gets it as the sandbox's rules, which take precedence over CLAUDE.md. /exomecha:setup writes it when a check cannot run in the sandbox as CLAUDE.md says, for example "run only the unit tests and start no containers".
- If the base branch has `.exomecha/Dockerfile`, exomecha builds it into an sbx template the first time builder or resolver needs a sandbox after the file changed. The build runs in a sandbox of its own and takes a few minutes (about 2 to 3 for a Dockerfile that installs Playwright browsers), and the first sandbox from a new template takes about 25 seconds more to start. builder and resolver runs that start during a build wait for it. Once the new template is in, exomecha removes the project's older ones.
- Only a change to the Dockerfile rebuilds the template. To pick up a newer base image or claude, remove the template with `sbx template rm --force`, and the next builder or resolver run builds it again.
- `.exomecha/allowed-hosts` lists the hosts that builder's and resolver's sandboxes and the template build may reach beyond sbx's network policy, one per line. `**` (every host) is rejected. Other sandboxes on this machine are not affected.
- If the base branch has `.exomecha/setup.sh`, exomecha runs it with `bash -eo pipefail` in the PRD's sandbox, at `/home/agent/repo` (where claude starts), before builder or resolver starts the AI, with the PRD number in `EXOMECHA_PRD` and the PRD's repositories in `EXOMECHA_REPOS`, one line each: `owner/name`, a space, and the repository's folder in the sandbox. A script that serves several repositories reads it, for example `printf '%s\n' "$EXOMECHA_REPOS" | while read -r repo dir; do (cd "$dir" && npm ci); done`. reviewer only reads the code and does not run it. The script may leave services running as Docker containers started with `--restart unless-stopped`, which come back when a stopped sandbox starts again. It must end within 10 minutes, must leave nothing git can see, and must not commit. exomecha does not pass it the GitHub token.
- exomecha always runs the base branch's copy of the script, even for a PR's branch, so a fix to the script reaches every open PRD. A branch older than the script can still fail it, for example when the branch lacks a `.gitignore` entry the script relies on. Such a branch needs the base branch merged into it.
- exomecha's own git commands (clone, fetch, push, and moving code in and out of sandboxes) run none of the project's git hooks. The AI commits in a repository exomecha creates inside the sandbox, so the project's git hooks run there only if setup turns them on. A repository that uses Git LFS is not supported.
- The AI in builder and resolver makes the commits, following the project's commit message rules and commit checks (in CLAUDE.md, or enforced by a check). Without rules, a commit looks like `feat: add greeting` with `PRD: #12` as its last line.
- Those commits have the token owner as author: the GitHub profile name (or the login when it has none) and the address `<id>+<login>@users.noreply.github.com`. exomecha sets them in the sandbox's repository before `.exomecha/setup.sh` runs, so the script can change them.
- The token owner is the author of every PR, so exomecha's reviews are COMMENT reviews. When the token is your own, GitHub does not let you approve these PRs or request changes on them either, and a "require approval" protection rule on the base branch blocks your merge.
- Every comment and review exomecha writes starts with a bold line that names the worker, such as **exomecha resolver**, and a hidden HTML comment that names the worker and what the entry is. builder leaves a summary of each run on the PR, with the checks it ran. resolver leaves how it settled each merge of the base branch. reviewer leaves its reviews, and a review for a failed CI that lists the failed runs. Stuck notices go on the PRD.
- Every worker reads the same record: the PRD, then everything written on the PRD and on its PR, oldest first, each entry marked with who wrote it. That includes exomecha's own entries, so reviewer sees what it asked for before, and builder sees what resolver changed.
- exomecha only acts on issues, comments and reviews written by the repository's owners, members and collaborators. On a public repository, anything an outsider writes is ignored. GitHub may report a member who keeps their organization membership private as an outsider, so their comments can be left out.
- exomecha checks GitHub every minute. A check whose answer has not changed uses a conditional request, which GitHub does not count against the API rate limit. If the limit still runs out, exomecha waits until it resets.
- GitHub links a PR to its PRD (and closes the PRD on merge) only when the PR targets the repository's default branch. A PRD GitHub closes keeps `status:approved`, since exomecha reads only open issues. With another base branch or several repositories the PR does not close the PRD, and exomecha closes the PRD itself after the merges.

## When a PRD is approved (status:approved)

reviewer's approval on the PR says what the PR changes and what the AI checked. The PR is yours to merge.

- If it is right, merge the PR on GitHub, or each PR in the order builder's summary gives. The PRD closes once all are merged.
- If something needs changing, write it on a PR (a review, comments on lines, or the conversation) or on the PRD, then relabel the PRD `status:ready`. Every worker reads both, builder continues on the same PR, and reviewer reviews it again. A review on the PR must be a "Comment" when the token is your own.
- If the PR comes to conflict with the base branch while it waits, resolver sends the PRD back to `status:review` and resolves it, and reviewer approves it again.
- If the work is not wanted, close the PRD first, then its PRs. A PR closed while its PRD is still open sends the PRD to stuck.

## When a PRD is blocked (status:stuck)

The worker's comment says why it is blocked and what needs deciding.

- If there is nothing left to do, close the issue.
- Otherwise, answer in a comment and relabel it the way the comment says. That sends it back to the worker that owns the problem:
  - `status:ready` when builder got stuck. builder continues on the existing PR branch.
  - `status:review` when resolver got stuck on a conflict. resolver merges the base branch again and settles the conflict the way you answered.
  - `status:review` when reviewer's run failed. reviewer reviews the PR again.
  - `status:ready` when reviewer sent the PR back `limits.reviews` times. Its last review on the PR says what it still wants. Answer what builder should do, and builder continues on the same PR with a new count.
- When the AI says the sandbox blocked a host, add that host to `.exomecha/allowed-hosts` on the base branch if the PRD really needs it. Then relabel the PRD the way the comment says.
- When `.exomecha/setup.sh` failed, there is nothing to answer. The comment names the folder that keeps the script's output. Fix the script on the base branch, or merge the base branch into the PRD's branch if the comment says the branch is behind. Then relabel the PRD the way the comment says.
- When `.exomecha/Dockerfile` failed to build or ran over 30 minutes, or `.exomecha/allowed-hosts` has a bad line, there is nothing to answer either. For a build, the comment names the folder that keeps the build output. Fix the file on the base branch, then relabel the PRD the way the comment says.
- When resolver's merge of the base branch failed without a conflict (for example, a git setting from `.exomecha/setup.sh` refused it), there is nothing to answer either. The comment names the folder that keeps git's output. Fix what it reports, then relabel the PRD the way the comment says.
- When the AI's run fails (the sandbox stopped or went away, claude's output did not change for 15 minutes, or claude ended without a usable result), the PRD goes to stuck at once, and the comment says why. There is nothing to answer. The comment names the folder that keeps what claude printed, and a lost sandbox is also an error in exomecha's log. To try again, relabel the PRD the way the comment says.
- A run that ended at the Claude usage limit is not a failure, and starts again once the limit resets. Other failures in exomecha's own steps (sbx, git, GitHub) do not send the PRD to stuck either. exomecha tries the step again after 1, 2, 4, 8 and 16 minutes, then every 30 minutes until it works.

## Removing a project

Stop exomecha, then remove what it keeps on this machine for the project:

- its sandboxes, named `exomecha-run-<slug>-...` and `exomecha-build-<slug>`, with `sbx rm --force`
- its templates, `exomecha-<slug>` with any tag, with `sbx template rm --force`
- `~/.exomecha/<owner>/<repo>`
- `~/.exomecha/<project>.yaml`

`<slug>` is `owner-repo` in lowercase, with every run of characters other than letters and digits turned into one `-`. A slug longer than 32 characters is cut and ends with a short hash of the repository's name. `sbx ls` and `sbx template ls` show the exact names.

## Upgrading from a version with a .env file

- Stop exomecha, then run `exomecha init <project>` and give the same repositories, PRD repository, base branch and token as in `<project>.env`.
- Move any optional setting from `<project>.env` into `~/.exomecha/<project>.yaml`:

  | `<project>.env` | `<project>.yaml` |
  |---|---|
  | `EXOMECHA_SECRET_<NAME>=<hosts> <command>` | `secrets.<NAME>.hosts` and `secrets.<NAME>.command` |
  | `EXOMECHA_FIVE_HOUR_LIMIT`, `EXOMECHA_WEEKLY_LIMIT` | `limits.five_hour`, `limits.weekly` |
  | `EXOMECHA_SANDBOX_LIMIT`, `EXOMECHA_READ_SANDBOX_LIMIT` | `limits.sandboxes`, `limits.read_sandboxes` |
  | `EXOMECHA_REVIEW_LIMIT` | `limits.reviews` |
  | `EXOMECHA_TIER_LIGHT`, `EXOMECHA_TIER_NORMAL`, `EXOMECHA_TIER_HEAVY` | `models.light`, `models.normal`, `models.heavy` |
  | `EXOMECHA_RESOLVER_MODEL` | `models.resolver` |

- Start it with `exomecha run <project>`. Once it runs, delete `<project>.env`, which still holds the token.

## Upgrading from a version with EXOMECHA_REPO

- Upgrade while no PRD is in `status:working` and `sbx ls` shows no `exomecha-run-` sandbox. A run started by the older version is not picked up.
- The repository in `EXOMECHA_REPO` goes in `repos`, as the section above describes. It holds the PRDs as before.
- Add a `## Repositories` section to each open PRD that a worker has yet to finish, listing the repository as `- owner/name`.
- exomecha clones each repository again, under `~/.exomecha/<owner>/<repo>/repos/`. You can remove the old clone, `~/.exomecha/<owner>/<repo>/repo`.

## Upgrading from a version with the issue, discuss and prd skills

- Relabel each open PRD. `type:prd` becomes `prd`, and `status:queued` becomes `status:ready`. A PRD that had `type:prd` and no status needs `status:ready` too, since builder no longer queues it.
- exomecha no longer closes a `type:problem` issue when its PRDs close. Close the open ones by hand, or relabel one `prd` and `status:draft` to keep it as a draft.
- The `type:problem`, `type:prd` and `status:queued` labels stay in the repository. You can remove them by hand.

## Upgrading from a version with splitter and sequencer

- Before you upgrade, wait until every open Task is closed and its PRD has closed, and until `sbx ls` shows no splitter or sequencer sandbox. An open PRD with the `tasked` label would be built again as a whole, and a PR on an `exomecha/task-<number>` branch is no longer found.
- In `.exomecha/setup.sh`, change `EXOMECHA_TASK` to `EXOMECHA_PRD`.
- The `type:task`, `tasked` and `status:pending` labels stay in the repository. You can remove them by hand.
