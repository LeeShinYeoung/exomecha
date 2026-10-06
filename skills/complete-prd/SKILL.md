---
name: complete-prd
description: Write a PRD (a plan with every decision made) from this conversation, or turn draft PRDs into ready ones. Build a decision ledger in plan mode and close every row, asking only what the conversation and drafts left open. Check the open drafts for overlap and ask what to do with each, split the work into several PRDs only when one PR would be too big, check each draft with a subagent that sees only that draft and the repository, get approval, then label each PRD status:ready with a tier so exomecha builds it. Use when a person invokes /exomecha:complete-prd.
---

# Complete a PRD

exomecha builds each PRD as one PR, then reviews and merges it without a person. builder sees only the PRD's body and its comments. Anything the PRD leaves out is missing when the code gets written. **Make every decision here.** After this, a person only answers when a PRD is blocked (`status:stuck`).

## Rules

- Do not change code.
- Do not fill decisions with guesses. Close every ledger row as the ledger section says.
- Do not file while any ledger row is open.
- One PRD is one PR.
- The Acceptance criteria and Test scenarios sections are never empty. builder builds to them and reviewer checks the PR against them.
- Check overlap only with drafts. A PRD that is already ready or further along is not checked. resolver settles conflicts between their PRs.

## Steps

1. Find what to complete. The person may give one or more draft numbers, or none.
   - With draft numbers, read each one: `gh issue view <number> --comments`. Each draft becomes its own PRD unless the person merges them.
   - Without a number, this conversation is the source, and the PRD is a new issue.
2. For each draft, re-check it against the latest base branch, the branch exomecha merges PRs into. If you don't know it, ask the person. Run `git fetch origin <base>`, then read `origin/<base>` with `git log`, `git show`, and `git grep`.
   - Has the related code changed since the draft was recorded?
   - Is it already fixed? If so, ask the person whether to close the draft instead.
   - Were the guesses in the investigation notes right?

   Briefly report what you found.
3. Enter plan mode. Read the code the change touches, then build the decision ledger below and close its rows. Most rows of a well discussed change close with `code`, `decided` or `n/a`, so ask only about the rest.
4. Check the other drafts for overlap. Read every open draft except the ones being completed.
   ```
   gh issue list --label prd --label status:draft --state open --limit 200 --json number,title,body
   ```
   For each draft that overlaps a PRD, ask the person which of these to do, and record the answer in the Filing row:
   - Merge it: the PRD covers the draft, and the draft is closed after filing.
   - Leave the overlap to the draft: take it out of the PRD and name it under Out of scope.
   - Take the overlap out of the draft: the PRD covers it, and the draft's body is edited after filing.
   - Keep both as they are.
5. Decide the size. Write one PRD unless one PR would be too big to build and review well. If it would, split the work into several PRDs, each of which holds all of these:
   - Merging it alone leaves the base branch working.
   - It makes sense when read on its own.
   - It has the whole body format, with its own share of the acceptance criteria and test scenarios and its own ledger. builder reads only its own PRD, so repeat in it any decision and context it needs from the others, and name the work the others cover under Out of scope.
   - It is blocked by each earlier PRD it needs, and by each earlier PRD that changes a file it changes.

   When unsure, keep one PRD. The person decides on the split when approving. A draft that is split becomes the first PRD, and the others are new issues.
