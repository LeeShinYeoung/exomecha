---
name: discuss
description: Re-check a type:problem issue against the latest base branch, settle on a direction with the person by talking through options, then leave the decision as one comment on the problem issue. Does not change code. Use when a person invokes /coworker:discuss.
---

# Decide a direction

Re-check a problem recorded with `/coworker:issue` against the current code, and settle on a direction with the person. The decision goes on the problem issue as a comment, so `/coworker:prd` can pick it up from any session.

## Rules

- Do not change code.
- Do not create or edit issues. Once the direction is settled, leave exactly one comment on the problem issue, in the format below.
- Do not change the current checkout.

## Steps

1. Read the issue: `gh issue view <number> --comments`. If no number was given, ask.
2. Find the base branch: the branch coworker merges PRs into. If you don't know it, ask the person.
3. Re-check against the latest base branch. Run `git fetch origin <base>`, then read `origin/<base>` with `git log`, `git show`, and `git grep`.
   - Has the related code changed since the issue was recorded?
   - Is it already fixed?
   - Were the guesses in the investigation notes right?
4. Briefly report what you found, then lay out options for the direction. For each option, say what changes and what it costs, and recommend one that the facts you confirmed support.
5. Settle on a direction together.
6. Write the comment in the format below and post it.
   ```
   gh issue comment <number> --body-file <comment file>
   ```
7. Tell the person to write the plan with `/coworker:prd <issue number>`.

## Comment format

```markdown
## Direction

(the direction chosen, and why)

## Re-checked on <base>@<first 7 characters of the commit>

(what the latest base branch showed: related code that changed, whether it is already fixed, whether the guesses held)

## Options considered

- (option): (why it was chosen or dropped)

## Decisions made

- (each decision made in the conversation)

## Open questions

- (what is still undecided, or None)
```
