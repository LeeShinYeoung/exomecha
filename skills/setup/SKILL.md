---
name: setup
description: Write the three files coworker uses to prepare a Task's sandbox (.coworker/Dockerfile, .coworker/setup.sh, .coworker/allowed-hosts) and prove them in a real sbx sandbox, the way coworker runs them. Does not commit. Use when a person invokes /coworker:setup.
---

# Set up the project's sandboxes

coworker runs builder and resolver inside Docker Sandboxes (sbx). Every Task gets a fresh sandbox that holds only what git tracks on the base branch: no installed dependencies, no ignored config files, no running services. Three files on the base branch prepare it. Write them for this project and prove they work in a real sandbox.

## What coworker does with the files

- `.coworker/Dockerfile` becomes an sbx template. coworker builds it the first time a Task needs a sandbox after the file changed on the base branch, and keeps it until the next change. The build runs inside a separate sandbox with the Dockerfile on stdin, so there is no build context. It must end within 30 minutes. If it fails, the Task goes to `status:stuck` and the build output stays in coworker's log. Without this file, sandboxes start from sbx's default claude template.
- `.coworker/allowed-hosts` lists the extra hosts a sandbox may reach. sbx's network policy blocks every host it does not know. A blocked HTTPS request gets a 403 response with the body `Approval required for <host>:443.`, and nobody is there to approve it. coworker allows the listed hosts in each Task sandbox and in the template build only. Other sandboxes on the machine are not affected.
- `.coworker/setup.sh` runs in each new Task sandbox before the AI starts:
  - as `bash -eo pipefail -c "<the script's text>"`, so it stops at the first command that fails and `$0` is not the script's path
  - at the root of the checkout, `/home/agent/repo`
  - with the Task number in `COWORKER_TASK`, nothing on stdin, and no GitHub token
  - for at most 10 minutes
  - always the base branch's copy, even for a PR branch
  - with nobody watching. If it fails, the Task goes to `status:stuck` and a person has to fix the script.

The sandbox runs Ubuntu as the user `agent`, who can use sudo. Node 22, git and Docker are already there. Docker runs inside the sandbox, so containers the script starts belong to that sandbox only. A sandbox stops when it sits idle, and when it starts again only containers started with `--restart unless-stopped` come back. coworker blocks `ssh.github.com` in Task sandboxes, and the script has no GitHub token, so it cannot fetch private repositories from GitHub.

## Rules

- Change only the three files, and `.gitignore` when the script creates a file git would otherwise see. Do not change anything else.
- Do not commit. The person commits to the base branch.
- **No secrets in any of the files.** They are committed. Use the dummy values the project's CI or example config already uses. Tests that need a real secret are not supported yet: report them as not run.

### Dockerfile

- The first line is `FROM docker/sandbox-templates:claude-code-docker`.
- The last `USER` is `agent`. Switch to `root` for system packages, then back.
- Do not change `ENTRYPOINT`, `CMD` or `WORKDIR`.
- No `COPY` or `ADD`. There is no build context.
- Only what does not depend on the checkout: OS packages, language runtimes, the version of the package manager the project pins (such as `packageManager` in package.json), and browsers pinned to the version in the lockfile, with their system libraries.
- No `docker pull` or other images for Docker inside the sandbox. The template does not keep them. The script starts those containers.
- Every change rebuilds the template, which takes a few minutes. What changes often belongs in the script.

### setup.sh

- Install dependencies with the command that follows the lockfile exactly.
- Create the ignored config files with dummy values.
- Start the services the tests need as Docker containers with `--restart unless-stopped` (in a compose file: `restart: unless-stopped`), and wait until they are ready. No other background processes.
- Run what the tests need before they start, such as migrations or generated code.
- Each Task has its own sandbox, so ports and database names do not need to depend on `COWORKER_TASK`.
- An install command that does nothing when the template already has the tool is fine. It guards against the template and the lockfile drifting apart.
- Leave nothing git can see. Everything the script creates is ignored by git, and it does not edit tracked files.
- Do not commit, check out, or otherwise change git history.
- Do not use `$0` or `BASH_SOURCE` to find the repository. Use paths relative to the root.
- Never ask for input. End well within 10 minutes. coworker runs the script for every new sandbox, so keep it fast.
- Keep it short enough to review by reading it.

### allowed-hosts

- One host pattern per line, as `sbx policy allow network` takes it, such as `api.example.com` or `*.example.com`. `#` starts a comment. Blank lines are skipped.
- `**` (every host) is rejected. A line with whitespace or a comma inside is rejected.
- Add a host only when all three hold:
  1. a step failed,
  2. that host was blocked during the step, and
  3. allowing just that host for the proof sandbox makes the step pass.
- A host that was blocked during a step that passed is never added.
- Use the exact host name. Use a wildcard only when the step needs several subdomains of one domain.
- Put a comment on each host that names the step that needs it.
- Without any host to add, do not create the file.

## Steps

1. Find the base branch: the branch coworker merges PRs into. If you don't know it, ask the person.
2. Read how the project is installed and checked: CLAUDE.md, the README and setup docs, package manifests and lockfiles, CI workflows, compose files, example config files, and `.gitignore`. A CI workflow is usually the most exact record of a setup from scratch.
3. Work out:
   - what to install once per template: system packages, runtimes, the pinned package manager, browsers (Dockerfile)
   - what each checkout needs: dependencies, ignored config files, services, migrations (setup.sh)
   - every check CLAUDE.md and CI require for a change: lint, unit tests, end-to-end tests
