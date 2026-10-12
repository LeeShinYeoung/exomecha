# Git

- First line of a commit: `<area>: <summary>`. Always write `area`, the short name of what the change is mainly about, such as a worker (builder, reviewer, resolver), a package under `internal` by its own name (worker, git), a skill (setup), cmd, docs, rules or e2e. When it is mainly about two, list both, such as `cmd, docs:`. When it is about the whole repository, write `all`.
- Write the summary in English, starting with an imperative verb, in 50 characters or fewer, with no period at the end. Start it in lowercase.
- Add a body after a blank line only when the reason for the change is not obvious. Say why, in 3 lines or fewer.
- When the commit belongs to a PRD, the last line is `PRD: #<PRD number>`.
- A merge commit keeps git's merge message.
- No `Co-Authored-By` line or other AI attribution.
- A PR title takes the same form as the first line of a commit.
- A branch name is the topic in English, in lowercase, with words joined by hyphens.
- Commit a finished change without asking the user first.
- Delete a branch locally and on the remote once its PR is merged.
