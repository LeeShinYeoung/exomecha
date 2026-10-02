---
name: issue
description: Record a problem and the facts you confirmed as a type:problem GitHub issue. Never write a solution. Use when a person invokes /exomecha:issue.
---

# Record a problem

Investigate the problem the person describes and file only the problem and the confirmed facts as a `type:problem` issue. The direction is decided in `/exomecha:discuss` and the plan in `/exomecha:prd`.

## Rules

- **Never write a solution.** Even if the person suggests a fix, leave it out of the issue.
- **Keep confirmed facts and guesses apart.** Only what you saw directly in code, logs, or a reproduction goes under "Confirmed facts".
- Do not change code. If you changed anything temporarily to investigate, put it back.
- One problem per issue. If two problems can be fixed separately, file two issues.

## Steps

1. The target repository is this session's git remote (`gh repo view --json nameWithOwner`). GitHub actions use the person's `gh` login.
2. Investigate: what happens, how to reproduce it, what should happen. Read the related code and history.
3. Write the body in the format below and file it.
   ```
   gh issue create --title "<title>" --body-file <body file> --label type:problem
   ```
   If the `type:problem` label does not exist, do not file. Tell the person that running exomecha once creates the labels.
4. Share the issue URL.

## Body format

```markdown
## Problem

- What happens:
- How to reproduce:
- Expected:

## Investigation notes

- Starting point: (file, screen, or log where the investigation began)
- Confirmed facts:
- Guesses (not confirmed):
- Ruled out:
- Open questions:
- Recorded at: <branch investigated>@<first 7 characters of the commit>
```
