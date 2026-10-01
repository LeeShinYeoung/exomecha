# Comments

- Write a comment only for what the code cannot show: why it is done this way, or a constraint that someone changing it could break.
- Before writing one, ask: if this line were gone, could a reader change the code the wrong way? If not, leave it out.
- Do not restate a name. `// BaseFile reads a file from the base branch.` adds nothing. `// Not TrimSpace: a line for an unstaged change starts with a space.` stays.
- Exported names in internal packages do not need a doc comment, whatever the Go convention says.
- Do not copy a plan into comments, and do not list what the code does not do.
