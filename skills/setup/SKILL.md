---
name: setup
description: Write the files exomecha uses to prepare a PRD's sandbox (.exomecha/Dockerfile, .exomecha/setup.sh, .exomecha/allowed-hosts, .exomecha/sandbox.md) in the PRD repository and prove them in a real sbx sandbox, the way exomecha runs them, for one repository or several. Does not commit. Use when a person invokes /exomecha:setup.
---

# Set up the project's sandboxes

exomecha runs builder and resolver inside Docker Sandboxes (sbx). Every PRD gets a fresh sandbox that holds only what git tracks on the base branch of each repository the PRD lists: no installed dependencies, no ignored config files, no running services. Files on the PRD repository's base branch prepare it, one set for every repository exomecha may change (`EXOMECHA_REPOS`). Write them in the PRD repository and prove they work in a real sandbox.

## Where the code is in a sandbox

- A PRD with one repository has it at `/home/agent/repo`.
- A PRD with several has each at `/home/agent/repo/<name>`, the repository's name without its owner. `/home/agent/repo` itself is a plain folder.
- claude and the setup script start in `/home/agent/repo` either way. `EXOMECHA_REPOS` lists the PRD's repositories, one line each: `owner/name`, a space, and its folder. One script serves both layouts when it finds each repository's folder there:
  ```
  printf '%s\n' "$EXOMECHA_REPOS" | while read -r repo dir; do
    case $repo in
      */api) (cd "$dir" && npm ci) ;;
      */web) (cd "$dir" && pnpm install --frozen-lockfile) ;;
    esac
  done
  ```

## What exomecha does with the files

- `.exomecha/Dockerfile` becomes an sbx template. exomecha builds it the first time a PRD needs a sandbox after the file changed on the base branch, and keeps it until the next change. The build runs inside a separate sandbox with the Dockerfile on stdin, so there is no build context. It must end within 30 minutes. If it fails, the PRD goes to `status:stuck`, and the comment names the folder on exomecha's machine that keeps the end of the build output. Without this file, sandboxes start from sbx's default claude template.
- `.exomecha/allowed-hosts` lists the extra hosts a sandbox may reach. sbx's network policy blocks every host it does not know. A blocked HTTPS request gets a 403 response with the body `Approval required for <host>:443.`, and nobody is there to approve it. exomecha allows the listed hosts in each PRD sandbox and in the template build only. Other sandboxes on the machine are not affected.
- `.exomecha/setup.sh` runs in each new PRD sandbox before the AI starts:
  - as `bash -eo pipefail -c "<the script's text>"`, so it stops at the first command that fails and `$0` is not the script's path
  - in `/home/agent/repo`, as above
  - with the PRD number in `EXOMECHA_PRD`, the repositories in `EXOMECHA_REPOS`, each secret's placeholder in its variable (see Secrets), nothing on stdin, and no GitHub token
  - for at most 10 minutes
  - always the base branch's copy, even for a PR branch
  - with nobody watching. If it fails, the PRD goes to `status:stuck` and a person has to fix the script.

- `.exomecha/sandbox.md` goes into builder's and resolver's input as the sandbox's rules, which take precedence over CLAUDE.md. It holds what the person chose for checks the sandbox cannot run as CLAUDE.md says, in plain sentences for the AI, such as "Run only the unit tests. Do not start containers, since the services they need are only on the developers' machines."

The sandbox runs Ubuntu as the user `agent`, who can use sudo. Node 22, git and Docker are already there. Docker runs inside the sandbox, so containers the script starts belong to that sandbox only. A sandbox stops when it sits idle, and when it starts again only containers started with `--restart unless-stopped` come back. exomecha blocks `ssh.github.com` in PRD sandboxes, and the script has no GitHub token, so it cannot fetch private repositories from GitHub.

## Rules

- Change only the files in `.exomecha/`, and `.gitignore` when the script creates a file git would otherwise see. Do not change anything else, in any repository.
- Do not commit. The person commits to the base branch.
- **No secrets in any of the files.** They are committed. Use the dummy values the project's CI or example config already uses. A secret that only a request to some host needs, such as a token for a private package registry, goes through sbx (see Secrets). Other tests that need a real secret cannot run in the sandbox: ask the person about them (step 4).

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
- Each PRD has its own sandbox, so ports and database names do not need to depend on `EXOMECHA_PRD`.
- An install command that does nothing when the template already has the tool is fine. It guards against the template and the lockfile drifting apart.
- Leave nothing git can see. Everything the script creates is ignored by git, and it does not edit tracked files.
- Do not commit, check out, or otherwise change git history.
- Do not use `$0` or `BASH_SOURCE` to find the repository. Use paths relative to the root.
- Never ask for input. End well within 10 minutes. exomecha runs the script for every new sandbox, so keep it fast.
- Keep it short enough to review by reading it.

