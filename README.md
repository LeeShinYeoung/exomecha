# exomecha

PRD-driven Agent Orchestrator

## Overview

A PRD is an issue in the PRD repository. It gets one PR in each repository it changes. exomecha reads the PRD's labels and hands the work to builder, reviewer and resolver.

```
[person]    /exomecha:complete-prd → the PRD is status:ready
[builder]   ready → writes the code in a sandbox → PR → status:review
[reviewer]  review → checks CI, reviews the code → status:approved (ready if something needs fixing)
[person]    approved → merges the PRs → the PRD closes
[resolver]  a conflicting PR → merges the base branch to resolve it → stays in review
```

When a worker cannot go on, it moves the PRD to status:stuck and leaves the reason in a comment.

For each run, a worker runs Claude Code in a Docker Sandboxes (sbx) sandbox of its own. The GitHub token never enters a sandbox.

## Requirements

- macOS or Linux
- Claude Code and a Claude account
- sbx (Docker Sandboxes). You don't need Docker Desktop.
- A GitHub fine-grained token that reaches only the PRD repository and the repositories PRDs change, with these permissions:
  - Issues: read and write
  - Pull requests: read and write
  - Contents: read and write
  - Actions: read

## Installation

1. Install exomecha in `~/.local/bin`, where Claude Code installs itself. On an Intel Mac, replace `arm64` with `amd64`. On an ARM Linux machine, replace `amd64` with `arm64`.

   ```
   # macOS
   curl -fsSLO https://github.com/LeeShinYeoung/exomecha/releases/latest/download/exomecha_darwin_arm64.tar.gz
   tar -xzf exomecha_darwin_arm64.tar.gz
   mkdir -p ~/.local/bin
   install exomecha ~/.local/bin/

   # Linux
   curl -fsSLO https://github.com/LeeShinYeoung/exomecha/releases/latest/download/exomecha_linux_amd64.tar.gz
   tar -xzf exomecha_linux_amd64.tar.gz
   mkdir -p ~/.local/bin
   install exomecha ~/.local/bin/
   ```

2. Sign sbx in to Docker and start from the balanced network policy. claude on this machine and sbx must use the same Claude account.

   ```
   sbx login
   sbx policy init balanced
   ```

3. In each repository's CLAUDE.md, write how to install the dependencies and run the tests. builder reads it to run the tests in its sandbox.

4. Create the project. It asks for the repositories, the PRD repository, the base branch and the token, and writes them to a settings file. The optional settings are in that file as comments. Then it checks this machine, and installs the exomecha plugin in Claude Code for your user.

   ```
   exomecha init <project>
   ```

5. In Claude Code, run `/exomecha:setup <project>`. It writes the files that prepare each PRD's sandbox to the project folder and proves them in a real sandbox. Answer its questions.

On a machine that only writes PRDs, without running exomecha, install the plugin in Claude Code yourself:

```
/plugin marketplace add LeeShinYeoung/exomecha
/plugin install exomecha@exomecha
```

## Update

Update exomecha and its plugin to the latest release:

```
exomecha update
```

On a machine that only writes PRDs, update the plugin on its own:

```
claude plugin marketplace update exomecha
claude plugin update exomecha@exomecha
```

## Usage

```
exomecha run <project>   # runs in this terminal until Ctrl-C
exomecha list            # lists the projects and whether each is running
exomecha update          # updates exomecha and its plugin to the latest release
exomecha --version       # prints the installed version
```

You write PRDs in Claude Code with the skills.

- `/exomecha:draft-prd`: records a problem as a draft. Workers don't take drafts.
- `/exomecha:summary-all-drafts`: shows each draft's title and a summary of up to three lines.
- `/exomecha:complete-prd`: makes every decision and sets the PRD to status:ready.

When a PRD reaches status:approved, merge its PRs. If there are several, merge them in the order builder's summary gives. If something needs fixing, write it on the PR or the PRD and set it back to status:ready.

When a PRD reaches status:stuck, the comment says why. Answer in a comment and change the label to the one the comment names. If there is nothing left to do, close the issue.

If the token is your own, you are the author of every PR. So a "require approval" protection rule on the base branch blocks your merge.

## License

MIT. See [LICENSE](LICENSE).
