---
name: draft-prd
description: Record a problem found or a problem to solve as a draft PRD, a light GitHub issue labeled prd and status:draft. Workers do not take a draft. Use when a person invokes /exomecha:draft-prd.
---

# Draft a PRD

Record a problem as a backlog item before the conversation that decides everything. A draft has the `prd` and `status:draft` labels, and no worker takes it. Later, `/exomecha:summary-all-drafts` lists the drafts, and `/exomecha:complete-prd <number>` turns one into a PRD that exomecha builds.

## Rules

- Do not change code. If you changed anything temporarily to investigate, put it back.
- **Keep confirmed facts and guesses apart.** Only what you saw directly in code, logs, or a reproduction goes under "Confirmed facts".
- Ideas from the conversation go under "Ideas", each marked as not decided. Do not decide a direction here.
- One problem per draft. If two problems can be fixed separately, file two drafts.
- Keep it light. Ask the person only when the problem itself is unclear.

## Steps

1. File the draft in the PRD repository, whose issues exomecha watches. It is usually this session's git remote (`gh repo view --json nameWithOwner`). When it is not, ask the person, and add `--repo <owner/name>` to each `gh` command. GitHub actions use the person's `gh` login.
2. Read the code related to the problem, enough to tell what happens and where.
3. Check that the labels exist: `gh label list --limit 100`. If `prd` or `status:draft` is missing, do not file. Tell the person that running exomecha once creates the labels.
4. Write the body in the format below and file it.
   ```
   gh issue create --title "<title>" --body-file <body file> --label prd --label status:draft
   ```
5. Share the issue URL.

## Body format

```markdown
## Problem

- What happens:
- How to reproduce:
- Expected:

## Investigation notes

- Confirmed facts:
- Guesses (not confirmed):
- Recorded at: <repository>, <branch investigated>@<first 7 characters of the commit>, for each repository read

## Ideas

- (idea): not decided

## Open questions

- (what must be decided before this can be built, or None)
```
