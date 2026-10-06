---
name: prd
description: Write a PRD (a plan with every decision made) from a type:problem issue whose direction is settled. Build a decision ledger in plan mode and close every row, split the work into several PRDs only when one PR would be too big, check each draft with a subagent that sees only that draft and the repository, get approval, then file each as a type:prd issue with a tier label, attached under the problem. Use when a person invokes /exomecha:prd.
---

# Write a PRD

exomecha builds each PRD as one PR, then reviews and merges it without a person. builder sees only the PRD's body and its comments. Anything the PRD leaves out is missing when the code gets written. **Make every decision here.** After this, a person only answers when a PRD is blocked (`status:stuck`).

## Rules

- Do not change code.
- Do not fill decisions with guesses. Close every ledger row as the ledger section says.
- Do not file while any ledger row is open.
- One PRD is one PR.
- The Acceptance criteria and Test scenarios sections are never empty. builder builds to them and reviewer checks the PR against them.
- Do not check for overlap with the PRDs of other problems. resolver settles conflicts between their PRs.

## Steps

1. Read the problem issue with its comments: `gh issue view <number> --comments`. If no number was given, ask. The comment left by `/exomecha:discuss` holds the chosen direction and the decisions made so far. Start from it.
2. Enter plan mode. Read the code the change touches, then build the decision ledger below and close its rows.
3. Decide the size. Write one PRD unless one PR would be too big to build and review well. If it would, split the work into several PRDs, each of which holds all of these:
   - Merging it alone leaves the base branch working.
   - It makes sense when read on its own.
   - It has the whole body format, with its own share of the acceptance criteria and test scenarios and its own ledger. builder reads only its own PRD, so repeat in it any decision and context it needs from the others, and name the work the others cover under Out of scope.
   - It is blocked by each earlier PRD it needs, and by each earlier PRD that changes a file it changes.

   When unsure, keep one PRD. The person decides on the split when approving.
4. Write each PRD draft in the plan file under its own heading `# PRD <k>: <title>`, in the order to file them, even when there is only one. Under the heading, write the body in exactly the body format below, with the ledger as its last section. Its Filing row names its tier (see Tier below) and the PRDs it is blocked by.
5. Check each draft with a fresh reader. Give a subagent only that PRD's body and the repository path, and ask: "If you had to implement this with only this text and the repository, how would you do it, and where would you get stuck or have to guess?" For each place it gets stuck, fix the draft if code or the ledger answers it. Otherwise ask the person. Repeat until it no longer gets stuck.
6. Get approval (exit plan mode).
7. Check that the labels exist: `gh label list --limit 100`. If `type:prd` or a tier label you need is missing, do not file. Tell the person that running exomecha once creates the labels.
8. File the PRDs in order. For each one:
   1. Save the text under its heading, **as is**, to a file outside the repository, and create the issue from it without labels.
      ```
      gh issue create --title "<title>" --body-file <body file>
      ```
   2. Attach it as a sub-issue of the problem. Sub-issues and blockers are attached by **issue ID**, not issue number.
      ```
      PRD_ID=$(gh api repos/{owner}/{repo}/issues/<PRD number> --jq .id)
      gh api --method POST repos/{owner}/{repo}/issues/<problem number>/sub_issues -F sub_issue_id="$PRD_ID"
      ```
   3. For each PRD it is blocked by:
      ```
      BLOCKER_ID=$(gh api repos/{owner}/{repo}/issues/<blocker number> --jq .id)
      gh api --method POST repos/{owner}/{repo}/issues/<PRD number>/dependencies/blocked_by -F issue_id="$BLOCKER_ID"
      ```
   4. Add the labels last. exomecha takes a PRD as soon as it has `type:prd`, so its blockers must already be attached.
      ```
      gh issue edit <PRD number> --add-label type:prd --add-label tier:<tier>
      ```
9. Share the PRD URLs. exomecha takes it from here.

## Decision ledger

Keep one row per category below. Close each row with the first of these that applies:

1. `code`: code or docs in the repository settle it. Cite the file.
2. `decided`: the problem issue, the discuss comment, or this conversation already decided it. Say where.
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
| Filing | target repository, title, tier, the PRDs this one is blocked by, duplicate issues |

## Tier

The tier picks the model that builds the PRD.

- `light`: a mechanical change, such as a rename, a config value, or a small fix in one file.
- `normal`: a usual feature or fix that needs reading the code across a few files. Pick this when unsure.
- `heavy`: a change that needs design judgment, spans many modules, or touches concurrency, data migration, security, or a public interface.

## Body format

```markdown
## Background

(why we are doing this, and the facts confirmed in the problem)

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
