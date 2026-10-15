# exomecha

exomecha has AI turn a PRD, which a person writes as a GitHub issue, into code and PRs. It has two parts: a plugin that people use, and an orchestrator that runs the AI. The README describes how it works.

This file is for agents that build exomecha. The instructions the orchestrator gives the AI in a sandbox are in `internal/worker/prompts`. When you change something this file describes, update this file in the same change.

## Plugin

A Claude Code plugin that people use. Its code is in `skills` and `.claude-plugin`. People write PRDs and prepare the sandboxes with these skills.

- `/exomecha:draft-prd`: records a problem as a draft. The issue gets the `prd` and `status:draft` labels, and no worker takes it.
- `/exomecha:summary-all-drafts`: summarizes the open drafts, to help pick what to settle next.
- `/exomecha:complete-prd`: writes a PRD with every decision the repository cannot answer, from the conversation or from drafts. builder makes the rest and lists them on the PR. When one PR would be too big, it splits the work into several PRDs, and a PRD that needs an earlier one is blocked by it. Last, it adds `status:ready` and a tier label. The tier picks the models that builder, reviewer and resolver run on for the PRD.
- `/exomecha:setup`: writes the files that prepare a PRD's sandbox to the project folder, and proves them in a real sandbox. `init` says to run it when the project has no setup script.

## Orchestrator

A Go daemon, the `exomecha` command. Its code is in `cmd/exomecha` and `internal`.

### Parts

- **Commands**: `init` (settings, checks and the plugin), `run` (runs a project), `list` (projects), `update` (exomecha and the plugin to the latest release), `--version` (the installed release).
- **Workers**: builder, reviewer and resolver. Each checks the PRDs every minute and takes one step.
- **Sandboxes**: one sbx sandbox per PRD, with claude running in it.
- **Project environment**: the setup files in the project folder. They prepare builder's and resolver's sandboxes.
- **Shared clones**: one per repository. Code goes to and from the sandboxes through it, and exomecha pushes from it.
- **Record**: what is written on the PRD and its PRs. It is the workers' input and output.
- **Usage gate**: stops new work when the Claude plan's usage reaches its limits.
- **Project folder**: `~/.exomecha/<project>`, with the setup files, the log, the clones and the runs.

### Status flow

| Status | Who | What | Next |
|---|---|---|---|
| `status:ready` | builder | Writes the code and opens the PRs, once no open issue blocks the PRD. | `status:working`, then `status:review` |
| `status:review` | reviewer | Reviews the PRs once CI passes. | `status:approved` or `status:ready` |
| `status:review` | resolver | Merges the base branch into a conflicting PR. | stays |
| `status:approved` | resolver | Sends the PRD back when a PR conflicts. | `status:review` |
| `status:approved` | person | Merges the PRs. | closed |
| `status:stuck` | person | Answers the worker's question. | the status the comment names |

A worker that needs a person's decision moves the PRD to `status:stuck`.

## Release

A person runs the release workflow in GitHub Actions with a version such as 0.4.0. The workflow sets the plugin's version, points the marketplace's plugin source at the new tag, and commits and tags this on main. Then GoReleaser builds the binaries for macOS and Linux and publishes the GitHub release with the list of PRs GitHub generates.

Do not change the plugin's version or the marketplace's plugin source by hand.
