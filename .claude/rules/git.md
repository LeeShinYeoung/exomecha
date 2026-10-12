# Git

- First line of a commit: `<area>: <summary>`. Always write `area`.
- `area` is the folder of what the change is mainly about, as its top folder and its last folder joined by `/`: `internal/githubtest` for `internal/github/githubtest`, `cmd/exomecha`, `skills/setup`, `.claude/rules`. A folder right under the root stays as it is, such as `docs`. README.md and the other documents at the root take `docs`. When the change is mainly about two folders, list both, such as `cmd/exomecha, docs:`. When it is about the whole repository, write `all`.
- A name `area` leaves out, such as builder or e2e, goes in the summary.
- Write the summary in English, starting with an imperative verb, in 50 characters or fewer, with no period at the end. Start it in lowercase.
- Add a body after a blank line only when the reason for the change is not obvious. Say why, in 3 lines or fewer.
- When the commit belongs to a PRD, the last line is `PRD: #<PRD number>`.
- A merge commit keeps git's merge message.
- No `Co-Authored-By` line or other AI attribution.
- A PR title takes the same form as the first line of a commit.
- A branch name is the topic in English, in lowercase, with words joined by hyphens.
- Commit a finished change without asking the user first.
- Delete a branch locally and on the remote once its PR is merged.