4. Write `.coworker/Dockerfile` and `.coworker/setup.sh`. Leave `.coworker/allowed-hosts` out until the proof shows a host is needed.
5. Prove the files as below.
6. Clean up as below.
7. Report, and tell the person what is left for them.

## Prove the files

Run sbx on this machine, not inside a sandbox. Use these names, where `<slug>` is the repository's `owner-repo` in lowercase, with every run of characters other than letters and digits turned into one `-`:

- `coworker-setup-build`: the sandbox that builds the template
- `coworker-setup-<slug>:<hash>`: the proof template, with `<hash>` the first 12 characters of `shasum -a 256 .coworker/Dockerfile`
- `coworker-setup-proof`: the sandbox that plays a Task

Commands that may run over 2 minutes (the build, the script, the tests) need a Bash timeout of up to 10 minutes, or run in the background.

### Build the template

```
sbx create --name coworker-setup-build claude
sbx policy allow network --sandbox coworker-setup-build "<hosts from allowed-hosts, joined with commas>"
sbx exec -i coworker-setup-build docker build -t coworker-setup-<slug>:<hash> - < .coworker/Dockerfile
sbx exec coworker-setup-build docker save coworker-setup-<slug>:<hash> | sbx template load /dev/stdin
sbx policy log coworker-setup-build --json
sbx rm --force coworker-setup-build
```

Skip the `policy allow` line while allowed-hosts has no hosts. If the build fails because a host was blocked, follow the allowed-hosts rule, with the build sandbox in place of the proof sandbox.

A download in the build can hang. If the build prints nothing for several minutes, stop it, remove the build sandbox and start the build over. Stopping `sbx exec` on this machine does not stop the build inside the sandbox, removing the sandbox does.

### Play a Task

1. Create the sandbox from the template, the way coworker does. Skip the `policy allow` line while allowed-hosts has no hosts.
   ```
   sbx create --name coworker-setup-proof --pull never -t coworker-setup-<slug>:<hash> --deny-network ssh.github.com claude
   sbx policy allow network --sandbox coworker-setup-proof "<hosts from allowed-hosts, joined with commas>"
   ```
2. Send the base branch, as it is on the remote. A plain clone of a bundle of a remote-tracking ref comes out empty, so fetch the ref by name:
   ```
   git fetch origin <base>
   git bundle create - origin/<base> | sbx exec -i coworker-setup-proof sh -c 'cat > /tmp/base.bundle'
   sbx exec coworker-setup-proof sh -c 'mkdir -p /home/agent/repo && cd /home/agent/repo && git init -q && git fetch -q /tmp/base.bundle refs/remotes/origin/<base> && git checkout -q -B <base> FETCH_HEAD && rm /tmp/base.bundle'
   ```
3. Send the files you wrote, and `.gitignore` if you changed it. `sbx cp` would leave them owned by this machine's user id, so pipe them in, one `cat` per file:
   ```
   sbx exec coworker-setup-proof mkdir -p /home/agent/repo/.coworker
   sbx exec -i coworker-setup-proof sh -c 'cat > /home/agent/repo/.coworker/setup.sh' < .coworker/setup.sh
   ```
4. Run the script exactly as coworker does, and time it:
   ```
   time sbx exec -w /home/agent/repo -e COWORKER_TASK=1 coworker-setup-proof bash -eo pipefail -c "$(cat .coworker/setup.sh)"
   ```
5. `sbx exec -w /home/agent/repo coworker-setup-proof git status --porcelain` shows only the files you sent.
6. Run every check CLAUDE.md and CI require for a change, each with `sbx exec -w /home/agent/repo coworker-setup-proof sh -c '<command>'`, with the environment variables CI sets for that check. They must pass.
7. `sbx stop coworker-setup-proof`, then run one check that needs the services again. `sbx exec` starts the sandbox, and the services must come back by themselves.
8. Run the script a second time. It must succeed again.

After each step, read `sbx policy log coworker-setup-proof --json`. A step's error message rarely names the host it could not reach, so the log is how a block is tied to a step. The log keeps its entries for a sandbox name even after that sandbox is removed, so compare `last_seen` with the time the step ran. Treat `<host>:443` and `<host>` as one host, and drop the `<host>.coworker-setup-proof.docker.internal` duplicates.

When a step fails, find out why. If a host was blocked during the step, allow just that host with `sbx policy allow network --sandbox coworker-setup-proof <host>` and run the step again. Then fix the file and start over. A changed Dockerfile needs a new template. Any change needs a new proof sandbox: `sbx rm --force coworker-setup-proof`, then go back to "Play a Task".

## Clean up

- `sbx rm --force coworker-setup-proof`, and `coworker-setup-build` if it is still there.
- `sbx template rm --force coworker-setup-<slug>:<hash>` for every proof template you built. Without `--force`, sbx asks for a confirmation it cannot read here.
- `sbx ls` and `sbx template ls` show nothing named `coworker-setup-`.
- Do not answer pending approvals (`sbx policy approval`). They go away with the sandbox.

## Report

- the three files, in full
- how long the script took
- every check you ran and what happened
- each host you added and the step that needs it
- each host that was blocked but left out, and why
- what you could not check, such as tests that need a real secret

Then tell the person what is left for them:

- Commit the files to the base branch, together with `.gitignore` if you changed it.
- coworker builds the template the first time a Task needs it, which takes a few minutes, and again after each change to the Dockerfile.
- PRs that are already open are older than the files. coworker still runs the base branch's script in them, and it can fail there when the branch lacks something the script relies on, such as a new `.gitignore` entry. Let those PRs merge first, or merge the base branch into them afterwards.
