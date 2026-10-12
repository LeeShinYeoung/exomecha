# Git

- First line of a commit: `<area>: <summary>`. `area` is what the change touches: a worker such as builder, reviewer or resolver, a package under `internal`, a skill, or e2e. Leave out `area:` when the change spans several.
- Write the summary in English, starting with an imperative verb, in 50 characters or fewer, with no period at the end. Start it in lowercase after `area:`, and with a capital letter without one.
- Add a body after a blank line only when the reason for the change is not obvious. Say why, in 3 lines or fewer.
- When the commit belongs to a PRD, the last line is `PRD: #<PRD number>`.
- A merge commit keeps git's merge message.
- No `Co-Authored-By` line or other AI attribution.
- A PR title takes the same form as the first line of a commit.
- A branch name is the topic in English, in lowercase, with words joined by hyphens.
- Delete a branch locally and on the remote once its PR is merged.
