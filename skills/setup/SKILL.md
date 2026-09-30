---
name: setup
description: Write .coworker/setup.sh, the script coworker runs in a fresh working copy before the AI starts building or resolving, so dependencies and ignored config files are already in place. Reads how the project installs and tests, writes the script, and proves it in a fresh working copy. Does not commit. Use when a person invokes /coworker:setup.
---

# Write the setup script

coworker gives every Task a fresh working copy that holds only what git tracks: no installed dependencies, no ignored config files. If the base branch has `.coworker/setup.sh`, coworker runs it at the root of that working copy before builder or resolver starts the AI. Write that script for this project and prove it works.

How coworker runs it:

- As `bash -eo pipefail -c "<the script's text>"`, from the root of the working copy. It stops at the first command that fails. `$0` is not the script's path.
- With the Task number in `COWORKER_TASK`, and nothing on standard input.
- Always the base branch's copy, even in a PR's working copy.
- For at most 10 minutes.
- With nobody watching. If it fails, the Task goes to `status:stuck` and a person has to fix the script.

## Rules

- Change only `.coworker/setup.sh`, and `.gitignore` when the script creates a file git would otherwise see. Do not change anything else.
- Do not commit. The person commits to the base branch.
- **No secrets in the script.** It is committed. Use the dummy values the project's CI or example config already uses. If a test needs a real secret, ask the person where that file lives on the machine that runs coworker, and have the script copy it from there.
- The script never asks for input.
- The script ends, well within 10 minutes. It does not start servers, watchers, or other background processes.
- The script leaves nothing git can see. Everything it creates is ignored by git, and it does not edit tracked files. Use the install command that follows the lockfile exactly.
- The script does not commit, check out, or otherwise change git history.
- The script does not use `$0` or `BASH_SOURCE` to find the repository. It already runs at the root, so use paths relative to it.
- The script is safe to run in several working copies at the same time. Where two copies would collide, such as a port or a database name, derive the value from `COWORKER_TASK`.
- The script only prepares the working copy. What is installed once per machine (a language runtime, Docker, a running database) is a prerequisite. Do not install it in the script. List it for the person.
- Keep the script short enough to review by reading it.

## Steps

1. Find the base branch: the branch coworker merges PRs into. If you don't know it, ask the person.
2. Read how the project is installed and tested: CLAUDE.md, the README and setup docs, package manifests and lockfiles, CI workflows, example config files, and `.gitignore`. A CI workflow is usually the most exact record of a setup from scratch.
3. Work out what a fresh working copy is missing before the tests can run:
   - dependencies, and the command that installs them
   - ignored config files, and the values they need
   - anything to prepare before the tests, such as migrations, generated code, or a build
   - what collides when two working copies run at the same time
   - what must already be installed on the machine
4. Write `.coworker/setup.sh`.
5. Prove it in a fresh working copy of the base branch, not in this checkout. This checkout already has everything installed, so it proves nothing. coworker's working copies are git worktrees, so use one, and run the script the way coworker does:
   ```
   git fetch origin <base>
   git worktree add --detach <temporary directory> origin/<base>
   mkdir -p <temporary directory>/.coworker
   cp .coworker/setup.sh <temporary directory>/.coworker/setup.sh
   cd <temporary directory>
   COWORKER_TASK=1 bash -eo pipefail -c "$(cat .coworker/setup.sh)" </dev/null
   git status --porcelain
   ```
   If you changed `.gitignore`, copy it over too.
   - `git status --porcelain` must show only the files you copied in.
   - Run the project's tests the way CLAUDE.md says. They must pass.
   - Run the script a second time. It must succeed again.
   - Fix the script and repeat until all three hold. Then remove the working copy: `git worktree remove --force <temporary directory>`.
6. Report to the person:
   - the script
   - what you ran in the fresh working copy and what happened, including how long the script took
   - what the machine that runs coworker must have installed
   - anything you could not check, such as a test you could not run here
7. Tell the person what is left for them:
   - Commit `.coworker/setup.sh` to the base branch, together with `.gitignore` if you changed it.
   - PRs that are already open are older than the script. coworker still runs the script in them, and it can fail there because the branch lacks the new `.gitignore` entries or a file the script reads. Let those PRs merge first, or merge the base branch into them afterwards.
   - If coworker runs on another machine, the script is proven only for this one. Run step 5 there too.
