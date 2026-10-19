# exomecha

PRD-driven Agent Orchestrator

## Overview

A PRD is an issue in the PRD repository. It gets one PR in each repository it changes. exomecha reads the PRD's labels and hands the work to builder, reviewer and resolver.

![exomecha's parts and how they connect](docs/diagrams/overview.svg)

![How the workers move a PRD](docs/diagrams/workers.svg)

When a worker cannot go on, it moves the PRD to status:stuck and leaves the reason in a comment.

For each run, a worker runs Claude Code in a Docker Sandboxes (sbx) sandbox of its own. The GitHub token never enters a sandbox.

## Working on PRDs

You write PRDs in Claude Code with the skills.

- `/exomecha:draft-prd`: records a problem as a draft. Workers don't take drafts.
- `/exomecha:summary-all-drafts`: shows each draft's title and a summary of up to three lines.
- `/exomecha:complete-prd`: makes every decision and sets the PRD to status:ready.

![How the PRD skills make a PRD](docs/diagrams/prd-skills.svg)

## Requirements

- macOS or Linux
- git
- Claude Code and a Claude subscription
- sbx (Docker Sandboxes), signed in to Docker and started from the balanced network policy. You don't need Docker Desktop.

  ```
  sbx login
  sbx policy init balanced
  ```

- A GitHub fine-grained token. `exomecha init` links to the form that makes it.
- In each repository's CLAUDE.md, how to install the dependencies and run the tests. builder follows it in its sandbox.

## Installation

### The machine that runs exomecha

1. Install exomecha.

   ```
   # macOS (Apple silicon)
   curl -fsSLO https://github.com/LeeShinYeoung/exomecha/releases/latest/download/exomecha_darwin_arm64.tar.gz
   tar -xzf exomecha_darwin_arm64.tar.gz
   mkdir -p ~/.local/bin
   install exomecha ~/.local/bin/

   # Linux (x86_64)
   curl -fsSLO https://github.com/LeeShinYeoung/exomecha/releases/latest/download/exomecha_linux_amd64.tar.gz
   tar -xzf exomecha_linux_amd64.tar.gz
   mkdir -p ~/.local/bin
   install exomecha ~/.local/bin/
   ```

2. Create the project. It asks for the settings, checks this machine and installs the exomecha plugin. When it signs sbx in to Claude, use the same account as claude on this machine.

   ```
   exomecha init <project>
   # Asks for:
   # * Repositories exomecha works on
   # * Repository where you file PRDs (with several repositories)
   # * Base branch
   # * GitHub token
   ```

3. Prepare the sandbox in Claude Code, and answer its questions.

   ```
   /exomecha:setup <project>
   # Reads each repository's CLAUDE.md, CI and config, and prepares the sandbox to run the tests:
   # * Runtimes and system packages
   # * Dependencies, and config files git ignores (such as .env)
   # * Services such as databases, and migrations
   # * Hosts the sandbox may reach, and access to private package registries
   # * What to do with checks the sandbox cannot run
   # Last, it starts a real sandbox, runs every check CLAUDE.md and CI ask for, and makes sure they all pass.
   ```

4. Run exomecha. It runs in this terminal until Ctrl-C.

   ```
   exomecha run <project>
   ```

### A machine that only writes PRDs

Install the plugin in Claude Code.

```
claude plugin marketplace add LeeShinYeoung/exomecha
claude plugin install exomecha@exomecha
```

## Commands

```
exomecha list        # lists the projects and whether each is running
exomecha update      # updates exomecha and its plugin to the latest release
exomecha --version   # prints the installed version
```

## License

[MIT](LICENSE)