6. Write each PRD draft in the plan file under its own heading `# PRD <k>: <title>`, in the order to file them, even when there is only one. Under the heading, write the body in exactly the body format below, with the ledger as its last section. Its Filing row names its issue (new, or the draft's number), its tier (see Tier below), the PRDs it is blocked by, and what was decided for each overlapping draft.
7. Check each draft with a fresh reader. Give a subagent only that PRD's body and the repository path, and ask: "If you had to implement this with only this text and the repository, how would you do it, and where would you get stuck or have to guess?" For each place it gets stuck, fix the draft if code or the ledger answers it. Otherwise ask the person. Repeat until it no longer gets stuck.
8. Get approval (exit plan mode).
9. Check that the labels exist: `gh label list --limit 100`. If `prd`, `status:ready` or a tier label you need is missing, do not file. Tell the person that running exomecha once creates the labels.
10. File the PRDs in order. For each one:
    1. Save the text under its heading, **as is**, to a file outside the repository. Then put it on its issue. Until the last step, the issue has no `status:ready`, so no worker takes it.
       - A new PRD:
         ```
         gh issue create --title "<title>" --body-file <body file> --label prd
         ```
       - A PRD from a draft keeps the draft's issue:
         ```
         gh issue edit <draft number> --title "<title>" --body-file <body file>
         ```
    2. For each PRD it is blocked by, attach the blocker. Blockers are attached by **issue ID**, not issue number.
       ```
       BLOCKER_ID=$(gh api repos/{owner}/{repo}/issues/<blocker number> --jq .id)
       gh api --method POST repos/{owner}/{repo}/issues/<PRD number>/dependencies/blocked_by -F issue_id="$BLOCKER_ID"
       ```
    3. Add the labels last. exomecha takes a PRD as soon as it has `status:ready`, so its blockers must already be attached.
       - A new PRD:
         ```
         gh issue edit <PRD number> --add-label status:ready --add-label tier:<tier>
         ```
       - A PRD from a draft:
         ```
         gh issue edit <PRD number> --remove-label status:draft --add-label status:ready --add-label tier:<tier>
         ```
11. Apply what was decided for each overlapping draft.
    - A merged draft:
      ```
      gh issue close <draft number> --reason "not planned" --comment "Merged into #<PRD number>"
      ```
    - A draft whose overlap was taken out: edit its body with `gh issue edit <draft number> --body-file <body file>`, then comment that #<PRD number> covers the part taken out.
12. Share the PRD URLs. exomecha takes it from here.

## Decision ledger

Keep one row per category below. Close each row with the first of these that applies:

1. `code`: code or docs in the repository settle it. Cite the file.
2. `decided`: a draft, its comments, or this conversation already decided it. Say where.
3. `n/a`: the change does not touch this category. Give a one-line reason.
4. `answered`: none of the above applies. Ask the person, a few questions at a time.

A small change closes most rows with `code` or `n/a` and needs few questions. A larger one needs more.

| Category | What to settle |
|---|---|
| Product intent | the goal, the evidence for the problem, who it is for, how we know it worked |
| Scope | what is in, what is out, what must happen first, existing behavior to keep |
| User experience | entry points, the main flow, empty, loading, error and permission states, wording |
| API and contracts | interfaces that change, request, response and error shapes, compatibility |
| Data and state | what is stored or deleted, where data comes from, caches, migrations, concurrency and retries |
| Security and privacy | who may do it, isolation between users, secrets, personal data |
| Integrations | external services, their failures and retries, configuration and environment variables |
| Feature flag and rollout | whether a flag applies, its default, staged rollout, rollback |
| Operations | deploy unit, rollback, performance and cost |
| Observability | logs, metrics and alerts needed to debug it |
| Edge cases and failure modes | bad input, partial success, timeouts, duplicates, races |
| Documentation and support | docs that must change |
| Testing and acceptance | acceptance criteria, test scenarios, fixtures, regressions to check |
| Filing | target repository, issue (new or the draft's number), title, tier, the PRDs this one is blocked by, what was decided for each overlapping draft |

## Tier

The tier picks the model that builds the PRD.

- `light`: a mechanical change, such as a rename, a config value, or a small fix in one file.
- `normal`: a usual feature or fix that needs reading the code across a few files. Pick this when unsure.
- `heavy`: a change that needs design judgment, spans many modules, or touches concurrency, data migration, security, or a public interface.

## Body format

```markdown
## Background

(why we are doing this, and the facts confirmed in the draft or this conversation)

## To do

(what was decided: what changes and how)

## Out of scope

- (what this work does not do)

## Interfaces and data

(screens and wording, APIs, and stored data that change, or None)

## Edge cases

- (case): (what happens)

## Files

A starting point for the work, not a limit.

- Change: `path` (what changes there)
- Read: `path` (why it matters)

## Acceptance criteria

- (a result you can see and check)
- Existing behavior: (what must not break)

## Test scenarios

- (scenario name)
  - Setup:
  - Action:
  - Expected:

## Context

- Findings:
- Decisions and reasons:
- Approaches dropped:
- Pitfalls:

## Decision ledger

| Category | Status | Decision | Source or reason |
|---|---|---|---|
```
