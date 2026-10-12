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

1. Install exomecha. On an ARM Linux machine, replace `amd64` with `arm64`.

   ```
   # macOS
   brew install leeshinyeoung/tap/exomecha

   # Linux
   curl -fsSLO https://github.com/LeeShinYeoung/exomecha/releases/latest/download/exomecha_linux_amd64.tar.gz
   tar -xzf exomecha_linux_amd64.tar.gz
   sudo install exomecha /usr/local/bin/
   ```

2. Install the plugin in Claude Code.

   ```
   /plugin marketplace add LeeShinYeoung/exomecha
   /plugin install exomecha@exomecha
   ```

3. Sign sbx in to Docker and start from the balanced network policy. claude on this machine and sbx must use the same Claude account.

   ```
   sbx login
   sbx policy init balanced
   ```

4. In each repository's CLAUDE.md, write how to install the dependencies and run the tests. builder reads it to run the tests in its sandbox. Then run `/exomecha:setup` in the PRD repository and commit the files it makes to the base branch.

5. Create the project. It asks for the repositories, the PRD repository, the base branch and the token, and writes them to a settings file. The optional settings are in that file as comments.

   ```
   exomecha init <project>
   ```

## Usage

```
exomecha run <project>   # runs in this terminal until Ctrl-C
exomecha list            # lists the projects and whether each is running
```

You write PRDs in Claude Code with the skills.

- `/exomecha:draft-prd`: records a problem as a draft. Workers don't take drafts.
- `/exomecha:summary-all-drafts`: shows each draft's title and a summary of up to three lines.
- `/exomecha:complete-prd`: makes every decision and sets the PRD to status:ready.

When a PRD reaches status:approved, merge its PRs. If there are several, merge them in the order builder's summary gives. If something needs fixing, write it on the PR or the PRD and set it back to status:ready.

When a PRD reaches status:stuck, the comment says why. Answer in a comment and change the label to the one the comment names. If there is nothing left to do, close the issue.

If the token is your own, you are the author of every PR. So a "require approval" protection rule on the base branch blocks your merge.
