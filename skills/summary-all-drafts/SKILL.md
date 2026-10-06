---
name: summary-all-drafts
description: Read every open draft PRD (labeled prd and status:draft) and print each one's title with a summary of up to three lines, so the person can pick what to discuss or complete next. Changes nothing. Use when a person invokes /exomecha:summary-all-drafts.
---

# Summarize the drafts

List the open drafts filed with `/exomecha:draft-prd`, so the person can choose how to go on.

## Rules

- Do not change issues or code.
- After printing, the person decides how to go on. Do not suggest an order or pick drafts unless asked.

## Steps

1. Read every open draft with its comments. Filter the labels with `--jq`, not `--label`: GitHub's label filter can miss an issue labeled in the last few minutes.
   ```
   gh issue list --state open --limit 1000 --json number,title,body,labels,comments --jq '[.[] | select(any(.labels[]; .name == "prd") and any(.labels[]; .name == "status:draft"))]'
   ```
2. Print each draft in number order, in the format below. If there is none, say so.
3. Wait for the person. They may want to discuss one draft in depth, or decide several small ones quickly and then run `/exomecha:complete-prd` with their numbers.
4. If asked which drafts can be decided quickly, pick the ones whose open questions the draft and the code already mostly answer, and say why for each.

## Output format

```
#<number> <title>
<summary of up to three lines: the problem, what is known, what is still open>
```