### sandbox.md

- Write it only when the person chose how a check runs in the sandbox (step 4). Without such a choice, do not create the file.
- One short paragraph or list item per choice: what the AI does in the sandbox, and why, so it does not try the other way.

### Secrets

exomecha takes each `EXOMECHA_SECRET_<NAME>` from its `.env` file: the hosts, then a space and a command on the person's machine that prints the value. sbx keeps the value for one sandbox. The sandbox holds only a placeholder in `$<NAME>`, and the sbx proxy puts the value in place of the placeholder in the headers of requests to those hosts.

- Use `$<NAME>` where the project needs the value, such as `//npm.pkg.github.com/:_authToken=${NPM_TOKEN}` in an `.npmrc` the script writes. Tell the person which `EXOMECHA_SECRET_` lines to add, and that the token needs only read access.
- Requests from a Docker build or container inside the sandbox do not go through that substitution on their own. If a build there installs private packages, give the build an `.npmrc` of its own (as a build secret) that sends npm through the proxy and trusts its certificate: the lines above, `https-proxy=http://gateway.docker.internal:3128`, `proxy=http://gateway.docker.internal:3128`, and every certificate of `/etc/ssl/certs/ca-certificates.crt` (which holds the proxy's) as a `ca[]="<certificate with \n for each line break>"` line. Expect such an install to take several times longer.

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

1. Find the base branch, the branch exomecha's PRs target, and the repositories exomecha may change (`EXOMECHA_REPOS`). If you don't know them, ask the person. This folder is the PRD repository, where the files go.
2. For each repository, read how it is installed and checked where this session has its code: CLAUDE.md, the README and setup docs, package manifests and lockfiles, CI workflows, compose files, example config files, and `.gitignore`. A CI workflow is usually the most exact record of a setup from scratch.
3. Work out:
   - what to install once per template, for every repository together: system packages, runtimes, the pinned package managers, browsers (Dockerfile)
   - what each checkout needs: dependencies, ignored config files, services, migrations (setup.sh)
   - every check CLAUDE.md and CI require for a change: lint, unit tests, end-to-end tests
4. Find each check that cannot run in the sandbox as CLAUDE.md says, such as one that needs a database or service only the developers' machines reach, a real secret that is not a request header, or containers the sandbox cannot run in time. Ask the person what the AI should do with each, with the choices that fit, such as running a narrower set of tests, starting a stand-in as a container in setup.sh, or not running it in the sandbox. Write the answers to `.exomecha/sandbox.md`.
5. Write `.exomecha/Dockerfile` and `.exomecha/setup.sh`. Leave `.exomecha/allowed-hosts` out until the proof shows a host is needed.
6. Prove the files as below.
7. Clean up as below.
8. Report, and tell the person what is left for them.

## Prove the files

Run sbx on this machine, not inside a sandbox. Use these names, where `<slug>` is the PRD repository's `owner-repo` in lowercase, with every run of characters other than letters and digits turned into one `-`:

- `exomecha-setup-build`: the sandbox that builds the template
- `exomecha-setup-<slug>:<hash>`: the proof template, with `<hash>` the first 12 characters of `shasum -a 256 .exomecha/Dockerfile`
- `exomecha-setup-proof`: the sandbox that plays a PRD

Commands that may run over 2 minutes (the build, the script, the tests) need a Bash timeout of up to 10 minutes, or run in the background.

### Build the template

```
sbx create --name exomecha-setup-build claude
sbx policy allow network --sandbox exomecha-setup-build "<hosts from allowed-hosts, joined with commas>"
sbx exec -i exomecha-setup-build docker build -t exomecha-setup-<slug>:<hash> - < .exomecha/Dockerfile
sbx exec exomecha-setup-build docker save exomecha-setup-<slug>:<hash> | sbx template load /dev/stdin
sbx policy log exomecha-setup-build --json
sbx rm --force exomecha-setup-build
```

Skip the `policy allow` line while allowed-hosts has no hosts. If the build fails because a host was blocked, follow the allowed-hosts rule, with the build sandbox in place of the proof sandbox.

A download in the build can hang. If the build prints nothing for several minutes, stop it, remove the build sandbox and start the build over. Stopping `sbx exec` on this machine does not stop the build inside the sandbox, removing the sandbox does.

### Play a PRD

1. Create the sandbox from the template, the way exomecha does. Skip the `policy allow` line while allowed-hosts has no hosts.
   ```
   sbx create --name exomecha-setup-proof --pull never -t exomecha-setup-<slug>:<hash> --deny-network ssh.github.com claude
   sbx policy allow network --sandbox exomecha-setup-proof "<hosts from allowed-hosts, joined with commas>"
   ```
   For each secret, have sbx keep it for the proof sandbox, the way exomecha does, and pass `-e <NAME>=<placeholder>` to every `sbx exec` below:
   ```
   sbx secret set-custom --sandbox exomecha-setup-proof --host <host> --env <NAME> --placeholder exomecha-secret-<name in lowercase> --command "<command>"
   ```
2. Send each repository's base branch, as it is on the remote, into its folder: `/home/agent/repo` with one repository, `/home/agent/repo/<name>` with several. With several, prove the layout with every repository exomecha may change. A plain clone of a bundle of a remote-tracking ref comes out empty, so fetch the ref by name. In each repository's clone:
   ```
   git fetch origin <base>
   git bundle create - origin/<base> | sbx exec -i exomecha-setup-proof sh -c 'cat > /tmp/base.bundle'
   sbx exec exomecha-setup-proof sh -c 'mkdir -p <folder> && cd <folder> && git init -q && git fetch -q /tmp/base.bundle refs/remotes/origin/<base> && git checkout -q -B <base> FETCH_HEAD && rm /tmp/base.bundle'
   ```
3. When the PRD repository is one of the repositories, send the files you wrote into its folder too, and `.gitignore` if you changed it. `sbx cp` would leave them owned by this machine's user id, so pipe them in, one `cat` per file:
   ```
   sbx exec exomecha-setup-proof mkdir -p <folder>/.exomecha
   sbx exec -i exomecha-setup-proof sh -c 'cat > <folder>/.exomecha/setup.sh' < .exomecha/setup.sh
   ```
4. Run the script exactly as exomecha does, and time it. `EXOMECHA_REPOS` holds one line per repository you sent:
   ```
   time sbx exec -w /home/agent/repo -e EXOMECHA_PRD=1 -e "EXOMECHA_REPOS=$(printf 'owner/api /home/agent/repo/api\nowner/web /home/agent/repo/web')" exomecha-setup-proof bash -eo pipefail -c "$(cat .exomecha/setup.sh)"
   ```
5. `sbx exec -w <folder> exomecha-setup-proof git status --porcelain` shows only the files you sent, in each repository's folder.
6. Run every check CLAUDE.md and CI require for a change, in each repository's folder, each with `sbx exec -w <folder> exomecha-setup-proof sh -c '<command>'`, with the environment variables CI sets for that check. They must pass, apart from the checks `.exomecha/sandbox.md` says not to run in the sandbox.
7. `sbx stop exomecha-setup-proof`, then run one check that needs the services again. `sbx exec` starts the sandbox, and the services must come back by themselves.
8. Run the script a second time. It must succeed again.

After each step, read `sbx policy log exomecha-setup-proof --json`. A step's error message rarely names the host it could not reach, so the log is how a block is tied to a step. The log keeps its entries for a sandbox name even after that sandbox is removed, so compare `last_seen` with the time the step ran. Treat `<host>:443` and `<host>` as one host, and drop the `<host>.exomecha-setup-proof.docker.internal` duplicates.

When a step fails, find out why. If a host was blocked during the step, allow just that host with `sbx policy allow network --sandbox exomecha-setup-proof <host>` and run the step again. Then fix the file and start over. A changed Dockerfile needs a new template. Any change needs a new proof sandbox: `sbx rm --force exomecha-setup-proof`, then go back to "Play a PRD".

## Clean up

- `sbx rm --force exomecha-setup-proof`, and `exomecha-setup-build` if it is still there.
- `sbx template rm --force exomecha-setup-<slug>:<hash>` for every proof template you built. Without `--force`, sbx asks for a confirmation it cannot read here.
- `sbx ls` and `sbx template ls` show nothing named `exomecha-setup-`.
- Do not answer pending approvals (`sbx policy approval`). They go away with the sandbox.

## Report

- the files, in full
- how long the script took
- every check you ran and what happened
- each host you added and the step that needs it
- each host that was blocked but left out, and why
- what you could not check, such as tests that need a real secret, and what `.exomecha/sandbox.md` tells the AI instead
- the `EXOMECHA_SECRET_` lines the person needs to add, with the hosts, without any value

Then tell the person what is left for them:

- Commit the files to the PRD repository's base branch, together with `.gitignore` if you changed it.
- exomecha builds the template the first time a PRD needs it, which takes a few minutes, and again after each change to the Dockerfile.
- PRs that are already open are older than the files. exomecha still runs the base branch's script in them, and it can fail there when the branch lacks something the script relies on, such as a new `.gitignore` entry. Let those PRs merge first, or merge the base branch into them afterwards.
