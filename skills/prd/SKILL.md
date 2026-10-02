---
name: prd
description: Write a PRD (a plan with every decision made) from a type:problem issue whose direction is settled. Build a decision ledger in plan mode and close every row, check the draft with a subagent that sees only the draft and the repository, get approval, then file it as a type:prd issue attached under the problem. Use when a person invokes /exomecha:prd.
---

# Write a PRD

exomecha splits the PRD into Tasks, then builds, reviews, and merges them without a person. The worker that builds a Task sees only that Task's body, which exomecha writes from this PRD. Anything the PRD leaves out is missing when the code gets written. **Make every decision here.** After this, a person only answers when a Task is blocked (`status:stuck`).

## Rules

- Do not change code.
- Do not fill decisions with guesses. Close every ledger row as the ledger section says.
- Do not file while any ledger row is open.
- The Acceptance criteria and Test scenarios sections are never empty. exomecha divides them among the Tasks and uses them when building and reviewing.
- Do not split into Tasks or check for overlap with other work. exomecha does that.

## Steps

1. Read the problem issue with its comments: `gh issue view <number> --comments`. If no number was given, ask. The comment left by `/exomecha:discuss` holds the chosen direction and the decisions made so far. Start from it.
2. Enter plan mode. Read the code the change touches, then build the decision ledger below and close its rows.
3. Write the PRD draft in the plan file, in exactly the body format below, with the ledger as its last section.
4. Check the draft with a fresh reader. Give a subagent only the draft text and the repository path, and ask: "If you had to implement this with only this text and the repository, how would you do it, and where would you get stuck or have to guess?" For each place it gets stuck, fix the draft if code or the ledger answers it. Otherwise ask the person. Repeat until it no longer gets stuck.
5. Get approval (exit plan mode).
6. File the approved plan file **as is** as the issue body.
   ```
   gh issue create --title "<title>" --body-file <plan file> --label type:prd
   ```
   If the `type:prd` label does not exist, do not file. Tell the person that running exomecha once creates the labels.
7. Attach the PRD as a sub-issue of the problem. Sub-issues are attached by **issue ID**, not issue number.
   ```
   PRD_ID=$(gh api repos/{owner}/{repo}/issues/<PRD number> --jq .id)
   gh api --method POST repos/{owner}/{repo}/issues/<problem number>/sub_issues -F sub_issue_id="$PRD_ID"
   ```
8. Share the PRD URL. exomecha takes it from here.

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
| Filing | target repository, title, duplicate issues |

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
